package main

// toolbelt owns bin/, opt/, npm/ and python/; the kiro-cli install manager
// owns kiro-cli-versions/ and kiro-cli-bin/, which leads the image PATH. These
// tests plant a toolbelt tool literally named kiro-cli (its name validator
// accepts it, and /api/tools is reachable from kiro-cli's `!` shell escape) and
// build the manager from kiroInstallConfig, the configuration production runs,
// so they pin that THIS APP's values keep the two engines apart.

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/go/pathinside"
	"github.com/cplieger/toolbelt/v3/pin"
)

// The pin these tests install, and the toolbelt tool name that collides with it.
const (
	nsVersion = "9.9.9"
	nsTool    = "kiro-cli"
)

// nsToolbeltBody is the content of every regular file the fake toolbelt
// footprint owns, so a survivor check can tell "still there" from "replaced
// by our own file at the same path".
const nsToolbeltBody = "toolbelt-owned\n"

// nsEnv is one planted volume: the tools dir, and the witness path the foreign
// dispatcher records its own execution in.
type nsEnv struct {
	t       *testing.T
	tools   string
	witness string
}

func newNSEnv(t *testing.T) *nsEnv {
	t.Helper()
	tools := t.TempDir()
	return &nsEnv{t: t, tools: tools, witness: filepath.Join(tools, "foreign-was-run")}
}

// config is this app's real install configuration for this volume.
func (e *nsEnv) config() *pin.Config {
	return kiroInstallConfig(&baseKiro{
		version: nsVersion,
		// Any well-formed digest: a volume that already holds the complete pin
		// downloads nothing, so no archive is ever fetched to verify.
		sha256:      strings.Repeat("a", 64),
		sha256ARM64: strings.Repeat("b", 64),
		toolsDir:    e.tools,
	})
}

// manager builds a manager from that configuration, as startKiroCLI does.
func (e *nsEnv) manager() *pin.Manager {
	e.t.Helper()
	mgr, err := pin.New(e.config())
	if err != nil {
		e.t.Fatalf("pin.New from the app's own config: %v", err)
	}
	return mgr
}

// plantToolbeltKiroCLI plants toolbelt's footprint for a tool named kiro-cli:
// opt/<name>/<version>/ with no `.complete` sentinel (to a manager rooted
// there it reads as an incomplete install the partial sweep deletes), and a
// bin/<name> symlink for each name in linked. The primary records its own
// execution, so "never read" is observable. It returns every path that must
// survive untouched, plus the tree directory.
func (e *nsEnv) plantToolbeltKiroCLI(linked ...string) (survivors []string, tree string) {
	e.t.Helper()
	tree = filepath.Join(e.tools, "opt", nsTool, nsVersion)
	binDir := filepath.Join(e.tools, toolbeltBinDir)
	for _, dir := range []string{tree, binDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			e.t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	// The engine's own PATH entry for an unrelated tool, so a sweep that walks the
	// toolbelt bin dir instead of naming its targets is caught too.
	survivors = append(survivors, e.symlink(filepath.Join(e.tools, "opt", "gopls", "1.0.0", "gopls"), filepath.Join(binDir, "gopls")))
	for _, name := range []string{nsTool, nsTool + "-chat", nsTool + "-term"} {
		target := filepath.Join(tree, name)
		if err := os.WriteFile(target, []byte(nsToolbeltBody), 0o600); err != nil {
			e.t.Fatalf("WriteFile(%s): %v", target, err)
		}
		survivors = append(survivors, target)
	}
	// The primary is executable AND self-reporting, so it is a viable probe
	// candidate rather than one excluded for being non-executable.
	foreign := filepath.Join(tree, nsTool)
	e.writeScript(foreign, "printf 'was-run\\n' >>"+nsShellQuote(e.witness)+"\nprintf 'kiro-cli "+nsVersion+"\\n'\n")
	for _, name := range linked {
		survivors = append(survivors, e.symlink(filepath.Join(tree, name), filepath.Join(binDir, name)))
	}
	return survivors, tree
}

// plantOwnVersion writes a COMPLETE version directory under the app's own
// install root, standing in for an install a previous boot finished -- the
// ordinary restart path, and the only way to drive this wiring with no
// download.
//
// It writes kiro-cli alone, the whole of this app's required set since
// kiro-cli 3.0.0 ships one binary.
func (e *nsEnv) plantOwnVersion() string {
	e.t.Helper()
	dir := filepath.Join(e.tools, nsTool+"-versions", nsVersion)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		e.t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
	// Answers --version with the directory's own name (what selection probes)
	// and exits 0 for every settings assertion.
	e.writeScript(filepath.Join(dir, nsTool), "case \"$1\" in --version) printf 'kiro-cli "+nsVersion+"\\n' ;; esac\n")
	// Written LAST, exactly as the install order requires: it is the sentinel that
	// makes the directory a selection candidate at all.
	if err := os.WriteFile(filepath.Join(dir, ".complete"), []byte(nsVersion+"\n"), 0o600); err != nil {
		e.t.Fatalf("write sentinel: %v", err)
	}
	return dir
}

// plantLegacyResidue plants the genuine shell-era residue THIS APP's own
// installer left, so a sweep assertion cannot pass by doing nothing. Unlike
// marotte, this app's installer promoted in place, so its residue includes
// the update journal, the `.prev` hard-link backups with their `.absent`
// tombstones, and both install markers -- every fixed-path artifact
// kiroLegacyPurge names but the dispatchers -- plus an orphan staging tree and
// each dispatcher the caller names in toolbelt's bin dir.
func (e *nsEnv) plantLegacyResidue(dispatchers ...string) []string {
	e.t.Helper()
	planted := make([]string, 0, len(e.config().Purge.Artifacts)+len(dispatchers)+1)
	for _, rel := range e.config().Purge.Artifacts {
		if slices.Contains(kiroLegacyDispatchers(), rel) {
			continue
		}
		planted = append(planted, e.writeFile(filepath.Join(e.tools, rel), "shell-era artifact\n"))
	}
	for _, name := range dispatchers {
		planted = append(planted, e.writeFile(filepath.Join(e.tools, toolbeltBinDir, name), "shell-era dispatcher\n"))
	}
	stage := filepath.Join(e.tools, legacyStagePrefix+"abc123")
	if err := os.MkdirAll(stage, 0o750); err != nil {
		e.t.Fatalf("MkdirAll(%s): %v", stage, err)
	}
	return append(planted, stage)
}

func (e *nsEnv) writeFile(path, body string) string {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		e.t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		e.t.Fatalf("WriteFile(%s): %v", path, err)
	}
	return path
}

