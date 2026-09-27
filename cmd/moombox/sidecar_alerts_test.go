package main

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

// fakeTimer is the injected clock. It records the delay it was asked for and
// fires only when the test says so, which is what makes "59 s" testable
// without waiting 59 seconds: the assertion is that nothing fired, and the
// delay the alerter ASKED for is asserted separately.
type fakeTimer struct {
	d       time.Duration
	f       func()
	stopped bool
}

func (t *fakeTimer) Stop() bool {
	was := t.stopped
	t.stopped = true
	return !was
}

func (t *fakeTimer) fire() {
	if !t.stopped {
		t.f()
	}
}

type fakeClock struct{ timers []*fakeTimer }

func (c *fakeClock) after(d time.Duration, f func()) stoppableTimer {
	t := &fakeTimer{d: d, f: f}
	c.timers = append(c.timers, t)
	return t
}

func (c *fakeClock) last(t *testing.T) *fakeTimer {
	t.Helper()
	if len(c.timers) == 0 {
		t.Fatal("no timer was scheduled — the alerter fired (or did nothing) instead of debouncing")
	}
	return c.timers[len(c.timers)-1]
}

func newTestSidecarAlerts(t *testing.T) (*sidecarAlerts, *notificationtest.Recorder, *fakeClock) {
	t.Helper()
	rec := notificationtest.New()
	clk := &fakeClock{}
	a := newSidecarAlerts(rec, &nopLogger{}, clk.after)
	return a, rec, clk
}

// TestSidecarDownIsDebounced is the §0 ruling: the supervisor's restart ladder
// handles the quick cases, so the webhook waits for the one it cannot fix.
//
// Mutants this kill:
//   - sending on the transition instead of after the window: every supervisor
//     restart pages the operator.
//   - scheduling the wrong delay (the assertion on d).
//   - RE-ARMING on each unhealthy snapshot: a child that publishes every few
//     seconds would push the alert out forever and it would never arrive.
//   - dropping the escape on Reason (the reason is markdown-hostile on
//     purpose, so the assertion can see the escaping it asserts through).
func TestSidecarDownIsDebounced(t *testing.T) {
	a, rec, clk := newTestSidecarAlerts(t)

	const rawReason = "read*me_ EOF"
	a.onHealth(sidecar.Health{Healthy: false, Reason: rawReason, Restarts: 3})
	if got := len(rec.Calls()); got != 0 {
		t.Fatalf("recorded %d calls the instant the sidecar went down, want 0", got)
	}
	if d := clk.last(t).d; d != sidecarDownDebounce {
		t.Errorf("debounce = %v, want %v", d, sidecarDownDebounce)
	}

	// Still unhealthy, twice more, inside the window: ONE timer total.
	a.onHealth(sidecar.Health{Healthy: false, Reason: rawReason, Restarts: 4})
	a.onHealth(sidecar.Health{Healthy: false, Reason: rawReason, Restarts: 5})
	if got := len(clk.timers); got != 1 {
		t.Fatalf("armed %d timers for one continuous outage, want 1 — a re-arming debounce never fires on a flapping child", got)
	}

	clk.last(t).fire()
	calls := rec.ByEvent("sidecar_down")
	if len(calls) != 1 {
		t.Fatalf("recorded %d sidecar_down calls after the window, want 1", len(calls))
	}
	if calls[0].Type != notifications.TypeError {
		t.Errorf("type = %v, want TypeError", calls[0].Type)
	}
	// The window, in words a sentence can carry: %s on a time.Duration prints
	// "1m0s", which reads as a stopwatch reading in the middle of a sentence.
	if !strings.Contains(calls[0].Description, "unhealthy for over 60 s") {
		t.Errorf("description = %q, want it to name the window as seconds", calls[0].Description)
	}
	var sawReason, sawRestarts bool
	for _, f := range calls[0].Fields {
		switch f.Name {
		case "Reason":
			sawReason = f.Value == notifications.EscapeMarkdown(rawReason)
		case "Restarts":
			// The CURRENT snapshot's count, not the first one's.
			sawRestarts = f.Value == "5"
		}
	}
	if !sawReason || !sawRestarts {
		t.Errorf("fields = %+v, want the escaped reason the child died of and the latest restart count", calls[0].Fields)
	}
}

