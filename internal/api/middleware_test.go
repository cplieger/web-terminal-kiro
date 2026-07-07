package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cplieger/webhttp"
)

// runRequestLogger drives req through RequestLogger and reports the request id
// the inner handler observed via webhttp.RequestIDFromContext along with the id
// echoed on the response. Keeps the per-behavior tests below linear.
func runRequestLogger(t *testing.T, req *http.Request) (ctxID, respHeader string) {
	t.Helper()
	var captured string
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = webhttp.RequestIDFromContext(r.Context())
	})
	rec := httptest.NewRecorder()
	RequestLogger(inner).ServeHTTP(rec, req)
	return captured, rec.Header().Get(webhttp.HeaderRequestID)
}

func TestRequestLogger_echoesValidInboundID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/x", http.NoBody)
	req.Header.Set(webhttp.HeaderRequestID, "valid-inbound_ID-123")

	ctxID, respHeader := runRequestLogger(t, req)

	if ctxID != "valid-inbound_ID-123" {
		t.Errorf("context id = %q, want the valid inbound id to be reused", ctxID)
	}
	if respHeader != "valid-inbound_ID-123" {
		t.Errorf("response %s = %q, want the inbound id echoed back", webhttp.HeaderRequestID, respHeader)
	}
}

func TestRequestLogger_replacesMalformedInboundID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/x", http.NoBody)
	// Header-injection attempt: a newline that could forge a second log line if
	// echoed unsanitised.
	const injected = "evil\ninjected log line"
	req.Header.Set(webhttp.HeaderRequestID, injected)

	ctxID, respHeader := runRequestLogger(t, req)

	if ctxID == injected {
		t.Error("malformed inbound id was reused; header-injection defense bypassed")
	}
	if ctxID == "" {
		t.Error("a fresh id should be minted when the inbound id is invalid")
	}
	if !webhttp.ValidRequestID(ctxID) {
		t.Errorf("minted id %q does not satisfy webhttp.ValidRequestID", ctxID)
	}
	if respHeader != ctxID {
		t.Errorf("response %s = %q, want the minted context id %q", webhttp.HeaderRequestID, respHeader, ctxID)
	}
}

func TestRequestLogger_mintsIDWhenAbsent(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/x", http.NoBody)

	ctxID, respHeader := runRequestLogger(t, req)

	if !webhttp.ValidRequestID(ctxID) {
		t.Errorf("minted id %q does not satisfy webhttp.ValidRequestID", ctxID)
	}
	if len(ctxID) != 32 {
		t.Errorf("minted id %q has length %d, want 32 hex chars", ctxID, len(ctxID))
	}
	if respHeader != ctxID {
		t.Errorf("response %s = %q, want the minted context id %q", webhttp.HeaderRequestID, respHeader, ctxID)
	}
}

func TestRequestLogger_skipsWebSocketPath(t *testing.T) {
	// /ws is a long-lived upgrade: RequestLogger passes it straight to the next
	// handler without minting an id, setting the response header, or storing an
	// id in the context.
	req := httptest.NewRequest(http.MethodGet, "/ws", http.NoBody)

	ctxID, respHeader := runRequestLogger(t, req)

	if ctxID != "" {
		t.Errorf("context id = %q, want \"\" (no id attached on /ws)", ctxID)
	}
	if respHeader != "" {
		t.Errorf("response %s = %q, want \"\" (header not set on /ws)", webhttp.HeaderRequestID, respHeader)
	}
}

