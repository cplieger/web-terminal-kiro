package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/slogx/capture"
	"github.com/cplieger/toolbelt/v2"
)

// countLevel reports how many captured records at exactly the given level
// carry sub in their message. capture.Recorder records EVERY level (its
// Enabled always returns true), so a level-blind Contains would keep passing
// if a production Warn/Error were silently demoted to Debug; counting at the
// asserted level keeps these tests level-sensitive like the old
// HandlerOptions{Level}-threshold handler was.
func countLevel(records *capture.Recorder, level slog.Level, sub string) int {
	n := 0
	for _, r := range records.Records() {
		if r.Level == level && strings.Contains(r.Message, sub) {
			n++
		}
	}
	return n
}

// fakeCLI writes an executable shell stub standing in for kiro-cli. Its whoami
// exits with whoamiRC (mirroring the real binary: 0 logged in, 1 not); login
// records its argv to a marker file and succeeds; chat records its argv
// (newline-separated, so a space inside one arg is distinguishable from two
// args) and prints a sentinel. The stub lets the sessionCommand wrapper be
// executed for real, so the guard's actual runtime behavior is pinned, not
// just the script text.
func fakeCLI(t *testing.T, dir string, whoamiRC int) (cliPath, loginMarker, chatMarker string) {
	t.Helper()
	cliPath = filepath.Join(dir, "fake kiro-cli") // space: pins the $0 quoting
	loginMarker = filepath.Join(dir, "login-args")
	chatMarker = filepath.Join(dir, "chat-args")
	stub := `#!/bin/sh
case "$1" in
whoami) exit ` + strconv.Itoa(whoamiRC) + ` ;;
login) shift; printf '%s' "$*" > ` + "'" + loginMarker + "'" + `; exit 0 ;;
chat) shift; printf '%s\n' "$@" > ` + "'" + chatMarker + "'" + `; echo CHAT_STARTED ;;
esac
`
	if err := os.WriteFile(cliPath, []byte(stub), 0o755); err != nil { // #nosec G306 -- test stub must be executable
		t.Fatalf("write fake cli: %v", err)
	}
	return cliPath, loginMarker, chatMarker
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
			cliPath, loginMarker, _ := fakeCLI(t, dir, tc.whoamiRC)

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

// TestSessionCommand_extraChatArgs pins the KIRO_CLI_CHAT_ARGS contract: extra
// launch flags (e.g. --v3) are appended to the chat invocation as separate,
// LITERAL argv entries — an arg carrying shell metacharacters or spaces must
// arrive verbatim (positional-param passing, not string splicing into the
// script) — and they never leak into the login call. Without extra args, chat
// runs with an empty argv tail (no stray empty-string argument).
func TestSessionCommand_extraChatArgs(t *testing.T) {
	t.Run("args reach chat verbatim, login unaffected", func(t *testing.T) {
		dir := t.TempDir()
		cliPath, loginMarker, chatMarker := fakeCLI(t, dir, 1) // logged out: login runs too

		injection := `$(touch ` + filepath.Join(dir, "pwned") + `); two words`
		argv := sessionCommand(cliPath, "--v3", "--effort", "high", injection)
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput() // #nosec G204 -- test executes its own wrapper
		if err != nil {
			t.Fatalf("wrapper run: %v\noutput: %s", err, out)
		}

		got, readErr := os.ReadFile(chatMarker) // #nosec G304 -- test-owned temp path
		if readErr != nil {
			t.Fatalf("chat was not invoked (marker missing): %v", readErr)
		}
		want := "--v3\n--effort\nhigh\n" + injection + "\n"
		if string(got) != want {
			t.Errorf("chat argv = %q, want %q (args must pass as literal positional params)", got, want)
		}

		login, readErr := os.ReadFile(loginMarker) // #nosec G304 -- test-owned temp path
		if readErr != nil {
			t.Fatalf("login was not invoked (marker missing): %v", readErr)
		}
		if string(login) != "--use-device-flow" {
			t.Errorf("login args = %q, want %q (chat args must not leak into login)", login, "--use-device-flow")
		}
		if _, statErr := os.Stat(filepath.Join(dir, "pwned")); statErr == nil {
			t.Error("injection canary fired: a chat arg was shell-evaluated instead of passed literally")
		}
	})

	t.Run("no args: chat argv tail is empty", func(t *testing.T) {
		dir := t.TempDir()
		cliPath, _, chatMarker := fakeCLI(t, dir, 0)

		argv := sessionCommand(cliPath)
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput() // #nosec G204 -- test executes its own wrapper
		if err != nil {
			t.Fatalf("wrapper run: %v\noutput: %s", err, out)
		}
		got, readErr := os.ReadFile(chatMarker) // #nosec G304 -- test-owned temp path
		if readErr != nil {
			t.Fatalf("chat was not invoked (marker missing): %v", readErr)
		}
		// `printf '%s\n' "$@"` with zero params still prints one empty line;
		// anything beyond that means a stray argument reached chat.
		if string(got) != "\n" {
			t.Errorf("chat argv tail = %q, want none (a stray empty arg would become kiro-cli's [INPUT])", got)
		}
	})
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
		{name: "localhost name is case-insensitive", addr: "LOCALHOST:9848", want: false},
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

// TestStartTools_configDirMissing pins the out-of-container shape: a missing
// config dir disables the tools engine (bare `go run` / tests), returning the
// zero toolsRuntime whose nil funcs make registerRoutes skip /api/tools and
// the health tools field, with a Warn naming the fix. close() on the zero
// value must be a safe no-op. This test mutates the process-global default
// logger, so it runs serially (no t.Parallel).
func TestStartTools_configDirMissing(t *testing.T) {
	records := capture.Default(t)

	rt := startTools(baseTools{
		configDir:   filepath.Join(t.TempDir(), "absent"),
		catalogPath: filepath.Join(t.TempDir(), "absent-catalog.json"),
	})

	if rt.engine != nil {
		t.Fatal("engine is non-nil for a missing config dir; want the zero runtime (no tools surface outside the container)")
	}
	if rt.syncing != nil || rt.state != nil {
		t.Error("syncing/state funcs are non-nil; registerRoutes keys the /api/tools mount and the health tools field on nil")
	}
	rt.close() // zero-runtime close must not panic
	if got := countLevel(records, slog.LevelWarn, "tools engine disabled"); got != 1 {
		t.Errorf("log = %q, want exactly one config-dir-missing Warn (got %d)", records.Messages(), got)
	}
}

// TestStartTools_engineStartFailure pins degraded-not-dead: a config dir whose
// tools.json is the retired v1 format fails toolbelt.New (strict v2 schema),
// and startTools logs the Error and continues with the zero runtime instead of
// taking the server down. Serial: mutates the global default logger.
func TestStartTools_engineStartFailure(t *testing.T) {
	records := capture.Default(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tools.json"),
		[]byte(`{"runtimes":{"node":{"enabled":false}}}`), 0o644); err != nil {
		t.Fatalf("write retired manifest: %v", err)
	}

	rt := startTools(baseTools{configDir: dir, catalogPath: filepath.Join(dir, "absent-catalog.json")})

	if rt.engine != nil {
		t.Fatal("engine is non-nil despite a failed toolbelt.New; want the zero runtime (degraded-not-dead)")
	}
	rt.close()
	if got := countLevel(records, slog.LevelError, "tools engine failed to start"); got != 1 {
		t.Errorf("log = %q, want exactly one failed-to-start Error (got %d)", records.Messages(), got)
	}
}

// TestStartTools_bootConvergenceLiftsGate pins the bind-first boot contract on
// the happy path: with a real (empty) config dir the engine seeds the default
// all-disabled manifest, the boot reconcile has nothing to install, and the
// syncing gate LIFTS with verdict "ok" -- the property that keeps session
// creation from answering 503 "tools installing" forever. All seeded entries
// are disabled, so the pass is offline and fast; the poll is a bounded
// eventually-check on the atomic-backed funcs (race-free).
func TestStartTools_bootConvergenceLiftsGate(t *testing.T) {
	dir := t.TempDir()
	rt := startTools(baseTools{configDir: dir, catalogPath: filepath.Join(dir, "absent-catalog.json")})
	if rt.engine == nil {
		t.Fatal("engine is nil for an existing config dir; want a running tools engine")
	}
	t.Cleanup(rt.close)

	deadline := time.Now().Add(10 * time.Second)
	for rt.syncing() {
		if time.Now().After(deadline) {
			t.Fatal("boot convergence gate never lifted; session creation would 503 forever")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := rt.state(); got != "ok" {
		t.Errorf("state after convergence = %q, want %q (all seeded templates are disabled: nothing to install)", got, "ok")
	}
}

// TestHostAllowlist pins the KWEB_ALLOWED_HOSTS anti-DNS-rebinding gate
// through the real middleware stack (buildHandler): a rebinding attack makes
// an attacker-controlled hostname resolve to this server, so Origin and Host
// AGREE and CrossOriginProtection alone admits both session creation and the
// /ws upgrade — the exact-host allowlist must reject those requests BEFORE
// the terminal routes, while an explicitly allowed Host still reaches them.
// Also pins canonicalization (port/case/trailing dot/IPv6 spelling), that
// X-Forwarded-Host cannot bypass the check, that the cross-origin guard still
// runs AFTER an allowed host, and that an unset allowlist stays permissive
// (backward compatible).
func TestHostAllowlist(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/sessions", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated) // stands in for REST session creation
	})
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // stands in for the WebSocket upgrade route
	})
	do := func(h http.Handler, method, url, origin, xfh string) int {
		req := httptest.NewRequest(method, url, nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if xfh != "" {
			req.Header.Set("X-Forwarded-Host", xfh)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	t.Setenv("KWEB_ALLOWED_HOSTS", "localhost, 192.168.1.5, ::1, Webterm.Example.COM.")
	h := buildHandler(mux, nil, "default-src 'self'", parseAllowedHosts())

	cases := []struct {
		name        string
		method, url string
		origin, xfh string
		want        int
	}{
		{
			name:   "rebound host + matching Origin: session creation rejected",
			method: "POST", url: "http://attacker.evil:9848/api/sessions",
			origin: "http://attacker.evil:9848", want: http.StatusForbidden,
		},
		{
			name:   "rebound host: ws upgrade rejected",
			method: "GET", url: "http://attacker.evil:9848/ws", want: http.StatusForbidden,
		},
		{
			name:   "X-Forwarded-Host cannot smuggle an allowed name",
			method: "GET", url: "http://attacker.evil:9848/ws",
			xfh: "localhost", want: http.StatusForbidden,
		},
		{
			name:   "allowed host: session creation passes",
			method: "POST", url: "http://localhost:9848/api/sessions",
			origin: "http://localhost:9848", want: http.StatusCreated,
		},
		{
			name:   "allowed host: ws upgrade passes",
			method: "GET", url: "http://localhost:9848/ws", want: http.StatusOK,
		},
		{
			name:   "allowed IP passes",
			method: "GET", url: "http://192.168.1.5:9848/ws", want: http.StatusOK,
		},
		{
			name:   "case + trailing dot + port canonicalize",
			method: "GET", url: "http://WEBTERM.example.com:1234/ws", want: http.StatusOK,
		},
		{
			name:   "IPv6 spelling canonicalizes",
			method: "GET", url: "http://[0:0:0:0:0:0:0:1]:9848/ws", want: http.StatusOK,
		},
		{
			name:   "allowed host but cross-origin POST still rejected",
			method: "POST", url: "http://localhost:9848/api/sessions",
			origin: "http://attacker.evil", want: http.StatusForbidden,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := do(h, tc.method, tc.url, tc.origin, tc.xfh); got != tc.want {
				t.Errorf("%s %s = %d, want %d", tc.method, tc.url, got, tc.want)
			}
		})
	}

	t.Run("unset allowlist stays permissive", func(t *testing.T) {
		open := buildHandler(mux, nil, "default-src 'self'", nil)
		if got := do(open, "GET", "http://anything.example:9848/ws", "", ""); got != http.StatusOK {
			t.Errorf("GET /ws with nil allowlist = %d, want %d (unset KWEB_ALLOWED_HOSTS must stay backward compatible)", got, http.StatusOK)
		}
	})
}

// TestHostAllowlist_blankConfigurationStaysPermissive drives a configured but
// blank KWEB_ALLOWED_HOSTS (only commas and whitespace) through the real
// parseAllowedHosts into the middleware: the parser's blank-entry filtering
// plus the final empty-map-to-nil branch must yield the documented permissive
// state. Accidentally retaining an empty-string map key would turn a blank
// configuration into a deny-all outage.
func TestHostAllowlist_blankConfigurationStaysPermissive(t *testing.T) {
	t.Setenv("KWEB_ALLOWED_HOSTS", "  ,  , ")
	mux := http.NewServeMux()
	mux.HandleFunc("/probe", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	rec := httptest.NewRecorder()
	buildHandler(mux, nil, "default-src 'self'", parseAllowedHosts()).ServeHTTP(
		rec,
		httptest.NewRequest(http.MethodGet, "http://anything.example:9848/probe", http.NoBody),
	)

	if rec.Code != http.StatusNoContent {
		t.Errorf("blank KWEB_ALLOWED_HOSTS: GET /probe status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

// TestStartTools_reconcileFailureLiftsGateDegraded pins the degraded-not-dead
// contract on the FAILURE path, which the happy-path convergence test cannot
// reach: a manifest with an enabled tool the (absent) catalog cannot resolve
// makes the boot reconcile job finish failed, and the syncing gate must STILL
// lift — with verdict "degraded" — so session creation never answers 503
// "tools installing" forever after a broken install. The install failure is
// local (no catalog knowledge), so the test is offline and fast.
func TestStartTools_reconcileFailureLiftsGateDegraded(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tools.json"),
		[]byte(`{"version":2,"tools":{"no-such-tool-xyz":{}}}`), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	rt := startTools(baseTools{configDir: dir, catalogPath: filepath.Join(dir, "absent-catalog.json")})
	if rt.engine == nil {
		t.Fatal("engine is nil for an existing config dir; want a running tools engine")
	}
	t.Cleanup(rt.close)

	deadline := time.Now().Add(10 * time.Second)
	for rt.syncing() {
		if time.Now().After(deadline) {
			t.Fatal("boot convergence gate never lifted after a failed reconcile; session creation would 503 forever")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := rt.state(); got != "degraded" {
		t.Errorf("state after failed reconcile = %q, want %q (a failed install must degrade, not stay syncing or report ok)", got, "degraded")
	}
}

// TestStartTools_emptyManifestSkipsGate pins the job==nil short-circuit: a
// pre-existing EMPTY manifest gives the boot reconcile nothing to converge
// (Reconcile returns a nil job), so the gate must never engage and the verdict
// is immediately "ok" — session creation is never blocked on a no-op pass.
func TestStartTools_emptyManifestSkipsGate(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tools.json"),
		[]byte(`{"version":2,"tools":{}}`), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	rt := startTools(baseTools{configDir: dir, catalogPath: filepath.Join(dir, "absent-catalog.json")})
	if rt.engine == nil {
		t.Fatal("engine is nil for an existing config dir; want a running tools engine")
	}
	t.Cleanup(rt.close)

	if rt.syncing() {
		t.Error("syncing gate engaged for an empty manifest; want an immediate no-op (nothing to converge)")
	}
	if got := rt.state(); got != "ok" {
		t.Errorf("state for an empty manifest = %q, want %q", got, "ok")
	}
}

// TestWarnIfNoLSPEnabled pins both silent branches of the code-intelligence
// nudge, which the boot-convergence path only exercises on the warning side:
// an ENABLED catalog-marked language server must silence the Warn (the whole
// point of the Lsp inventory marker), and an inventory read failure must skip
// the nudge quietly (Debug only) instead of warning spuriously. Serial:
// mutates the process-global default logger.
func TestWarnIfNoLSPEnabled(t *testing.T) {
	const warnMsg = "no language servers enabled"

	newEngine := func(t *testing.T, manifest, catalog string) (*toolbelt.Engine, string) {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "tools.json"), []byte(manifest), 0o644); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
		catalogPath := filepath.Join(dir, "catalog.json")
		if catalog == "" {
			catalogPath = filepath.Join(dir, "absent-catalog.json")
		} else if err := os.WriteFile(catalogPath, []byte(catalog), 0o644); err != nil {
			t.Fatalf("write catalog: %v", err)
		}
		eng, err := toolbelt.New(&toolbelt.Config{
			ConfigDir:   dir,
			ToolsDir:    filepath.Join(dir, "tools"),
			CatalogPath: catalogPath,
		})
		if err != nil {
			t.Fatalf("toolbelt.New: %v", err)
		}
		t.Cleanup(eng.Close)
		return eng, dir
	}
	t.Run("enabled catalog-marked LSP silences the warn", func(t *testing.T) {
		eng, _ := newEngine(t,
			`{"version":2,"tools":{"gopls":{}}}`,
			`{"entries":{"gopls":{"name":"gopls","source":"go:golang.org/x/tools/gopls","lsp":true}}}`)
		records := capture.Default(t)
		warnIfNoLSPEnabled(eng)
		if got := countLevel(records, slog.LevelWarn, warnMsg); got != 0 {
			t.Errorf("log = %q; an enabled Lsp-marked tool must silence the nudge (got %d Warns)", records.Messages(), got)
		}
	})

	t.Run("no enabled LSP warns", func(t *testing.T) {
		// gopls present but disabled (a template), so the nudge must fire.
		eng, _ := newEngine(t,
			`{"version":2,"tools":{"gopls":{"disabled":true}}}`,
			`{"entries":{"gopls":{"name":"gopls","source":"go:golang.org/x/tools/gopls","lsp":true}}}`)
		records := capture.Default(t)
		warnIfNoLSPEnabled(eng)
		if got := countLevel(records, slog.LevelWarn, warnMsg); got != 1 {
			t.Errorf("log = %q, want exactly one %q Warn (no enabled language server; got %d)", records.Messages(), warnMsg, got)
		}
	})

	t.Run("inventory failure skips the nudge quietly", func(t *testing.T) {
		eng, dir := newEngine(t, `{"version":2,"tools":{}}`, "")
		// Corrupt the manifest AFTER engine start: Inventory re-reads it from
		// disk, so the read now fails and the nudge must be skipped (Debug
		// only, no Warn).
		if err := os.WriteFile(filepath.Join(dir, "tools.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatalf("corrupt manifest: %v", err)
		}
		records := capture.Default(t)
		warnIfNoLSPEnabled(eng)
		if got := countLevel(records, slog.LevelWarn, warnMsg); got != 0 {
			t.Errorf("log = %q; an inventory failure must not produce the LSP Warn (got %d)", records.Messages(), got)
		}
	})
}
