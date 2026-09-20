package main

// kiro-cli notifies the terminal when a permission or question prompt RISES and never when it
// falls, so a prompt raised inside a workflow step or a subagent leaves the engine's input
// latch standing after the answer: the tab's own turn is not open, and the OSC 9;4 state the
// falling edge re-emits is one the engine deliberately does not read as a contradiction. The
// pending_interaction and interaction_resolved rows KAS appends to messages.jsonl are the only
// record of settlement a terminal host can read, so the latch is withdrawn from there.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/cplieger/atomicfile/v3"
	"github.com/cplieger/web-terminal-engine/v6/terminal"
)

const (
	// The row precedes its notification by milliseconds and the poller notices the latch up
	// to one poll period plus one status sweep later, so the window opens before the arm.
	pendingArmSlack = 10 * time.Second

	messagesFileName = "messages.jsonl"

	// Measured over 2307 files: median 1.36 MB, max 19.2 MB, longest row 3.08 MB. Over-bound
	// is refused rather than truncated, because a partial read can miss the row that keeps
	// the latch.
	maxMessagesFileBytes = 64 << 20
	maxMessagesRowBytes  = 8 << 20

	// 67 pending rows in the whole measured corpus; the cap exists because the file is
	// written by a process this app does not run.
	maxPendingRowsPerFile = 1024

	payloadPendingInteraction  = "pending_interaction"
	payloadInteractionResolved = "interaction_resolved"
	payloadTurnEnd             = "turn_end"
)

var (
	errFileOverBound   = errors.New("file over the byte bound")
	errRowOverBound    = errors.New("row over the byte bound")
	errRowNotDecodable = errors.New("row is not decodable")
	errRowMalformed    = errors.New("pending row carries no usable timestamp or toolCallId")
	errRowsOverCap     = errors.New("pending rows over the per-file cap")
)

// messageRow is the only decode of a messages.jsonl row. A field added here is what would let
// question, option or answer text reach memory the poller keeps or a log sink.
type messageRow struct {
	Timestamp string `json:"timestamp"`
	Payload   struct {
		Type       string `json:"type"`
		ToolCallID string `json:"toolCallId"`
	} `json:"payload"`
}

// fingerprint keys a tab id or a toolCallId across passes without keeping the value: the tab
// id is the /ws attach capability and the toolCallId is the writer's, not this app's.
type fingerprint [sha256.Size]byte

func fingerprintOf(s string) fingerprint { return sha256.Sum256([]byte(s)) }

type pendingRow struct {
	at          time.Time
	settledPass uint64
}

type messagesTail struct {
	ident os.FileInfo
	rows  map[fingerprint]pendingRow
	// holders are the tabs whose arms have read this file; carry keeps the tail while one lives.
	holders map[fingerprint]struct{}
	offset  int64
	logged  bool
}

type armedLatch struct {
	at   time.Time
	seq  uint64
	pass uint64
}

type pollState struct {
	armed map[fingerprint]armedLatch
	tails map[string]*messagesTail
	paths map[string]string
}

// carry keeps every tail one of whose holders is still a live tab, and the paths those tails
// were resolved through. A tail dropped while its holder lives would be re-read from its start
// the next time that tab reaches it, stamping rows already seen settled into a later arm's
// pass and counting them as that arm's proof; tab liveness is the one input that cannot go
// transiently missing the way a mapping or the run owners can.
func (s *pollState) carry(sessions []terminal.SessionInfo) pollState {
	live := make(map[fingerprint]struct{}, len(sessions))
	for i := range sessions {
		live[fingerprintOf(string(sessions[i].ID))] = struct{}{}
	}
	next := pollState{
		armed: make(map[fingerprint]armedLatch, len(s.armed)),
		tails: make(map[string]*messagesTail, len(s.tails)),
		paths: make(map[string]string, len(s.paths)),
	}
	for path, tail := range s.tails {
		if tail.held(live) {
			next.tails[path] = tail
		}
	}
	for id, path := range s.paths {
		if _, kept := next.tails[path]; kept {
			next.paths[id] = path
		}
	}
	return next
}

type latchStore interface {
	List() []terminal.SessionInfo
	StatusLatch(id terminal.SessionID) (status string, seq uint64, ok bool)
	WithdrawStatusLatch(id terminal.SessionID, want string, seq uint64) bool
}

// pendingInteractionWatch withdraws a tab's input latch once kiro-cli's own session log shows
// every ask behind it settled. It only ever withdraws: nothing here may set a latch, so the
// worst outcome of any defect is the dot that stands today.
type pendingInteractionWatch struct {
	mapping      func() map[terminal.SessionID]string
	runOwners    func() (owners [][]string, ok bool)
	state        pollState
	sessionsRoot string
	passes       uint64
}

