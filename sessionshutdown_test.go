package main

import (
	"context"
	"testing"
)

// orderedShutdowner records whether the base context was already cancelled when the
// teardown began.
type orderedShutdowner struct {
	base             context.Context
	calls            int
	cancelledAtStart bool
}

func (s *orderedShutdowner) Shutdown(context.Context) error {
	s.calls++
	s.cancelledAtStart = s.base.Err() != nil
	return nil
}

func TestSessionShutdown_cancelsBeforeTeardown(t *testing.T) {
	base, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr := &orderedShutdowner{base: base}

	newSessionShutdown(cancel, mgr)(t.Context())

	if mgr.calls != 1 {
		t.Fatalf("Shutdown calls = %d, want 1", mgr.calls)
	}
	if !mgr.cancelledAtStart {
		t.Error("the manager's teardown began with the base context live, want it cancelled first so no save captures the emptied set")
	}
}
