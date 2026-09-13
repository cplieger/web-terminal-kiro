package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/web-terminal-engine/v5/terminal"
)

// workflowFixtureParent is the synthetic parentSessionId every redacted fixture
// carries; a test rewrites it to the kiro session it joins through.
const workflowFixtureParent = "sess_c0ffee01-2222-3333-4444-555555555555"

// tabStart is the tab CreatedAt every case is judged against, so the third
// admission clause (file mtime at or after the tab's start) is exercised with
// explicit times rather than with whatever the filesystem happened to stamp.
var tabStart = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// fakeWorkflowSessions is the live-tab snapshot the watcher reads, standing in
// for the session manager so no PTY is needed.
type fakeWorkflowSessions struct{ sessions []terminal.SessionInfo }

func (f *fakeWorkflowSessions) List() []terminal.SessionInfo { return f.sessions }

func liveTabs(ids ...terminal.SessionID) *fakeWorkflowSessions {
	out := &fakeWorkflowSessions{}
	for _, id := range ids {
		out.sessions = append(out.sessions, terminal.SessionInfo{ID: id, CreatedAt: tabStart})
	}
	return out
}

// workflowFixture builds a watcher over a staged $HOME/.kiro/sessions tree plus
// the mapping the title poller would publish.
type workflowFixture struct {
	t     *testing.T
	watch *workflowWatch
	pairs map[terminal.SessionID]string
	home  string
}

func newWorkflowFixture(t *testing.T) *workflowFixture {
	t.Helper()
	f := &workflowFixture{t: t, pairs: map[terminal.SessionID]string{}, home: t.TempDir()}
	f.watch = newWorkflowWatch(f.home, func() map[terminal.SessionID]string { return f.pairs })
	return f
}

// run plants one run's state file from a redacted fixture, repointed at parent,
// and stamps its mtime. It returns the path so a case can rewrite it.
func (f *workflowFixture) run(hash, runID, fixture, parent string, modTime time.Time) string {
	f.t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "workflow-state", fixture))
	if err != nil {
		f.t.Fatalf("read fixture %s: %v", fixture, err)
	}
	return f.stage(hash, runID, strings.ReplaceAll(string(raw), workflowFixtureParent, parent), modTime)
}

// stage plants one run's state file from bytes a case built itself, for a record
// no committed fixture should carry.
func (f *workflowFixture) stage(hash, runID, body string, modTime time.Time) string {
	f.t.Helper()
	dir := filepath.Join(f.home, ".kiro", "sessions", hash, workflowsDirName, runID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		f.t.Fatalf("mkdir run %s: %v", runID, err)
	}
	path := filepath.Join(dir, workflowStateFileName)
	f.write(path, body, modTime)
	return path
}

// write replaces one state file's bytes and stamps its mtime, which is what the
// verdict cache keys on beside the size.
func (f *workflowFixture) write(path, body string, modTime time.Time) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		f.t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		f.t.Fatalf("chtimes %s: %v", path, err)
	}
}

// fixtureState decodes one redacted fixture the way the watcher does, for the
// pure-function cases.
func fixtureState(t *testing.T, name string) *workflowState {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "workflow-state", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var st workflowState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
	return &st
}

// refusedRead is a context every bounded read refuses at entry, so one pass fails its reads
// without touching a record: the shape an EMFILE, an ENOMEM or an EIO has from here, none of
// which is a property of the file.
func refusedRead(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx
}

