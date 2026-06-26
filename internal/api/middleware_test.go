package api

import (
	"context"
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
