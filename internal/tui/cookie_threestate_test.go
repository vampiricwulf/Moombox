package tui

import (
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestUncheckedCookiesAreNotRenderedAsRejected is S12 at the pixel.
//
// CookieStatusUnknown used to arrive as CookieStatusCookiesOnly, which is the
// red indicator that survives every width tier — so a DNS blip shouted at the
// operator, in the same colour and with the same persistence as a dead session,
// for as long as the network was unhappy.
//
// The assertions compare RENDERED output, styles included, because the whole
// finding is about colour and persistence. stripANSI would erase the half that
// matters: red "YT" and warning "YT" are the same three characters.
func TestUncheckedCookiesAreNotRenderedAsRejected(t *testing.T) {
	for _, platform := range []struct {
		name string
		code string
		set  func(m *StatusBarModel, s CookieStatus)
	}{
		{"youtube", "YT", func(m *StatusBarModel, s CookieStatus) { m.SetCookieStatus(s, CookieStatusOK) }},
		{"twitch", "TW", func(m *StatusBarModel, s CookieStatus) { m.SetCookieStatus(CookieStatusOK, s) }},
	} {
		t.Run(platform.name, func(t *testing.T) {
			m := NewStatusBarModel()
			m.SetActivePlatforms(true, true)
			platform.set(m, CookieStatusUnknown)

			wide := m.renderCookieStatus(tierFull)
			if !strings.Contains(wide, statusBarWrnStyle.Render(platform.code+": Unknown")) {
				t.Errorf("%s unknown at tierFull = %q, want the warning-styled %q label",
					platform.name, wide, platform.code+": Unknown")
			}
			if strings.Contains(wide, statusBarRedStyle.Render(platform.code)) {
				t.Errorf("%s renders the could-not-check state in the REJECTED colour: %q. "+
					"That is the claim Arc 1 stopped the machinery from making, made to the "+
					"operator's face instead", platform.name, wide)
			}

			// cookieUnknownLabel's own claim — "abbreviates at tierTight",
			// like the re-login prompt beside it. tierFull and tierEssential
			// bracket this tier without saying anything about it, and the
			// width budget is what the whole tier ladder is for: a label that
			// stops shortening pushes the bar over its width and wraps.
			// Rendered comparison again, not stripANSI: the abbreviated form
			// is a PREFIX of the long one as plain text, so only the styled
			// string can tell "YT" from "YT: Unknown".
			tight := m.renderCookieStatus(tierTight)
			if !strings.Contains(tight, statusBarWrnStyle.Render(platform.code)) {
				t.Errorf("%s unknown at tierTight = %q, want the bare %q — the label no longer "+
					"abbreviates where every other indicator does", platform.name, tight, platform.code)
			}

			// The tier rule, applied: un-actionable information goes when
			// space does, exactly like the green OK badge beside it. Before
			// this state existed it was CookiesOnly, which never went.
			if got := m.renderCookieStatus(tierEssential); strings.Contains(got, platform.code) {
				t.Errorf("%s unknown survived to tierEssential (%q). The narrowest bar is "+
					"reserved for what needs acting on, and a site that could not be reached "+
					"is not something the operator can act on", platform.name, got)
			}

			// The premise. If CookiesOnly did not survive tierEssential the
			// contrast above would be vacuous — both states would simply be
			// dropped and the test would pass while saying nothing.
			platform.set(m, CookieStatusCookiesOnly)
			if got := m.renderCookieStatus(tierEssential); !strings.Contains(got, statusBarRedStyle.Render(platform.code)) {
				t.Errorf("premise lost: %s CookiesOnly no longer survives tierEssential (%q), so "+
					"the drop asserted above no longer distinguishes anything", platform.name, got)
			}
		})
	}
}

// TestParkedCookieJobsOutrankAnUnknownCheck pins the ARM ORDER in
// renderCookieStatus's YouTube switch, which until now was asserted only by
// the comment sitting on it — and a comment is not an assertion. Swapping the
// two case arms left this package green.
//
// What the swap ships is the worst direction this arc has: a job parked in
// COOKIES? is evidence from a real download attempt that the credentials are
// dead — independent of the auth check, and stronger than it, because
// something actually tried to use them. Ranked below the unknown arm it
// renders as the hedged "YT: Unknown" AND THEN DISAPPEARS at tierEssential,
// because that arm is gated on `healthy`. Evidence of death, displayed as
// uncertainty, then hidden. The whole point of the tri-state is to stop
// asserting more than we know; it must not start asserting LESS.
//
// The premise block is what makes the first two assertions mean anything. A
// model that rendered red for everything would satisfy them; the same model
// with the parked job removed has to reach the hedged label at tierFull and
// nothing at all at tierEssential, which is the behaviour the parked job is
// overriding.
func TestParkedCookieJobsOutrankAnUnknownCheck(t *testing.T) {
	// YouTube only: cookiesRejected is an input to the YouTube branch alone,
	// so an active Twitch indicator would just add noise to the comparison.
	newBar := func(jobs []*database.Job) *StatusBarModel {
		m := NewStatusBarModel()
		m.SetActivePlatforms(true, false)
		m.SetCookieStatus(CookieStatusUnknown, CookieStatusNone)
		m.SetJobs(jobs)
		return m
	}

	parked := newBar([]*database.Job{{Status: database.StatusCookies}})

	wide := parked.renderCookieStatus(tierFull)
	if !strings.Contains(wide, statusBarRedStyle.Render("YT")) {
		t.Errorf("a job parked in COOKIES? renders as %q. Real evidence the credentials are "+
			"dead must outrank a check that could not reach the site — it is the stronger "+
			"signal, not the weaker one", wide)
	}
	if strings.Contains(wide, statusBarWrnStyle.Render("YT: Unknown")) {
		t.Errorf("a job parked in COOKIES? is being reported as an inconclusive check: %q", wide)
	}

	// The half the hedged arm cannot do, and the reason the swap is dangerous
	// rather than merely wrong: `healthy` drops the unknown arm here.
	if narrow := parked.renderCookieStatus(tierEssential); !strings.Contains(narrow, statusBarRedStyle.Render("YT")) {
		t.Errorf("the COOKIES? alert vanished at tierEssential (%q). The narrowest bar is "+
			"reserved for exactly this — something that needs acting on", narrow)
	}

	// THE PREMISE. Without the parked job the same model must reach the
	// hedged arm and then be dropped; otherwise the two assertions above are
	// satisfied by a bar that alarms unconditionally and prove nothing.
	unparked := newBar(nil)
	if got := unparked.renderCookieStatus(tierFull); !strings.Contains(got, statusBarWrnStyle.Render("YT: Unknown")) {
		t.Fatalf("premise lost: with no parked job the unknown check no longer renders hedged "+
			"(%q), so the override asserted above is not overriding anything", got)
	}
	if got := unparked.renderCookieStatus(tierEssential); got != "" {
		t.Fatalf("premise lost: the unknown check no longer drops at tierEssential (%q), so "+
			"\"the alert survives where the hedge does not\" distinguishes nothing", got)
	}
}

// TestTwitchCookiesOnlyBadgeIsReachable is V5's other half. The arm existed in
// status_bar.go and nothing could assign it: AuthStatus had no
// HasTwitchCookies, so cmd/moombox's wiring could only ever produce
// CookieStatusOK for Twitch.
//
// The YouTube comparison is the point of the test rather than decoration —
// the two switches had already diverged, and "Twitch renders red" is only
// meaningful next to "YouTube renders the same red for the same state".
func TestTwitchCookiesOnlyBadgeIsReachable(t *testing.T) {
	m := NewStatusBarModel()
	m.SetActivePlatforms(true, true)
	m.SetCookieStatus(CookieStatusCookiesOnly, CookieStatusCookiesOnly)

	got := m.renderCookieStatus(tierFull)
	for _, code := range []string{"YT", "TW"} {
		if !strings.Contains(got, statusBarRedStyle.Render(code)) {
			t.Errorf("%s cookies-present-but-rejected is not rendered as an alert: %q", code, got)
		}
	}
}

// TestNoCookiesKeepsItsPlatformSpecificTreatment guards the states this arc did
// NOT change. CookieStatusNone is the zero value, so appending a constant to
// the enum is the classic way to move it by accident; and the YouTube/Twitch
// asymmetry here is deliberate — YouTube without cookies is a warning because
// almost everything wants them, Twitch without cookies is ordinary anonymous
// mode.
func TestNoCookiesKeepsItsPlatformSpecificTreatment(t *testing.T) {
	var zero CookieStatus
	if zero != CookieStatusNone {
		t.Fatalf("the zero CookieStatus is %v, want CookieStatusNone — an unset status must "+
			"keep meaning \"no cookies\", which is what every caller relies on", zero)
	}

	m := NewStatusBarModel()
	m.SetActivePlatforms(true, true)
	m.SetCookieStatus(CookieStatusNone, CookieStatusNone)

	got := m.renderCookieStatus(tierFull)
	if !strings.Contains(got, statusBarYelStyle.Render("YT")) {
		t.Errorf("YouTube with no cookies is no longer the yellow warning: %q", got)
	}
	if !strings.Contains(got, DimStyle.Render("TW")) {
		t.Errorf("Twitch with no cookies is no longer the neutral dim indicator: %q", got)
	}
}

// TestCookieRecheckPartSpeaksFromTheVerdict covers the R C feedback line.
//
// It said "YouTube not authenticated" for every check that failed to REACH
// YouTube — a conclusion the check did not draw, and the most direct way the
// operator gets told to go re-export working cookies.
//
// The invariant at the bottom is the real guard: across every row, the
// conclusive wording appears if and only if the verdict was conclusive. A
// fourth verdict added later cannot quietly inherit it.
func TestCookieRecheckPartSpeaksFromTheVerdict(t *testing.T) {
	for _, tc := range []struct {
		name       string
		verdict    cookies.RefreshVerdict
		wantSaid   []string
		wantUnsaid []string
	}{
		{"alive", cookies.RefreshOK, []string{"YouTube OK"}, []string{"not authenticated", "could not establish"}},
		{"rejected", cookies.RefreshFailed, []string{"not authenticated"}, []string{"could not establish"}},
		{"unreachable", cookies.RefreshUnknown, []string{"could not establish"}, []string{"not authenticated"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := cookieRecheckPart("YouTube", tc.verdict)
			for _, want := range tc.wantSaid {
				if !strings.Contains(got, want) {
					t.Errorf("%v feedback does not say %q: %q", tc.verdict, want, got)
				}
			}
			for _, unwanted := range tc.wantUnsaid {
				if strings.Contains(got, unwanted) {
					t.Errorf("%v feedback asserts %q, which this verdict does not establish: %q",
						tc.verdict, unwanted, got)
				}
			}
			if !strings.HasPrefix(got, "YouTube") {
				t.Errorf("feedback lost the platform name it is joined under: %q", got)
			}
		})
	}
}