// TestWorkflowClassifier pins the mapping from one run's record to its mark
// state, against the redacted fixtures so the key set, the enum values and the
// nesting are the ones kiro-cli writes.
func TestWorkflowClassifier(t *testing.T) {
	cases := []struct {
		fixture string
		want    string
		wantOK  bool
	}{
		{"running.json", workflowMarkWorking, true},
		{"paused-need-input.json", workflowMarkInput, true},
		{"paused-transient.json", workflowMarkWaiting, true},
		{"completed.json", "", false},
		{"failed.json", "", false},
		{"aborted.json", "", false},
		{"unknown-status.json", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			st := fixtureState(t, tc.fixture)
			got, ok := classifyWorkflowState(st)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("classifyWorkflowState(%s: status %q) = (%q, %t), want (%q, %t)",
					tc.fixture, st.Status, got, ok, tc.want, tc.wantOK)
			}
		})
	}

	t.Run("an aborted run still holding a need_input node lights nothing", func(t *testing.T) {
		// The status allowlist is what decides this, not the signal: a run
		// abandoned while a step awaited a human keeps that node forever.
		st := fixtureState(t, "aborted.json")
		if !anyNodeNeedsInput(&st.Root) {
			t.Fatal("aborted.json no longer carries a need_input node, so it cannot pin the allowlist")
		}
		if got, ok := classifyWorkflowState(st); ok {
			t.Errorf("classifyWorkflowState(aborted.json) = (%q, true), want not admitted", got)
		}
	})

	t.Run("an unknown status is not admitted and is distinguishable from a terminal one", func(t *testing.T) {
		st := fixtureState(t, "unknown-status.json")
		if knownWorkflowStatus(st.Status) {
			t.Errorf("knownWorkflowStatus(%q) = true, want false", st.Status)
		}
		if !knownWorkflowStatus(workflowStatusCompleted) {
			t.Error("knownWorkflowStatus(completed) = false, want true: a terminal status is known and must not be logged")
		}
	})
}

// TestWorkflowNeedsInputWalk pins the tree walk: a signal two levels down is
// found, a completed step's success signal is not mistaken for one, and the node
// budget stops a pathological tree instead of exhausting the stack.
func TestWorkflowNeedsInputWalk(t *testing.T) {
	t.Run("a need_input node nested two levels deep is found", func(t *testing.T) {
		st := fixtureState(t, "paused-need-input.json")
		if !anyNodeNeedsInput(&st.Root) {
			t.Error("anyNodeNeedsInput(paused-need-input.json) = false, want true: the signal sits on a grandchild of root")
		}
	})

	t.Run("a success signal is not a need_input signal", func(t *testing.T) {
		st := fixtureState(t, "paused-transient.json")
		if anyNodeNeedsInput(&st.Root) {
			t.Error("anyNodeNeedsInput(paused-transient.json) = true, want false: a completed step reports success and an unfinished one reports nothing")
		}
	})

	t.Run("a chain within the budget is walked to its end", func(t *testing.T) {
		if !anyNodeNeedsInput(chainOfNodes(maxWorkflowNodes-1, workflowSignalNeedInput)) {
			t.Errorf("anyNodeNeedsInput(chain of %d) = false, want true", maxWorkflowNodes-1)
		}
	})

	t.Run("a chain past the budget stops and answers no input", func(t *testing.T) {
		if anyNodeNeedsInput(chainOfNodes(maxWorkflowNodes+50, workflowSignalNeedInput)) {
			t.Errorf("anyNodeNeedsInput(chain of %d) = true, want false: the budget must stop the walk", maxWorkflowNodes+50)
		}
	})
}

// deepRecord is a paused run whose tree is one chain past the node budget, with the
// need-input signal on the leaf the walk cannot reach. Generated rather than
// committed, so it tracks maxWorkflowNodes instead of silently stopping testing the
// budget the next time that constant is raised.
func deepRecord(t *testing.T, parent string) string {
	t.Helper()
	raw, err := json.Marshal(workflowState{
		Status:          workflowStatusPaused,
		ParentSessionID: parent,
		Root:            *chainOfNodes(maxWorkflowNodes+50, workflowSignalNeedInput),
	})
	if err != nil {
		t.Fatalf("marshal a chain of %d nodes: %v", maxWorkflowNodes+50, err)
	}
	return string(raw)
}

// chainOfNodes builds a single-child chain of n nodes carrying signal on the
// deepest one, which is the worst case for both the stack and the budget.
func chainOfNodes(n int, signal string) *workflowNode {
	deepest := workflowNode{CompletionSignal: signal}
	node := &deepest
	for range n - 1 {
		node = &workflowNode{Children: []workflowNode{*node}}
	}
	return node
}

