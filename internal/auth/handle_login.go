// handle_login.go implements the POST /api/login endpoint.

package auth

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"slices"
	"strings"
	"time"

	"vibecli/internal/api"
)

// LoginScanResult is the typed result sent from scanLoginOutput to the
// handler over the urlCh channel. JSON tags match the old map keys
// exactly so the HTTP response is byte-identical.
type LoginScanResult struct {
	URL   string `json:"url,omitempty"`
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// loginReap bundles the state handed off from handleLogin to the reap
// goroutine. Ownership transfers atomically: after the go statement,
// the reap goroutine is the sole owner of ctx cancellation, cmd.Wait,
// stderrBuf reads, and the waitDone close.
type loginReap struct {
	ctx       context.Context
	cancel    context.CancelFunc
	cmd       *exec.Cmd
	stderrBuf *bytes.Buffer
	waitDone  chan struct{}
}

// lineRing holds the first N and last N lines pushed into it, capping
// each line at perLineCap bytes. Used for the line-cap diagnostic log
// in scanLoginOutput.
type lineRing struct {
	first      []string
	last       []string
	halfCap    int
	perLineCap int
}

func newLineRing(halfCap, perLineCap int) *lineRing {
	return &lineRing{
		first:      make([]string, 0, halfCap),
		last:       make([]string, 0, halfCap),
		halfCap:    halfCap,
		perLineCap: perLineCap,
	}
}

// Push appends line to the ring, truncating at perLineCap bytes so an
// adversarial CLI can't blow up a single structured log attribute.
func (r *lineRing) Push(line string) {
	if len(line) > r.perLineCap {
		line = line[:r.perLineCap]
	}
	switch {
	case len(r.first) < r.halfCap:
		r.first = append(r.first, line)
	case len(r.last) == r.halfCap:
		r.last = append(r.last[1:], line)
	default:
		r.last = append(r.last, line)
	}
}

// Sample returns the concatenation of the first-N and last-N slices.
func (r *lineRing) Sample() []string {
	return slices.Concat(r.first, r.last)
}

// validateProvider rejects anything that isn't a well-formed HTTPS URL.
// Empty strings are allowed (kiro-cli falls back to the default Builder
// ID flow). Guardrail against phishing: a LAN-reachable POST could
// otherwise forward an attacker-controlled start URL.
func validateProvider(v string) error {
	if v == "" {
		return nil
	}
	if len(v) > maxProviderLen {
		return errors.New("provider too long")
	}
	u, perr := url.Parse(v)
	if perr != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("provider must be an https URL")
	}
	if u.User != nil {
		return errors.New("provider must not contain credentials")
	}
	return nil
}

// validateRegion rejects anything that isn't a canonical AWS region id.
// Empty strings pass through so kiro-cli picks its default.
func validateRegion(v string) error {
	if v == "" {
		return nil
	}
	if len(v) > maxRegionLen {
		return errors.New("region too long")
	}
	if !awsRegionRe.MatchString(v) {
		return errors.New("invalid region")
	}
	return nil
}

// buildLoginArgs returns the argv tail (after the binary path) for a
// `kiro-cli login` invocation with optional provider/region overrides.
func buildLoginArgs(provider, region string) []string {
	args := []string{"login", "--use-device-flow"}
	if provider != "" {
		args = append(args, "--identity-provider", provider)
	}
	if region != "" {
		args = append(args, "--region", region)
	}
	return args
}

// parseLoginRequest decodes the POST body, enforces the MaxJSONBody
// cap, and validates provider+region. Writes the error response and
// returns ok=false on any failure. An empty body returns zero values
// with ok=true (default Builder ID flow).
func parseLoginRequest(w http.ResponseWriter, r *http.Request) (provider, region string, ok bool) {
	api.LimitBody(w, r, api.MaxJSONBody)
	var body struct {
		Provider string `json:"provider"`
		Region   string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			slog.Warn("login: body exceeds limit",
				"limit_bytes", api.MaxJSONBody)
			api.WriteError(w, r, http.StatusRequestEntityTooLarge,
				"request_too_large", "request too large")
			return "", "", false
		}
		slog.Warn("login: decode body", "error", err)
		api.BadRequest(w, r, "invalid JSON body")
		return "", "", false
	}
	if err := validateProvider(body.Provider); err != nil {
		api.BadRequest(w, r, err.Error())
		return "", "", false
	}
	if err := validateRegion(body.Region); err != nil {
		api.BadRequest(w, r, err.Error())
		return "", "", false
	}
	return body.Provider, body.Region, true
}

