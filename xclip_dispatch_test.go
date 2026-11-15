package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// xclipCommand runs this test binary as the image's xclip link, in a new
// session so it has no controlling terminal unless the caller gives it one.
func xclipCommand(t *testing.T, stdin string, args ...string) *exec.Cmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), self, args...)
	cmd.Args[0] = filepath.Join(t.TempDir(), "xclip")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = strings.NewReader(stdin)
	return cmd
}

func xclipCmd(t *testing.T, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := xclipCommand(t, stdin, args...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
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

// TestXclipCopyHoldsTheTTYForOneWrite: with the pty buffer full, a split OSC 52
// write lets another writer's bytes land inside the sequence, which cuts the
// payload the browser decodes.
func TestXclipCopyHoldsTheTTYForOneWrite(t *testing.T) {
	master, slave := openPTY(t)
	text := strings.Repeat("full-screen drag selection\n", 12_000)
	cmd := xclipCommand(t, text, "-selection", "clipboard")
	cmd.ExtraFiles = []*os.File{slave}
	cmd.SysProcAttr.Setctty = true
	cmd.SysProcAttr.Ctty = 3 // ExtraFiles[0] in the child
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	noiseDone := make(chan struct{})
	go func() {
		defer close(noiseDone)
		for {
			if _, err := slave.Write([]byte{'#'}); err != nil {
				return // the master closed
			}
		}
	}()
	t.Cleanup(func() {
		_ = master.Close()
		<-noiseDone
	})

	const prefix, terminator = "\x1b]52;c;", "\x1b\\"
	if err := master.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var out []byte
	buf := make([]byte, 1024)
	start, end := -1, -1
	for end < 0 {
		n, err := master.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("read the pty master: %v; xclip stderr %q", err, stderr.String())
		}
		if start < 0 {
			start = bytes.Index(out, []byte(prefix))
		}
		if start >= 0 {
			end = bytes.Index(out[start:], []byte(terminator))
		}
		time.Sleep(50 * time.Microsecond) // a slow reader keeps the pty buffer full
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("xclip copy: %v; stderr %q", err, stderr.String())
	}

	payload := out[start+len(prefix) : start+end]
	if foreign := bytes.Count(payload, []byte{'#'}); foreign > 0 {
		t.Fatalf("%d foreign bytes inside the %d-byte OSC 52 payload", foreign, len(payload))
	}
	got, err := base64.StdEncoding.DecodeString(string(payload))
	if err != nil {
		t.Fatalf("decode the OSC 52 payload: %v", err)
	}
	if string(got) != text {
		t.Errorf("OSC 52 payload decodes to %d bytes, want the %d-byte input", len(got), len(text))
	}
}

// openPTY returns a pty master, read with deadlines, and its slave, opened
// blocking.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := master.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var unlock int32
	var ptn uint32
	var errno syscall.Errno
	if err := conn.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock)))
		if errno == 0 {
			_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGPTN, uintptr(unsafe.Pointer(&ptn)))
		}
	}); err != nil {
		t.Fatal(err)
	}
	if errno != 0 {
		t.Fatalf("unlock and number the pty: %v", errno)
	}
	path := "/dev/pts/" + strconv.FormatUint(uint64(ptn), 10)
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	slave = os.NewFile(uintptr(fd), path)
	// Registered first so it runs after the master closes: a write blocked on
	// the slave ends only then, and Close waits for it.
	t.Cleanup(func() { _ = slave.Close() })
	t.Cleanup(func() { _ = master.Close() })
	return master, slave
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
