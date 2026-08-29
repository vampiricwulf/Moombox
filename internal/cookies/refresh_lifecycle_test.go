package cookies

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// setPassHook installs the refresh test seam under the lock refresh reads it
// with. A free function rather than a method so it is obvious at every call
// site that this is test-only wiring: production has no setter for the field.
func setPassHook(rs *RefreshService, h func()) {
	rs.mu.Lock()
	rs.refreshPassHook = h
	rs.mu.Unlock()
}

// setLockedHook installs the seam that fires INSIDE refresh's status-update
// critical section, with rs.mu held for writing. See refreshLockedHook for why
// it is a second field and not a second call to the first.
func setLockedHook(rs *RefreshService, h func()) {
	rs.mu.Lock()
	rs.refreshLockedHook = h
	rs.mu.Unlock()
}

// awaitOrFail runs fn on its own goroutine and fails the test if it has not
// returned within d.
//
// Used wherever the regression under test is a DEADLOCK rather than a wrong
// answer. Calling such a function directly would hang the whole package binary
// until go test's global timeout and report as an unattributable "test timed
// out" naming whatever ran last; this names the call that never came back and
// lets the rest of the suite finish.
func awaitOrFail(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %v — a panic almost certainly unwound with rs.mu held, so the in-flight guard's release defer is blocked on Lock() and the goroutine is parked holding the lock", what, d)
	}
}

// warnRecordingLogger counts Warn calls and only Warn calls.
//
// Deliberately not recordingCookieLogger, which flattens all four levels into
// one slice: the clamp's whole contract is that it is LOUD (Warn) for a refused
// interval and SILENT for an accepted one, and a level-blind recorder cannot
// tell "no warning" from "a Debug line instead".
type warnRecordingLogger struct {
	mu    sync.Mutex
	warns []string
}

func (l *warnRecordingLogger) Debug(msg string, args ...any) {}
func (l *warnRecordingLogger) Info(msg string, args ...any)  {}
func (l *warnRecordingLogger) Error(msg string, args ...any) {}
func (l *warnRecordingLogger) Warn(msg string, args ...any) {
	l.mu.Lock()
	l.warns = append(l.warns, msg)
	l.mu.Unlock()
}

func (l *warnRecordingLogger) warnCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.warns)
}

// blockingPass wires a pass hook that parks the FIRST pass until the returned
// release channel is closed, counting every pass that gets past the in-flight
// guard.
//
// The counter is the point. "The second call returned" is a junction that a
// build with no guard at all satisfies just as well as a guarded one — both
// return — so every assertion below counts PASSES, which only a real guard can
// hold at one.
func blockingPass(rs *RefreshService) (passes *atomic.Int64, entered <-chan struct{}, release chan struct{}) {
	var n atomic.Int64
	enteredCh := make(chan struct{})
	releaseCh := make(chan struct{})
	setPassHook(rs, func() {
		if n.Add(1) == 1 {
			close(enteredCh)
			<-releaseCh
		}
	})
	return &n, enteredCh, releaseCh
}

