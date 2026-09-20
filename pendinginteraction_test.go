package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/web-terminal-engine/v6/terminal"
)

const (
	pendingFixtureSession = "sess_11111111-2222-3333-4444-555555555555"
	pendingFixtureStep    = "sess_a1b2c3d4-5566-7788-99aa-bbccddeeff00"
	pendingFixtureParent  = "sess_c0ffee01-2222-3333-4444-555555555555"
)

const pendingFixtureQuestion = "Allow `rm -rf build/` to run? PENDING-FIXTURE-QUESTION"

const rowStamp = "2006-01-02T15:04:05.000Z"

type fakeLatch struct {
	status string
	seq    uint64
}

type withdrawCall struct {
	tab  terminal.SessionID
	want string
	seq  uint64
	ok   bool
}

// fakeLatchStore mirrors the engine's withdrawal guard; relatch replaces a tab's latch right
// after the poller reads it, which is the window the sequence guard exists for.
type fakeLatchStore struct {
	latches  map[terminal.SessionID]fakeLatch
	relatch  map[terminal.SessionID]fakeLatch
	sessions []terminal.SessionInfo
	calls    []withdrawCall
}

func (s *fakeLatchStore) List() []terminal.SessionInfo { return s.sessions }

func (s *fakeLatchStore) StatusLatch(id terminal.SessionID) (string, uint64, bool) {
	l, ok := s.latches[id]
	if !ok {
		return "", 0, false
	}
	if next, armed := s.relatch[id]; armed {
		s.latches[id] = next
		delete(s.relatch, id)
	}
	return l.status, l.seq, true
}

func (s *fakeLatchStore) WithdrawStatusLatch(id terminal.SessionID, want string, seq uint64) bool {
	l, ok := s.latches[id]
	ok = ok && l.status == want && l.seq == seq && (want == terminal.StatusInput || want == terminal.StatusDone)
	if ok {
		s.latches[id] = fakeLatch{}
	}
	s.calls = append(s.calls, withdrawCall{tab: id, want: want, seq: seq, ok: ok})
	return ok
}

func (s *fakeLatchStore) SetSessionTitle(terminal.SessionID, string) bool { return true }

type pendingFixture struct {
	t        *testing.T
	watch    *pendingInteractionWatch
	store    *fakeLatchStore
	pairs    map[terminal.SessionID]string
	owners   [][]string
	home     string
	ownersOK bool
}

func newPendingFixture(t *testing.T) *pendingFixture {
	t.Helper()
	f := &pendingFixture{
		t:        t,
		store:    &fakeLatchStore{latches: map[terminal.SessionID]fakeLatch{}, relatch: map[terminal.SessionID]fakeLatch{}},
		pairs:    map[terminal.SessionID]string{},
		home:     t.TempDir(),
		ownersOK: true,
	}
	f.watch = newPendingInteractionWatch(f.home,
		func() map[terminal.SessionID]string { return f.pairs },
		func() ([][]string, bool) { return f.owners, f.ownersOK })
	return f
}

func (f *pendingFixture) latch(tab terminal.SessionID, status string, seq uint64) {
	f.store.sessions = append(f.store.sessions, terminal.SessionInfo{ID: tab})
	f.store.latches[tab] = fakeLatch{status: status, seq: seq}
}

func (f *pendingFixture) messages(hash, kiroID string, rows ...string) string {
	f.t.Helper()
	dir := filepath.Join(f.home, ".kiro", "sessions", hash, kiroID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		f.t.Fatalf("mkdir session %s: %v", kiroID, err)
	}
	path := filepath.Join(dir, messagesFileName)
	f.write(path, strings.Join(rows, "\n")+"\n")
	return path
}

func (f *pendingFixture) write(path, body string) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		f.t.Fatalf("write %s: %v", path, err)
	}
}

func (f *pendingFixture) appendRows(path string, rows ...string) {
	f.t.Helper()
	f.appendBytes(path, strings.Join(rows, "\n")+"\n")
}

func (f *pendingFixture) appendBytes(path, body string) {
	f.t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		f.t.Fatalf("open %s for append: %v", path, err)
	}
	if _, err := file.WriteString(body); err != nil {
		f.t.Fatalf("append to %s: %v", path, err)
	}
	if err := file.Close(); err != nil {
		f.t.Fatalf("close %s: %v", path, err)
	}
}

func (f *pendingFixture) pass() {
	f.watch.pass(f.t.Context(), f.store)
}

func pendingApprovalRow(toolCallID string, at time.Time) string {
	return fmt.Sprintf(`{"id":"%s-pending","timestamp":%q,"payload":{"type":"pending_interaction","interactionType":"tool_approval","toolCallId":%q,"executionId":%q,"question":%q,"options":[{"optionId":"accept","kind":"allow_once","name":"Allow once"},{"optionId":"reject","kind":"reject_once","name":"Reject"},{"optionId":"always-reject","kind":"reject_always","name":"Always reject"}]}}`,
		toolCallID, at.UTC().Format(rowStamp), toolCallID, toolCallID, pendingFixtureQuestion)
}

