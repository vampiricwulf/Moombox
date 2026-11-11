package cookies

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// A status is unchecked until a pass has finished, and checked from then on
// (owner ruling R-H2). The dashboard is up before the first pass answers and
// reads this to show "checking cookies" in place of a zero status that would
// read as "no cookies".
//
// Mutant: drop `Checked: true` from refresh's status literal AND the release
// defer's write (the status stays unchecked after the pass).
func TestStatusIsUncheckedUntilAPassFinishes(t *testing.T) {
	healthyRefreshSeams(t)
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})

	if rs.GetStatus().Checked {
		t.Fatal("a new service reports Checked before any pass ran")
	}

	passes, entered, release := blockingPass(rs)
	done := make(chan struct{})
	go func() {
		defer close(done)
		rs.CheckNow(context.Background())
	}()
	<-entered
	if rs.GetStatus().Checked {
		t.Error("Checked is set while the first pass is still running")
	}
	// A second caller is turned away by the single-flight and must not end
	// the checking state: nothing has been learned yet.
	if rs.CheckNow(context.Background()) {
		t.Fatal("a second pass ran beside the first")
	}
	if rs.GetStatus().Checked {
		t.Error("a pass that was skipped set Checked")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the pass did not finish")
	}
	if passes.Load() != 1 {
		t.Fatalf("passes = %d, want 1", passes.Load())
	}
	if !rs.GetStatus().Checked {
		t.Error("Checked is false after a finished pass")
	}
}

// The first pass always fires OnAuthChange, even when it finds nothing
// configured and every other field is still its zero value: that push is what
// takes the TUI's badge out of "checking".
//
// Mutant: drop Checked from authStatusChanged (no callback for an empty jar).
func TestFirstPassAnnouncesItselfEvenWithNothingConfigured(t *testing.T) {
	rs := NewRefreshService(NewCookieJar(), 0, nopLogger{})
	var fires atomic.Int64
	var last atomic.Value
	rs.OnAuthChange = func(s AuthStatus) {
		fires.Add(1)
		last.Store(s)
	}

	rs.CheckNow(context.Background())
	if fires.Load() != 1 {
		t.Fatalf("OnAuthChange fired %d times on the first pass, want 1", fires.Load())
	}
	got := last.Load().(AuthStatus)
	if !got.Checked {
		t.Error("the pushed status is not Checked")
	}
	if got.HasYouTubeCookies || got.HasTwitchCookies || got.YouTubeAuthenticated || got.TwitchAuthenticated {
		t.Errorf("an empty jar produced %+v", got)
	}

	// The second pass changes nothing and stays silent, as before.
	rs.CheckNow(context.Background())
	if fires.Load() != 1 {
		t.Errorf("OnAuthChange fired %d times after an unchanged second pass, want 1", fires.Load())
	}
}

// SkipStartupCheck is the no-cookie-file install: no check will run at
// startup, so the status reads as it always has instead of "checking".
func TestSkipStartupCheckMarksTheStatusChecked(t *testing.T) {
	rs := NewRefreshService(NewCookieJar(), 0, nopLogger{})
	rs.SkipStartupCheck()
	got := rs.GetStatus()
	if !got.Checked {
		t.Fatal("Checked is false after SkipStartupCheck")
	}
	if got != (AuthStatus{Checked: true}) {
		t.Errorf("SkipStartupCheck changed more than Checked: %+v", got)
	}
}

// A pass that panics before it publishes a status still ends the checking
// state, so the UIs fall back to what they showed before that state existed.
// The flip is pushed through OnAuthChange once, or a TUI already showing
// "checking" would keep it until some later pass changed another field.
//
// Mutant: drop the OnAuthChange call from refresh's release defer.
func TestAPanickedPassStillEndsTheCheckingState(t *testing.T) {
	healthyRefreshSeams(t)
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	setPassHook(rs, func() { panic("boom") })
	var fires atomic.Int64
	var last atomic.Value
	rs.OnAuthChange = func(s AuthStatus) {
		fires.Add(1)
		last.Store(s)
	}

	awaitOrFail(t, 10*time.Second, "Start", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		rs.Start(ctx)
		rs.Stop()
	})
	if !rs.GetStatus().Checked {
		t.Error("Checked is false after a pass that panicked")
	}
	if fires.Load() != 1 {
		t.Errorf("OnAuthChange fired %d times for the panicked first pass, want 1", fires.Load())
	}
	if got, ok := last.Load().(AuthStatus); !ok || !got.Checked {
		t.Errorf("the pushed status is not Checked: %+v", last.Load())
	}
}

// The gate row: Checked alone moving is a change.
func TestAuthStatusChangedSeesChecked(t *testing.T) {
	if !authStatusChanged(AuthStatus{}, AuthStatus{Checked: true}) {
		t.Error("unchecked -> checked is not reported as a change")
	}
}