// TestWorkflowFold pins one tab's fold: which state wins, and that the census
// counts every admitted run.
func TestWorkflowFold(t *testing.T) {
	t.Run("input wins whatever the order", func(t *testing.T) {
		for _, states := range permuteThree(workflowMarkWorking, workflowMarkWaiting, workflowMarkInput) {
			got := foldWorkflowMarks(states)
			want := workflowMark{State: workflowMarkInput, Tally: workflowTally{Total: 3, Working: 1, Waiting: 1, Input: 1}}
			if got != want {
				t.Errorf("foldWorkflowMarks(%v) = %+v, want %+v", states, got, want)
			}
		}
	})

	t.Run("waiting outranks working", func(t *testing.T) {
		for _, states := range [][]string{
			{workflowMarkWorking, workflowMarkWaiting},
			{workflowMarkWaiting, workflowMarkWorking},
		} {
			got := foldWorkflowMarks(states)
			want := workflowMark{State: workflowMarkWaiting, Tally: workflowTally{Total: 2, Working: 1, Waiting: 1}}
			if got != want {
				t.Errorf("foldWorkflowMarks(%v) = %+v, want %+v", states, got, want)
			}
		}
	})

	t.Run("the tally sums to Total", func(t *testing.T) {
		states := []string{workflowMarkWorking, workflowMarkWorking, workflowMarkWaiting, workflowMarkInput}
		got := foldWorkflowMarks(states)
		if sum := got.Tally.Working + got.Tally.Waiting + got.Tally.Input; sum != got.Tally.Total {
			t.Errorf("foldWorkflowMarks(%v) tally = %+v, want the three states to sum to Total (%d != %d)", states, got.Tally, sum, got.Tally.Total)
		}
		if got.Tally.Total != len(states) {
			t.Errorf("foldWorkflowMarks(%v).Tally.Total = %d, want %d", states, got.Tally.Total, len(states))
		}
	})

	t.Run("no runs folds to the zero mark", func(t *testing.T) {
		if got := foldWorkflowMarks(nil); got != (workflowMark{}) {
			t.Errorf("foldWorkflowMarks(nil) = %+v, want the zero mark", got)
		}
	})

	t.Run("a state outside the closed set contributes nothing", func(t *testing.T) {
		got := foldWorkflowMarks([]string{"queued", workflowMarkWorking})
		want := workflowMark{State: workflowMarkWorking, Tally: workflowTally{Total: 1, Working: 1}}
		if got != want {
			t.Errorf("foldWorkflowMarks([queued working]) = %+v, want %+v", got, want)
		}
	})
}

// permuteThree returns every ordering of three values, so a precedence claim is
// pinned against order rather than against one arrangement.
func permuteThree(a, b, c string) [][]string {
	return [][]string{{a, b, c}, {a, c, b}, {b, a, c}, {b, c, a}, {c, a, b}, {c, b, a}}
}

