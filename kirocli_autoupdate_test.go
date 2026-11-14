package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// emptyEnv empties the process environment for the rest of t, as `env -i` does for a child, and
// restores it at cleanup. t.Setenv refuses a parallel t, so no parallel test sees the empty env.
func emptyEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		t.Setenv(name, value)
	}
	os.Clearenv()
}

// Every runtime shape's session process starts with kiro-cli's self-updater off, under a parent
// whose environment is empty.
func TestEverySessionShapeSpawnsKiroCLIWithSelfUpdateOff(t *testing.T) {
	fixture := newNSEnv(t)
	fixture.plantOwnVersion()
	managed := startKiroCLI(&baseKiro{
		version:     nsVersion,
		sha256:      strings.Repeat("a", 64),
		sha256ARM64: strings.Repeat("b", 64),
		toolsDir:    fixture.tools,
	})
	t.Cleanup(managed.stop)
	shapes := map[string]kiroRuntime{
		"unmanaged":   startKiroCLI(&baseKiro{}),
		"unavailable": unavailableKiroRuntime(),
		"managed":     managed,
	}
	emptyEnv(t)
	for name, rt := range shapes {
		t.Run(name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "seen")
			deps := newTestDeps(true)
			deps.sessionEnv = rt.env
			deps.cmd = staticCmd("/bin/sh", "-c", `printf '%s\n' "${KIRO_NO_AUTO_UPDATE-unset}" > '`+marker+`'; exec /bin/cat`)
			mustStartSession(t, deps)
			got := strings.TrimSpace(string(readMarkerWithin(t, marker, 2, "record KIRO_NO_AUTO_UPDATE")))
			if got != "1" {
				t.Errorf("the session process saw KIRO_NO_AUTO_UPDATE %q, want 1", got)
			}
		})
	}
}

// The install's version probe and settings assertions run the binary before app.disableAutoupdates
// is set, so each carries KIRO_NO_AUTO_UPDATE=1, under an empty parent environment too.
func TestKiroInstallRunsEveryCommandWithSelfUpdateOff(t *testing.T) {
	fixture := newNSEnv(t)
	dir := fixture.plantOwnVersion()
	log := filepath.Join(t.TempDir(), "runs.log")
	writeExecutable(t, filepath.Join(dir, nsTool), []byte("#!/bin/sh\nprintf '%s %s\\n' \"$1\" \"${KIRO_NO_AUTO_UPDATE-unset}\" >>'"+log+"'\n"+
		"[ \"$1\" = --version ] && printf 'kiro-cli "+nsVersion+"\\n'\nexit 0\n"))
	emptyEnv(t)
	mgr := fixture.manager()
	if err := mgr.Ensure(t.Context()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if ok, err := mgr.Rescan(t.Context()); !ok || err != nil {
		t.Fatalf("Rescan = (%v, %v), want (true, nil)", ok, err)
	}

	data, err := os.ReadFile(log) // #nosec G304 -- test-owned temp path
	if err != nil {
		t.Fatalf("the fake kiro-cli never ran: %v", err)
	}
	seen := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		verb, value, _ := strings.Cut(line, " ")
		seen[verb] = true
		if value != "1" {
			t.Errorf("kiro-cli %s saw KIRO_NO_AUTO_UPDATE %q, want 1", verb, value)
		}
	}
	for _, verb := range []string{"--version", "settings"} {
		if !seen[verb] {
			t.Errorf("the install never ran kiro-cli %s; runs: %q", verb, data)
		}
	}
}