func pendingQuestionRow(toolCallID, executionID string, at time.Time) string {
	return fmt.Sprintf(`{"id":"%s-pending","timestamp":%q,"payload":{"type":"pending_interaction","interactionType":"user_input","toolCallId":%q,"executionId":%q,"question":%q,"options":[]}}`,
		toolCallID, at.UTC().Format(rowStamp), toolCallID, executionID, pendingFixtureQuestion)
}

func resolvedRow(toolCallID, selected string, at time.Time) string {
	return fmt.Sprintf(`{"id":"%s-resolved","timestamp":%q,"payload":{"type":"interaction_resolved","toolCallId":%q,"outcome":"selected","selectedOption":%q,"executionId":%q}}`,
		toolCallID, at.UTC().Format(rowStamp), toolCallID, selected, toolCallID)
}

func answeredRow(toolCallID, executionID string, at time.Time) string {
	return fmt.Sprintf(`{"id":"%s-resolved","timestamp":%q,"payload":{"type":"interaction_resolved","toolCallId":%q,"outcome":"answered","executionId":%q}}`,
		toolCallID, at.UTC().Format(rowStamp), toolCallID, executionID)
}

func eventRow(kind, executionID string, at time.Time) string {
	return fmt.Sprintf(`{"id":"%s-%d","timestamp":%q,"payload":{"type":%q,"executionId":%q}}`,
		kind, at.UnixNano(), at.UTC().Format(rowStamp), kind, executionID)
}

func turnEndRow(executionID, stopReason string, at time.Time) string {
	return fmt.Sprintf(`{"id":"turn-end-%d","timestamp":%q,"payload":{"type":"turn_end","executionId":%q,"stopReason":%q}}`,
		at.UnixNano(), at.UTC().Format(rowStamp), executionID, stopReason)
}

func TestPendingInteractionWithdrawsASettledAsk(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		rows []string
	}{
		{name: "an approval allowed", rows: []string{
			pendingApprovalRow("tool-1", now.Add(-2*time.Second)),
			resolvedRow("tool-1", "accept", now.Add(-time.Second)),
		}},
		{name: "an approval rejected", rows: []string{
			pendingApprovalRow("tool-1", now.Add(-2*time.Second)),
			resolvedRow("tool-1", "reject", now.Add(-time.Second)),
		}},
		{name: "a question answered", rows: []string{
			pendingQuestionRow("ask-1", "exec-1", now.Add(-2*time.Second)),
			answeredRow("ask-1", "exec-1", now.Add(-time.Second)),
		}},
		{name: "an ask cancelled with its turn", rows: []string{
			eventRow("turn_start", "exec-1", now.Add(-3*time.Second)),
			pendingApprovalRow("tool-1", now.Add(-2*time.Second)),
			turnEndRow("exec-1", "cancelled", now.Add(-time.Second)),
		}},
		{name: "two queued asks both resolved", rows: []string{
			pendingApprovalRow("tool-1", now.Add(-3*time.Second)),
			pendingApprovalRow("tool-2", now.Add(-3*time.Second)),
			resolvedRow("tool-1", "accept", now.Add(-2*time.Second)),
			resolvedRow("tool-2", "reject", now.Add(-time.Second)),
		}},
		{name: "a subagent ask recorded in the parent file", rows: []string{
			eventRow("sub_agent_start", "exec-1", now.Add(-4*time.Second)),
			pendingApprovalRow("sub-tool-1", now.Add(-2*time.Second)),
			resolvedRow("sub-tool-1", "accept", now.Add(-time.Second)),
			eventRow("sub_agent_complete", "exec-1", now),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPendingFixture(t)
			f.latch("tab1", terminal.StatusInput, 7)
			f.pairs["tab1"] = pendingFixtureSession
			f.messages("hash0", pendingFixtureSession, tc.rows...)

			f.pass()

			want := []withdrawCall{{tab: "tab1", want: terminal.StatusInput, seq: 7, ok: true}}
			if got := f.store.calls; len(got) != 1 || got[0] != want[0] {
				t.Errorf("withdraw calls = %+v, want %+v", got, want)
			}
			if got := f.store.latches["tab1"]; got != (fakeLatch{}) {
				t.Errorf("latch after the pass = %+v, want cleared", got)
			}
		})
	}
}

