package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/toolbelt/v3"
	"github.com/cplieger/toolbelt/v3/httpapi"
	"github.com/cplieger/web-terminal-engine/v6/terminal"
)

// App-owned route paths. Every engine route comes from the engine's exported
// terminal.* path constants; these are the surfaces this app mounts itself, named once
// so a mount, a handler's declared base path, and the middleware policy that keys on
// the same path cannot drift apart across files.
const (
	apiPrefix = "/api/"
	// healthPath is the readiness route; buildHandler's ProbeLogLevel policy must name
	// the same path the mux registers, or a healthy probe stops being demoted to Debug
	// and a failing one stops being promoted.
	healthPath = apiPrefix + "health"
	// toolsPath is the loopback tools API mount; httpapi.Handler is told the same base
	// path so its own routing matches where it is mounted.
	toolsPath = apiPrefix + "tools"
	// kiroRescanPath is the loopback kiro-cli repair hook: it makes an install fixed
	// INSIDE the container observable to the running server. Registered with a method
	// pattern, so anything but POST is a 405 rather than a silent no-op.
	kiroRescanPath = apiPrefix + "kiro-cli/rescan"
	// uploadsPath receives pasted clipboard images; static-src/image-paste.ts posts to it.
	uploadsPath = apiPrefix + "uploads"
)

type routeDeps struct {
	// static is the embedded-static serving handler, built by the composition root
	// together with the hash-pinned CSP the same tree produces.
	static http.Handler
	ready  *httpserver.Ready
	// listenHint is "localhost[:port]" for THIS deployment; loopback refusals quote
	// it so the remedy works on a server that did not keep the default port.
	listenHint string
	// nil is reserved for the two MOUNTING decisions (tools, kiroRescan); every other
	// policy function below is always non-nil, defaulted by the composition root's
	// off-shape constructors. toolsMissing's second return distinguishes "none
	// outstanding" from "not counted yet".
	tools        *toolbelt.Engine
	toolsSyncing func() bool
	toolsState   func() string
	toolsMissing func() (int, bool)
	// containment, when non-nil, puts each tab's kiro-cli process tree in its own
	// cgroup so ending the session ends the tree. `kiro-cli chat` is a four-deep tree
	// whose agent server calls setsid() and installs no stdin-EOF exit path, so it
	// can outlive the session that spawned it (measured: 13 stranded processes
	// holding 1.35 GB on a two-tab container).
	containment *terminal.Containment
	// kiroReady is the install manager's readiness verdict plus the reason to report
	// when false; gates /api/health and session creation.
	kiroReady func() (bool, string)
	// restoring is true from boot until the tab restore finished; it holds session
	// creation and the layout read (restorePendingGate).
	restoring func() bool
	// kiroRescan, when non-nil, re-derives the active kiro-cli version from disk
	// without downloading anything. Backs kiroRescanPath.
	kiroRescan func(context.Context) (bool, error)
	// cmd builds one session's argv. A FUNCTION, not a slice: the active kiro-cli
	// version can change while the server runs.
	cmd func() []string
	// sessionEnv returns the per-session environment overlay (the active version
	// directory leading PATH). A nil result leaves the child with the server's own
	// environment.
	sessionEnv func() []string
	// engineEnv returns the high-throughput overlay for kiro-cli's agent engine
	// (engineTuning.env); nil, or a nil result, adds nothing.
	engineEnv func() []string
	// sessionTitleEnv returns the per-session variables a kiro-cli hook needs to
	// report which kiro session this tab is running (the tab's TITLE HANDLE, not the
	// session id — see sessiontitle.go). A nil function leaves tabs on the engine's
	// automatic name ladder.
	sessionTitleEnv func(id terminal.SessionID) []string
	// sessionActivity reports one tab's SECONDARY state — what a workflow run launched
	// in it is doing — which the engine carries beside Status and never merges into it.
	// A nil function OMITS the option, so every tab's activity stays empty.
	sessionActivity func(id terminal.SessionID) terminal.SessionActivity
	// scrollback is the operator's retained-history depth, or nil when unset (the
	// option is then OMITTED and the engine's own default applies). A POINTER
	// because 0 is a legal depth meaning "retain nothing" — an int sentinel would
	// silently disable scrollback in every hand-built routeDeps.
	scrollback *int
	workDir    string
	// uploadDir is where pasted images are written; empty leaves uploadsPath unmounted.
	uploadDir string
	// sandbox is the startup check's /api/health state; "" omits the field.
	sandbox string
}

