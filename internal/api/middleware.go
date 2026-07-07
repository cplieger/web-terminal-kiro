// Package api holds vibecli's one app-specific piece of HTTP plumbing: the
// request-logging + request-id access logger. The JSON response writers, the
// error envelope, the status recorder, and the request-id primitives it used
// to carry now come from github.com/cplieger/webhttp, so this package is just
// the access-log middleware that layers vibecli's `remote` field on top.
package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/cplieger/webhttp"
)

// RequestLogger wraps next with method/path/status/latency/request-id/remote
// access logging at slog.Info, built on webhttp's request-id and
// status-recorder primitives. It skips the long-lived streams (/ws and the
// session status SSE /api/sessions/events): logging them at open time would
// record only "opened" with a meaningless duration, and the terminal package
// logs its own lifecycle.
//
// The `remote` field (r.RemoteAddr) is vibecli's documented access-log
// contract, so this stays app-side rather than using webhttp.Logging (which
// omits it) — collapse to webhttp.Logging(WithClientIP) after webhttp releases
// it. An inbound X-Request-ID is reused when it satisfies
// webhttp.ValidRequestID; otherwise a fresh id is minted with
// webhttp.NewRequestID. The id is echoed on the response and threaded into the
// request context via webhttp.WithRequestID.
func RequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" || r.URL.Path == "/api/sessions/events" {
			next.ServeHTTP(w, r)
			return
		}

		id := r.Header.Get(webhttp.HeaderRequestID)
		if !webhttp.ValidRequestID(id) {
			id = webhttp.NewRequestID()
		}
		w.Header().Set(webhttp.HeaderRequestID, id)
		r = r.WithContext(webhttp.WithRequestID(r.Context(), id))

		rw := webhttp.NewStatusRecorder(w)
		start := time.Now()
		next.ServeHTTP(rw, r)

		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", id,
			"remote", r.RemoteAddr,
		)
	})
}
