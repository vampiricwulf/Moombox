package main

import (
	"context"
	"strings"
	"testing"
)

// Arc 10 Task 7a. The in-process re-check that must follow every pass which
// may have rewritten cookies.txt, because refresh's status block is the only
// place the Twitch credential fingerprint is compared and the auth mark
// cleared.

// recheckLogger records Info messages and their args, so the skipped line can
// be asserted on. It records nothing else: the other three levels are
// discarded, and no cookie value can reach any of them anyway.
type recheckLogger struct {
	infos []string
	args  [][]any
}

func (l *recheckLogger) Debug(string, ...any) {}
func (l *recheckLogger) Warn(string, ...any)  {}
func (l *recheckLogger) Error(string, ...any) {}
func (l *recheckLogger) Info(msg string, args ...any) {
	l.infos = append(l.infos, msg)
	l.args = append(l.args, args)
}

// TestRecheckAfterCookieWriteRunsThePassAndStaysQuiet.
//
// The mutation: swapping the return polarity, so a pass that RAN logs the
// "status may lag" line. That line tells an operator their badge is stale when
// it is not, which is the one thing worse than not logging at all.
func TestRecheckAfterCookieWriteRunsThePassAndStaysQuiet(t *testing.T) {
	var gotCtx context.Context
	calls := 0
	log := &recheckLogger{}
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "carried")

	ran := recheckAfterCookieWrite(ctx, func(c context.Context) bool {
		calls++
		gotCtx = c
		return true
	}, log, "the browser refresh")

	if !ran {
		t.Error("recheckAfterCookieWrite reported no pass although CheckNow said one ran")
	}
	if calls != 1 {
		t.Errorf("CheckNow was called %d times, want exactly 1", calls)
	}
	if gotCtx == nil || gotCtx.Value(ctxKey{}) != "carried" {
		t.Error("the caller's context did not reach CheckNow — a cancelled request would not cancel the pass")
	}
	if len(log.infos) != 0 {
		t.Errorf("logged %q for a pass that ran, want silence", log.infos)
	}
}

// TestRecheckAfterCookieWriteSaysSoWhenSkipped.
//
// The mutation: dropping the Info line. The caller has just rewritten
// cookies.txt and the in-flight pass read the OLD file, so the badge stays
// stale until the next tick — and this line is the only evidence of why. It
// has been at Info rather than the service's own Debug since Arc 6, for
// exactly that reason.
func TestRecheckAfterCookieWriteSaysSoWhenSkipped(t *testing.T) {
	log := &recheckLogger{}

	ran := recheckAfterCookieWrite(context.Background(), func(context.Context) bool { return false },
		log, "recovery", "platform", "twitch")

	if ran {
		t.Error("recheckAfterCookieWrite reported a pass although CheckNow said none ran")
	}
	if len(log.infos) != 1 {
		t.Fatalf("logged %q, want exactly one line", log.infos)
	}
	if !strings.Contains(log.infos[0], "recovery") {
		t.Errorf("the skipped line does not name the gesture: %q", log.infos[0])
	}
	if !strings.Contains(log.infos[0], "status may lag") {
		t.Errorf("the skipped line does not say what it costs: %q", log.infos[0])
	}
	if len(log.args[0]) != 2 || log.args[0][0] != "platform" || log.args[0][1] != "twitch" {
		t.Errorf("the caller's structured args were dropped: %v", log.args[0])
	}
}

// TestRecheckAfterCookieWriteWithNoCheckFuncIsSafe pins what the guard in the
// helper actually covers: a caller that passes NO func at all.
//
// It is deliberately not named for a runState without a refresh service, which
// is what it claimed until Arc 10's review. The helper cannot see that case:
// every production caller passes s.checkNowFn(), and before that accessor
// existed they passed s.cookieRefresh.CheckNow — a bound method value, which is
// NON-NIL even when taken off a nil *RefreshService, so it steps straight over
// this guard and panics later inside refresh. runState.checkNowFn is what turns
// a nil service into a nil func, and TestCheckNowFnHasNoFuncWithoutAService is
// what pins that half.
//
// The mutation: dropping the nil guard.
func TestRecheckAfterCookieWriteWithNoCheckFuncIsSafe(t *testing.T) {
	log := &recheckLogger{}
	if ran := recheckAfterCookieWrite(context.Background(), nil, log, "recovery"); ran {
		t.Error("reported a pass with no check func wired")
	}
	if len(log.infos) != 0 {
		t.Errorf("logged %q with no check func wired — there was no stale badge to explain", log.infos)
	}
}

// TestCheckNowFnHasNoFuncWithoutAService is the other half of the guard above,
// and the half that makes it real at the five production call sites.
//
// The mutation: returning s.cookieRefresh.CheckNow unconditionally. That
// compiles, and it is what every call site did before the accessor existed —
// the returned func is non-nil, recheckAfterCookieWrite runs it, and the nil
// dereference lands inside refresh at rs.mu.Lock() rather than being declined
// here.
func TestCheckNowFnHasNoFuncWithoutAService(t *testing.T) {
	var s runState
	if fn := s.checkNowFn(); fn != nil {
		t.Fatal("checkNowFn returned a callable re-check for a runState with no refresh service — " +
			"a method value taken off a nil *RefreshService is non-nil and panics when called")
	}
}

// TestCheckNowFnRunsTheServicesCheck. The accessor must still be the real
// re-check when there IS a service; a guard that returns nil unconditionally
// would silence every one of the five sites.
//
// The mutation: returning nil unconditionally.
func TestCheckNowFnRunsTheServicesCheck(t *testing.T) {
	s, _ := recoveryTestState(t)
	fn := s.checkNowFn()
	if fn == nil {
		t.Fatal("checkNowFn returned no re-check although a refresh service is wired")
	}
	if !fn(context.Background()) {
		t.Error("the returned func reported no pass — it is not RefreshService.CheckNow")
	}
}

// TestPostRefreshRecheckHookSurvivesAPanic. The hook this returns is wired into
// AutoCookieService.OnPassCompleted, which the periodic timer calls at loop
// level — and that goroutine's own recover sits OUTSIDE its for loop, so a
// panic reaching it does not cost one tick, it costs the 30-minute browser
// refresh for the life of the process. The hook body is no longer a one-line
// call either: it runs the whole of RefreshService.refresh, including the
// OnCredentialsChanged fan-out into the worker's Twitch chat registry.
//
// The mutation: dropping the recover. The panic then unwinds past the hook,
// out of notePassCompleted, out of the tick, and past the goroutine's recover,
// which returns instead of looping.
func TestPostRefreshRecheckHookSurvivesAPanic(t *testing.T) {
	log := &recheckLogger{}
	ran := 0
	hook := postRefreshRecheckHook(func() {
		ran++
		panic("the credential fan-out blew up")
	}, log)

	hook() // must not panic
	hook() // and the caller is still able to call it again

	if ran != 2 {
		t.Errorf("the hook body ran %d times, want 2 — a recovered panic must not latch the hook off", ran)
	}
}

// TestPostRefreshRecheckHookRunsItsBody. A recover that swallows the work as
// well as the panic would pass the test above and do nothing at all.
//
// The mutation: a body that never calls through.
func TestPostRefreshRecheckHookRunsItsBody(t *testing.T) {
	log := &recheckLogger{}
	ran := 0
	postRefreshRecheckHook(func() { ran++ }, log)()

	if ran != 1 {
		t.Errorf("the hook body ran %d times, want 1", ran)
	}
}
