package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// maxKiroLockBytes bounds the read of kiro-cli's {"pid","started_at",...} lock file.
const maxKiroLockBytes = 4096

// sweepStaleKiroLocks removes every <hash>/<session id>/.lock and cli/<session id>.lock
// under sessionsRoot whose process is not running (procView.lockHeld); it runs once at
// boot, ahead of the tab restore. A container stop leaves kiro-cli's lock behind, and
// kiro-cli reads a lock without its owner's start ticks (every V2 lock) as live while a
// kiro-cli process reuses that pid ("Session is active in another process").
func sweepStaleKiroLocks(ctx context.Context, sessionsRoot string) {
	// An empty HOME makes the root relative, and the sweep would walk the server's cwd.
	if !filepath.IsAbs(sessionsRoot) {
		return
	}
	proc, err := readProcView(procRoot)
	if err != nil {
		slog.Warn("kiro session locks: /proc is unreadable; stale locks were not cleared", "error", err)
		return
	}
	sweepKiroLocks(ctx, sessionsRoot, proc)
}

// procRoot is the /proc the sweep reads process identities from; a test points it at a
// fixture tree.
var procRoot = "/proc"

func sweepKiroLocks(ctx context.Context, sessionsRoot string, proc procView) {
	hashDirs, err := os.ReadDir(sessionsRoot)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) { // absent before kiro-cli's first session
			slog.Warn("kiro session locks: the session store is unreadable; stale locks were not cleared",
				"dir", sessionsRoot, "error", err)
		}
		return
	}
	t := lockSweepTally{proc: proc}
	for _, hd := range hashDirs {
		dir := filepath.Join(sessionsRoot, hd.Name())
		switch {
		case hd.IsDir() && hd.Name() == kiroV2SessionsDir:
			t.sweepV2Dir(ctx, dir)
		case hd.IsDir():
			t.sweepHashDir(ctx, dir)
		}
		if ctx.Err() != nil {
			return
		}
	}
	if t.cleared > 0 {
		slog.Info("kiro session locks: cleared the locks of processes that no longer run", "cleared", t.cleared)
	}
}

// kiroV2SessionsDir holds kiro-cli's V2-engine sessions as flat <id>.json, <id>.jsonl and
// <id>.lock files, beside the V3 hash dirs.
const kiroV2SessionsDir = "cli"

type lockSweepTally struct {
	proc    procView
	cleared int
}

// sweepHashDir clears the stale lock of every session directory in one workspace hash dir.
func (t *lockSweepTally) sweepHashDir(ctx context.Context, hashDir string) {
	sessions, err := os.ReadDir(hashDir)
	if err != nil {
		warnLocksUnchecked(hashDir, err)
		return
	}
	for _, sd := range sessions {
		if !sd.IsDir() || !validKiroResumeID(sd.Name()) {
			continue
		}
		t.clear(ctx, filepath.Join(hashDir, sd.Name(), ".lock"))
	}
}

// sweepV2Dir clears every stale <session id>.lock file in the V2 sessions dir. A directory
// named *.lock is another lock kind (a held mkdir lock), never a session's.
func (t *lockSweepTally) sweepV2Dir(ctx context.Context, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		warnLocksUnchecked(dir, err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".lock") {
			t.clear(ctx, filepath.Join(dir, e.Name()))
		}
	}
}

func (t *lockSweepTally) clear(ctx context.Context, path string) {
	switch removed, err := clearStaleKiroLock(ctx, path, t.proc); {
	case err != nil:
		slog.Warn("kiro session locks: kept a lock that could not be judged or removed; its conversation may refuse to open",
			"path", path, "error", err)
	case removed:
		t.cleared++
	}
}

func warnLocksUnchecked(dir string, err error) {
	slog.Warn("kiro session locks: a session dir is unreadable; its locks were not checked", "dir", dir, "error", err)
}

// clearStaleKiroLock removes the kiro-cli session lock at path when proc.lockHeld judges
// its process gone, reporting whether it removed one. A missing lock is (false, nil). A
// lock it cannot judge (unreadable, a symlink or non-regular file, oversize, or a body or
// process lockHeld refuses) is kept and its reason returned, as kiro-cli itself never
// deletes a lock it cannot parse.
func clearStaleKiroLock(ctx context.Context, path string, proc procView) (bool, error) {
	raw, err := readSmallFile(ctx, path, maxKiroLockBytes)
	if errors.Is(err, fs.ErrNotExist) || ctx.Err() != nil {
		return false, nil
	}
	held := false
	if err == nil {
		held, err = proc.lockHeld(raw)
	}
	switch {
	case err != nil:
		return false, err
	case held:
		return false, nil
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
