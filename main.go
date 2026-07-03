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
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
	// Embed the IANA tz database so TZ (default Europe/Paris) is honored regardless
	// of the base image's zoneinfo; without it, on a base that ships no
	// /usr/share/zoneinfo, time.Local silently falls back to UTC.
	_ "time/tzdata"

	"github.com/cplieger/vibecli/internal/api"
)

//go:embed static
var staticFS embed.FS

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envInt parses key as an integer >= minVal, returning def when the var is unset.
// It errors on a non-integer or an out-of-range value.
func envInt(key string, def, minVal int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < minVal {
		return 0, fmt.Errorf("%s must be an integer >= %d, got %q", key, minVal, v)
	}
	return n, nil
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	addr := envOr("KWEB_ADDR", ":9848")
	if host, _, splitErr := net.SplitHostPort(addr); splitErr == nil {
		// Warn for any bind reachable beyond loopback: the wildcard forms
		// (empty host in ":9848", 0.0.0.0, ::) AND any specific routable IP
		// (LAN/public) are equally exposed. Only an explicit loopback bind
		// (127.0.0.0/8, ::1, or the "localhost" name) is safe to skip.
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			slog.Warn("serving an UNAUTHENTICATED kiro-cli shell on a non-loopback address; front it with an authenticating reverse proxy",
				"addr", addr,
				"hint", "any client that can reach this port gets a kiro-cli PTY with filesystem access to /workspace and the /config home (auth tokens, ssh keys, gitconfig)")
		}
	}
	cliPath := envOr("KIRO_CLI_PATH", "kiro-cli")
	workDir := envOr("KWEB_WORK_DIR", "/workspace")

	if _, err := os.Stat(workDir); err != nil {
		slog.Error("work directory missing",
			"work_dir", workDir, "error", err,
			"hint", "bind-mount a host directory to /workspace in compose.yaml")
		os.Exit(1)
	}

	// Cap concurrent kiro-cli chat sessions (browser tabs). Each is a full
	// kiro-cli process, so the default is deliberately modest; raise it with
	// KWEB_MAX_SESSIONS for a beefier host.
	maxSessions, err := envInt("KWEB_MAX_SESSIONS", 6, 1)
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	cmd := []string{cliPath, "chat"}

	mux := http.NewServeMux()
	var ready atomic.Bool

	mgr, err := registerRoutes(mux, &routeDeps{
		staticFS:    staticFS,
		cmd:         cmd,
		workDir:     workDir,
		ready:       &ready,
		maxSessions: maxSessions,
	})
	if err != nil {
		slog.Error("route registration failed", "error", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           http.NewCrossOriginProtection().Handler(api.RequestLogger(mux)),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", srv.Addr)
	if err != nil {
		slog.Error("listen failed", "addr", srv.Addr, "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("vibecli listening",
			"addr", addr, "cli_path", cliPath,
			"work_dir", workDir, "max_sessions", maxSessions)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server exited", "error", err)
			stop()
		}
	}()
	ready.Store(true)

	<-ctx.Done()
	ready.Store(false)
	slog.Info("shutting down", "cause", context.Cause(ctx))
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("server shutdown returned error", "error", err)
	}
	mgr.Shutdown()
}
