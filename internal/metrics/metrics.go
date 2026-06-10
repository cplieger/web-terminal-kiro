// Package metrics provides application-level Prometheus metrics backed by
// github.com/cplieger/metrics/v2. The registry prefix ("vibecli") is applied
// by the library to every registered metric name.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	m "github.com/cplieger/metrics/v2"
)

var registry = m.NewRegistry("vibecli")

// Exported metrics (names are auto-prefixed with "vibecli_" by the registry).
var (
	HTTPRequests = m.NewLabeledCounter(
		"http_requests_total",
		"Total HTTP requests",
		[]string{"method", "status"},
	)
	HTTPDuration = m.NewHistogram(
		"http_request_duration_seconds",
		"HTTP request latency",
	)
)

func init() {
	registry.RegisterLabeledCounter(HTTPRequests)
	registry.RegisterHistogram(HTTPDuration)
}

// Handler returns an HTTP handler serving Prometheus text format.
func Handler() http.HandlerFunc { return registry.Handler() }

// StatusRecorder captures an HTTP response status for instrumentation.
type StatusRecorder = m.StatusRecorder

// NewStatusRecorder wraps w to capture its response status code.
func NewStatusRecorder(w http.ResponseWriter) *m.StatusRecorder { return m.NewStatusRecorder(w) }

// RecordHTTP records one request into the package HTTP metrics.
func RecordHTTP(method string, status int, d time.Duration) {
	m.RecordHTTP(HTTPRequests, HTTPDuration, d, method, strconv.Itoa(status))
}
