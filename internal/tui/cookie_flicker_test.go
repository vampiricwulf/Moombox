package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// TestStatusBarShowsAFlickeringSession is owner ruling K4(c) on the TUI: a
// platform that is signed in by the API check while its pages intermittently
// render signed out says so as text, "YT~", with the end of the recovery
// back-off window at the two widest tiers. It is kept at tierEssential, which is
// all the compact layout's status line shows, and it never replaces a badge
// that already asks for something.
//
// Mutants: drop the flicker arm from either ladder (that platform renders the
// green code, or nothing at tierEssential); test the flicker ahead of the
// precedence switch (the re-login row fails).
func TestStatusBarShowsAFlickeringSession(t *testing.T) {
	next := time.Date(2026, 11, 13, 14, 5, 0, 0, time.Local)
	m := NewStatusBarModel()
	m.SetActivePlatforms(true, true)
	m.SetCookieStatus(CookieStatusOK, CookieStatusOK)
	m.SetSessionHealth(cookies.SessionFlickering, cookies.SessionFlickering, next, time.Time{})

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

	// A badge that already asks for something keeps its own text.
	m.SetCookieStatus(CookieStatusRelogin, CookieStatusCookiesOnly)
	if got := stripANSI(m.renderCookieStatus(tierTight, m.tallyJobs())); got != "YT! TW " {
		t.Errorf("with a re-login and a refusal the bar reads %q, want those badges unchanged", got)
	}

	// Any other health draws nothing new.
	m.SetCookieStatus(CookieStatusOK, CookieStatusOK)
	m.SetSessionHealth(cookies.SessionOK, cookies.SessionRelogin, next, next)
	if got := stripANSI(m.renderCookieStatus(tierFull, m.tallyJobs())); got != "YT TW " {
		t.Errorf("ok / relogin health = %q, want the plain badges", got)
	}
}

// TestStatusBarFlickerNarrowsWithTheTiers: fitTiers scans for the first tier
// that fits and relies on each being no wider than the one before.
func TestStatusBarFlickerNarrowsWithTheTiers(t *testing.T) {
	m := busyStatusBar()
	m.SetSessionHealth(cookies.SessionFlickering, cookies.SessionFlickering, time.Now(), time.Now())
	prev := -1
	for tier := tierFull; tier <= tierNone; tier++ {
		w := len(stripANSI(m.renderCookieStatus(tier, m.tallyJobs())))
		if prev >= 0 && w > prev {
			t.Errorf("tier %d is %d cells, wider than the %d of the tier before it", tier, w, prev)
		}
		prev = w
	}
}
