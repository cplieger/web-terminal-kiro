package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cplieger/go/envx"
)

const (
	highThroughputKey = "KIRO_CLI_HIGH_THROUGHPUT"
	// nodeValidatedKey carries the Node.js version the flags were measured on,
	// exported by entrypoint.sh beside the kiro-cli pin.
	nodeValidatedKey = "KIRO_CLI_NODE_VALIDATED"
	// kasNodePathKey is kiro-cli's own override for the node its TUI starts the
	// agent engine with (tui.js reads process.env.KIRO_KAS_NODE_PATH || "node").
	kasNodePathKey = "KIRO_KAS_NODE_PATH"
	// kasRealNodeKey names the node the launcher execs: the file the server checked.
	kasRealNodeKey = "WT_KAS_NODE"
	// kasNodeLauncherName is the argv[0] base name that makes this binary the
	// launcher instead of the server; the image links kasNodeLauncher to it.
	kasNodeLauncherName = "kas-node"
	// nodeVersionTimeout bounds one `node --version` probe.
	nodeVersionTimeout = 5 * time.Second
)

// kasNodeLauncher is the image's link to this binary; a var so tests can point it
// at a file that exists outside the image.
var kasNodeLauncher = "/opt/web-terminal-kiro/engine/" + kasNodeLauncherName

// kasNodeFlags ride the engine's argv, never NODE_OPTIONS: every command the agent
// runs inherits the engine's environment, and Node 26 and later refuse to start
// with --experimental-async-context-frame in NODE_OPTIONS. On the command line
// they also win over an inherited NODE_OPTIONS spelling of the same flag.
var kasNodeFlags = []string{"--max-semi-space-size=64", "--experimental-async-context-frame"}

// nodeNotValidatedMsg is the one Warn logged per reason a tab starts without the flags.
const nodeNotValidatedMsg = "kiro-cli's Node.js is not the one the high-throughput flags were validated on; tabs start without them"

// execKasNode replaces the process with node, kasNodeFlags and args. It returns
// only when the exec failed.
func execKasNode(node string, args []string) error {
	if node == "" {
		return errors.New(kasRealNodeKey + " is unset, so there is no node to run")
	}
	argv := append(append([]string{node}, kasNodeFlags...), args...)
	// node is WT_KAS_NODE, which only the server sets, to the file it verified.
	return syscall.Exec(node, argv, os.Environ()) //nolint:gosec // G702: the exec target is the launcher's whole job
}

// engineTuning decides, per tab, whether kiro-cli's agent engine starts through
// the launcher. A nil *engineTuning is the setting turned off.
type engineTuning struct {
	// engineBinary returns the active kiro-cli, the binary that embeds the agent
	// engine and its Node.js, or "" while none is installed.
	engineBinary func() string
	reported     map[string]bool
	key          nodeCheckKey
	node         string
	validated    string
	launcher     string
	verdict      string
	mu           sync.Mutex
}

// nodeCheckKey identifies the files one verdict was computed from.
type nodeCheckKey struct {
	node, sum, engine os.FileInfo
	enginePath        string
}

func (k nodeCheckKey) same(o nodeCheckKey) bool {
	return k.enginePath == o.enginePath && sameFile(k.node, o.node) && sameFile(k.sum, o.sum) && sameFile(k.engine, o.engine)
}

