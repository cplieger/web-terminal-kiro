// handle_logout.go implements the POST /api/logout endpoint.

package auth

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"

	"github.com/cplieger/vibecli/internal/api"
)

// LogoutResponse is the typed HTTP response for the logout endpoint.
// JSON tags match the old map keys exactly so the wire format is
// byte-identical: {"output":"..."} on success, {"output":"...","error":"..."}
// on failure.
type LogoutResponse struct {
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
}

// handleLogout shells out to `kiro-cli logout`, feeding "y\n" on stdin
// to acknowledge the confirmation prompt, and returns the
// stdout+stderr as "output".
func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		api.MethodNotAllowed(w, r)
		return
	}
	slog.Info("logout: request received",
		"remote_addr", r.RemoteAddr,
		"user_agent", r.Header.Get("User-Agent"))
	ctx, cancel := context.WithTimeout(r.Context(), h.timeouts.Logout)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.cliPath, "logout") // #nosec G204 -- cliPath is operator-controlled
	setLoginProcAttr(cmd)
	cmd.Stdin = strings.NewReader("y\n")
	var buf bytes.Buffer
	lw := &limitedWriter{W: &buf, N: logoutMaxOutput}
	cmd.Stdout = lw
	cmd.Stderr = lw
	cliErr := runCLI(ctx, cmd)
	out := buf.Bytes()
	result := LogoutResponse{Output: api.SanitizeOutput(string(out))}
	if cliErr != nil {
		switch cliErr.Kind {
		case CLIErrTimeout:
			slog.Warn("logout: kiro-cli timed out",
				"timeout", h.timeouts.Logout, "output_bytes", len(out))
			audit(r, slog.LevelWarn, AuditLogout, false,
				slog.String("reason", "timeout"),
				slog.Duration("timeout", h.timeouts.Logout))
			result.Error = "logout timed out"
			api.WriteJSONStatus(w, http.StatusGatewayTimeout, result)
			return
		case CLIErrNotFound:
			slog.Error("logout: kiro-cli binary not found",
				"cli_path", h.cliPath)
			audit(r, slog.LevelWarn, AuditLogout, false,
				slog.String("reason", "binary_not_found"))
			result.Error = "logout unavailable"
			api.WriteJSONStatus(w, http.StatusServiceUnavailable, result)
			return
		default:
			slog.Warn("logout: kiro-cli failed",
				"error", cliErr.Err, "output_bytes", len(out))
			audit(r, slog.LevelWarn, AuditLogout, false,
				slog.String("reason", "subprocess_error"))
			result.Error = "logout failed"
			api.WriteJSONStatus(w, http.StatusBadGateway, result)
			return
		}
	}
	slog.Info("logout: completed", "output_bytes", len(out))
	audit(r, slog.LevelInfo, AuditLogout, true)
	api.WriteJSON(w, result)
}
