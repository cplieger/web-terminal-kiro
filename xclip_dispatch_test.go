package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// xclipCmd runs this test binary as the image's xclip link, in a new session so
// it has no controlling terminal.
func xclipCmd(t *testing.T, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), self, args...)
	cmd.Args[0] = filepath.Join(t.TempDir(), "xclip")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		code = exit.ExitCode()
	default:
		t.Fatalf("run as xclip: %v", err)
	}
	return out.String(), errOut.String(), code
}

// TestXclipDispatch: run as xclip, the binary is osc52copy's stand-in, whose
// Main returns 1 for a failed copy or a paste, with one line on stderr.
func TestXclipDispatch(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		code       int
		mentionTTY bool
	}{
		{"copy with no terminal fails at the tty open", []string{"-selection", "clipboard"}, 1, true},
		{"paste is refused before the terminal is opened", []string{"-o"}, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := xclipCmd(t, "copied text", tc.args...)
			if code != tc.code {
				t.Errorf("xclip %v exited %d, want %d; stderr %q", tc.args, code, tc.code, stderr)
			}
			if stdout != "" {
				t.Errorf("xclip %v wrote %q to stdout, want nothing", tc.args, stdout)
			}
			if !strings.HasPrefix(stderr, "xclip: ") {
				t.Errorf("xclip %v stderr = %q, want one line starting \"xclip: \"", tc.args, stderr)
			}
			if got := strings.Contains(stderr, "/dev/tty"); got != tc.mentionTTY {
				t.Errorf("xclip %v stderr = %q; mentions /dev/tty = %v, want %v", tc.args, stderr, got, tc.mentionTTY)
			}
		})
	}
}

// TestOpenBlockingWriteLeavesFdBlocking: a non-blocking fd splits one large
// OSC 52 write into several write(2) calls. A FIFO stands in for the tty,
// since Go's poller takes both.
func TestOpenBlockingWriteLeavesFdBlocking(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "tty")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := syscall.Open(fifo, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(reader) })

	f, err := openBlockingWrite(fifo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	conn, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags uintptr
	var errno syscall.Errno
	// SyscallConn, not f.Fd: Fd switches the fd back to blocking.
	if err := conn.Control(func(fd uintptr) {
		flags, _, errno = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0)
	}); err != nil {
		t.Fatal(err)
	}
	if errno != 0 {
		t.Fatalf("fcntl F_GETFL: %v", errno)
	}
	if flags&syscall.O_NONBLOCK != 0 {
		t.Error("openBlockingWrite returned a non-blocking fd")
	}
}

func TestOpenBlockingWriteNamesPathOnFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	_, err := openBlockingWrite(missing)
	var pe *os.PathError
	if !errors.As(err, &pe) || pe.Op != "open" || pe.Path != missing {
		t.Errorf("openBlockingWrite(%q) error = %v, want an open PathError naming the path", missing, err)
	}
}

// TestDockerfileLinksXclip: kiro-cli copies by running xclip, so the image links
// that name to the server binary.
func TestDockerfileLinksXclip(t *testing.T) {
	raw, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	logical := strings.ReplaceAll(string(raw), "\\\n", " ")
	for line := range strings.Lines(logical) {
		if strings.HasPrefix(line, "RUN ") && strings.Contains(line, "ln -s /app/web-terminal-kiro /usr/local/bin/xclip") {
			return
		}
	}
	t.Error("no Dockerfile RUN links /app/web-terminal-kiro to /usr/local/bin/xclip")
}
