package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/web-terminal-engine/v6/terminal"
)

// The byte shapes kiro-cli writes: OSC 7501 with BEL as captured from its TUI,
// and its OSC 9 notifier texts.
const (
	osc7501Working = `\033]7501;state=working:app=kiro-cli\a`
	osc7501Blocked = `\033]7501;state=blocked:kind=permission:app=kiro-cli\a`
	osc7501Error   = `\033]7501;state=error:app=kiro-cli\a`

	osc9PermissionRequired = `\033]9;Permission required\a`
	osc9ResponseComplete   = `\033]9;Response complete\a`
)

// sessionStatus is one session's status as the engine's last sweep left it on
// the manager's list, the observable the UI reads.
func sessionStatus(t *testing.T, mgr *terminal.SessionManager, id terminal.SessionID) string {
	t.Helper()
	for _, s := range mgr.List() {
		if s.ID == id {
			return s.Status
		}
	}
	t.Fatalf("session %s is not listed", terminal.LogID(id))
	return ""
}

// waitStatus polls the list until the session reports want, failing at the
// deadline with the last status seen.
func waitStatus(t *testing.T, mgr *terminal.SessionManager, id terminal.SessionID, want, why string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := sessionStatus(t, mgr, id)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session status = %q, never %q: %s", got, want, why)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitFile is the child's half of a step the test releases by creating path.
func awaitFile(path string) string {
	return `while [ ! -e '` + path + `' ]; do sleep 0.02; done; `
}

// release creates the file a child's awaitFile waits on.
func release(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("release the child: %v", err)
	}
}

func TestProgramStatusWiredIntoManager(t *testing.T) {
	deps := newTestDeps(true)
	deps.cmd = staticCmd("/bin/sh", "-c", `printf '`+osc7501Blocked+`'; exec cat`)
	_, mgr, _, id := mustStartSession(t, deps)

	waitStatus(t, mgr, id, terminal.StatusInput,
		"an OSC 7501 blocked report must show the tab waiting for the user")
}

func TestProgramStatusErrorThenPermissionTextShowsInput(t *testing.T) {
	dir := t.TempDir()
	asked, resumed := filepath.Join(dir, "asked"), filepath.Join(dir, "resumed")
	deps := newTestDeps(true)
	deps.cmd = staticCmd("/bin/sh", "-c", `printf '`+osc7501Error+`'; `+
		awaitFile(asked)+`printf '`+osc9PermissionRequired+`'; `+
		awaitFile(resumed)+`printf '`+osc7501Working+`'; exec cat`)
	_, mgr, _, id := mustStartSession(t, deps)

	waitStatus(t, mgr, id, terminal.StatusFailed, "an OSC 7501 error report must show the turn failed")
	release(t, asked)
	waitStatus(t, mgr, id, terminal.StatusInput,
		"a permission ask notified after an OSC 7501 error must show the tab waiting for the user, since kiro-cli keeps reporting error over a pending ask")
	release(t, resumed)
	waitStatus(t, mgr, id, terminal.StatusWorking,
		"the next OSC 7501 change must take the status back from the notification")
}

func TestProgramStatusErrorSurvivesResponseComplete(t *testing.T) {
	deps := newTestDeps(true)
	deps.cmd = staticCmd("/bin/sh", "-c", `printf '`+osc7501Error+osc9ResponseComplete+`'; exec cat`)
	_, mgr, _, id := mustStartSession(t, deps)

	// kiro-cli sends "Response complete" after a turn it reports as error, so
	// done here would paint a failed turn as finished. A done latch persists, so
	// watching several engine sweeps after failed shows whether one formed.
	deadline := time.Now().Add(10 * time.Second)
	var failedAt time.Time
	for failedAt.IsZero() || time.Since(failedAt) < time.Second {
		switch got := sessionStatus(t, mgr, id); {
		case got == terminal.StatusFailed:
			if failedAt.IsZero() {
				failedAt = time.Now()
			}
		case got == terminal.StatusDone || !failedAt.IsZero():
			t.Fatalf("session status = %q after OSC 7501 error then OSC 9 %q, want %q", got, "Response complete", terminal.StatusFailed)
		case time.Now().After(deadline):
			t.Fatalf("session status = %q, never %q after an OSC 7501 error report", got, terminal.StatusFailed)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