// buildStaticSurface assembles the embedded-static serving surface: the static handler
// and the hash-pinned CSP policy built from the same tree, fail-loud on a malformed
// embed. httpserver.StaticHandler supplies content-hash ETags (embed.FS reports a zero
// ModTime, so a bare http.FileServer emits no validator) plus precomputed gzip; the
// per-path cache POLICY stays this app's (kiroCacheControl).
func buildStaticSurface(staticFS fs.FS) (http.Handler, string, error) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, "", err
	}
	cspPolicy, err := buildCSPPolicy(sub)
	if err != nil {
		return nil, "", err
	}
	staticSrv, err := httpserver.StaticHandler(sub, httpserver.WithStaticCacheControl(kiroCacheControl))
	if err != nil {
		return nil, "", err
	}
	return staticSrv, cspPolicy, nil
}

// sessionCreateGate throttles session creation in aggregate, because each
// admitted POST forks a PTY: a burst of six absorbs a user opening several tabs
// at once, and one token a second bounds sustained churn. Other methods on the
// sessions path pass through without spending a token.
func sessionCreateGate() httpserver.Middleware {
	return httpserver.RateLimiter(6, time.Second,
		httpserver.WithRateLimitWhen(func(r *http.Request) bool {
			return r.Method == http.MethodPost && r.URL.Path == terminal.SessionsPath
		}),
		httpserver.WithRateLimitError("rate_limited", "session creation rate exceeded"),
	)
}

// registerRoutes wires the full route table on mux and returns the session manager (for
// shutdown). factory builds each tab the browser creates; the caller owns it because
// the tab restorer builds from the same newSessionBuilder.
func registerRoutes(mux *http.ServeMux, deps *routeDeps, factory func(terminal.SessionID) *terminal.Handler) *terminal.SessionManager {
	mux.Handle("/", deps.static)

	mgrOpts := []terminal.ManagerOption{
		terminal.WithManagerLogger(slog.Default()),
		terminal.WithStatusClassifier(classifyNotification),
	}
	if deps.sessionActivity != nil {
		mgrOpts = append(mgrOpts, terminal.WithSessionActivity(deps.sessionActivity))
	}
	mgr := terminal.NewSessionManager(factory, mgrOpts...)

	// The engine owns its route topology: MountAPI wires exactly its documented set,
	// so no engine-internal route can appear on this unauthenticated surface
	// unannounced.
	//
	// The create gate composes four layers, outermost first: kiro-cli install (a
	// session created during the first-boot download exits 1), the tools boot
	// convergence (kiro-cli scans PATH for language servers once at session start),
	// the tab restore (restorePendingGate), then the rate limit.
	toolsBlocked := func() (bool, string) { return deps.toolsSyncing(), "tools installing" }
	kiroBlocked := func() (bool, string) {
		ready, reason := deps.kiroReady()
		return !ready, reason
	}
	createGate := sessionCreateGate()
	createGate = restorePendingGate(createGate, deps.restoring, kiroBlocked, toolsBlocked)
	createGate = composeGate(createGate, toolsBlocked)
	createGate = composeGate(createGate, kiroBlocked)
	mgr.MountAPI(mux, terminal.WithCreateGate(createGate))

	// Tools REST surface, loopback-only. The consumer is an agent inside the
	// container via kiro-cli's `!` shell escape; the gate checks the socket peer and
	// the Host header, never forwarded headers, so it cannot be spoofed through a
	// proxy nor reached by a DNS-rebound page.
	if deps.tools != nil {
		toolsAPI := loopbackOnly("tools API", deps.listenHint, httpapi.Handler(deps.tools, toolsPath))
		mux.Handle(toolsPath, toolsAPI)
		mux.Handle(toolsPath+"/", toolsAPI)
	}

	// kiro-cli repair hook, admitted the same way and for the same consumer: a
	// one-shot background install that exhausted its retries leaves the server
	// answering 503 forever, and a repair made inside the container is invisible to
	// it until recreated. Downloads nothing.
	if deps.kiroRescan != nil {
		mux.Handle("POST "+kiroRescanPath, loopbackOnly("kiro-cli rescan hook", deps.listenHint, handleKiroRescan(deps)))
		// ServeMux synthesizes 405 only when NO pattern matches, and the "/" static mount
		// matches every path — so without this mount a GET here gets a bare 404. Less
		// specific than the POST pattern above, so POST still reaches the handler.
		mux.Handle(kiroRescanPath, loopbackOnly("kiro-cli rescan hook", deps.listenHint,
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Allow", http.MethodPost)
				httpserver.WriteError(w, r, http.StatusMethodNotAllowed, "method_not_allowed",
					"kiro-cli rescan is POST-only. Call it with curl -X POST "+deps.listenHint+kiroRescanPath)
			})))
	}

	// Browser-facing, so it sits behind the same host allowlist and cross-origin check as
	// /ws and session creation; it grants nothing a tab's shell does not already have.
	if deps.uploadDir != "" {
		mux.Handle("POST "+uploadsPath, handleUpload(deps.uploadDir, maxUploadSize))
		// Same two-pattern shape as kiroRescanPath: the "/" mount would otherwise 404 a GET.
		mux.HandleFunc(uploadsPath, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Allow", http.MethodPost)
			httpserver.WriteError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "image upload is POST-only")
		})
	}

	mux.HandleFunc(healthPath, handleHealth(deps))

	return mgr
}

