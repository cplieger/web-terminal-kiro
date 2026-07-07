// Package main is vibecli — a browser terminal wrapped around kiro-cli.
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
// The single step runs tsgo to build the JS bundle from static-src.
// The CSS bundle is concatenated by the Dockerfile at build time;
// no go:generate step for it.
//
//go:generate tsgo --project static-src/tsconfig.json

import (
	"context"
	"embed"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cplieger/webhttp"
)

//go:embed static
var staticFS embed.FS

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

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

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{ReplaceAttr: utcTimeAttr})))

	addr := envOr("KWEB_ADDR", ":9848")
	// Warn for any bind reachable beyond loopback (see isExposedBind): a client
	// that can reach this port gets an UNAUTHENTICATED kiro-cli PTY.
	if isExposedBind(addr) {
		slog.Warn("serving an UNAUTHENTICATED kiro-cli shell on a non-loopback address; front it with an authenticating reverse proxy",
			"addr", addr,
			"hint", "any client that can reach this port gets a kiro-cli PTY with filesystem access to /workspace and the /config home (auth tokens, ssh keys, gitconfig)")
	}
	cliPath := envOr("KIRO_CLI_PATH", "kiro-cli")
	workDir := envOr("KWEB_WORK_DIR", "/workspace")
	// Readiness marker written by entrypoint.sh after it verifies a runnable,
	// correctly-versioned kiro-cli. Empty outside the container (bare `go run`,
	// tests) so /api/health keeps pure-listener readiness there.
	kiroReadyMarker := envOr("KIRO_CLI_READY_MARKER", "")

	if fi, err := os.Stat(workDir); err != nil || !fi.IsDir() {
		slog.Error("work directory missing or not a directory",
			"work_dir", workDir, "error", err,
			"hint", "bind-mount a host directory to /workspace in compose.yaml")
		os.Exit(1)
	}

	// Concurrent kiro-cli chat sessions (browser tabs) are uncapped, like a
	// browser: managing tabs is the user's job.
	cmd := []string{cliPath, "chat"}

	mux := http.NewServeMux()
	var ready atomic.Bool

	mgr, err := registerRoutes(mux, &routeDeps{
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
	srv := webhttp.NewServer(buildHandler(mux))
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

	slog.Info("vibecli listening", "addr", addr, "cli_path", cliPath, "work_dir", workDir)
	ready.Store(true)

	if err := webhttp.Run(ctx, srv, ln, func(context.Context) { mgr.Shutdown() },
		webhttp.WithShutdownGrace(5*time.Second)); err != nil {
		slog.Error("http server exited", "error", err)
		mgr.Shutdown()
		os.Exit(1)
	}
}

// utcTimeAttr is a slog ReplaceAttr that renders the record's built-in time
// key in UTC, so log-line timestamps are zone-stable regardless of the
// container's TZ (the fleet logs-in-UTC standard). It rewrites only the
// top-level time attribute; a user attribute that happens to share the "time"
// key inside a group is left untouched.
func utcTimeAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
		a.Value = slog.TimeValue(a.Value.Time().UTC())
	}
	return a
}

// buildHandler wraps the route mux in vibecli's middleware stack via
// webhttp.Chain. Chain(h, A, B, C, D) == A(B(C(D(h)))), so the first entry is
// the outermost wrapper; a request flows Logging -> Recoverer ->
// SecurityHeaders -> CrossOriginProtection -> mux, and the response unwinds the
// other way.
//
//   - Logging — webhttp's access logger. Outermost so it observes every final
//     status, including a recovered 500 and a cross-origin 403. WithClientIP()
//     with NO trusted ranges logs the real socket peer as the `client_ip`
//     field: vibecli is LAN-direct (no reverse proxy), so the unspoofable TCP
//     peer host IS the client. This replaces the former app-side
//     api.RequestLogger, whose only reason to exist was the `remote`
//     (host:port) field; `client_ip` (host only, no port) is its successor.
//     Skips the long-lived streams (/ws and the /api/sessions/events SSE) so
//     neither emits a misleading open-time access line; the request id is still
//     minted, echoed, and threaded on those paths.
//   - Recoverer — turns a downstream panic into a logged 500 (inside the logger
//     so the access line records the 500, not the recorder's default 200).
//   - SecurityHeaders — the fleet baseline (nosniff, X-Frame-Options: DENY,
//     Referrer-Policy) on every response. No CSP: vibecli serves an HTML
//     terminal UI (fonts + WebSocket) and a wrong policy would silently break
//     it. X-Frame-Options DENY is the default and is safe because vibecli is
//     never embedded in a frame. Placed outside CrossOriginProtection so even a
//     rejected cross-origin request still carries the headers.
//   - CrossOriginProtection — the stdlib cross-origin/CSRF guard, kept
//     innermost (its long-standing position directly in front of the routes) so
//     it rejects a forged cross-origin unsafe request with 403.
func buildHandler(mux http.Handler) http.Handler {
	return webhttp.Chain(mux,
		// requires webhttp >= v1.2.0 (WithClientIP); local build via go.work replace until released.
		webhttp.Logging(
			webhttp.WithLogger(slog.Default()),
			webhttp.WithSkipPaths("/ws", "/api/sessions/events"),
			webhttp.WithClientIP(),
		),
		webhttp.Recoverer(webhttp.WithRecoverLogger(slog.Default())),
		webhttp.SecurityHeaders(),
		http.NewCrossOriginProtection().Handler,
	)
}
