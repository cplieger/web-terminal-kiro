package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/web-terminal-engine/v6/terminal"
)

type fakePollerSessions struct {
	sessions   []terminal.SessionInfo
	lists      atomic.Int32
	latchReads atomic.Int32
	captures   atomic.Int32
}

// fakeCapturedSet is what fakePollerSessions captures: a fixed one-entry set, built
// without calling List so a capture never counts as a sweep.
var fakeCapturedSet = terminal.SessionSet{
	Version:  terminal.SessionSetVersion,
	Sessions: []terminal.SessionSetEntry{{Alias: "k3q7m2xa"}},
	Layout:   terminal.SessionSetLayout{Left: new(0), Selected: terminal.PaneLeft, Handle: 0.5},
}

func (f *fakePollerSessions) CaptureSessionSet(func(terminal.SessionInfo) string) terminal.SessionSet {
	f.captures.Add(1)
	return fakeCapturedSet
}

func (f *fakePollerSessions) List() []terminal.SessionInfo {
	f.lists.Add(1)
	return f.sessions
}

func (*fakePollerSessions) SetSessionTitle(terminal.SessionID, string) bool { return true }

func (*fakePollerSessions) SetSessionAlias(terminal.SessionID, string) bool { return true }

func (f *fakePollerSessions) StatusLatch(terminal.SessionID) (string, uint64, bool) {
	f.latchReads.Add(1)
	return "", 0, true
}

func (*fakePollerSessions) WithdrawStatusLatch(terminal.SessionID, string, uint64) bool {
	return false
}

func TestSessionPollersJoinTheAskToTheTabInOneTick(t *testing.T) {
	home := t.TempDir()
	p := newSessionPollers(filepath.Join(t.TempDir(), "state"), home, nil)
	if p.sessionEnv == nil {
		t.Fatal("a verified state directory produced no session title environment")
	}
	handle := titleHandleFor(t, p.titles, "tab1")
	if err := os.WriteFile(filepath.Join(p.titles.stateDir, handle), []byte(pendingFixtureSession+"\n"), 0o600); err != nil {
		t.Fatalf("write the mapping file: %v", err)
	}
	wf := &workflowFixture{t: t, home: home}
	wf.run("hash0", "wf_live", "running.json", pendingFixtureSession, tabStart.Add(time.Minute))
	now := time.Now()
	pf := &pendingFixture{t: t, home: home}
	pf.messages("hash0", pendingFixtureSession, eventRow("turn_start", "exec-1", now.Add(-time.Hour)))
	pf.messages("hash1", workflowFixtureStepOne,
		pendingApprovalRow("step-tool-1", now.Add(-2*time.Second)),
		resolvedRow("step-tool-1", "accept", now.Add(-time.Second)))
	store := &fakeLatchStore{
		latches:  map[terminal.SessionID]fakeLatch{"tab1": {status: terminal.StatusInput, seq: 7}},
		relatch:  map[terminal.SessionID]fakeLatch{},
		sessions: []terminal.SessionInfo{{ID: "tab1"}},
	}

	p.pass(t.Context(), store)

	want := withdrawCall{tab: "tab1", want: terminal.StatusInput, seq: 7, ok: true}
	if got := store.calls; len(got) != 1 || got[0] != want {
		t.Errorf("withdraw calls after one tick = %+v, want [%+v]: the tick must map the tab, publish the run's owners and read the step's file in that order", got, want)
	}
}

func TestSessionPollersRunUnderTheStateDirectoryGuard(t *testing.T) {
	t.Run("a verified state directory sweeps all three each tick and stops with its context", func(t *testing.T) {
		p := newSessionPollers(filepath.Join(t.TempDir(), "state"), t.TempDir(), nil)
		if p.sessionEnv == nil {
			t.Fatal("a verified state directory produced no session title environment")
		}
		synctest.Test(t, func(t *testing.T) {
			mgr := &fakePollerSessions{sessions: []terminal.SessionInfo{{ID: "tab1"}}}
			ctx, cancel := context.WithCancel(t.Context())
			returned := make(chan struct{})
			go func() {
				defer close(returned)
				p.run(ctx, mgr)
			}()

			time.Sleep(sessionPollInterval)
			synctest.Wait()

			if got := mgr.lists.Load(); got != 3 {
				t.Errorf("List calls after one interval = %d, want 3: one sweep each from the title, workflow and pending pollers", got)
			}
			if got := mgr.latchReads.Load(); got != 1 {
				t.Errorf("StatusLatch calls after one interval = %d, want 1: the pending poller must sweep the manager it was handed", got)
			}

			cancel()
			synctest.Wait()
			select {
			case <-returned:
			default:
				t.Error("run is still running after its context was cancelled, want it returned")
			}
		})
	})

	t.Run("a refused state directory runs none", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatalf("plant the root: %v", err)
		}
		if err := os.Chmod(root, 0o770); err != nil {
			t.Fatalf("widen the planted root: %v", err)
		}
		p := newSessionPollers(root, t.TempDir(), nil)
		if p.sessionEnv != nil {
			t.Fatal("a refused state directory still produced a session title environment")
		}
		synctest.Test(t, func(t *testing.T) {
			mgr := &fakePollerSessions{sessions: []terminal.SessionInfo{{ID: "tab1"}}}
			returned := make(chan struct{})
			go func() {
				defer close(returned)
				p.run(t.Context(), mgr)
			}()

			time.Sleep(3 * sessionPollInterval)
			synctest.Wait()

			select {
			case <-returned:
			default:
				t.Error("run is still running against a refused state directory, want it returned at once")
			}
			if got := mgr.lists.Load(); got != 0 {
				t.Errorf("List calls after three intervals = %d, want 0: no poller may sweep when the state directory was refused", got)
			}
		})
	})

	t.Run("a refused state directory with a tab store saves tabs and sweeps nothing", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatalf("plant the root: %v", err)
		}
		if err := os.Chmod(root, 0o770); err != nil {
			t.Fatalf("widen the planted root: %v", err)
		}
		config := t.TempDir()
		store := newTabStore(config)
		store.arm()
		p := newSessionPollers(root, t.TempDir(), store)
		if p.sessionEnv != nil {
			t.Fatal("a refused state directory still produced a session title environment")
		}
		synctest.Test(t, func(t *testing.T) {
			mgr := &fakePollerSessions{sessions: []terminal.SessionInfo{{ID: "tab1"}}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			returned := make(chan struct{})
			go func() {
				defer close(returned)
				p.run(ctx, mgr)
			}()

			time.Sleep(sessionPollInterval + sessionPollInterval/2)
			synctest.Wait()

			if got := mgr.lists.Load(); got != 0 {
				t.Errorf("List calls after 1.5 intervals = %d, want 0: no title, workflow or pending sweep may run", got)
			}
			if got := mgr.captures.Load(); got != 1 {
				t.Errorf("CaptureSessionSet calls after 1.5 intervals = %d, want 1: the tab set is still saved", got)
			}
			select {
			case <-returned:
				t.Error("run returned with a tab store to save to, want it still ticking")
			default:
			}
			raw, err := os.ReadFile(filepath.Join(config, tabsFileName))
			if err != nil {
				t.Fatalf("read the saved tab set: %v", err)
			}
			want, _ := json.Marshal(fakeCapturedSet)
			if string(raw) != string(want) {
				t.Errorf("tabs.json = %s, want %s", raw, want)
			}
		})
	})
}
