// Package api holds the HTTP response helpers shared by the server.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// MaxJSONBody is the maximum size for JSON request bodies (1 MiB).
const MaxJSONBody = 1024 * 1024

// LimitBody wraps r.Body with MaxBytesReader to prevent oversized requests.
func LimitBody(w http.ResponseWriter, r *http.Request, maxBytes int64) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
}

// --- JSON response writers ---

func jsonHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

// WriteJSON encodes v as JSON with status 200.
func WriteJSON(w http.ResponseWriter, v any) {
	WriteJSONStatus(w, http.StatusOK, v)
}

// WriteJSONStatus encodes v with the given status code. Encode
// failures after the status has been committed are logged at Warn.
func WriteJSONStatus(w http.ResponseWriter, code int, v any) {
	jsonHeaders(w)
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("api: json encode failed after status committed",
			"code", code, "error", err)
	}
}

// --- Named error responses ---
//
// These keep the historical `error` field on the JSON wire format and
// add `code` + `request_id` (when a request context is available) for
// machine-readable handling. Existing clients that only read .error
// continue to work unchanged.

// BadRequest writes a 400. Prefer this signature where r is in scope.
func BadRequest(w http.ResponseWriter, r *http.Request, msg string) {
	WriteError(w, r, http.StatusBadRequest, "bad_request", msg)
}

// Conflict writes a 409.
func Conflict(w http.ResponseWriter, r *http.Request, msg string) {
	WriteError(w, r, http.StatusConflict, "conflict", msg)
}

// MethodNotAllowed writes a 405.
func MethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}

// Ok writes a 200 with {"ok": true}.
func Ok(w http.ResponseWriter) {
	WriteJSON(w, map[string]bool{"ok": true})
}
