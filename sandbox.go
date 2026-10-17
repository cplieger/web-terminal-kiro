package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cplieger/envx/v2"
)

const (
	sandboxOptInKey = "KIRO_CLI_SANDBOX"
	// sandboxBinDir holds the bwrap shim (sandbox/bwrap.sh) and nothing else, so
	// putting it first on a tab's PATH cannot shadow any other command.
	sandboxBinDir = "/opt/web-terminal-kiro/sandbox/bin"
	// sandboxRefuseDir holds only a bwrap that refuses every command
	// (sandbox/bwrap-unavailable.sh). Leading a tab's PATH with it hides every
	// other bwrap, one installed after startup included.
	sandboxRefuseDir = "/opt/web-terminal-kiro/sandbox/unavailable"
	sandboxOutputCap = 512
	// sandboxWaitDelay bounds the wait for a killed bwrap's descendants to
	// release its stderr pipe.
	sandboxWaitDelay = time.Second
	// runUserRoot is where kiro-cli's "common dependencies" network mode puts its
	// proxy socket; it refuses that mode when /run/user/<uid> is missing.
	runUserRoot = "/run/user"
	// sandboxAppArmorLabel is the label of a process under the profile
	// sandbox/web-terminal-kiro-sandbox declares, in enforce mode.
	sandboxAppArmorLabel = "web-terminal-kiro-sandbox (enforce)"
)

// The AppArmor-only attr file exists from Linux 5.8
// (https://git.kernel.org/torvalds/c/6413f852ce08); older kernels expose the
// label only in the shared one.
var appArmorAttrPaths = []string{"/proc/self/attr/apparmor/current", "/proc/self/attr/current"}

// sandboxProbeTimeout bounds each sandboxed /bin/true; the check runs two
// before the listener binds, well inside the healthcheck start period. A var so
// a test can prove a hung bwrap cannot hold startup.
var sandboxProbeTimeout = 5 * time.Second

// The sandbox states /api/health serves; a published contract like the
// readiness reasons.
const (
	sandboxOff         = "off"
	sandboxAvailable   = "available"
	sandboxUnavailable = "unavailable"
)

const (
	sandboxAvailableMsg   = "kiro-cli sandbox available"
	sandboxUnavailableMsg = "kiro-cli sandbox unavailable"
	sandboxStrayBwrapMsg  = "a bwrap is on PATH while " + sandboxOptInKey + " is off; kiro-cli may report its sandbox as on while it cannot run a sandboxed command"
	sandboxRunUserMsg     = "could not create the kiro-cli sandbox proxy directory, so the sandbox's \"common dependencies\" network mode will fail; \"block all\" and \"allow all\" still work"
	sandboxOptInBadMsg    = "unparseable " + sandboxOptInKey + "; leaving the kiro-cli sandbox off (the default)"
)

// parseSandboxOptIn reads KIRO_CLI_SANDBOX (default false). A malformed value
// leaves the check off and warns by name only, like LOG_OSC_TEXT.
func parseSandboxOptIn() bool {
	on, _, err := envx.BoolStrict(sandboxOptInKey)
	if err != nil {
		slog.Warn(sandboxOptInBadMsg, "hint", "use true, 1, yes or on to turn it on, or false, 0, no or off, in any letter case")
		return false
	}
	return on
}

// kasProbeArgs and sandboxShapeArgs mirror kiro-cli's own bwrap probe and the
// namespace and mount argv of a command in its default "block all" network mode
// (KAS LocalSandbox isAvailable and buildArgs, kiro-cli 2.32.0). The app probes
// at all because kiro-cli reports its sandbox as on when bwrap cannot start one
// (https://github.com/kirodotdev/Kiro/issues/11941).
var kasProbeArgs = []string{"--ro-bind", "/", "/", "--proc", "/proc", "--", "/bin/true"}

