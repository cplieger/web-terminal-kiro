package main

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/cplieger/go/slogx/capture"
	"github.com/cplieger/toolbelt/v3"
)

// engineJudgedRoots is every directory toolbelt's VerifyRootIntegrity judges, as
// paths relative to the tools root ("" is the root itself), from toolbelt
// docs/configuration.md "Root integrity check" at the pinned version.
var engineJudgedRoots = []string{
	"", "bin", "opt", "npm", "npm/bin", "npm/lib", "npm/lib/node_modules",
	"python", "python/bin", "python/tools", "python/interpreters",
}

var securedToolsDirLine = regexp.MustCompile(`(?m)^secure_tools_dir "\$TOOLS(/[^"]*)?"`)

// entrypointSecuredToolsDirs returns the directories entrypoint.sh's boot pass
// tightens with secure_tools_dir under $TOOLS, relative to it.
func entrypointSecuredToolsDirs(t *testing.T) []string {
	t.Helper()
	script, err := os.ReadFile("entrypoint.sh")
	if err != nil {
		t.Fatalf("read entrypoint.sh: %v", err)
	}
	var rels []string
	for _, m := range securedToolsDirLine.FindAllStringSubmatch(string(script), -1) {
		rels = append(rels, filepath.Clean("."+m[1]))
	}
	if len(rels) == 0 {
		t.Fatal("no secure_tools_dir \"$TOOLS...\" call parsed out of entrypoint.sh")
	}
	return rels
}

func refusedRoots(t *testing.T, root string, err error) []string {
	t.Helper()
	unfit, ok := errors.AsType[*toolbelt.RootIntegrityError](err)
	if !ok {
		t.Fatalf("toolbelt.New error = %v, want a *toolbelt.RootIntegrityError", err)
	}
	var rels []string
	for _, f := range unfit.Findings {
		rel, relErr := filepath.Rel(root, f.Path)
		if relErr != nil {
			t.Fatalf("finding %q is not under %s: %v", f.Path, root, relErr)
		}
		if !slices.Contains(rels, rel) {
			rels = append(rels, rel)
		}
	}
	slices.Sort(rels)
	return rels
}

// TestEntrypointSecuresEveryRootTheEngineJudges holds entrypoint.sh to the
// engine it boots: on a volume where every judged directory is group-writable,
// the boot's `chmod go-w` over its secure_tools_dir list must leave nothing the
// engine refuses, or the server comes up with no tools engine. Serial: the
// engine logs its refusal to the captured default logger.
func TestEntrypointSecuresEveryRootTheEngineJudges(t *testing.T) {
	capture.Default(t)
	root := filepath.Join(t.TempDir(), "tools")
	for _, rel := range engineJudgedRoots {
		dir := filepath.Join(root, rel)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
		// Chmod, not the MkdirAll perm, which the umask filters.
		if err := os.Chmod(dir, 0o775); err != nil {
			t.Fatalf("loosen %s: %v", dir, err)
		}
	}
	cfg := toolsConfig(&baseTools{catalogPath: filepath.Join(t.TempDir(), "absent-catalog.json")}, root, nil)

	// Precondition: the engine refuses every planted directory, so the second
	// half cannot pass over one it never looks at. A directory a newer toolbelt
	// adds is not planted, so a bump re-reads that docs section.
	_, err := toolbelt.New(cfg)
	want := slices.Clone(engineJudgedRoots)
	want[0] = "."
	slices.Sort(want)
	if got := refusedRoots(t, root, err); !slices.Equal(got, want) {
		t.Fatalf("over a fully group-writable tree the engine refused %q, want every planted %q; update engineJudgedRoots and entrypoint.sh together", got, want)
	}

	for _, rel := range entrypointSecuredToolsDirs(t) {
		dir := filepath.Join(root, rel)
		if _, statErr := os.Lstat(dir); errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatalf("tighten %s: %v", dir, err)
		}
	}
	eng, err := toolbelt.New(cfg)
	if err != nil {
		if _, ok := errors.AsType[*toolbelt.RootIntegrityError](err); ok {
			t.Fatalf("after the entrypoint's tightening the engine still refuses %q; add them to entrypoint.sh's make_config_dir and secure_tools_dir lists", refusedRoots(t, root, err))
		}
		t.Fatalf("toolbelt.New after tightening: %v", err)
	}
	eng.Close()
}
