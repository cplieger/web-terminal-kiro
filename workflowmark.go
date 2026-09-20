package main

// A kiro-cli workflow run reaches the terminal byte stream through no channel at all --
// neither pause nor needs-input produces an OSC 9 notification -- so the signal is read from
// $HOME/.kiro/sessions/<workspace-hash>/workflows/<workflowId>/workflow-state.json instead.

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cplieger/runesafe/v2"
	"github.com/cplieger/web-terminal-engine/v6/terminal"
)

const (
	workflowMarkWorking = "working"
	workflowMarkWaiting = "waiting"
	workflowMarkInput   = "input"

	// A CLOSED allowlist: an unknown future value must default to no mark rather than to a
	// lit one, which a "status != completed" test could not express.
	workflowStatusRunning   = "running"
	workflowStatusPaused    = "paused"
	workflowStatusCompleted = "completed"
	workflowStatusFailed    = "failed"
	workflowStatusAborted   = "aborted"

	workflowSignalNeedInput = "need_input"

	workflowsDirName      = "workflows"
	workflowStateFileName = "workflow-state.json"
	workflowBeatFileName  = "run.beat"

	// Generous because capturedOutputs holds one free-text agent report per step, and the
	// read REFUSES an over-bound file rather than truncating it, so too small a bound drops
	// the biggest runs outright.
	maxWorkflowStateBytes = 4 << 20

	// Real beats measure 102-106 bytes; the bound exists because the file is written by a
	// process this app does not run, and an over-bound read admits the run.
	maxWorkflowBeatBytes = 4 << 10

	// The largest real tree measured holds 30 nodes at depth 5; the bound exists because a
	// corrupt or hostile file can nest without limit and the walk runs per changed file.
	maxWorkflowNodes = 1024

	unknownWorkflowStatusCap  = 8
	maxWorkflowStatusLogBytes = 64
)

// workflowState is the NARROW decode of one workflow-state.json. Every other field the
// record carries -- workflowName, runLabel, inputs, artifacts, capturedOutputs -- is
// deliberately absent: none of them is decoded, so none of them can reach a log sink, which
// is what keeps the sanitize question answerable in one place (noteUnknownStatus). The two
// id families that ARE decoded, parentSessionId and each node's sessionId, are gated by
// validKiroSessionID before they are used and never logged, so the invariant survives them.
type workflowState struct {
	Status          string       `json:"status"`
	ParentSessionID string       `json:"parentSessionId"`
	Root            workflowNode `json:"root"`
}

type workflowNode struct {
	CompletionSignal string `json:"completionSignal"`
	// The kiro session KAS created for this step. Decoded because the tab's mapping names
	// the RUNNING step's session, not the launching one, for most of a run's life.
	SessionID string         `json:"sessionId"`
	Children  []workflowNode `json:"children"`
}

// workflowBeat is the NARROW decode of one run.beat, for workflowState's reason: neither
// instanceId nor stampedAt can decide liveness from here, so neither reaches a log sink.
type workflowBeat struct {
	PID int `json:"pid"`
}

// workflowTally is the census behind one tab's fold.
type workflowTally struct{ Total, Working, Waiting, Input int }

// workflowMark is one tab's folded secondary state plus that census.
type workflowMark struct {
	State string
	Tally workflowTally
}

// classifyWorkflowState maps one run to its mark state. ok=false means the run contributes
// nothing: a terminal status, or one this build does not recognise.
func classifyWorkflowState(st *workflowState) (state string, ok bool) {
	switch st.Status {
	case workflowStatusRunning:
		return workflowMarkWorking, true
	case workflowStatusPaused:
		if anyNodeNeedsInput(&st.Root) {
			return workflowMarkInput, true
		}
		return workflowMarkWaiting, true
	default:
		return "", false
	}
}