// TestWorkflowWatchAdmission drives the watcher over a staged sessions tree and
// pins all three admission clauses.
func TestWorkflowWatchAdmission(t *testing.T) {
	const kiro = "sess_11111111-2222-3333-4444-555555555555"

	t.Run("a running run on a mapped live tab lights working", func(t *testing.T) {
		f := newWorkflowFixture(t)
		f.pairs["tab1"] = kiro
		f.run("hash0", "wf_aaaa", "running.json", kiro, tabStart.Add(time.Minute))

		f.watch.pass(t.Context(), liveTabs("tab1"))

		want := workflowMark{State: workflowMarkWorking, Tally: workflowTally{Total: 1, Working: 1}}
		if got := f.watch.mark("tab1"); got != want {
			t.Errorf("mark(tab1) = %+v, want %+v", got, want)
		}
	})

	t.Run("the same run with no live tab lights nothing", func(t *testing.T) {
		f := newWorkflowFixture(t)
		f.pairs["tab1"] = kiro
		f.run("hash0", "wf_aaaa", "running.json", kiro, tabStart.Add(time.Minute))

		f.watch.pass(t.Context(), liveTabs())

		if got := f.watch.mark("tab1"); got != (workflowMark{}) {
			t.Errorf("mark(tab1) = %+v, want the zero mark: a run whose tab is gone must light nothing", got)
		}
	})

	t.Run("a run written before the tab started lights nothing", func(t *testing.T) {
		f := newWorkflowFixture(t)
		f.pairs["tab1"] = kiro
		f.run("hash0", "wf_aaaa", "paused-need-input.json", kiro, tabStart.Add(-time.Hour))

		f.watch.pass(t.Context(), liveTabs("tab1"))

		if got := f.watch.mark("tab1"); got != (workflowMark{}) {
			t.Errorf("mark(tab1) = %+v, want the zero mark: a non-terminal run predating the tab is abandoned, not live", got)
		}
	})

	t.Run("a paused run awaiting a human lights input", func(t *testing.T) {
		f := newWorkflowFixture(t)
		f.pairs["tab1"] = kiro
		f.run("hash0", "wf_aaaa", "paused-need-input.json", kiro, tabStart.Add(time.Minute))

		f.watch.pass(t.Context(), liveTabs("tab1"))

		want := workflowMark{State: workflowMarkInput, Tally: workflowTally{Total: 1, Input: 1}}
		if got := f.watch.mark("tab1"); got != want {
			t.Errorf("mark(tab1) = %+v, want %+v", got, want)
		}
	})

	t.Run("two runs on one tab fold by precedence and both are counted", func(t *testing.T) {
		f := newWorkflowFixture(t)
		f.pairs["tab1"] = kiro
		f.run("hash0", "wf_aaaa", "running.json", kiro, tabStart.Add(time.Minute))
		f.run("hash0", "wf_bbbb", "paused-need-input.json", kiro, tabStart.Add(2*time.Minute))

		f.watch.pass(t.Context(), liveTabs("tab1"))

		want := workflowMark{State: workflowMarkInput, Tally: workflowTally{Total: 2, Working: 1, Input: 1}}
		if got := f.watch.mark("tab1"); got != want {
			t.Errorf("mark(tab1) = %+v, want %+v", got, want)
		}
	})

	t.Run("a terminal run lights nothing even on a live mapped tab", func(t *testing.T) {
		f := newWorkflowFixture(t)
		f.pairs["tab1"] = kiro
		for i, fixture := range []string{"completed.json", "failed.json", "aborted.json"} {
			f.run("hash0", "wf_term"+string(rune('a'+i)), fixture, kiro, tabStart.Add(time.Minute))
		}

		f.watch.pass(t.Context(), liveTabs("tab1"))

		if got := f.watch.mark("tab1"); got != (workflowMark{}) {
			t.Errorf("mark(tab1) = %+v, want the zero mark: 267 of 269 real records are terminal", got)
		}
	})

	t.Run("a run whose parent resolves to nothing lights nothing", func(t *testing.T) {
		f := newWorkflowFixture(t)
		f.pairs["tab1"] = kiro
		// A run launched under a kiro session this tab is not mapped to, and one
		// whose record names no parent at all.
		f.run("hash0", "wf_aaaa", "running.json", "sess_99999999-8888-7777-6666-555555555555", tabStart.Add(time.Minute))
		f.run("hash0", "wf_bbbb", "no-parent.json", kiro, tabStart.Add(time.Minute))

		f.watch.pass(t.Context(), liveTabs("tab1"))

		if got := f.watch.mark("tab1"); got != (workflowMark{}) {
			t.Errorf("mark(tab1) = %+v, want the zero mark: no tab is guessed for a run whose parent does not resolve", got)
		}
	})

	t.Run("only the launching tab is lit", func(t *testing.T) {
		f := newWorkflowFixture(t)
		const other = "sess_99999999-8888-7777-6666-555555555555"
		f.pairs["tab1"], f.pairs["tab2"] = kiro, other
		f.run("hash0", "wf_aaaa", "running.json", kiro, tabStart.Add(time.Minute))

		f.watch.pass(t.Context(), liveTabs("tab1", "tab2"))

		if got := f.watch.mark("tab2"); got != (workflowMark{}) {
			t.Errorf("mark(tab2) = %+v, want the zero mark: the run belongs to tab1's kiro session", got)
		}
		if got := f.watch.mark("tab1").State; got != workflowMarkWorking {
			t.Errorf("mark(tab1).State = %q, want %q", got, workflowMarkWorking)
		}
	})
}

