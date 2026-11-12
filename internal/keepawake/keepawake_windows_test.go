//go:build windows

package keepawake

import (
	"testing"
	"time"
)

// TestHoldSetsAndClearsTheStateOnOneThread: SetThreadExecutionState answers
// with the state the CALLING THREAD had before the call, so the answers show
// both that Windows took each request and that every one was made on the same
// thread — a request made from wherever the calling goroutine happened to run
// would be released on a different thread, and the first would hold the PC
// awake until Moombox exited.
//
// Mutants: drop LockOSThread (under load the previous state read on release
// lacks the system-required bit); set the state without ES_SYSTEM_REQUIRED
// (the same check); release by not calling at all (the re-hold reads
// system-required still set).
func TestHoldSetsAndClearsTheStateOnOneThread(t *testing.T) {
	h := New()
	defer h.Close()

	if got := h.impl.ask(true); !got.ok {
		t.Fatal("Windows refused the keep-awake request")
	}
	// Other goroutines doing work in between gives the scheduler every reason
	// to move things around.
	for i := 0; i < 50; i++ {
		done := make(chan struct{})
		go func() { close(done) }()
		<-done
		if got := h.impl.ask(true); !got.ok || got.prev&esSystemRequired == 0 {
			t.Fatalf("repeat %d: ok=%v, previous state %#x lacks system-required: the hold was not on this thread", i, got.ok, got.prev)
		}
	}
	released := h.impl.ask(false)
	if !released.ok || released.prev&esSystemRequired == 0 {
		t.Fatalf("release: ok=%v, previous state %#x lacks system-required", released.ok, released.prev)
	}
	if again := h.impl.ask(true); !again.ok || again.prev&esSystemRequired != 0 {
		t.Fatalf("hold after a release: ok=%v, previous state %#x still has system-required: the release did nothing", again.ok, again.prev)
	}

	h.Close()
	if h.Hold(true) {
		t.Error("Hold reported success after Close")
	}
}

// TestAPanicOnTheKeepAwakeThreadRefusesInsteadOfHanging: Hold is called under
// the controller's lock from the database's job callbacks, so a request the
// dead thread will never answer has to come back refused. Before, the request
// in flight waited for its answer forever and every later one blocked on the
// send, each holding the Holder's lock.
//
// Mutants: do not close dead in run's defer (the first Hold never returns);
// select on dead for the answer only (the second Hold never returns).
func TestAPanicOnTheKeepAwakeThreadRefusesInsteadOfHanging(t *testing.T) {
	orig := setState
	setState = func(uint32) (uint32, bool) { panic("boom") }
	defer func() { setState = orig }()

	h := New()
	defer h.Close()
	for i, name := range []string{"the request in flight", "a later request"} {
		got := make(chan bool, 1)
		go func() { got <- h.Hold(true) }()
		select {
		case ok := <-got:
			if ok {
				t.Errorf("%s (%d) reported success after the thread panicked", name, i)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s is still waiting on a thread that has ended", name)
		}
	}
}