// kiroRescanBody is the repair hook's response envelope, matching healthBody's key order.
// The status vocabulary is only partly shared: "ok"/"unready" mean what they mean on
// /api/health; "abandoned" is this hook's own — only a rescan can fail to reach a verdict.
type kiroRescanBody struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// reasonRescanAbandoned is the repair hook's 503 reason when the request was abandoned
// before pin reached any verdict. Deliberately NOT one of main.go's kiroReasonText
// strings: those name the install manager's readiness state, and this reaches no verdict
// about the install at all.
const reasonRescanAbandoned = "kiro-cli rescan not performed: request abandoned before any verdict"

// handleKiroRescan re-derives the active kiro-cli version from what is on disk right now
// and reports the resulting readiness: 200 when a version is active afterwards, 503 with
// the manager's own reason when none is.
func handleKiroRescan(deps *routeDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		// pin owns both halves this handler used to hand-roll: WAITING for its
		// operation slot honours this context, while an ADMITTED rescan runs detached, so
		// a Ctrl-C or --max-time cannot make every candidate probe fail and have the
		// manager record "no usable version".
		ok, err := deps.kiroRescan(r.Context())
		if ok {
			httpserver.WriteJSON(w, kiroRescanBody{Status: "ok"})
			return
		}
		// A context error means the request was ABANDONED before pin reached any
		// verdict (the caller's own cancellation, or this server's shutdown). Reporting
		// that as "no usable version" would be a false broken-install record, and
		// "unready" is wrong too — a rescan queued behind another can abandon on a
		// perfectly ready manager.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			slog.Debug("kiro-cli rescan abandoned before a verdict was reached",
				"error", err)
			httpserver.WriteJSONStatus(w, http.StatusServiceUnavailable,
				kiroRescanBody{Status: "abandoned", Reason: reasonRescanAbandoned})
			return
		}
		// The manager has already logged the specific fault; report the verdict rather
		// than the error text, which can name a filesystem path.
		reason := reasonUnavailable
		if _, why := deps.kiroReady(); why != "" {
			reason = why
		}
		// pin's Rescan's ordinary failure is (false, nil); a nil error attribute would
		// put "error":null there, indistinguishable from a state-write failure.
		attrs := []any{"reason", reason}
		if err != nil {
			attrs = append(attrs, "error", err)
		}
		slog.Warn("kiro-cli rescan found no usable version", attrs...)
		httpserver.WriteJSONStatus(w, http.StatusServiceUnavailable,
			kiroRescanBody{Status: "unready", Reason: reason})
	}
}

