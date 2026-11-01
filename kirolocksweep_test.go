package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// plantLock writes kiro-cli's pid-only session lock into dir, naming this test's own
// (live) pid, and returns its path.
func plantLock(t *testing.T, dir string, startedAt time.Time) string {
	t.Helper()
	path := filepath.Join(dir, ".lock")
	body := `{"pid":` + strconv.Itoa(os.Getpid()) + `,"started_at":"` + startedAt.UTC().Format(time.RFC3339Nano) + `"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("plant .lock: %v", err)
	}
	return path
}

// resumeC is a third well-formed session id beside tabstate_test.go's two.
const resumeC = "sess_47400f57-1c2d-4e5f-8a9b-00000000000c"

func plantFile(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return err == nil
}

func TestClearStaleKiroLock(t *testing.T) {
	boot := time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)
	tests := []struct {
		plant       func(t *testing.T, dir string) string
		name        string
		wantRemoved bool
		wantErr     bool
	}{
		{name: "no lock", plant: func(*testing.T, string) string { return "" }},
		{
			name:        "a lock from before the boot naming a live pid",
			plant:       func(t *testing.T, dir string) string { return plantLock(t, dir, boot.Add(-time.Hour)) },
			wantRemoved: true,
		},
		{
			name:  "a lock written in this boot",
			plant: func(t *testing.T, dir string) string { return plantLock(t, dir, boot) },
		},
		{
			name: "a lock kiro-cli writes today, from before the boot",
			plant: func(t *testing.T, dir string) string {
				return plantFile(t, filepath.Join(dir, ".lock"),
					`{"pid":1,"started_at":"2026-10-11T00:00:00.000Z","linux_boot_id":"b","linux_start_ticks":"7"}`)
			},
			wantRemoved: true,
		},
		{
			name:        "an undecodable lock",
			plant:       func(t *testing.T, dir string) string { return plantFile(t, filepath.Join(dir, ".lock"), "{garbage") },
			wantRemoved: true,
		},
		{
			name: "an unparseable started_at",
			plant: func(t *testing.T, dir string) string {
				return plantFile(t, filepath.Join(dir, ".lock"), `{"pid":1,"started_at":"yesterday"}`)
			},
			wantRemoved: true,
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
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			lock := tt.plant(t, dir)
			removed, err := clearStaleKiroLock(t.Context(), dir, boot)
			if removed != tt.wantRemoved || (err != nil) != tt.wantErr {
				t.Errorf("clearStaleKiroLock = (%v, %v), want removed %v, error %v", removed, err, tt.wantRemoved, tt.wantErr)
			}
			if kept := lock != "" && exists(t, lock); lock != "" && kept == tt.wantRemoved {
				t.Errorf("lock kept = %v, want %v", kept, !tt.wantRemoved)
			}
		})
	}

	t.Run("a symlinked lock is removed and its target kept", func(t *testing.T) {
		dir := t.TempDir()
		target := plantFile(t, filepath.Join(t.TempDir(), "target"), "keep")
		lock := filepath.Join(dir, ".lock")
		if err := os.Symlink(target, lock); err != nil {
			t.Fatal(err)
		}
		if removed, err := clearStaleKiroLock(t.Context(), dir, boot); !removed || err != nil {
			t.Errorf("clearStaleKiroLock = (%v, %v), want the link removed", removed, err)
		}
		if exists(t, lock) {
			t.Error("the symlinked lock survived")
		}
		if got := mustReadFile(t, target); string(got) != "keep" {
			t.Errorf("the link's target changed to %q", got)
		}
	})
}

func TestSweepStaleKiroLocks(t *testing.T) {
	boot := time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)

	t.Run("clears every stale session lock and nothing else", func(t *testing.T) {
		logs := capturedLog(t)
		root := filepath.Join(t.TempDir(), "sessions")
		staleA := plantLock(t, filepath.Dir(plantFile(t, filepath.Join(root, "hash0", resumeA, "session.json"), "{}")), boot.Add(-time.Hour))
		staleB := plantLock(t, filepath.Dir(plantFile(t, filepath.Join(root, "hash1", resumeB, "session.json"), "{}")), boot.Add(-time.Minute))
		live := plantLock(t, filepath.Dir(plantFile(t, filepath.Join(root, "hash1", resumeC, "session.json"), "{}")), boot.Add(time.Minute))
		index := plantFile(t, filepath.Join(root, "hash0", ".index", ".lock"), "")
		notSession := plantFile(t, filepath.Join(root, "hash0", "sess_-", ".lock"), "{garbage")
		plantFile(t, filepath.Join(root, "hash0", "sess_47400f57-1c2d-4e5f-8a9b-00000000000d"), "a file, not a session")
		plantFile(t, filepath.Join(root, "stray-file"), "")

		sweepStaleKiroLocks(t.Context(), root, boot)

		for _, p := range []string{staleA, staleB} {
			if exists(t, p) {
				t.Errorf("stale lock %s survived the sweep", p)
			}
		}
		for _, p := range []string{live, index, notSession} {
			if !exists(t, p) {
				t.Errorf("%s was removed, want it kept", p)
			}
		}
		if !strings.Contains(logs.String(), "cleared=2") {
			t.Errorf("want one summary naming cleared=2, got log %q", logs.String())
		}
	})

	t.Run("a lock it cannot clear warns once", func(t *testing.T) {
		logs := capturedLog(t)
		root := t.TempDir()
		for _, id := range []string{resumeA, resumeB} {
			if err := os.MkdirAll(filepath.Join(root, "hash0", id, ".lock", "inner"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		sweepStaleKiroLocks(t.Context(), root, boot)
		const msg = "some stale locks could not be cleared"
		if n := strings.Count(logs.String(), msg); n != 1 || !strings.Contains(logs.String(), "failed=2") {
			t.Errorf("want one %q record with failed=2, got log %q", msg, logs.String())
		}
	})

	t.Run("an unreadable store warns", func(t *testing.T) {
		logs := capturedLog(t)
		root := plantFile(t, filepath.Join(t.TempDir(), "sessions"), "not a dir")
		sweepStaleKiroLocks(t.Context(), root, boot)
		if !strings.Contains(logs.String(), "the session store is unreadable") {
			t.Errorf("want the unreadable-store Warn, got log %q", logs.String())
		}
	})

	t.Run("a missing store and a relative root are silent no-ops", func(t *testing.T) {
		logs := capturedLog(t)
		sweepStaleKiroLocks(t.Context(), filepath.Join(t.TempDir(), "absent"), boot)
		t.Chdir(t.TempDir())
		lock := plantLock(t, filepath.Dir(plantFile(t, filepath.Join(".kiro", "sessions", "hash0", resumeA, "session.json"), "{}")), boot.Add(-time.Hour))
		sweepStaleKiroLocks(t.Context(), filepath.Join(".kiro", "sessions"), boot)
		if !exists(t, lock) {
			t.Error("a relative root was swept")
		}
		if logs.Len() != 0 {
			t.Errorf("want no log, got %q", logs.String())
		}
	})

	t.Run("a cancelled context stops the sweep", func(t *testing.T) {
		root := t.TempDir()
		lock := plantLock(t, filepath.Dir(plantFile(t, filepath.Join(root, "hash0", resumeA, "session.json"), "{}")), boot.Add(-time.Hour))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		sweepStaleKiroLocks(ctx, root, boot)
		if !exists(t, lock) {
			t.Error("a cancelled sweep removed a lock")
		}
	})
}
