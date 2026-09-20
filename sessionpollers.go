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
	titleSetter
	workflowSessions
	latchStore
}

// sessionPollers runs the title, workflow and pending-interaction sweeps in that order on one
// ticker. Each reads what the one before it published in the same tick, so a withdrawal is
// decided on one coherent picture of the tab mapping and the live runs. sessionEnv is nil
// when the state directory was refused.
type sessionPollers struct {
	titles     *sessionTitleSync
	workflows  *workflowWatch
	pending    *pendingInteractionWatch
	sessionEnv func(tabID terminal.SessionID) []string
}

func newSessionPollers(stateRoot, home string) *sessionPollers {
	titles := newSessionTitleSync(stateRoot, home)
	workflows := newWorkflowWatch(home, titles.mappedSessions)
	return &sessionPollers{
		titles:     titles,
		workflows:  workflows,
		pending:    newPendingInteractionWatch(home, titles.mappedSessions, workflows.runOwners),
		sessionEnv: enableSessionTitles(titles),
	}
}

// Run sweeps until ctx is cancelled. It starts nothing when sessionEnv is nil (the state
// directory was refused): the title sweep is os.ReadDir plus os.Remove over that directory,
// and a rejected path must never be traversed.
func (p *sessionPollers) Run(ctx context.Context, mgr pollerSessions) {
	if p.sessionEnv == nil {
		return
	}
	t := time.NewTicker(sessionPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.pass(ctx, mgr)
		}
	}
}

func (p *sessionPollers) pass(ctx context.Context, mgr pollerSessions) {
	p.titles.pass(ctx, mgr)
	p.workflows.pass(ctx, mgr)
	p.pending.pass(ctx, mgr)
}
