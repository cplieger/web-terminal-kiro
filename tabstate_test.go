package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cplieger/web-terminal-engine/v6/terminal"
)

const (
	tabsUnreadableMsg = "tabs.json unreadable; no tabs restored"
	tabsKeptMsg       = "tabs.json unreadable and could not be moved aside; tabs are not saved this run"
	resumeA           = "sess_47400f57-1c2d-4e5f-8a9b-00000000000a"
	resumeB           = "sess_47400f57-1c2d-4e5f-8a9b-00000000000b"
)

// catManager is a real session manager whose tabs run /bin/cat, torn down with t.
func catManager(t *testing.T) *terminal.SessionManager {
	t.Helper()
	mgr := terminal.NewSessionManager(func(terminal.SessionID) *terminal.Handler {
		return terminal.NewHandler([]string{"/bin/cat"}, terminal.WithLogger(nil))
	})
	t.Cleanup(func() { shutdownManager(t, mgr) })
	return mgr
}

func createTab(t *testing.T, mgr *terminal.SessionManager) terminal.SessionID {
	t.Helper()
	id, err := mgr.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return id
}

// fixedCapturer captures one set whatever the live state.
type fixedCapturer terminal.SessionSet

func (f fixedCapturer) CaptureSessionSet(func(terminal.SessionInfo) string) terminal.SessionSet {
	return terminal.SessionSet(f)
}

var liveSet = fixedCapturer{
	Version:  terminal.SessionSetVersion,
	Sessions: []terminal.SessionSetEntry{{Alias: "k3q7m2xa"}},
	Layout:   terminal.SessionSetLayout{Left: new(0), Selected: terminal.PaneLeft, Handle: 0.5},
}

func readSavedSet(t *testing.T, path string) terminal.SessionSet {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	set, err := terminal.ParseSessionSet(raw)
	if err != nil {
		t.Fatalf("ParseSessionSet(%s) = %v", raw, err)
	}
	return set
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return raw
}

