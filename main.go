// Package main is vibecli — a browser terminal wrapped around kiro-cli.
// Each /ws connection exec's `kiro-cli chat` directly in a PTY. Server-side
// state lives in vibecli's own VT screen (internal/vt): on reconnect, the
// current cell snapshot is replayed to the client. No external multiplexer.
package main

// Build inputs for `go:embed static`. The Dockerfile invokes the same
// commands inline; running `go generate ./...` locally produces the
// same `static/` tree so `go run .` and `go build .` work without the
// container.
//
// Order matters: wire-codegen first (TS sources reference its output),
// then tsgo for the JS bundle (last because it fails fast if the
// generated input is missing). The CSS bundle is concatenated by the
// Dockerfile at build time; no go:generate step for it.
//
//go:generate tsgo --project static-src/tsconfig.json

import (
	"context"
	"embed"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

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

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	addr := envOr("KWEB_ADDR", ":9848")
	cliPath := envOr("KIRO_CLI_PATH", "kiro-cli")
	workDir := envOr("KWEB_WORK_DIR", "/workspace")

	if _, err := os.Stat(workDir); err != nil {
		slog.Error("work directory missing",
			"work_dir", workDir, "error", err,
			"hint", "bind-mount a host directory to /workspace in compose.yaml")
		os.Exit(1)
	}

	cmd := []string{cliPath, "chat"}

	mux := http.NewServeMux()
	var ready atomic.Bool

	term, err := registerRoutes(mux, &routeDeps{
		staticFS: staticFS,
		cmd:      cmd,
		workDir:  workDir,
		ready:    &ready,
	})
	if err != nil {
		slog.Error("route registration failed", "error", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           http.NewCrossOriginProtection().Handler(api.RequestLogger(mux)),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", srv.Addr)
	if err != nil {
		slog.Error("listen failed", "addr", srv.Addr, "error", err)
		stop()
		return
	}

	go func() {
		slog.Info("vibecli listening",
			"addr", addr, "cli_path", cliPath,
			"work_dir", workDir)
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
	term.Shutdown()
}
