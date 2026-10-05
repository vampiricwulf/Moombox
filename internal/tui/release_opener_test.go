package tui

import (
	"testing"
	"time"
)

// fakeOpener records what releaseOpener did to a started opener.
type fakeOpener struct {
	released chan struct{}
	waited   chan struct{}
}

func newFakeOpener() *fakeOpener {
	return &fakeOpener{released: make(chan struct{}, 1), waited: make(chan struct{}, 1)}
}

func (f *fakeOpener) Wait() error    { f.waited <- struct{}{}; return nil }
func (f *fakeOpener) Release() error { f.released <- struct{}{}; return nil }

// TestReleaseOpenerHandsTheChildBack pins both arms. The TUI's O S / O W / O G
// opener was Started and forgotten: a process handle leaked per press on
// Windows, and a zombie per press elsewhere, for the life of the process.
//
// Mutants: drop the Release on windows; drop the Wait elsewhere.
func TestReleaseOpenerHandsTheChildBack(t *testing.T) {
	win := newFakeOpener()
	releaseOpener("windows", win)
	select {
	case <-win.released:
	default:
		t.Error("windows: the process handle was not released")
	}
	select {
	case <-win.waited:
		t.Error("windows: waited on the child — there is nothing to reap, and the wait can last as long as the window")
	default:
	}

	unix := newFakeOpener()
	releaseOpener("linux", unix)
	select {
	case <-unix.waited:
	case <-time.After(5 * time.Second):
		t.Error("linux: the child was never reaped")
	}
	select {
	case <-unix.released:
		t.Error("linux: released instead of reaping — the child stays a zombie")
	default:
	}
}