func newPendingInteractionWatch(home string, mapping func() map[terminal.SessionID]string, runOwners func() ([][]string, bool)) *pendingInteractionWatch {
	return &pendingInteractionWatch{
		mapping:      mapping,
		runOwners:    runOwners,
		sessionsRoot: filepath.Join(home, ".kiro", "sessions"),
	}
}

func (w *pendingInteractionWatch) pass(ctx context.Context, store latchStore) {
	w.passes++
	sessions := store.List()
	prev := w.state
	w.state = prev.carry(sessions)
	mapping := w.mapping()
	owners, reachable := w.runOwners()
	for i := range sessions {
		tab := sessions[i].ID
		kiroID, mapped := mapping[tab]
		var ids []string
		if mapped && reachable {
			ids = reachableSessions(kiroID, owners)
		}
		status, seq, ok := store.StatusLatch(tab)
		if !ok || status != terminal.StatusInput {
			continue
		}
		key := fingerprintOf(string(tab))
		rec := w.arm(&prev, key, seq)
		w.state.armed[key] = rec
		if len(ids) == 0 {
			continue
		}
		settled, ok := w.settledAsks(ctx, key, rec, ids)
		if !ok {
			continue
		}
		if !store.WithdrawStatusLatch(tab, terminal.StatusInput, rec.seq) {
			slog.Debug("pending interaction: withdrawal refused, a newer notification re-latched the tab",
				"tab", terminal.LogID(tab))
			continue
		}
		delete(w.state.armed, key)
		slog.Debug("pending interaction: withdrew a settled input latch",
			"tab", terminal.LogID(tab), "settled", settled)
	}
}

func (w *pendingInteractionWatch) arm(prev *pollState, key fingerprint, seq uint64) armedLatch {
	if rec, known := prev.armed[key]; known && rec.seq == seq {
		return rec
	}
	return armedLatch{at: time.Now(), seq: seq, pass: w.passes}
}

// reachableSessions names every kiro session whose messages.jsonl can hold one tab's asks: a
// step asks from its own session and a subagent asks in its parent's file, so it is the
// mapped session plus every owner of every live run that contains it.
func reachableSessions(kiroID string, owners [][]string) []string {
	ids := []string{kiroID}
	for _, run := range owners {
		if !slices.Contains(run, kiroID) {
			continue
		}
		for _, owner := range run {
			if !slices.Contains(ids, owner) {
				ids = append(ids, owner)
			}
		}
	}
	return ids
}

// settledAsks reads every file one tab's asks can land in. ok is true only when every file was
// found and read to an end it still has, at least one row can be the arm's and none is open.
func (w *pendingInteractionWatch) settledAsks(ctx context.Context, holder fingerprint, rec armedLatch, ids []string) (settled int, ok bool) {
	complete := true
	unsettled := 0
	read := make(map[string]*messagesTail, len(ids))
	for _, id := range ids {
		path, found := w.resolve(id)
		if !found {
			complete = false
			continue
		}
		tail := w.tailFor(path, holder)
		if !w.advance(ctx, tail, path) {
			complete = false
		}
		read[path] = tail
		s, u := tail.tally(rec)
		settled, unsettled = settled+s, unsettled+u
	}
	for path, tail := range read {
		if !tail.unchanged(path) {
			complete = false
		}
	}
	return settled, complete && settled > 0 && unsettled == 0
}

// tally counts the rows that can be the arm's. A row settled in a pass before the arm cannot
// be the ask that raised it, whatever its timestamp says.
func (t *messagesTail) tally(rec armedLatch) (settled, unsettled int) {
	since := rec.at.Add(-pendingArmSlack)
	for _, row := range t.rows {
		if row.at.Before(since) {
			continue
		}
		switch {
		case row.settledPass == 0:
			unsettled++
		case row.settledPass >= rec.pass:
			settled++
		}
	}
	return settled, unsettled
}

// resolve finds one kiro session's messages.jsonl. A step launched from a worktree lands under
// a different workspace hash than the tab's own session, so the hash level is scanned per id.
func (w *pendingInteractionWatch) resolve(id string) (string, bool) {
	if path, ok := w.state.paths[id]; ok {
		return path, true
	}
	// Both publishers validate the id; this is the consumer that makes it a path component.
	if !validKiroSessionID(id) {
		return "", false
	}
	hashDirs, err := os.ReadDir(w.sessionsRoot)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("pending interaction: kiro session store unreadable",
				"dir", w.sessionsRoot, "error", err)
		}
		return "", false
	}
	for _, hd := range hashDirs {
		if !hd.IsDir() {
			continue
		}
		dir := filepath.Join(w.sessionsRoot, hd.Name(), id)
		if fi, err := os.Lstat(dir); err == nil && fi.IsDir() {
			path := filepath.Join(dir, messagesFileName)
			w.state.paths[id] = path
			return path, true
		}
	}
	return "", false
}