// childEnv is the environment overlay for one tab's kiro-cli process: the active version's
// PATH lead, the agent-engine overlay, and the two variables a hook needs to report which
// kiro session this tab is running. Built as a fresh slice rather than appending to
// sessionEnv's return, so one session's overlay can never alias another's backing array.
func (d *routeDeps) childEnv(id terminal.SessionID) []string {
	base := d.sessionEnv()
	var engine, title []string
	if d.engineEnv != nil {
		engine = d.engineEnv()
	}
	if d.sessionTitleEnv != nil {
		title = d.sessionTitleEnv(id)
	}
	if len(engine) == 0 && len(title) == 0 {
		return base
	}
	return slices.Concat(base, engine, title)
}

// newSessionBuilder returns the per-session handler factory for one resume id ("" starts
// a fresh chat): one independent PTY-backed kiro-cli chat process with its own VT screen
// and scrollback. It owns five session-scoped policies — the argv and PATH resolved from
// the install manager AT session-create time, the resume argv, the LogID-truncated
// per-session logger, the argv redaction, and the fast-death Warn hook that surfaces a
// broken kiro-cli install.
func newSessionBuilder(deps *routeDeps) func(resumeID string) func(terminal.SessionID) *terminal.Handler {
	return func(resumeID string) func(terminal.SessionID) *terminal.Handler {
		if resumeID != "" && !validKiroResumeID(resumeID) {
			slog.Warn("session: refusing a malformed resume id; starting a fresh chat")
			resumeID = ""
		}
		return newSessionFactory(deps, resumeID)
	}
}

func newSessionFactory(deps *routeDeps, resumeID string) func(terminal.SessionID) *terminal.Handler {
	// The child gets the engine's real DEC 1004 focus, not WithKeepUnfocused: kiro-cli draws
	// its home ghost in colour and animates it only while focused, and tab status rides OSC
	// 7501, which focus does not gate.
	//
	// No TERM_PROGRAM override: the engine advertises iTerm.app, which puts kiro-cli in
	// its OSC 9;4 progress allowlist and enables DEC 2026 synchronized output.
	return func(id terminal.SessionID) *terminal.Handler {
		start := time.Now()
		// The session id doubles as the /ws attach + resume capability token, so only
		// the engine's LogID-truncated form is ever bound to a logger.
		safeID := terminal.LogID(id)
		// The argv carries the operator's KIRO_CLI_CHAT_ARGS values, which could hold a
		// credential from a compose interpolation mistake (CWE-532), so the engine is
		// told to log a fixed marker instead.
		sessionLogger := slog.Default().With("session", safeID)
		opts := []terminal.Option{
			terminal.WithWorkDir(deps.workDir),
			terminal.WithLogger(sessionLogger),
			terminal.WithCommandLogValue("[redacted]"),
			// The active kiro-cli version's own directory first on the child's PATH, plus
			// the two variables that let a kiro-cli hook report which kiro session this
			// tab is running. The engine appends WithEnv LAST, so this PATH wins.
			//
			// The engine's input-stream title deriver is deliberately NOT requested: every
			// discard key it does not model fuses abandoned text onto the next prompt.
			terminal.WithEnv(deps.childEnv(id)),
			// kiro-cli colours a markdown link with SGR 34 (palette slot 4); under the
			// engine's earlier VGA palette that slot was #0000aa, 1.58:1 against black.
			// 4.5 is the WCAG AA floor for body text; backgrounds and default foregrounds
			// are never touched.
			terminal.WithMinimumContrast(4.5),
			// Closing a tab must end the whole tree, and for this app it otherwise does
			// not: `kiro-cli chat` starts the agent server as a descendant, and that
			// process calls setsid(), so no signal the engine can aim reaches it. A nil containment is a no-op.
			terminal.WithContainment(deps.containment, id),
			// A dev-box tab can live for days; containment reclaims only at teardown, so
			// a tab whose agent connection died while its TUI stays alive keeps holding
			// memory for as long as it stays open, deliberately. This log line is what
			// keeps closing it the user's decision.
			terminal.WithContainmentSampleInterval(sessionCostInterval),
			// A session whose process dies within seconds of spawn is the
			// kiro-cli-missing/broken signature; raise it to Warn (the engine logs child
			// exit at Info by design).
			//
			// Gated on deps.ready: an app-initiated shutdown clears readiness before
			// mgr.Shutdown cancels the children, whose killed/canceled wait errors would
			// otherwise fire this as a false broken-install alert on every deploy.
			terminal.WithOnProcessExit(func(err error) {
				kiroReady, _ := deps.kiroReady()
				if err != nil && deps.ready.Ready() && kiroReady && time.Since(start) < 10*time.Second {
					sessionLogger.Warn(sessionFastDeathMsg,
						"error", err,
						"hint", "check /api/health and the kiro-cli install under /config/tools/kiro-cli-versions")
				}
			}),
		}
		if deps.scrollback != nil {
			opts = append(opts, terminal.WithScrollbackCapacity(*deps.scrollback))
		}
		// slices.Concat, not append: unmanagedKiroRuntime.cmd returns one shared slice
		// with spare capacity, which an append would overwrite across tabs. The id rides
		// sessionCommand's positional "$@" after chat, never the script text.
		argv := deps.cmd()
		if resumeID != "" {
			argv = slices.Concat(argv, []string{"--resume-id", resumeID})
		}
		return terminal.NewHandler(argv, opts...)
	}
}

