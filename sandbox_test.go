package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cplieger/slogx/capture"
	"github.com/cplieger/web-terminal-engine/v6/terminal"
)

type fakeSandboxResult struct {
	err error
	out string
}

// fakeSandboxRun answers each call with the next result and records the argv
// and whether the call carried a deadline.
type fakeSandboxRun struct {
	calls       [][]string
	hadDeadline []bool
	results     []fakeSandboxResult
}

func (f *fakeSandboxRun) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	_, ok := ctx.Deadline()
	f.hadDeadline = append(f.hadDeadline, ok)
	r := f.results[len(f.calls)-1]
	return []byte(r.out), r.err
}

func fakeShim(t *testing.T) string {
	t.Helper()
	shim := filepath.Join(t.TempDir(), "bwrap")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return shim
}

var errBwrap = errors.New("exit status 1")

func TestSandboxProbe_check(t *testing.T) {
	shim := fakeShim(t)
	probeArgv := []string{shim, "--ro-bind", "/", "/", "--proc", "/proc", "--", "/bin/true"}
	shapeArgv := func(proc ...string) []string {
		argv := []string{
			shim, "--ro-bind", "/", "/", "--tmpfs", "/tmp", "--bind", "/ws", "/ws",
			"--bind", "/home/.kiro", "/home/.kiro", "--dev", "/dev",
		}
		argv = append(argv, proc...)
		return append(argv, "--unshare-pid", "--die-with-parent", "--new-session",
			"--tmpfs", "/run", "--clearenv", "--unshare-net", "--", "/bin/true")
	}
	for _, tc := range []struct {
		name       string
		results    []fakeSandboxResult
		wantState  string
		wantProc   string
		wantOutput string
		wantShape  []string
	}{
		{
			name:      "probe and shape pass with a fresh proc",
			results:   []fakeSandboxResult{{}, {}},
			wantState: sandboxAvailable, wantProc: "proc",
			wantShape: shapeArgv("--proc", "/proc"),
		},
		{
			name:      "a failed probe falls back to an empty proc like kiro-cli",
			results:   []fakeSandboxResult{{err: errBwrap, out: "bwrap: Can't mount proc"}, {}},
			wantState: sandboxAvailable, wantProc: "tmpfs",
			wantShape: shapeArgv("--tmpfs", "/proc"),
		},
		{
			name:      "a passing probe does not make a failing shape available",
			results:   []fakeSandboxResult{{}, {err: errBwrap, out: "bwrap: Failed to make / slave: Permission denied\n"}},
			wantState: sandboxUnavailable, wantOutput: "bwrap: Failed to make / slave: Permission denied",
			wantShape: shapeArgv("--proc", "/proc"),
		},
		{
			name: "both failing is unavailable with the shape's stderr",
			results: []fakeSandboxResult{
				{err: errBwrap, out: "probe text"},
				{err: errBwrap, out: "bwrap: Creating new namespace failed: Operation not permitted"},
			},
			wantState: sandboxUnavailable, wantOutput: "bwrap: Creating new namespace failed: Operation not permitted",
			wantShape: shapeArgv("--tmpfs", "/proc"),
		},
		{
			name:      "a deadline error is an ordinary failure",
			results:   []fakeSandboxResult{{}, {err: context.DeadlineExceeded}},
			wantState: sandboxUnavailable,
			wantShape: shapeArgv("--proc", "/proc"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSandboxRun{results: tc.results}
			p := sandboxProbe{run: f.run, shim: shim, workDir: "/ws", kiroDir: "/home/.kiro"}
			v := p.check(t.Context())
			if v.state != tc.wantState {
				t.Errorf("state = %q, want %q", v.state, tc.wantState)
			}
			if v.proc != tc.wantProc {
				t.Errorf("proc = %q, want %q", v.proc, tc.wantProc)
			}
			if v.output != tc.wantOutput {
				t.Errorf("output = %q, want %q", v.output, tc.wantOutput)
			}
			if (v.err != nil) != (tc.wantState == sandboxUnavailable) {
				t.Errorf("err = %v, want an error exactly when unavailable", v.err)
			}
			if len(f.calls) != 2 {
				t.Fatalf("runner calls = %d, want 2 (kiro-cli's probe, then the command shape)", len(f.calls))
			}
			if !slices.Equal(f.calls[0], probeArgv) {
				t.Errorf("probe argv = %q, want kiro-cli's own probe %q", f.calls[0], probeArgv)
			}
			if !slices.Equal(f.calls[1], tc.wantShape) {
				t.Errorf("shape argv = %q, want %q", f.calls[1], tc.wantShape)
			}
			if !f.hadDeadline[0] || !f.hadDeadline[1] {
				t.Errorf("deadlines = %v, want every sandboxed run bounded", f.hadDeadline)
			}
		})
	}
	t.Run("a missing shim is unavailable without running anything", func(t *testing.T) {
		f := &fakeSandboxRun{}
		missing := filepath.Join(t.TempDir(), "bwrap")
		v := sandboxProbe{run: f.run, shim: missing, workDir: "/ws"}.check(t.Context())
		if v.state != sandboxUnavailable {
			t.Errorf("state = %q, want unavailable", v.state)
		}
		if v.err == nil || !strings.Contains(v.err.Error(), missing) {
			t.Errorf("err = %v, want it to name the missing shim %s", v.err, missing)
		}
		if len(f.calls) != 0 {
			t.Errorf("runner calls = %q, want none", f.calls)
		}
	})
	t.Run("an interrupted check is not reported as a timeout", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		killed := errors.New("signal: killed")
		f := &fakeSandboxRun{results: []fakeSandboxResult{{err: killed}, {err: killed}}}
		v := sandboxProbe{run: f.run, shim: shim, workDir: "/ws"}.check(ctx)
		if v.state != sandboxUnavailable {
			t.Errorf("state = %q, want unavailable", v.state)
		}
		if v.err == nil || !errors.Is(v.err, context.Canceled) || !strings.Contains(v.err.Error(), "interrupted") {
			t.Errorf("err = %v, want an interruption wrapping context.Canceled", v.err)
		}
		if v.err != nil && strings.Contains(v.err.Error(), "did not finish") {
			t.Errorf("err = %v, want no timeout wording for a cancelled parent", v.err)
		}
	})
	t.Run("no kiro dir means no bind for it", func(t *testing.T) {
		f := &fakeSandboxRun{results: []fakeSandboxResult{{}, {}}}
		sandboxProbe{run: f.run, shim: shim, workDir: "/ws"}.check(t.Context())
		if len(f.calls) != 2 {
			t.Fatalf("runner calls = %d, want 2", len(f.calls))
		}
		if slices.Contains(f.calls[1], "/home/.kiro") || slices.Contains(f.calls[1], "") {
			t.Errorf("shape argv = %q, want no kiro bind and no empty argument", f.calls[1])
		}
	})
}