// TestWorkflowWatchTolerates pins what the watcher meets on disk that is not an
// ordinary readable run: the shapes that are not runs at all, a partial write, a
// record nested past the node budget, one the read refuses outright, and one whose
// read fails for a reason that is not a property of the file.
func TestWorkflowWatchTolerates(t *testing.T) {
	const kiro = "sess_11111111-2222-3333-4444-555555555555"

	t.Run("a directory with no state file is silent", func(t *testing.T) {
		f := newWorkflowFixture(t)
		f.pairs["tab1"] = kiro
		f.run("hash0", "wf_aaaa", "running.json", kiro, tabStart.Add(time.Minute))
		// workflows/generated/ holds gen_*.workflow.json and no run state, which
		// is why directory names are not filtered on a wf_ prefix.
		generated := filepath.Join(f.home, ".kiro", "sessions", "hash0", workflowsDirName, "generated")
		if err := os.MkdirAll(generated, 0o750); err != nil {
			t.Fatalf("mkdir generated: %v", err)
		}
		if err := os.WriteFile(filepath.Join(generated, "gen_abcd.workflow.json"), []byte("{}"), 0o600); err != nil {
			t.Fatalf("write a generated definition: %v", err)
		}

		logged := capturedLog(t)
		f.watch.pass(t.Context(), liveTabs("tab1"))

		if got := f.watch.mark("tab1").State; got != workflowMarkWorking {
			t.Errorf("mark(tab1).State = %q, want %q", got, workflowMarkWorking)
		}
		if out := logged.String(); strings.Contains(out, "could not be stat'ed") {
			t.Errorf("logged %q, want silence: a directory holding no run state is the normal case", out)
		}
	})

	t.Run("a hash dir with no workflows dir is silent", func(t *testing.T) {
		f := newWorkflowFixture(t)
		f.pairs["tab1"] = kiro
		// Two of the three real hash dirs have no workflows/ at all, and one is
		// not a directory at all.
		if err := os.MkdirAll(filepath.Join(f.home, ".kiro", "sessions", "hash1"), 0o750); err != nil {
			t.Fatalf("mkdir a hash dir with no workflows: %v", err)
		}
		f.run("hash2", "wf_aaaa", "running.json", kiro, tabStart.Add(time.Minute))

		logged := capturedLog(t)
		f.watch.pass(t.Context(), liveTabs("tab1"))

		if got := f.watch.mark("tab1").State; got != workflowMarkWorking {
			t.Errorf("mark(tab1).State = %q, want %q: the scan must carry on past a hash dir with no runs", got, workflowMarkWorking)
		}
		if out := logged.String(); strings.Contains(out, "workflow store unreadable") {
			t.Errorf("logged %q, want silence: an absent workflows/ is the normal case", out)
		}
	})

	t.Run("an absent sessions tree is silent", func(t *testing.T) {
		f := newWorkflowFixture(t)
		f.pairs["tab1"] = kiro

		logged := capturedLog(t)
		f.watch.pass(t.Context(), liveTabs("tab1"))

		if got := f.watch.mark("tab1"); got != (workflowMark{}) {
			t.Errorf("mark(tab1) = %+v, want the zero mark", got)
		}
		if out := logged.String(); strings.Contains(out, "session store unreadable") {
			t.Errorf("logged %q, want silence: no session store exists before kiro-cli writes its first session", out)
		}
	})

	t.Run("a partially written file leaves the previous mark unchanged", func(t *testing.T) {
		f := newWorkflowFixture(t)
		f.pairs["tab1"] = kiro
		path := f.run("hash0", "wf_aaaa", "running.json", kiro, tabStart.Add(time.Minute))
		f.watch.pass(t.Context(), liveTabs("tab1"))
		if got := f.watch.mark("tab1").State; got != workflowMarkWorking {
			t.Fatalf("first pass mark(tab1).State = %q, want %q", got, workflowMarkWorking)
		}

		raw, err := os.ReadFile(filepath.Join("testdata", "workflow-state", "truncated.json"))
		if err != nil {
			t.Fatalf("read the truncated fixture: %v", err)
		}
		f.write(path, string(raw), tabStart.Add(2*time.Minute))
		f.watch.pass(t.Context(), liveTabs("tab1"))

		if got := f.watch.mark("tab1").State; got != workflowMarkWorking {
			t.Errorf("mark(tab1).State = %q after a torn read, want %q: a partial write is followed by another write, so the state must not flicker", got, workflowMarkWorking)
		}

		// And it is not permanent: the next complete write decides.
		done, err := os.ReadFile(filepath.Join("testdata", "workflow-state", "completed.json"))
		if err != nil {
			t.Fatalf("read the completed fixture: %v", err)
		}
		f.write(path, strings.ReplaceAll(string(done), workflowFixtureParent, kiro), tabStart.Add(3*time.Minute))
		f.watch.pass(t.Context(), liveTabs("tab1"))

		if got := f.watch.mark("tab1"); got != (workflowMark{}) {
			t.Errorf("mark(tab1) = %+v after the run completed, want the zero mark", got)
		}
	})

	t.Run("a record nested past the node budget is folded rather than crashing", func(t *testing.T) {
		f := newWorkflowFixture(t)
		f.pairs["tab1"] = kiro
		f.stage("hash0", "wf_deep", deepRecord(t, kiro), tabStart.Add(time.Minute))

		f.watch.pass(t.Context(), liveTabs("tab1"))

		// The budget stops the walk short of the signalled leaf, and answering "no
		// input" reads as waiting rather than claiming the user's attention.
		want := workflowMark{State: workflowMarkWaiting, Tally: workflowTally{Total: 1, Waiting: 1}}
		if got := f.watch.mark("tab1"); got != want {
			t.Errorf("mark(tab1) = %+v over a chain of %d nodes, want %+v", got, maxWorkflowNodes+50, want)
		}
	})

	t.Run("a record the read refuses withdraws the mark rather than latching it", func(t *testing.T) {
		f := newWorkflowFixture(t)
		f.pairs["tab1"] = kiro
		path := f.run("hash0", "wf_aaaa", "running.json", kiro, tabStart.Add(time.Minute))
		f.watch.pass(t.Context(), liveTabs("tab1"))
		if got := f.watch.mark("tab1").State; got != workflowMarkWorking {
			t.Fatalf("first pass mark(tab1).State = %q, want %q", got, workflowMarkWorking)
		}

		// capturedOutputs is unbounded in principle, so a record past the read bound is
		// reachable -- and unlike a torn write it fails on EVERY later pass, so carrying
		// the lit verdict forward would keep this tab working for as long as it is open,
		// whatever the run went on to do.
		overBound := `{"status":"` + workflowStatusRunning + `","parentSessionId":"` + kiro +
			`","capturedOutputs":{"step":"` + strings.Repeat("x", maxWorkflowStateBytes) + `"}}`
		f.write(path, overBound, tabStart.Add(2*time.Minute))
		f.watch.pass(t.Context(), liveTabs("tab1"))

		if got := f.watch.mark("tab1"); got != (workflowMark{}) {
			t.Errorf("mark(tab1) = %+v over a record of %d bytes against a %d bound, want the zero mark", got, len(overBound), maxWorkflowStateBytes)
		}
	})

	t.Run("a record whose first read is refused is read again rather than written off", func(t *testing.T) {
		f := newWorkflowFixture(t)
		f.pairs["tab1"] = kiro
		// A paused run awaiting a human is the record kiro-cli never writes again, because
		// writes are event-driven, and it is the state the mark exists to report. So the
		// failing pass is the only read there will ever be unless the next pass retries.
		f.run("hash0", "wf_aaaa", "paused-need-input.json", kiro, tabStart.Add(time.Minute))

		f.watch.pass(refusedRead(t), liveTabs("tab1"))
		f.watch.pass(t.Context(), liveTabs("tab1"))

		want := workflowMark{State: workflowMarkInput, Tally: workflowTally{Total: 1, Input: 1}}
		if got := f.watch.mark("tab1"); got != want {
			t.Errorf("mark(tab1) = %+v one pass after a refused first read, want %+v", got, want)
		}
	})

	t.Run("a read refused over a record the writer just moved is retried on the next pass", func(t *testing.T) {
		f := newWorkflowFixture(t)
		f.pairs["tab1"] = kiro
		path := f.run("hash0", "wf_aaaa", "running.json", kiro, tabStart.Add(time.Minute))
		f.watch.pass(t.Context(), liveTabs("tab1"))
		if got := f.watch.mark("tab1").State; got != workflowMarkWorking {
			t.Fatalf("first pass mark(tab1).State = %q, want %q", got, workflowMarkWorking)
		}

		// The run pauses for input, so mtime and size both move, and that pass's read fails.
		paused, err := os.ReadFile(filepath.Join("testdata", "workflow-state", "paused-need-input.json"))
		if err != nil {
			t.Fatalf("read the paused fixture: %v", err)
		}
		f.write(path, strings.ReplaceAll(string(paused), workflowFixtureParent, kiro), tabStart.Add(2*time.Minute))
		f.watch.pass(refusedRead(t), liveTabs("tab1"))
		f.watch.pass(t.Context(), liveTabs("tab1"))

		want := workflowMark{State: workflowMarkInput, Tally: workflowTally{Total: 1, Input: 1}}
		if got := f.watch.mark("tab1"); got != want {
			t.Errorf("mark(tab1) = %+v, want %+v: the pass after a refused read must re-read rather than hold until the writer moves the record again", got, want)
		}
	})
}