// TestSidecarFlapUnderTheWindowIsSilent is the case the debounce exists for:
// the supervisor restarted the child and nobody needed to know.
//
// Mutant: not stopping the timer on the healthy edge — the alert fires for a
// sidecar that is already back.
func TestSidecarFlapUnderTheWindowIsSilent(t *testing.T) {
	a, rec, clk := newTestSidecarAlerts(t)

	a.onHealth(sidecar.Health{Healthy: false, Reason: "stdout EOF"})
	a.onHealth(sidecar.Health{Healthy: true, Restarts: 1})
	clk.last(t).fire() // the supervisor's own timer, now cancelled

	if got := len(rec.Calls()); got != 0 {
		t.Fatalf("recorded %d calls for a flap inside the window, want 0: %+v", got, rec.Calls())
	}
}

// TestSidecarRestoredOnlyClosesAnAlertThatWasSent.
//
// Mutants this kill:
//   - sending the all-clear on every healthy edge: a healthy boot, and every
//     supervisor restart, would announce a recovery from nothing.
//   - failing to clear the sent flag: the second outage never alerts.
func TestSidecarRestoredOnlyClosesAnAlertThatWasSent(t *testing.T) {
	a, rec, clk := newTestSidecarAlerts(t)

	// A healthy first snapshot says nothing.
	a.onHealth(sidecar.Health{Healthy: true})
	if got := len(rec.Calls()); got != 0 {
		t.Fatalf("a healthy snapshot recorded %d calls, want 0", got)
	}

	a.onHealth(sidecar.Health{Healthy: false, Reason: "readPump panic"})
	clk.last(t).fire()
	rec.Reset()

	a.onHealth(sidecar.Health{Healthy: true, Restarts: 4})
	calls := rec.ByEvent("sidecar_restored")
	if len(calls) != 1 {
		t.Fatalf("recorded %d sidecar_restored calls, want 1", len(calls))
	}
	if calls[0].Type != notifications.TypeSuccess {
		t.Errorf("type = %v, want TypeSuccess", calls[0].Type)
	}

	// A second healthy snapshot is not a second recovery.
	rec.Reset()
	a.onHealth(sidecar.Health{Healthy: true, Restarts: 4})
	if got := len(rec.Calls()); got != 0 {
		t.Fatalf("a repeated healthy snapshot recorded %d calls, want 0", got)
	}

	// And a second outage still alerts.
	a.onHealth(sidecar.Health{Healthy: false, Reason: "failed initial start"})
	clk.last(t).fire()
	if got := len(rec.ByEvent("sidecar_down")); got != 1 {
		t.Fatalf("the second outage recorded %d sidecar_down calls, want 1", got)
	}
}

// TestSidecarTeardownStopsALateDelivery covers the teardown closure
// wireSidecarAlerts returns — `unsub(); a.stop()` — against the one delivery
// unsubscribing cannot prevent.
//
// PublishHealth COPIES the subscriber slice and fans out off its lock, so a
// publish already in flight still calls this subscriber after unsubscribe has
// returned. A slow subscriber registered first holds that fan-out open for as
// long as the test likes, which turns the race into a schedule: teardown
// completes, THEN the alerter's callback runs.
//
// Mutant this kills: dropping the `stopped` latch. The late callback arms a
// debounce that stop() has already run past, and the process announces a dead
// sidecar on its way out the door — the one moment the alert is guaranteed
// wrong.
func TestSidecarTeardownStopsALateDelivery(t *testing.T) {
	rec := notificationtest.New()
	clk := &fakeClock{}
	a := newSidecarAlerts(rec, &nopLogger{}, clk.after)

	// armed keeps the blocking behaviour for THIS test's publish only:
	// SubscribeHealth replays the current snapshot immediately, and whatever a
	// previous test left in the package-level health must not wedge here.
	var armed atomic.Bool
	entered := make(chan struct{})
	release := make(chan struct{})
	unsubSlow := sidecar.SubscribeHealth(func(sidecar.Health) {
		if !armed.Load() {
			return
		}
		close(entered)
		<-release
	})
	defer unsubSlow()

	unsub := sidecar.SubscribeHealth(a.onHealth)
	teardown := func() { unsub(); a.stop() } // wireSidecarAlerts' closure, same order

	armed.Store(true)
	published := make(chan struct{})
	go func() {
		defer close(published)
		sidecar.PublishHealth(sidecar.Health{Healthy: false, Reason: "stdout EOF"})
	}()

	<-entered  // the fan-out is inside the slow subscriber, before ours
	teardown() // …and completes while it is held
	close(release)
	<-published // now our callback has run, after the teardown
	armed.Store(false)

	// Fire anything that was armed, whenever it was armed: the assertion is
	// that nothing reaches the operator.
	for _, timer := range clk.timers {
		timer.fire()
	}
	if got := rec.Calls(); len(got) != 0 {
		t.Fatalf("a completed teardown still produced %d embeds: %+v", len(got), got)
	}
}