// Serial: it shortens the package-level probe budget.
func TestSandboxProbe_checkBoundsAHungBwrap(t *testing.T) {
	saved := sandboxProbeTimeout
	sandboxProbeTimeout = 20 * time.Millisecond
	t.Cleanup(func() { sandboxProbeTimeout = saved })
	hang := func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		select {
		case <-ctx.Done():
			return nil, errors.New("signal: killed")
		case <-time.After(10 * time.Second):
			return nil, nil
		}
	}
	start := time.Now()
	v := sandboxProbe{run: hang, shim: fakeShim(t), workDir: "/ws"}.check(t.Context())
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("check took %s, want it bounded by the per-run budget", elapsed)
	}
	if v.state != sandboxUnavailable {
		t.Errorf("state = %q, want unavailable", v.state)
	}
	if v.err == nil || !errors.Is(v.err, context.DeadlineExceeded) || !strings.Contains(v.err.Error(), "did not finish") {
		t.Errorf("err = %v, want a timeout wrapping context.DeadlineExceeded", v.err)
	}
}

func TestRunSandboxCommand(t *testing.T) {
	t.Run("returns stderr and the exit error", func(t *testing.T) {
		out, err := runSandboxCommand(t.Context(), "/bin/sh", "-c", "echo out; echo 'bwrap: denied' >&2; exit 3")
		if got := strings.TrimSpace(string(out)); got != "bwrap: denied" {
			t.Errorf("output = %q, want stderr only", got)
		}
		if exitErr, ok := errors.AsType[*exec.ExitError](err); !ok || exitErr.ExitCode() != 3 {
			t.Errorf("err = %v, want exit status 3", err)
		}
	})
	t.Run("a success returns no error", func(t *testing.T) {
		if _, err := runSandboxCommand(t.Context(), "/bin/true"); err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	})
}

func TestCapSandboxOutput(t *testing.T) {
	if got := capSandboxOutput([]byte("  bwrap: x\n")); got != "bwrap: x" {
		t.Errorf("short output = %q, want it trimmed", got)
	}
	long := strings.Repeat("a", sandboxOutputCap-1) + "é" + "tail"
	got := capSandboxOutput([]byte(long))
	if want := strings.Repeat("a", sandboxOutputCap-1) + "..."; got != want {
		t.Errorf("capped output = %q, want the cut moved back to a rune start", got)
	}
}