// TestWorkflowWatchCachesByFileIdentity pins the read economy: 269 real records
// sit in one directory, so an unchanged file must cost a stat rather than a read
// plus a decode -- and a changed one must be re-read.
func TestWorkflowWatchCachesByFileIdentity(t *testing.T) {
	const kiro = "sess_11111111-2222-3333-4444-555555555555"
	f := newWorkflowFixture(t)
	f.pairs["tab1"] = kiro
	path := f.run("hash0", "wf_aaaa", "running.json", kiro, tabStart.Add(time.Minute))

	f.watch.pass(t.Context(), liveTabs("tab1"))
	if got := f.watch.mark("tab1").State; got != workflowMarkWorking {
		t.Fatalf("first pass mark(tab1).State = %q, want %q", got, workflowMarkWorking)
	}

	// Replace the content with a VALID terminal record of exactly the same size
	// ("running" and "aborted" are both seven bytes) and restore the mtime. A
	// re-read would withdraw the mark, so the mark surviving is the proof.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the staged run: %v", err)
	}
	swapped := strings.ReplaceAll(string(raw), workflowStatusRunning, workflowStatusAborted)
	if len(swapped) != len(raw) {
		t.Fatalf("the swapped record is %d bytes against %d: the case needs an identical size", len(swapped), len(raw))
	}
	f.write(path, swapped, tabStart.Add(time.Minute))
	f.watch.pass(t.Context(), liveTabs("tab1"))

	if got := f.watch.mark("tab1").State; got != workflowMarkWorking {
		t.Errorf("mark(tab1).State = %q, want %q: a file whose mtime and size are unchanged must not be re-read", got, workflowMarkWorking)
	}

	// Move the mtime and the same bytes must now be read.
	if err := os.Chtimes(path, tabStart.Add(5*time.Minute), tabStart.Add(5*time.Minute)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	f.watch.pass(t.Context(), liveTabs("tab1"))

	if got := f.watch.mark("tab1"); got != (workflowMark{}) {
		t.Errorf("mark(tab1) = %+v, want the zero mark: a changed mtime must invalidate the cached verdict", got)
	}
}

