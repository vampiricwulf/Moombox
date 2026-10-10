package main

import "testing"

// requestDiskRecheck is called from save paths, so it must never block one: a
// request already pending covers the next, and a runState without the channel
// (one a test built) makes it a no-op.
//
// Mutant: a blocking send — the second request hangs the test.
func TestRequestDiskRecheckCoalescesAndNeverBlocks(t *testing.T) {
	(&runState{}).requestDiskRecheck()

	s := &runState{diskRecheck: make(chan struct{}, 1)}
	s.requestDiskRecheck()
	s.requestDiskRecheck()
	if n := len(s.diskRecheck); n != 1 {
		t.Errorf("%d pending rechecks, want the two requests coalesced into 1", n)
	}
}
