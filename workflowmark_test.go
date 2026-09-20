package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cplieger/web-terminal-engine/v6/terminal"
)

// workflowFixtureParent is the synthetic parentSessionId every redacted fixture
// carries; a test rewrites it to the kiro session it joins through.
const workflowFixtureParent = "sess_c0ffee01-2222-3333-4444-555555555555"

// The two step sessions the fixtures carry on their nodes. They are the run's
// other owners: KAS runs each step as its own kiro session, so these are the ids a
// tab's mapping actually names while the run is working.
const (
	workflowFixtureStepOne = "sess_a1b2c3d4-5566-7788-99aa-bbccddeeff00"
	workflowFixtureStepTwo = "sess_b2c3d4e5-6677-8899-aabb-ccddeeff0011"
)

// workflowFixtureBeatPID is the placeholder pid run-beat.json carries; beat
// rewrites it to the pid a case wants the probe asked about.
const workflowFixtureBeatPID = "999999999"

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

// beat plants one run's run.beat from the redacted fixture, repointed at pid.
func (f *workflowFixture) beat(hash, runID string, pid int, modTime time.Time) {
	f.t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "workflow-state", "run-beat.json"))
	if err != nil {
		f.t.Fatalf("read the run-beat fixture: %v", err)
	}
	body := strings.ReplaceAll(string(raw), workflowFixtureBeatPID, strconv.Itoa(pid))
	f.beatBody(hash, runID, body, modTime)
}

// beatBody plants one run's run.beat from bytes a case built itself, for a body
// no committed fixture should carry.
func (f *workflowFixture) beatBody(hash, runID, body string, modTime time.Time) {
	f.t.Helper()
	dir := filepath.Join(f.home, ".kiro", "sessions", hash, workflowsDirName, runID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		f.t.Fatalf("mkdir run %s: %v", runID, err)
	}
	f.write(filepath.Join(dir, workflowBeatFileName), body, modTime)
}

// probeRecorder is the liveness probe a case installs on the watcher: it answers
// one verdict and records every pid it was asked about, which is how a case
// asserts what the code did NOT ask.
type probeRecorder struct {
	pids  []int
	alive bool
}

