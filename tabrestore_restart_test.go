package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/web-terminal-engine/v6/terminal"
)

// bootFixture is one server run assembled from the production units in run()'s order,
// over temp directories.
type bootFixture struct {
	pollers *sessionPollers
	mgr     *terminal.SessionManager
	store   *tabStore
	srv     *httptest.Server
	build   func(string) func(terminal.SessionID) *terminal.Handler
	pending *atomic.Bool
	set     terminal.SessionSet
	cancel  context.CancelFunc
	ctx     context.Context
	restore bool
}

// boot wires one run. Each tab's command records its positional args into argvDir under
// its title handle, then waits on stdin.
func boot(t *testing.T, configDir, home, argvDir string) *bootFixture {
	t.Helper()
	f := &bootFixture{pending: new(atomic.Bool)}
	f.ctx, f.cancel = context.WithCancel(context.Background())
	t.Cleanup(f.cancel)
	f.store = newTabStore(configDir)
	f.set, f.restore = f.store.load(f.ctx)
	f.pollers = newSessionPollers(filepath.Join(t.TempDir(), "state"), home, f.store)
	f.pending.Store(f.restore && len(f.set.Sessions) > 0)
	deps := newTestDeps(true)
	deps.restoring = f.pending.Load
	deps.sessionTitleEnv = f.pollers.sessionEnv
	deps.cmd = staticCmd("/bin/sh", "-c",
		`printf '%s\n' "$@" >"`+argvDir+`/$WT_TITLE_HANDLE-$$"; exec cat`, "sh", "--v3")
	f.build = newSessionBuilder(deps)
	mux := http.NewServeMux()
	f.mgr = registerRoutes(mux, deps, f.build(""))
	t.Cleanup(func() { shutdownManager(t, f.mgr) })
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *bootFixture) call(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, f.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

func (f *bootFixture) mustCall(t *testing.T, method, path, body string, want int) []byte {
	t.Helper()
	status, raw := f.call(t, method, path, body)
	if status != want {
		t.Fatalf("%s %s = %d %s, want %d", method, path, status, raw, want)
	}
	return raw
}

// awaitArgv reads the args the tab with this title handle was started with.
func awaitArgv(t *testing.T, argvDir, handle string) []string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		matches, _ := filepath.Glob(filepath.Join(argvDir, handle+"-*"))
		if len(matches) == 1 {
			if raw, err := os.ReadFile(matches[0]); err == nil && strings.HasSuffix(string(raw), "\n") {
				return strings.Fields(string(raw))
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no argv recorded for title handle %s (%d files)", handle, len(matches))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTabRestore_survivesARestart(t *testing.T) {
	configDir, home, argvDir := t.TempDir(), t.TempDir(), t.TempDir()

	// Boot 1: three tabs, one mapped to a kiro conversation, one pinned, a reorder
	// and a split, then one poller pass and the production shutdown.
	b1 := boot(t, configDir, home, argvDir)
	if b1.restore {
		t.Fatal("boot 1 found a saved set in an empty config dir")
	}
	b1.store.arm()
	var tabs []terminal.SessionInfo
	for range 3 {
		var info terminal.SessionInfo
		if err := json.Unmarshal(b1.mustCall(t, http.MethodPost, terminal.SessionsPath, "", http.StatusCreated), &info); err != nil {
			t.Fatal(err)
		}
		tabs = append(tabs, info)
	}
	recordDir := plantRecord(t, home, resumeA, resumeA, "restored title")
	lock := plantLock(t, recordDir, time.Now().Add(-time.Hour))
	handle, ok := b1.pollers.titles.handleFor(tabs[1].ID)
	if !ok {
		t.Fatal("tab 1 has no title handle")
	}
	if err := os.WriteFile(filepath.Join(b1.pollers.titles.stateDir, handle), []byte(resumeA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b1.mustCall(t, http.MethodPut, terminal.SessionsSubtreePath+string(tabs[2].ID)+"/pinned-title", `{"title":"pinned two"}`, http.StatusNoContent)
	b1.mustCall(t, http.MethodPut, terminal.SessionsSubtreePath+"order",
		`{"order":["`+string(tabs[2].ID)+`","`+string(tabs[0].ID)+`","`+string(tabs[1].ID)+`"]}`, http.StatusNoContent)
	b1.mustCall(t, http.MethodPut, terminal.SessionsSubtreePath+"layout",
		`{"open":true,"left":"`+string(tabs[1].ID)+`","right":"`+string(tabs[2].ID)+`","selected":"right","handle":0.3}`, http.StatusNoContent)
	b1.pollers.pass(b1.ctx, b1.mgr)
	newSessionShutdown(b1.cancel, b1.mgr)(context.Background())

	// Boot 2: a fresh store, routes and restorer over the same config dir and home.
	bootTime := time.Now()
	b2 := boot(t, configDir, home, argvDir)
	if !b2.restore || !b2.pending.Load() {
		t.Fatalf("boot 2 restore = %v pending = %v, want the saved set pending", b2.restore, b2.pending.Load())
	}
	if status, _ := b2.call(t, http.MethodGet, terminal.SessionsSubtreePath+"layout", ""); status != http.StatusServiceUnavailable {
		t.Errorf("GET layout before the restore = %d, want 503", status)
	}
	newTabRestorer(b2.pollers.titles, &b2.set, b2.build, b2.pending, bootTime).
		run(b2.ctx, b2.mgr, func() bool { return true }, b2.store)
	if b2.pending.Load() {
		t.Fatal("pending still set after the restore")
	}

	var list []terminal.SessionInfo
	if err := json.Unmarshal(b2.mustCall(t, http.MethodGet, terminal.SessionsPath, "", http.StatusOK), &list); err != nil {
		t.Fatal(err)
	}
	aliases := make([]string, len(list))
	for i, info := range list {
		aliases[i] = info.Alias
	}
	if want := []string{tabs[2].Alias, tabs[0].Alias, resumeA}; !slices.Equal(aliases, want) {
		t.Fatalf("restored aliases in order = %v, want %v", aliases, want)
	}
	if list[0].PinnedTitle != "pinned two" {
		t.Errorf("restored pin = %q, want %q", list[0].PinnedTitle, "pinned two")
	}
	var layout terminal.PaneLayout
	if err := json.Unmarshal(b2.mustCall(t, http.MethodGet, terminal.SessionsSubtreePath+"layout", "", http.StatusOK), &layout); err != nil {
		t.Fatal(err)
	}
	if layout.Left == nil || *layout.Left != list[2].ID || layout.Right == nil || *layout.Right != list[0].ID ||
		layout.Selected != terminal.PaneRight || layout.Handle != 0.3 || !layout.Open {
		t.Errorf("restored layout = %+v, want the split over the resumed tab and the pinned one, right selected, handle 0.3", layout)
	}

	for i, info := range list {
		h, ok := b2.pollers.titles.handleFor(info.ID)
		if !ok {
			t.Fatalf("restored tab %d has no title handle", i)
		}
		argv := awaitArgv(t, argvDir, h)
		want := []string{"--v3"}
		if info.Alias == resumeA {
			want = []string{"--v3", "--resume-id", resumeA}
		}
		if !slices.Equal(argv, want) {
			t.Errorf("restored tab %d (%s) argv = %v, want %v", i, info.Alias, argv, want)
		}
	}
	if _, err := os.Lstat(lock); !os.IsNotExist(err) {
		t.Errorf("the stale .lock survived the restore (%v)", err)
	}
	b2.pollers.pass(b2.ctx, b2.mgr)
	for _, info := range b2.mgr.List() {
		if info.Alias == resumeA && info.Title != "restored title" {
			t.Errorf("resumed tab title = %q after one pass, want the record's", info.Title)
		}
	}
}