func (e *nsEnv) writeScript(path, body string) {
	e.t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"exit 0\n"), 0o700); err != nil { // #nosec G306 -- a dispatcher fake must be executable
		e.t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func (e *nsEnv) symlink(target, newname string) string {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		e.t.Fatalf("MkdirAll(%s): %v", filepath.Dir(target), err)
	}
	if err := os.Symlink(target, newname); err != nil {
		e.t.Fatalf("Symlink(%s -> %s): %v", newname, target, err)
	}
	return newname
}

// assertIntact checks that every planted path is not merely PRESENT but
// unchanged: a symlink still points where the engine pointed it, and a
// regular file still holds the engine's bytes. Presence alone is not
// enough -- a publish removes a colliding version directory and renames its
// own tree into the same place, so the paths reappear holding our files.
//
// It also asserts the witness is absent, which is the "never READ" half:
// the foreign primary appends to it when executed.
func (e *nsEnv) assertIntact(survivors []string) {
	e.t.Helper()
	optTree := filepath.Join(e.tools, "opt")
	for _, p := range survivors {
		fi, err := os.Lstat(p)
		if err != nil {
			e.t.Errorf("%s is gone: the install reached into the toolbelt engine's namespace", p)
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				e.t.Errorf("Readlink(%s): %v", p, err)
				continue
			}
			// pathinside.Root(...).Contains rather than a prefix test on the raw
			// string: the link target is read off disk and need not be cleaned,
			// so "<opt>/../elsewhere" carries the prefix while pointing outside
			// the tree.
			if !pathinside.Root(optTree).Contains(target) {
				e.t.Errorf("%s now points at %q, outside the engine's own opt tree: its symlink was republished under it", p, target)
			}
			continue
		}
		raw, err := os.ReadFile(p) // #nosec G304 -- a path this test planted.
		if err != nil {
			e.t.Errorf("read %s: %v", p, err)
			continue
		}
		// The foreign primary is a script, so it is checked for still being one of
		// the two shapes this test wrote rather than for the marker body.
		if body := string(raw); body != nsToolbeltBody && !strings.HasPrefix(body, "#!/bin/sh") {
			e.t.Errorf("%s holds %q, neither of the shapes this test planted: it was removed and rewritten", p, body)
		}
	}
	if _, err := os.Lstat(e.witness); err == nil {
		e.t.Error("the manager EXECUTED the toolbelt-owned kiro-cli; another owner's files must never be a version candidate")
	}
}

