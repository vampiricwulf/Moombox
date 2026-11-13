package sidecar

import "testing"

// Recoverable is what a mint reads before waiting for a child that is down
// (bgutils.PotProvider.awaitSidecar): only a handle whose child has come up
// once, and that was not stopped, can be brought back by a restart.
//
// Mutants: Recoverable ignoring everUp (a handle that never started reads
// true); ignoring the stopped latch (true after Stop).
func TestRecoverableNeedsAChildThatCameUpAndNoStop(t *testing.T) {
	s := New(Config{Logger: silentLogger{}})
	if s.Recoverable() {
		t.Error("a handle that was never started reads recoverable")
	}
	s.everUp.Store(true) // what startLocked records beside healthy
	if !s.Recoverable() {
		t.Error("a handle whose child came up, now down, reads unrecoverable")
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	if s.Recoverable() {
		t.Error("a stopped handle reads recoverable")
	}
}