// TestRequestLogger_emitsAccessLogWithResponseStatus pins vibecli's sole
// request-observability output: the slog access-log line. vibecli is slog-only
// (no /metrics), so this line is the only per-request signal an operator or a
// Loki/Alloy dashboard receives, and its field set
// (level/msg/method/path/status/request_id) is the documented contract. The
// test captures slog output, drives a request whose inner handler writes 404,
// and asserts the emitted "http" record carries that status -- proving the
// access log reports the recorded code rather than a constant -- at level INFO,
// with the method, path, and a minted valid request id. It runs serially (no
// t.Parallel) because it swaps the process-global slog.Default(), restored via
// t.Cleanup.
func TestRequestLogger_emitsAccessLogWithResponseStatus(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	req := httptest.NewRequest(http.MethodPost, "/api/thing", http.NoBody)
	RequestLogger(inner).ServeHTTP(httptest.NewRecorder(), req)

	var logged struct {
		Level     string `json:"level"`
		Msg       string `json:"msg"`
		Method    string `json:"method"`
		Path      string `json:"path"`
		Status    int    `json:"status"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(buf.Bytes(), &logged); err != nil {
		t.Fatalf("decode access-log line %q: %v", buf.String(), err)
	}
	if logged.Msg != "http" {
		t.Errorf("access-log msg = %q, want %q", logged.Msg, "http")
	}
	if logged.Level != "INFO" {
		t.Errorf("access-log level = %q, want INFO (a downgrade hides the line in production)", logged.Level)
	}
	if logged.Method != http.MethodPost {
		t.Errorf("access-log method = %q, want %q", logged.Method, http.MethodPost)
	}
	if logged.Path != "/api/thing" {
		t.Errorf("access-log path = %q, want %q", logged.Path, "/api/thing")
	}
	if logged.Status != http.StatusNotFound {
		t.Errorf("access-log status = %d, want %d (the line must report the recorded code)", logged.Status, http.StatusNotFound)
	}
	if !webhttp.ValidRequestID(logged.RequestID) {
		t.Errorf("access-log request_id = %q, want a minted valid id", logged.RequestID)
	}
}

// TestStatusRecorder_UnwrapReachesFlusher pins the documented streaming-flush
// contract: http.NewResponseController must reach the underlying Flusher
// through statusRecorder.Unwrap. statusRecorder embeds the http.ResponseWriter
// interface, whose method set has no Flush, so Flush is not promoted onto the
// wrapper; without Unwrap the controller cannot find a flusher and returns
// http.ErrNotSupported. A regression that dropped Unwrap would silently break
// flushing on any streaming handler wrapped by RequestLogger.
func TestStatusRecorder_UnwrapReachesFlusher(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, status: http.StatusOK}

	if err := http.NewResponseController(sr).Flush(); err != nil {
		t.Fatalf("Flush through statusRecorder = %v, want nil (Unwrap must expose the recorder's Flusher)", err)
	}
	if !rec.Flushed {
		t.Error("recorder was not flushed; Flush did not reach the underlying ResponseWriter")
	}
}

// TestRequestLogger_failedStreamOpenLogsWarn pins the failed-open branch cycle
// 1 added: when a long-lived stream path (/ws or /api/sessions/events) returns
// an error status before the engine takes over, RequestLogger emits a single
// WARN "http" access line carrying method/path/status/remote. It is vibecli's
// only signal for such a failure (slog-only, no /metrics), so it must fire; the
// line deliberately omits request_id and duration_ms (the no-id-on-/ws
// contract), which this test also pins. Serial: swaps the process-global
// slog.Default(), restored via t.Cleanup.
func TestRequestLogger_failedStreamOpenLogsWarn(t *testing.T) {
	for _, path := range []string{"/ws", "/api/sessions/events"} {
		t.Run(path, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			})
			req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
			rec := httptest.NewRecorder()
			RequestLogger(inner).ServeHTTP(rec, req)

			var logged struct {
				Level  string `json:"level"`
				Msg    string `json:"msg"`
				Method string `json:"method"`
				Path   string `json:"path"`
				Status int    `json:"status"`
			}
			if err := json.Unmarshal(buf.Bytes(), &logged); err != nil {
				t.Fatalf("decode warn line %q: %v", buf.String(), err)
			}
			if logged.Level != "WARN" {
				t.Errorf("level = %q, want WARN (failed stream open must warn)", logged.Level)
			}
			if logged.Msg != "http" {
				t.Errorf("msg = %q, want %q", logged.Msg, "http")
			}
			if logged.Method != http.MethodGet {
				t.Errorf("method = %q, want %q", logged.Method, http.MethodGet)
			}
			if logged.Path != path {
				t.Errorf("path = %q, want %q", logged.Path, path)
			}
			if logged.Status != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want %d", logged.Status, http.StatusServiceUnavailable)
			}
			if strings.Contains(buf.String(), `"request_id"`) {
				t.Errorf("failed stream open leaked request_id (breaks no-id-on-/ws contract): %s", buf.String())
			}
			if strings.Contains(buf.String(), `"duration_ms"`) {
				t.Errorf("failed stream open emitted duration_ms (branch omits it): %s", buf.String())
			}
			if got := rec.Header().Get(RequestID); got != "" {
				t.Errorf("response %s = %q, want empty on a stream path", RequestID, got)
			}
		})
	}
}

// TestRequestLogger_successfulStreamOpenEmitsNoLog pins the deliberate skip: a
// successful open (status < 400) of a long-lived stream path emits NO access
// line, because a per-connection line at open time would carry a meaningless
// duration. Together with TestRequestLogger_failedStreamOpenLogsWarn this pins
// both arms of the status guard, so a mutant that dropped the ">= 400" check
// (always logging) is caught here. Serial: swaps slog.Default(), restored via
// t.Cleanup.
func TestRequestLogger_successfulStreamOpenEmitsNoLog(t *testing.T) {
	for _, path := range []string{"/ws", "/api/sessions/events"} {
		t.Run(path, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {})
			req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
			rec := httptest.NewRecorder()
			RequestLogger(inner).ServeHTTP(rec, req)

			if buf.Len() != 0 {
				t.Errorf("successful stream open emitted a log line, want none: %s", buf.String())
			}
			if got := rec.Header().Get(RequestID); got != "" {
				t.Errorf("response %s = %q, want empty on a stream path", RequestID, got)
			}
		})
	}
}

// TestRequestLogger_accessLogDefaultsTo200AndCarriesRemoteAndDuration pins two
// parts of the documented access-log contract (method/path/status/duration_ms/
// request_id/remote) that the status test does not. When the inner handler
// writes a body without an explicit WriteHeader, Go sends an implicit 200 on the
// underlying writer, so statusRecorder keeps its initial http.StatusOK and the
// access line must report 200 -- this guards the status: http.StatusOK
// initialiser on the normal path (a mutant zeroing it is caught here but not by
// the WriteHeader(404) status test). It also pins remote (deterministic under
// httptest: 192.0.2.1:1234) and duration_ms, decoded through a pointer so
// presence is asserted independent of its time-dependent value. Serial: swaps
// the process-global slog.Default(), restored via t.Cleanup.
func TestRequestLogger_accessLogDefaultsTo200AndCarriesRemoteAndDuration(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("body"))
	})
	req := httptest.NewRequest(http.MethodGet, "/api/thing", http.NoBody)
	RequestLogger(inner).ServeHTTP(httptest.NewRecorder(), req)

	var logged struct {
		Status     int    `json:"status"`
		Remote     string `json:"remote"`
		DurationMs *int64 `json:"duration_ms"`
	}
	if err := json.Unmarshal(buf.Bytes(), &logged); err != nil {
		t.Fatalf("decode access-log line %q: %v", buf.String(), err)
	}
	if logged.Status != http.StatusOK {
		t.Errorf("access-log status = %d, want 200 (statusRecorder default when the handler omits WriteHeader)", logged.Status)
	}
	if logged.Remote != "192.0.2.1:1234" {
		t.Errorf("access-log remote = %q, want the request RemoteAddr (documented field)", logged.Remote)
	}
	if logged.DurationMs == nil {
		t.Errorf("access-log line omitted duration_ms (documented field): %s", buf.String())
	}
}

// TestRequestLogger_emitsAccessLogWhenHandlerPanics pins the deferred-emission
// guarantee: when a downstream handler panics, RequestLogger still emits the
// "http" access line (vibecli's only per-request signal -- slog-only, no
// /metrics), AND the panic still propagates unchanged to net/http's conn-level
// handler because RequestLogger adds no recover(). Without the deferred
// emission a panic would unwind straight past the log call and the failing
// request would be invisible to any Loki query keyed on the msg="http"
// contract -- precisely on the failure that matters most. The test recovers the
// propagated panic itself (so the suite does not crash), asserts the recovered
// value is the handler's original one (proving RequestLogger did not swallow or
// alter it), then asserts the captured line carries msg="http", the
// method/path, and a minted valid request id. Serial: swaps the process-global
// slog.Default(), restored via t.Cleanup.
func TestRequestLogger_emitsAccessLogWhenHandlerPanics(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const panicMsg = "boom from handler"
	inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic(panicMsg)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/panics", http.NoBody)

	// Drive the request inside a recover so the propagated panic does not
	// crash the test binary; capture what unwound so we can assert it is
	// unchanged.
	propagated := func() (recovered any) {
		defer func() { recovered = recover() }()
		RequestLogger(inner).ServeHTTP(httptest.NewRecorder(), req)
		return nil
	}()

	if propagated == nil {
		t.Fatal("panic did not propagate through RequestLogger; it must not recover()")
	}
	if got, ok := propagated.(string); !ok || got != panicMsg {
		t.Errorf("propagated panic = %v, want the handler's original value %q unchanged", propagated, panicMsg)
	}

	var logged struct {
		Msg       string `json:"msg"`
		Method    string `json:"method"`
		Path      string `json:"path"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(buf.Bytes(), &logged); err != nil {
		t.Fatalf("decode access-log line %q: %v", buf.String(), err)
	}
	if logged.Msg != "http" {
		t.Errorf("access-log msg = %q, want %q (the line must be emitted despite the panic)", logged.Msg, "http")
	}
	if logged.Method != http.MethodGet {
		t.Errorf("access-log method = %q, want %q", logged.Method, http.MethodGet)
	}
	if logged.Path != "/api/panics" {
		t.Errorf("access-log path = %q, want %q", logged.Path, "/api/panics")
	}
	if !validRequestID(logged.RequestID) {
		t.Errorf("access-log request_id = %q, want a minted valid id", logged.RequestID)
	}
}