// TestToolbeltKiroCLIFootprintSurvivesABoot: with toolbelt's kiro-cli on the
// volume, a full Ensure neither reads nor deletes any of it and activates its
// own version. The foreign tree carries the pinned version, so with
// overlapping roots it would be the pin's own directory.
func TestToolbeltKiroCLIFootprintSurvivesABoot(t *testing.T) {
	env := newNSEnv(t)
	survivors, tree := env.plantToolbeltKiroCLI(nsTool+"-chat", nsTool+"-term")
	own := env.plantOwnVersion()
	mgr := env.manager()

	if err := mgr.Ensure(t.Context()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	env.assertIntact(survivors)
	if ready, why := mgr.Ready(); !ready {
		t.Errorf("Ready() = false (%s), want true: the app's own install must activate regardless", why)
	}
	if got := mgr.PathEntry(); got != own {
		t.Errorf("PathEntry() = %q, want the app's own version directory %q", got, own)
	}
	if got := mgr.Path(); got != filepath.Join(own, nsTool) {
		t.Errorf("Path() = %q, want the dispatcher inside the app's own version directory", got)
	}
	if strings.HasPrefix(mgr.Path(), tree+string(filepath.Separator)) {
		t.Fatalf("Path() = %q resolves INSIDE the toolbelt-owned tree", mgr.Path())
	}
	// The primary name was left free, so the boot publishes its own link there: it
	// must point into the app's own install, never at the colliding tree.
	link := filepath.Join(env.tools, kiroLinkDir, nsTool)
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("the `docker exec … kiro-cli` convenience symlink was not published: %v", err)
	}
	if target != filepath.Join(own, nsTool) {
		t.Errorf("%s points at %q, want the active version's own dispatcher", link, target)
	}
}

// nsToolsRel names dir relative to the tools dir, refusing anything not under
// it. The escape test is pathinside.RelEscapes, since filepath.Rel returns
// "../evil" with a nil error for a target above its base.
func nsToolsRel(root, dir string) (string, error) {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return "", fmt.Errorf("%q cannot be named relative to %q: %w", dir, root, err)
	}
	if pathinside.RelEscapes(rel) {
		return rel, fmt.Errorf("%q is outside %q (relative name %q)", dir, root, rel)
	}
	return rel, nil
}

// TestNSToolsRelDetectsEscape pins the predicate the structural test below
// relies on. "../x" splits into two components like a legitimate name, so the
// depth check cannot stand in for the containment check.
func TestNSToolsRelDetectsEscape(t *testing.T) {
	root := t.TempDir()
	cases := map[string]struct {
		dir     string
		wantRel string
	}{
		"the shape this app's configuration produces": {
			dir:     filepath.Join(root, nsTool+"-versions", nsVersion),
			wantRel: filepath.Join(nsTool+"-versions", nsVersion),
		},
		"a sibling of the tools dir, two components up-and-over": {
			dir:     filepath.Join(filepath.Dir(root), "evil"),
			wantRel: "",
		},
		"a path that cannot be compared with the tools dir at all": {
			dir:     "relative/not/absolute",
			wantRel: "",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rel, err := nsToolsRel(root, tc.dir)
			if tc.wantRel == "" {
				if err == nil {
					t.Fatalf("nsToolsRel(%q, %q) = %q, nil: a directory outside the tools dir was accepted as under it", root, tc.dir, rel)
				}
				return
			}
			if err != nil {
				t.Fatalf("nsToolsRel(%q, %q) = %v, want %q", root, tc.dir, err, tc.wantRel)
			}
			if rel != tc.wantRel {
				t.Errorf("nsToolsRel(%q, %q) = %q, want %q", root, tc.dir, rel, tc.wantRel)
			}
		})
	}
	// The depth assertion the structural test makes on top of this predicate
	// cannot substitute for it: the escaping name has the same component
	// count as the legitimate one.
	escaping, err := filepath.Rel(root, filepath.Join(filepath.Dir(root), "evil"))
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	if parts := strings.Split(escaping, string(filepath.Separator)); len(parts) != 2 {
		t.Fatalf("the escaping relative name %q has %d components; the case that fooled the depth check had 2", escaping, len(parts))
	}
}

