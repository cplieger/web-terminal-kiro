package main

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// retractFormerKiroLink removes a manager-shaped symlink at toolbelt's
// bin/kiro-cli, which a volume can hold and nothing else retracts; it dangles
// once retention prunes its version. Only a link with the manager's shape (a
// kiro-cli directly inside a version directory of this install root) is
// removed; toolbelt's own link at that name points into opt/ and is kept.
func retractFormerKiroLink(toolsDir string) {
	root, err := os.OpenRoot(toolsDir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("failed to open the tools dir to retract the former kiro-cli link", "dir", toolsDir, "error", err)
		}
		return
	}
	defer root.Close()
	link := filepath.Join(toolbeltBinDir, kiroName)
	fi, err := root.Lstat(link)
	if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		return
	}
	text, err := root.Readlink(link)
	if err != nil || !isFormerKiroLink(toolsDir, text) {
		return
	}
	if err := root.Remove(link); err != nil {
		slog.Warn("failed to retract the former kiro-cli link from toolbelt's bin dir; it dangles once its version is pruned",
			"path", filepath.Join(toolsDir, link), "error", err)
		return
	}
	slog.Info("retracted the former kiro-cli link from toolbelt's bin dir; the pinned kiro-cli resolves through its own link dir",
		"path", filepath.Join(toolsDir, link), "link_dir", filepath.Join(toolsDir, kiroLinkDir))
}

// isFormerKiroLink reports whether a symlink text has the shape the install
// manager publishes, judged on the cleaned text with no ".." element (which
// resolves through a symlinked directory to somewhere other than its lexical
// meaning).
func isFormerKiroLink(toolsDir, text string) bool {
	if slices.Contains(strings.Split(text, string(filepath.Separator)), "..") {
		return false
	}
	clean := filepath.Clean(text)
	return filepath.Base(clean) == kiroName &&
		filepath.Dir(filepath.Dir(clean)) == filepath.Join(toolsDir, kiroName+"-versions")
}
