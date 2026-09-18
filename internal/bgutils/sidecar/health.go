package sidecar

import (
	"sync"
	"sync/atomic"
	"time"
)

// Health is the BotGuard sidecar's liveness as both user interfaces report it.
//
// It is a PACKAGE-LEVEL snapshot rather than a field on Sidecar, mirroring
// routes.SharedDiskStatus and routes.SharedUpdateInfo: one value the producer
// writes and each UI's wiring reads, so neither the web routes nor
// cmd/moombox's TUI wiring needs a handle on the Sidecar itself — and
// internal/tui keeps importing neither this package nor internal/bgutils
// (it receives a plain bool through the same app.Send shape ConnectivityMsg
// uses).
type Health struct {
	// Healthy is false from the moment the child dies until a supervisor
	// restart succeeds.
	Healthy bool
	// Reason is why it last went down ("stdout EOF", "readPump panic", a
	// failed initial start). Empty while healthy.
	Reason string
	// Restarts counts SUCCESSFUL supervisor restarts this process.
	Restarts uint64
	// Since is when Healthy last changed.
	Since time.Time
}

// sharedHealth holds the last published snapshot. nil means "never published",
// which is exactly what a process with `[bgutils] use_sidecar = false` looks
// like — and is deliberately NOT the same as "unhealthy", so a correct
// sidecar-disabled config never draws an alert.
var sharedHealth atomic.Pointer[Health]

// healthSubs are the PUSH consumers — today exactly one, cmd/moombox's TUI
// wiring, which turns each call into an app.Send. The Web dashboard polls
// CurrentHealth from /api/status instead and is not in this list.
var (
	healthSubsMu sync.Mutex
	healthSubs   []*func(Health)
)

// PublishHealth records the current sidecar health for both UIs and pushes it
// to every SubscribeHealth consumer.
func PublishHealth(h Health) {
	sharedHealth.Store(&h)

	healthSubsMu.Lock()
	subs := make([]*func(Health), len(healthSubs))
	copy(subs, healthSubs)
	healthSubsMu.Unlock()

	// Fan out off the lock: a subscriber that unsubscribes from inside its own
	// callback (the TUI's exit path can) would otherwise deadlock.
	for _, p := range subs {
		callHealthSub(p, h)
	}
}

// SubscribeHealth registers fn for every health change and calls it IMMEDIATELY
// with the current snapshot (when one exists), so a subscriber that starts
// after the sidecar died still draws the alert. Returns an unsubscribe func;
// mirrors connectivity's OnStateChange, which cmd/moombox unsubscribes the
// same way at TUI exit.
//
// fn runs on the publisher's goroutine (the supervisor loop, or startup) and
// must not block. A panic in it is recovered so one bad subscriber cannot take
// the supervisor down.
func SubscribeHealth(fn func(Health)) (unsubscribe func()) {
	p := &fn
	healthSubsMu.Lock()
	healthSubs = append(healthSubs, p)
	healthSubsMu.Unlock()

	if h, ok := CurrentHealth(); ok {
		callHealthSub(p, h)
	}

	return func() {
		healthSubsMu.Lock()
		defer healthSubsMu.Unlock()
		for i, q := range healthSubs {
			if q == p {
				healthSubs = append(healthSubs[:i], healthSubs[i+1:]...)
				return
			}
		}
	}
}

// callHealthSub isolates one subscriber from the publisher. The silence is
// DELIBERATE: this package has no logger of its own (Sidecar and Supervisor
// each carry the caller's), and the subscribers are one-line projections —
// cmd/moombox's app.Send and the status setter beside it — so the only panic
// reachable here is a nil program pointer or a nil callback, neither of which
// a log line would help anyone diagnose. What matters is that a bad subscriber
// cannot take the supervisor loop or a startup path down with it.
func callHealthSub(p *func(Health), h Health) {
	defer func() { _ = recover() }()
	(*p)(h)
}

// ResetHealthForTesting clears the package-level snapshot AND every
// subscriber. Exported because internal/web/routes needs it: the snapshot is
// process-wide state, and a route test that left one published would change
// what the next test sees. Production never calls it.
func ResetHealthForTesting() {
	sharedHealth.Store(nil)
	healthSubsMu.Lock()
	healthSubs = nil
	healthSubsMu.Unlock()
}

// CurrentHealth returns the last published snapshot. ok is false until the
// first publish.
func CurrentHealth() (Health, bool) {
	if h := sharedHealth.Load(); h != nil {
		return *h, true
	}
	return Health{}, false
}
