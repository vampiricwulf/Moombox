package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/logger"
	"github.com/vampiricwulf/Moombox/internal/tui"
)

// apiCheckCause is the cause of a tier-1 recovery: the API check failed, or
// the credentials were refused where they were used. Every recovery test that
// is not about the trigger passes it.
var apiCheckCause = cookies.RecoveryCause{Trigger: cookies.RecoveryTriggerAuthCheck}

// triggerTestState is recoveryTestState with the log kept at info, so the
// lines themselves can be read back, over a jar that holds a cookie.
func triggerTestState(t *testing.T) *runState {
	t.Helper()
	log, err := logger.New(filepath.Join(t.TempDir(), "trigger.log"), "info", 4096, 1)
	if err != nil {
		t.Fatalf("logger.New: %v", err)
	}
	log.SuppressStdout()
	t.Cleanup(log.Close)
	return &runState{
		log:           log,
		cookieRefresh: cookies.NewRefreshService(cookies.NewCookieJar(), time.Hour, log),
	}
}

func linesContaining(log *logger.Logger, sub string) int {
	n := 0
	for _, line := range log.GetRecentLines() {
		if strings.Contains(line, sub) {
			n++
		}
	}
	return n
}

// TestRecoverySucceededIsClaimedOnlyAfterAFailedAPICheck is owner ruling
// K4(b). A pass that verified may say "succeeded" only when a failed API check
// asked for it. A pass a signed-out page asked for started with that check
// passing, so it says it ran, that the check passes, and that the result is
// unconfirmed, with its attempt number and the next window. October 2026's
// field log has 55 "succeeded" lines in 60 hours for a session nothing changed.
//
// Mutations: drop the trigger branch from the RefreshOK arm (the page half
// logs "succeeded"); log the page line for every trigger (the API half fails).
func TestRecoverySucceededIsClaimedOnlyAfterAFailedAPICheck(t *testing.T) {
	const succeeded = "auto-cookie recovery succeeded"
	const unconfirmed = "auto-cookie recovery pass ran: the API check passes"
	verified := stubRefresh(cookies.RefreshOK, cookies.RefreshOK)
	var sent []sentNotification

	api := triggerTestState(t)
	api.runCookieRecovery(context.Background(), "youtube", apiCheckCause, verified, recoveryNotifier(&sent))
	if linesContaining(api.log, succeeded) != 1 || linesContaining(api.log, unconfirmed) != 0 {
		t.Errorf("a recovery after a failed API check that verified must say it succeeded, once: %q", api.log.GetRecentLines())
	}

	page := triggerTestState(t)
	cause := cookies.RecoveryCause{Trigger: cookies.RecoveryTriggerPage, Attempt: 4, NextWindow: 4 * time.Hour}
	page.runCookieRecovery(context.Background(), "youtube", cause, verified, recoveryNotifier(&sent))
	if linesContaining(page.log, succeeded) != 0 {
		t.Errorf("a pass a signed-out page asked for claimed success: %q", page.log.GetRecentLines())
	}
	var line string
	for _, l := range page.log.GetRecentLines() {
		if strings.Contains(l, unconfirmed) {
			line = l
		}
	}
	for _, want := range []string{"unconfirmed until pages stay signed in", "attempt=4", "nextAttemptAfter=4h0m0s"} {
		if !strings.Contains(line, want) {
			t.Errorf("the page-triggered line lacks %q: %q", want, line)
		}
	}
	if len(sent) != 0 {
		t.Errorf("a pass that verified sent a notification: %+v", sent)
	}
}

// TestPageTriggeredPassArmsTheSignedOutWarn: the pass above tells the refresh
// service it ran, so the next signed-out page is reported once at Warn, and a
// pass the API check asked for does not arm it.
//
// Mutation: drop NotePageRecoveryPass from the RefreshOK arm.
func TestPageTriggeredPassArmsTheSignedOutWarn(t *testing.T) {
	const stillOut = "a page still renders signed out after the automatic recovery pass"
	verified := stubRefresh(cookies.RefreshOK, cookies.RefreshOK)
	var sent []sentNotification

	for _, tc := range []struct {
		name  string
		cause cookies.RecoveryCause
		want  int
	}{
		{"page", cookies.RecoveryCause{Trigger: cookies.RecoveryTriggerPage, Attempt: 1, NextWindow: 30 * time.Minute}, 1},
		{"api check", apiCheckCause, 0},
	} {
		s := triggerTestState(t)
		s.runCookieRecovery(context.Background(), "youtube", tc.cause, verified, recoveryNotifier(&sent))
		time.Sleep(2 * time.Millisecond) // the verdicts below must be after the pass's stamp
		s.cookieRefresh.ObserveLiveness("youtube", false)
		s.cookieRefresh.ObserveLiveness("youtube", false)
		if got := linesContaining(s.log, stillOut); got != tc.want {
			t.Errorf("%s: %d lines for two signed-out pages after the pass, want %d: %q", tc.name, got, tc.want, s.log.GetRecentLines())
		}
	}
}

// TestTUICookieStatusCarriesTheSessionHealth: the one sender of cookie status
// to the TUI lays the derived health (cookies.SessionHealthOf, the predicate
// the dashboard mirrors) and each back-off window's end on its message.
//
// Mutant: drop the health lines from newTUICookieStatusSender (the bar never
// shows a flicker).
func TestTUICookieStatusCarriesTheSessionHealth(t *testing.T) {
	next := time.Now().Add(time.Hour)
	auth := cookies.AuthStatus{
		Checked:              true,
		YouTubeAuthenticated: true,
		HasYouTubeCookies:    true,
		YouTubeVerification:  cookies.RefreshOK,
		YouTubePages:         cookies.PageHealth{LastSignedOut: time.Now().Add(-time.Hour), Attempts: 2, NextAttempt: next},
		TwitchAuthenticated:  true,
		HasTwitchCookies:     true,
		TwitchVerification:   cookies.RefreshOK,
	}
	ch := make(chan tui.CookieStatusMsg, 1)
	send := newTUICookieStatusSender(
		func() cookies.AuthStatus { return auth },
		func() map[string]bool { return nil },
		func() (bool, bool) { return true, true },
		ch,
	)
	msg := send(auth)
	if msg.YTHealth != cookies.SessionFlickering || !msg.YTNextTry.Equal(next) {
		t.Errorf("YouTube: health %q next %v, want flickering and %v", msg.YTHealth, msg.YTNextTry, next)
	}
	if msg.TWHealth != cookies.SessionOK || !msg.TWNextTry.IsZero() {
		t.Errorf("Twitch: health %q next %v, want ok and no time", msg.TWHealth, msg.TWNextTry)
	}
	if queued := <-ch; queued != msg {
		t.Errorf("the queued message differs from the one returned: %+v", queued)
	}
}