func TestPendingInteractionKeepsAnUnsettledOrUnprovenLatch(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name   string
		rows   []string
		owners [][]string
		unmap  bool
		reason string
	}{
		{
			name:   "no ask recorded at all",
			rows:   []string{eventRow("turn_start", "exec-1", now.Add(-time.Second)), turnEndRow("exec-1", "end_turn", now)},
			reason: "zero relevant rows is no evidence of settlement, only of an ask kind that writes none",
		},
		{
			name:   "one of two queued asks still open",
			rows:   []string{pendingApprovalRow("tool-1", now.Add(-2*time.Second)), pendingApprovalRow("tool-2", now.Add(-2*time.Second)), resolvedRow("tool-1", "accept", now.Add(-time.Second))},
			reason: "the second queued ask fires no new notification, so its row is the only thing holding the dot",
		},
		{
			name:   "only an ask older than the window",
			rows:   []string{pendingApprovalRow("tool-0", now.Add(-time.Hour)), resolvedRow("tool-0", "accept", now.Add(-time.Hour))},
			reason: "an ask settled an hour ago is not the ask that raised this latch",
		},
		{
			name:   "a settled ask on a tab with no mapping",
			rows:   []string{pendingApprovalRow("tool-1", now.Add(-2*time.Second)), resolvedRow("tool-1", "accept", now.Add(-time.Second))},
			unmap:  true,
			reason: "without a mapping no file is this tab's, so nothing can prove its ask settled",
		},
		{
			name:   "a settled ask while a step the tab owns has no session directory",
			rows:   []string{pendingApprovalRow("tool-1", now.Add(-2*time.Second)), resolvedRow("tool-1", "accept", now.Add(-time.Second))},
			owners: [][]string{{pendingFixtureSession, pendingFixtureStep}},
			reason: "a reachable file that cannot be found may hold the row that keeps the latch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPendingFixture(t)
			f.latch("tab1", terminal.StatusInput, 7)
			if !tc.unmap {
				f.pairs["tab1"] = pendingFixtureSession
			}
			f.owners = tc.owners
			f.messages("hash0", pendingFixtureSession, tc.rows...)

			f.pass()

			if len(f.store.calls) != 0 {
				t.Errorf("withdraw calls = %+v, want none: %s", f.store.calls, tc.reason)
			}
			if got := f.store.latches["tab1"]; got != (fakeLatch{status: terminal.StatusInput, seq: 7}) {
				t.Errorf("latch after the pass = %+v, want the input latch untouched", got)
			}
		})
	}
}

func TestPendingInteractionReadsOnlyLatchedTabs(t *testing.T) {
	now := time.Now()
	f := newPendingFixture(t)
	f.latch("idle", "", 0)
	f.latch("done", terminal.StatusDone, 3)
	f.pairs["idle"], f.pairs["done"] = pendingFixtureSession, pendingFixtureStep
	rows := []string{pendingApprovalRow("tool-1", now.Add(-2*time.Second)), resolvedRow("tool-1", "accept", now.Add(-time.Second))}
	f.messages("hash0", pendingFixtureSession, rows...)
	f.messages("hash0", pendingFixtureStep, rows...)

	f.pass()

	if len(f.store.calls) != 0 {
		t.Errorf("withdraw calls = %+v, want none for an idle and a done tab", f.store.calls)
	}
	if got := len(f.watch.state.tails); got != 0 {
		t.Errorf("tails after the pass = %d, want 0: a tab whose latch is not input must cost no file read", got)
	}
}

func TestPendingInteractionReachesAStepThroughRunOwners(t *testing.T) {
	const strangerStep = "sess_b2c3d4e5-6677-8899-aabb-ccddeeff0011"
	const strangerParent = "sess_99999999-8888-7777-6666-555555555555"
	now := time.Now()
	f := newPendingFixture(t)
	f.latch("tab1", terminal.StatusInput, 7)
	f.pairs["tab1"] = pendingFixtureSession
	f.owners = [][]string{
		{pendingFixtureParent, pendingFixtureSession, pendingFixtureStep},
		{strangerParent, strangerStep},
	}
	f.messages("hash0", pendingFixtureSession, eventRow("turn_start", "exec-1", now.Add(-time.Hour)))
	f.messages("hash0", pendingFixtureParent, eventRow("turn_start", "exec-0", now.Add(-time.Hour)))
	f.messages("hash1", pendingFixtureStep,
		pendingApprovalRow("step-tool-1", now.Add(-2*time.Second)),
		resolvedRow("step-tool-1", "accept", now.Add(-time.Second)))
	f.messages("hash1", strangerStep, pendingApprovalRow("stranger-tool-1", now.Add(-2*time.Second)))

	f.pass()

	want := []withdrawCall{{tab: "tab1", want: terminal.StatusInput, seq: 7, ok: true}}
	if got := f.store.calls; len(got) != 1 || got[0] != want[0] {
		t.Errorf("withdraw calls = %+v, want %+v: the step's settled ask is reachable only through the run owners", got, want)
	}
	if _, read := f.watch.state.paths[strangerStep]; read {
		t.Errorf("resolved %s, want it never read: no run containing the tab's session names it", strangerStep)
	}
}

