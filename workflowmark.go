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
	"github.com/cplieger/web-terminal-engine/v5/terminal"
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

	workflowPollInterval = 2 * time.Second

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
// unbounded work; the budget is spent one node per POP, so an exhausted walk simply stops
// where it is and every caller has to be correct on a partial answer.
func walkWorkflowNodes(root *workflowNode, visit func(*workflowNode) bool) {
	stack := []*workflowNode{root}
	for budget := maxWorkflowNodes; len(stack) > 0 && budget > 0; budget-- {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visit(node) {
			return
		}
		for i := range node.Children {
			stack = append(stack, &node.Children[i])
		}
	}
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
// nodes and a repeat would count the same run twice in one tab's tally.
func runSessionIDs(st *workflowState) []string {
	seen := make(map[string]struct{}, 4)
	owners := make([]string, 0, 4)
	admit := func(id string) {
		if _, dup := seen[id]; dup || !validKiroSessionID(id) {
			return
		}
		seen[id] = struct{}{}
		owners = append(owners, id)
	}
	admit(st.ParentSessionID)
	walkWorkflowNodes(&st.Root, func(node *workflowNode) bool {
		admit(node.SessionID)
		return false
	})
	return owners
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
// observed. An empty state means the run contributes nothing.
type workflowVerdict struct {
	modTime time.Time
	state   string
	owners  []string
	size    int64
}

// workflowWatch reports what a workflow run launched in a tab is doing, by polling
// kiro-cli's own on-disk run state. It owns no tab list: the live set is the engine's and
// the tab -> kiro-session pairing is the title poller's, so a tab with no mapping simply
// carries no mark.
type workflowWatch struct {
	// Replaced per pass, never mutated in place, so a reader needs no lock.
	marks      atomic.Pointer[map[terminal.SessionID]workflowMark]
	mapping    func() map[terminal.SessionID]string
	probeAlive func(pid int) bool
	// Both maps are touched only by the poller goroutine, so neither needs a lock.
	verdicts        map[string]workflowVerdict
	unknownStatuses map[string]struct{}
	sessionsRoot    string
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

// Run polls until ctx is cancelled. ONE goroutine for every tab, the same shape the title
// poller uses: one directory listing plus a stat per run, and a read only for a run whose
// file changed.
func (w *workflowWatch) Run(ctx context.Context, mgr workflowSessions) {
	t := time.NewTicker(workflowPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.pass(ctx, mgr)
		}
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

// pass runs one sweep: admit every run whose status, parent tab and file age all pass, fold
// the survivors per tab, and publish.
func (w *workflowWatch) pass(ctx context.Context, mgr workflowSessions) {
	// One liveness snapshot per sweep, so every run is judged against the same picture.
	sessions := mgr.List()
	live := make(map[terminal.SessionID]time.Time, len(sessions))
	for i := range sessions {
		live[sessions[i].ID] = sessions[i].CreatedAt
	}
	admitted := w.admit(ctx, live, w.tabsByKiroSession(live))
	marks := make(map[terminal.SessionID]workflowMark, len(admitted))
	for tab, states := range admitted {
		if mark := foldWorkflowMarks(states); mark.State != "" {
			marks[tab] = mark
		}
	}
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
func (w *workflowWatch) admit(ctx context.Context, live map[terminal.SessionID]time.Time, tabs map[string][]terminal.SessionID) map[terminal.SessionID][]string {
	seen := make(map[string]workflowVerdict, len(w.verdicts))
	admitted := make(map[terminal.SessionID][]string)
	for _, path := range w.statePaths() {
		fi, ok := statWorkflowState(path)
		if !ok {
			continue
		}
		verdict := w.verdictFor(ctx, path, fi)
		seen[path] = verdict
		if w.refusesRun(ctx, path, verdict, tabs) {
			continue
		}
		// A tab carries ONE mapped session, so no tab is reachable through two owners and
		// the tally cannot double-count a run.
		for _, owner := range verdict.owners {
			for _, tab := range tabs[owner] {
				// The third admission clause, and deliberately not a freshness window: a run
				// legitimately sits paused awaiting input overnight. "Written after the tab
				// started" still refuses a run abandoned before this tab existed.
				if !fi.ModTime().Before(live[tab]) {
					admitted[tab] = append(admitted[tab], verdict.state)
				}
			}
		}
	}
	w.verdicts = seen
	return admitted
}

// refusesRun reports whether one run is refused for every tab; a false answer leaves admit's
// per-tab clause to decide. The dead-writer clause refuses abandonment DURING the tab's life,
// which admit's mtime clause structurally cannot see; it rests on the run's writer being a
// descendant of this process, so a per-session PID namespace would make every pid read dead.
func (w *workflowWatch) refusesRun(ctx context.Context, path string, verdict workflowVerdict, tabs map[string][]terminal.SessionID) bool {
	// mapsToATab is an economy, not a gate -- admit iterates the owners, so a run no live tab
	// maps to admits nothing anyway -- and it only spares the beat read.
	return verdict.state == "" || !mapsToATab(verdict.owners, tabs) || w.writerIsDead(ctx, path)
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
// verdict was measured under, and classifies it otherwise.
func (w *workflowWatch) verdictFor(ctx context.Context, path string, fi os.FileInfo) workflowVerdict {
	verdict, cached := w.verdicts[path]
	if cached && verdict.size == fi.Size() && verdict.modTime.Equal(fi.ModTime()) {
		return verdict
	}
	verdict, decided := w.classify(ctx, path, verdict)
	if !decided {
		// A failed read measured nothing, so stamping the identity onto it would let one
		// failure freeze the mark: a paused run's record never changes again.
		return verdict
	}
	verdict.modTime, verdict.size = fi.ModTime(), fi.Size()
	return verdict
}

// statWorkflowState reports the file identity the verdict cache keys on, or false when the
// path holds no run state to read.
func statWorkflowState(path string) (os.FileInfo, bool) {
	fi, err := os.Lstat(path)
	if err != nil {
		// ENOENT is the normal miss: workflows/ also holds generated/, which carries
		// definitions and no run state, and a wf_ prefix filter would only guess at naming.
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("workflow mark: run state could not be stat'ed", "error", err)
		}
		return nil, false
	}
	return fi, fi.Mode().IsRegular()
}

// statePaths lists every run's state file across the workspace-hash level. workflows/ is a
// SIBLING of the sess_<uuid> directories under a hash dir, not a child of one, so a run is
// not reachable by walking down from the session directory the title poller reads -- and
// only one hash dir has a workflows/ at all, so an absent one is the normal case.
func (w *workflowWatch) statePaths() []string {
	hashDirs, err := os.ReadDir(w.sessionsRoot)
	if err != nil {
		// An absent tree is normal before kiro-cli's first session; anything else kills every
		// tab's mark, so it is recorded.
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("workflow mark: kiro session store unreadable",
				"dir", w.sessionsRoot, "error", err)
		}
		return nil
	}
	var paths []string
	for _, hd := range hashDirs {
		if !hd.IsDir() {
			continue
		}
		paths = append(paths, runStatePaths(filepath.Join(w.sessionsRoot, hd.Name(), workflowsDirName))...)
	}
	return paths
}

// runStatePaths lists one hash directory's runs. An absent workflows/ is silent.
func runStatePaths(dir string) []string {
	runs, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("workflow mark: workflow store unreadable", "dir", dir, "error", err)
		}
		return nil
	}
	paths := make([]string, 0, len(runs))
	for _, run := range runs {
		if run.IsDir() {
			paths = append(paths, filepath.Join(dir, run.Name(), workflowStateFileName))
		}
	}
	return paths
}

// classify reads and decodes one state file. decided=false means the READ failed, which is
// not a verdict about the file at all: nothing was measured, so nothing may be cached.
//
// A DECODE failure carries the path's PREVIOUS verdict forward. That holds the mark steady
// across a LIVE writer's torn write, which is the case it is for; a writer killed mid-write
// leaves a permanently torn record, and its mark then stands until the tab closes.
func (w *workflowWatch) classify(ctx context.Context, path string, previous workflowVerdict) (verdict workflowVerdict, decided bool) {
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
		return previous, true
	}
	if !knownWorkflowStatus(st.Status) {
		w.noteUnknownStatus(st.Status)
		return workflowVerdict{}, true
	}
	state, ok := classifyWorkflowState(&st)
	if !ok {
		return workflowVerdict{}, true
	}
	// The owner ids are joined against the title poller's mapping, and the file is written by
	// a process this app does not run: a record naming no valid session id names no tab. The
	// return is an early-out, not a gate -- admit iterates the owners, so an empty set already
	// admits nothing.
	owners := runSessionIDs(&st)
	if len(owners) == 0 {
		return workflowVerdict{}, true
	}
	return workflowVerdict{owners: owners, state: state}, true
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
