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

// TestRecheckAfterCookieWriteWithNoServiceIsSafe. wireMonitorCallbacks and
// initServices both run during startup, and a harness may build a runState
// without a refresh service. A nil deref at the moment an operator's
// credentials are repaired is the worst time for one.
//
// The mutation: dropping the nil guard.
func TestRecheckAfterCookieWriteWithNoServiceIsSafe(t *testing.T) {
	log := &recheckLogger{}
	if ran := recheckAfterCookieWrite(context.Background(), nil, log, "recovery"); ran {
		t.Error("reported a pass with no refresh service wired")
	}
	if len(log.infos) != 0 {
		t.Errorf("logged %q with no refresh service wired — there was no stale badge to explain", log.infos)
	}
}
