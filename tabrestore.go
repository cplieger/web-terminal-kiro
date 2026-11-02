package main

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/cplieger/web-terminal-engine/v6/terminal"
)

// tabRestoreReadyPoll is how often the restorer re-checks that kiro-cli is installed and
// the tools converged, the two conditions the create gate also waits on.
const tabRestoreReadyPoll = 500 * time.Millisecond

// sessionRestorer is what the restorer needs from the session manager.
type sessionRestorer interface {
	RestoreSessionSet(context.Context, *terminal.SessionSet, terminal.RestorePlan) (terminal.RestoreResult, error)
}

var _ sessionRestorer = (*terminal.SessionManager)(nil)

// tabRestorer brings back the tabs tabs.json held, each resuming its kiro-cli
// conversation with --resume-id when the conversation's record still exists.
type tabRestorer struct {
	titles  *sessionTitleSync
	build   func(resumeID string) func(terminal.SessionID) *terminal.Handler
	pending *atomic.Bool   // the create gate's verdict; cleared once the restore finished
	owner   map[string]int // kiro session id -> the one index allowed to resume it
	resumed map[int]string // index -> kiro session id plan resumed; restorer goroutine only
	set     terminal.SessionSet
	warned  bool // the store-unreadable Warn was emitted; restorer goroutine only
}

func newTabRestorer(titles *sessionTitleSync, set *terminal.SessionSet,
	build func(string) func(terminal.SessionID) *terminal.Handler,
	pending *atomic.Bool,
) *tabRestorer {
	return &tabRestorer{
		titles: titles, build: build, set: *set, pending: pending,
		owner: resumeOwners(set.Sessions), resumed: make(map[int]string),
	}
}

// resumeOwners maps each kiro session id to the one entry allowed to resume it. The entry
// whose address already names the conversation owns it, then the earliest: an old
// bookmark expects that tab, and order alone would let a duplicate take it.
func resumeOwners(entries []terminal.SessionSetEntry) map[string]int {
	owner := make(map[string]int)
	for i, e := range entries {
		if validKiroResumeID(e.Resume) && e.Alias == e.Resume {
			owner[e.Resume] = i
		}
	}
	for i, e := range entries {
		if _, held := owner[e.Resume]; !held && validKiroResumeID(e.Resume) {
			owner[e.Resume] = i
		}
	}
	return owner
}

// adoptLaunchingSessions re-points every entry saved with a workflow step's session at the
// conversation that launched the run (sessionTitleSync.tabSession says why a tab's mapping
// can name a step), then re-claims the owners, since two entries may now name one session.
func (r *tabRestorer) adoptLaunchingSessions(ctx context.Context) {
	for i := range r.set.Sessions {
		e := &r.set.Sessions[i]
		if !validKiroResumeID(e.Resume) {
			continue
		}
		if id, _, _ := r.titles.tabSession(ctx, e.Resume); id != e.Resume && validKiroResumeID(id) {
			slog.Info("tab restore: stored kiro session is a workflow step; resuming the conversation that launched it",
				"kiro_session", id)
			e.Resume = id
		}
	}
	r.owner = resumeOwners(r.set.Sessions)
}

// run waits until ready, restores the set, then arms store and clears pending. A ctx
// cancelled or a manager shut down first leaves both alone: shutdown is under way and the
// file must keep the pre-shutdown set.
func (r *tabRestorer) run(ctx context.Context, mgr sessionRestorer, ready func() bool, store *tabStore) {
	if !waitReady(ctx, ready) {
		return
	}
	r.adoptLaunchingSessions(ctx)
	res, err := mgr.RestoreSessionSet(ctx, &r.set, r.plan)
	if ctx.Err() != nil || errors.Is(err, terminal.ErrManagerClosed) {
		return
	}
	if err != nil {
		slog.Error("tab restore: restore did not run", "error", err)
	}
	for i := range res.Restored {
		rs := &res.Restored[i]
		if kiroID, ok := r.resumed[rs.Index]; ok {
			_ = r.titles.seedMapping(ctx, rs.Info.ID, kiroID) // logged inside; the hook maps it at the next prompt
		}
	}
	if len(res.Restored) == 0 && res.Failed > 0 {
		// Arming would overwrite the saved set with this run's empty one, losing every tab
		// to a failure that may be gone at the next boot.
		slog.Error("tab restore: no tab could be started; tabs.json kept for the next boot", "failed", res.Failed)
	} else {
		store.arm()
	}
	r.pending.Store(false)
}

// waitReady polls ready until it reports true, or ctx ends (false).
func waitReady(ctx context.Context, ready func() bool) bool {
	if ready() {
		return true
	}
	t := time.NewTicker(tabRestoreReadyPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			if ready() {
				return true
			}
		}
	}
}

// plan is the terminal.RestorePlan: what each stored tab comes back as.
func (r *tabRestorer) plan(ctx context.Context, i int, e terminal.SessionSetEntry) (terminal.CreateSpec, bool) {
	// A fresh chat gets a minted alias: the stored one named a conversation it no longer holds.
	fresh := terminal.CreateSpec{Build: r.build(""), PinnedTitle: e.PinnedTitle}
	if e.Resume == "" {
		fresh.Alias = e.Alias // a tab never prompted keeps its address
		return fresh, true
	}
	if !validKiroResumeID(e.Resume) {
		slog.Info("tab restore: stored kiro session id is malformed; restored with a fresh chat", "index", i)
		return fresh, true
	}
	if r.owner[e.Resume] != i {
		// A second TUI on one conversation blocks on the first one's lock for good.
		slog.Info("tab restore: a second tab held the same kiro session; not restored", "kiro_session", e.Resume)
		return terminal.CreateSpec{}, false
	}
	dir, state := r.titles.sessionRecord(ctx, e.Resume)
	switch state {
	case recordMissing:
		// kiro-cli would silently start an empty conversation under the stale id.
		slog.Info("tab restore: kiro session record missing; restored with a fresh chat", "kiro_session", e.Resume)
		return fresh, true
	case recordUnknown:
		// A transient EACCES must not turn every conversation into a fresh chat and then
		// save that over tabs.json.
		if dir != "" {
			slog.Info("tab restore: kiro session record unreadable; resuming without the record check",
				"kiro_session", e.Resume)
		} else if !r.warned {
			r.warned = true
			slog.Warn("tab restore: kiro session store unreadable; resuming without the record check",
				"dir", r.titles.sessionsRoot)
		}
	}
	r.resumed[i] = e.Resume
	return terminal.CreateSpec{Build: r.build(e.Resume), Alias: e.Resume, PinnedTitle: e.PinnedTitle}, true
}
