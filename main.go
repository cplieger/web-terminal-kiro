// Package main is web-terminal-kiro — a browser terminal wrapped around kiro-cli.
// Each /ws connection exec's `kiro-cli chat` directly in a PTY. Server-side
// state lives in the web-terminal-engine VT screen buffer (its vt package):
// on reconnect, the current cell snapshot is replayed to the client. No
// external multiplexer.
package main

// Build inputs for `go:embed static`. The Dockerfile invokes the same
// commands inline; running `go generate ./...` locally produces the
// same `static/` tree so `go run .` and `go build .` work without the
// container.
//
// The single step runs tsc (the TS7 native compiler, from static-src's
// @typescript/native devDependency) to build the JS bundle from static-src.
// The CSS bundle is concatenated by the Dockerfile at build time;
// no go:generate step for it.
//
//go:generate static-src/node_modules/.bin/tsc --project static-src/tsconfig.json

import (
	"context"
	"embed"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cplieger/envx"
	"github.com/cplieger/slogx"
	"github.com/cplieger/toolbelt/v2"
	"github.com/cplieger/webhttp"
)

//go:embed static
var staticFS embed.FS

// isExposedBind reports whether addr binds beyond loopback. The wildcard forms
// (empty host in ":9848", 0.0.0.0, ::) AND any specific routable IP (LAN/public)
// are exposed; only an explicit loopback bind (127.0.0.0/8, ::1, or the
// "localhost" name) is safe. An addr that does not parse as host:port is treated
// as not exposed (no warn) — it will fail at Listen anyway.
func isExposedBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

// parseTrustedProxies reads a comma-separated list of CIDRs / bare IPs from the
// TRUSTED_PROXIES env var into the trusted-proxy set the access log's client-IP
// resolver consults (webhttp.WithClientIP -> ClientIP). It delegates the
// CIDR/bare-IP parsing to the shared webhttp.ParseCIDRs helper, which trims
// whitespace, skips blanks, treats a bare IP as a single host (/32 or /128), and
// reports invalid entries separately.
//
// It is intentionally LENIENT: a malformed entry is logged (named) at Warn and
// skipped, and the valid subset is used, rather than aborting startup — one typo
// in an operator's proxy list must not disable proxy awareness entirely, and it
// must never fall open. An unset or empty var yields nil, i.e. "trust nothing",
// so ClientIP ignores X-Forwarded-For and logs the spoof-proof socket peer — the
// correct default for a directly-exposed deployment. Behind a reverse proxy, set
// the var to the proxy's CIDR(s) so the access log records the real client.
func parseTrustedProxies() []*net.IPNet {
	const key = "TRUSTED_PROXIES"
	v := envx.String(key, "")
	if v == "" {
		return nil
	}
	nets, invalid := webhttp.ParseCIDRs(strings.Split(v, ","))
	if len(invalid) > 0 {
		slog.Warn("ignoring malformed "+key+" entries; using the valid proxy set",
			"invalid", invalid,
			"hint", "each entry must be a CIDR (e.g. 10.0.0.0/8) or a bare IP (e.g. 192.168.1.5)")
	}
	return nets
}

// parseAllowedHosts reads the comma-separated KWEB_ALLOWED_HOSTS list of exact
// hostnames / IPs this server answers for, canonicalized via canonicalHost so
// the runtime comparison is exact-match (no wildcards, no suffix logic). It
// returns nil when the var is unset or empty — "any Host accepted", the
// backward-compatible default; main warns about the DNS-rebinding exposure
// that default leaves open (see hostAllowlist).
func parseAllowedHosts() map[string]struct{} {
	v := envx.String("KWEB_ALLOWED_HOSTS", "")
	if v == "" {
		return nil
	}
	allowed := make(map[string]struct{})
	for entry := range strings.SplitSeq(v, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		allowed[canonicalHost(entry)] = struct{}{}
	}
	if len(allowed) == 0 {
		return nil
	}
	return allowed
}

// canonicalHost normalizes an HTTP Host header value (or a configured
// allowlist entry) for exact comparison: strip an optional :port, strip
// IPv6 brackets, lowercase, trim a trailing FQDN dot, and canonicalize IP
// literals through net.ParseIP so textually different spellings of the same
// address (e.g. ::1 vs 0:0:0:0:0:0:0:1) compare equal.
func canonicalHost(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}