type fakeLookPath string

func (f fakeLookPath) look(string) (string, error) {
	if f == "" {
		return "", exec.ErrNotFound
	}
	return string(f), nil
}

// Serial: capture.Default swaps the process-global logger.
func TestStartSandboxWith(t *testing.T) {
	newDeps := func(t *testing.T, f *fakeSandboxRun, stray fakeLookPath) *sandboxDeps {
		return &sandboxDeps{
			probe:       sandboxProbe{run: f.run, shim: fakeShim(t), workDir: "/ws"},
			runUserRoot: t.TempDir(),
			refuseDir:   "/refuse",
			lookPath:    stray.look,
			uid:         4242,
		}
	}
	t.Run("off runs nothing and logs nothing", func(t *testing.T) {
		records := capture.Default(t)
		f := &fakeSandboxRun{}
		if got := startSandboxWith(t.Context(), false, newDeps(t, f, "")); got != (sandboxRuntime{state: sandboxOff}) {
			t.Errorf("runtime = %+v, want off with tabs' PATH left alone", got)
		}
		if len(f.calls) != 0 || records.Len() != 0 {
			t.Errorf("calls = %q, log = %q, want neither", f.calls, records.Messages())
		}
	})
	t.Run("off still names a bwrap kiro-cli would find", func(t *testing.T) {
		records := capture.Default(t)
		startSandboxWith(t.Context(), false, newDeps(t, &fakeSandboxRun{}, "/config/tools/bin/bwrap"))
		if records.CountLevel(slog.LevelWarn, sandboxStrayBwrapMsg) != 1 || !records.HasAttr(sandboxStrayBwrapMsg, "path", "/config/tools/bin/bwrap") {
			t.Errorf("log = %q, want one stray-bwrap Warn naming its path", records.Messages())
		}
	})
	t.Run("available logs info and creates the proxy directory", func(t *testing.T) {
		records := capture.Default(t)
		f := &fakeSandboxRun{results: []fakeSandboxResult{{err: errBwrap}, {}}}
		deps := newDeps(t, f, "")
		want := sandboxRuntime{state: sandboxAvailable, bwrapDir: filepath.Dir(deps.probe.shim)}
		if got := startSandboxWith(t.Context(), true, deps); got != want {
			t.Errorf("runtime = %+v, want %+v (tabs find the checked shim)", got, want)
		}
		if records.CountLevel(slog.LevelInfo, sandboxAvailableMsg) != 1 || !records.HasAttr(sandboxAvailableMsg, "proc", "tmpfs") {
			t.Errorf("log = %q, want one Info with proc=tmpfs", records.Messages())
		}
		if records.CountLevel(slog.LevelWarn, "") != 0 {
			t.Errorf("log = %q, want no Warn", records.Messages())
		}
		fi, err := os.Stat(filepath.Join(deps.runUserRoot, strconv.Itoa(deps.uid)))
		if err != nil {
			t.Fatalf("proxy directory: %v", err)
		}
		if !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Errorf("proxy directory mode = %v, want a 0700 directory", fi.Mode())
		}
	})
	t.Run("an uncreatable proxy directory warns and stays available", func(t *testing.T) {
		records := capture.Default(t)
		f := &fakeSandboxRun{results: []fakeSandboxResult{{}, {}}}
		deps := newDeps(t, f, "")
		file := filepath.Join(deps.runUserRoot, "file")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		deps.runUserRoot = file
		if got := startSandboxWith(t.Context(), true, deps).state; got != sandboxAvailable {
			t.Errorf("state = %q, want available", got)
		}
		if records.CountLevel(slog.LevelWarn, sandboxRunUserMsg) != 1 {
			t.Errorf("log = %q, want one Warn for the proxy directory", records.Messages())
		}
	})
	t.Run("unavailable warns with the error and bwrap's text", func(t *testing.T) {
		records := capture.Default(t)
		f := &fakeSandboxRun{results: []fakeSandboxResult{{err: errBwrap}, {err: errBwrap, out: "bwrap: No permissions"}}}
		deps := newDeps(t, f, "/usr/local/bin/bwrap")
		if got := startSandboxWith(t.Context(), true, deps); got != (sandboxRuntime{state: sandboxUnavailable, bwrapDir: "/refuse"}) {
			t.Errorf("runtime = %+v, want unavailable with tabs finding the refusing bwrap", got)
		}
		if records.CountLevel(slog.LevelWarn, "") != 1 || records.CountExact(sandboxUnavailableMsg) != 1 {
			t.Errorf("log = %q, want exactly the one unavailable Warn", records.Messages())
		}
		if !records.HasAttr(sandboxUnavailableMsg, "output", "bwrap: No permissions") || !records.HasAttr(sandboxUnavailableMsg, "error", errBwrap.Error()) {
			t.Errorf("log = %q, want the error and bwrap's stderr on the Warn", records.Messages())
		}
		if _, err := os.Stat(filepath.Join(deps.runUserRoot, strconv.Itoa(deps.uid))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("proxy directory stat = %v, want it not created", err)
		}
	})
}