// handleLogin spawns `kiro-cli login --use-device-flow` and streams
// its stdout looking for the first "Open this URL:" (or bare https://)
// token to return to the browser. The subprocess intentionally
// outlives the HTTP request (device-flow login takes minutes) — see
// context.WithTimeout below which caps the subprocess at
// loginProcessHardCap. Only one login may be in flight at a time: a
// concurrent POST gets HTTP 409.
func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		api.MethodNotAllowed(w, r)
		return
	}
	slog.Info("login: request received",
		"remote_addr", r.RemoteAddr,
		"user_agent", r.Header.Get("User-Agent"))
	audit(r, slog.LevelInfo, AuditLoginStart, true)
	select {
	case h.loginSem <- struct{}{}:
		// Ownership transfers to the reap goroutine after cmd.Start.
	default:
		audit(r, slog.LevelWarn, AuditLoginBusy, false,
			slog.String("reason", "concurrent_login_in_progress"))
		api.Conflict(w, r, "login in progress")
		return
	}
	semReleased := false
	defer func() {
		if !semReleased {
			<-h.loginSem
		}
	}()
	provider, region, ok := parseLoginRequest(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.timeouts.LoginHardCap)
	cmd := exec.CommandContext(ctx, h.cliPath, buildLoginArgs(provider, region)...) // #nosec G204 -- cliPath is operator-controlled
	setLoginProcAttr(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		slog.Error("login: stdout pipe failed",
			"error", err, "cli_path", h.cliPath)
		api.WriteError(w, r, http.StatusInternalServerError, "internal_error", "login unavailable")
		return
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &limitedWriter{W: &stderrBuf, N: stderrCap}
	if err := cmd.Start(); err != nil {
		cancel()
		status := classifyLoginStartErr(err, h.cliPath)
		api.WriteError(w, r, status, "login_unavailable", "login unavailable")
		return
	}
	urlCh := make(chan LoginScanResult, 1)
	go scanLoginOutputWithDrain(stdout, urlCh)

	// Transfer loginSem ownership to the reap goroutine.
	semReleased = true

	waitDone := make(chan struct{})
	go h.reapLoginProcess(loginReap{
		ctx:       ctx,
		cancel:    cancel,
		cmd:       cmd,
		stderrBuf: &stderrBuf,
		waitDone:  waitDone,
	})

	select {
	case result := <-urlCh:
		if result.URL == "" {
			slog.Warn("login: scanner reported error without URL",
				"error", result.Error)
			audit(r, slog.LevelWarn, AuditLoginFailure, false,
				slog.String("reason", "scanner_no_url"),
				slog.String("error", result.Error))
			killLoginProcess(cmd)
			<-waitDone
		} else {
			// URL extracted; the actual login completes asynchronously
			// once the user follows the device flow. Audit the URL-issued
			// step here as login.success because that is the last point
			// vibecli's process tree can observe; downstream completion
			// happens in kiro-cli's credential cache.
			audit(r, slog.LevelInfo, AuditLoginSuccess, true,
				slog.String("provider", provider),
				slog.String("region", region))
		}
		api.WriteJSON(w, result)
	case <-time.After(h.timeouts.LoginURL):
		killLoginProcess(cmd)
		<-waitDone
		attrs := make([]any, 0, 4)
		attrs = append(attrs, "timeout", h.timeouts.LoginURL)
		attrs = append(attrs, stderrAttr(&stderrBuf)...)
		slog.Warn("login: timeout waiting for auth URL", attrs...)
		audit(r, slog.LevelWarn, AuditLoginFailure, false,
			slog.String("reason", "timeout_waiting_for_url"),
			slog.Duration("timeout", h.timeouts.LoginURL))
		api.WriteJSONStatus(w, http.StatusGatewayTimeout,
			LoginScanResult{Error: "timeout waiting for auth URL"})
	}
}

// classifyLoginStartErr maps a cmd.Start error to an HTTP status code.
// fs.ErrNotExist catches fork/exec ENOENT (absolute cliPath);
// exec.ErrNotFound catches LookPath failures. Both surface as 503 so
// operators can distinguish "binary missing" (redeploy) from
// "transient fork failure" (retry — 500).
func classifyLoginStartErr(err error, cliPath string) int {
	cliErr := classifyCLIError(context.Background(), err)
	switch cliErr.Kind {
	case CLIErrNotFound:
		slog.Error("login: kiro-cli binary not found",
			"cli_path", cliPath)
		return http.StatusServiceUnavailable
	default:
		slog.Error("login: kiro-cli start failed",
			"error", err, "cli_path", cliPath)
		return http.StatusInternalServerError
	}
}