// hostAllowlist rejects any request whose Host header does not canonicalize
// to a configured entry, closing the DNS-rebinding hole that same-origin
// checks alone leave open: a rebinding attack makes an attacker-controlled
// hostname resolve to this server, so Origin and Host AGREE (both carry the
// attacker's name) and http.CrossOriginProtection plus the WebSocket
// same-origin check both pass — same-origin proves only that the two headers
// match, never that the hostname is authorized for this remote-shell service
// (CWE-346). Comparing r.Host against an exact allowlist breaks that chain:
// the attacker's hostname is not on it.
//
// Only r.Host is consulted — X-Forwarded-Host is deliberately ignored (it is
// client-controlled and this check must hold on the direct-exposure path).
// A nil/empty allowlist disables the check entirely (backward compatible;
// main warns). Runs before CrossOriginProtection so an unauthorized host is
// rejected before any terminal route, session creation included.
func hostAllowlist(allowed map[string]struct{}) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if len(allowed) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := allowed[canonicalHost(r.Host)]; !ok {
				webhttp.WriteJSONStatus(w, http.StatusForbidden, map[string]string{
					errorField: "host not allowed; add it to KWEB_ALLOWED_HOSTS to serve this hostname",
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// sessionCommand builds the per-session PTY command: `kiro-cli chat` behind a
// sign-in guard. When no identity is present (`whoami` exits non-zero, verified
// against the pinned build: 0 logged in, 1 not), the guard first runs
// `kiro-cli login --use-device-flow` IN the terminal, then execs chat in the
// same PTY on success.
//
// The device flow is the only sign-in that works here. kiro-cli's default flow
// opens a browser on THIS host — a headless container, so the open fails and
// chat exits, leaving a dead session (historically: a stuck loading screen and
// a flashing "Reconnecting…" after the engine's 4001 close). Its PKCE localhost
// callback could not be reached from the user's machine even if a browser
// existed. The device flow instead prints a verification URL + code inline; the
// terminal UI linkifies URLs, so the user opens it in their OWN browser (any
// device), confirms, and the chat starts in the same tab. Method/license
// selection stays interactive inside the TUI — nothing org-specific is baked
// into the image.
//
// The script never interpolates cliPath or chatArgs: cliPath is passed as $0
// (the argument after -c's script) and chatArgs ride the positional params
// (`"$@"`), so a path or flag with spaces or shell metacharacters cannot break
// or inject into the script. chatArgs (operator flags from KIRO_CLI_CHAT_ARGS,
// e.g. --v3) are appended to the chat invocation only — login and whoami never
// see them.
func sessionCommand(cliPath string, chatArgs ...string) []string {
	const script = `if ! command -v "$0" >/dev/null 2>&1; then
printf '%s\n' 'kiro-cli is not installed or not on PATH. The first-boot install may have failed; check the container logs and /api/health.'
exit 1
fi
if ! "$0" whoami >/dev/null 2>&1; then
printf '%s\n' 'kiro-cli is not signed in. Starting the device-flow sign-in:' 'open the URL it prints (tap or click it), confirm the code there, and the chat starts here on its own.' ''
"$0" login --use-device-flow || exit 1
fi
exec "$0" chat "$@"`
	return append([]string{"/bin/sh", "-c", script, cliPath}, chatArgs...)
}

func main() {
	slogx.Setup(slogx.Options{})

	addr := envx.String("KWEB_ADDR", ":9848")
	// Warn for any bind reachable beyond loopback (see isExposedBind): a client
	// that can reach this port gets an UNAUTHENTICATED kiro-cli PTY.
	if isExposedBind(addr) {
		slog.Warn("serving an UNAUTHENTICATED kiro-cli shell on a non-loopback address; front it with an authenticating reverse proxy",
			"addr", addr,
			"hint", "any client that can reach this port gets a kiro-cli PTY with filesystem access to /workspace and the /config home (auth tokens, ssh keys, gitconfig)")
	}
	cliPath := envx.String("KIRO_CLI_PATH", "kiro-cli")
	workDir := envx.String("KWEB_WORK_DIR", "/workspace")
	// Readiness marker written by entrypoint.sh after it verifies a runnable,
	// correctly-versioned kiro-cli. Empty outside the container (bare `go run`,
	// tests) so /api/health keeps pure-listener readiness there.
	kiroReadyMarker := envx.String("KIRO_CLI_READY_MARKER", "")

	if fi, err := os.Stat(workDir); err != nil || !fi.IsDir() {
		slog.Error("work directory missing or not a directory",
			"work_dir", workDir, "error", err,
			"hint", "bind-mount a host directory to /workspace in compose.yaml")
		os.Exit(1)
	}

	// Tools engine (cplieger/toolbelt): declarative provisioning of the
	// /config/tools tree from the tools.json manifest, replacing the
	// retired setup-tools.sh. Constructed only when the config dir
	// exists (the container's /config bind mount); bare `go run` and
	// tests outside the container run without a tools surface.
	tools := startTools(baseTools{
		configDir:   envx.String("KWEB_CONFIG_DIR", "/config"),
		catalogPath: envx.String("TOOL_CATALOG_PATH", "/app/tool-catalog.json"),
	})

	// TRUSTED_PROXIES names the reverse proxies (CIDRs or bare IPs) whose
	// X-Forwarded-For the access log may trust to recover the real client IP.
	// Unset/empty ⇒ nil ⇒ trust nothing ⇒ log the unspoofable socket peer (the
	// spoof-safe default for a directly-exposed deployment). See parseTrustedProxies.
	trustedProxies := parseTrustedProxies()

	// KWEB_ALLOWED_HOSTS names the exact hostnames/IPs this server answers
	// for; anything else is rejected before the terminal routes (see
	// hostAllowlist for the DNS-rebinding rationale). Unset ⇒ permissive
	// (backward compatible), but that leaves rebinding open even on a
	// loopback/private bind — the attack rides the victim's browser, so the
	// README's "keep it loopback" mitigation does not cover it. Warn.
	allowedHosts := parseAllowedHosts()
	if allowedHosts == nil {
		slog.Warn("KWEB_ALLOWED_HOSTS is unset; any Host header is accepted, leaving DNS rebinding open even on loopback/private binds",
			"hint", "set KWEB_ALLOWED_HOSTS to the exact hostnames/IPs you browse to (e.g. localhost,192.168.1.5,webterm.example.com)")
	}

	// KIRO_CLI_CHAT_ARGS appends extra launch flags to the per-session
	// `kiro-cli chat` command (whitespace-separated, e.g. "--v3" or
	// "--agent-engine v3 --effort high"). Empty ⇒ no extra flags. The values
	// reach chat as positional shell params (see sessionCommand), never via
	// string splicing.
	chatArgs := strings.Fields(envx.String("KIRO_CLI_CHAT_ARGS", ""))
	if len(chatArgs) > 0 {
		slog.Info("appending extra kiro-cli chat flags", "chat_args", chatArgs)
	}

	// Concurrent kiro-cli chat sessions (browser tabs) are uncapped, like a
	// browser: managing tabs is the user's job.
	cmd := sessionCommand(cliPath, chatArgs...)

	mux := http.NewServeMux()
	var ready atomic.Bool

	mgr, cspPolicy, err := registerRoutes(mux, &routeDeps{
		staticFS:        staticFS,
		cmd:             cmd,
		workDir:         workDir,
		ready:           &ready,
		kiroReadyMarker: kiroReadyMarker,
		tools:           tools.engine,
		toolsSyncing:    tools.syncing,
		toolsState:      tools.state,
	})
	if err != nil {
		slog.Error("route registration failed", "error", err)
		tools.close()
		os.Exit(1)
	}

	// Bind the listener before building the base context + server so the
	// listen-failure os.Exit(1) runs with no pending defer (gocritic
	// exitAfterDefer).
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		slog.Error("listen failed", "addr", addr, "error", err)
		tools.close()
		os.Exit(1)
	}

	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()

	// buildHandler wraps mux in the middleware stack (see its doc comment for the
	// ordering rationale). webhttp.NewServer supplies the streaming-safe defaults
	// (ReadHeaderTimeout 10s, IdleTimeout 120s, Read/WriteTimeout unset) that the
	// hijacked /ws stream needs.
	srv := webhttp.NewServer(buildHandler(mux, trustedProxies, cspPolicy, allowedHosts))
	// BaseContext hands every request a context we can cancel on shutdown (see
	// the shutdown goroutine): the always-open /api/sessions/events SSE handler
	// returns only on r.Context().Done(), and srv.Shutdown does not interrupt an
	// active stream, so cancelling baseCtx is what unblocks the drain instead of
	// blocking the full grace window whenever a browser tab is open.
	srv.BaseContext = func(net.Listener) context.Context { return baseCtx }

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("web-terminal-kiro listening", "addr", addr, "cli_path", cliPath, "work_dir", workDir)
	ready.Store(true)

	// The pre-drain hook flips readiness false and cancels in-flight request
	// contexts before webhttp.Run drains, so /api/health reports 503 during the
	// drain window. cancelBase unblocks the always-open /api/sessions/events SSE
	// handler (it returns only on r.Context().Done(); srv.Shutdown does not
	// interrupt an active stream, so without this the drain blocks the full
	// grace window whenever a tab is open).
	if err := webhttp.Run(ctx, srv, ln, func(context.Context) { mgr.Shutdown() },
		webhttp.WithShutdownGrace(5*time.Second),
		webhttp.WithPreDrain(func(context.Context) {
			ready.Store(false)
			cancelBase()
			slog.Info("shutting down", "cause", context.Cause(ctx))
		})); err != nil {
		slog.Error("http server exited", "error", err)
		mgr.Shutdown()
		tools.close()
		os.Exit(1) //nolint:gocritic // exitAfterDefer: a failed Serve must exit non-zero; the deferred stop()/cancelBase() only release signal+context state the process exit reclaims anyway.
	}
	tools.close()
}

// baseTools carries startTools's inputs (env-resolved paths).
type baseTools struct {
	configDir   string
	catalogPath string
}

// toolsRuntime is the running tools subsystem handed to the routes: the
// engine (nil when disabled), the session-create gate predicate, and the
// health detail. A zero value (engine nil, funcs nil) means "no tools
// surface" — bare `go run` and tests outside the container.
type toolsRuntime struct {
	engine *toolbelt.Engine
	// syncing reports whether the boot convergence pass is still
	// running; session creation is gated on it so the first kiro-cli
	// never spawns before the manifest's tools are on PATH.
	syncing func() bool
	// state is the /api/health informational detail:
	// syncing | ok | degraded.
	state func() string
}

func (t *toolsRuntime) close() {
	if t.engine != nil {
		t.engine.Close()
	}
}

// startTools builds the toolbelt engine and launches the boot
// convergence pass (bind-first: the listener comes up while installs
// run; only session CREATION waits, via the syncing gate). The gate
// lifts regardless of per-tool failures — degraded-not-dead, matching
// the retired setup-tools.sh warn-and-continue posture — and the
// health detail records the verdict. After convergence an async update
// pass refreshes unpinned tools, and a boot warning nudges when no
// language server is enabled (kiro-cli scans PATH for LSPs at session
// start).
func startTools(cfg baseTools) toolsRuntime {
	if fi, err := os.Stat(cfg.configDir); err != nil || !fi.IsDir() {
		slog.Warn("tools engine disabled: config dir missing",
			"config_dir", cfg.configDir,
			"hint", "bind-mount the persistent config volume (compose.yaml) or set KWEB_CONFIG_DIR")
		return toolsRuntime{}
	}
	eng, err := toolbelt.New(&toolbelt.Config{
		ConfigDir:   cfg.configDir,
		ToolsDir:    filepath.Join(cfg.configDir, "tools"),
		CatalogPath: cfg.catalogPath,
		Seed:        toolbelt.DefaultSeed(),
		System:      []string{"git", "jq", "curl", "unzip", "xz", "ssh", "tar", "bash"},
		Logger:      slog.Default(),
	})
	if err != nil {
		slog.Error("tools engine failed to start; continuing without it", "error", err)
		return toolsRuntime{}
	}

	var syncing atomic.Bool
	var verdict atomic.Value // string: syncing | ok | degraded
	verdict.Store("syncing")
	finish := func(v string) {
		verdict.Store(v)
		syncing.Store(false)
	}

	job, rerr := eng.Reconcile(toolbelt.ReconcileMissing)
	switch {
	case rerr != nil:
		slog.Warn("tools: boot reconcile not enqueued", "error", rerr)
		finish("degraded")
		warnIfNoLSPEnabled(eng)
	case job == nil: // empty manifest: nothing to converge
		finish("ok")
		warnIfNoLSPEnabled(eng)
	default:
		syncing.Store(true)
		go func() {
			final, werr := eng.Wait(context.Background(), job.ID)
			switch {
			case werr != nil:
				slog.Warn("tools: boot reconcile wait failed", "error", werr)
				finish("degraded")
			case final.State != toolbelt.JobDone:
				slog.Warn("tools: boot reconcile finished degraded",
					"state", final.State, "error", final.Error)
				finish("degraded")
			default:
				slog.Info("tools: boot reconcile converged")
				finish("ok")
			}
			// Freshness pass for unpinned entries, off the boot path
			// (version-check network never holds the session gate).
			if _, uerr := eng.Update(); uerr != nil {
				slog.Warn("tools: update pass not enqueued", "error", uerr)
			}
			warnIfNoLSPEnabled(eng)
		}()
	}
	return toolsRuntime{
		engine:  eng,
		syncing: syncing.Load,
		state:   func() string { s, _ := verdict.Load().(string); return s },
	}
}

// warnIfNoLSPEnabled logs the code-intelligence nudge when no
// language-server entry is enabled: kiro-cli scans PATH for language
// servers at session start, so a box without one silently lacks code
// intelligence. Detection uses the inventory's catalog-derived Lsp
// marker, so any enabled LSP (seeded template or hand-added) silences
// it.
func warnIfNoLSPEnabled(e *toolbelt.Engine) {
	inv, err := e.Inventory()
	if err != nil {
		slog.Debug("tools: inventory read failed; skipping the language-server nudge", "error", err)
		return
	}
	for i := range inv.Tools {
		if inv.Tools[i].Lsp && !inv.Tools[i].Disabled {
			return
		}
	}
	slog.Warn("no language servers enabled; kiro code intelligence will be limited",
		"hint", `enable gopls (Go), typescript-language-server (TypeScript), or pyright (Python): set "disabled": false in /config/tools.json and restart, or use the loopback tools API`)
}

// buildHandler wraps the route mux in web-terminal-kiro's middleware stack via
// webhttp.Chain. Chain(h, A, B, C, D) == A(B(C(D(h)))), so the first entry is
// the outermost wrapper; a request flows Logging -> Recoverer ->
// SecurityHeaders -> CrossOriginProtection -> mux, and the response unwinds the
// other way.
//
//   - Logging — webhttp's access logger. Outermost so it observes every final
//     status, including a recovered 500 and a cross-origin 403. WithClientIP is
//     passed the TRUSTED_PROXIES set (parseTrustedProxies) as the `client_ip`
//     field's trusted-proxy ranges: unset/empty ⇒ trust nothing, so `client_ip`
//     is the unspoofable socket peer and X-Forwarded-For is ignored — the
//     spoof-safe default for a directly-exposed deployment. Behind a reverse
//     proxy, TRUSTED_PROXIES names the proxy CIDR(s) so `client_ip` resolves to
//     the real client from a trusted XFF instead of the proxy's own address.
//     This replaces the former app-side api.RequestLogger, whose only reason to
//     exist was the `remote` (host:port) field; `client_ip` (host only, no port)
//     is its successor. Skips the long-lived streams (/ws and the
//     /api/sessions/events SSE) so neither emits a misleading open-time access
//     line; the request id is still minted, echoed, and threaded on those paths.
//   - Recoverer — turns a downstream panic into a logged 500 (inside the logger
//     so the access line records the 500, not the recorder's default 200).
//   - SecurityHeaders — the fleet baseline (nosniff, X-Frame-Options: DENY,
//     Referrer-Policy) plus the app's hash-pinned Content-Security-Policy
//     (csp, built fail-loud by buildCSPPolicy from the embedded index.html —
//     the same script-src sha256 pinning web-terminal-server ships, closing
//     the family-drift gap where this app served the same embedded-static +
//     inline-importmap pattern with no CSP at all). X-Frame-Options DENY is
//     the default and is consistent with the CSP's frame-ancestors 'none' —
//     web-terminal-kiro is never embedded in a frame. Placed outside
//     CrossOriginProtection so even a rejected cross-origin request still
//     carries the headers.
//   - hostAllowlist — the KWEB_ALLOWED_HOSTS exact-host check (see its doc
//     comment for the DNS-rebinding rationale). Placed before
//     CrossOriginProtection because rebinding makes Origin and Host agree, so
//     the origin check alone cannot reject it; kept inside SecurityHeaders so
//     even a rejected host gets the baseline headers and an access-log line.
//     A nil allowlist (env unset) collapses to a pass-through.
//   - CrossOriginProtection — the stdlib cross-origin/CSRF guard, kept
//     innermost (its long-standing position directly in front of the routes) so
//     it rejects a forged cross-origin unsafe request with 403.
func buildHandler(mux http.Handler, trustedProxies []*net.IPNet, csp string, allowedHosts map[string]struct{}) http.Handler {
	return webhttp.Chain(mux,
		webhttp.Logging(
			webhttp.WithLogger(slog.Default()),
			webhttp.WithSkipPaths("/ws", "/api/sessions/events"),
			webhttp.WithClientIP(trustedProxies...),
		),
		webhttp.Recoverer(webhttp.WithRecoverLogger(slog.Default())),
		webhttp.SecurityHeaders(webhttp.WithCSP(csp)),
		hostAllowlist(allowedHosts),
		http.NewCrossOriginProtection().Handler,
	)
}
