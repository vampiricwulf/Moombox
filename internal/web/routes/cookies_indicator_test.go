package routes

import (
	"strings"
	"testing"

	"github.com/dop251/goja"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// This file covers the dashboard badge — the surface S12 is about — and it
// covers it by RUNNING the shipped module, for the reason cookies_setup_
// utilsvm_test.go states at length: three rounds of review found three defects
// where an assertion about JS was written as a string match and stayed green
// while the behaviour it named was broken.
//
// cookieIndicatorState lives in utils.js precisely so it can be run here. The
// chains it replaced were inline in app.js's updateStatusBar, which is
// DOM-coupled and cannot be loaded — which is why they were never tested and
// why they had already drifted apart between the two platforms.

// indicatorState calls cookieIndicatorState and unpacks {className, title}.
func indicatorState(t *testing.T, vm *goja.Runtime, platform string, status map[string]any, relogin bool) (className, title string) {
	t.Helper()
	raw := jsCall(t, vm, "cookieIndicatorState", platform, status, relogin)
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("cookieIndicatorState returned %T, want the {className, title} object", raw)
	}
	className, _ = m["className"].(string)
	title, _ = m["title"].(string)
	if className == "" || title == "" {
		t.Fatalf("indicator is missing className or title: %v", m)
	}
	return className, title
}