// knownWorkflowStatus reports whether a status is one of the five measured values. It is
// separate from the classifier because a TERMINAL status and an UNKNOWN one both contribute
// nothing while only the second is worth a log line.
func knownWorkflowStatus(status string) bool {
	switch status {
	case workflowStatusRunning, workflowStatusPaused, workflowStatusCompleted,
		workflowStatusFailed, workflowStatusAborted:
		return true
	}
	return false
}

// walkWorkflowNodes visits every node of one run's tree until visit answers true. Iterative
// with an explicit stack and a node budget, so a corrupt deeply-nested file cannot spend
// unbounded work; the budget is spent one node per POP. exhausted is true when the budget
// ran out with nodes still unvisited, so a caller can tell a partial answer from a whole one.
func walkWorkflowNodes(root *workflowNode, visit func(*workflowNode) bool) (exhausted bool) {
	stack := []*workflowNode{root}
	for budget := maxWorkflowNodes; len(stack) > 0 && budget > 0; budget-- {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visit(node) {
			return false
		}
		for i := range node.Children {
			stack = append(stack, &node.Children[i])
		}
	}
	return len(stack) > 0
}

// anyNodeNeedsInput reports whether any node carries completionSignal == "need_input".
// Exhausting the walk's budget answers false, which reads as waiting rather than claiming the
// user's attention. completionSignalSource is deliberately not decoded: every signalled node
// in the measured corpus carries source=send_message, so it discriminates nothing.
func anyNodeNeedsInput(root *workflowNode) bool {
	needsInput := false
	walkWorkflowNodes(root, func(node *workflowNode) bool {
		needsInput = node.CompletionSignal == workflowSignalNeedInput
		return needsInput
	})
	return needsInput
}

// runSessionIDs returns the kiro sessions that OWN one run: the launching session first, then
// every step's own session. Both families are the run, because KAS creates a step's session
// inside the launching tab's kiro-cli process, and a COMPLETED step keeps its sessionId in the
// tree -- which is what holds the mark lit over the gap between one step ending and the next
// one prompting. Deduplicated, because a real record repeats one step's session id across
// nodes and a repeat would count the same run twice in one tab's tally. partial is true when
// the node budget stopped the walk, so owners may omit a step the record names.
func runSessionIDs(st *workflowState) (owners []string, partial bool) {
	seen := make(map[string]struct{}, 4)
	owners = make([]string, 0, 4)
	admit := func(id string) {
		if _, dup := seen[id]; dup || !validKiroSessionID(id) {
			return
		}
		seen[id] = struct{}{}
		owners = append(owners, id)
	}
	admit(st.ParentSessionID)
	partial = walkWorkflowNodes(&st.Root, func(node *workflowNode) bool {
		admit(node.SessionID)
		return false
	})
	return owners, partial
}

// foldWorkflowMarks folds one tab's admitted runs. Precedence input > waiting > working.
func foldWorkflowMarks(states []string) workflowMark {
	var mark workflowMark
	for _, state := range states {
		switch state {
		case workflowMarkWorking:
			mark.Tally.Working++
		case workflowMarkWaiting:
			mark.Tally.Waiting++
		case workflowMarkInput:
			mark.Tally.Input++
		default:
			continue
		}
		mark.Tally.Total++
	}
	switch {
	case mark.Tally.Input > 0:
		mark.State = workflowMarkInput
	case mark.Tally.Waiting > 0:
		mark.State = workflowMarkWaiting
	case mark.Tally.Working > 0:
		mark.State = workflowMarkWorking
	}
	return mark
}

// workflowSessions is the engine surface this needs: the live tab set with each tab's start
// time (the admission rule's third clause). An interface, not the concrete manager, so the
// watcher is testable without a PTY.
type workflowSessions interface{ List() []terminal.SessionInfo }

// workflowVerdict is one state file's classification, cached under the identity the pass
// observed. An empty state lights no mark; owners with an empty state is a run whose status
// this build cannot place, kept reachable because its steps may still be asking. partial
// marks owners that may omit a session the record names: the bytes did not decode and an
// earlier pass's answer is carried, or the tree ran past the node budget.
type workflowVerdict struct {
	modTime time.Time
	state   string
	owners  []string
	size    int64
	partial bool
}

