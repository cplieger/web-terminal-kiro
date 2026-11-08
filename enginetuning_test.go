package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/go/slogx/capture"
)

// TestMain lets a test run this package's test binary as the launcher, the way
// the image's kas-node link runs the server binary.
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == kasNodeLauncherName {
		err := execKasNode(os.Getenv(kasRealNodeKey), os.Args[1:])
		fmt.Fprintln(os.Stderr, kasNodeLauncherName+":", err)
		os.Exit(127)
	}
	os.Exit(m.Run())
}

const validatedNode = "22.23.4"

// kiroInstall is a fake kiro-cli data dir and active kiro-cli.
type kiroInstall struct {
	dataDir string
	engine  string
	node    string
}

func newKiroInstall(t *testing.T) *kiroInstall {
	t.Helper()
	dir := t.TempDir()
	in := &kiroInstall{dataDir: filepath.Join(dir, "share", "kiro-cli"), engine: filepath.Join(dir, "kiro-cli")}
	in.node = filepath.Join(in.dataDir, "node")
	if err := os.MkdirAll(filepath.Dir(in.node), 0o755); err != nil {
		t.Fatal(err)
	}
	return in
}

// unpack writes a node answering --version with version, its node.sha256, and,
// when embedded, a kiro-cli carrying that hash the way the real one does.
// The node is renamed into place so a replacement is a new file, as kiro-cli's
// own unpack makes it.
func (in *kiroInstall) unpack(t *testing.T, version string, embedded bool) {
	t.Helper()
	body := []byte("#!/bin/sh\nprintf '%s\\n' '" + version + "'\n")
	sum := sha256.Sum256(body)
	hexSum := hex.EncodeToString(sum[:])
	tmp := in.node + ".new"
	writeExecutable(t, tmp, body)
	if err := os.Rename(tmp, in.node); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(in.node+".sha256", []byte(hexSum), 0o644); err != nil {
		t.Fatal(err)
	}
	engine := []byte("\x7fELF..." + strings.Repeat("x", 4096))
	if embedded {
		engine = append(engine, hexSum+"KIRO_KAS_NODE_PATHKIRO_KAS_SERVER_PATH"...)
	}
	if err := os.WriteFile(in.engine, engine, 0o755); err != nil {
		t.Fatal(err)
	}
}

func (in *kiroInstall) tuning() *engineTuning {
	return newEngineTuning(true, validatedNode, in.dataDir, func() string { return in.engine })
}

// noPresetKasNode clears an inherited KIRO_KAS_NODE_PATH and points the launcher
// at a file that exists. Serial: it sets a package var.
func noPresetKasNode(t *testing.T) {
	t.Helper()
	t.Setenv(kasNodePathKey, "")
	old := kasNodeLauncher
	kasNodeLauncher = filepath.Join(t.TempDir(), kasNodeLauncherName)
	if err := os.WriteFile(kasNodeLauncher, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { kasNodeLauncher = old })
}

func TestEngineTuning_offAddsNothingAndLogsNothing(t *testing.T) {
	records := capture.Default(t)
	tuning := newEngineTuning(false, validatedNode, t.TempDir(), func() string { return "/bin/sh" })
	if tuning != nil {
		t.Fatalf("newEngineTuning(off) = %+v, want nil", tuning)
	}
	if got := tuning.env(); got != nil {
		t.Errorf("env() with the setting off = %q, want nothing", got)
	}
	if records.Len() != 0 {
		t.Errorf("log = %q, want nothing", records.Messages())
	}
}

func TestEngineTuning_validatedNodeStartsTheEngineThroughTheLauncher(t *testing.T) {
	noPresetKasNode(t)
	records := capture.Default(t)
	in := newKiroInstall(t)
	in.unpack(t, "v"+validatedNode, true)
	tuning := in.tuning()

	want := []string{kasNodePathKey + "=" + kasNodeLauncher, kasRealNodeKey + "=" + in.node}
	for tab := range 2 {
		if got := tuning.env(); !slices.Equal(got, want) {
			t.Errorf("tab %d env() = %q, want %q", tab, got, want)
		}
	}
	if n := records.CountLevel(slog.LevelWarn, ""); n != 0 {
		t.Errorf("log = %q, want no Warn", records.Messages())
	}
}