// healthBody is /api/health's response envelope. A struct, not a map, because
// encoding/json sorts map keys, and httpserver's own readiness handler (and therefore
// web-terminal-server and subflux) emits {"status":…,"reason":…}. ToolsMissing is a
// POINTER so it is absent rather than 0 when unknown, since zero means converged.
// FIELD ORDER IS THE WIRE CONTRACT: never reorder for fieldalignment.
type healthBody struct {
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
	Tools        string `json:"tools,omitempty"`
	ToolsMissing *int   `json:"tools_missing,omitempty"`
	Sandbox      string `json:"sandbox,omitempty"`
}

// handleHealth returns the /api/health readiness handler. It reflects, in order: listener
// readiness, the kiro-cli install manager's verdict, and the INFORMATIONAL tools and
// sandbox fields — neither gates readiness.
func handleHealth(deps *routeDeps) http.HandlerFunc {
	// One constructor for both outcomes, so the informational fields cannot diverge
	// between them; they belong on the 503 bodies too, since an operator diagnosing a
	// 503 needs to see tools state as much as a healthy caller does.
	healthResponse := func(status, reason string) healthBody {
		body := healthBody{
			Status:  status,
			Reason:  reason,
			Tools:   deps.toolsState(),
			Sandbox: deps.sandbox,
		}
		if n, ok := deps.toolsMissing(); ok {
			body.ToolsMissing = &n
		}
		return body
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		// Set here rather than by middleware, so the handler carries its own contract
		// wherever it is mounted: a 200 with no explicit freshness is heuristically
		// cacheable under RFC 9111, and a cached "ok" keeps traffic arriving at an
		// instance that has begun draining.
		w.Header().Set("Cache-Control", "no-store")
		unready := func(reason string) {
			httpserver.WriteJSONStatus(w, http.StatusServiceUnavailable, healthResponse("unready", reason))
		}
		if !deps.ready.Ready() {
			unready("starting up or shutting down")
			return
		}
		// kiro-cli readiness, straight from the install manager. The listener binds
		// BEFORE the install runs, so this is what tells `docker ps` and the monitoring
		// probe apart from "serving". A READINESS signal, not liveness: under Swarm or
		// k8s, wire this to a readinessProbe, never a livenessProbe.
		if ok, reason := deps.kiroReady(); !ok {
			unready(reason)
			return
		}
		// The tools field is INFORMATIONAL: kiro-cli is the only core dependency, so
		// monitoring stays green during a long first-boot install while operators can
		// still see it.
		httpserver.WriteJSON(w, healthResponse("ok", ""))
	}
}