func TestSandboxRuntime_sessionEnv(t *testing.T) {
	available := sandboxRuntime{state: sandboxAvailable, bwrapDir: sandboxBinDir}
	t.Run("available leads the last PATH and leaves the input alone", func(t *testing.T) {
		in := []string{"PATH=/first", "FOO=1", "PATH=/v:/usr/bin"}
		got := available.sessionEnv(func() []string { return in })()
		want := []string{"PATH=/first", "FOO=1", "PATH=" + sandboxBinDir + ":/v:/usr/bin"}
		if !slices.Equal(got, want) {
			t.Errorf("env = %q, want %q", got, want)
		}
		if in[2] != "PATH=/v:/usr/bin" {
			t.Errorf("input PATH = %q, want it unmodified", in[2])
		}
	})
	t.Run("no overlay builds PATH from the server's", func(t *testing.T) {
		t.Setenv("PATH", "/usr/bin:/bin")
		got := available.sessionEnv(func() []string { return nil })()
		if want := []string{"PATH=" + sandboxBinDir + ":/usr/bin:/bin"}; !slices.Equal(got, want) {
			t.Errorf("env = %q, want %q", got, want)
		}
	})
	t.Run("an empty server PATH adds no separator", func(t *testing.T) {
		t.Setenv("PATH", "")
		got := available.sessionEnv(func() []string { return nil })()
		if want := []string{"PATH=" + sandboxBinDir}; !slices.Equal(got, want) {
			t.Errorf("env = %q, want %q", got, want)
		}
	})
	t.Run("off returns the overlay unchanged", func(t *testing.T) {
		got := sandboxRuntime{state: sandboxOff}.sessionEnv(func() []string { return []string{"PATH=/v"} })()
		if !slices.Equal(got, []string{"PATH=/v"}) {
			t.Errorf("env = %q, want the base overlay", got)
		}
	})
	t.Run("unavailable leads PATH with the refusing bwrap", func(t *testing.T) {
		unavailable := sandboxRuntime{state: sandboxUnavailable, bwrapDir: sandboxRefuseDir}
		got := unavailable.sessionEnv(func() []string { return []string{"PATH=/config/tools/bin:/usr/bin"} })()
		if want := []string{"PATH=" + sandboxRefuseDir + ":/config/tools/bin:/usr/bin"}; !slices.Equal(got, want) {
			t.Errorf("env = %q, want %q", got, want)
		}
	})
	t.Run("the tab overlay carries the shim directory first", func(t *testing.T) {
		d := &routeDeps{
			sessionEnv:      available.sessionEnv(func() []string { return []string{"PATH=/v:/usr/bin"} }),
			sessionTitleEnv: func(id terminal.SessionID) []string { return []string{"WT_TITLE_HANDLE=" + string(id)} },
		}
		got := d.childEnv("tab1")
		want := []string{"PATH=" + sandboxBinDir + ":/v:/usr/bin", "WT_TITLE_HANDLE=tab1"}
		if !slices.Equal(got, want) {
			t.Errorf("childEnv = %q, want %q", got, want)
		}
	})
}

// Serial: capture.Default swaps the process-global logger.
func TestParseSandboxOptIn(t *testing.T) {
	const token = "s3cr3t-token-abc123"
	for _, tc := range []struct {
		name      string
		raw       string
		want      bool
		wantWarns int
	}{
		{"true", "true", true, 0},
		{"false", "false", false, 0},
		{"a padded upper-case alias turns it on", " YES ", true, 0},
		{"one turns it on", "1", true, 0},
		{"an alias leaves it off", "Off", false, 0},
		{"zero leaves it off", "0", false, 0},
		{"unset", "", false, 0},
		{"a token-shaped value leaves it off", token, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records := capture.Default(t)
			t.Setenv(sandboxOptInKey, tc.raw)
			if got := parseSandboxOptIn(); got != tc.want {
				t.Errorf("parseSandboxOptIn() with %q = %v, want %v", tc.raw, got, tc.want)
			}
			if got := records.CountLevel(slog.LevelWarn, ""); got != tc.wantWarns {
				t.Errorf("log = %q, want %d Warn", records.Messages(), tc.wantWarns)
			}
			if tc.wantWarns > 0 && records.CountExact(sandboxOptInBadMsg) != 1 {
				t.Errorf("log = %q, want the malformed-value Warn", records.Messages())
			}
			if tc.raw == token && logContains(records, token) {
				t.Errorf("log = %q carries the raw value; it must warn by name only", records.Messages())
			}
		})
	}
}

