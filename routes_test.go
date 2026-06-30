package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/fstest"
)

// TestDebugRoutesNotExposed pins the security posture of registerRoutes: the
// engine's terminal handler is mounted at /ws ONLY (via mux.Handle), never via
// term.RegisterRoutes, which would also wire the unauthenticated /debug/raw
// (raw PTY ring) and /debug/screen (full VT buffer) on this network surface.
// Regressing to RegisterRoutes re-opens the leak this test guards against.
func TestDebugRoutesNotExposed(t *testing.T) {
	mux := http.NewServeMux()
	var ready atomic.Bool
	deps := &routeDeps{
		staticFS: fstest.MapFS{"static/index.html": &fstest.MapFile{Data: []byte("ok")}},
		ready:    &ready,
		workDir:  "",
		cmd:      []string{"/bin/cat"},
	}
	if _, err := registerRoutes(mux, deps); err != nil {
		t.Fatalf("registerRoutes: %v", err)
	}

	// /ws must be registered as its own pattern.
	if _, pat := mux.Handler(httptest.NewRequest(http.MethodGet, "/ws", http.NoBody)); pat != "/ws" {
		t.Errorf("/ws routed to pattern %q, want \"/ws\"", pat)
	}

	// /debug/* must NOT be registered — an unregistered path falls through to
	// the "/" file-server catch-all, so its matched pattern must not be itself.
	for _, p := range []string{"/debug/raw", "/debug/screen"} {
		if _, pat := mux.Handler(httptest.NewRequest(http.MethodGet, p, http.NoBody)); pat == p {
			t.Errorf("%s is registered (pattern %q); /debug routes must not be exposed", p, pat)
		}
	}
}