var errWorkflowStateIrregular = errors.New("workflow state is not a regular file")

// workflowWatch reports what a workflow run launched in a tab is doing, by polling
// kiro-cli's own on-disk run state. It owns no tab list: the live set is the engine's and
// the tab -> kiro-session pairing is the title poller's, so a tab with no mapping simply
// carries no mark.
type workflowWatch struct {
	// Replaced per pass, never mutated in place, so a reader needs no lock.
	marks      atomic.Pointer[map[terminal.SessionID]workflowMark]
	mapping    func() map[terminal.SessionID]string
	probeAlive func(pid int) bool
	// Everything below is touched only by the poller goroutine, so none of it needs a lock.
	verdicts        map[string]workflowVerdict
	unknownStatuses map[string]struct{}
	sessionsRoot    string
	owners          [][]string
	ownersComplete  bool
}

// newWorkflowWatch builds the watcher. home is the HOME whose .kiro/sessions tree kiro-cli
// writes; mapping is sessionTitleSync.mappedSessions, which is the only join from one of a
// run's own sessions to a tab.
func newWorkflowWatch(home string, mapping func() map[terminal.SessionID]string) *workflowWatch {
	return &workflowWatch{
		mapping:         mapping,
		probeAlive:      processAlive,
		verdicts:        make(map[string]workflowVerdict),
		unknownStatuses: make(map[string]struct{}),
		sessionsRoot:    filepath.Join(home, ".kiro", "sessions"),
	}
}

// mark returns one tab's folded state, the zero workflowMark when it has none.
func (w *workflowWatch) mark(id terminal.SessionID) workflowMark {
	if marks := w.marks.Load(); marks != nil {
		return (*marks)[id]
	}
	return workflowMark{}
}

// sessionActivity is the terminal.WithSessionActivity getter. The engine PULLS it from
// three goroutines -- the status sweep, every List and each new SSE subscriber -- so it
// must stay lock-free and do no I/O; mark loads one atomic.Pointer. Count is the TOTAL,
// which is what the one rendered mark words its tooltip from.
func (w *workflowWatch) sessionActivity(id terminal.SessionID) terminal.SessionActivity {
	mark := w.mark(id)
	return terminal.SessionActivity{State: mark.State, Count: mark.Tally.Total}
}

// runOwners returns the owner sessions of every run the last pass found live and joined to a
// mapped tab, one slice per run. ok is false before the first pass and after a pass in which
// any run could not be listed, read, decoded or walked to the end of its tree, so a reader
// that needs the whole set treats the answer as unavailable rather than as empty. The slices
// are read-only and shared.
func (w *workflowWatch) runOwners() (owners [][]string, ok bool) {
	return w.owners, w.ownersComplete
}

// pass runs one sweep: admit every run whose status, parent tab and file age all pass, fold
// the survivors per tab, and publish.
func (w *workflowWatch) pass(ctx context.Context, mgr workflowSessions) {
	// One liveness snapshot per sweep, so every run is judged against the same picture.
	sessions := mgr.List()
	live := make(map[terminal.SessionID]time.Time, len(sessions))
	for i := range sessions {
		live[sessions[i].ID] = sessions[i].CreatedAt
	}
	admitted, owners, complete := w.admit(ctx, live, w.tabsByKiroSession(live))
	marks := make(map[terminal.SessionID]workflowMark, len(admitted))
	for tab, states := range admitted {
		if mark := foldWorkflowMarks(states); mark.State != "" {
			marks[tab] = mark
		}
	}
	w.owners, w.ownersComplete = owners, complete
	w.publish(marks)
}

// tabsByKiroSession inverts the title poller's mapping to the join direction a run needs,
// dropping any tab the liveness snapshot does not hold: a run's owner set names kiro sessions,
// and one kiro session can be resumed in more than one tab.
func (w *workflowWatch) tabsByKiroSession(live map[terminal.SessionID]time.Time) map[string][]terminal.SessionID {
	tabs := make(map[string][]terminal.SessionID, len(live))
	for tab, kiroID := range w.mapping() {
		if _, ok := live[tab]; ok {
			tabs[kiroID] = append(tabs[kiroID], tab)
		}
	}
	return tabs
}