func TestTabStore_roundTrip(t *testing.T) {
	mgr := catManager(t)
	a, b := createTab(t, mgr), createTab(t, mgr)
	if !mgr.SetSessionAlias(a, resumeA) || !mgr.SetSessionPinnedTitle(b, "fix build") {
		t.Fatal("seed the tabs' alias and pin")
	}
	dir := t.TempDir()
	store := newTabStore(dir)
	store.arm()
	store.persist(t.Context(), mgr, nil)

	fi, err := os.Stat(store.path)
	if err != nil {
		t.Fatalf("stat tabs.json: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("tabs.json mode = %#o, want 0600", fi.Mode().Perm())
	}
	got, ok := newTabStore(dir).load(t.Context())
	if !ok {
		t.Fatal("load after persist = not ok, want the saved set")
	}
	want := terminal.SessionSet{
		Version: terminal.SessionSetVersion,
		Sessions: []terminal.SessionSetEntry{
			{Alias: resumeA, Resume: resumeA},
			{Alias: listedAlias(t, mgr, b), PinnedTitle: "fix build"},
		},
	}
	if len(got.Sessions) != 2 || got.Sessions[0] != want.Sessions[0] || got.Sessions[1] != want.Sessions[1] {
		t.Errorf("loaded sessions = %+v, want %+v", got.Sessions, want.Sessions)
	}

	ino := fi.Sys().(*syscall.Stat_t).Ino
	store.persist(t.Context(), mgr, nil)
	again, err := os.Stat(store.path)
	if err != nil {
		t.Fatalf("stat tabs.json: %v", err)
	}
	if again.Sys().(*syscall.Stat_t).Ino != ino || !again.ModTime().Equal(fi.ModTime()) {
		t.Error("an unchanged set was written again, want the file untouched")
	}
}

func listedAlias(t *testing.T, mgr *terminal.SessionManager, id terminal.SessionID) string {
	t.Helper()
	for _, info := range mgr.List() {
		if info.ID == id {
			return info.Alias
		}
	}
	t.Fatalf("tab %s is not listed", terminal.LogID(id))
	return ""
}

func TestTabStore_refusesHostileFiles(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(outside, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatalf("write the link target: %v", err)
	}
	regular := func(body []byte) func(t *testing.T, path string) {
		return func(t *testing.T, path string) {
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatalf("plant tabs.json: %v", err)
			}
		}
	}
	tests := []struct {
		plant func(t *testing.T, path string)
		check func(t *testing.T, aside string)
		name  string
	}{
		{name: "corrupt", plant: regular([]byte("{not json"))},
		{name: "oversize", plant: regular(bytes.Repeat([]byte(" "), terminal.MaxSessionSetBytes+1))},
		{name: "a future version", plant: regular([]byte(`{"version":2,"sessions":[],"layout":{"selected":"left","handle":0.5}}`))},
		{
			name: "a symlink",
			plant: func(t *testing.T, path string) {
				if err := os.Symlink(outside, path); err != nil {
					t.Fatalf("plant the link: %v", err)
				}
			},
			check: func(t *testing.T, aside string) {
				if fi, err := os.Lstat(aside); err != nil || fi.Mode()&os.ModeSymlink == 0 {
					t.Errorf("tabs.json.unreadable is not the moved link (%v)", err)
				}
				if got := mustReadFile(t, outside); string(got) != `{"version":1}` {
					t.Errorf("the link's target changed to %q", got)
				}
			},
		},
		{
			name: "a FIFO",
			plant: func(t *testing.T, path string) {
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatalf("plant the FIFO: %v", err)
				}
			},
			check: func(t *testing.T, aside string) {
				if fi, err := os.Lstat(aside); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
					t.Errorf("tabs.json.unreadable is not the moved FIFO (%v)", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := capturedLog(t)
			store := newTabStore(t.TempDir())
			tt.plant(t, store.path)
			var planted []byte
			if tt.check == nil {
				planted = mustReadFile(t, store.path)
			}

			if _, ok := store.load(t.Context()); ok {
				t.Fatal("load = ok, want nothing restored")
			}
			if n := strings.Count(logs.String(), tabsUnreadableMsg); n != 1 {
				t.Errorf("unreadable Warns = %d, want 1", n)
			}
			aside := store.path + ".unreadable"
			if tt.check != nil {
				tt.check(t, aside)
			} else if got := mustReadFile(t, aside); !bytes.Equal(got, planted) {
				t.Error("tabs.json.unreadable does not hold the planted bytes")
			}
			moved, _ := os.Lstat(aside)

			store.arm()
			store.persist(t.Context(), liveSet, nil)
			// Lstat first: reading a FIFO left in place would block the test.
			if fi, err := os.Lstat(store.path); err != nil || !fi.Mode().IsRegular() {
				t.Fatalf("tabs.json after a save is not a regular file (%v)", err)
			}
			if got := readSavedSet(t, store.path); len(got.Sessions) != 1 || got.Sessions[0].Alias != "k3q7m2xa" {
				t.Errorf("tabs.json after a save = %+v, want the live set", got)
			}
			if after, err := os.Lstat(aside); err != nil || !os.SameFile(moved, after) {
				t.Errorf("tabs.json.unreadable changed after a save (%v)", err)
			}
		})
	}

	t.Run("the unreadable file cannot be moved aside", func(t *testing.T) {
		logs := capturedLog(t)
		store := newTabStore(t.TempDir())
		corrupt := []byte(`{"version":2}`)
		if err := os.WriteFile(store.path, corrupt, 0o600); err != nil {
			t.Fatalf("plant tabs.json: %v", err)
		}
		blocker := store.path + ".unreadable"
		if err := os.MkdirAll(filepath.Join(blocker, "keep"), 0o700); err != nil {
			t.Fatalf("plant the blocking directory: %v", err)
		}
		if _, ok := store.load(t.Context()); ok {
			t.Fatal("load = ok, want nothing restored")
		}
		if n := strings.Count(logs.String(), tabsKeptMsg); n != 1 {
			t.Errorf("could-not-move Errors = %d, want 1", n)
		}
		store.arm()
		store.persist(t.Context(), liveSet, nil)
		if got := mustReadFile(t, store.path); !bytes.Equal(got, corrupt) {
			t.Errorf("tabs.json = %q after a save, want the unmovable file kept byte-identical", got)
		}
	})
}

func TestTabStore_sweepsStaleTemps(t *testing.T) {
	dir := t.TempDir()
	stale, fresh := filepath.Join(dir, ".atomicfile-1.tmp"), filepath.Join(dir, ".atomicfile-2.tmp")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("plant %s: %v", p, err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	newTabStore(dir).load(t.Context())
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Errorf("a two-hour-old temp survived load (%v), want it reclaimed", err)
	}
	if _, err := os.Lstat(fresh); err != nil {
		t.Errorf("a fresh temp was reclaimed (%v), want it kept", err)
	}
}

func TestTabStore_disarmedNeverWrites(t *testing.T) {
	store := newTabStore(t.TempDir())
	saved := []byte(`{"version":1,"sessions":[{"alias":"before"}],"layout":{"left":0,"selected":"left","handle":0.5}}`)
	if err := os.WriteFile(store.path, saved, 0o600); err != nil {
		t.Fatalf("plant tabs.json: %v", err)
	}
	store.persist(t.Context(), liveSet, nil)
	if got := mustReadFile(t, store.path); !bytes.Equal(got, saved) {
		t.Errorf("a disarmed store wrote %q, want the file untouched", got)
	}
}