func sameFile(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// parseHighThroughput reads KIRO_CLI_HIGH_THROUGHPUT; unset or malformed is off.
func parseHighThroughput() bool {
	on, _, err := envx.BoolStrict(highThroughputKey)
	if err != nil {
		slog.Warn("unparseable "+highThroughputKey+"; tabs start kiro-cli's agent engine without the high-throughput flags",
			"hint", "use true, 1, yes or on to turn it on, or false, 0, no or off, in any letter case")
		return false
	}
	return on
}

// newEngineTuning returns nil when the setting is off or cannot apply, logging why
// in the second case. dataHome is kiro-cli's data home (kiroDataHome).
func newEngineTuning(on bool, validated, dataHome string, engineBinary func() string) *engineTuning {
	if !on {
		return nil
	}
	validated = strings.TrimPrefix(strings.TrimSpace(validated), "v")
	switch {
	case validated == "":
		slog.Warn(highThroughputKey+" is on but no validated Node.js version is exported; tabs start without the high-throughput flags",
			"hint", "entrypoint.sh exports "+nodeValidatedKey+" in the image; outside it, export the version the flags were measured on")
		return nil
	case os.Getenv(kasNodePathKey) != "":
		slog.Warn(highThroughputKey+" is on but "+kasNodePathKey+" is already set in the container; leaving it alone, so tabs start without the high-throughput flags",
			"hint", "unset "+kasNodePathKey+" to let "+highThroughputKey+" choose kiro-cli's agent engine node")
		return nil
	case dataHome == "":
		slog.Warn(highThroughputKey + " is on but neither XDG_DATA_HOME nor HOME is set, so kiro-cli's Node.js cannot be found; tabs start without the high-throughput flags")
		return nil
	}
	// A KIRO_KAS_NODE_PATH naming nothing would stop every tab's agent engine.
	if _, err := os.Stat(kasNodeLauncher); err != nil {
		slog.Warn(highThroughputKey+" is on but the agent-engine launcher is missing; tabs start without the high-throughput flags",
			"path", kasNodeLauncher, "error", err,
			"hint", "the image links it to the server binary; outside the image, link "+kasNodeLauncher+" to this binary")
		return nil
	}
	slog.Info("high-throughput flags on for kiro-cli's agent engine", "validated_node", validated)
	return &engineTuning{
		engineBinary: engineBinary,
		node:         filepath.Join(dataHome, "kiro-cli", "node"),
		validated:    validated,
		launcher:     kasNodeLauncher,
		reported:     map[string]bool{},
	}
}

// kiroDataHome mirrors entrypoint.sh's kiro_cli_data_dir parent.
func kiroDataHome() string {
	if d := envx.String("XDG_DATA_HOME"); d != "" {
		return d
	}
	if h := envx.String("HOME"); h != "" {
		return filepath.Join(h, ".local", "share")
	}
	return ""
}

// env is one tab's overlay: the launcher as kiro-cli's agent-engine node when
// kiro-cli's unpacked Node.js is intact, is the one the active kiro-cli
// embeds, and is the validated version; else nil, so the tab starts as stock
// kiro-cli. kiro-cli skips its Node.js unpack while KIRO_KAS_NODE_PATH is set, so
// a stock start is also what repairs a missing or stale file for the next tab.
func (t *engineTuning) env() []string {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key, why := t.stat()
	if why != "" {
		t.report(why)
		return nil
	}
	if !key.same(t.key) {
		t.key, t.verdict = key, t.check(key.enginePath)
	}
	if t.verdict != "" {
		t.report(t.verdict)
		return nil
	}
	return []string{kasNodePathKey + "=" + t.launcher, kasRealNodeKey + "=" + t.node}
}

// stat reads the identity of the three files a verdict depends on.
func (t *engineTuning) stat() (key nodeCheckKey, why string) {
	engine := ""
	if t.engineBinary != nil {
		engine = t.engineBinary()
	}
	if engine == "" {
		return nodeCheckKey{}, "no active kiro-cli to check its Node.js against"
	}
	var err error
	key.enginePath = engine
	if key.node, err = os.Stat(t.node); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return key, "missing"
		}
		return key, fmt.Sprintf("cannot read %s: %v", t.node, err)
	}
	if key.sum, err = os.Stat(t.node + ".sha256"); err != nil {
		return key, fmt.Sprintf("cannot read %s.sha256: %v", t.node, err)
	}
	if key.engine, err = os.Stat(engine); err != nil {
		return key, fmt.Sprintf("cannot read %s: %v", engine, err)
	}
	return key, ""
}

// check returns "" when the flags apply, else why not.
func (t *engineTuning) check(engine string) string {
	raw, err := os.ReadFile(t.node + ".sha256")
	if err != nil {
		return fmt.Sprintf("cannot read %s.sha256: %v", t.node, err)
	}
	want := strings.ToLower(strings.TrimSpace(string(raw)))
	got, err := fileSHA256(t.node)
	if err != nil {
		return fmt.Sprintf("cannot hash %s: %v", t.node, err)
	}
	if got != want {
		return "the unpacked Node.js does not match its node.sha256 (a partial or altered unpack)"
	}
	embedded, err := fileContains(engine, []byte(want))
	if err != nil {
		return fmt.Sprintf("cannot read %s: %v", engine, err)
	}
	if !embedded {
		return "the unpacked Node.js is not the one the active kiro-cli embeds (left by another kiro-cli version)"
	}
	ctx, cancel := context.WithTimeout(context.Background(), nodeVersionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, t.node, "--version").Output() //nolint:gosec // G204: kiro-cli's node, already hash-checked above
	if err != nil {
		return fmt.Sprintf("%s --version failed: %v", t.node, err)
	}
	if version := strings.TrimPrefix(strings.TrimSpace(string(out)), "v"); version != t.validated {
		return "node_version " + version
	}
	return ""
}

// report logs why once per distinct reason. A missing file is a first boot,
// logged at Info; the version case names both versions as attributes.
func (t *engineTuning) report(why string) {
	if t.reported[why] {
		return
	}
	t.reported[why] = true
	if why == "missing" {
		slog.Info("kiro-cli has not unpacked its Node.js yet; this tab starts without the high-throughput flags and unpacks it", "path", t.node)
		return
	}
	if version, ok := strings.CutPrefix(why, "node_version "); ok {
		slog.Warn(nodeNotValidatedMsg, "node_version", version, "validated_version", t.validated,
			"hint", "a kiro-cli bump moved its Node.js: re-measure the flags on it, then update "+nodeValidatedKey+" in entrypoint.sh")
		return
	}
	slog.Warn(nodeNotValidatedMsg, "reason", why, "validated_version", t.validated)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fileContains streams path looking for needle, keeping len(needle)-1 bytes of
// overlap so a match across two reads is found.
func fileContains(path string, needle []byte) (bool, error) {
	if len(needle) == 0 {
		return false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	buf := make([]byte, 0, 1<<20+len(needle))
	chunk := make([]byte, 1<<20)
	for {
		n, err := f.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if bytes.Contains(buf, needle) {
			return true, nil
		}
		if keep := len(needle) - 1; len(buf) > keep {
			buf = append(buf[:0], buf[len(buf)-keep:]...)
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}