func writeExecutable(t *testing.T, path string, body []byte) {
	t.Helper()
	// ETXTBSY: https://go.dev/issue/22315
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxShim_execsBwrapWithTheNamespacesKiroCLIOmits(t *testing.T) {
	const libexecBwrap = "/opt/web-terminal-kiro/libexec/bwrap"
	shim, err := os.ReadFile(filepath.Join("sandbox", "bwrap.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(shim), libexecBwrap); n != 1 {
		t.Fatalf("sandbox/bwrap.sh names %s %d times, want once", libexecBwrap, n)
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "bwrap-real")
	writeExecutable(t, fake, []byte("#!/bin/sh\necho \"pid=$$\"\nfor a; do printf '[%s]\\n' \"$a\"; done\n"))
	shimPath := filepath.Join(dir, "bwrap")
	writeExecutable(t, shimPath, []byte(strings.Replace(string(shim), libexecBwrap, fake, 1)))

	args := append(slices.Clone(kasProbeArgs), "two words", "")
	cmd := exec.CommandContext(t.Context(), shimPath, args...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("shim: %v", err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if want := "pid=" + strconv.Itoa(cmd.Process.Pid); got[0] != want {
		t.Errorf("bwrap ran as %s, want %s: the shim must exec it so kiro-cli's signals reach bwrap", got[0], want)
	}
	want := []string{"[--unshare-user]", "[--unshare-pid]"}
	for _, a := range args {
		want = append(want, "["+a+"]")
	}
	if !slices.Equal(got[1:], want) {
		t.Errorf("bwrap argv = %q, want %q", got[1:], want)
	}
}

// Serial: it sets PATH.
func TestSandboxRuntime_unavailableTabsResolveOnlyTheRefusingBwrap(t *testing.T) {
	refuser, err := os.ReadFile(filepath.Join("sandbox", "bwrap-unavailable.sh"))
	if err != nil {
		t.Fatal(err)
	}
	refuseDir, toolsDir, laterDir := t.TempDir(), t.TempDir(), t.TempDir()
	writeExecutable(t, filepath.Join(refuseDir, "bwrap"), refuser)
	stray := []byte("#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(toolsDir, "bwrap"), stray)
	basePath := toolsDir + string(os.PathListSeparator) + laterDir
	t.Setenv("PATH", basePath)
	if got, err := exec.LookPath("bwrap"); err != nil || got != filepath.Join(toolsDir, "bwrap") {
		t.Fatalf("LookPath on the base PATH = %q, %v; want the stray bwrap installed before start", got, err)
	}

	tabEnv := sandboxRuntime{state: sandboxUnavailable, bwrapDir: refuseDir}.
		sessionEnv(func() []string { return []string{"PATH=" + basePath} })
	writeExecutable(t, filepath.Join(laterDir, "bwrap"), stray)
	env := tabEnv()
	tabPath, ok := strings.CutPrefix(env[len(env)-1], "PATH=")
	if !ok {
		t.Fatalf("tab env = %q, want PATH last", env)
	}
	t.Setenv("PATH", tabPath)
	got, err := exec.LookPath("bwrap")
	if err != nil || got != filepath.Join(refuseDir, "bwrap") {
		t.Fatalf("a tab resolves bwrap to %q, %v; want the refusing one ahead of the strays", got, err)
	}

	cmd := exec.CommandContext(t.Context(), got, kasProbeArgs...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if exitErr, ok := errors.AsType[*exec.ExitError](runErr); !ok || exitErr.ExitCode() != 127 {
		t.Errorf("kiro-cli's probe through it = %v, want exit status 127, which kiro-cli reads as unavailable", runErr)
	}
	if strings.Contains(stderr.String(), "bwrap: execvp ") {
		t.Errorf("stderr = %q carries \"bwrap: execvp \", which makes kiro-cli report its sandbox as on", stderr.String())
	}
	if !strings.Contains(stderr.String(), "server log") {
		t.Errorf("stderr = %q, want it to point at the server log", stderr.String())
	}
}
