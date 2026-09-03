package tui

import (
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// rlIsOffered reports whether R L is reachable by the three routes an operator
// has: the chord dispatcher, the action menu, and the help overlay. All three,
// because they are three separate reads of OnStartAutoCookie and a fix that
// restored one would look complete from the others. Same shape as rfIsOffered
// in cookie_forcerefresh_chord_test.go, deliberately — the two chords answer
// the same question about the same service.
func rlIsOffered(t *testing.T, app *App) (dispatch, menu, help bool) {
	t.Helper()

	app.dispatchAction("R L", nil)
	dispatch = app.setupWiz.IsVisible() && app.setupWiz.cookieOnly
	app.setupWiz.closeCookieLogin() // the funnel, not Close(): leaves no cookieOnly behind

	items := app.buildMenuItems()
	if len(items) == 0 {
		t.Fatal("buildMenuItems returned nothing — nothing below can be concluded")
	}
	for _, it := range items {
		if strings.TrimSpace(it.Chord) == "R L" {
			menu = true
		}
	}

	h := NewHelpModel()
	h.SetMenuItems(items)
	for _, sec := range h.orderedSections() {
		for _, k := range sec.keys {
			if strings.TrimSpace(k.key) == "R L" {
				help = true
			}
		}
	}
	return dispatch, menu, help
}

// wiredCookieLoginApp is an App with the interactive-setup callbacks bound the
// way cmd/moombox binds them — unconditionally, all three.
func wiredCookieLoginApp() *App {
	app := NewApp()
	app.SetSetupCallbacks(
		func(*config.MoomboxConfig) error { return nil },
		func(int, bool) {},
		func(string) error { return nil },
		func() (cookies.SetupResult, error) { return cookies.SetupResult{}, nil },
		func() {},
		func() {},
	)
	return app
}

// TestCookieLoginChordExistsWheneverTheWizardCanStartALogin is the junction
// guard, and the reason the chord is gated on the callback rather than on
// cookies.auto_enabled. A nil callback does not make a chord inert, it DELETES
// it — dispatchAction, buildMenuItems and the help overlay each test the field
// — which is exactly the defect R F was fixed for. cmd/moombox binds this
// callback unconditionally, so the chord exists on every real install,
// including one with auto_enabled off: StartSetup is acquisition, never gated.
//
// The nil row gives the wired row its meaning: without it, "R L is offered"
// would be satisfied by a build that offers every chord unconditionally.
func TestCookieLoginChordExistsWheneverTheWizardCanStartALogin(t *testing.T) {
	t.Run("wired", func(t *testing.T) {
		dispatch, menu, help := rlIsOffered(t, wiredCookieLoginApp())
		if !dispatch {
			t.Error("R L opened no cookie-login overlay although the callback is wired")
		}
		if !menu {
			t.Error("R L is absent from the action menu although the callback is wired")
		}
		if !help {
			t.Error("R L is absent from help although the callback is wired — an operator cannot " +
				"discover a chord that is documented nowhere")
		}
	})

	t.Run("not wired", func(t *testing.T) {
		app := NewApp() // SetSetupCallbacks never called
		dispatch, menu, help := rlIsOffered(t, app)
		if dispatch || menu || help {
			t.Fatalf("premise lost: R L is offered with no interactive-setup callback "+
				"(dispatch=%v menu=%v help=%v)", dispatch, menu, help)
		}
	})
}

// TestCookieLoginChordOpensAtTheCookieStep pins WHERE the chord lands.
//
// MUTANT: have the case call a.setupWiz.Open(). The wizard then opens on the
// "Welcome to Moombox — Quick or Advanced" mode-selection screen, from a chord
// whose label says Cookie Login, and its Advanced path leads to a form that
// rewrites config.toml.
func TestCookieLoginChordOpensAtTheCookieStep(t *testing.T) {
	app := wiredCookieLoginApp()

	app.dispatchAction("R L", nil)

	if !app.setupWiz.IsVisible() {
		t.Fatal("R L opened nothing")
	}
	if app.setupWiz.mode != setupModeSimple {
		t.Errorf("mode = %v, want setupModeSimple", app.setupWiz.mode)
	}
	if app.setupWiz.simpleStage != setupSimpleCookies {
		t.Errorf("stage = %v, want setupSimpleCookies", app.setupWiz.simpleStage)
	}
	if !app.setupWiz.cookieOnly {
		t.Error("the chord opened the first-run flow rather than the cookie-login overlay")
	}
}

// TestCookieLoginChordPreselectsThePlatformTheBadgeIsAlarmingAbout closes the
// loop between the alert and its remedy.
//
// MUTANT: pass "" instead of a.statusBar.ReloginPlatform(). The Twitch row then
// opens focused on YouTube, and the operator answering a "TW: Re-login" badge
// signs in to the wrong site.
func TestCookieLoginChordPreselectsThePlatformTheBadgeIsAlarmingAbout(t *testing.T) {
	for _, tc := range []struct {
		name      string
		yt, tw    CookieStatus
		ytA, twA  bool
		wantFocus int
	}{
		{"twitch flagged", CookieStatusOK, CookieStatusRelogin, true, true, 1},
		{"youtube flagged", CookieStatusRelogin, CookieStatusOK, true, true, 0},
		{"both flagged — youtube first", CookieStatusRelogin, CookieStatusRelogin, true, true, 0},
		{"nothing flagged", CookieStatusOK, CookieStatusOK, true, true, 0},
		{"twitch flagged but inactive", CookieStatusOK, CookieStatusRelogin, true, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := wiredCookieLoginApp()
			app.statusBar.SetActivePlatforms(tc.ytA, tc.twA)
			app.statusBar.SetCookieStatus(tc.yt, tc.tw)

			app.dispatchAction("R L", nil)

			if app.setupWiz.cookieFocus != tc.wantFocus {
				t.Errorf("cookieFocus = %d, want %d", app.setupWiz.cookieFocus, tc.wantFocus)
			}
		})
	}
}

// TestCookieLoginChordRefusesWithoutTheService pins the DEFENSIVE branch: a
// direct dispatch with no interactive-setup callback opens nothing and says
// so. From the keyboard this branch is unreachable — processSecondKey resolves
// the second key against buildMenuItems, and with the callback nil R L is not
// registered, so the operator sees the chord system's own "Invalid Chord: R L"
// and finds no menu or help entry (the "not wired" row above pins that). The
// guard exists so a programmatic caller cannot open an overlay whose every
// Enter dead-ends, and so the two reads of OnStartAutoCookie — buildMenuItems
// and this case — cannot disagree silently.
//
// MUTANT: drop the nil check. dispatchAction("R L") on a bare App then opens
// the overlay with nothing behind it.
func TestCookieLoginChordRefusesWithoutTheService(t *testing.T) {
	app := NewApp()

	app.dispatchAction("R L", nil)

	if app.setupWiz.IsVisible() {
		t.Error("R L opened a login overlay with no auto-cookie service behind it")
	}
	if app.feedbackMsg == "" {
		t.Fatal("R L refused silently — a direct caller cannot tell that from a no-op")
	}
	if !strings.Contains(strings.ToLower(app.feedbackMsg), "cookie login") {
		t.Errorf("the refusal does not name what was refused: %q", app.feedbackMsg)
	}
}