func TestEngineTuning_eachGuardStartsStockAndWarnsOnce(t *testing.T) {
	cases := []struct {
		name       string
		arrange    func(t *testing.T, in *kiroInstall)
		wantReason string
	}{
		{
			name:       "another Node.js version",
			arrange:    func(t *testing.T, in *kiroInstall) { in.unpack(t, "v24.11.0", true) },
			wantReason: "",
		},
		{
			name: "a node that does not match node.sha256",
			arrange: func(t *testing.T, in *kiroInstall) {
				in.unpack(t, "v"+validatedNode, true)
				writeExecutable(t, in.node, []byte("#!/bin/sh\necho v"+validatedNode+"\n# altered\n"))
			},
			wantReason: "does not match its node.sha256",
		},
		{
			name:       "a node the active kiro-cli does not embed",
			arrange:    func(t *testing.T, in *kiroInstall) { in.unpack(t, "v"+validatedNode, false) },
			wantReason: "not the one the active kiro-cli embeds",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			noPresetKasNode(t)
			records := capture.Default(t)
			in := newKiroInstall(t)
			tc.arrange(t, in)
			tuning := in.tuning()
			for range 3 {
				if got := tuning.env(); got != nil {
					t.Errorf("env() = %q, want nothing", got)
				}
			}
			if n := records.CountLevel(slog.LevelWarn, nodeNotValidatedMsg); n != 1 {
				t.Errorf("log = %q, want one Warn across three tabs, got %d", records.Messages(), n)
			}
			if tc.wantReason != "" && !logContains(records, tc.wantReason) {
				t.Errorf("log = %q, want the reason %q", records.Messages(), tc.wantReason)
			}
		})
	}
}

func TestEngineTuning_versionWarnNamesBothVersions(t *testing.T) {
	noPresetKasNode(t)
	records := capture.Default(t)
	in := newKiroInstall(t)
	in.unpack(t, "v24.11.0", true)
	in.tuning().env()
	if !records.HasAttr(nodeNotValidatedMsg, "node_version", "24.11.0") || !records.HasAttr(nodeNotValidatedMsg, "validated_version", validatedNode) {
		t.Errorf("log = %q, want the Warn to name 24.11.0 and %s", records.Messages(), validatedNode)
	}
}

func TestEngineTuning_followsTheFilesKiroCLIUnpacks(t *testing.T) {
	noPresetKasNode(t)
	records := capture.Default(t)
	in := newKiroInstall(t)
	tuning := in.tuning()

	if got := tuning.env(); got != nil {
		t.Errorf("env() before kiro-cli unpacked its Node.js = %q, want nothing so this tab unpacks it", got)
	}
	if n := records.CountLevel(slog.LevelWarn, ""); n != 0 {
		t.Errorf("log = %q, want no Warn for a first boot", records.Messages())
	}
	in.unpack(t, "v"+validatedNode, true)
	if got := tuning.env(); len(got) == 0 {
		t.Error("env() after the unpack = nothing, want the overlay")
	}
	in.unpack(t, "v22.24.0", true)
	if got := tuning.env(); got != nil {
		t.Errorf("env() after kiro-cli unpacked another Node.js = %q, want nothing", got)
	}
	if !records.HasAttr(nodeNotValidatedMsg, "node_version", "22.24.0") {
		t.Errorf("log = %q, want the new file's version in a Warn", records.Messages())
	}
}

func TestEngineTuning_noActiveKiroCLIStartsStock(t *testing.T) {
	noPresetKasNode(t)
	capture.Default(t)
	in := newKiroInstall(t)
	in.unpack(t, "v"+validatedNode, true)
	tuning := newEngineTuning(true, validatedNode, in.dataDir, func() string { return "" })
	if got := tuning.env(); got != nil {
		t.Errorf("env() with no active kiro-cli = %q, want nothing", got)
	}
}

