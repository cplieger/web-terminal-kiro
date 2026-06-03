// Package api: request logging middleware, typed error envelope, and
// JSON body-decode helper. Lives in this file so all "HTTP plumbing"
// helpers stay grouped; http.go owns response writers and sanitisation.

package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"vibecli/internal/metrics"
)

// RequestID is the canonical HTTP header carrying the per-request id.
// We accept inbound values (validated for shape; see RequestLogger) and
// echo them back so a reverse proxy (Caddy, Traefik) that injects an
// id can correlate logs end-to-end. When absent, RequestLogger mints
// a fresh one. Header naming follows the X-Request-ID de-facto standard.
const RequestID = "X-Request-ID"

type ctxKey struct{}

// RequestIDFromContext returns the request id stored by RequestLogger,
// or "" if the context does not carry one.
func RequestIDFromContext(ctx context.Context) string {
	v, ok := ctx.Value(ctxKey{}).(string)
	if !ok {
		return ""
	}
	return v
}

// RequestLogger wraps next with method/path/status/latency/request-id
// access logging at slog.Info. Skips noisy paths (the WebSocket /ws
// handler runs for the lifetime of a session and should not emit per-
// connection access logs at session-open time; the WS handler logs
// its own lifecycle events).
//
// Each request gets a stable id available via RequestIDFromContext.
// An inbound X-Request-Id header is reused when it matches the
// validated shape (alphanumeric + dashes + underscores, 1..64 chars);
// otherwise a 16-byte random hex id is generated. The id is also set
// on the response so callers can correlate without server logs.
func RequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /ws is a long-lived upgrade; logging at request-time would
		// just mark "session opened" without latency. The terminal
		// package logs its own lifecycle.
		if r.URL.Path == "/ws" {
			next.ServeHTTP(w, r)
			return
		}

		id := requestIDOrNew(r.Header.Get(RequestID))
		w.Header().Set(RequestID, id)
		ctx := context.WithValue(r.Context(), ctxKey{}, id)

		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rw, r.WithContext(ctx))
		dur := time.Since(start)

		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration_ms", dur.Milliseconds(),
			"request_id", id,
			"remote", r.RemoteAddr,
		)
		metrics.HTTPRequests.Inc(r.Method, strconv.Itoa(rw.status))
		metrics.HTTPDuration.Observe(dur.Seconds())
	})
}

// statusRecorder captures the status code so RequestLogger can report
// it. WriteHeader-not-called means 200 (Go's default).
type statusRecorder struct {
	http.ResponseWriter

	status      int
	wroteHeader bool
}

func (sr *statusRecorder) WriteHeader(code int) {
	if !sr.wroteHeader {
		sr.status = code
		sr.wroteHeader = true
	}
	sr.ResponseWriter.WriteHeader(code)
}

// requestIDOrNew returns inbound when it is valid, otherwise mints a
// new 32-char hex id. Validation: 1..64 chars, alphanumeric / dash /
// underscore only. Defends against a header-injection vector where
// inbound text is logged or echoed back unsanitised.
func requestIDOrNew(inbound string) string {
	if validRequestID(inbound) {
		return inbound
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read on Linux uses getrandom(2) and effectively cannot
		// fail; if it does, fall back to a timestamp-based id so we
		// still set a value rather than crashing the request.
		return time.Now().UTC().Format("20060102T150405.000000000")
	}
	return hex.EncodeToString(b[:])
}

func validRequestID(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// APIError is the typed error envelope. The JSON wire format keeps
// the historical `error` field as the primary message so existing
// clients keep working; new fields (`code` and `request_id`) are
// additive. Callers branch on Code (machine-readable) rather than
// string-matching Message.
//
// The name `APIError` over `Error` is deliberate even though revive
// flags it as stuttering: shortening to `api.Error` would collide
// with this struct's own `Error` field (the message string), which
// is what JSON consumers depend on.
//
//nolint:revive // APIError avoids field/type name collision; see godoc above.
type APIError struct {
	Error     string `json:"error"`
	Code      string `json:"code,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// WriteError writes a typed APIError at the given HTTP status. Pulls
// the request id from r.Context() (set by RequestLogger). Code is a
// short machine-readable token like "bad_request" or "request_too_large".
// Pass an empty Code to emit only error+request_id.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	WriteJSONStatus(w, status, APIError{
		Error:     msg,
		Code:      code,
		RequestID: RequestIDFromContext(r.Context()),
	})
}