func sandboxShapeArgs(workDir, kiroDir string, procSupported bool) []string {
	args := []string{"--ro-bind", "/", "/", "--tmpfs", "/tmp", "--bind", workDir, workDir}
	if kiroDir != "" {
		args = append(args, "--bind", kiroDir, kiroDir)
	}
	args = append(args, "--dev", "/dev")
	if procSupported {
		args = append(args, "--proc", "/proc")
	} else {
		args = append(args, "--tmpfs", "/proc")
	}
	return append(args, "--unshare-pid", "--die-with-parent", "--new-session",
		"--tmpfs", "/run", "--clearenv", "--unshare-net", "--", "/bin/true")
}

// sandboxRunner runs one command and returns its stderr.
type sandboxRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func runSandboxCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = sandboxWaitDelay
	err := cmd.Run()
	return stderr.Bytes(), err
}

type sandboxProbe struct {
	run     sandboxRunner
	shim    string
	workDir string
	// kiroDir is bound like kiro-cli binds ~/.kiro; "" when it does not exist.
	kiroDir       string
	appArmorAttrs []string
}

// checkAppArmorConfinement returns nil only when the first of paths that
// exists holds sandboxAppArmorLabel. Docker ignores apparmor= on a host
// without AppArmor, which leaves the seccomp profile's mount rules unchecked.
func checkAppArmorConfinement(paths []string) error {
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read the container's AppArmor confinement: %w", err)
		}
		if strings.TrimSpace(string(raw)) != sandboxAppArmorLabel {
			return fmt.Errorf("AppArmor confines the container as %q, want %q", capSandboxOutput(raw), sandboxAppArmorLabel)
		}
		return nil
	}
	return fmt.Errorf("read the container's AppArmor confinement: none of %q exists: %w", paths, fs.ErrNotExist)
}

type sandboxVerdict struct {
	err    error
	state  string
	proc   string
	output string
}

// check confirms the container's AppArmor confinement, then runs kiro-cli's
// probe and the command shape kiro-cli would pick from that probe's result.
// Only the second run decides: kiro-cli treats a failed probe as "no fresh
// /proc" and carries on, so the probe alone proves nothing.
func (p *sandboxProbe) check(ctx context.Context) sandboxVerdict {
	if _, err := os.Stat(p.shim); err != nil {
		return sandboxVerdict{state: sandboxUnavailable, err: fmt.Errorf("bwrap shim %s: %w", p.shim, err)}
	}
	if err := checkAppArmorConfinement(p.appArmorAttrs); err != nil {
		return sandboxVerdict{state: sandboxUnavailable, err: err}
	}
	_, probeErr := p.runOne(ctx, kasProbeArgs)
	procSupported := probeErr == nil
	out, err := p.runOne(ctx, sandboxShapeArgs(p.workDir, p.kiroDir, procSupported))
	if err != nil {
		return sandboxVerdict{state: sandboxUnavailable, err: err, output: capSandboxOutput(out)}
	}
	proc := "tmpfs"
	if procSupported {
		proc = "proc"
	}
	return sandboxVerdict{state: sandboxAvailable, proc: proc}
}

func (p *sandboxProbe) runOne(ctx context.Context, args []string) ([]byte, error) {
	runCtx, cancel := context.WithTimeout(ctx, sandboxProbeTimeout)
	defer cancel()
	out, err := p.run(runCtx, p.shim, args...)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		err = fmt.Errorf("startup check interrupted: %w", errors.Join(ctx.Err(), err))
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		err = fmt.Errorf("sandboxed command did not finish within %s: %w", sandboxProbeTimeout, errors.Join(runCtx.Err(), err))
	}
	return out, err
}

func capSandboxOutput(out []byte) string {
	s := strings.TrimSpace(string(out))
	if len(s) <= sandboxOutputCap {
		return strings.ToValidUTF8(s, "\uFFFD")
	}
	cut := sandboxOutputCap
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.ToValidUTF8(s[:cut], "\uFFFD") + "..."
}

type sandboxDeps struct {
	lookPath    func(string) (string, error)
	runUserRoot string
	// refuseDir leads an unavailable tab's PATH; the probe's shim directory
	// leads an available one's.
	refuseDir string
	probe     sandboxProbe
	uid       int
}

