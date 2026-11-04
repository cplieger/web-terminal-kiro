package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxKiroLockBytes bounds the read of kiro-cli's {"pid","started_at",...} lock file.
const maxKiroLockBytes = 4096

// sweepStaleKiroLocks removes every <hash>/<session id>/.lock and cli/<session id>.lock
// under sessionsRoot written before bootTime; it runs once at boot, ahead of the tab
// restore. A container stop leaves kiro-cli's lock behind, and a lock holding only a pid
// reads live to kiro-cli while any process or thread reuses that pid ("session is active
// in another terminal").
// Every kiro-cli this app runs is a child of this boot, so an older lock is never held.
func sweepStaleKiroLocks(ctx context.Context, sessionsRoot string, bootTime time.Time) {
	// An empty HOME makes the root relative, and the sweep would walk the server's cwd.
	if !filepath.IsAbs(sessionsRoot) {
		return
	}
	hashDirs, err := os.ReadDir(sessionsRoot)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) { // absent before kiro-cli's first session
			slog.Warn("kiro session locks: the session store is unreadable; stale locks were not cleared",
				"dir", sessionsRoot, "error", err)
		}
		return
	}
	var t lockSweepTally
	for _, hd := range hashDirs {
		dir := filepath.Join(sessionsRoot, hd.Name())
		switch {
		case hd.IsDir() && hd.Name() == kiroV2SessionsDir:
			t.sweepV2Dir(ctx, dir, bootTime)
		case hd.IsDir():
			t.sweepHashDir(ctx, dir, bootTime)
		}
		if ctx.Err() != nil {
			return
		}
	}
	if t.failed > 0 {
		slog.Warn("kiro session locks: some stale locks could not be cleared; their conversations may refuse to open",
			"failed", t.failed, "error", t.firstErr)
	}
	if t.cleared > 0 {
		slog.Info("kiro session locks: cleared the stale locks a previous boot left", "cleared", t.cleared)
	}
}

// kiroV2SessionsDir holds kiro-cli's V2-engine sessions as flat <id>.json, <id>.jsonl and
// <id>.lock files, beside the V3 hash dirs.
const kiroV2SessionsDir = "cli"

type lockSweepTally struct {
	firstErr        error
	cleared, failed int
}

// sweepHashDir clears the stale lock of every session directory in one workspace hash dir.
func (t *lockSweepTally) sweepHashDir(ctx context.Context, hashDir string, bootTime time.Time) {
	sessions, err := os.ReadDir(hashDir)
	if err != nil {
		t.fail(err)
		return
	}
	for _, sd := range sessions {
		if !sd.IsDir() || !validKiroResumeID(sd.Name()) {
			continue
		}
		t.clear(ctx, filepath.Join(hashDir, sd.Name(), ".lock"), bootTime)
	}
}

// sweepV2Dir clears every stale <session id>.lock file in the V2 sessions dir. A directory
// named *.lock is another lock kind (a held mkdir lock), never a session's.
func (t *lockSweepTally) sweepV2Dir(ctx context.Context, dir string, bootTime time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.fail(err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".lock") {
			t.clear(ctx, filepath.Join(dir, e.Name()), bootTime)
		}
	}
}

func (t *lockSweepTally) clear(ctx context.Context, path string, bootTime time.Time) {
	switch removed, err := clearStaleKiroLock(ctx, path, bootTime); {
	case err != nil:
		t.fail(err)
	case removed:
		t.cleared++
	}
}

func (t *lockSweepTally) fail(err error) {
	t.failed++
	t.firstErr = cmp.Or(t.firstErr, err)
}

// clearStaleKiroLock removes the kiro-cli session lock at path unless a process of this
// boot wrote it, reporting whether it removed one. A missing lock is (false, nil).
func clearStaleKiroLock(ctx context.Context, path string, bootTime time.Time) (bool, error) {
	raw, err := readSmallFile(ctx, path, maxKiroLockBytes)
	if errors.Is(err, fs.ErrNotExist) || ctx.Err() != nil {
		return false, nil
	}
	if err == nil && lockFromThisBoot(raw, bootTime) {
		return false, nil
	}
	// Anything else (older, undecodable, a symlink or non-regular file readSmallFile
	// refused, oversize) was not written by a live process of this boot. os.Remove on a
	// symlink removes the link, never its target.
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func lockFromThisBoot(raw []byte, bootTime time.Time) bool {
	var lock struct {
		StartedAt string `json:"started_at"`
	}
	if json.Unmarshal(raw, &lock) != nil {
		return false
	}
	started, err := time.Parse(time.RFC3339, lock.StartedAt)
	return err == nil && !started.Before(bootTime)
}