// admit collects each live tab's admitted run states and refreshes the verdict cache. The
// cache is REBUILT from the paths seen here, so a removed run drops out with no eviction.
// complete is false when any run could not be listed, stat'ed, read, decoded or walked whole,
// refused or not: the owner set is then short, and a consumer that must know every file a
// tab can reach has to wait. A partial verdict is checked before the refusal because the
// sessions it omits may be the ones a live tab is mapped to.
func (w *workflowWatch) admit(ctx context.Context, live map[terminal.SessionID]time.Time, tabs map[string][]terminal.SessionID) (admitted map[terminal.SessionID][]string, owners [][]string, complete bool) {
	seen := make(map[string]workflowVerdict, len(w.verdicts))
	admitted = make(map[terminal.SessionID][]string)
	paths, complete := w.statePaths()
	for _, path := range paths {
		fi, err := statWorkflowState(path)
		if err != nil {
			complete = complete && errors.Is(err, fs.ErrNotExist)
			continue
		}
		verdict, decided := w.verdictFor(ctx, path, fi)
		if !decided {
			complete = false
			continue
		}
		seen[path] = verdict
		complete = complete && !verdict.partial
		if w.refusesRun(ctx, path, &verdict, tabs) {
			continue
		}
		owners = append(owners, verdict.owners)
		admitRun(admitted, &verdict, fi.ModTime(), live, tabs)
	}
	w.verdicts = seen
	return admitted, owners, complete
}

// A tab carries ONE mapped session, so no tab is reachable through two owners and the tally
// cannot double-count a run.
func admitRun(admitted map[terminal.SessionID][]string, verdict *workflowVerdict, written time.Time, live map[terminal.SessionID]time.Time, tabs map[string][]terminal.SessionID) {
	if verdict.state == "" {
		return
	}
	for _, owner := range verdict.owners {
		for _, tab := range tabs[owner] {
			// The third admission clause, and deliberately not a freshness window: a run
			// legitimately sits paused awaiting input overnight. "Written after the tab
			// started" still refuses a run abandoned before this tab existed.
			if !written.Before(live[tab]) {
				admitted[tab] = append(admitted[tab], verdict.state)
			}
		}
	}
}

// refusesRun reports whether one run is refused for every tab; a false answer leaves admit's
// per-tab clause to decide. The dead-writer clause refuses abandonment DURING the tab's life,
// which admit's mtime clause structurally cannot see; it rests on the run's writer being a
// descendant of this process, so a per-session PID namespace would make every pid read dead.
func (w *workflowWatch) refusesRun(ctx context.Context, path string, verdict *workflowVerdict, tabs map[string][]terminal.SessionID) bool {
	// mapsToATab is an economy, not a gate -- admit iterates the owners, so a run no live tab
	// maps to admits nothing anyway -- and it only spares the beat read.
	return len(verdict.owners) == 0 || !mapsToATab(verdict.owners, tabs) || w.writerIsDead(ctx, path)
}

// mapsToATab reports whether any of a run's owner sessions names a live tab.
func mapsToATab(owners []string, tabs map[string][]terminal.SessionID) bool {
	for _, owner := range owners {
		if len(tabs[owner]) > 0 {
			return true
		}
	}
	return false
}

// verdictFor answers from the cache while the file still carries the identity the cached
// verdict was measured under, and classifies it otherwise. decided is false when the read
// failed and nothing was measured.
func (w *workflowWatch) verdictFor(ctx context.Context, path string, fi os.FileInfo) (workflowVerdict, bool) {
	verdict, cached := w.verdicts[path]
	if cached && verdict.size == fi.Size() && verdict.modTime.Equal(fi.ModTime()) {
		return verdict, true
	}
	verdict, decided := w.classify(ctx, path, &verdict)
	if !decided {
		// A failed read measured nothing, so stamping the identity onto it would let one
		// failure freeze the mark: a paused run's record never changes again.
		return verdict, false
	}
	verdict.modTime, verdict.size = fi.ModTime(), fi.Size()
	return verdict, true
}