func TestPendingInteractionANewSequenceNeedsItsOwnRow(t *testing.T) {
	settle := func(t *testing.T, f *pendingFixture, path string, calls int) {
		t.Helper()
		f.appendRows(path, pendingApprovalRow("tool-2", time.Now()))
		f.pass()
		if got := f.store.calls; len(got) != calls {
			t.Fatalf("withdraw calls with the second ask open = %+v, want %d", got, calls)
		}
		f.appendRows(path, resolvedRow("tool-2", "accept", time.Now()))
		f.pass()
		settled := withdrawCall{tab: "tab1", want: terminal.StatusInput, seq: 6, ok: true}
		if got := f.store.calls; len(got) != calls+1 || got[calls] != settled {
			t.Errorf("withdraw calls once the second ask settled = %+v, want %+v last", got, settled)
		}
	}

	t.Run("after a withdrawal", func(t *testing.T) {
		now := time.Now()
		f := newPendingFixture(t)
		f.latch("tab1", terminal.StatusInput, 5)
		f.pairs["tab1"] = pendingFixtureSession
		path := f.messages("hash0", pendingFixtureSession,
			pendingApprovalRow("tool-1", now.Add(-2*time.Second)),
			resolvedRow("tool-1", "accept", now.Add(-time.Second)))

		f.pass()
		if got := f.store.calls; len(got) != 1 || !got[0].ok {
			t.Fatalf("withdraw calls for the first ask = %+v, want one accepted call", got)
		}
		f.pass()

		f.store.latches["tab1"] = fakeLatch{status: terminal.StatusInput, seq: 6}
		f.pass()
		if got := f.store.calls; len(got) != 1 {
			t.Fatalf("withdraw calls for a new sequence with no new row = %+v, want none: the settled row was the first latch's", got)
		}

		settle(t, f, path, 1)
	})

	t.Run("after a withdrawal, when the settled ask's turn then ends", func(t *testing.T) {
		now := time.Now()
		f := newPendingFixture(t)
		f.latch("tab1", terminal.StatusInput, 5)
		f.pairs["tab1"] = pendingFixtureSession
		path := f.messages("hash0", pendingFixtureSession,
			pendingApprovalRow("tool-1", now.Add(-2*time.Second)),
			resolvedRow("tool-1", "accept", now.Add(-time.Second)))

		f.pass()
		if got := f.store.calls; len(got) != 1 || !got[0].ok {
			t.Fatalf("withdraw calls for the first ask = %+v, want one accepted call", got)
		}
		f.pass()

		f.appendRows(path, turnEndRow("exec-1", "end_turn", time.Now()))
		f.store.latches["tab1"] = fakeLatch{status: terminal.StatusInput, seq: 6}
		f.pass()
		if got := f.store.calls; len(got) != 1 {
			t.Fatalf("withdraw calls for a new sequence after the old turn ended = %+v, want none: the turn's end settles nothing that was already settled", got)
		}

		settle(t, f, path, 1)
	})

	t.Run("after a refused withdrawal", func(t *testing.T) {
		now := time.Now()
		f := newPendingFixture(t)
		f.latch("tab1", terminal.StatusInput, 5)
		f.pairs["tab1"] = pendingFixtureSession
		path := f.messages("hash0", pendingFixtureSession,
			pendingApprovalRow("tool-1", now.Add(-2*time.Second)),
			resolvedRow("tool-1", "accept", now.Add(-time.Second)))
		f.store.relatch["tab1"] = fakeLatch{status: terminal.StatusInput, seq: 6}

		f.pass()
		refused := withdrawCall{tab: "tab1", want: terminal.StatusInput, seq: 5, ok: false}
		if got := f.store.calls; len(got) != 1 || got[0] != refused {
			t.Fatalf("withdraw calls after the re-latch = %+v, want the stale %+v refused", got, refused)
		}
		if got := f.store.latches["tab1"]; got != (fakeLatch{status: terminal.StatusInput, seq: 6}) {
			t.Fatalf("latch after the refused withdrawal = %+v, want the newer notification's latch standing", got)
		}

		f.pass()
		if got := f.store.calls; len(got) != 1 {
			t.Fatalf("withdraw calls for the new sequence with no new row = %+v, want no new call", got)
		}

		settle(t, f, path, 1)
	})
}