// TestInstallRootIsOutsideTheToolbeltNamespace: the install root is one
// component directly under the tools dir and none of toolbelt's four trees,
// so no tool name toolbelt accepts can contain or alias it. Derived from a
// live manager, since the root is the library's function of the release name.
func TestInstallRootIsOutsideTheToolbeltNamespace(t *testing.T) {
	env := newNSEnv(t)
	own := env.plantOwnVersion()
	mgr := env.manager()
	if err := mgr.Ensure(t.Context()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if mgr.PathEntry() != own {
		t.Fatalf("PathEntry() = %q, want %q", mgr.PathEntry(), own)
	}

	rel, err := nsToolsRel(env.tools, mgr.PathEntry())
	if err != nil {
		t.Fatalf("the active version directory is not under the tools dir: %v", err)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 {
		t.Fatalf("the active version directory is %s below the tools dir (%q); a nested root can sit inside a tree the engine enumerates", rel, parts)
	}
	// The engine's binDir/optDir/npmDir/pythonDir, i.e. every directory it creates
	// under the tools dir. It never scans the tools dir itself.
	if owned := []string{"bin", "opt", "npm", "python"}; slices.Contains(owned, parts[0]) {
		t.Fatalf("the install root %q collides with the toolbelt engine's own %v trees", parts[0], owned)
	}
}

// TestLegacySweepSparesToolbeltSymlinks: the purge names the shell
// installer's dispatchers, and a toolbelt symlink at one of those paths is
// refused while the genuine residue beside it is removed. The refusal must not
// withhold the completion marker, or every boot re-reads toolbelt's bin dir.
func TestLegacySweepSparesToolbeltSymlinks(t *testing.T) {
	env := newNSEnv(t)
	survivors, _ := env.plantToolbeltKiroCLI(nsTool+"-chat", nsTool+"-term")
	// No bin dispatchers: the two bin names the shell installer also wrote
	// are the engine's symlinks in this volume, and planting a regular file
	// over a symlink would write through it into the tree under test.
	residue := env.plantLegacyResidue()
	own := env.plantOwnVersion()

	if err := env.manager().Ensure(t.Context()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	env.assertIntact(survivors)
	for _, p := range residue {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("the sweep left the shell installer's own residue at %s", p)
		}
	}
	if _, err := os.Lstat(filepath.Join(env.tools, legacyPurgeMarker)); err != nil {
		t.Errorf("a refused foreign entry blocked %s, so every later boot re-runs the sweep over toolbelt's bin dir: %v", legacyPurgeMarker, err)
	}
	// The two entries this install keeps directly under the tools dir must be
	// unreachable by the orphan-stage pass that shares that directory with them.
	if _, err := os.Lstat(own); err != nil {
		t.Errorf("the orphan-stage sweep removed the installation root %s: %v", own, err)
	}
}

// TestLegacySweepRunsOncePerVolume pins the once-only property the marker
// exists for. The layout the sweep deletes cannot come back -- no code
// writes it any more -- so after the migration a pass over toolbelt's
// bin dir can only find another owner's files. A SECOND boot must
// therefore remove nothing at all.
func TestLegacySweepRunsOncePerVolume(t *testing.T) {
	env := newNSEnv(t)
	env.plantOwnVersion()
	residue := env.plantLegacyResidue(nsTool, nsTool+"-chat", nsTool+"-term")

	if err := env.manager().Ensure(t.Context()); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	for _, p := range residue {
		if _, err := os.Lstat(p); err == nil {
			t.Fatalf("the first boot left the shell installer's own residue at %s", p)
		}
	}
	marker := filepath.Join(env.tools, legacyPurgeMarker)
	if _, err := os.Lstat(marker); err != nil {
		t.Fatalf("the first boot did not record %s, so every later boot re-runs the sweep: %v", legacyPurgeMarker, err)
	}

	// Plant the sweep's own targets again, in the shape it removes. Only a second
	// pass could take them.
	replanted := env.plantLegacyResidue(nsTool, nsTool+"-chat", nsTool+"-term")
	if err := env.manager().Ensure(t.Context()); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	for _, p := range replanted {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("the second boot removed %s: the migration sweep still runs on every boot", p)
		}
	}
	if _, err := os.Lstat(marker); err != nil {
		t.Errorf("the completion marker was removed by a later boot: %v", err)
	}
}

// nsShellQuote wraps s in single quotes for the fake dispatcher's script
// body. The only inputs are t.TempDir paths, so a path holding a quote
// would break the script loudly rather than silently misdirect the witness
// write.
func nsShellQuote(s string) string {
	return "'" + s + "'"
}

// TestKiroInstallConfig_activatesTheSingleBinary: since 3.0.0 the archive
// ships no sidecars, so requiring one would leave every 3.x version unready.
// Rescan, not Ensure, which would fetch the pinned archive over the network.
func TestKiroInstallConfig_activatesTheSingleBinary(t *testing.T) {
	env := newNSEnv(t)
	dir := env.plantOwnVersion()
	mgr := env.manager()

	ok, err := mgr.Rescan(t.Context())
	if !ok || err != nil {
		t.Fatalf("Rescan of a version directory holding only %s = (%v, %v), want (true, nil)", nsTool, ok, err)
	}
	if ready, why := mgr.Ready(); !ready || why != pin.ReasonReady {
		t.Errorf("Ready() = (%v, %v), want (true, %v)", ready, why, pin.ReasonReady)
	}
	if got, want := mgr.Path(), filepath.Join(dir, nsTool); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}
