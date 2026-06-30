package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- validRequestID: length boundaries ---

// TestValidRequestID_lengthBoundaries pins the inclusive length window
// [1, 64]. The boundary cases (exactly 1 and exactly 64 valid chars are
// accepted; 0 and 65 are rejected) are the ones a mistaken `<=`/`>=`
// comparison would flip, so they are asserted explicitly. Inputs are
// built only by repetition; the expected booleans are hardcoded.
func TestValidRequestID_lengthBoundaries(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{name: "single valid char at lower boundary", in: "a", want: true},
		{name: "sixty-four valid chars at upper boundary", in: strings.Repeat("a", 64), want: true},
		{name: "empty is rejected", in: "", want: false},
		{name: "sixty-five chars exceeds upper boundary", in: strings.Repeat("a", 65), want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := validRequestID(tc.in)
			if got != tc.want {
				t.Errorf("validRequestID(len=%d) = %v, want %v", len(tc.in), got, tc.want)
			}
		})
	}
}

// --- RequestIDFromContext ---

func TestRequestIDFromContext_absentReturnsEmpty(t *testing.T) {
	if got := RequestIDFromContext(context.Background()); got != "" {
		t.Errorf("RequestIDFromContext(empty ctx) = %q, want \"\"", got)
	}
}

// runRequestLogger drives req through RequestLogger and reports the
// request id the inner handler observed via RequestIDFromContext along
// with the id echoed on the response. Keeps the per-behavior tests
// below linear (no setup duplicated into each).
func runRequestLogger(t *testing.T, req *http.Request) (ctxID, respHeader string) {
	t.Helper()
	var captured string
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = RequestIDFromContext(r.Context())
	})
	rec := httptest.NewRecorder()
	RequestLogger(inner).ServeHTTP(rec, req)
	return captured, rec.Header().Get(RequestID)
}

func TestRequestLogger_echoesValidInboundID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/x", http.NoBody)
	req.Header.Set(RequestID, "valid-inbound_ID-123")

	ctxID, respHeader := runRequestLogger(t, req)

	if ctxID != "valid-inbound_ID-123" {
		t.Errorf("context id = %q, want the valid inbound id to be reused", ctxID)
	}
	if respHeader != "valid-inbound_ID-123" {
		t.Errorf("response %s = %q, want the inbound id echoed back", RequestID, respHeader)
	}
}

func TestRequestLogger_replacesMalformedInboundID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/x", http.NoBody)
	// Header-injection attempt: a newline that could forge a second log
	// line if echoed unsanitised.
	const injected = "evil\ninjected log line"
	req.Header.Set(RequestID, injected)

	ctxID, respHeader := runRequestLogger(t, req)

	if ctxID == injected {
		t.Error("malformed inbound id was reused; header-injection defense bypassed")
	}
	if ctxID == "" {
		t.Error("a fresh id should be minted when the inbound id is invalid")
	}
	if !validRequestID(ctxID) {
		t.Errorf("minted id %q does not satisfy validRequestID", ctxID)
	}
	if respHeader != ctxID {
		t.Errorf("response %s = %q, want the minted context id %q", RequestID, respHeader, ctxID)
	}
}

func TestRequestLogger_mintsIDWhenAbsent(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/x", http.NoBody)

	ctxID, respHeader := runRequestLogger(t, req)

	if !validRequestID(ctxID) {
		t.Errorf("minted id %q does not satisfy validRequestID", ctxID)
	}
	if len(ctxID) != 32 {
		t.Errorf("minted id %q has length %d, want 32 hex chars", ctxID, len(ctxID))
	}
	if respHeader != ctxID {
		t.Errorf("response %s = %q, want the minted context id %q", RequestID, respHeader, ctxID)
	}
}

func TestRequestLogger_skipsWebSocketPath(t *testing.T) {
	// /ws is a long-lived upgrade: RequestLogger passes it straight to
	// the next handler without minting an id, setting the response
	// header, or storing an id in the context.
	req := httptest.NewRequest(http.MethodGet, "/ws", http.NoBody)

	ctxID, respHeader := runRequestLogger(t, req)

	if ctxID != "" {
		t.Errorf("context id = %q, want \"\" (no id attached on /ws)", ctxID)
	}
	if respHeader != "" {
		t.Errorf("response %s = %q, want \"\" (header not set on /ws)", RequestID, respHeader)
	}
}

// TestStatusRecorder_capturesFirstStatusOnly pins the access-log status
// recorder: it records the first explicit WriteHeader code (which the access
// log reports) and forwards it once to the client; a second WriteHeader is
// suppressed so a late, spurious call cannot rewrite the logged status. The
// recorder's own status field is asserted directly because httptest's recorder
// has its own first-write guard, so checking only the forwarded code would not
// catch a regression in statusRecorder's guard.
func TestStatusRecorder_capturesFirstStatusOnly(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, status: http.StatusOK}

	sr.WriteHeader(http.StatusNotFound)
	if sr.status != http.StatusNotFound {
		t.Errorf("recorded status after first WriteHeader = %d, want %d", sr.status, http.StatusNotFound)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("forwarded status after first WriteHeader = %d, want %d", rec.Code, http.StatusNotFound)
	}

	sr.WriteHeader(http.StatusInternalServerError)
	if sr.status != http.StatusNotFound {
		t.Errorf("recorded status after second WriteHeader = %d, want it pinned at %d", sr.status, http.StatusNotFound)
	}
}

// TestValidRequestID_rejectsInvalidCharacters pins the charset half of the
// header-injection defense: a same-length id containing any byte outside
// [a-zA-Z0-9_-] is rejected. The length-boundary test covers only length and
// FuzzValidRequestID asserts only the accept direction, so without this a
// mutant that widened the accepted set (spaces, dots, control bytes) would go
// uncaught by the deterministic suite. All inputs are 1..64 chars to isolate
// the charset check from the length check; every expectation is false.
func TestValidRequestID_rejectsInvalidCharacters(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{name: "space", in: "abc def"},
		{name: "dot", in: "abc.def"},
		{name: "slash", in: "abc/def"},
		{name: "newline", in: "abc\ndef"},
		{name: "carriage return", in: "abc\rdef"},
		{name: "tab", in: "abc\tdef"},
		{name: "null byte", in: "abc\x00def"},
		{name: "bracket", in: "abc]def"},
		{name: "colon", in: "abc:def"},
		{name: "non-ASCII letter", in: "abcédef"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if validRequestID(tc.in) {
				t.Errorf("validRequestID(%q) = true, want false", tc.in)
			}
		})
	}
}

// TestRequestLogger_emitsAccessLogWithResponseStatus pins vibecli's sole
// request-observability output: the slog access-log line. vibecli is
// slog-only (no /metrics), so this line is the only per-request signal an
// operator or a Loki/Alloy dashboard receives, and its field set
// (level/msg/method/path/status/request_id) is the documented contract. The
// test captures slog output, drives a request whose inner handler writes 404,
// and asserts the emitted "http" record carries that status -- proving the
// access log reports statusRecorder's captured code rather than a constant --
// at level INFO, with the method, path, and a minted valid request id. It runs
// serially (no t.Parallel) because it swaps the process-global slog.Default(),
// restored via t.Cleanup.
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
		t.Errorf("access-log status = %d, want %d (the line must report statusRecorder's code)", logged.Status, http.StatusNotFound)
	}
	if !validRequestID(logged.RequestID) {
		t.Errorf("access-log request_id = %q, want a minted valid id", logged.RequestID)
	}
}