func TestPendingInteractionTailsIncrementally(t *testing.T) {
	t.Run("a torn trailing row waits for its newline", func(t *testing.T) {
		now := time.Now()
		f := newPendingFixture(t)
		f.latch("tab1", terminal.StatusInput, 7)
		f.pairs["tab1"] = pendingFixtureSession
		path := f.messages("hash0", pendingFixtureSession, pendingApprovalRow("tool-1", now.Add(-2*time.Second)))
		resolved := resolvedRow("tool-1", "accept", now.Add(-time.Second))
		f.appendBytes(path, resolved[:len(resolved)/2])

		f.pass()
		if len(f.store.calls) != 0 {
			t.Fatalf("withdraw calls with the resolved row torn = %+v, want none", f.store.calls)
		}

		f.appendBytes(path, resolved[len(resolved)/2:]+"\n")
		f.pass()
		if got := f.store.calls; len(got) != 1 || !got[0].ok {
			t.Errorf("withdraw calls once the row completed = %+v, want one accepted call", got)
		}
	})

	t.Run("a torn row after a settled ask may be the ask that holds the dot", func(t *testing.T) {
		now := time.Now()
		f := newPendingFixture(t)
		f.latch("tab1", terminal.StatusInput, 7)
		f.pairs["tab1"] = pendingFixtureSession
		path := f.messages("hash0", pendingFixtureSession,
			pendingApprovalRow("tool-1", now.Add(-2*time.Second)),
			resolvedRow("tool-1", "accept", now.Add(-time.Second)))
		second := pendingApprovalRow("tool-2", now)
		f.appendBytes(path, second[:len(second)/2])

		f.pass()
		if len(f.store.calls) != 0 {
			t.Fatalf("withdraw calls with a torn row after the settled ask = %+v, want none", f.store.calls)
		}

		f.appendBytes(path, second[len(second)/2:]+"\n")
		f.pass()
		if len(f.store.calls) != 0 {
			t.Fatalf("withdraw calls with the second ask open = %+v, want none", f.store.calls)
		}

		f.appendRows(path, resolvedRow("tool-2", "accept", time.Now()))
		f.pass()
		if got := f.store.calls; len(got) != 1 || !got[0].ok {
			t.Errorf("withdraw calls once the second ask settled = %+v, want one accepted call", got)
		}
	})

	t.Run("appended rows are read from the cursor", func(t *testing.T) {
		now := time.Now()
		f := newPendingFixture(t)
		f.latch("tab1", terminal.StatusInput, 7)
		f.pairs["tab1"] = pendingFixtureSession
		path := f.messages("hash0", pendingFixtureSession, pendingApprovalRow("tool-1", now.Add(-2*time.Second)))

		f.pass()
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := f.watch.state.tails[path].offset; got != fi.Size() {
			t.Fatalf("cursor after the first read = %d, want the file size %d", got, fi.Size())
		}

		f.appendRows(path, resolvedRow("tool-1", "accept", time.Now()))
		f.pass()
		if got := f.store.calls; len(got) != 1 || !got[0].ok {
			t.Errorf("withdraw calls after the appended resolution = %+v, want one accepted call", got)
		}
	})

	t.Run("a file truncated below the cursor is read again", func(t *testing.T) {
		now := time.Now()
		f := newPendingFixture(t)
		f.latch("tab1", terminal.StatusInput, 7)
		f.pairs["tab1"] = pendingFixtureSession
		filler := make([]string, 8)
		for i := range filler {
			filler[i] = eventRow("turn_start", fmt.Sprintf("exec-%d", i), now.Add(-time.Hour))
		}
		path := f.messages("hash0", pendingFixtureSession, append(filler, pendingApprovalRow("tool-1", now.Add(-2*time.Second)))...)

		f.pass()
		if len(f.store.calls) != 0 {
			t.Fatalf("withdraw calls with the ask open = %+v, want none", f.store.calls)
		}

		f.write(path, pendingApprovalRow("tool-1", now.Add(-2*time.Second))+"\n"+resolvedRow("tool-1", "accept", now.Add(-time.Second))+"\n")
		f.pass()
		if got := f.store.calls; len(got) != 1 || !got[0].ok {
			t.Errorf("withdraw calls after the rewrite = %+v, want one accepted call: a size below the cursor resets it", got)
		}
	})

	t.Run("a file replaced under the cursor is read again", func(t *testing.T) {
		now := time.Now()
		f := newPendingFixture(t)
		f.latch("tab1", terminal.StatusInput, 7)
		f.pairs["tab1"] = pendingFixtureSession
		path := f.messages("hash0", pendingFixtureSession, pendingApprovalRow("tool-1", now.Add(-2*time.Second)))

		f.pass()
		if len(f.store.calls) != 0 {
			t.Fatalf("withdraw calls with the ask open = %+v, want none", f.store.calls)
		}

		// The second ask's row is byte-for-byte as long as the first's, so a cursor carried
		// onto the new file would land exactly on the resolution and miss the open ask.
		replacement := filepath.Join(f.t.TempDir(), messagesFileName)
		f.write(replacement, pendingApprovalRow("tool-2", now.Add(-2*time.Second))+"\n"+
			resolvedRow("tool-1", "accept", now.Add(-time.Second))+"\n")
		if err := os.Rename(replacement, path); err != nil {
			t.Fatalf("replace %s: %v", path, err)
		}
		f.pass()
		if len(f.store.calls) != 0 {
			t.Fatalf("withdraw calls after the replacement = %+v, want none: a new file is read from its start, and its ask is open", f.store.calls)
		}

		f.appendRows(path, resolvedRow("tool-2", "accept", time.Now()))
		f.pass()
		if got := f.store.calls; len(got) != 1 || !got[0].ok {
			t.Errorf("withdraw calls once the new file's ask settled = %+v, want one accepted call: the tail follows the new file", got)
		}
	})
}

