package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// imageToolsPath is the runtime image PATH's /config/tools segments (the last
// ENV PATH line, the builder stages set their own), in order, moved under tools.
// The others are dropped so a kiro-cli on the host cannot answer.
func imageToolsPath(t *testing.T, tools string) string {
	t.Helper()
	raw, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	all := regexp.MustCompile(`(?m)^ENV PATH="([^"]+)"$`).FindAllSubmatch(raw, -1)
	if all == nil {
		t.Fatal("no ENV PATH line in the Dockerfile")
	}
	var dirs []string
	for dir := range strings.SplitSeq(string(all[len(all)-1][1]), ":") {
		if rest, ok := strings.CutPrefix(dir, "/config/tools/"); ok {
			dirs = append(dirs, filepath.Join(tools, rest))
		}
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}

// TestToolbeltKiroCLIKeepsThePinnedBareName: with toolbelt's own bin/kiro-cli
// on the volume, a `docker exec` shell's bare kiro-cli still resolves to the
// pinned binary, and toolbelt's link is left as it is.
func TestToolbeltKiroCLIKeepsThePinnedBareName(t *testing.T) {
	env := newNSEnv(t)
	survivors, _ := env.plantToolbeltKiroCLI(nsTool)
	own := env.plantOwnVersion()
	if err := env.manager().Ensure(t.Context()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	env.assertIntact(survivors)

	t.Setenv("PATH", imageToolsPath(t, env.tools))
	found, err := exec.LookPath(nsTool)
	if err != nil {
		t.Fatalf("kiro-cli does not resolve on the image PATH %q: %v", os.Getenv("PATH"), err)
	}
	got, err := filepath.EvalSymlinks(found)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", found, err)
	}
	if want := filepath.Join(own, nsTool); got != want {
		t.Errorf("bare kiro-cli resolves through %s to %s, want the pinned %s", found, got, want)
	}
}

func TestRetractFormerKiroLink(t *testing.T) {
	cases := map[string]struct {
		plant    func(e *nsEnv, link string)
		wantKept bool
	}{
		"the manager's former link into a version dir is removed": {
			plant: func(e *nsEnv, link string) {
				e.symlink(filepath.Join(e.tools, nsTool+"-versions", nsVersion, nsTool), link)
			},
		},
		"toolbelt's link into opt is kept": {
			plant: func(e *nsEnv, link string) {
				e.symlink(filepath.Join(e.tools, "opt", nsTool, nsVersion, nsTool), link)
			},
			wantKept: true,
		},
		"a link climbing out through .. is kept": {
			plant: func(e *nsEnv, link string) {
				// Concatenated, not filepath.Join, which would clean the ".." away.
				target := e.tools + "/" + nsTool + "-versions/" + nsVersion + "/../" + nsVersion + "/" + nsTool
				if err := os.Symlink(target, link); err != nil {
					e.t.Fatalf("Symlink: %v", err)
				}
			},
			wantKept: true,
		},
		"a link to another name in a version dir is kept": {
			plant: func(e *nsEnv, link string) {
				e.symlink(filepath.Join(e.tools, nsTool+"-versions", nsVersion, nsTool+"-chat"), link)
			},
			wantKept: true,
		},
		"a regular file is kept": {
			plant:    func(e *nsEnv, link string) { e.writeFile(link, "dispatcher\n") },
			wantKept: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := newNSEnv(t)
			link := filepath.Join(env.tools, toolbeltBinDir, nsTool)
			if err := os.MkdirAll(filepath.Dir(link), 0o750); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			tc.plant(env, link)
			retractFormerKiroLink(env.tools)
			_, err := os.Lstat(link)
			if kept := err == nil; kept != tc.wantKept {
				t.Errorf("after the retract, %s present = %v, want %v", link, kept, tc.wantKept)
			}
		})
	}
	t.Run("a missing tools dir is a no-op", func(t *testing.T) {
		retractFormerKiroLink(filepath.Join(t.TempDir(), "absent"))
	})
}