// TestStartupRefreshPanicDoesNotEscapeStart is S3(a).
//
// Start's initial check runs SYNCHRONOUSLY on the caller's goroutine —
// cmd/moombox's run(), before the web server binds — so an unrecovered panic in
// the first pass does not lose a refresh, it kills the process at boot with no
// dashboard, no TUI and no log surface up to say why. The ticker goroutine has
// carried a recover forever; this call did not.
//
// The pass counter is not decoration. Without it, "Start returned normally" is
// satisfied equally well by a working recover and by a seam that never fired,
// and the test would stay green against a build with the recover deleted and
// the hook call deleted too.
//
// THE PANIC IS INJECTED INSIDE THE STATUS-UPDATE CRITICAL SECTION, with rs.mu
// held for writing, and that is the whole point of the seam choice. A panic
// raised outside every lock unwinds to Start's recover trivially and proves only
// that the recover and the guard-release defer exist. The case that can actually
// go wrong is a panic while rs.mu is held: the release defer needs that same
// non-reentrant lock, so it blocks on Lock() forever, the goroutine parks
// holding rs.mu, the panic never leaves refresh, Start's recover never runs, and
// every later GetStatus() (RLock) queues behind it. At boot that is not a crash,
// it is a silent hang with no dashboard, no TUI and no log line — strictly worse
// than the loud crash this task set out to fix.
//
// Every step therefore runs through awaitOrFail: the failure mode is a deadlock,
// so a broken build must produce a named failure, not a hung binary.
func TestStartupRefreshPanicDoesNotEscapeStart(t *testing.T) {
	healthyRefreshSeams(t)
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})

	// Counts outside the lock (the premise: a pass really started), panics
	// inside it (the property: the unwind releases rs.mu AND the guard).
	var passes atomic.Int64
	setPassHook(rs, func() { passes.Add(1) })
	setLockedHook(rs, func() {
		panic("synthetic panic inside refresh's status-update critical section")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	awaitOrFail(t, 10*time.Second, "Start", func() { rs.Start(ctx) })
	rs.Stop()

	if got := passes.Load(); got != 1 {
		t.Fatalf("the startup pass ran %d times, want 1 — the panic seam never fired, so Start returning proves nothing", got)
	}

	// rs.mu must be free. This is the reader every surface uses — the Web
	// indicators, the TUI badge — and it takes RLock, so a write lock stranded
	// by the unwind blocks it forever even though Start itself returned.
	awaitOrFail(t, 10*time.Second, "GetStatus after the panic", func() { rs.GetStatus() })

	// The service must still be USABLE, not merely alive. The in-flight guard is
	// released by a defer, so it has to survive stack unwinding; a guard left
	// latched by the panic would make every later pass — ticker included — a
	// silent no-op for the life of the process.
	setLockedHook(rs, nil)
	var ran bool
	awaitOrFail(t, 10*time.Second, "CheckNow after the panic", func() {
		ran = rs.CheckNow(context.Background())
	})
	if !ran {
		t.Error("a refresh after the startup panic reported that it did not run — the in-flight guard was left latched by the unwind")
	}
	if got := passes.Load(); got != 2 {
		t.Errorf("passes = %d after the post-panic refresh, want 2 — the service did not recover to a working state", got)
	}
}

// TestConcurrentCheckNowRunsOnePass is S2 on the manual path.
//
// Two overlapping passes are not merely wasteful: each one fetches the guide,
// merges its own Set-Cookie headers and rewrites the SAME cookies.txt, and the
// two rewrites interleave. /api/cookies/recheck sits on the plain router, so the
// collision needs no special timing to arrange.
func TestConcurrentCheckNowRunsOnePass(t *testing.T) {
	healthyRefreshSeams(t)
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	passes, entered, release := blockingPass(rs)

	first := make(chan bool, 1)
	go func() { first <- rs.CheckNow(context.Background()) }()
	<-entered

	// A pass is now parked inside the guard.
	if started := rs.CheckNow(context.Background()); started {
		t.Error("a second CheckNow reported that it ran a pass while one was already in flight")
	}
	if got := passes.Load(); got != 1 {
		t.Errorf("passes = %d while one was in flight, want 1 — the second caller ran a full second pass over the same cookie file", got)
	}

	close(release)
	if started := <-first; !started {
		t.Error("the first CheckNow reported that it did not run — the guard refused the caller that was holding it")
	}
	if got := passes.Load(); got != 1 {
		t.Errorf("passes = %d after both calls returned, want 1", got)
	}

	// THE RELEASE. A guard that never clears would pass every assertion above
	// and then wedge the service permanently.
	if started := rs.CheckNow(context.Background()); !started {
		t.Error("a CheckNow after both earlier calls returned did not run — the guard latched")
	}
	if got := passes.Load(); got != 2 {
		t.Errorf("passes = %d after the third CheckNow, want 2", got)
	}
}

// TestTickerRefreshIsGuardedByAnInFlightManualPass is the same guard from the
// other direction, and it is the direction with the operator-visible cost: a
// tick dropped this way does not come back for a full interval.
//
// It is asserted separately because doRefresh is a distinct entry point with a
// distinct argument (allowFallback=true), and a guard placed in CheckNow rather
// than in the shared body would satisfy the test above and leave this one open.
func TestTickerRefreshIsGuardedByAnInFlightManualPass(t *testing.T) {
	healthyRefreshSeams(t)
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	passes, entered, release := blockingPass(rs)

	first := make(chan bool, 1)
	go func() { first <- rs.CheckNow(context.Background()) }()
	<-entered

	rs.doRefresh(context.Background())
	if got := passes.Load(); got != 1 {
		t.Errorf("passes = %d after a tick landed on an in-flight manual pass, want 1 — the ticker doubled up", got)
	}

	close(release)
	<-first

	rs.doRefresh(context.Background())
	if got := passes.Load(); got != 2 {
		t.Errorf("passes = %d after a tick following a completed pass, want 2 — the guard latched and the ticker is now dead", got)
	}
}

// TestNewRefreshServiceRefusesAnIntervalBelowTheLivenessWindow is follow-up 5.
//
// livenessFreshWindow bounds how old a liveness observation may be and still
// suppress the fallback probe, and the probe records its own answer through the
// same method. Let the window meet or exceed the refresh interval and the
// probe's own stamp is still fresh on the next tick, so it suppresses itself on
// alternate cycles — coverage halved, with no symptom anywhere. Nothing in
// production reaches this today (no config knob feeds the parameter, and the
// one production constructor passes 0); the clamp is what stops the next test
// constructor or config knob from breaking the invariant silently.
//
// The effective interval is read from the field the ticker is actually built
// with, not re-derived from the rule under test.
func TestNewRefreshServiceRefusesAnIntervalBelowTheLivenessWindow(t *testing.T) {
	cases := []struct {
		name  string
		in    time.Duration
		want  time.Duration
		warns int
	}{
		{"below the window is refused, loudly", 10 * time.Minute, defaultRefreshInterval, 1},
		// The invariant is "strictly shorter", so equality is a breach.
		{"exactly the window is refused too", livenessFreshWindow, defaultRefreshInterval, 1},
		{"zero means the default, silently", 0, defaultRefreshInterval, 0},
		{"above the window is kept as asked", 45 * time.Minute, 45 * time.Minute, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := &warnRecordingLogger{}
			rs := NewRefreshService(NewCookieJar(), tc.in, log)

			if rs.refreshInterval != tc.want {
				t.Errorf("effective interval = %v, want %v", rs.refreshInterval, tc.want)
			}
			if got := log.warnCount(); got != tc.warns {
				t.Errorf("Warn calls = %d, want %d — a substituted interval must say so, and an accepted one must stay quiet", got, tc.warns)
			}
		})
	}
}

// TestLivenessFreshWindowStaysBelowTheDefaultInterval pins the invariant the
// clamp enforces, at the one value production actually uses. If this pair ever
// inverts, the clamp above is protecting a rule the constants themselves break.
func TestLivenessFreshWindowStaysBelowTheDefaultInterval(t *testing.T) {
	if livenessFreshWindow >= defaultRefreshInterval {
		t.Fatalf("livenessFreshWindow (%v) must be strictly shorter than defaultRefreshInterval (%v) — the fallback probe's own observation would still read as fresh on the next tick and suppress the probe on alternate cycles",
			livenessFreshWindow, defaultRefreshInterval)
	}
}
