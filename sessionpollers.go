package main

import (
	"context"
	"time"

	"github.com/cplieger/web-terminal-engine/v6/terminal"
)

// sessionPollInterval is deliberately slow beside the engine's 250 ms status sweep: that sweep
// drives a live activity dot, while these sweeps drive a label and a secondary mark.
const sessionPollInterval = 2 * time.Second

type pollerSessions interface {
	sessionNamer
	workflowSessions
	sessionSetCapturer
}

// sessionPollers runs the title and workflow sweeps in that order on one ticker, then saves
// the tab set. The workflow sweep and the saved set read the mapping the title sweep
// published in the same tick. sessionEnv is nil when the state directory was refused; tabs
// is nil when there is no /config to save to.
type sessionPollers struct {
	titles     *sessionTitleSync
	workflows  *workflowWatch
	sessionEnv func(tabID terminal.SessionID) []string
	tabs       *tabStore
}

func newSessionPollers(stateRoot, home string, tabs *tabStore) *sessionPollers {
	titles := newSessionTitleSync(stateRoot, home)
	return &sessionPollers{
		titles:     titles,
		workflows:  newWorkflowWatch(home, titles.mappedSessions),
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
	p.tabs.persist(ctx, mgr, p.titles.mappedSessions())
}