func TestTabStore_cancelledContextAfterShutdownDoesNotWipe(t *testing.T) {
	mgr := catManager(t)
	createTab(t, mgr)
	createTab(t, mgr)
	store := newTabStore(t.TempDir())
	store.arm()
	ctx, cancel := context.WithCancel(t.Context())
	store.persist(ctx, mgr, nil)
	saved := mustReadFile(t, store.path)
	if got := readSavedSet(t, store.path); len(got.Sessions) != 2 {
		t.Fatalf("saved %d tabs, want 2", len(got.Sessions))
	}

	cancel()
	shutdownManager(t, mgr)
	store.persist(ctx, mgr, nil)
	if got := mustReadFile(t, store.path); !bytes.Equal(got, saved) {
		t.Errorf("a save after shutdown wrote %s, want the pre-shutdown set kept", got)
	}
}

// logLines counts the captured records at level whose message contains msg.
func logLines(logs *strings.Builder, level, msg string) int {
	n := 0
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, "level="+level+" ") && strings.Contains(line, msg) {
			n++
		}
	}
	return n
}

func TestTabStore_retriesAFailedWrite(t *testing.T) {
	const failedMsg, recoveredMsg = "tabs.json write failed", "tabs.json written again"
	logs := capturedLog(t)
	store := newTabStore(t.TempDir())
	// A non-empty directory where tabs.json belongs makes the rename fail, as root too.
	if err := os.MkdirAll(filepath.Join(store.path, "keep"), 0o700); err != nil {
		t.Fatalf("plant the blocking directory: %v", err)
	}
	store.arm()

	store.persist(t.Context(), liveSet, nil)
	if n := logLines(logs, "WARN", failedMsg); n != 1 {
		t.Fatalf("write-failed Warns after one failing pass = %d, want 1", n)
	}
	store.persist(t.Context(), liveSet, nil)
	if n := logLines(logs, "WARN", failedMsg); n != 1 {
		t.Errorf("write-failed Warns after two failing passes = %d, want still 1", n)
	}
	if n := logLines(logs, "DEBUG", failedMsg); n != 1 {
		t.Errorf("write-failed Debugs after two failing passes = %d, want 1", n)
	}

	if err := os.RemoveAll(store.path); err != nil {
		t.Fatalf("remove the blocking directory: %v", err)
	}
	store.persist(t.Context(), liveSet, nil)
	if got := readSavedSet(t, store.path); len(got.Sessions) != 1 || got.Sessions[0].Alias != "k3q7m2xa" {
		t.Errorf("tabs.json after the condition cleared = %+v, want the unchanged live set retried and saved", got)
	}
	if n := logLines(logs, "INFO", recoveredMsg); n != 1 {
		t.Errorf("recovery Infos = %d, want 1", n)
	}
	store.persist(t.Context(), liveSet, nil)
	if n := logLines(logs, "INFO", recoveredMsg); n != 1 {
		t.Errorf("recovery Infos after a further pass = %d, want still 1", n)
	}
}

func TestTabStore_userCloseRemovesTheEntry(t *testing.T) {
	mgr := catManager(t)
	a := createTab(t, mgr)
	b := createTab(t, mgr)
	store := newTabStore(t.TempDir())
	store.arm()
	store.persist(t.Context(), mgr, nil)
	if !mgr.Close(a) {
		t.Fatal("Close = false")
	}
	store.persist(t.Context(), mgr, nil)
	got := readSavedSet(t, store.path)
	if len(got.Sessions) != 1 || got.Sessions[0].Alias != listedAlias(t, mgr, b) {
		t.Errorf("saved %+v after a close, want only the remaining tab", got.Sessions)
	}
}

func TestTabStore_persistFallsBackToTheAlias(t *testing.T) {
	mgr := catManager(t)
	id := createTab(t, mgr)
	tests := []struct {
		mapped map[terminal.SessionID]string
		name   string
		alias  string
		want   string
	}{
		{name: "a kiro session alias", alias: resumeA, want: resumeA},
		{name: "a minted alias", alias: "k3q7m2xa", want: ""},
		{name: "a loose sess_ alias", alias: "sess_-", want: ""},
		{name: "a mapped id outranks the alias", alias: resumeA, mapped: map[terminal.SessionID]string{id: resumeB}, want: resumeB},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !mgr.SetSessionAlias(id, tt.alias) {
				t.Fatalf("SetSessionAlias(%q) = false", tt.alias)
			}
			store := newTabStore(t.TempDir())
			store.arm()
			store.persist(t.Context(), mgr, tt.mapped)
			if got := readSavedSet(t, store.path).Sessions[0].Resume; got != tt.want {
				t.Errorf("saved resume = %q, want %q", got, tt.want)
			}
		})
	}
}

// encodeSet is the bytes persist writes for set.
func encodeSet(t *testing.T, set terminal.SessionSet) []byte {
	t.Helper()
	raw, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return raw
}
