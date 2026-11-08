package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestToolsRuntimeCloseJoinsTheBootWaiter: close returns only after the boot
// waiter has recorded its verdict, so nothing it does lands after close.
func TestToolsRuntimeCloseJoinsTheBootWaiter(t *testing.T) {
	dir := t.TempDir()
	manifest := `{"version":2,"tools":{"sleepytool":{"source":"manual","version":"1.0.0",` +
		`"probe":"sleepytool","install":"sleep 300"}}}`
	if err := os.WriteFile(filepath.Join(dir, "tools.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	rt := startTools(t.Context(), &baseTools{configDir: dir, catalogPath: filepath.Join(dir, "absent-catalog.json")})
	if rt.engine == nil {
		t.Fatal("engine is nil for an existing config dir")
	}
	if !rt.syncing() {
		t.Fatal("the boot reconcile of a 300 s install is not running; the test has no waiter to join")
	}
	rt.close()
	if rt.syncing() {
		t.Error("the boot verdict is still syncing right after close: the boot waiter was still running")
	}
}
