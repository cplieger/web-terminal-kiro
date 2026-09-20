package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cplieger/web-terminal-engine/v6/terminal"
)

// TestSessionActivityWiredIntoManager pins the WIRING of the workflow mark:
// terminal.WithSessionActivity is the only reason a published mark reaches a client,
// and workflowmark_test.go covers the watcher alone — so dropping that option from
// registerRoutes empties every tab's activity forever while the suite stays green.
// Verified by deleting it: only this test reddens.
func TestSessionActivityWiredIntoManager(t *testing.T) {
	deps := newTestDeps(true)
	watch := newWorkflowWatch(t.TempDir(), func() map[terminal.SessionID]string { return nil })
	deps.sessionActivity = watch.sessionActivity
	mux, _, _, id := mustStartSession(t, deps)

	// Published AFTER Create because the engine PULLS: the mark only has to be there
	// when List serves the request. The tally is lopsided so a per-state Count fails.
	want := workflowMark{
		State: workflowMarkInput,
		Tally: workflowTally{Total: 3, Working: 1, Waiting: 1, Input: 1},
	}
	marks := map[terminal.SessionID]workflowMark{id: want}
	watch.marks.Store(&marks)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, terminal.SessionsPath, http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want %d; body %s", terminal.SessionsPath, rec.Code, http.StatusOK, rec.Body.String())
	}
	var listed []terminal.SessionInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode GET %s body %s: %v", terminal.SessionsPath, rec.Body.String(), err)
	}
	if len(listed) != 1 {
		t.Fatalf("GET %s listed %d sessions, want 1; body %s", terminal.SessionsPath, len(listed), rec.Body.String())
	}

	got := listed[0]
	if got.ID != id {
		t.Fatalf("listed session id = %q, want the created tab %q", terminal.LogID(got.ID), terminal.LogID(id))
	}
	if got.Activity != want.State {
		t.Errorf("GET %s activity = %q, want %q (registerRoutes must pass terminal.WithSessionActivity(deps.sessionActivity) to terminal.NewSessionManager)",
			terminal.SessionsPath, got.Activity, want.State)
	}
	if got.ActivityCount != want.Tally.Total {
		t.Errorf("GET %s activityCount = %d, want %d (the tab's TOTAL admitted runs, not the folded state's own count of %d)",
			terminal.SessionsPath, got.ActivityCount, want.Tally.Total, want.Tally.Input)
	}
}