func TestPendingInteractionTailReportsAFileChangedSinceItsRead(t *testing.T) {
	now := time.Now()
	f := newPendingFixture(t)
	f.latch("tab1", terminal.StatusInput, 7)
	f.pairs["tab1"] = pendingFixtureSession
	path := f.messages("hash0", pendingFixtureSession, pendingApprovalRow("tool-1", now.Add(-2*time.Second)))
	f.pass()
	tail := f.watch.state.tails[path]
	if tail == nil {
		t.Fatalf("no tail for %s after the pass", path)
	}
	if !tail.unchanged(path) {
		t.Errorf("unchanged(%s) = false right after the read, want true", path)
	}

	f.appendRows(path, pendingApprovalRow("tool-2", now))
	if tail.unchanged(path) {
		t.Errorf("unchanged(%s) = true after an appended row, want false", path)
	}

	f.pass()
	if !tail.unchanged(path) {
		t.Fatalf("unchanged(%s) = false after the next read, want true", path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	replacement := filepath.Join(f.t.TempDir(), messagesFileName)
	f.write(replacement, strings.Repeat("x", int(fi.Size())))
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("replace %s: %v", path, err)
	}
	if tail.unchanged(path) {
		t.Errorf("unchanged(%s) = true after a same-size replacement, want false", path)
	}
}

func TestPendingInteractionRefusesWhatItCannotRead(t *testing.T) {
	now := time.Now()
	settled := []string{
		pendingApprovalRow("tool-1", now.Add(-2*time.Second)),
		resolvedRow("tool-1", "accept", now.Add(-time.Second)),
	}
	cases := []struct {
		name   string
		plant  func(f *pendingFixture, path string)
		reason string
	}{
		{
			name: "a messages log that is a symlink to a settled one",
			plant: func(f *pendingFixture, path string) {
				target := f.messages("elsewhere", pendingFixtureStep, settled...)
				if err := os.Remove(path); err != nil {
					f.t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					f.t.Fatal(err)
				}
			},
			reason: "a planted link is refused at open, whatever its target says",
		},
		{
			name: "a messages log over the byte bound",
			plant: func(f *pendingFixture, path string) {
				pad := `{"id":"pad","timestamp":"2026-09-01T10:00:00.000Z","payload":{"type":"tool_call","content":"` + strings.Repeat("x", maxMessagesRowBytes-256) + `"}}`
				for range maxMessagesFileBytes/maxMessagesRowBytes + 1 {
					f.appendRows(path, pad)
				}
			},
			reason: "an over-bound file is refused whole, never read in part",
		},
		{
			name: "a row over the row bound after the settled ask",
			plant: func(f *pendingFixture, path string) {
				f.appendRows(path, `{"id":"huge","timestamp":"2026-09-01T10:00:00.000Z","payload":{"type":"tool_call","content":"`+strings.Repeat("x", maxMessagesRowBytes)+`"}}`)
			},
			reason: "the cursor must not pass a row it could not read",
		},
		{
			name: "a row that does not decode after the settled ask",
			plant: func(f *pendingFixture, path string) {
				f.appendRows(path, `{"id":"torn","timestamp":`)
			},
			reason: "the cursor must not pass a row it could not decode",
		},
		{
			name: "a pending row with no toolCallId after the settled ask",
			plant: func(f *pendingFixture, path string) {
				f.appendRows(path, `{"id":"x-pending","timestamp":"2026-09-01T10:00:00.000Z","payload":{"type":"pending_interaction","question":"?"}}`)
			},
			reason: "a pending row nothing can join to a resolution can never be proven settled",
		},
		{
			name: "more pending rows than the cap",
			plant: func(f *pendingFixture, path string) {
				rows := make([]string, 0, maxPendingRowsPerFile)
				for i := range maxPendingRowsPerFile {
					rows = append(rows, pendingApprovalRow(fmt.Sprintf("flood-%d", i), now.Add(-time.Hour)))
				}
				f.appendRows(path, rows...)
			},
			reason: "the file is written by a process this app does not run, so its row count is bounded here",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPendingFixture(t)
			f.latch("tab1", terminal.StatusInput, 7)
			f.pairs["tab1"] = pendingFixtureSession
			tc.plant(f, f.messages("hash0", pendingFixtureSession, settled...))

			f.pass()

			if len(f.store.calls) != 0 {
				t.Errorf("withdraw calls = %+v, want none: %s", f.store.calls, tc.reason)
			}
		})
	}
}

// slog.Default is process-global, so this test is not parallel.
func TestPendingInteractionRecordsTheOutcomeAndNothingFromTheFile(t *testing.T) {
	const tab terminal.SessionID = "3f7a1c4e5b6d8092a1b2c3d4e5f60718"
	const toolCallID = "toolu_01HXAMPLE7QUESTIONABLE"
	now := time.Now()
	f := newPendingFixture(t)
	f.latch(tab, terminal.StatusInput, 7)
	f.pairs[tab] = pendingFixtureSession
	f.messages("hash0", pendingFixtureSession,
		pendingApprovalRow(toolCallID, now.Add(-2*time.Second)),
		resolvedRow(toolCallID, "accept", now.Add(-time.Second)))

	logged := capturedLog(t)
	f.pass()

	out := logged.String()
	if !strings.Contains(out, "withdrew a settled input latch") || !strings.Contains(out, "settled=1") {
		t.Errorf("logged %q, want one record of the withdrawal with its settled count", out)
	}
	if !strings.Contains(out, terminal.LogID(tab)) {
		t.Errorf("logged %q, want the tab named by LogID so a withdrawal is correlatable", out)
	}
	for _, secret := range []string{string(tab), toolCallID, pendingFixtureQuestion, "Allow once", "Always reject"} {
		if strings.Contains(out, secret) {
			t.Errorf("logged %q carrying %q, want it kept out of every sink", out, secret)
		}
	}
}

