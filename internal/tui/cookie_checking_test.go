package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// While the first cookie check of the process has not answered, the bar shows
// "checking" for each active platform and nothing that reads as a verdict
// (owner ruling R-H2): not the no-cookies badge, not the re-login prompt, not
// its E L chord.
//
// Mutant: drop the badgeChecking arm from either ladder (that platform falls
// to its default arm and renders a bare code).
func TestStatusBarShowsCheckingUntilTheFirstCookieCheckAnswers(t *testing.T) {
	m := NewStatusBarModel()
	m.SetActivePlatforms(true, true)
	m.SetCookieStatus(CookieStatusChecking, CookieStatusChecking)

	full := stripANSI(m.renderCookieStatus(tierFull, m.tallyJobs()))
	if full != "YT: checking… TW: checking… " {
		t.Errorf("tierFull = %q", full)
	}
	if strings.Contains(full, "Re-login") || strings.Contains(full, "E L") {
		t.Errorf("tierFull names a re-login while checking: %q", full)
	}
	if got := stripANSI(m.renderCookieStatus(tierTight, m.tallyJobs())); got != "YT… TW… " {
		t.Errorf("tierTight = %q", got)
	}
	if m.ReloginPlatform() != "" {
		t.Errorf("ReloginPlatform() = %q while checking", m.ReloginPlatform())
	}

	// Dim, like the neutral Twitch badge: no colour may suggest a verdict.
	raw := m.renderCookieStatus(tierFull, m.tallyJobs())
	if want := DimStyle.Render("YT: checking…"); !strings.Contains(raw, want) {
		t.Errorf("the checking badge is not drawn dim: %q", raw)
	}
	for _, loud := range []lipgloss.Style{statusBarRedStyle, statusBarYelStyle, statusBarGrnStyle} {
		if strings.Contains(raw, loud.Render("YT")) {
			t.Errorf("the checking badge carries a verdict colour: %q", raw)
		}
	}
}

// It is not something to act on, so it is dropped where the healthy badges
// are: at tierEssential, which is all the compact layout's status line shows
// (alerts). A terminal below 60x20 therefore says nothing about cookies
// until there is something to say.
func TestCheckingBadgeIsNotAnAlert(t *testing.T) {
	m := NewStatusBarModel()
	m.SetActivePlatforms(true, true)
	m.SetCookieStatus(CookieStatusChecking, CookieStatusChecking)
	if got := m.renderCookieStatus(tierEssential, m.tallyJobs()); got != "" {
		t.Errorf("tierEssential = %q, want dropped", got)
	}
	if got := m.alerts(); got != "" {
		t.Errorf("alerts() = %q while checking, want none", got)
	}
}

// Checking outranks everything, a parked job included: the precedence list
// puts it first.
func TestCheckingOutranksEveryOtherBadge(t *testing.T) {
	if CookieBadgePrecedence[0] != "checking" {
		t.Fatalf("CookieBadgePrecedence = %v, want checking first", CookieBadgePrecedence)
	}
	for _, parked := range []bool{false, true} {
		if got := cookieBadgeState(CookieStatusChecking, parked); got != "checking" {
			t.Errorf("cookieBadgeState(Checking, parked %v) = %q", parked, got)
		}
	}

	m := NewStatusBarModel()
	m.SetActivePlatforms(true, false)
	m.SetCookieStatus(CookieStatusChecking, CookieStatusChecking)
	m.SetJobs([]*database.Job{{Status: database.StatusCookies, Platform: "youtube"}})
	if got := stripANSI(m.renderCookieStatus(tierFull, m.tallyJobs())); got != "YT: checking… " {
		t.Errorf("checking beside a parked job = %q", got)
	}
}

// The cached bar follows the move out of checking (the badge is a key field).
func TestStatusBarLeavesCheckingWhenTheStatusArrives(t *testing.T) {
	m := NewStatusBarModel()
	m.SetWidth(120)
	m.SetActivePlatforms(true, true)
	m.SetCookieStatus(CookieStatusChecking, CookieStatusChecking)
	before := stripANSI(m.View())
	if !strings.Contains(before, "YT: checking…") {
		t.Fatalf("bar while checking = %q", before)
	}
	m.SetCookieStatus(CookieStatusOK, CookieStatusNone)
	after := stripANSI(m.View())
	if strings.Contains(after, "checking") {
		t.Errorf("bar after the check answered still says checking: %q", after)
	}
}