// reapLoginProcess waits for the login subprocess to exit, escalates
// the process-group kill on hard-cap expiry, releases the semaphore,
// and closes waitDone.
func (h *Handler) reapLoginProcess(r loginReap) {
	killOnDeadline := make(chan struct{})
	go func() {
		select {
		case <-r.ctx.Done():
			if errors.Is(r.ctx.Err(), context.DeadlineExceeded) {
				killLoginProcess(r.cmd)
			}
		case <-killOnDeadline:
		}
	}()
	werr := r.cmd.Wait()
	close(killOnDeadline)
	if errors.Is(r.ctx.Err(), context.DeadlineExceeded) {
		killLoginProcess(r.cmd)
	}
	r.cancel()
	switch {
	case werr == nil:
		slog.Info("login: subprocess completed cleanly")
	case errors.Is(r.ctx.Err(), context.DeadlineExceeded):
		attrs := make([]any, 0, 4)
		attrs = append(attrs, "cap", h.timeouts.LoginHardCap)
		attrs = append(attrs, stderrAttr(r.stderrBuf)...)
		slog.Warn("login: subprocess hit hard cap", attrs...)
	default:
		slog.Debug("login: cmd wait returned", "error", werr)
	}
	<-h.loginSem
	close(r.waitDone)
}

// extractAuthURL pulls an auth URL out of a single already-stripped
// login-output line. An explicit "Open this URL:" prefix anchors the
// search to the tail after the prefix. A non-https scheme yields "" —
// defense-in-depth against a compromised kiro-cli emitting
// scheme-injection payloads.
func extractAuthURL(line string) string {
	if after, found := strings.CutPrefix(line, "Open this URL:"); found {
		for word := range strings.FieldsSeq(after) {
			if strings.HasPrefix(word, "https://") {
				return word
			}
		}
		return ""
	}
	if strings.Contains(line, "https://") {
		for word := range strings.FieldsSeq(line) {
			if strings.HasPrefix(word, "https://") {
				return word
			}
		}
	}
	return ""
}

// scanLoginOutputWithDrain runs scanLoginOutput and then keeps reading
// stdout to io.Discard until the subprocess closes the pipe. Without
// the drain, the pipe fills at 64 KiB and kiro-cli blocks on write(2)
// until the 16m hard cap fires.
func scanLoginOutputWithDrain(stdout io.ReadCloser, urlCh chan<- LoginScanResult) {
	scanLoginOutput(stdout, urlCh)
	if _, err := io.Copy(io.Discard, stdout); err != nil {
		slog.Debug("login: stdout drain stopped", "error", err)
	}
}

// scanLoginOutput reads lines from r until it finds an auth URL. Sends
// the discovered URL + optional "Code:" into urlCh. On EOF with no URL,
// scanner error, or line cap hit, sends an error result. urlCh MUST be
// buffered so this function never blocks after a timeout.
func scanLoginOutput(stdout io.Reader, urlCh chan<- LoginScanResult) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), maxScanLineBytes)
	ring := newLineRing(5, 128)
	var code, authURL string
	var lineCount int
	for scanner.Scan() {
		line := strings.TrimSpace(api.StripANSI(scanner.Text()))
		lineCount++
		ring.Push(line)
		if strings.Contains(strings.ToLower(line), "already logged in") {
			urlCh <- LoginScanResult{Error: "already_logged_in"}
			return
		}
		if after, found := strings.CutPrefix(line, "Code:"); found {
			code = strings.TrimSpace(after)
		}
		if authURL == "" {
			authURL = extractAuthURL(line)
		}
		if authURL != "" {
			slog.Info("login: auth URL extracted",
				"has_code", code != "",
				"lines_before_url", lineCount)
			urlCh <- LoginScanResult{URL: authURL, Code: code}
			return
		}
		if lineCount >= maxLoginLines {
			slog.Warn("login: output line cap hit without auth URL",
				"lines", lineCount,
				"first_and_last_sample", ring.Sample())
			urlCh <- LoginScanResult{Error: "CLI produced too much output without auth URL"}
			return
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("login: scanner failed before URL",
			"error", err, "lines_read", lineCount)
		urlCh <- LoginScanResult{Error: "scanner error: " + err.Error()}
		return
	}
	urlCh <- LoginScanResult{Error: "no auth URL found in CLI output"}
}