// sessionFastDeathMsg is the fast-death Warn's wording; a test asserts on it, and its
// quiet branch asserts an ABSENCE, which a reworded literal would satisfy vacuously.
const sessionFastDeathMsg = "session process exited almost immediately after start; kiro-cli may be missing or broken"

// classifyNotification maps kiro-cli's two ask notifications to StatusInput and leaves
// every other OSC 9 text unclassified, so done and error come from OSC 7501 alone. The
// engine shows a classified notification newer than the last OSC 7501 change until the
// next one, which covers kiro-cli reporting error over a pending ask
// (https://github.com/kirodotdev/Kiro/issues/11987).
func classifyNotification(msg string) (string, bool) {
	switch msg {
	case "Permission required", "Input required":
		return terminal.StatusInput, true
	}
	return "", false
}

// Cache-Control policies handed to httpserver.StaticHandler. Only a content-addressed asset may
// promise its bytes never change, so `immutable` is earned by the name.
const (
	fontAssetPrefix    = "vendor/fonts/"
	fingerprintedAsset = "public, max-age=31536000, immutable"
	revalidatedAsset   = "no-cache, must-revalidate"
)

// kiroCacheControl is the per-asset policy; asset paths arrive normalized, no leading slash.
// Reading the hash off the NAME rather than trusting the directory is what makes dropping
// scripts/font-fingerprint.sh degrade to revalidation instead of to a stale face.
func kiroCacheControl(assetPath string) string {
	if strings.HasPrefix(assetPath, fontAssetPrefix) && fingerprintedName(path.Base(assetPath)) {
		return fingerprintedAsset
	}
	return revalidatedAsset
}

// fingerprintedName reports whether name carries the `<stem>.<8 lowercase hex>.<ext>` hash
// scripts/font-fingerprint.sh stamps.
func fingerprintedName(name string) bool {
	ext := path.Ext(name)
	if ext == "" {
		return false
	}
	stem := strings.TrimSuffix(name, ext)
	hash := path.Ext(stem)
	if len(hash) != 9 { // a dot plus eight hex digits
		return false
	}
	for _, r := range hash[1:] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// cspTemplate is the Content-Security-Policy applied to every response, with a %s
// placeholder for the script-src hash tokens and a second for the style-src hash token.
//
// index.html's single loading-overlay <style> is hash-pinned like the inline scripts, so
// an injected style block or style attribute cannot obscure or spoof the terminal UI. The
// renderer itself needs no relaxation: it styles via CSSOM property setters, which
// style-src does not govern, and neither the UI nor the engine emits a style= attribute.
const cspTemplate = "default-src 'self'; " +
	"script-src 'self' %s; " +
	"style-src 'self' %s; " +
	"img-src 'self' data:; font-src 'self'; connect-src 'self'; " +
	"frame-ancestors 'none'; base-uri 'none'; object-src 'none'; " +
	"form-action 'none'"

// buildCSPPolicy reads index.html from sub, hashes every inline <script> in it plus its
// single inline <style> block, and assembles the full CSP string. This app's index.html
// carries TWO inline scripts, the importmap and the bootstrap watchdog.
//
// FAIL LOUD: a malformed build aborts startup rather than silently dropping the hardening.
// The one-block assertion stays HERE rather than in the library: "exactly one" is this
// app's contract about its own page, not a property of style hashing.
func buildCSPPolicy(sub fs.FS) (string, error) {
	html, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return "", fmt.Errorf("buildCSPPolicy: read index.html: %w", err)
	}
	hashes := httpserver.InlineScriptHashes(html)
	if len(hashes) == 0 {
		return "", errors.New("buildCSPPolicy: no inline <script> blocks in index.html")
	}
	styleHashes := httpserver.InlineStyleHashes(html)
	if len(styleHashes) != 1 {
		return "", fmt.Errorf(
			"buildCSPPolicy: want exactly one inline <style> block in index.html, found %d",
			len(styleHashes),
		)
	}
	return fmt.Sprintf(cspTemplate, strings.Join(hashes, " "), styleHashes[0]), nil
}