func TestNewEngineTuning_refusesWhatItCannotApply(t *testing.T) {
	engine := func() string { return "/bin/sh" }
	t.Run("an operator's own KIRO_KAS_NODE_PATH", func(t *testing.T) {
		t.Setenv(kasNodePathKey, "/usr/local/bin/node")
		records := capture.Default(t)
		if got := newEngineTuning(true, validatedNode, t.TempDir(), engine); got != nil {
			t.Errorf("newEngineTuning = %+v, want nil so the operator's override stays", got)
		}
		if records.CountLevel(slog.LevelWarn, "") != 1 {
			t.Errorf("log = %q, want one Warn", records.Messages())
		}
	})
	t.Run("no validated version exported", func(t *testing.T) {
		noPresetKasNode(t)
		records := capture.Default(t)
		if got := newEngineTuning(true, " ", t.TempDir(), engine); got != nil {
			t.Errorf("newEngineTuning = %+v, want nil", got)
		}
		if records.CountLevel(slog.LevelWarn, "") != 1 {
			t.Errorf("log = %q, want one Warn", records.Messages())
		}
	})
	t.Run("no launcher outside the image", func(t *testing.T) {
		noPresetKasNode(t)
		kasNodeLauncher = filepath.Join(t.TempDir(), "absent")
		records := capture.Default(t)
		if got := newEngineTuning(true, validatedNode, t.TempDir(), engine); got != nil {
			t.Errorf("newEngineTuning = %+v, want nil: a KIRO_KAS_NODE_PATH naming nothing stops the agent engine", got)
		}
		if records.CountLevel(slog.LevelWarn, "") != 1 {
			t.Errorf("log = %q, want one Warn", records.Messages())
		}
	})
	t.Run("no data home", func(t *testing.T) {
		noPresetKasNode(t)
		records := capture.Default(t)
		if got := newEngineTuning(true, validatedNode, "", engine); got != nil {
			t.Errorf("newEngineTuning = %+v, want nil", got)
		}
		if records.CountLevel(slog.LevelWarn, "") != 1 {
			t.Errorf("log = %q, want one Warn", records.Messages())
		}
	})
	t.Run("a leading v on the validated version", func(t *testing.T) {
		noPresetKasNode(t)
		capture.Default(t)
		in := newKiroInstall(t)
		in.unpack(t, "v"+validatedNode, true)
		got := newEngineTuning(true, "v"+validatedNode, in.dataDir, func() string { return in.engine }).env()
		if len(got) == 0 {
			t.Error("env() = nothing, want the overlay: v22.23.4 and 22.23.4 name one version")
		}
	})
}

func TestParseHighThroughput(t *testing.T) {
	cases := []struct {
		raw       string
		want      bool
		wantWarns int
	}{
		{raw: "", want: false},
		{raw: "1", want: true},
		{raw: "ON", want: true},
		{raw: "false", want: false},
		{raw: "fast", want: false, wantWarns: 1},
	}
	for _, tc := range cases {
		t.Run("value "+tc.raw, func(t *testing.T) {
			t.Setenv(highThroughputKey, tc.raw)
			records := capture.Default(t)
			if got := parseHighThroughput(); got != tc.want {
				t.Errorf("parseHighThroughput(%q) = %v, want %v", tc.raw, got, tc.want)
			}
			if n := records.CountLevel(slog.LevelWarn, ""); n != tc.wantWarns {
				t.Errorf("log = %q, want %d Warn", records.Messages(), tc.wantWarns)
			}
		})
	}
}