// statWorkflowState reports the file identity the verdict cache keys on. fs.ErrNotExist is
// the normal miss: workflows/ also holds generated/, which carries definitions and no run
// state, and a wf_ prefix filter would only guess at naming.
func statWorkflowState(path string) (os.FileInfo, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("workflow mark: run state could not be stat'ed", "error", err)
		}
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errWorkflowStateIrregular
	}
	return fi, nil
}

// statePaths lists every run's state file across the workspace-hash level. workflows/ is a
// SIBLING of the sess_<uuid> directories under a hash dir, not a child of one, so a run is
// not reachable by walking down from the session directory the title poller reads -- and
// only one hash dir has a workflows/ at all, so an absent one is the normal case. complete
// is false when a directory could not be listed.
func (w *workflowWatch) statePaths() (paths []string, complete bool) {
	hashDirs, err := os.ReadDir(w.sessionsRoot)
	if err != nil {
		// An absent tree is normal before kiro-cli's first session; anything else kills every
		// tab's mark, so it is recorded.
		if errors.Is(err, fs.ErrNotExist) {
			return nil, true
		}
		slog.Debug("workflow mark: kiro session store unreadable",
			"dir", w.sessionsRoot, "error", err)
		return nil, false
	}
	complete = true
	for _, hd := range hashDirs {
		if !hd.IsDir() {
			continue
		}
		runs, listed := runStatePaths(filepath.Join(w.sessionsRoot, hd.Name(), workflowsDirName))
		paths = append(paths, runs...)
		complete = complete && listed
	}
	return paths, complete
}

// runStatePaths lists one hash directory's runs. An absent workflows/ is silent and complete.
func runStatePaths(dir string) (paths []string, complete bool) {
	runs, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, true
		}
		slog.Debug("workflow mark: workflow store unreadable", "dir", dir, "error", err)
		return nil, false
	}
	paths = make([]string, 0, len(runs))
	for _, run := range runs {
		if run.IsDir() {
			paths = append(paths, filepath.Join(dir, run.Name(), workflowStateFileName))
		}
	}
	return paths, true
}

// classify reads and decodes one state file. decided=false means the READ failed, which is
// not a verdict about the file at all: nothing was measured, so nothing may be cached.
//
// A DECODE failure carries the path's PREVIOUS verdict forward, marked partial: the mark
// holds steady across a live writer's torn write, and the owner set goes unavailable because
// the torn bytes may name a step the carried verdict does not. A record torn for good, its
// writer killed mid-write, keeps both effects for as long as it exists.
func (w *workflowWatch) classify(ctx context.Context, path string, previous *workflowVerdict) (verdict workflowVerdict, decided bool) {
	raw, err := readSmallFile(ctx, path, maxWorkflowStateBytes)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("workflow mark: run state unreadable", "error", err)
		}
		return workflowVerdict{}, false
	}
	var st workflowState
	if err := json.Unmarshal(raw, &st); err != nil {
		slog.Debug("workflow mark: run state is not decodable", "error", err)
		carried := *previous
		carried.partial = true
		return carried, true
	}
	if !knownWorkflowStatus(st.Status) {
		// Liveness unknown, so the run lights nothing and stays reachable.
		w.noteUnknownStatus(st.Status)
		return runVerdict(&st, ""), true
	}
	state, ok := classifyWorkflowState(&st)
	if !ok {
		return workflowVerdict{}, true
	}
	return runVerdict(&st, state), true
}

func runVerdict(st *workflowState, state string) workflowVerdict {
	owners, partial := runSessionIDs(st)
	return workflowVerdict{owners: owners, state: state, partial: partial}
}

