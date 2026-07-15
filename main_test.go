package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCLI writes an executable shell stub standing in for kiro-cli. Its whoami
// exits with whoamiRC (mirroring the real binary: 0 logged in, 1 not); login
// records its argv to a marker file and succeeds; chat prints a sentinel. The
// stub lets the sessionCommand wrapper be executed for real, so the guard's
// actual runtime behavior is pinned, not just the script text.
func fakeCLI(t *testing.T, dir string, whoamiRC int) (cliPath, loginMarker string) {
	t.Helper()
	cliPath = filepath.Join(dir, "fake kiro-cli") // space: pins the $0 quoting
	loginMarker = filepath.Join(dir, "login-args")
	stub := `#!/bin/sh
case "$1" in
whoami) exit ` + map[bool]string{true: "0", false: "1"}[whoamiRC == 0] + ` ;;
login) shift; printf '%s' "$*" > ` + "'" + loginMarker + "'" + `; exit 0 ;;
chat) echo CHAT_STARTED ;;
esac
`
	if err := os.WriteFile(cliPath, []byte(stub), 0o755); err != nil { // #nosec G306 -- test stub must be executable
		t.Fatalf("write fake cli: %v", err)
	}
	return cliPath, loginMarker
}

// TestSessionCommand_loginGuard executes the wrapper against a fake kiro-cli
// and pins the guard's contract: a logged-out CLI (whoami exits 1) gets the
// DEVICE-flow login before chat — the only sign-in flow that works from a
// browser terminal on a headless container (the default flow tries to open a
// local browser, fails, and used to leave a dead session wedging the page) —
// and a logged-in CLI goes straight to chat with no login call.
func TestSessionCommand_loginGuard(t *testing.T) {
	cases := []struct {
		name      string
		whoamiRC  int
		wantLogin bool
	}{
		{name: "logged out: device-flow login runs, then chat", whoamiRC: 1, wantLogin: true},
		{name: "logged in: straight to chat, no login", whoamiRC: 0, wantLogin: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cliPath, loginMarker := fakeCLI(t, dir, tc.whoamiRC)

			argv := sessionCommand(cliPath)
			out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput() // #nosec G204 -- test executes its own wrapper
			if err != nil {
				t.Fatalf("wrapper run: %v\noutput: %s", err, out)
			}
			if !strings.Contains(string(out), "CHAT_STARTED") {
				t.Errorf("chat did not start; output: %s", out)
			}

			args, readErr := os.ReadFile(loginMarker) // #nosec G304 -- test-owned temp path
			if tc.wantLogin {
				if readErr != nil {
					t.Fatalf("login was not invoked (marker missing): %v", readErr)
				}
				if got := string(args); got != "--use-device-flow" {
					t.Errorf("login args = %q, want %q (the browser-opening default flow cannot work headless)", got, "--use-device-flow")
				}
				if !strings.Contains(string(out), "device-flow sign-in") {
					t.Errorf("missing the sign-in explainer line; output: %s", out)
				}
			} else {
				if readErr == nil {
					t.Errorf("login was invoked for a logged-in CLI; args: %s", args)
				}
				if strings.Contains(string(out), "device-flow sign-in") {
					t.Errorf("sign-in explainer printed for a logged-in CLI; output: %s", out)
				}
			}
		})
	}
}

// TestSessionCommand_loginFailureAborts pins the guard's failure mode: when the
// device-flow login itself fails (user hit Esc, network down), the wrapper
// exits non-zero WITHOUT starting chat — the session ends cleanly (the engine
// closes it as process-exited) instead of dropping into a chat that would just
// re-prompt for sign-in and dead-end on the browser open.
func TestSessionCommand_loginFailureAborts(t *testing.T) {
	dir := t.TempDir()
	cliPath := filepath.Join(dir, "kiro-cli")
	stub := `#!/bin/sh
case "$1" in
whoami) exit 1 ;;
login) exit 1 ;;
chat) echo CHAT_STARTED ;;
esac
`
	if err := os.WriteFile(cliPath, []byte(stub), 0o755); err != nil { // #nosec G306 -- test stub must be executable
		t.Fatalf("write fake cli: %v", err)
	}

	argv := sessionCommand(cliPath)
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput() // #nosec G204 -- test executes its own wrapper
	if err == nil {
		t.Fatalf("wrapper succeeded despite login failure; output: %s", out)
	}
	if strings.Contains(string(out), "CHAT_STARTED") {
		t.Errorf("chat started despite login failure; output: %s", out)
	}
}

// TestIsExposedBind pins the security guard behind the "unauthenticated shell
// on a non-loopback address" warning: the wildcard forms and any routable IP
// are exposed, while an explicit loopback bind (127.0.0.0/8, ::1, or the
// "localhost" name) is safe, and an unparseable addr is not flagged. A
// regression dropping the ip==nil disjunct (which covers the empty-host
// wildcard and a hostname) would stop warning operators they are exposing a
// filesystem-access shell.
func TestIsExposedBind(t *testing.T) {
	cases := []struct {
		name string
		addr string
		want bool
	}{
		{name: "wildcard empty host is exposed", addr: ":9848", want: true},
		{name: "0.0.0.0 wildcard is exposed", addr: "0.0.0.0:9848", want: true},
		{name: "ipv6 wildcard is exposed", addr: "[::]:9848", want: true},
		{name: "routable LAN ip is exposed", addr: "192.168.1.5:9848", want: true},
		{name: "public ip is exposed", addr: "203.0.113.7:9848", want: true},
		{name: "hostname is exposed", addr: "myhost:9848", want: true},
		{name: "ipv4 loopback is safe", addr: "127.0.0.1:9848", want: false},
		{name: "ipv4 loopback subnet is safe", addr: "127.0.0.2:9848", want: false},
		{name: "ipv6 loopback is safe", addr: "[::1]:9848", want: false},
		{name: "localhost name is safe", addr: "localhost:9848", want: false},
		{name: "unparseable addr is not flagged", addr: "9848", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isExposedBind(tc.addr); got != tc.want {
				t.Errorf("isExposedBind(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}