// composeGate wraps a session-create gate with one more blocking check: while `blocked`
// reports true, only SESSION CREATION answers 503, so kiro-cli never spawns before its
// own install finishes or before the manifest's tools are on PATH. List/close/title
// requests through the same doubly-mounted handler pass through, and the inner gate
// applies once this layer clears — which is how the blocking checks and the rate
// limit stack without knowing about each other.
func composeGate(inner func(http.Handler) http.Handler, blocked func() (bool, string)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		gated := inner(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != terminal.SessionsPath {
				gated.ServeHTTP(w, r)
				return
			}
			if block, reason := blocked(); block {
				// Retry-After matches the inner rate limit's 429 contract. A fixed short
				// hint: neither a tools convergence nor a kiro-cli download has a
				// predictable remaining time, so a cheap re-poll beats a computed date.
				w.Header().Set("Retry-After", "5")
				httpserver.WriteError(w, r, http.StatusServiceUnavailable, "not_ready", reason)
				return
			}
			gated.ServeHTTP(w, r)
		})
	}
}

// restorePendingGate holds session creation and the layout read with 503 while pending
// reports true, and passes every other request (the list included) to inner. The layout
// read is held because a page reads it as the signal that the set is complete; a default
// layout served mid-restore would be paired with a partial list. A held layout read
// reports the first blocked outer check, since the page waits on it and never reaches
// the create those checks gate; otherwise both routes report terminal.RestoringMessage.
func restorePendingGate(inner func(http.Handler) http.Handler, pending func() bool,
	outer ...func() (bool, string),
) func(http.Handler) http.Handler {
	layoutPath := terminal.SessionsSubtreePath + "layout"
	return func(next http.Handler) http.Handler {
		gated := inner(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			layoutRead := r.Method == http.MethodGet && r.URL.Path == layoutPath
			held := layoutRead || (r.Method == http.MethodPost && r.URL.Path == terminal.SessionsPath)
			if !held || !pending() {
				gated.ServeHTTP(w, r)
				return
			}
			if layoutRead {
				if reason, block := firstBlocked(outer); block {
					w.Header().Set("Retry-After", "5") // composeGate's hint for the same wait
					httpserver.WriteError(w, r, http.StatusServiceUnavailable, "not_ready", reason)
					return
				}
			}
			// One second: the restore itself takes milliseconds once both outer gates
			// open, and a longer hint would hold pages on the overlay after the tabs exist.
			w.Header().Set("Retry-After", "1")
			httpserver.WriteError(w, r, http.StatusServiceUnavailable, "not_ready", terminal.RestoringMessage)
		})
	}
}

// firstBlocked returns the reason of the first check that blocks, in order.
func firstBlocked(checks []func() (bool, string)) (string, bool) {
	for _, blocked := range checks {
		if block, reason := blocked(); block {
			return reason, true
		}
	}
	return "", false
}

// loopbackOnly admits only requests whose SOCKET PEER and Host header are both loopback
// and which carry no proxy/browser provenance header (mechanism: httpserver.LoopbackOnly).
// What stays here is the refusal wording, which names the guarded surface and the
// deployment's own listen hint — the 403 is the whole of what a refused caller is told.
func loopbackOnly(surface, hint string, next http.Handler) http.Handler {
	refuse := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpserver.WriteError(w, r, http.StatusForbidden, "loopback_only",
			surface+" is loopback-only. Call it from inside the container with curl "+hint)
	})
	return httpserver.LoopbackOnly(refuse)(next)
}

// sessionCostInterval is how often a contained session logs what it is currently costing.
// Fifteen minutes because the question it answers is "what is this tab costing me", not
// "watch a leak develop" — a stranded process is already reported the instant it happens,
// by the engine's reclaim WARN at teardown.
const sessionCostInterval = 15 * time.Minute
