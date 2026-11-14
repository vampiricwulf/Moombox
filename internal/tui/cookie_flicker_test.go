package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// flickeringFacts is a session the API check passes whose page rendered
// signed out a minute ago, with the recovery back-off window ending at next
// (zero for no recovery decided).
func flickeringFacts(next time.Time) SessionFacts {
	return SessionFacts{
		Found:        true,
		Verification: cookies.RefreshOK,
		Pages:        cookies.PageHealth{LastSignedOut: time.Now().Add(-time.Minute), Attempts: 1, NextAttempt: next},
	}
}

// todayAt is a time today at the local hour and minute, with the clock the
// test hands the bar set to noon, so "14:05" is ahead and on the same day.
func todayAt(hour, minute int) (at, noon time.Time) {
	y, m, d := time.Now().Date()
	return time.Date(y, m, d, hour, minute, 0, 0, time.Local), time.Date(y, m, d, 12, 0, 0, 0, time.Local)
}

// TestStatusBarShowsAFlickeringSession is the session-health ruling on the
// TUI: a platform that is signed in by the API check while its pages
// intermittently render signed out says so as text, "YT~", with the end of the
// recovery back-off window at the two widest tiers. It is kept at
// tierEssential, which is all the compact layout's status line shows, and it
// never replaces a badge that already asks for something.
//
// Mutants: drop the flicker arm from either ladder (that platform renders the
// green code, or nothing at tierEssential); test the flicker ahead of the
// precedence switch (the re-login row fails).
func TestStatusBarShowsAFlickeringSession(t *testing.T) {
	next, noon := todayAt(14, 5)
	m := NewStatusBarModel()
	m.now = func() time.Time { return noon }
	m.SetActivePlatforms(true, true)
	m.SetCookieStatus(CookieStatusOK, CookieStatusOK)
	yt, tw := flickeringFacts(next), flickeringFacts(time.Time{})
	yt.Pages.LastSignedOut, tw.Pages.LastSignedOut = noon.Add(-time.Minute), noon.Add(-time.Minute)
	m.SetSessionHealth(yt, tw)

	if got := stripANSI(m.renderCookieStatus(tierFull, m.tallyJobs())); got != "YT~ pages flicker, retry after 14:05 TW~ pages flicker " {
		t.Errorf("tierFull = %q", got)
	}
	if got := stripANSI(m.renderCookieStatus(tierCompact, m.tallyJobs())); got != "YT~ retry 14:05 TW~ " {
		t.Errorf("tierCompact = %q", got)
	}
	for _, tier := range []barTier{tierKeys, tierTight, tierEssential} {
		if got := stripANSI(m.renderCookieStatus(tier, m.tallyJobs())); got != "YT~ TW~ " {
			t.Errorf("tier %d = %q, want the short form at every narrower tier", tier, got)
		}
	}
	if got := stripANSI(m.alerts()); !strings.Contains(got, "YT~ TW~") {
		t.Errorf("the compact layout's alerts = %q, want the flicker on them", got)
	}
	// Warn-coloured, not the healthy green and not the red of a refusal.
	raw := m.renderCookieStatus(tierTight, m.tallyJobs())
	if !strings.Contains(raw, statusBarWrnStyle.Render("YT~")) {
		t.Errorf("the flicker badge is not drawn in the warning style: %q", raw)
	}

	// The words and the time, for the R C result: there at every terminal
	// size, where the narrow bar has room for neither.
	const wantLine = "YouTube is signed in, but its pages are intermittently signed out; no automatic attempt before 14:05" +
		" | Twitch is signed in, but its pages are intermittently signed out; next automatic attempt when a page next renders signed out"
	if got := m.sessionHealthLine(); got != wantLine {
		t.Errorf("sessionHealthLine = %q\nwant %q", got, wantLine)
	}

	// A badge that already asks for something keeps its own text.
	m.SetCookieStatus(CookieStatusRelogin, CookieStatusCookiesOnly)
	if got := stripANSI(m.renderCookieStatus(tierTight, m.tallyJobs())); got != "YT! TW " {
		t.Errorf("with a re-login and a refusal the bar reads %q, want those badges unchanged", got)
	}
	if got := m.sessionHealthLine(); got != "" {
		t.Errorf("sessionHealthLine = %q beside badges that ask for something, want nothing", got)
	}

	// Any other health draws nothing new: no signed-out page, and a check
	// that failed (relogin, which the badge already says).
	m.SetCookieStatus(CookieStatusOK, CookieStatusOK)
	failed := flickeringFacts(next)
	failed.Verification = cookies.RefreshFailed
	m.SetSessionHealth(SessionFacts{Found: true, Verification: cookies.RefreshOK}, failed)
	if got := stripANSI(m.renderCookieStatus(tierFull, m.tallyJobs())); got != "YT TW " {
		t.Errorf("ok / relogin health = %q, want the plain badges", got)
	}
}

