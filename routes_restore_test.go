package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/web-terminal-engine/v6/terminal"
)

// awaitHandlerText polls h's screen until it holds want, failing t after 20 s.
func awaitHandlerText(t *testing.T, h *terminal.Handler, want string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		snap, ok := h.Text(0)
		if ok && strings.Contains(snap.Text, want) {
			return snap.Text
		}
		if time.Now().After(deadline) {
			t.Fatalf("session screen never showed %q; it shows %q", want, snap.Text)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestValidKiroResumeID(t *testing.T) {
	const good = "sess_47400f57-1c2d-4e5f-8a9b-0123456789ab"
	tests := []struct {
		id   string
		want bool
	}{
		{good, true},
		{"sess_47400F57-1C2D-4E5F-8A9B-0123456789AB", true},
		{strings.TrimPrefix(good, "sess_"), false},
		{"SESS_47400f57-1c2d-4e5f-8a9b-0123456789ab", false},
		{"sess_47400f57-1c2d-4e5f-8a9b-0123456789a", false},
		{"sess_47400f57-1c2d-4e5f-8a9b-0123456789abc", false},
		{"sess_47400f5701c2d-4e5f-8a9b-0123456789ab", false},
		{"sess_47400f57-1c2d04e5f-8a9b-0123456789ab", false},
		{"sess_47400f57-1c2d-4e5f08a9b-0123456789ab", false},
		{"sess_47400f57-1c2d-4e5f-8a9b00123456789ab", false},
		{"sess_47400f57-1c2d-4e5f-8a9b-0123456789ag", false},
		{"sess_-", false},
		{"sess_", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := validKiroResumeID(tt.id); got != tt.want {
			t.Errorf("validKiroResumeID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

func TestSessionBuilder_resumeArgv(t *testing.T) {
	logs := capturedLog(t)
	// The shape sessionCommand returns: four fixed items plus one chat arg, so len 5
	// and cap 8, with spare capacity an append would write into across tabs.
	argv := append([]string{"/bin/sh", "-c", `printf '<%s>' "$@"; printf '|\n'; exec cat`, "sh"}, "--v3")
	if len(argv) != 5 || cap(argv) != 8 {
		t.Fatalf("fixture argv len %d cap %d, want 5 and 8", len(argv), cap(argv))
	}
	deps := newTestDeps(true)
	deps.cmd = func() []string { return argv }
	build := newSessionBuilder(deps)

	const idA = "sess_47400f57-1c2d-4e5f-8a9b-00000000000a"
	const idB = "sess_47400F57-1C2D-4E5F-8A9B-00000000000B"
	hA := build(idA)("tab-a")
	hB := build(idB)("tab-b")
	for _, h := range []*terminal.Handler{hA, hB} {
		t.Cleanup(func() { shutdownHandler(t, h) })
		if err := h.StartEager(); err != nil {
			t.Fatalf("StartEager: %v", err)
		}
	}
	awaitHandlerText(t, hA, "<--v3><--resume-id><"+idA+">|")
	awaitHandlerText(t, hB, "<--v3><--resume-id><"+idB+">|")

	const refusal = "session: refusing a malformed resume id; starting a fresh chat"
	for _, bad := range []string{
		"sess_-",
		"sess_" + strings.Repeat("a", 60),
		"sess_47400f57-1c2d-4e5f-8a9b0-123456789ab",
	} {
		t.Run(bad[:min(len(bad), 20)], func(t *testing.T) {
			before := strings.Count(logs.String(), refusal)
			h := build(bad)("tab-bad")
			t.Cleanup(func() { shutdownHandler(t, h) })
			if err := h.StartEager(); err != nil {
				t.Fatalf("StartEager: %v", err)
			}
			if got := awaitHandlerText(t, h, "<--v3>|"); strings.Contains(got, "--resume-id") {
				t.Errorf("argv for %q carries --resume-id: %q", bad, got)
			}
			if n := strings.Count(logs.String(), refusal) - before; n != 1 {
				t.Errorf("refusal Warns for %q = %d, want 1", bad, n)
			}
		})
	}
}

func TestCreateGate_restoringLayer(t *testing.T) {
	var pending, kiroUnready, toolsSyncing atomic.Bool
	deps := newTestDeps(true)
	deps.restoring = pending.Load
	deps.toolsSyncing = toolsSyncing.Load
	deps.kiroReady = func() (bool, string) {
		if kiroUnready.Load() {
			return false, "kiro-cli installing"
		}
		return true, ""
	}
	mux := http.NewServeMux()
	mgr := registerRoutes(mux, deps, newSessionBuilder(deps)(""))
	t.Cleanup(func() { shutdownManager(t, mgr) })

	serve := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, http.NoBody))
		return rec
	}
	layout := terminal.SessionsSubtreePath + "layout"
	expect503 := func(t *testing.T, rec *httptest.ResponseRecorder, what, reason, retry string) {
		t.Helper()
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"error":"`+reason+`"`) {
			t.Errorf("%s = %d %s, want 503 %q", what, rec.Code, rec.Body.String(), reason)
		}
		if got := rec.Header().Get("Retry-After"); got != retry {
			t.Errorf("%s Retry-After = %q, want %q", what, got, retry)
		}
	}

	t.Run("pending holds create and the layout read, not the list", func(t *testing.T) {
		pending.Store(true)
		expect503(t, serve(http.MethodPost, terminal.SessionsPath), "POST", terminal.RestoringMessage, "1")
		expect503(t, serve(http.MethodGet, layout), "GET layout", terminal.RestoringMessage, "1")
		if rec := serve(http.MethodGet, terminal.SessionsPath); rec.Code != http.StatusOK {
			t.Errorf("GET list while pending = %d, want 200", rec.Code)
		}
	})

	t.Run("an unready kiro-cli outranks the restore on create and the layout read", func(t *testing.T) {
		pending.Store(true)
		kiroUnready.Store(true)
		defer kiroUnready.Store(false)
		expect503(t, serve(http.MethodPost, terminal.SessionsPath), "POST", "kiro-cli installing", "5")
		expect503(t, serve(http.MethodGet, layout), "GET layout", "kiro-cli installing", "5")
		if rec := serve(http.MethodGet, terminal.SessionsPath); rec.Code != http.StatusOK {
			t.Errorf("GET list while kiro-cli installs = %d, want 200", rec.Code)
		}
	})

	t.Run("converging tools outrank the restore on the layout read", func(t *testing.T) {
		pending.Store(true)
		toolsSyncing.Store(true)
		defer toolsSyncing.Store(false)
		expect503(t, serve(http.MethodGet, layout), "GET layout", "tools installing", "5")
	})

	t.Run("an unready kiro-cli does not hold the layout read once the restore is done", func(t *testing.T) {
		pending.Store(false)
		kiroUnready.Store(true)
		defer kiroUnready.Store(false)
		if rec := serve(http.MethodGet, layout); rec.Code != http.StatusOK {
			t.Errorf("GET layout with no restore pending = %d %s, want 200", rec.Code, rec.Body.String())
		}
	})

	t.Run("cleared, both pass through", func(t *testing.T) {
		pending.Store(false)
		if rec := serve(http.MethodGet, layout); rec.Code != http.StatusOK {
			t.Errorf("GET layout after the restore = %d, want 200", rec.Code)
		}
		// The created /bin/cat session is torn down by the manager's cleanup.
		if rec := serve(http.MethodPost, terminal.SessionsPath); rec.Code != http.StatusCreated {
			t.Errorf("POST after the restore = %d %s, want 201", rec.Code, rec.Body.String())
		}
	})
}