func (w *pendingInteractionWatch) tailFor(path string, holder fingerprint) *messagesTail {
	tail, ok := w.state.tails[path]
	if !ok {
		tail = &messagesTail{holders: make(map[fingerprint]struct{}, 1)}
		w.state.tails[path] = tail
	}
	tail.holders[holder] = struct{}{}
	return tail
}

func (t *messagesTail) held(live map[fingerprint]struct{}) bool {
	for holder := range t.holders {
		if _, ok := live[holder]; ok {
			return true
		}
	}
	return false
}

// advance folds every complete row appended since the cursor. The cursor stays at the last
// complete row on every failure: a torn trailing row is then read whole once its newline
// lands, and unchanged reports the gap until it does.
func (w *pendingInteractionWatch) advance(ctx context.Context, tail *messagesTail, path string) bool {
	f, fi, err := atomicfile.OpenRegular(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("pending interaction: messages log unreadable", "error", err)
		}
		return false
	}
	defer func() { _ = f.Close() }() // read-only
	size := fi.Size()
	if size > maxMessagesFileBytes {
		return tail.refuse(path, errFileOverBound)
	}
	if tail.ident == nil || !os.SameFile(tail.ident, fi) || size < tail.offset {
		*tail = messagesTail{ident: fi, rows: make(map[fingerprint]pendingRow), holders: tail.holders}
	}
	if size == tail.offset {
		return true
	}
	if ctx.Err() != nil {
		return false
	}
	if _, err := f.Seek(tail.offset, io.SeekStart); err != nil {
		slog.Debug("pending interaction: messages log seek failed", "error", err)
		return false
	}
	r := bufio.NewReaderSize(io.LimitReader(f, size-tail.offset), 64<<10)
	if err := tail.fold(r, w.passes); err != nil {
		return tail.refuse(path, err)
	}
	return true
}

func (t *messagesTail) fold(r *bufio.Reader, pass uint64) error {
	for {
		line, err := readRow(r)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := t.apply(line, pass); err != nil {
			return err
		}
		t.offset += int64(len(line))
	}
}

// readRow returns the next row including its newline. io.EOF means the region ended, at a row
// boundary or inside a torn row, whose bytes are dropped so the cursor stays short of them.
func readRow(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxMessagesRowBytes {
			return nil, errRowOverBound
		}
		if err == nil {
			return line, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}

// apply folds one complete row in. A turn_end row settles every pending row before it: the
// cancel path writes no resolved row, and a turn that ended cannot hold an open ask.
func (t *messagesTail) apply(line []byte, pass uint64) error {
	var row messageRow
	if err := json.Unmarshal(line, &row); err != nil {
		return errRowNotDecodable
	}
	switch row.Payload.Type {
	case payloadPendingInteraction:
		return t.open(&row)
	case payloadInteractionResolved:
		t.settle(fingerprintOf(row.Payload.ToolCallID), pass)
	case payloadTurnEnd:
		for id := range t.rows {
			t.settle(id, pass)
		}
	}
	return nil
}

func (t *messagesTail) open(row *messageRow) error {
	at, err := time.Parse(time.RFC3339Nano, row.Timestamp)
	if err != nil || row.Payload.ToolCallID == "" {
		return errRowMalformed
	}
	id := fingerprintOf(row.Payload.ToolCallID)
	if _, known := t.rows[id]; !known && len(t.rows) >= maxPendingRowsPerFile {
		return errRowsOverCap
	}
	t.rows[id] = pendingRow{at: at}
	return nil
}

func (t *messagesTail) settle(id fingerprint, pass uint64) {
	if pending, ok := t.rows[id]; ok && pending.settledPass == 0 {
		pending.settledPass = pass
		t.rows[id] = pending
	}
}

// unchanged reports whether path still names the file the tail read, ending at the byte the
// read ended: a row appended since may be a queued ask this pass has not seen.
func (t *messagesTail) unchanged(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && t.ident != nil && os.SameFile(t.ident, fi) && fi.Size() == t.offset
}

// refuse logs once per tail: a refused row never changes in an append-only file, so every
// later pass fails at the same byte.
func (t *messagesTail) refuse(path string, err error) bool {
	if !t.logged {
		t.logged = true
		slog.Debug("pending interaction: messages log refused; the latch stands for the tabs it serves",
			"path", path, "offset", t.offset, "error", err)
	}
	return false
}
