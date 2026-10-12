package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/web-terminal-engine/v6/terminal"
)

// plantRecord writes kiro-cli's session record for id under one hash directory, its "id"
// field set to recordID, and returns the record directory.
func plantRecord(t *testing.T, home, id, recordID, title string) string {
	t.Helper()
	dir := filepath.Join(home, ".kiro", "sessions", "hash0", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("plant the record dir: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"id": recordID, "title": title})
	if err := os.WriteFile(filepath.Join(dir, "session.json"), body, 0o600); err != nil {
		t.Fatalf("plant session.json: %v", err)
	}
	return dir
}

func plantLock(t *testing.T, dir string, startedAt time.Time) string {
	t.Helper()
	path := filepath.Join(dir, ".lock")
	body := `{"pid":` + strconv.Itoa(os.Getpid()) + `,"started_at":"` + startedAt.UTC().Format(time.RFC3339Nano) + `"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("plant .lock: %v", err)
	}
	return path
}

// recordingBuilder stands in for newSessionBuilder: invoking a spec's Build reports the
// resume id the builder was asked for.
func recordingBuilder() (build func(string) func(terminal.SessionID) *terminal.Handler, built func(terminal.CreateSpec) string) {
	var last string
	build = func(resumeID string) func(terminal.SessionID) *terminal.Handler {
		return func(terminal.SessionID) *terminal.Handler {
			last = resumeID
			return nil
		}
	}
	built = func(spec terminal.CreateSpec) string {
		last = "<not built>"
		spec.Build("tab")
		return last
	}
	return build, built
}

func entrySet(entries ...terminal.SessionSetEntry) terminal.SessionSet {
	return terminal.SessionSet{
		Version:  terminal.SessionSetVersion,
		Sessions: entries,
		Layout:   terminal.SessionSetLayout{Left: new(0), Selected: terminal.PaneLeft, Handle: 0.5},
	}
}

func TestTabRestorer_plan(t *testing.T) {
	boot := time.Now()
	newRestorer := func(t *testing.T, home string, set terminal.SessionSet) (*tabRestorer, func(terminal.CreateSpec) string) {
		t.Helper()
		build, built := recordingBuilder()
		var pending atomic.Bool
		return newTabRestorer(newSessionTitleSync(t.TempDir(), home), &set, build, &pending, boot), built
	}
	planOne := func(t *testing.T, r *tabRestorer, i int) (terminal.CreateSpec, bool) {
		t.Helper()
		return r.plan(t.Context(), i, r.set.Sessions[i])
	}

	t.Run("an existing record resumes under its id", func(t *testing.T) {
		home := t.TempDir()
		plantRecord(t, home, resumeA, resumeA, "fix build")
		r, built := newRestorer(t, home, entrySet(terminal.SessionSetEntry{Alias: "k3q7m2xa", PinnedTitle: "pin", Resume: resumeA}))
		spec, ok := planOne(t, r, 0)
		if !ok || spec.Alias != resumeA || spec.PinnedTitle != "pin" || built(spec) != resumeA {
			t.Errorf("plan = %+v ok=%v built %q, want a resume of %s under its id", spec, ok, built(spec), resumeA)
		}
		if r.resumed[0] != resumeA {
			t.Errorf("resumed[0] = %q, want %q", r.resumed[0], resumeA)
		}
	})

	fresh := []struct {
		name, resume, recordID, log string
	}{
		{"a missing record", resumeA, "", "kiro session record missing"},
		{"a record whose id differs", resumeA, resumeB, "kiro session record missing"},
		{"a malformed id", "sess_-", "", "stored kiro session id is malformed"},
		{"an id only the loose grammar accepts", "sess_0123456789abcdef", "", "stored kiro session id is malformed"},
	}
	for _, tt := range fresh {
		t.Run(tt.name+" opens a fresh chat", func(t *testing.T) {
			logs := capturedLog(t)
			home := t.TempDir()
			if tt.recordID != "" {
				plantRecord(t, home, tt.resume, tt.recordID, "title")
			}
			r, built := newRestorer(t, home, entrySet(terminal.SessionSetEntry{Alias: tt.resume, Resume: tt.resume}))
			spec, ok := planOne(t, r, 0)
			if !ok || spec.Alias != "" || built(spec) != "" {
				t.Errorf("plan = %+v ok=%v built %q, want a fresh chat with a minted alias", spec, ok, built(spec))
			}
			if n := strings.Count(logs.String(), tt.log); n != 1 {
				t.Errorf("%q records = %d, want 1", tt.log, n)
			}
			if _, resumed := r.resumed[0]; resumed {
				t.Error("a fresh chat was recorded as resumed")
			}
		})
	}

	t.Run("a stale lock is cleared before the resume", func(t *testing.T) {
		home := t.TempDir()
		lock := plantLock(t, plantRecord(t, home, resumeA, resumeA, "t"), boot.Add(-time.Hour))
		r, built := newRestorer(t, home, entrySet(terminal.SessionSetEntry{Alias: resumeA, Resume: resumeA}))
		if spec, ok := planOne(t, r, 0); !ok || built(spec) != resumeA {
			t.Errorf("plan = %+v ok=%v, want a resume", spec, ok)
		}
		if _, err := os.Lstat(lock); !os.IsNotExist(err) {
			t.Errorf("the stale .lock survived the plan (%v)", err)
		}
	})

	t.Run("an oversized record resumes and its stale lock is cleared", func(t *testing.T) {
		logs := capturedLog(t)
		home := t.TempDir()
		dir := plantRecord(t, home, resumeA, resumeA, "t")
		big := bytes.Repeat([]byte(" "), maxTitleFileBytes+1)
		if err := os.WriteFile(filepath.Join(dir, "session.json"), big, 0o600); err != nil {
			t.Fatalf("plant the oversized session.json: %v", err)
		}
		lock := plantLock(t, dir, boot.Add(-time.Hour))
		r, built := newRestorer(t, home, entrySet(terminal.SessionSetEntry{Alias: resumeA, Resume: resumeA}))
		if spec, ok := planOne(t, r, 0); !ok || built(spec) != resumeA {
			t.Errorf("plan = %+v ok=%v, want a resume of %s", spec, ok, resumeA)
		}
		if _, err := os.Lstat(lock); !os.IsNotExist(err) {
			t.Errorf("the stale .lock beside an unreadable record survived the plan (%v)", err)
		}
		if strings.Contains(logs.String(), "kiro session store unreadable") {
			t.Error("one unreadable record was reported as an unreadable store")
		}
	})

	t.Run("a null resume keeps the stored alias", func(t *testing.T) {
		r, built := newRestorer(t, t.TempDir(), entrySet(terminal.SessionSetEntry{Alias: "k3q7m2xa", PinnedTitle: "pin"}))
		spec, ok := planOne(t, r, 0)
		if !ok || spec.Alias != "k3q7m2xa" || spec.PinnedTitle != "pin" || built(spec) != "" {
			t.Errorf("plan = %+v ok=%v, want a fresh chat under k3q7m2xa", spec, ok)
		}
	})

	t.Run("a duplicate is skipped and the entry named after the id owns it", func(t *testing.T) {
		home := t.TempDir()
		plantRecord(t, home, resumeA, resumeA, "t")
		r, built := newRestorer(t, home, entrySet(
			terminal.SessionSetEntry{Alias: "k3q7m2xa", Resume: resumeA},
			terminal.SessionSetEntry{Alias: resumeA, Resume: resumeA},
		))
		if spec, ok := planOne(t, r, 0); ok {
			t.Errorf("plan(0) = %+v, want the earlier duplicate skipped", spec)
		}
		if spec, ok := planOne(t, r, 1); !ok || built(spec) != resumeA || spec.Alias != resumeA {
			t.Errorf("plan(1) = %+v ok=%v, want the alias owner resumed", spec, ok)
		}
	})

	t.Run("an unreadable store resumes with one Warn", func(t *testing.T) {
		logs := capturedLog(t)
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, ".kiro"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".kiro", "sessions"), []byte("not a dir"), 0o600); err != nil {
			t.Fatal(err)
		}
		r, built := newRestorer(t, home, entrySet(
			terminal.SessionSetEntry{Alias: resumeA, Resume: resumeA},
			terminal.SessionSetEntry{Alias: resumeB, Resume: resumeB},
		))
		for i, want := range []string{resumeA, resumeB} {
			if spec, ok := planOne(t, r, i); !ok || built(spec) != want {
				t.Errorf("plan(%d) = %+v ok=%v, want a resume of %s", i, spec, ok, want)
			}
		}
		if n := strings.Count(logs.String(), "kiro session store unreadable; resuming without the record check"); n != 1 {
			t.Errorf("store-unreadable Warns = %d, want 1 for two entries", n)
		}
	})
}

func TestClearStaleKiroLock(t *testing.T) {
	boot := time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)
	tests := []struct {
		plant    func(t *testing.T, dir string) string
		name     string
		log      string
		wantKept bool
	}{
		{name: "no lock", plant: func(*testing.T, string) string { return "" }},
		{
			name:  "a lock from before the boot naming a live pid",
			plant: func(t *testing.T, dir string) string { return plantLock(t, dir, boot.Add(-time.Hour)) },
		},
		{
			name:     "a lock written in this boot",
			plant:    func(t *testing.T, dir string) string { return plantLock(t, dir, boot) },
			log:      "kiro session is open in another process of this boot",
			wantKept: true,
		},
		{
			name: "an undecodable lock",
			plant: func(t *testing.T, dir string) string {
				p := filepath.Join(dir, ".lock")
				_ = os.WriteFile(p, []byte("{garbage"), 0o600)
				return p
			},
		},
		{
			name: "an unparseable started_at",
			plant: func(t *testing.T, dir string) string {
				p := filepath.Join(dir, ".lock")
				_ = os.WriteFile(p, []byte(`{"pid":1,"started_at":"yesterday"}`), 0o600)
				return p
			},
		},
		{
			name: "a lock that cannot be removed",
			plant: func(t *testing.T, dir string) string {
				p := filepath.Join(dir, ".lock")
				if err := os.MkdirAll(filepath.Join(p, "inner"), 0o700); err != nil {
					t.Fatal(err)
				}
				return p
			},
			log:      "could not clear a stale kiro session lock",
			wantKept: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := capturedLog(t)
			dir := t.TempDir()
			lock := tt.plant(t, dir)
			clearStaleKiroLock(t.Context(), dir, resumeA, boot)
			if lock != "" {
				_, err := os.Lstat(lock)
				if kept := err == nil; kept != tt.wantKept {
					t.Errorf("lock kept = %v, want %v", kept, tt.wantKept)
				}
			}
			if tt.log != "" && strings.Count(logs.String(), tt.log) != 1 {
				t.Errorf("want one %q record, got log %q", tt.log, logs.String())
			}
			if tt.log == "" && lock == "" && logs.Len() != 0 {
				t.Errorf("no lock logged %q, want nothing", logs.String())
			}
		})
	}

	t.Run("a symlinked lock is removed and its target kept", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		lock := filepath.Join(dir, ".lock")
		if err := os.Symlink(target, lock); err != nil {
			t.Fatal(err)
		}
		clearStaleKiroLock(t.Context(), dir, resumeA, boot)
		if _, err := os.Lstat(lock); !os.IsNotExist(err) {
			t.Errorf("the symlinked lock survived (%v)", err)
		}
		if got := mustReadFile(t, target); string(got) != "keep" {
			t.Errorf("the link's target changed to %q", got)
		}
	})
}

// argvIgnoringCmd is a session command that accepts and ignores --resume-id.
func argvIgnoringCmd() func() []string {
	return staticCmd("/bin/sh", "-c", "exec cat", "sh")
}

func TestTabRestorer_seedsTheMapping(t *testing.T) {
	t.Run("titles enabled", func(t *testing.T) {
		home := t.TempDir()
		plantRecord(t, home, resumeA, resumeA, "fix the build")
		pollers := newSessionPollers(filepath.Join(t.TempDir(), "state"), home, nil)
		if pollers.sessionEnv == nil {
			t.Fatal("a verified state directory produced no session title environment")
		}
		deps := newTestDeps(true)
		deps.cmd = argvIgnoringCmd()
		deps.sessionTitleEnv = pollers.sessionEnv
		build := newSessionBuilder(deps)
		mgr := terminal.NewSessionManager(build(""))
		t.Cleanup(func() { shutdownManager(t, mgr) })
		var pending atomic.Bool
		pending.Store(true)
		r := newTabRestorer(pollers.titles, new(entrySet(terminal.SessionSetEntry{Alias: resumeA, Resume: resumeA})), build, &pending, time.Now())

		r.run(t.Context(), mgr, func() bool { return true }, nil)
		pollers.pass(t.Context(), mgr)

		list := mgr.List()
		if len(list) != 1 {
			t.Fatalf("restored %d tabs, want 1", len(list))
		}
		if list[0].Title != "fix the build" || list[0].Alias != resumeA {
			t.Errorf("restored tab title %q alias %q, want the record's title and its id", list[0].Title, list[0].Alias)
		}
		if got := pollers.titles.mappedSessions()[list[0].ID]; got != resumeA {
			t.Errorf("mapped kiro session = %q, want %q with no hook run", got, resumeA)
		}
	})

	t.Run("titles refused", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(root, 0o770); err != nil {
			t.Fatal(err)
		}
		leaf := filepath.Join(root, titleStateDirName)
		if err := os.Mkdir(leaf, 0o700); err != nil {
			t.Fatal(err)
		}
		pollers := newSessionPollers(root, t.TempDir(), nil)
		if pollers.sessionEnv != nil {
			t.Fatal("a refused state directory still produced a session title environment")
		}
		mgr := catManager(t)
		tab := createTab(t, mgr)
		if err := pollers.titles.seedMapping(t.Context(), tab, resumeA); err != nil {
			t.Errorf("seedMapping = %v, want nil", err)
		}
		if entries, err := os.ReadDir(leaf); err != nil || len(entries) != 0 {
			t.Errorf("the refused state dir holds %d entries (%v), want none", len(entries), err)
		}
	})
}

func TestTabRestorer_keepsTheFileWhenNothingStarts(t *testing.T) {
	deps := newTestDeps(true)
	deps.cmd = staticCmd("/nonexistent/kiro-cli")
	build := newSessionBuilder(deps)
	mgr := terminal.NewSessionManager(build(""))
	t.Cleanup(func() { shutdownManager(t, mgr) })
	if _, err := mgr.CreateWith(terminal.CreateSpec{}); err == nil {
		t.Fatal("fixture: a create with a missing command started, want it to fail")
	}

	store := newTabStore(t.TempDir())
	saved := encodeSet(t, entrySet(terminal.SessionSetEntry{Alias: "k3q7m2xa"}, terminal.SessionSetEntry{Alias: "q2w3e4r5"}))
	if err := os.WriteFile(store.path, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	set, ok := store.load(t.Context())
	if !ok {
		t.Fatal("load = not ok")
	}
	var pending atomic.Bool
	pending.Store(true)
	newTabRestorer(newSessionTitleSync(t.TempDir(), t.TempDir()), &set, build, &pending, time.Now()).
		run(t.Context(), mgr, func() bool { return true }, store)

	if pending.Load() {
		t.Error("pending still set after the restore, want it cleared")
	}
	store.persist(t.Context(), liveSet, nil)
	if got := mustReadFile(t, store.path); !bytes.Equal(got, saved) {
		t.Errorf("tabs.json = %s after a save, want the stored set kept", got)
	}
}