func TestPendingInteractionKeepsTheLatchWhileRunOwnersAreUnavailable(t *testing.T) {
	// The mapped session's ask is settled and a step of the same run still asks; only the
	// run owners name the step's file.
	plant := func(f *pendingFixture) (stepPath string) {
		now := time.Now()
		f.latch("tab1", terminal.StatusInput, 7)
		f.pairs["tab1"] = pendingFixtureSession
		f.messages("hash0", pendingFixtureSession,
			pendingApprovalRow("tool-1", now.Add(-2*time.Second)),
			resolvedRow("tool-1", "accept", now.Add(-time.Second)))
		return f.messages("hash1", pendingFixtureStep, pendingApprovalRow("step-tool-1", now.Add(-2*time.Second)))
	}
	settlesOnceOwnersAreBack := func(t *testing.T, f *pendingFixture, stepPath string) {
		t.Helper()
		f.owners, f.ownersOK = [][]string{{pendingFixtureSession, pendingFixtureStep}}, true
		f.pass()
		if len(f.store.calls) != 0 {
			t.Fatalf("withdraw calls with the step's ask open = %+v, want none", f.store.calls)
		}
		f.appendRows(stepPath, resolvedRow("step-tool-1", "accept", time.Now()))
		f.pass()
		want := withdrawCall{tab: "tab1", want: terminal.StatusInput, seq: 7, ok: true}
		if got := f.store.calls; len(got) != 1 || got[0] != want {
			t.Errorf("withdraw calls once the step's ask settled = %+v, want [%+v]", got, want)
		}
	}

	t.Run("before the workflow sweep has published", func(t *testing.T) {
		f := newPendingFixture(t)
		stepPath := plant(f)
		f.owners, f.ownersOK = nil, false

		f.pass()

		if len(f.store.calls) != 0 {
			t.Fatalf("withdraw calls with no owner publication = %+v, want none: the mapped file alone cannot prove the step is not asking", f.store.calls)
		}
		settlesOnceOwnersAreBack(t, f, stepPath)
	})

	t.Run("after a run scan that could not read every run", func(t *testing.T) {
		f := newPendingFixture(t)
		stepPath := plant(f)
		f.owners, f.ownersOK = [][]string{{pendingFixtureSession}}, false

		f.pass()

		if len(f.store.calls) != 0 {
			t.Fatalf("withdraw calls over an incomplete owner set = %+v, want none: a run the scan missed may hold the open ask", f.store.calls)
		}
		settlesOnceOwnersAreBack(t, f, stepPath)
	})

	t.Run("a row settled before the outage is not re-read as fresh proof", func(t *testing.T) {
		now := time.Now()
		f := newPendingFixture(t)
		f.latch("tab1", terminal.StatusInput, 5)
		f.pairs["tab1"] = pendingFixtureSession
		path := f.messages("hash0", pendingFixtureSession,
			pendingApprovalRow("tool-1", now.Add(-2*time.Second)),
			resolvedRow("tool-1", "accept", now.Add(-time.Second)))
		f.pass()
		if got := f.store.calls; len(got) != 1 || !got[0].ok {
			t.Fatalf("withdraw calls for the first ask = %+v, want one accepted call", got)
		}

		f.store.latches["tab1"] = fakeLatch{status: terminal.StatusInput, seq: 6}
		f.ownersOK = false
		f.pass()
		f.ownersOK = true
		f.pass()

		if got := f.store.calls; len(got) != 1 {
			t.Fatalf("withdraw calls for a new sequence across an owner outage = %+v, want no new call: the tail must survive the outage, or the old row is re-stamped into the new arm", got)
		}
		f.appendRows(path, pendingApprovalRow("tool-2", time.Now()), resolvedRow("tool-2", "accept", time.Now()))
		f.pass()
		want := withdrawCall{tab: "tab1", want: terminal.StatusInput, seq: 6, ok: true}
		if got := f.store.calls; len(got) != 2 || got[1] != want {
			t.Errorf("withdraw calls once the new ask settled = %+v, want %+v last", got, want)
		}
	})
}

