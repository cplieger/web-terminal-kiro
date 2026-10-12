package main

import (
	"context"
	"log/slog"

	"github.com/cplieger/web-terminal-engine/v6/terminal"
)

// sessionShutdowner is what the session teardown needs from the session manager.
type sessionShutdowner interface{ Shutdown(context.Context) error }

var _ sessionShutdowner = (*terminal.SessionManager)(nil)

// newSessionShutdown returns the engine's blocking teardown, on whatever budget the caller
// hands it. It cancels the base context first on every path, including webhttp.Run's error
// path, which skips the pre-drain hook: a tab-set save that captured the emptied manager
// must see a cancelled context, or it would save an empty set over tabs.json.
//
// An expiry means a session's teardown did not finish inside the grace, which is otherwise
// silent, and this is the one place a stranded session tree is reported at shutdown.
func newSessionShutdown(cancel context.CancelFunc, mgr sessionShutdowner) func(context.Context) {
	return func(ctx context.Context) {
		cancel()
		if err := mgr.Shutdown(ctx); err != nil {
			slog.Warn("session teardown did not finish within the shutdown grace",
				"grace", shutdownGrace, "error", err)
		}
	}
}
