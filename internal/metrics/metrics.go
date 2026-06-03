// Package metrics provides application-level Prometheus metrics backed by
// github.com/cplieger/metrics.
package metrics

import (
	"net/http"

	m "github.com/cplieger/metrics"
)

var registry = m.NewRegistry("")

// Exported metrics.
var (
	HTTPRequests = m.NewLabeledCounter(
		"vibecli_http_requests_total",
		"Total HTTP requests",
		[]string{"method", "status"},
	)
	HTTPDuration = m.NewHistogram(
		"vibecli_http_request_duration_seconds",
		"HTTP request latency",
	)
)

func init() {
	registry.RegisterLabeledCounter(HTTPRequests)
	registry.RegisterHistogram(HTTPDuration)
}

// Handler returns an HTTP handler serving Prometheus text format.
func Handler() http.HandlerFunc {
	return registry.Handler()
}