func (p *probeRecorder) probe(pid int) bool {
	p.pids = append(p.pids, pid)
	return p.alive
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

// TestWorkflowMarkSurvivesAStepTransition pins the join across a step boundary,
// which is the reported defect. Each workflow step runs as its OWN kiro session and
// kiro-cli fires the session-title hook for it, so the tab's mapping names the
// RUNNING STEP rather than the launching session for almost the whole life of a
// run: measured on the live container, one tab's mapping named a step session
// throughout a 17-minute window and followed the run from one step to the next. A
// join on parentSessionId alone therefore finds no tab, the run is admitted for
// nobody, and the mark goes dark the moment the next step's hook fires.
func TestWorkflowMarkSurvivesAStepTransition(t *testing.T) {
	// A launching session no tab is mapped to, so every leg below joins through a
	// STEP session and the parent id can light nothing on its own.
	const launching = "sess_dddddddd-eeee-ffff-1111-222222222222"

	t.Run("one run stays lit while its mapping moves from step one to step two", func(t *testing.T) {
		f := newWorkflowFixture(t)
		f.pairs["tab1"] = workflowFixtureStepOne
		path := f.run("hash0", "wf_1111", "running.json", launching, tabStart.Add(time.Minute))

		f.watch.pass(t.Context(), liveTabs("tab1"))

		want := workflowMark{State: workflowMarkWorking, Tally: workflowTally{Total: 1, Working: 1}}
		if got := f.watch.mark("tab1"); got != want {
			t.Fatalf("mark(tab1) = %+v while step one runs, want %+v", got, want)
		}

		// The transition: step one completes, step two starts as a new kiro session,
		// and the hook re-points the tab's mapping to it. Same run, same file.
		second, err := os.ReadFile(filepath.Join("testdata", "workflow-state", "running-second-step.json"))
		if err != nil {
			t.Fatalf("read the second-step fixture: %v", err)
		}
		f.write(path, strings.ReplaceAll(string(second), workflowFixtureParent, launching), tabStart.Add(2*time.Minute))
		f.pairs["tab1"] = workflowFixtureStepTwo

		f.watch.pass(t.Context(), liveTabs("tab1"))

		if got := f.watch.mark("tab1"); got != want {
			t.Errorf("mark(tab1) = %+v after the run advanced to its second step, want %+v: the mark must not blink at a step boundary", got, want)
		}
	})

	t.Run("a completed step's session does not resurrect a terminal run", func(t *testing.T) {
		// A run's owner set reaches a step whose own status is completed, so the
		// run's TOP-LEVEL status has to stay the only thing that admits it.
		f := newWorkflowFixture(t)
		f.pairs["tab1"] = workflowFixtureStepOne
		f.run("hash0", "wf_4444", "completed.json", launching, tabStart.Add(time.Minute))

		f.watch.pass(t.Context(), liveTabs("tab1"))

		if got := f.watch.mark("tab1"); got != (workflowMark{}) {
			t.Errorf("mark(tab1) = %+v, want the zero mark: a completed run still holding its step's session must light nothing", got)
		}
	})

	t.Run("a run repeating one step session across nodes is counted once", func(t *testing.T) {
		// paused-need-input.json carries the same step session on two nodes, which is
		// what a real record does; an undeduplicated owner set would admit the run
		// twice for the one tab and report two runs where there is one.
		f := newWorkflowFixture(t)
		f.pairs["tab1"] = workflowFixtureStepOne
		f.run("hash0", "wf_2222", "paused-need-input.json", launching, tabStart.Add(time.Minute))

		f.watch.pass(t.Context(), liveTabs("tab1"))

		want := workflowMark{State: workflowMarkInput, Tally: workflowTally{Total: 1, Input: 1}}
		if got := f.watch.mark("tab1"); got != want {
			t.Errorf("mark(tab1) = %+v, want %+v", got, want)
		}
	})
}

// TestWorkflowMarkWithdrawsARunWhoseWriterDied pins the liveness clause. KAS never
// rewrites a record whose writer died, so its status stays running forever and the tab's
// mapping keeps naming one of the run's sessions: every other clause admits such a record,
// and the dead pid in its run.beat is the only thing that does not. It reddens if a dead
// writer stops withdrawing the mark, if a live writer is refused too, or if the liveness
// answer is folded into the verdict cache.
func TestWorkflowMarkWithdrawsARunWhoseWriterDied(t *testing.T) {
	const kiro = "sess_11111111-2222-3333-4444-555555555555"

	t.Run("a run abandoned during the tab's life lights nothing", func(t *testing.T) {
		f := newWorkflowFixture(t)
		f.pairs["tab1"] = kiro
		f.run("hash0", "wf_dead", "running.json", kiro, tabStart.Add(time.Minute))
		f.beat("hash0", "wf_dead", 4242, tabStart.Add(time.Minute))
		probe := &probeRecorder{}
		f.watch.probeAlive = probe.probe

		f.watch.pass(t.Context(), liveTabs("tab1"))

		if got := f.watch.mark("tab1"); got != (workflowMark{}) {
			t.Errorf("mark(tab1) = %+v, want the zero mark: the record is newer than the tab and its writer is gone", got)
		}
		if want := []int{4242}; !slices.Equal(probe.pids, want) {
			t.Errorf("the probe was asked about %v, want %v: the pid run.beat names is the whole signal", probe.pids, want)
		}
	})

	t.Run("the same record with a live writer stays lit", func(t *testing.T) {
		// The control that makes the clause a refusal of the DEAD rather than a blanket
		// one: it reddens if the probe's answer is inverted or ignored.
		f := newWorkflowFixture(t)
		f.pairs["tab1"] = kiro
		f.run("hash0", "wf_live", "running.json", kiro, tabStart.Add(time.Minute))
		f.beat("hash0", "wf_live", 4242, tabStart.Add(time.Minute))
		f.watch.probeAlive = (&probeRecorder{alive: true}).probe

		f.watch.pass(t.Context(), liveTabs("tab1"))

		want := workflowMark{State: workflowMarkWorking, Tally: workflowTally{Total: 1, Working: 1}}
		if got := f.watch.mark("tab1"); got != want {
			t.Errorf("mark(tab1) = %+v, want %+v: a run.beat naming a live pid refuses nothing", got, want)
		}
	})

	t.Run("the writer dying is seen without the record changing", func(t *testing.T) {
		f := newWorkflowFixture(t)
		f.pairs["tab1"] = kiro
		f.run("hash0", "wf_live", "running.json", kiro, tabStart.Add(time.Minute))
		f.beat("hash0", "wf_live", 4242, tabStart.Add(time.Minute))
		f.watch.probeAlive = (&probeRecorder{alive: true}).probe
		f.watch.pass(t.Context(), liveTabs("tab1"))
		if got := f.watch.mark("tab1").State; got != workflowMarkWorking {
			t.Fatalf("first pass mark(tab1).State = %q, want %q", got, workflowMarkWorking)
		}

		// Nothing on disk moves: verdictFor caches on the state file's size and mtime, and
		// an abandoned record's file never changes again, so a cached liveness answer would
		// stay lit for the tab's whole life.
		f.watch.probeAlive = (&probeRecorder{}).probe
		f.watch.pass(t.Context(), liveTabs("tab1"))

		if got := f.watch.mark("tab1"); got != (workflowMark{}) {
			t.Errorf("mark(tab1) = %+v after its writer died, want the zero mark: liveness is re-derived every pass", got)
		}
	})

	t.Run("the watcher's own probe answers the kernel", func(t *testing.T) {
		// The only leg that leaves probeAlive as newWorkflowWatch built it, so a default
		// wired to nothing -- which fails open, silently, for every run in production --
		// cannot pass while the rest of the suite installs its own probe.
		f := newWorkflowFixture(t)
		f.pairs["tab1"] = kiro
		f.run("hash0", "wf_dead", "running.json", kiro, tabStart.Add(time.Minute))
		f.beat("hash0", "wf_dead", impossiblePID(t), tabStart.Add(time.Minute))

		f.watch.pass(t.Context(), liveTabs("tab1"))

		if got := f.watch.mark("tab1"); got != (workflowMark{}) {
			t.Errorf("mark(tab1) = %+v, want the zero mark: the run.beat names a pid the kernel never allocated", got)
		}
	})
}

// TestWorkflowMarkKeepsAPausedRunLitWhileItsWriterLives pins that no freshness window
// exists on run.beat. kiro-cli's beat loop is scoped to the run loop and that loop returns
// when a node pauses, so a run awaiting a human has a FROZEN run.beat -- the one measured
// paused record on disk took no tick after its pause -- while its writer stays alive for as
// long as the tab is open. It reddens if anyone compares the beat's mtime or a decoded
// stampedAt against now, which would blank the one state the mark exists to report.
func TestWorkflowMarkKeepsAPausedRunLitWhileItsWriterLives(t *testing.T) {
	const kiro = "sess_11111111-2222-3333-4444-555555555555"
	f := newWorkflowFixture(t)
	f.pairs["tab1"] = kiro
	// Both files are stamped weeks behind wall clock, so a window of any plausible size
	// refuses them.
	f.run("hash0", "wf_paused", "paused-need-input.json", kiro, tabStart.Add(time.Minute))
	f.beat("hash0", "wf_paused", 4242, tabStart.Add(time.Minute))
	f.watch.probeAlive = (&probeRecorder{alive: true}).probe

	f.watch.pass(t.Context(), liveTabs("tab1"))

	want := workflowMark{State: workflowMarkInput, Tally: workflowTally{Total: 1, Input: 1}}
	if got := f.watch.mark("tab1"); got != want {
		t.Errorf("mark(tab1) = %+v, want %+v: a paused run's beat is frozen by design", got, want)
	}
}

// TestWorkflowMarkFailsOpenWithoutAUsableRunBeat pins the hard invariant: only an explicit
// dead answer about a positive pid decoded from a readable run.beat withdraws a mark.
// Absence is the ordinary case -- 388 of 392 measured runs carry no run.beat, an older
// kiro-cli writes none, and a takeover releases one -- and the file is written without a
// temp-plus-rename, so a torn read is reachable; reading any of those as death would blank
// a live tab for a reason that is not about the run. Every leg runs a probe reporting EVERY
// pid dead, so only the fail-open path can leave the mark lit.
func TestWorkflowMarkFailsOpenWithoutAUsableRunBeat(t *testing.T) {
	const kiro = "sess_11111111-2222-3333-4444-555555555555"
	want := workflowMark{State: workflowMarkWorking, Tally: workflowTally{Total: 1, Working: 1}}

	t.Run("no run.beat at all", func(t *testing.T) {
		f := newWorkflowFixture(t)
		f.pairs["tab1"] = kiro
		f.run("hash0", "wf_open", "running.json", kiro, tabStart.Add(time.Minute))
		f.watch.probeAlive = (&probeRecorder{}).probe

		f.watch.pass(t.Context(), liveTabs("tab1"))

		if got := f.watch.mark("tab1"); got != want {
			t.Errorf("mark(tab1) = %+v with no run.beat staged, want %+v", got, want)
		}
	})

	cases := []struct {
		name string
		body string
	}{
		{"a torn write", `{"pid":40515`},
		{"a record naming no pid", `{}`},
		{"pid zero", `{"pid":0,"instanceId":"00000000-1111-2222-3333-444444444444"}`},
		{"a negative pid", `{"pid":-1,"instanceId":"00000000-1111-2222-3333-444444444444"}`},
		{"a body past the read bound", `{"pid":4242,"instanceId":"` + strings.Repeat("x", maxWorkflowBeatBytes) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newWorkflowFixture(t)
			f.pairs["tab1"] = kiro
			f.run("hash0", "wf_open", "running.json", kiro, tabStart.Add(time.Minute))
			f.beatBody("hash0", "wf_open", tc.body, tabStart.Add(time.Minute))
			probe := &probeRecorder{}
			f.watch.probeAlive = probe.probe

			f.watch.pass(t.Context(), liveTabs("tab1"))

			if got := f.watch.mark("tab1"); got != want {
				t.Errorf("mark(tab1) = %+v over a run.beat of %d bytes starting %.60q, want %+v", got, len(tc.body), tc.body, want)
			}
			if len(probe.pids) != 0 {
				t.Errorf("the probe was asked about %v, want no call: this record names no usable pid, and kill(0, 0) signals the caller's own process group", probe.pids)
			}
		})
	}
}

