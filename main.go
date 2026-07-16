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
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cplieger/envx"
	"github.com/cplieger/slogx"
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
	v := os.Getenv(key)
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
// The script never interpolates cliPath: it is passed as $0 (the argument after
// -c's script), so a path with spaces or shell metacharacters cannot break or
// inject into the script.
func sessionCommand(cliPath string) []string {
	const script = `if ! "$0" whoami >/dev/null 2>&1; then
printf '%s\n' 'kiro-cli is not signed in. Starting the device-flow sign-in:' 'open the URL it prints (tap or click it), confirm the code there, and the chat starts here on its own.' ''
"$0" login --use-device-flow || exit 1
fi
exec "$0" chat`
	return []string{"/bin/sh", "-c", script, cliPath}
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

	// TRUSTED_PROXIES names the reverse proxies (CIDRs or bare IPs) whose
	// X-Forwarded-For the access log may trust to recover the real client IP.
	// Unset/empty ⇒ nil ⇒ trust nothing ⇒ log the unspoofable socket peer (the
	// spoof-safe default for a directly-exposed deployment). See parseTrustedProxies.
	trustedProxies := parseTrustedProxies()

	// Concurrent kiro-cli chat sessions (browser tabs) are uncapped, like a
	// browser: managing tabs is the user's job.
	cmd := sessionCommand(cliPath)

	mux := http.NewServeMux()
	var ready atomic.Bool

	mgr, cspPolicy, err := registerRoutes(mux, &routeDeps{
		staticFS:        staticFS,
		cmd:             cmd,
		workDir:         workDir,
		ready:           &ready,
		kiroReadyMarker: kiroReadyMarker,
	})
	if err != nil {
		slog.Error("route registration failed", "error", err)
		os.Exit(1)
	}

	// Bind the listener before building the base context + server so the
	// listen-failure os.Exit(1) runs with no pending defer (gocritic
	// exitAfterDefer). srv.Addr == addr, so use addr directly here.
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		slog.Error("listen failed", "addr", addr, "error", err)
		os.Exit(1)
	}

	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()

	// buildHandler wraps mux in the middleware stack (see its doc comment for the
	// ordering rationale). webhttp.NewServer supplies the streaming-safe defaults
	// (ReadHeaderTimeout 10s, IdleTimeout 120s, Read/WriteTimeout unset) that the
	// hijacked /ws stream needs.
	srv := webhttp.NewServer(buildHandler(mux, trustedProxies, cspPolicy))
	srv.Addr = addr
	// BaseContext hands every request a context we can cancel on shutdown (see
	// the shutdown goroutine): the always-open /api/sessions/events SSE handler
	// returns only on r.Context().Done(), and srv.Shutdown does not interrupt an
	// active stream, so cancelling baseCtx is what unblocks the drain instead of
	// blocking the full grace window whenever a browser tab is open.
	srv.BaseContext = func(net.Listener) context.Context { return baseCtx }

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Flip readiness false and cancel in-flight request contexts the moment
	// shutdown is signalled, before webhttp.Run drains, so /api/health reports
	// 503 during the drain window. cancelBase unblocks the always-open
	// /api/sessions/events SSE handler (it returns only on r.Context().Done();
	// srv.Shutdown does not interrupt an active stream, so without this the drain
	// blocks the full grace window whenever a tab is open).
	go func() {
		<-ctx.Done()
		ready.Store(false)
		cancelBase()
		slog.Info("shutting down", "cause", context.Cause(ctx))
	}()

	slog.Info("web-terminal-kiro listening", "addr", addr, "cli_path", cliPath, "work_dir", workDir)
	ready.Store(true)

	if err := webhttp.Run(ctx, srv, ln, func(context.Context) { mgr.Shutdown() },
		webhttp.WithShutdownGrace(5*time.Second)); err != nil {
		slog.Error("http server exited", "error", err)
		mgr.Shutdown()
		os.Exit(1) //nolint:gocritic // exitAfterDefer: a failed Serve must exit non-zero; the deferred stop()/cancelBase() only release signal+context state the process exit reclaims anyway.
	}
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
//   - CrossOriginProtection — the stdlib cross-origin/CSRF guard, kept
//     innermost (its long-standing position directly in front of the routes) so
//     it rejects a forged cross-origin unsafe request with 403.
func buildHandler(mux http.Handler, trustedProxies []*net.IPNet, csp string) http.Handler {
	return webhttp.Chain(mux,
		webhttp.Logging(
			webhttp.WithLogger(slog.Default()),
			webhttp.WithSkipPaths("/ws", "/api/sessions/events"),
			webhttp.WithClientIP(trustedProxies...),
		),
		webhttp.Recoverer(webhttp.WithRecoverLogger(slog.Default())),
		webhttp.SecurityHeaders(webhttp.WithCSP(csp)),
		http.NewCrossOriginProtection().Handler,
	)
}
