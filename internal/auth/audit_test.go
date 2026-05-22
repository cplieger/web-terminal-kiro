package auth

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureSlog swaps the default slog logger for one writing JSON to a
// buffer for the duration of fn, then restores it. Returns parsed JSON
// records (one per emitted log line).
func captureSlog(t *testing.T, fn func()) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	fn()

	var records []map[string]any
	for line := range strings.SplitSeq(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("malformed slog JSON line %q: %v", line, err)
		}
		records = append(records, rec)
	}
	return records
}

func TestAudit_emits_fixed_attributes(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/login", nil)
	req.RemoteAddr = "10.0.0.1:54321"
	req.Header.Set("User-Agent", "vibecli-test/1.0")

	records := captureSlog(t, func() {
		audit(req, slog.LevelInfo, AuditLoginSuccess, true,
			slog.String("provider", "amazon"))
	})

	if got := len(records); got != 1 {
		t.Fatalf("want 1 audit record, got %d", got)
	}
	rec := records[0]
	if got := rec["msg"]; got != "audit" {
		t.Errorf("msg: got %v, want \"audit\"", got)
	}
	if got := rec["event_kind"]; got != AuditEventKind {
		t.Errorf("event_kind: got %v, want %q", got, AuditEventKind)
	}
	if got := rec["event"]; got != string(AuditLoginSuccess) {
		t.Errorf("event: got %v, want %v", got, AuditLoginSuccess)
	}
	if got := rec["success"]; got != true {
		t.Errorf("success: got %v, want true", got)
	}
	if got := rec["ip"]; got != "10.0.0.1" {
		t.Errorf("ip: got %v, want \"10.0.0.1\"", got)
	}
	if got := rec["user_agent"]; got != "vibecli-test/1.0" {
		t.Errorf("user_agent: got %v, want \"vibecli-test/1.0\"", got)
	}
	if got := rec["provider"]; got != "amazon" {
		t.Errorf("extra attr provider: got %v, want \"amazon\"", got)
	}
	if got := rec["level"]; got != "INFO" {
		t.Errorf("level: got %v, want \"INFO\"", got)
	}
}

func TestAudit_failure_emits_at_warn(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/login", nil)
	req.RemoteAddr = "192.168.5.10:8080"

	records := captureSlog(t, func() {
		audit(req, slog.LevelWarn, AuditLoginFailure, false,
			slog.String("reason", "timeout_waiting_for_url"))
	})

	if got := len(records); got != 1 {
		t.Fatalf("want 1 audit record, got %d", got)
	}
	rec := records[0]
	if got := rec["level"]; got != "WARN" {
		t.Errorf("level: got %v, want \"WARN\"", got)
	}
	if got := rec["success"]; got != false {
		t.Errorf("success: got %v, want false", got)
	}
	if got := rec["reason"]; got != "timeout_waiting_for_url" {
		t.Errorf("reason: got %v, want \"timeout_waiting_for_url\"", got)
	}
}

func TestClientIP_strips_port(t *testing.T) {
	cases := []struct {
		remote string
		want   string
	}{
		{"10.0.0.1:12345", "10.0.0.1"},
		{"[::1]:8080", "::1"},
		{"203.0.113.7:443", "203.0.113.7"},
		{"no-port", "no-port"}, // SplitHostPort errors, return as-is
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = c.remote
		if got := clientIP(req); got != c.want {
			t.Errorf("clientIP(%q) = %q, want %q", c.remote, got, c.want)
		}
	}
}
