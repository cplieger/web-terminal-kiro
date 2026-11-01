package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cplieger/web-terminal-engine/v6/terminal"
)

// planRecorder is a sessionRestorer that runs the plan over every entry, as the engine
// does, and records what each entry would come back as.
type planRecorder struct {
	specs map[int]terminal.CreateSpec
}

func (p *planRecorder) RestoreSessionSet(ctx context.Context, set *terminal.SessionSet, plan terminal.RestorePlan) (terminal.RestoreResult, error) {
	p.specs = make(map[int]terminal.CreateSpec)
	var res terminal.RestoreResult
	for i, e := range set.Sessions {
		spec, ok := plan(ctx, i, e)
		if !ok {
			res.Skipped++
			continue
		}
		p.specs[i] = spec
	}
	return res, nil
}

// A tabs.json saved while a tab's mapping named one of its workflow steps holds the step's
// session id (the hook in the step's session wrote it). Resuming that id shows the step's
// thread in the tab, so the restore resumes the conversation that launched the run.
func TestTabRestorer_resumesTheConversationThatLaunchedAStep(t *testing.T) {
	const step = "sess_47400f57-1c2d-4e5f-8a9b-0000000000cc"
	plantStep := func(t *testing.T, home string) {
		t.Helper()
		dir := filepath.Join(home, ".kiro", "sessions", "hash0", step)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte(stepJSON(step, resumeA, "run · review #1")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	restore := func(t *testing.T, home string, set terminal.SessionSet) (*planRecorder, func(terminal.CreateSpec) string) {
		t.Helper()
		build, built := recordingBuilder()
		var pending atomic.Bool
		pending.Store(true)
		rec := &planRecorder{}
		newTabRestorer(newSessionTitleSync(t.TempDir(), home), &set, build, &pending).
			run(t.Context(), rec, func() bool { return true }, nil)
		return rec, built
	}

	t.Run("a step entry resumes its launching conversation", func(t *testing.T) {
		home := t.TempDir()
		plantRecord(t, home, resumeA, resumeA, "the tab's conversation")
		plantStep(t, home)
		rec, built := restore(t, home, entrySet(terminal.SessionSetEntry{Alias: step, Resume: step}))
		spec, ok := rec.specs[0]
		if !ok || built(spec) != resumeA || spec.Alias != resumeA {
			t.Errorf("plan = %+v ok=%v built %q, want a resume of %s under its id", spec, ok, built(spec), resumeA)
		}
	})

	t.Run("a step entry beside its launching conversation's own tab is not restored twice", func(t *testing.T) {
		home := t.TempDir()
		plantRecord(t, home, resumeA, resumeA, "the tab's conversation")
		plantStep(t, home)
		rec, built := restore(t, home, entrySet(
			terminal.SessionSetEntry{Alias: step, Resume: step},
			terminal.SessionSetEntry{Alias: resumeA, Resume: resumeA},
		))
		if spec, ok := rec.specs[0]; ok {
			t.Errorf("entry 0 planned %+v (built %q), want it skipped: entry 1's address names the conversation", spec, built(spec))
		}
		if spec, ok := rec.specs[1]; !ok || built(spec) != resumeA {
			t.Errorf("entry 1 = %+v ok=%v, want a resume of %s", spec, ok, resumeA)
		}
	})
}
