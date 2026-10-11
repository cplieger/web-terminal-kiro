package main

import (
	"slices"
	"testing"

	"github.com/cplieger/slogx/capture"
	"github.com/cplieger/web-terminal-engine/v6/terminal"
)

const (
	aliasThreadA = "sess_11111111-2222-3333-4444-555555555555"
	aliasThreadB = "sess_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

func TestSessionAliasFollowsTheMappedThread(t *testing.T) {
	f := newTitleFixture(t)
	f.mapping("tab1", aliasThreadA)
	set := &fakeSetter{live: []terminal.SessionID{"tab1"}}

	f.sync.pass(t.Context(), set)
	if got := set.aliases["tab1"]; got != aliasThreadA {
		t.Fatalf("alias after the first pass = %q, want the mapped thread %q", got, aliasThreadA)
	}

	f.sync.pass(t.Context(), set)
	if len(set.aliasCalls) != 1 {
		t.Errorf("SetSessionAlias calls = %v, want one for an unchanged mapping", set.aliasCalls)
	}

	// /chat inside the tab re-points its mapping to another thread.
	f.mapping("tab1", aliasThreadB)
	f.sync.pass(t.Context(), set)
	if got := set.aliases["tab1"]; got != aliasThreadB {
		t.Errorf("alias after a repoint = %q, want %q", got, aliasThreadB)
	}
}

// Serial: capture.Default swaps the process-global logger.
func TestSessionAliasRefusalIsLoggedOnceAndRetried(t *testing.T) {
	records := capture.Default(t)
	f := newTitleFixture(t)
	f.mapping("tab1", aliasThreadA)
	f.mapping("tab2", aliasThreadA)
	set := &fakeSetter{live: []terminal.SessionID{"tab1", "tab2"}}
	set.aliases = map[terminal.SessionID]string{"tab1": aliasThreadA}

	f.sync.pass(t.Context(), set)
	f.sync.pass(t.Context(), set)
	const refusal = "URL alias not applied"
	if n := records.Count(refusal); n != 1 {
		t.Errorf("refusal logged %d times over two passes, want once; log = %q", n, records.Messages())
	}
	if !slices.Contains(set.aliasCalls, "tab2="+aliasThreadA) {
		t.Fatalf("SetSessionAlias calls = %v, want tab2 asked for the thread", set.aliasCalls)
	}
	if got := set.aliases["tab2"]; got != "" {
		t.Fatalf("tab2 alias = %q while tab1 holds the thread, want none", got)
	}

	// The holder closes; the next pass hands the thread to tab2.
	set.live = []terminal.SessionID{"tab2"}
	f.sync.pass(t.Context(), set)
	if got := set.aliases["tab2"]; got != aliasThreadA {
		t.Errorf("tab2 alias after the holder closed = %q, want %q", got, aliasThreadA)
	}
}

func TestSessionAliasIsNeverSetForAClosedTab(t *testing.T) {
	f := newTitleFixture(t)
	f.mapping("closedtab", aliasThreadA)
	set := &fakeSetter{}

	f.sync.pass(t.Context(), set)
	if len(set.aliasCalls) != 0 {
		t.Errorf("SetSessionAlias calls = %v for a tab the manager does not list, want none", set.aliasCalls)
	}
}

func TestSessionAliasForgetsAClosedTabsRecord(t *testing.T) {
	f := newTitleFixture(t)
	f.mapping("tab1", aliasThreadA)
	set := &fakeSetter{live: []terminal.SessionID{"tab1"}}
	f.sync.pass(t.Context(), set)

	set.live = nil
	f.sync.pass(t.Context(), set)
	if _, ok := f.sync.aliased["tab1"]; ok {
		t.Error("aliased still holds a closed tab, want it pruned to the live set")
	}
}

func TestSessionAliasReachesTheLiveManager(t *testing.T) {
	deps := newTestDeps(true)
	deps.cmd = staticCmd("/bin/sh", "-c", "exec cat")
	_, mgr, _, tab := mustStartSession(t, deps)

	f := newTitleFixture(t)
	f.mapping(tab, aliasThreadA)
	f.sync.pass(t.Context(), mgr)

	list := mgr.List()
	for i := range list {
		if list[i].ID == tab {
			if list[i].Alias != aliasThreadA {
				t.Errorf("List() alias for the tab = %q, want %q", list[i].Alias, aliasThreadA)
			}
			return
		}
	}
	t.Fatalf("List() does not carry the started tab; got %d sessions", len(list))
}
