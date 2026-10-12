package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/cplieger/atomicfile/v4"
	"github.com/cplieger/web-terminal-engine/v6/terminal"
)

// tabsFileName is the saved tab set under /config. It holds no tab id: a tab id is the
// /ws attach capability, while the kiro-cli session id it carries grants nothing.
const tabsFileName = "tabs.json"

// tabsStaleTempAge is how old an atomicfile temp beside tabs.json must be before load
// reclaims it: far beyond the one small write this store ever has in flight.
const tabsStaleTempAge = time.Hour

// sessionSetCapturer is what persist needs from the session manager.
type sessionSetCapturer interface {
	CaptureSessionSet(func(terminal.SessionInfo) string) terminal.SessionSet
}

var _ sessionSetCapturer = (*terminal.SessionManager)(nil)

// tabStore saves the tab set to tabs.json and reads it back once at boot. Every method is
// safe on a nil store, which is the "no /config" verdict.
type tabStore struct {
	dir, path string
	// last is the bytes last written or read, so a pass writes only on change. Touched by
	// load before the pollers start, then by the poller goroutine only.
	last []byte
	// armed stays false until the restore finished: a pass before that would replace the
	// stored set with a partial one.
	armed atomic.Bool
	// keepFile is set by load, before any arm, when the file could be neither read nor
	// moved aside; overwriting it would lose it, so arm is then a no-op for this run.
	keepFile bool
	// failing is whether the last write failed. Poller goroutine only.
	failing bool
}

// newTabStore returns the store for configDir, or nil when configDir is not a directory.
func newTabStore(configDir string) *tabStore {
	fi, err := os.Stat(configDir)
	if err != nil || !fi.IsDir() {
		return nil
	}
	return &tabStore{dir: configDir, path: filepath.Join(configDir, tabsFileName)}
}

// load reads the saved set. ok is false when there is nothing to restore: no file, a
// cancelled ctx, or a file that is unreadable or invalid, which is moved to
// tabs.json.unreadable so the next save starts fresh and the bytes stay recoverable.
func (s *tabStore) load(ctx context.Context) (terminal.SessionSet, bool) {
	if s == nil {
		return terminal.SessionSet{}, false
	}
	// A SIGKILL between an atomicfile create and its rename leaves a temp here, and the
	// next write picks a new name, so without the sweep they accumulate.
	swept, err := atomicfile.CleanupStaleTemps(ctx, s.dir, tabsStaleTempAge, atomicfile.WithRecursive(false))
	if (err != nil || swept.Failed > 0) && ctx.Err() == nil {
		slog.Warn("tabs.json: stale temp sweep incomplete", "dir", s.dir, "failed", swept.Failed, "error", err)
	}
	data, err := readSmallFile(ctx, s.path, terminal.MaxSessionSetBytes)
	if errors.Is(err, fs.ErrNotExist) || ctx.Err() != nil {
		return terminal.SessionSet{}, false
	}
	set := terminal.SessionSet{}
	if err == nil {
		set, err = terminal.ParseSessionSet(data)
	}
	if err != nil {
		s.moveAside(err)
		return terminal.SessionSet{}, false
	}
	if s.last, err = json.Marshal(set); err != nil {
		s.last = nil
	}
	return set, true
}

// moveAside renames an unreadable tabs.json (the link itself, for a symlink) so a save
// cannot overwrite it; when even that fails the store refuses to arm.
func (s *tabStore) moveAside(readErr error) {
	aside := s.path + ".unreadable"
	if err := os.Rename(s.path, aside); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.keepFile = true
		slog.Error("tabs.json unreadable and could not be moved aside; tabs are not saved this run",
			"path", s.path, "error", readErr, "rename_error", err)
		return
	}
	slog.Warn("tabs.json unreadable; no tabs restored",
		"path", s.path, "error", readErr,
		"hint", "the file was moved to "+aside+" and the next save writes a fresh "+tabsFileName)
}

// arm lets persist write, unless load found a file it could not move aside.
func (s *tabStore) arm() {
	if s == nil || s.keepFile {
		return
	}
	s.armed.Store(true)
}

// persist saves the live set when it changed. mapped is the poller's tab -> kiro session
// pairing; a tab missing from it falls back to its alias when that is a kiro session id,
// which covers a restored tab before its first mapped pass.
func (s *tabStore) persist(ctx context.Context, mgr sessionSetCapturer, mapped map[terminal.SessionID]string) {
	if s == nil || !s.armed.Load() {
		return
	}
	set := mgr.CaptureSessionSet(func(info terminal.SessionInfo) string {
		if id, ok := mapped[info.ID]; ok {
			return id
		}
		if validKiroResumeID(info.Alias) {
			return info.Alias
		}
		return ""
	})
	// The shutdown guarantee: cancelBase runs before mgr.Shutdown empties the set, so a
	// capture that saw the emptied set sees a cancelled ctx here and never saves it.
	if ctx.Err() != nil {
		return
	}
	data, err := json.Marshal(set)
	if err == nil && bytes.Equal(data, s.last) {
		return
	}
	if err == nil {
		_, err = atomicfile.WriteFile(ctx, s.path, data,
			atomicfile.WithMode(0o600), atomicfile.WithMaxBytes(terminal.MaxSessionSetBytes))
	}
	s.recordWrite(ctx, data, err)
}

// recordWrite logs a failed save once per failing run (Debug after that) and leaves last
// alone so the next pass retries; a save that lands after failures says so.
func (s *tabStore) recordWrite(ctx context.Context, data []byte, err error) {
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		level := slog.LevelWarn
		if s.failing {
			level = slog.LevelDebug
		}
		s.failing = true
		slog.Log(ctx, level, "tabs.json write failed; the last saved tabs stay on disk", "path", s.path, "error", err)
		return
	}
	s.last = data
	if s.failing {
		s.failing = false
		slog.Info("tabs.json written again", "path", s.path)
	}
}