func TestPendingInteractionKeepsATailWhileItsTabLives(t *testing.T) {
	t.Run("a row settled before the tab lost its mapping is not re-read as fresh proof", func(t *testing.T) {
		now := time.Now()
		f := newPendingFixture(t)
		f.latch("tab1", terminal.StatusInput, 5)
		f.pairs["tab1"] = pendingFixtureSession
		path := f.messages("hash0", pendingFixtureSession,
			pendingApprovalRow("tool-1", now.Add(-2*time.Second)),
			resolvedRow("tool-1", "accept", now.Add(-time.Second)))
		f.pass()
		if got := f.store.calls; len(got) != 1 || !got[0].ok {
			t.Fatalf("withdraw calls for the first ask = %+v, want one accepted call", got)
		}

		f.store.latches["tab1"] = fakeLatch{status: terminal.StatusInput, seq: 6}
		delete(f.pairs, "tab1")
		f.pass()
		f.pairs["tab1"] = pendingFixtureSession
		f.pass()

		if got := f.store.calls; len(got) != 1 {
			t.Fatalf("withdraw calls for a new sequence across a mapping outage = %+v, want no new call: the tail must survive the pass in which the tab had no mapping", got)
		}
		f.appendRows(path, pendingApprovalRow("tool-2", time.Now()), resolvedRow("tool-2", "accept", time.Now()))
		f.pass()
		want := withdrawCall{tab: "tab1", want: terminal.StatusInput, seq: 6, ok: true}
		if got := f.store.calls; len(got) != 2 || got[1] != want {
			t.Errorf("withdraw calls once the new ask settled = %+v, want %+v last", got, want)
		}
	})

	t.Run("a closed tab's tails and paths are dropped and a live tab's kept", func(t *testing.T) {
		now := time.Now()
		f := newPendingFixture(t)
		f.latch("tab1", terminal.StatusInput, 7)
		f.latch("tab2", terminal.StatusInput, 9)
		f.pairs["tab1"], f.pairs["tab2"] = pendingFixtureSession, pendingFixtureStep
		f.messages("hash0", pendingFixtureSession, pendingApprovalRow("tool-1", now.Add(-2*time.Second)))
		second := f.messages("hash0", pendingFixtureStep, pendingApprovalRow("tool-2", now.Add(-2*time.Second)))
		f.pass()
		if got := len(f.watch.state.tails); got != 2 {
			t.Fatalf("tails after both tabs read = %d, want 2", got)
		}

		f.store.sessions = f.store.sessions[1:]
		f.pass()

		if _, kept := f.watch.state.tails[second]; !kept || len(f.watch.state.tails) != 1 {
			t.Errorf("tails after tab1 closed = %d holding tab2's file: %v, want only tab2's file kept", len(f.watch.state.tails), kept)
		}
		if _, kept := f.watch.state.paths[pendingFixtureStep]; !kept || len(f.watch.state.paths) != 1 {
			t.Errorf("resolved paths after tab1 closed = %d holding tab2's session: %v, want only tab2's session kept", len(f.watch.state.paths), kept)
		}
	})
}

// tab1's row settles one pass after its arm and is proven complete a pass later still, so the
// withdrawal is right only if tab1 kept its first-pass arm while tab2 stood armed on another
// sequence.
func TestPendingInteractionArmsEachTabOnItsOwn(t *testing.T) {
	now := time.Now()
	f := newPendingFixture(t)
	f.latch("tab1", terminal.StatusInput, 7)
	f.latch("tab2", terminal.StatusInput, 9)
	f.pairs["tab1"], f.pairs["tab2"] = pendingFixtureSession, pendingFixtureStep
	first := f.messages("hash0", pendingFixtureSession, pendingApprovalRow("tool-1", now.Add(-2*time.Second)))
	second := f.messages("hash0", pendingFixtureStep, pendingApprovalRow("tool-2", now.Add(-2*time.Second)))
	f.pass()
	if len(f.store.calls) != 0 {
		t.Fatalf("withdraw calls with both asks open = %+v, want none", f.store.calls)
	}

	torn := eventRow("turn_start", "exec-2", now)
	f.appendRows(first, resolvedRow("tool-1", "accept", now.Add(-time.Second)))
	f.appendBytes(first, torn[:len(torn)/2])
	f.pass()
	if len(f.store.calls) != 0 {
		t.Fatalf("withdraw calls with a torn row after tab1's resolution = %+v, want none", f.store.calls)
	}

	f.appendBytes(first, torn[len(torn)/2:]+"\n")
	f.pass()
	want := []withdrawCall{{tab: "tab1", want: terminal.StatusInput, seq: 7, ok: true}}
	if got := f.store.calls; !slices.Equal(got, want) {
		t.Fatalf("withdraw calls once tab1's file was complete = %+v, want %+v: tab1's resolution settled under the arm it took two passes ago", got, want)
	}

	f.appendRows(second, resolvedRow("tool-2", "accept", time.Now()))
	f.pass()
	want = append(want, withdrawCall{tab: "tab2", want: terminal.StatusInput, seq: 9, ok: true})
	if got := f.store.calls; !slices.Equal(got, want) {
		t.Errorf("withdraw calls once tab2's ask settled = %+v, want %+v", got, want)
	}
}
