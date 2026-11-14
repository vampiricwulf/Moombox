package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// The renewal line (cookies.RenewalLine, owner ruling K5) on the TUI's two
// cookie results: R C and a successful R F.

const renewalLineSample = "Renewal: YouTube replies renewed cookies 3 min ago; " +
	"browser pass 2 h ago renewed SID family, SIDCC (oldest: PSIDTS pair, 3 d ago); next daily visit in 21 h"

// TestRecheckLineEndsWithTheRenewalLine: the line comes last, after a
// recorded error, so the row cap takes it first, and it moves no colour: a
// healthy check stays green and a recorded error stays a warning.
//
// Mutants: the clause appended ahead of the recorded error; the clause
// raising the severity.
func TestRecheckLineEndsWithTheRenewalLine(t *testing.T) {
	ok := cookies.RecheckReport(cookies.RecheckedPlatform{Label: "YouTube", Verdict: cookies.RefreshOK})

	app := NewApp()
	app.width = 200
	app.statusBar.SetActivePlatforms(true, false)
	app.Update(cookieRecheckResultMsg{YouTube: cookies.RefreshOK, Renewal: renewalLineSample})
	if want := ok + " | " + renewalLineSample; app.feedback.msg != want {
		t.Errorf("R C feedback = %q, want %q", app.feedback.msg, want)
	}
	if app.feedback.sev != severitySuccess {
		t.Errorf("severity = %v, want success: the renewal line states facts and asks for nothing", app.feedback.sev)
	}

	app.Update(cookieRecheckResultMsg{YouTube: cookies.RefreshOK, LastError: recordedCookieError, Renewal: renewalLineSample})
	if want := ok + " | Last cookie error: " + recordedCookieError + " | " + renewalLineSample; app.feedback.msg != want {
		t.Errorf("R C feedback = %q, want %q", app.feedback.msg, want)
	}
	if app.feedback.sev != severityWarning {
		t.Errorf("severity = %v, want warning from the recorded error", app.feedback.sev)
	}
}

// TestRenewalLineIsDrawnWholeAtEverySize: on R C and on a successful R F the
// line wraps instead of being cut, in the normal layout and in the compact
// one, and the result stays green.
//
// Mutant: R F's success arm back on setFeedback (one row, cut at the width).
func TestRenewalLineIsDrawnWholeAtEverySize(t *testing.T) {
	renewed := cookies.RefreshResult{Ran: true, Renewed: true, Mechanism: cookies.RefreshMechanismBrowser, YouTube: cookies.RefreshOK}
	if renewed.Overall() != cookies.RefreshOK {
		t.Fatalf("fixture: Overall = %v", renewed.Overall())
	}
	for _, size := range []struct{ w, h int }{{120, 30}, {60, 20}, {40, 16}} {
		for name, msg := range map[string]any{
			"R C": cookieRecheckResultMsg{YouTube: cookies.RefreshOK, Renewal: renewalLineSample},
			"R F": cookieForceRefreshResultMsg{Result: renewed, Renewal: renewalLineSample},
		} {
			t.Run(fmt.Sprintf("%s/%dx%d", name, size.w, size.h), func(t *testing.T) {
				frame := drawnFrame(t, size.w, size.h, true, false, msg)
				if !strings.Contains(strings.Join(frame, " "), renewalLineSample) {
					t.Errorf("the renewal line is not on screen whole:\n%s", strings.Join(frame, "\n"))
				}
			})
		}
	}

	app := NewApp()
	app.width, app.height = 120, 30
	app.recalcLayout()
	app.Update(cookieForceRefreshResultMsg{Result: renewed, Renewal: renewalLineSample})
	if !strings.HasSuffix(app.feedback.msg, " successful | "+renewalLineSample) || app.feedback.sev != severitySuccess {
		t.Errorf("R F feedback = %q (severity %v), want the success line, then the renewal line, in green",
			app.feedback.msg, app.feedback.sev)
	}
}

// TestCookieChordsReadTheRenewalLine: both chords' commands ask the callback
// and carry its answer on their result.
func TestCookieChordsReadTheRenewalLine(t *testing.T) {
	app := NewApp()
	app.OnCookieRenewalLine = func() string { return renewalLineSample }
	app.OnRecheckCookies = func() (cookies.RefreshVerdict, cookies.RefreshVerdict, string, string) {
		return cookies.RefreshOK, cookies.RefreshOK, "", ""
	}
	app.OnForceRefreshCookies = func() (cookies.RefreshResult, error) { return cookies.RefreshResult{}, nil }

	_, cmd := app.dispatchAction("R C", nil)
	if got, _ := cmd().(cookieRecheckResultMsg); got.Renewal != renewalLineSample {
		t.Errorf("R C result carries %q", got.Renewal)
	}
	_, cmd = app.dispatchAction("R F", nil)
	if got, _ := cmd().(cookieForceRefreshResultMsg); got.Renewal != renewalLineSample {
		t.Errorf("R F result carries %q", got.Renewal)
	}
}

// TestRecheckLineSaysAFlickeringSessionInWords: the bar has room only for
// "YT~" below its two widest tiers, so the R C result carries the state as a
// sentence with the time of the next automatic attempt, at every terminal
// size. It asks for nothing: a healthy check stays green.
//
// Mutant: leave sessionHealthLine out of cookieRecheckFeedback.
func TestRecheckLineSaysAFlickeringSessionInWords(t *testing.T) {
	next, noon := todayAt(14, 5)
	app := NewApp()
	app.width = 200
	app.statusBar.now = func() time.Time { return noon }
	app.statusBar.SetActivePlatforms(true, false)
	app.statusBar.SetCookieStatus(CookieStatusOK, CookieStatusOK)
	facts := flickeringFacts(next)
	facts.Pages.LastSignedOut = noon.Add(-time.Minute)
	app.SetSessionHealth(facts, SessionFacts{})

	app.Update(cookieRecheckResultMsg{YouTube: cookies.RefreshOK, Renewal: renewalLineSample})
	const health = "YouTube is signed in, but its pages are intermittently signed out; no automatic attempt before 14:05"
	ok := cookies.RecheckReport(cookies.RecheckedPlatform{Label: "YouTube", Verdict: cookies.RefreshOK})
	if want := ok + " | " + health + " | " + renewalLineSample; app.feedback.msg != want {
		t.Errorf("R C feedback = %q\nwant %q", app.feedback.msg, want)
	}
	if app.feedback.sev != severitySuccess {
		t.Errorf("severity = %v, want success: the flicker asks for nothing", app.feedback.sev)
	}
}
