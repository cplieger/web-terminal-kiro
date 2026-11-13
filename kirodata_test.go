package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/toolbelt/v3/pin"
)

// newDataDir is a kiro-cli data dir under XDG_DATA_HOME with empty kas/ and
// run/ stores.
func newDataDir(t *testing.T) string {
	t.Helper()
	share := filepath.Join(t.TempDir(), "share")
	t.Setenv("XDG_DATA_HOME", share)
	dir := filepath.Join(share, "kiro-cli")
	for _, d := range []string{"kas", "run"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func plant(t *testing.T, dir string, dirs, files []string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// cleanFor runs the cleanup to completion as the server does once the install
// manager activates version.
func cleanFor(t *testing.T, dir, version string) *kiroDataCleanup {
	t.Helper()
	c := newKiroDataCleanup(dir)
	c.watch(t.Context(), func() string { return version })
	if !c.settled() {
		t.Fatal("cleanup not settled after watch returned on an active version")
	}
	return c
}

func TestKiroDataCleanupKeepsOnlyTheActiveRuntime(t *testing.T) {
	t.Run("superseded runtimes and their locks go, the active one stays", func(t *testing.T) {
		dir := newDataDir(t)
		kas := filepath.Join(dir, "kas")
		plant(t, kas, []string{"2.14.2-abc", "2.13.0-def", "2.12.1-ghi"}, []string{"2.14.2-abc.lock", "2.13.0-def.lock"})
		cleanFor(t, dir, "2.14.2")
		if !exists(t, filepath.Join(kas, "2.14.2-abc")) || !exists(t, filepath.Join(kas, "2.14.2-abc.lock")) {
			t.Error("the active version's runtime or lock was removed")
		}
		for _, gone := range []string{"2.13.0-def", "2.13.0-def.lock", "2.12.1-ghi"} {
			if exists(t, filepath.Join(kas, gone)) {
				t.Errorf("%s survived, want it pruned", gone)
			}
		}
	})
	t.Run("a failed pin install that activates the fallback keeps the fallback's kas tree", func(t *testing.T) {
		dir := newDataDir(t)
		kas := filepath.Join(dir, "kas")
		plant(t, kas, []string{"2.13.0-fallback"}, nil)
		cleanFor(t, dir, "2.13.0")
		if !exists(t, filepath.Join(kas, "2.13.0-fallback")) {
			t.Error("the active fallback's runtime was pruned, so its first tab unpacks it again")
		}
	})
	t.Run("a version prefix of another is not kept with it", func(t *testing.T) {
		dir := newDataDir(t)
		kas := filepath.Join(dir, "kas")
		plant(t, kas, []string{"2.1.0-abc", "2.1.01-def"}, nil)
		cleanFor(t, dir, "2.1.0")
		if !exists(t, filepath.Join(kas, "2.1.0-abc")) || exists(t, filepath.Join(kas, "2.1.01-def")) {
			t.Error("want 2.1.0-abc kept and 2.1.01-def pruned")
		}
	})
	t.Run("entries with no leading version are kiro-cli's and stay", func(t *testing.T) {
		dir := newDataDir(t)
		kas := filepath.Join(dir, "kas")
		plant(t, kas, []string{"unpack-scratch", "index"}, []string{"store.lock"})
		cleanFor(t, dir, "2.14.2")
		for _, kept := range []string{"unpack-scratch", "index", "store.lock"} {
			if !exists(t, filepath.Join(kas, kept)) {
				t.Errorf("%s was removed, want it left alone", kept)
			}
		}
	})
	t.Run("a version-keyed symlink is removed, never its target", func(t *testing.T) {
		dir := newDataDir(t)
		victim := filepath.Join(t.TempDir(), "victim")
		plant(t, victim, []string{"."}, []string{"data"})
		link := filepath.Join(dir, "kas", "2.13.0-evil")
		if err := os.Symlink(victim, link); err != nil {
			t.Fatal(err)
		}
		cleanFor(t, dir, "2.14.2")
		if !exists(t, filepath.Join(victim, "data")) {
			t.Error("the cleanup deleted through a symlink in the store")
		}
		if exists(t, link) {
			t.Error("the superseded symlink entry survived")
		}
	})
}

func TestKiroDataCleanupRefusesARedirectedStore(t *testing.T) {
	cases := []struct {
		name string
		// redirect replaces one path under dir with a symlink to victim and
		// returns the victim path the bait sits under.
		redirect func(t *testing.T, dir, victim string) string
	}{
		{"a symlinked kas store", func(t *testing.T, dir, victim string) string {
			t.Helper()
			mustSwap(t, filepath.Join(dir, "kas"), victim)
			return victim
		}},
		{"a symlinked data dir", func(t *testing.T, dir, victim string) string {
			t.Helper()
			plant(t, victim, []string{"kas", "run"}, nil)
			mustSwap(t, dir, victim)
			return filepath.Join(victim, "kas")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := newDataDir(t)
			victim := filepath.Join(t.TempDir(), "victim")
			bait := tc.redirect(t, dir, victim)
			plant(t, bait, []string{"2.13.0-victim"}, []string{"2.13.0-victim/data"})
			cleanFor(t, dir, "2.14.2")
			if !exists(t, filepath.Join(bait, "2.13.0-victim", "data")) {
				t.Error("the cleanup deleted through the symlink")
			}
		})
	}
}

// mustSwap replaces path with a symlink to target.
func mustSwap(t *testing.T, path, target string) {
	t.Helper()
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func TestKiroDataCleanupPurgesPinnedCopies(t *testing.T) {
	copies := []string{"chat-cli-2.22.0", ".chat-cli-2.22.0.heartbeat", "chat-cli-2.27.0", ".chat-cli-2.27.0.heartbeat"}
	t.Run("copies and heartbeats go when pinning is off", func(t *testing.T) {
		t.Setenv(skipBinaryPinningKey, "1")
		dir := newDataDir(t)
		run := filepath.Join(dir, "run")
		plant(t, run, nil, copies)
		cleanFor(t, dir, "2.14.2")
		for _, c := range copies {
			if exists(t, filepath.Join(run, c)) {
				t.Errorf("%s survived, want it removed", c)
			}
		}
	})
	for _, gate := range []string{"", "0", "true"} {
		t.Run("run/ is kiro-cli's live state under gate "+`"`+gate+`"`, func(t *testing.T) {
			t.Setenv(skipBinaryPinningKey, gate)
			dir := newDataDir(t)
			run := filepath.Join(dir, "run")
			plant(t, run, nil, copies)
			cleanFor(t, dir, "2.14.2")
			for _, c := range copies {
				if !exists(t, filepath.Join(run, c)) {
					t.Errorf("%s was removed while pinning is on", c)
				}
			}
		})
	}
	t.Run("other run/ state, directories and symlinks stay", func(t *testing.T) {
		t.Setenv(skipBinaryPinningKey, "1")
		dir := newDataDir(t)
		run := filepath.Join(dir, "run")
		plant(t, run, []string{"turn-markers", "chat-cli-9.9.9"}, []string{"turn-markers/m", "run-receipts", "chat-cli-notes", ".chat-cli-notes.heartbeat"})
		bait := filepath.Join(t.TempDir(), "bait")
		plant(t, filepath.Dir(bait), nil, []string{"bait"})
		if err := os.Symlink(bait, filepath.Join(run, "chat-cli-8.8.8")); err != nil {
			t.Fatal(err)
		}
		cleanFor(t, dir, "2.14.2")
		for _, kept := range []string{"turn-markers/m", "run-receipts", "chat-cli-notes", ".chat-cli-notes.heartbeat", "chat-cli-9.9.9", "chat-cli-8.8.8"} {
			if !exists(t, filepath.Join(run, kept)) {
				t.Errorf("%s was removed, want it left alone", kept)
			}
		}
		if !exists(t, bait) {
			t.Error("the symlink's target was removed")
		}
	})
	t.Run("a symlinked run dir is refused", func(t *testing.T) {
		t.Setenv(skipBinaryPinningKey, "1")
		dir := newDataDir(t)
		victim := filepath.Join(t.TempDir(), "victim")
		mustSwap(t, filepath.Join(dir, "run"), victim)
		plant(t, victim, nil, []string{"chat-cli-1.0.0"})
		cleanFor(t, dir, "2.14.2")
		if !exists(t, filepath.Join(victim, "chat-cli-1.0.0")) {
			t.Error("the purge deleted through the symlink")
		}
	})
}

func TestKiroDataCleanupWatch(t *testing.T) {
	t.Run("it cleans for the first activated version only", func(t *testing.T) {
		dir := newDataDir(t)
		kas := filepath.Join(dir, "kas")
		plant(t, kas, []string{"2.13.0-old"}, nil)
		synctest.Test(t, func(t *testing.T) {
			var active atomic.Value
			active.Store("")
			c := newKiroDataCleanup(dir)
			go c.watch(t.Context(), func() string { return active.Load().(string) })
			time.Sleep(2 * activationPoll)
			synctest.Wait()
			if c.settled() || !exists(t, filepath.Join(kas, "2.13.0-old")) {
				t.Fatal("the cleanup ran before any version was active")
			}
			active.Store("2.13.0")
			time.Sleep(activationPoll)
			synctest.Wait()
			if !c.settled() {
				t.Fatal("not settled one poll after a version became active")
			}
			// A retry activating the pin later must leave the runtime a live tab runs on.
			plant(t, kas, []string{"2.14.2-new"}, nil)
			active.Store("2.14.2")
			time.Sleep(2 * activationPoll)
			synctest.Wait()
			if !exists(t, filepath.Join(kas, "2.13.0-old")) {
				t.Error("a later activation pruned the first version's runtime")
			}
		})
	})
	t.Run("it stops unsettled when its context ends", func(t *testing.T) {
		c := newKiroDataCleanup(newDataDir(t))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		c.watch(ctx, func() string { return "" })
		if c.settled() {
			t.Error("settled with no active version")
		}
	})
	t.Run("no data dir settles with nothing to clean", func(t *testing.T) {
		c := cleanFor(t, "", "2.14.2")
		if !c.settled() {
			t.Error("not settled")
		}
	})
}

func TestManagedReadyWaitsForTheCleanup(t *testing.T) {
	c := newKiroDataCleanup("")
	ready := managedReady(func() (bool, pin.Reason) { return true, pin.ReasonReady }, c)
	if ok, why := ready(); ok || why != reasonInstalling {
		t.Errorf("before the cleanup ran: ready() = %v, %q, want false, %q", ok, why, reasonInstalling)
	}
	c.watch(t.Context(), func() string { return "2.14.2" })
	if ok, why := ready(); !ok || why != "" {
		t.Errorf("after the cleanup ran: ready() = %v, %q, want true, \"\"", ok, why)
	}
	retrying := managedReady(func() (bool, pin.Reason) { return false, pin.ReasonRetrying }, c)
	if ok, why := retrying(); ok || why != reasonRetrying {
		t.Errorf("manager retrying: ready() = %v, %q, want false, %q", ok, why, reasonRetrying)
	}
}

func TestKiroDataDir(t *testing.T) {
	t.Run("XDG_DATA_HOME wins", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "/data")
		t.Setenv("HOME", "/config/home")
		if got := kiroDataDir(); got != "/data/kiro-cli" {
			t.Errorf("kiroDataDir() = %q, want /data/kiro-cli", got)
		}
	})
	t.Run("HOME otherwise", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "")
		t.Setenv("HOME", "/config/home")
		if got := kiroDataDir(); got != "/config/home/.local/share/kiro-cli" {
			t.Errorf("kiroDataDir() = %q, want /config/home/.local/share/kiro-cli", got)
		}
	})
	t.Run("neither set", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "")
		t.Setenv("HOME", "")
		if got := kiroDataDir(); got != "" {
			t.Errorf("kiroDataDir() = %q, want \"\"", got)
		}
	})
}
