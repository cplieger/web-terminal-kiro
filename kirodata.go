package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cplieger/go/envx"
)

// skipBinaryPinningKey is kiro-cli's own switch, set by the image: with it on,
// kiro-cli copies no chat binary into its run/ dir, so a copy there is left
// from an image that ran with it off.
const skipBinaryPinningKey = "KIRO_SKIP_BINARY_PINNING"

var (
	// versionKeyed matches a kas/ entry kiro-cli names after a version: the
	// runtime tree <version>-<hash> and its sibling <version>-<hash>.lock.
	versionKeyed = regexp.MustCompile(`^\d+\.\d+\.\d+-`)
	pinnedCopy   = regexp.MustCompile(`^(chat-cli-\d.*|\.chat-cli-\d.*\.heartbeat)$`)
)

// kiroDataDir is kiro-cli's data dir, resolved the way kiro-cli resolves it
// (XDG_DATA_HOME, else $HOME/.local/share, then kiro-cli), or "" when neither
// is set.
func kiroDataDir() string {
	if d := envx.String("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "kiro-cli")
	}
	if h := envx.String("HOME"); h != "" {
		return filepath.Join(h, ".local", "share", "kiro-cli")
	}
	return ""
}

// activationPoll is how often the cleanup checks whether the install manager
// has activated a version; the manager offers no activation callback.
const activationPoll = 100 * time.Millisecond

// kiroDataCleanup removes what kiro-cli leaves in its data dir for versions
// that no longer run: each version's ~240 MB agent-server runtime under kas/,
// and the pinned chat binaries under run/ when binary pinning is off.
type kiroDataCleanup struct {
	dir         string
	skipPinning bool
	done        atomic.Bool
}

func newKiroDataCleanup(dir string) *kiroDataCleanup {
	return &kiroDataCleanup{dir: dir, skipPinning: envx.String(skipBinaryPinningKey) == "1"}
}

// watch waits for active to name a version, cleans for it, and marks the
// cleanup settled; it returns early when ctx ends. Only that first version is
// cleaned for: a version a later retry activates leaves the earlier one's
// runtime to the next boot, since a tab may still run on it.
func (c *kiroDataCleanup) watch(ctx context.Context, active func() string) {
	tick := time.NewTicker(activationPoll)
	defer tick.Stop()
	for {
		if v := active(); v != "" {
			c.run(v)
			c.done.Store(true)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// settled reports whether watch has cleaned for the first active version.
func (c *kiroDataCleanup) settled() bool { return c.done.Load() }

func (c *kiroDataCleanup) run(active string) {
	if c.dir == "" {
		return
	}
	// The cleanup deletes as root on a volume another image may have written, so
	// every delete goes through an os.Root that refuses to leave the opened dir,
	// and neither the data dir nor a store may be a symlink.
	parent, err := os.OpenRoot(filepath.Dir(c.dir))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("cannot open kiro-cli's data home; leaving its data dir uncleaned", "dir", c.dir, "error", err)
		}
		return
	}
	defer parent.Close()
	data := openStore(parent, filepath.Base(c.dir), c.dir)
	if data == nil {
		return
	}
	defer data.Close()
	pruneRuntimes(data, filepath.Join(c.dir, "kas"), active)
	if c.skipPinning {
		purgePinnedCopies(data, filepath.Join(c.dir, "run"))
	}
}

// openStore opens name under parent when it is a real directory, or returns nil:
// quietly when it is absent, with a warning when it is a symlink or unreadable.
func openStore(parent *os.Root, name, path string) *os.Root {
	info, err := parent.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		slog.Warn("cannot read a kiro-cli data dir; leaving it uncleaned", "dir", path, "error", err)
		return nil
	case info.Mode()&fs.ModeSymlink != 0:
		slog.Warn("a kiro-cli data dir is a symlink; refusing to clean through it", "dir", path)
		return nil
	case !info.IsDir():
		slog.Warn("a kiro-cli data dir is not a directory; leaving it alone", "dir", path)
		return nil
	}
	r, err := parent.OpenRoot(name)
	if err != nil {
		slog.Warn("cannot open a kiro-cli data dir; leaving it uncleaned", "dir", path, "error", err)
		return nil
	}
	return r
}

// pruneRuntimes removes every version-keyed kas/ entry but active's. An entry
// with no leading version is kiro-cli state this cleanup does not know, and
// stays.
func pruneRuntimes(data *os.Root, path, active string) {
	kas := openStore(data, "kas", path)
	if kas == nil {
		return
	}
	defer kas.Close()
	entries, err := fs.ReadDir(kas.FS(), ".")
	if err != nil {
		slog.Warn("cannot list kiro-cli's agent runtimes; leaving them", "dir", path, "error", err)
		return
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasPrefix(name, active+"-"):
			continue
		case !versionKeyed.MatchString(name):
			slog.Info("leaving an unrecognized entry in kiro-cli's agent runtime store", "entry", name)
			continue
		}
		if err := kas.RemoveAll(name); err != nil {
			slog.Warn("failed to prune a superseded kiro-cli agent runtime", "entry", name, "error", err)
			continue
		}
		slog.Info("pruned a superseded kiro-cli agent runtime", "entry", name, "keep", active)
	}
}

// purgePinnedCopies removes the regular files named like kiro-cli's pinned chat
// binary copies and their heartbeats from run/; a directory or symlink of that
// name is not a copy and stays.
func purgePinnedCopies(data *os.Root, path string) {
	run := openStore(data, "run", path)
	if run == nil {
		return
	}
	defer run.Close()
	entries, err := fs.ReadDir(run.FS(), ".")
	if err != nil {
		slog.Warn("cannot list kiro-cli's run dir; leaving its pinned binaries", "dir", path, "error", err)
		return
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !pinnedCopy.MatchString(e.Name()) {
			continue
		}
		if err := run.Remove(e.Name()); err != nil {
			slog.Warn("failed to remove an unused kiro-cli pinned binary", "entry", e.Name(), "error", err)
			continue
		}
		slog.Info("removed an unused kiro-cli pinned binary", "entry", e.Name())
	}
}