// TestWorkflowWatchWarnsOncePerUnknownStatus pins the fail-open half of the
// closed allowlist: an unrecognized status lights nothing and says so once, so a
// kiro-cli release that widens the enum is diagnosable without a log flood at
// one line every two seconds.
func TestWorkflowWatchWarnsOncePerUnknownStatus(t *testing.T) {
	const kiro = "sess_11111111-2222-3333-4444-555555555555"
	f := newWorkflowFixture(t)
	f.pairs["tab1"] = kiro
	path := f.run("hash0", "wf_aaaa", "unknown-status.json", kiro, tabStart.Add(time.Minute))

	logged := capturedLog(t)
	f.watch.pass(t.Context(), liveTabs("tab1"))
	// A second pass over a file the cache would short-circuit proves nothing, so
	// the mtime moves and the record is genuinely re-read.
	if err := os.Chtimes(path, tabStart.Add(2*time.Minute), tabStart.Add(2*time.Minute)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	f.watch.pass(t.Context(), liveTabs("tab1"))

	if got := f.watch.mark("tab1"); got != (workflowMark{}) {
		t.Errorf("mark(tab1) = %+v, want the zero mark: an unknown status defaults to absent, never to lit", got)
	}
	const record = "unrecognized workflow run status"
	if got := strings.Count(logged.String(), record); got != 1 {
		t.Errorf("logged %d records naming %q, want exactly 1:\n%s", got, record, logged.String())
	}
	if out := logged.String(); !strings.Contains(out, "suspended") {
		t.Errorf("logged %q, want the offending status value named so the enum change is diagnosable", out)
	}
}

// TestWorkflowWatchSanitizesTheOnlyTextItLogs pins the rune policy on the one
// value from the file that reaches a sink. Nothing else in the record is decoded,
// which is what keeps this the only place the question arises.
func TestWorkflowWatchSanitizesTheOnlyTextItLogs(t *testing.T) {
	const kiro = "sess_11111111-2222-3333-4444-555555555555"
	f := newWorkflowFixture(t)
	f.pairs["tab1"] = kiro
	path := f.run("hash0", "wf_aaaa", "unknown-status.json", kiro, tabStart.Add(time.Minute))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the staged run: %v", err)
	}
	hostile := strings.Replace(string(raw), `"suspended"`, `"sus\u202epended\nforged=line\u0085tail"`, 1)
	f.write(path, hostile, tabStart.Add(time.Minute))

	logged := capturedLog(t)
	f.watch.pass(t.Context(), liveTabs("tab1"))

	out := logged.String()
	if strings.Contains(out, "\u202e") || strings.Contains(out, "\u0085") {
		t.Errorf("logged %q with bidi or C1 controls intact; upstream text must not forge what an operator reads", out)
	}
	if !strings.Contains(out, "sus pended forged=line tail") {
		t.Errorf("logged %q, want the sanitized status value", out)
	}
}