// writerIsDead reports the one liveness proof that withdraws a mark: this run's run.beat
// names a pid the kernel says is gone. Freshness is NOT readable from that file -- kiro-cli
// only touches it while the run loop runs, and a run paused awaiting input leaves the mtime
// frozen -- so the pid is the whole signal. Absent, unreadable, undecodable, a non-positive
// pid or no probe all admit the run: the beat is written without a temp-plus-rename, so a
// torn read is reachable, and 388 of 392 measured runs carry no run.beat at all.
func (w *workflowWatch) writerIsDead(ctx context.Context, statePath string) bool {
	path := filepath.Join(filepath.Dir(statePath), workflowBeatFileName)
	raw, err := readSmallFile(ctx, path, maxWorkflowBeatBytes)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("workflow mark: run beat unreadable", "error", err)
		}
		return false
	}
	var beat workflowBeat
	if err := json.Unmarshal(raw, &beat); err != nil {
		slog.Debug("workflow mark: run beat is not decodable", "error", err)
		return false
	}
	return beat.PID > 0 && w.probeAlive != nil && !w.probeAlive(beat.PID)
}

// processAlive reports whether pid names a LIVE process. Only os.ErrProcessDone -- Go's
// translation of ESRCH -- and a zombie answer false, because every other errno leaves the
// question unanswered and an unanswered question must not blank a mark. A non-positive pid
// answers true without a syscall: kill(0, 0) signals the caller's own process group.
func processAlive(pid int) bool {
	if pid <= 0 {
		return true
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	if errors.Is(proc.Signal(syscall.Signal(0)), os.ErrProcessDone) {
		return false
	}
	return !processIsZombie(pid)
}

// processIsZombie reports whether pid names a task that has exited and that nobody has reaped,
// which a signal-0 probe still answers alive for. An unreadable /proc answers false.
func processIsZombie(pid int) bool {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// comm is parenthesized and can hold spaces and parens itself, so the state is the field
	// after the LAST ')'.
	rest := string(raw)
	if end := strings.LastIndexByte(rest, ')'); end >= 0 {
		rest = rest[end+1:]
	}
	fields := strings.Fields(rest)
	return len(fields) > 0 && fields[0] == "Z"
}

// publish stores the new snapshot and records the change. One line per pass in which the
// PUBLISHED set moved, so a steady state is silent.
func (w *workflowWatch) publish(marks map[terminal.SessionID]workflowMark) {
	var previous map[terminal.SessionID]workflowMark
	if loaded := w.marks.Load(); loaded != nil {
		previous = *loaded
	}
	w.marks.Store(&marks)
	if maps.Equal(previous, marks) {
		return
	}
	var total workflowTally
	lit := make([]string, 0, len(marks))
	for id, mark := range marks {
		total.Total += mark.Tally.Total
		total.Working += mark.Tally.Working
		total.Waiting += mark.Tally.Waiting
		total.Input += mark.Tally.Input
		// LogID, never the raw tab id: that id is the /ws attach and resume capability.
		lit = append(lit, terminal.LogID(id))
	}
	slices.Sort(lit)
	slog.Debug("workflow mark: published set changed",
		"tabs", len(marks), "runs", total.Total,
		"working", total.Working, "waiting", total.Waiting, "input", total.Input,
		"tab_ids", strings.Join(lit, ","))
}

// noteUnknownStatus warns once per distinct unrecognized status, capped. The status is the
// ONLY text from the file that reaches a sink, so it goes through runesafe's single-line
// policy plus a byte cap first: the record is written by a process this app does not run.
func (w *workflowWatch) noteUnknownStatus(status string) {
	clean := runesafe.SanitizeSingleLineBounded(status, maxWorkflowStatusLogBytes)
	if _, warned := w.unknownStatuses[clean]; warned || len(w.unknownStatuses) >= unknownWorkflowStatusCap {
		return
	}
	w.unknownStatuses[clean] = struct{}{}
	slog.Warn("workflow mark: unrecognized workflow run status",
		"status", clean,
		"hint", "the measured enum is running, paused, completed, failed, aborted; this run lights no mark")
}
