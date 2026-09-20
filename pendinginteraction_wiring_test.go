package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/web-terminal-engine/v6/terminal"
)

func TestPendingInteractionWiredIntoManager(t *testing.T) {
	deps := newTestDeps(true)
	deps.cmd = staticCmd("/bin/sh", "-c", `printf '\033]9;Permission required\a'; exec cat`)
	mux, mgr, _, tab := mustStartSession(t, deps)
	awaitListedStatus(t, mux, terminal.StatusInput)

	f := newPendingFixture(t)
	f.pairs[tab] = pendingFixtureSession
	now := time.Now()
	f.messages("hash0", pendingFixtureSession,
		pendingApprovalRow("tool-1", now.Add(-time.Second)),
		resolvedRow("tool-1", "accept", now))

	f.watch.pass(t.Context(), mgr)

	if status, _, ok := mgr.StatusLatch(tab); !ok || status != "" {
		t.Fatalf("StatusLatch after the pass = (%q, ok=%v), want the input latch withdrawn from the live manager", status, ok)
	}
	awaitListedStatus(t, mux, terminal.StatusIdle)
}

// The notification reaches the classifier asynchronously, so this polls rather than sleeps.
func awaitListedStatus(t *testing.T, mux *http.ServeMux, status string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, terminal.SessionsPath, http.NoBody))
		if strings.Contains(rec.Body.String(), `"status":"`+status+`"`) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session status never reached %q; body %s", status, rec.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