// sandboxRuntime is the startup check's verdict, fixed for the container's
// life: seccomp and AppArmor profiles are set when the container is created.
type sandboxRuntime struct {
	state string
	// bwrapDir leads every tab's PATH so kiro-cli resolves its bwrap there; ""
	// leaves PATH alone.
	bwrapDir string
}

// startSandbox runs the startup check when the operator opted in and logs the
// verdict. It blocks for at most two sandboxProbeTimeouts plus two
// sandboxWaitDelays.
func startSandbox(ctx context.Context, optIn bool, workDir, home string) sandboxRuntime {
	kiroDir := ""
	if home != "" {
		if fi, err := os.Stat(filepath.Join(home, ".kiro")); err == nil && fi.IsDir() {
			kiroDir = filepath.Join(home, ".kiro")
		}
	}
	return startSandboxWith(ctx, optIn, &sandboxDeps{
		probe: sandboxProbe{
			run:           runSandboxCommand,
			shim:          filepath.Join(sandboxBinDir, "bwrap"),
			workDir:       workDir,
			kiroDir:       kiroDir,
			appArmorAttrs: appArmorAttrPaths,
		},
		runUserRoot: runUserRoot,
		refuseDir:   sandboxRefuseDir,
		lookPath:    exec.LookPath,
		uid:         os.Getuid(),
	})
}

func startSandboxWith(ctx context.Context, optIn bool, deps *sandboxDeps) sandboxRuntime {
	if !optIn {
		warnStrayBwrap(deps.lookPath)
		return sandboxRuntime{state: sandboxOff}
	}
	v := deps.probe.check(ctx)
	if v.state != sandboxAvailable {
		slog.Warn(sandboxUnavailableMsg,
			"error", v.err,
			"output", v.output,
			"hint", "every bwrap kiro-cli finds refuses to run, so it reports its sandbox as unavailable; the container usually lacks the seccomp and AppArmor profiles in the README section \"Turning on the command sandbox\"")
		return sandboxRuntime{state: sandboxUnavailable, bwrapDir: deps.refuseDir}
	}
	slog.Info(sandboxAvailableMsg, "proc", v.proc)
	dir := filepath.Join(deps.runUserRoot, strconv.Itoa(deps.uid))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Warn(sandboxRunUserMsg, "path", dir, "error", err)
	}
	return sandboxRuntime{state: sandboxAvailable, bwrapDir: filepath.Dir(deps.probe.shim)}
}

// warnStrayBwrap reports a bwrap on the server's PATH, which tabs inherit when
// the switch is off: kiro-cli would find it and could claim a sandbox it
// cannot run (#11941).
func warnStrayBwrap(lookPath func(string) (string, error)) {
	// exec.ErrDot still returns the path a shell would run, so path, not err,
	// decides.
	path, _ := lookPath("bwrap")
	if path == "" {
		return
	}
	slog.Warn(sandboxStrayBwrapMsg, "path", path,
		"hint", "remove that bwrap, or set "+sandboxOptInKey+"=true so the server checks the sandbox and tabs only find a bwrap that passed")
}

// sessionEnv leads each tab's PATH with bwrapDir, and returns base unchanged
// when it is "". The child keeps the LAST PATH= entry, so that is the one
// rewritten; every call returns a fresh slice.
func (s sandboxRuntime) sessionEnv(base func() []string) func() []string {
	if s.bwrapDir == "" {
		return base
	}
	return func() []string {
		env := slices.Clone(base())
		for i := len(env) - 1; i >= 0; i-- {
			if rest, ok := strings.CutPrefix(env[i], "PATH="); ok {
				env[i] = "PATH=" + leadPath(s.bwrapDir, rest)
				return env
			}
		}
		return append(env, "PATH="+leadPath(s.bwrapDir, os.Getenv("PATH")))
	}
}

// leadPath puts dir first; an empty rest adds no separator, because an empty
// PATH element means the current directory.
func leadPath(dir, rest string) string {
	if rest == "" {
		return dir
	}
	return dir + string(os.PathListSeparator) + rest
}
