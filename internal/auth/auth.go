// Package auth wires the /api/whoami, /api/login, /api/logout endpoints
// that shell out to the bundled kiro-cli binary for identity operations.
// No state persists in the package beyond the CLI path resolved at
// construction time.
//
// Ported verbatim from apps/vibekit/web/internal/auth/auth.go (minus
// the module-qualified api import). The only difference is the import
// path; behaviour and all hardening rules (output caps, process group
// kills, URL-scanner line cap, AWS region regex, 16m hard cap) are
// identical.
package auth

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"

	"vibecli/internal/api"
)

// TimeoutPolicy groups the four subprocess timeout durations. Passed to
// NewHandler at construction so tests can inject microsecond-scale values
// without mutating package-level state (enables t.Parallel).
type TimeoutPolicy struct {
	LoginURL     time.Duration
	LoginHardCap time.Duration
	Logout       time.Duration
	Whoami       time.Duration
}

// DefaultTimeoutPolicy returns the production timeout configuration.
func DefaultTimeoutPolicy() TimeoutPolicy {
	return TimeoutPolicy{
		LoginURL:     10 * time.Second,
		LoginHardCap: 16 * time.Minute,
		Logout:       10 * time.Second,
		Whoami:       5 * time.Second,
	}
}

// Scanner caps for scanLoginOutput: a generous per-line limit guards
// against accidental 64 KiB overflows from debug dumps embedded in the
// login banner, and maxLoginLines bounds total memory.
const (
	maxScanLineBytes = 256 * 1024
	maxLoginLines    = 200
)

// Subprocess output caps. Legitimate whoami output is ~150 bytes of
// JSON; the 64 KiB cap catches a pathological/malicious kiro-cli
// replacement trying to OOM the container. The logout cap is 1 MiB
// since kiro-cli prints a multi-line confirmation banner.
const (
	whoamiMaxOutput = 64 * 1024
	logoutMaxOutput = 1 << 20 // 1 MiB

	// stderrCap bounds subprocess stderr capture across every
	// handler. 32 KiB fits every legitimate kiro-cli diagnostic
	// with several orders of magnitude of headroom.
	stderrCap = 32 * 1024
)

// Maximum byte lengths for login request fields. A well-formed start URL
// is under ~200 bytes; the 2048 ceiling matches browser URL limits.
const (
	maxProviderLen = 2048
	maxRegionLen   = 32
)

// awsRegionRe matches AWS region ids across all partitions: commercial
// (us-east-1), China (cn-north-1), GovCloud (us-gov-west-1), ISO
// (us-iso-east-1, us-isob-east-1, eu-isoe-west-1). Rejects
// flag-smuggling (`--help`), shell metacharacters, whitespace,
// uppercase, and empty interior segments.
var awsRegionRe = regexp.MustCompile(`^[a-z]{2}(?:-[a-z]+)+-\d+$`)

// Handler is the /api/whoami + /api/login + /api/logout endpoint bundle.
// It shells out to kiro-cli for identity operations; no state persists
// beyond the CLI path. `loginSem` serialises login subprocesses for the
// full device-flow lifetime: vibecli is single-user, and a browser
// double-click or LAN probe would otherwise spawn duplicate kiro-cli
// subprocesses each pinning their own AWS device code for the full
// loginProcessHardCap (16m). The semaphore is released by the reap
// goroutine when cmd.Wait returns, not when the HTTP handler returns —
// so a second POST that arrives after the first URL has been emitted
// but while the first subprocess is still alive still gets 409.
type Handler struct {
	loginSem chan struct{}
	cliPath  string
	timeouts TimeoutPolicy
}

// NewHandler returns an auth handler that shells out to the kiro-cli
// binary at cliPath for whoami / login / logout operations. The
// TimeoutPolicy controls all subprocess timeout durations.
func NewHandler(cliPath string, tp TimeoutPolicy) *Handler {
	return &Handler{cliPath: cliPath, loginSem: make(chan struct{}, 1), timeouts: tp}
}

// RegisterRoutes wires the auth handler's HTTP endpoints onto mux:
//   - GET  /api/whoami — current kiro-cli identity
//   - POST /api/login  — device-code login flow
//   - POST /api/logout — clear local credentials
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/whoami", h.handleWhoami)
	mux.HandleFunc("/api/login", h.handleLogin)
	mux.HandleFunc("/api/logout", h.handleLogout)
}

// limitedWriter writes at most N bytes to W and silently drops the rest.
// Used for bounded stderr / stdout capture from subprocess handlers so
// a misbehaving or hostile kiro-cli can't OOM the container.
type limitedWriter struct {
	W io.Writer
	N int64
}

// Write implements io.Writer, enforcing the byte cap.
func (lw *limitedWriter) Write(p []byte) (int, error) {
	if lw.N <= 0 {
		return len(p), nil // pretend we wrote, drop the bytes
	}
	if int64(len(p)) > lw.N {
		p = p[:lw.N]
	}
	n, err := lw.W.Write(p)
	lw.N -= int64(n)
	return n, err
}

// stderrAttr returns slog key/value attributes for a captured stderr
// buffer, omitting the "stderr" key entirely when the buffer is empty.
func stderrAttr(stderr *bytes.Buffer) []any {
	s := api.SanitizeOutput(strings.TrimSpace(stderr.String()))
	if s == "" {
		return nil
	}
	return []any{"stderr", s}
}

// killLoginProcess sends SIGKILL to the entire process group of the
// login subprocess. kiro-cli is a bun/Node wrapper that may spawn
// helper children; killing only the parent leaves orphans pinning the
// stdout pipe open. Idempotent: calling after reap is a no-op.
func killLoginProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	err := loginKill(cmd)
	if err == nil {
		return
	}
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
		slog.Debug("login: kill group no-op (already reaped)",
			"group_err", err)
		return
	}
	kerr := cmd.Process.Kill()
	if kerr == nil {
		return
	}
	if errors.Is(kerr, syscall.ESRCH) || errors.Is(kerr, os.ErrProcessDone) {
		slog.Debug("login: kill pid no-op (already reaped)",
			"group_err", err, "pid_err", kerr)
		return
	}
	slog.Error("login: kill timeout process",
		"group_err", err, "pid_err", kerr)
}