// TestFlickerFollowsTheClock: the health is derived at every draw and never
// stored, so with no further status push the retry time goes once the window
// is behind (the next signed-out page then fires an attempt, which is what the
// dashboard says too), and the badge is plain again once a day has passed with
// no signed-out page. A window that ends on another day names the day.
//
// Mutants: work the health out once in SetSessionHealth (the frames after the
// clock moves stay as they were); leave the derived values out of
// statusBarKey (the cached frame is served).
func TestFlickerFollowsTheClock(t *testing.T) {
	next, noon := todayAt(14, 5)
	now := noon
	m := NewStatusBarModel()
	m.now = func() time.Time { return now }
	m.SetWidth(200)
	m.SetActivePlatforms(true, false)
	m.SetCookieStatus(CookieStatusOK, CookieStatusOK)
	facts := flickeringFacts(next)
	facts.Pages.LastSignedOut = noon.Add(-time.Minute)
	m.SetSessionHealth(facts, SessionFacts{})

	if got := stripANSI(m.View()); !strings.Contains(got, "YT~ pages flicker, retry after 14:05") {
		t.Fatalf("at noon the bar reads %q", got)
	}

	now = next.Add(time.Minute) // the window is behind
	if got := stripANSI(m.View()); !strings.Contains(got, "YT~ pages flicker") || strings.Contains(got, "retry after") {
		t.Errorf("after the window the bar reads %q, want the flicker without a retry time", got)
	}
	if got := m.sessionHealthLine(); !strings.Contains(got, "next automatic attempt when a page next renders signed out") {
		t.Errorf("after the window the R C line reads %q", got)
	}

	now = noon.Add(25 * time.Hour) // a quiet day
	if got := stripANSI(m.View()); strings.Contains(got, "YT~") {
		t.Errorf("a day after the last signed-out page the bar still reads %q", got)
	}

	// A window that ends tomorrow says which day.
	now = noon
	facts.Pages.NextAttempt = next.Add(24 * time.Hour)
	m.SetSessionHealth(facts, SessionFacts{})
	want := "retry after " + next.Add(24*time.Hour).Format("Mon 15:04")
	if got := stripANSI(m.View()); !strings.Contains(got, want) {
		t.Errorf("a window ending tomorrow reads %q, want %q in it", got, want)
	}
}

// TestStatusBarFlickerNarrowsWithTheTiers: fitTiers scans for the first tier
// that fits and relies on each being no wider than the one before.
func TestStatusBarFlickerNarrowsWithTheTiers(t *testing.T) {
	m := busyStatusBar()
	soon := time.Now().Add(time.Hour)
	m.SetSessionHealth(flickeringFacts(soon), flickeringFacts(soon))
	prev := -1
	for tier := tierFull; tier <= tierNone; tier++ {
		w := len(stripANSI(m.renderCookieStatus(tier, m.tallyJobs())))
		if prev >= 0 && w > prev {
			t.Errorf("tier %d is %d cells, wider than the %d of the tier before it", tier, w, prev)
		}
		prev = w
	}
}
