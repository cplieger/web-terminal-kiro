package main

import (
	"context"
	"time"

	"github.com/cplieger/web-terminal-engine/v6/terminal"
)

// sessionPollInterval is deliberately slow beside the engine's 250 ms status sweep: that sweep
// drives a live activity dot, while these sweeps drive a label, a secondary mark and the
// withdrawal of a latch that would otherwise stand forever.
const sessionPollInterval = 2 * time.Second

type pollerSessions interface {
	sessionNamer
	workflowSessions
	latchStore
	sessionSetCapturer
}

// sessionPollers runs the title, workflow and pending-interaction sweeps in that order on one
// ticker, then saves the tab set. Each reads what the one before it published in the same
// tick, so a withdrawal is decided on one coherent picture of the tab mapping and the live
// runs, and the saved set carries this tick's mapping. sessionEnv is nil when the state
// directory was refused; tabs is nil when there is no /config to save to.
type sessionPollers struct {
	titles     *sessionTitleSync
	workflows  *workflowWatch
	pending    *pendingInteractionWatch
	sessionEnv func(tabID terminal.SessionID) []string
	tabs       *tabStore
}

func newSessionPollers(stateRoot, home string, tabs *tabStore) *sessionPollers {
	titles := newSessionTitleSync(stateRoot, home)
	workflows := newWorkflowWatch(home, titles.mappedSessions)
	return &sessionPollers{
		titles:     titles,
		workflows:  workflows,
		pending:    newPendingInteractionWatch(home, titles.mappedSessions, workflows.runOwners),
		sessionEnv: enableSessionTitles(titles),
		tabs:       tabs,
	}
}

// run sweeps until ctx is cancelled, in one of three arms:
//   - no state directory and no tab store: it returns at once;
//   - no state directory but a tab store: each tick only saves the tab set, so the refused
//     directory is never traversed (the title sweep is os.ReadDir plus os.Remove over it);
//   - otherwise each tick runs pass.
func (p *sessionPollers) run(ctx context.Context, mgr pollerSessions) {
	if p.sessionEnv == nil && p.tabs == nil {
		return
	}
	t := time.NewTicker(sessionPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if p.sessionEnv == nil {
				p.tabs.persist(ctx, mgr, nil)
				continue
			}
			p.pass(ctx, mgr)
		}
	}
}

func (p *sessionPollers) pass(ctx context.Context, mgr pollerSessions) {
	p.titles.pass(ctx, mgr)
	p.workflows.pass(ctx, mgr)
	p.pending.pass(ctx, mgr)
	p.tabs.persist(ctx, mgr, p.titles.mappedSessions())
}