// TestWorkflowProcessAliveAnswersTheKernel drives the real probe against the real kernel.
// Every other case replaces it through the watcher's field, so without this the default
// could be wired to anything and the suite would stay green.
func TestWorkflowProcessAliveAnswersTheKernel(t *testing.T) {
	t.Run("this process reads alive", func(t *testing.T) {
		if !processAlive(os.Getpid()) {
			t.Errorf("processAlive(%d) = false, want true for the test binary's own pid", os.Getpid())
		}
	})

	t.Run("a non-positive pid reads alive", func(t *testing.T) {
		// pid 0 addresses the caller's own process group and a negative pid addresses a
		// group, so neither is evidence about a writer.
		for _, pid := range []int{0, -1} {
			if !processAlive(pid) {
				t.Errorf("processAlive(%d) = false, want true", pid)
			}
		}
	})

	t.Run("a pid the kernel cannot have allocated reads dead", func(t *testing.T) {
		pid := impossiblePID(t)
		if processAlive(pid) {
			t.Errorf("processAlive(%d) = true for pid_max, want false: the dead answer is the only thing that withdraws a mark", pid)
		}
	})

	t.Run("a process that exited and nobody reaped reads dead", func(t *testing.T) {
		// A zombie answers a signal-0 probe ALIVE, so without the state read a writer that died
		// under a live parent keeps a mark lit for as long as nothing reaps it.
		cmd := exec.Command("/bin/sleep", "300")
		if err := cmd.Start(); err != nil {
			t.Skipf("start a throwaway child: %v", err)
		}
		pid := cmd.Process.Pid
		// This test binary is the parent, so nothing collects the child until it does.
		t.Cleanup(func() { _ = cmd.Wait() })
		if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatalf("kill child %d: %v", pid, err)
		}
		// The state is read here rather than through processIsZombie, so the case does not
		// wait on its own subject.
		stat := filepath.Join("/proc", strconv.Itoa(pid), "stat")
		for deadline := time.Now().Add(10 * time.Second); ; {
			raw, err := os.ReadFile(stat)
			if err == nil && strings.Contains(string(raw), ") Z ") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("child %d never reached state Z, so the case cannot pin the zombie answer", pid)
			}
			time.Sleep(10 * time.Millisecond)
		}

		if processAlive(pid) {
			t.Errorf("processAlive(%d) = true for a zombie, want false: a task that has exited will never touch its run's beat again", pid)
		}
	})
}

// impossiblePID returns a pid the kernel never allocates -- pid_max itself, because the
// kernel's bound is exclusive -- so a case gets a deterministic dead pid where a reaped
// child's would carry a reuse race.
func impossiblePID(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile("/proc/sys/kernel/pid_max")
	if err != nil {
		t.Skipf("read pid_max: %v", err)
	}
	pidMax, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Skipf("parse pid_max %q: %v", raw, err)
	}
	return pidMax
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