func TestFileContains_findsANeedleAcrossReads(t *testing.T) {
	needle := []byte("94efd062ed066c1a0b332e18249859c905e920eeada5df55d24c99b919629a90")
	for _, at := range []int{0, 1<<20 - 10, 1 << 20, 3<<20 + 7} {
		t.Run(fmt.Sprintf("at %d", at), func(t *testing.T) {
			data := bytes.Repeat([]byte{'x'}, at+len(needle)+100)
			copy(data[at:], needle)
			path := filepath.Join(t.TempDir(), "bin")
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			if ok, err := fileContains(path, needle); err != nil || !ok {
				t.Errorf("fileContains = %v, %v; want true", ok, err)
			}
			copy(data[at:], bytes.Repeat([]byte{'y'}, len(needle)))
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			if ok, err := fileContains(path, needle); err != nil || ok {
				t.Errorf("fileContains without the needle = %v, %v; want false", ok, err)
			}
		})
	}
}

// launcherCmd runs this test binary as the kas-node launcher.
func launcherCmd(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), self, args...)
	cmd.Args[0] = filepath.Join(t.TempDir(), kasNodeLauncherName)
	return cmd
}

// TestKasNodeLauncher_flagsReachTheEngineArgvButNoAgentCommand runs the launcher
// over a fake node that, like the agent engine, runs a command of its own.
func TestKasNodeLauncher_flagsReachTheEngineArgvButNoAgentCommand(t *testing.T) {
	node := filepath.Join(t.TempDir(), "node")
	writeExecutable(t, node, []byte("#!/bin/sh\necho \"pid=$$\"\nfor a; do printf 'argv=%s\\n' \"$a\"; done\nprintf 'engine NODE_OPTIONS=%s\\n' \"$NODE_OPTIONS\"\nsh -c 'env' | sed 's/^/agent /'\n"))

	const inherited = "--max-old-space-size=4096 --max-semi-space-size=64"
	cmd := launcherCmd(t, "--experimental-wasm-modules", "acp-server.js", "--transport=stdio")
	cmd.Env = append(os.Environ(), "NODE_OPTIONS="+inherited, kasRealNodeKey+"="+node)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("launcher: %v", err)
	}
	var argv, agent []string
	var pid, engineOpts string
	for line := range strings.Lines(string(out)) {
		line = strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(line, "pid="):
			pid = strings.TrimPrefix(line, "pid=")
		case strings.HasPrefix(line, "argv="):
			argv = append(argv, strings.TrimPrefix(line, "argv="))
		case strings.HasPrefix(line, "engine NODE_OPTIONS="):
			engineOpts = strings.TrimPrefix(line, "engine NODE_OPTIONS=")
		case strings.HasPrefix(line, "agent "):
			agent = append(agent, strings.TrimPrefix(line, "agent "))
		}
	}
	if want := fmt.Sprint(cmd.Process.Pid); pid != want {
		t.Errorf("node ran as pid %s, want %s: the launcher must exec it so kiro-cli's signals reach node", pid, want)
	}
	wantArgv := []string{"--max-semi-space-size=64", "--experimental-async-context-frame", "--experimental-wasm-modules", "acp-server.js", "--transport=stdio"}
	if !slices.Equal(argv, wantArgv) {
		t.Errorf("engine argv = %q, want %q", argv, wantArgv)
	}
	if engineOpts != inherited {
		t.Errorf("engine NODE_OPTIONS = %q, want the inherited %q untouched", engineOpts, inherited)
	}
	if len(agent) == 0 {
		t.Fatal("the fake engine's command printed no environment")
	}
	for _, kv := range agent {
		if strings.Contains(kv, "--experimental-async-context-frame") {
			t.Errorf("an agent command inherited %q; Node 26 and later refuse to start with that flag in NODE_OPTIONS", kv)
		}
	}
}

func TestKasNodeLauncher_refusesWithoutTheNodeToRun(t *testing.T) {
	cmd := launcherCmd(t, "--version")
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, kasRealNodeKey+"=") })
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("launcher with no %s exited 0 (%q), want a refusal naming it", kasRealNodeKey, out)
	}
	if !strings.Contains(string(out), kasRealNodeKey) {
		t.Errorf("refusal = %q, want it to name %s", out, kasRealNodeKey)
	}
}
