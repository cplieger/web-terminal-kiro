package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- Helpers ---

// decodeAPIError decodes the JSON error envelope, failing the test on a
// decode error.
func decodeAPIError(t *testing.T, body io.Reader) APIError {
	t.Helper()
	var e APIError
	if err := json.NewDecoder(body).Decode(&e); err != nil {
		t.Fatalf("decode APIError body: %v", err)
	}
	return e
}

// assertJSONHeaders verifies the headers every JSON writer must set:
// the content type and the nosniff guard that stops a browser from
// MIME-sniffing an error payload into an executable type.
func assertJSONHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
	}
}

// --- WriteJSON / WriteJSONStatus ---

func TestWriteJSON_setsStatus200AndEncodesBody(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, map[string]int{"count": 42})

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	assertJSONHeaders(t, rec)

	var got map[string]int
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got["count"] != 42 {
		t.Errorf("body[count] = %d, want 42", got["count"])
	}
}

func TestWriteJSONStatus_setsCustomStatusCode(t *testing.T) {
	cases := []struct {
		name string
		code int
	}{
		{"201 created", http.StatusCreated},
		{"400 bad request", http.StatusBadRequest},
		{"500 internal", http.StatusInternalServerError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			WriteJSONStatus(rec, tc.code, map[string]string{"msg": "ok"})

			if rec.Code != tc.code {
				t.Errorf("status = %d, want %d", rec.Code, tc.code)
			}
			assertJSONHeaders(t, rec)
		})
	}
}

func TestWriteJSONStatus_encodeErrorDoesNotPanic(t *testing.T) {
	// A channel cannot be JSON-encoded, so Encode returns an error
	// after the status line is already committed. The helper must log
	// and return rather than panic, leaving the committed status intact.
	rec := httptest.NewRecorder()
	WriteJSONStatus(rec, http.StatusOK, make(chan int))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// --- Ok ---

func TestOk_returns200WithOkTrue(t *testing.T) {
	rec := httptest.NewRecorder()
	Ok(rec)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	assertJSONHeaders(t, rec)

	var got map[string]bool
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !got["ok"] {
		t.Errorf(`body = %v, want {"ok": true}`, got)
	}
}

// --- WriteError envelope ---

func TestWriteError_envelopeCarriesCodeAndRequestID(t *testing.T) {
	rec := httptest.NewRecorder()
	// ctxKey is package-private; a white-box test seeds the request id
	// the same way RequestLogger does, so WriteError can lift it.
	ctx := context.WithValue(context.Background(), ctxKey{}, "req-abc123")
	req := httptest.NewRequest(http.MethodGet, "/api/x", http.NoBody).WithContext(ctx)

	WriteError(rec, req, http.StatusBadRequest, "bad_request", "bad input")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	got := decodeAPIError(t, rec.Body)
	if got.Error != "bad input" {
		t.Errorf("error = %q, want %q", got.Error, "bad input")
	}
	if got.Code != "bad_request" {
		t.Errorf("code = %q, want %q", got.Code, "bad_request")
	}
	if got.RequestID != "req-abc123" {
		t.Errorf("request_id = %q, want %q", got.RequestID, "req-abc123")
	}
}

func TestWriteError_emptyCodeAndNoRequestIDOmitsBothFields(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)

	WriteError(rec, req, http.StatusBadRequest, "", "bad input")

	// Backward-compat wire format: a client that only reads .error keeps
	// working, and the additive fields are absent when empty.
	body := rec.Body.String()
	if strings.Contains(body, `"code"`) {
		t.Errorf("empty code leaked a code field: %s", body)
	}
	if strings.Contains(body, `"request_id"`) {
		t.Errorf("absent request id leaked a request_id field: %s", body)
	}
	if !strings.Contains(body, `"error":"bad input"`) {
		t.Errorf("missing error field: %s", body)
	}
}

// --- Named error helpers ---