// TestCookieIndicatorSeparatesUncheckedFromRejected is the Web half of S12,
// executed.
//
// The old chain ended `else -> indicator-error, "YouTube: Not verified"`, and
// the server reports `authenticated: false` for a check that could not REACH
// YouTube exactly as it does for one YouTube rejected. So a transient fault
// rendered as the red badge, and the reason sat in an AuthStatus field with no
// reader at all.
//
// THE ROWS THAT CARRY THE ADDITIVE CONTRACT are the ones with no
// `verification` key: a NEWER FRONTEND AGAINST AN OLDER BINARY. Those users
// must see exactly the badge they see today, and the property that delivers it
// is that the comparison runs POSITIVELY (`=== "unknown"`). Invert it and the
// undefined rows go warning, which is a change to what every older install
// displays.
//
// The Twitch "older binary" row carries the other half, and it is the one a
// bracketed source match could not have caught: `found` did not exist in the
// twitchAuthStatus payload until this arc, so on an older binary it is
// undefined — and if the absence test ran before the authenticated test, a
// working signed-in Twitch session would render as "Anonymous".
func TestCookieIndicatorSeparatesUncheckedFromRejected(t *testing.T) {
	vm := utilsVM(t)

	for _, tc := range []struct {
		name       string
		platform   string
		status     map[string]any
		relogin    bool
		wantClass  string
		wantSaid   []string
		wantUnsaid []string
	}{
		{
			name: "youtube signed in", platform: "youtube",
			status:    map[string]any{"found": true, "authenticated": true, "verification": "ok"},
			wantClass: "indicator-ok", wantSaid: []string{"Authenticated"},
			wantUnsaid: []string{"could not establish"},
		},
		{
			// Conclusive. The only row that earns the red badge and the word.
			name: "youtube rejected", platform: "youtube",
			status:    map[string]any{"found": true, "authenticated": false, "verification": "failed"},
			wantClass: "indicator-error", wantSaid: []string{"Not authenticated"},
			wantUnsaid: []string{"could not establish"},
		},
		{
			// THE FIX. Byte-identical `authenticated` to the row above.
			name: "youtube could not be reached", platform: "youtube",
			status:    map[string]any{"found": true, "authenticated": false, "verification": "unknown"},
			wantClass: "indicator-warn", wantSaid: []string{"could not establish"},
			wantUnsaid: []string{"Not authenticated", "No cookies"},
		},
		{
			name: "youtube never configured", platform: "youtube",
			status:    map[string]any{"found": false, "authenticated": false, "verification": "failed"},
			wantClass: "indicator-warn", wantSaid: []string{"No cookies"},
			wantUnsaid: []string{"could not establish", "Not authenticated"},
		},
		{
			// A platform that was never set up must not be described by the
			// check's verdict at all: the check answers "not authenticated"
			// conclusively for it, and saying so invents a sign-in.
			name: "youtube never configured, and unchecked too", platform: "youtube",
			status:    map[string]any{"found": false, "authenticated": false, "verification": "unknown"},
			wantClass: "indicator-warn", wantSaid: []string{"No cookies"},
			wantUnsaid: []string{"could not establish"},
		},
		{
			name: "older binary emits no verification", platform: "youtube",
			status:    map[string]any{"found": true, "authenticated": false},
			wantClass: "indicator-error", wantSaid: []string{"Not authenticated"},
			wantUnsaid: []string{"could not establish"},
		},
		{
			name: "relogin outranks everything", platform: "youtube",
			status:  map[string]any{"found": true, "authenticated": true, "verification": "ok"},
			relogin: true, wantClass: "indicator-error", wantSaid: []string{"Re-login"},
		},
		{
			name: "twitch signed in", platform: "twitch",
			status:    map[string]any{"found": true, "authenticated": true, "verification": "ok"},
			wantClass: "indicator-ok", wantSaid: []string{"Authenticated"},
		},
		{
			// V5 on the Web side. Cookies configured, token rejected — this
			// rendered as the neutral "Anonymous" dot, indistinguishable from
			// a Twitch that was never set up.
			name: "twitch configured but rejected", platform: "twitch",
			status:    map[string]any{"found": true, "authenticated": false, "verification": "failed"},
			wantClass: "indicator-error", wantSaid: []string{"Not authenticated"},
			wantUnsaid: []string{"Anonymous"},
		},
		{
			name: "twitch could not be reached", platform: "twitch",
			status:    map[string]any{"found": true, "authenticated": false, "verification": "unknown"},
			wantClass: "indicator-warn", wantSaid: []string{"could not establish"},
			wantUnsaid: []string{"Anonymous", "Not authenticated"},
		},
		{
			name: "twitch never configured", platform: "twitch",
			status:    map[string]any{"found": false, "authenticated": false, "verification": "failed"},
			wantClass: "indicator-off", wantSaid: []string{"Anonymous"},
		},
		{
			// THE ORDERING TRAP. An older binary sends twitchAuthStatus with
			// `authenticated` and nothing else. Test the absence of `found`
			// before the presence of `authenticated` and every signed-in
			// Twitch user on an older build is told they are anonymous.
			name: "twitch on an older binary, signed in, no found key", platform: "twitch",
			status:    map[string]any{"authenticated": true},
			wantClass: "indicator-ok", wantSaid: []string{"Authenticated"},
			wantUnsaid: []string{"Anonymous"},
		},
		{
			name: "twitch on an older binary, not signed in", platform: "twitch",
			status:    map[string]any{"authenticated": false},
			wantClass: "indicator-off", wantSaid: []string{"Anonymous"},
			wantUnsaid: []string{"could not establish"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			className, title := indicatorState(t, vm, tc.platform, tc.status, tc.relogin)
			if className != tc.wantClass {
				t.Errorf("className = %q, want %q (title %q)", className, tc.wantClass, title)
			}
			assertCopy(t, title, tc.wantSaid, tc.wantUnsaid)

			// THE INVARIANT, asserted on every row rather than only where the
			// hedge is expected: the hedged wording appears if and only if
			// the payload said "unknown" about a configured platform. A row
			// added later cannot start hedging, and — the direction that
			// actually costs the user — cannot stop.
			hedged := strings.Contains(title, "could not establish")
			wantHedged := tc.status["verification"] == "unknown" &&
				tc.status["found"] == true && tc.status["authenticated"] != true && !tc.relogin
			if hedged != wantHedged {
				t.Errorf("hedged = %v, want %v for %q — only a configured platform whose check "+
					"reached no conclusion may be worded that way", hedged, wantHedged, title)
			}
		})
	}
}