// TestWorkflowWatchRecordsTheChangeAndNotTheTabID pins two properties of the
// one line the watcher writes in ordinary operation: a steady state is silent
// (this runs every two seconds), and the tab is named through terminal.LogID,
// because a raw tab id in a log is the /ws attach and resume capability sitting
// in the log store.
func TestWorkflowWatchRecordsTheChangeAndNotTheTabID(t *testing.T) {
	const kiro = "sess_11111111-2222-3333-4444-555555555555"
	// A realistic minted tab id, so the truncation LogID applies is visible.
	const tab terminal.SessionID = "3f7a1c4e5b6d8092a1b2c3d4e5f60718"
	f := newWorkflowFixture(t)
	f.pairs[tab] = kiro
	f.run("hash0", "wf_aaaa", "running.json", kiro, tabStart.Add(time.Minute))
	tabs := &fakeWorkflowSessions{sessions: []terminal.SessionInfo{{ID: tab, CreatedAt: tabStart}}}

	logged := capturedLog(t)
	f.watch.pass(t.Context(), tabs)

	out := logged.String()
	if !strings.Contains(out, "published set changed") {
		t.Errorf("logged %q, want one record of the change", out)
	}
	if strings.Contains(out, string(tab)) {
		t.Errorf("logged %q carrying the whole tab id, want only terminal.LogID's prefix", out)
	}
	if !strings.Contains(out, terminal.LogID(tab)) {
		t.Errorf("logged %q, want the tab named by LogID so a lit tab is correlatable", out)
	}

	before := len(out)
	f.watch.pass(t.Context(), tabs)
	if extra := logged.String()[before:]; strings.Contains(extra, "published set changed") {
		t.Errorf("a second pass over an unchanged set logged %q, want silence at one line every two seconds", extra)
	}
}

// TestWorkflowWatchRunStopsWithItsContext pins Run's whole contract: it sweeps
// until its context is cancelled and then RETURNS. Every other case here drives
// pass() directly.
func TestWorkflowWatchRunStopsWithItsContext(t *testing.T) {
	f := newWorkflowFixture(t)
	ctx, cancel := context.WithCancel(t.Context())

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		f.watch.Run(ctx, liveTabs())
	}()
	cancel()

	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context was cancelled; the watcher outlives the server it was started for, holding its ticker and re-scanning the session store")
	}
}
