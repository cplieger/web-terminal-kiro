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
	"time"
)

// maxKiroLockBytes bounds the read of kiro-cli's {"pid","started_at",...} lock file.
const maxKiroLockBytes = 4096

// sweepStaleKiroLocks removes every <hash>/<session id>/.lock under sessionsRoot written
// before bootTime; it runs once at boot, ahead of the tab restore. A container stop
// leaves kiro-cli's lock behind, and a lock holding only a pid reads live to kiro-cli
// while any process or thread reuses that pid ("session is active in another terminal").
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
		if hd.IsDir() {
			t.sweepHashDir(ctx, filepath.Join(sessionsRoot, hd.Name()), bootTime)
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
		switch removed, err := clearStaleKiroLock(ctx, filepath.Join(hashDir, sd.Name()), bootTime); {
		case err != nil:
			t.fail(err)
		case removed:
			t.cleared++
		}
	}
}

func (t *lockSweepTally) fail(err error) {
	t.failed++
	t.firstErr = cmp.Or(t.firstErr, err)
}

// clearStaleKiroLock removes dir's kiro-cli session lock unless a process of this boot
// wrote it, reporting whether it removed one. A missing lock is (false, nil).
func clearStaleKiroLock(ctx context.Context, dir string, bootTime time.Time) (bool, error) {
	path := filepath.Join(dir, ".lock")
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