func TestNamedErrorHelpers(t *testing.T) {
	cases := []struct {
		fn       func(http.ResponseWriter, *http.Request)
		name     string
		wantMsg  string
		wantCode string
		wantHTTP int
	}{
		{
			fn:       func(w http.ResponseWriter, r *http.Request) { BadRequest(w, r, "missing field") },
			name:     "BadRequest",
			wantMsg:  "missing field",
			wantCode: "bad_request",
			wantHTTP: http.StatusBadRequest,
		},
		{
			fn:       func(w http.ResponseWriter, r *http.Request) { Conflict(w, r, "already exists") },
			name:     "Conflict",
			wantMsg:  "already exists",
			wantCode: "conflict",
			wantHTTP: http.StatusConflict,
		},
		{
			fn:       MethodNotAllowed,
			name:     "MethodNotAllowed",
			wantMsg:  "method not allowed",
			wantCode: "method_not_allowed",
			wantHTTP: http.StatusMethodNotAllowed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)

			tc.fn(rec, req)

			if rec.Code != tc.wantHTTP {
				t.Errorf("%s status = %d, want %d", tc.name, rec.Code, tc.wantHTTP)
			}
			assertJSONHeaders(t, rec)
			got := decodeAPIError(t, rec.Body)
			if got.Error != tc.wantMsg {
				t.Errorf("%s error = %q, want %q", tc.name, got.Error, tc.wantMsg)
			}
			if got.Code != tc.wantCode {
				t.Errorf("%s code = %q, want %q", tc.name, got.Code, tc.wantCode)
			}
		})
	}
}

// --- Security header invariant across every JSON writer ---

// TestAllJSONWriters_setNosniffHeader pins the nosniff guard once across
// the whole writer set, so a new writer that forgets jsonHeaders fails
// here rather than shipping an unprotected JSON response.
func TestAllJSONWriters_setNosniffHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)

	writers := []struct {
		call func(http.ResponseWriter)
		name string
	}{
		{func(w http.ResponseWriter) { WriteJSON(w, 1) }, "WriteJSON"},
		{func(w http.ResponseWriter) { WriteJSONStatus(w, http.StatusCreated, 1) }, "WriteJSONStatus"},
		{Ok, "Ok"},
		{func(w http.ResponseWriter) { WriteError(w, req, http.StatusBadRequest, "bad_request", "x") }, "WriteError"},
		{func(w http.ResponseWriter) { BadRequest(w, req, "x") }, "BadRequest"},
		{func(w http.ResponseWriter) { Conflict(w, req, "x") }, "Conflict"},
		{func(w http.ResponseWriter) { MethodNotAllowed(w, req) }, "MethodNotAllowed"},
	}

	for _, wr := range writers {
		t.Run(wr.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			wr.call(rec)
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("%s: X-Content-Type-Options = %q, want %q", wr.name, got, "nosniff")
			}
		})
	}
}

// TestWriteJSONStatus_logsWarnOnEncodeFailure pins the documented WriteJSONStatus
// contract ("Encode failures after the status has been committed are logged at
// Warn"). The existing panic-safety test enters this branch but asserts nothing
// about the log, so a mutant dropping the slog.Warn would survive. A channel
// cannot be JSON-encoded, so Encode fails after the 200 status is committed and
// the helper must emit a WARN line carrying the committed code. Serial: swaps the
// process-global slog.Default(), restored via t.Cleanup.
func TestWriteJSONStatus_logsWarnOnEncodeFailure(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	WriteJSONStatus(httptest.NewRecorder(), http.StatusOK, make(chan int))

	var logged struct {
		Level string `json:"level"`
		Msg   string `json:"msg"`
		Code  int    `json:"code"`
	}
	if err := json.Unmarshal(buf.Bytes(), &logged); err != nil {
		t.Fatalf("decode warn line %q: %v", buf.String(), err)
	}
	if logged.Level != "WARN" {
		t.Errorf("level = %q, want WARN (encode-failure is a documented Warn contract)", logged.Level)
	}
	if !strings.Contains(logged.Msg, "encode failed") {
		t.Errorf("msg = %q, want it to describe the encode failure", logged.Msg)
	}
	if logged.Code != http.StatusOK {
		t.Errorf("logged code = %d, want the committed status %d", logged.Code, http.StatusOK)
	}
}