// TestCookieIndicatorReadsTheHandlersOwnPayload runs the JS against the exact
// map the Go handler emits, rather than against a hand-written imitation of it.
//
// This is the Go↔JS seam, and it is the one a table of literals cannot see: a
// rename on either side leaves the badge reading `undefined`, which is worse
// than a crash — `undefined === "unknown"` is false and `!undefined` is true,
// so a renamed `verification` silently reverts every install to the old red
// badge and a renamed `found` reports every platform as unconfigured.
//
// The payload comes from CookieStatusPayload / TwitchAuthStatusPayload, which
// is also what pins the three endpoints together: they all project through
// these two functions now, so a field that reaches one reaches all three.
func TestCookieIndicatorReadsTheHandlersOwnPayload(t *testing.T) {
	vm := utilsVM(t)

	for _, tc := range []struct {
		name     string
		status   cookies.AuthStatus
		wantYT   string
		wantTW   string
		ytHedged bool
		twHedged bool
	}{
		{
			name: "both alive",
			status: cookies.AuthStatus{
				YouTubeAuthenticated: true, TwitchAuthenticated: true,
				HasYouTubeCookies: true, HasTwitchCookies: true,
				YouTubeVerification: cookies.RefreshOK, TwitchVerification: cookies.RefreshOK,
			},
			wantYT: "indicator-ok", wantTW: "indicator-ok",
		},
		{
			name: "both conclusively rejected",
			status: cookies.AuthStatus{
				HasYouTubeCookies: true, HasTwitchCookies: true,
				YouTubeVerification: cookies.RefreshFailed, TwitchVerification: cookies.RefreshFailed,
			},
			wantYT: "indicator-error", wantTW: "indicator-error",
		},
		{
			// The state the whole arc exists for, carried end to end: the
			// service could not reach either site, the booleans are false on
			// both, and neither badge may say so.
			name: "neither site could be asked",
			status: cookies.AuthStatus{
				HasYouTubeCookies: true, HasTwitchCookies: true,
				YouTubeVerification: cookies.RefreshUnknown, TwitchVerification: cookies.RefreshUnknown,
			},
			wantYT: "indicator-warn", wantTW: "indicator-warn",
			ytHedged: true, twHedged: true,
		},
		{
			name: "nothing configured",
			status: cookies.AuthStatus{
				YouTubeVerification: cookies.RefreshFailed, TwitchVerification: cookies.RefreshFailed,
			},
			wantYT: "indicator-warn", wantTW: "indicator-off",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, side := range []struct {
				platform string
				payload  map[string]any
				want     string
				hedged   bool
			}{
				{"youtube", CookieStatusPayload(tc.status), tc.wantYT, tc.ytHedged},
				{"twitch", TwitchAuthStatusPayload(tc.status), tc.wantTW, tc.twHedged},
			} {
				className, title := indicatorState(t, vm, side.platform, side.payload, false)
				if className != side.want {
					t.Errorf("%s: className = %q, want %q from payload %v (title %q)",
						side.platform, className, side.want, side.payload, title)
				}
				if got := strings.Contains(title, "could not establish"); got != side.hedged {
					t.Errorf("%s: hedged = %v, want %v — the handler's own payload no longer "+
						"reaches the arm it was built for: %q", side.platform, got, side.hedged, title)
				}
			}
		})
	}
}

// TestDashboardBadgeDelegatesToTheTestableHelper pins the one thing execution
// cannot: that app.js still calls the helper, with the payloads, instead of
// wording the badge itself again.
//
// updateStatusBar is DOM-coupled and cannot be loaded into goja, so this is a
// source-shape assertion — the category the sibling file keeps deliberately.
// It is bracketed to the method AND asserts the absence of the thing that
// would replace it: bracketing alone is not enough when the decoy can sit
// inside the bracket, and an inlined chain necessarily reintroduces the
// indicator class literals.
func TestDashboardBadgeDelegatesToTheTestableHelper(t *testing.T) {
	app := readEmbeddedModule(t, "public/app.js")

	if !strings.Contains(app, "cookieIndicatorState") {
		t.Fatal("app.js no longer imports cookieIndicatorState")
	}

	body := jsMethodBody(t, app, "updateStatusBar")

	args := jsCallArgs(body, "cookieIndicatorState")
	if len(args) != 3 {
		t.Fatalf("updateStatusBar calls cookieIndicatorState with %d arguments (%v), want 3 "+
			"(platform, status, reloginRequired)", len(args), args)
	}
	for _, want := range []string{"this.cookieStatus", "this.twitchAuthStatus"} {
		if !strings.Contains(body, want) {
			t.Errorf("updateStatusBar no longer feeds %s to the badge — the indicator would "+
				"render from nothing", want)
		}
	}
	if strings.Contains(body, "indicator-") {
		t.Error("updateStatusBar words an indicator class itself again. That is where the two " +
			"platform chains drifted apart in the first place, and it is the half no test can " +
			"execute — the decision belongs in cookieIndicatorState")
	}
}

// jsMethodBody brackets one CLASS METHOD out of app.js.
//
// jsFunctionBody in the sibling file handles module-level `function` and
// `export function` declarations; a class method has neither keyword. Same
// purpose, same limitation: it narrows the window for a source-shape
// assertion, and it is not a substitute for asserting on structure or
// behaviour inside that window.
func jsMethodBody(t *testing.T, js, name string) string {
	t.Helper()
	header := "\n  " + name + "() {"
	at := strings.Index(js, header)
	if at < 0 {
		t.Fatalf("app.js no longer defines a %s() method", name)
	}
	body := js[at+len(header):]
	if end := strings.Index(body, "\n  }\n"); end >= 0 {
		body = body[:end]
	}
	return body
}
