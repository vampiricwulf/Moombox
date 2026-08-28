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

// jsMethodBody brackets one CLASS METHOD out of a module.
//
// jsFunctionBody in the sibling file handles module-level `function` and
// `export function` declarations; a class method has neither keyword. Same
// purpose, same limitation: it narrows the window for a source-shape
// assertion, and it is not a substitute for asserting on structure or
// behaviour inside that window.
func jsMethodBody(t *testing.T, js, name string) string {
	t.Helper()
	var header string
	at := -1
	for _, candidate := range []string{
		"\n  " + name + "() {",
		"\n  async " + name + "() {",
	} {
		if i := strings.Index(js, candidate); i >= 0 {
			header, at = candidate, i
			break
		}
	}
	if at < 0 {
		t.Fatalf("no %s() method found — the module was restructured and this assertion is "+
			"reading nothing", name)
	}
	body := js[at+len(header):]
	if end := strings.Index(body, "\n  }\n"); end >= 0 {
		body = body[:end]
	}
	return body
}

// TestReloginWarningIsNotGatedOnAutoCookies is V6.
//
// The dashboard computed `autoCookiesEnabled` from cookies.auto_enabled and
// conjoined it into all three places the re-login state is surfaced: both
// status-bar warning items and the third argument to cookieIndicatorState. The
// TUI (cmd/moombox/tui_wiring.go) applies the same flag unconditionally, so the
// two UIs disagreed about whether a true alarm is worth showing.
//
// The flag means "the session Moombox holds has been rejected; a human must
// sign in again". That is exactly as true, and as actionable, for a
// manual-cookie install — they do theirs by hand — and it is reachable for
// them: POST /api/cookies/auto-refresh is not gated on AutoEnabled. Gating hid
// the alarm from the users least able to find it another way. The same ruling
// was already made for the auth-loss notification.
//
// updateStatusBar is DOM-coupled and cannot be executed, so this is a bracketed
// source assertion — but it is written as an ABSENCE plus an exact rendering of
// each condition, because a presence check cannot see a gate and a bracket
// alone cannot see a gate that sits inside the bracket. Any reintroduced gate
// has to name the config key `auto_enabled` somewhere in this method, whatever
// local it is bound to.
func TestReloginWarningIsNotGatedOnAutoCookies(t *testing.T) {
	body := jsMethodBody(t, readEmbeddedModule(t, "public/app.js"), "updateStatusBar")
	// The absence check runs against CODE. The method carries a comment naming
	// the flag and saying why it is not read, and that comment is the durable
	// half of the fix — a check that forbade the token outright would forbid
	// explaining it.
	code := jsWithoutLineComments(body)

	for _, gate := range []string{"auto_enabled", "autoCookiesEnabled"} {
		if strings.Contains(code, gate) {
			t.Errorf("updateStatusBar reads %s again. The re-login indicator must not be gated on "+
				"auto-cookies: a manual-cookie install has to re-login by hand, which makes them the "+
				"audience that most needs to be told — and the TUI shows it to them either way", gate)
		}
	}

	// The exact condition, rendered. Each warning turns on the platform being
	// active and the platform's own re-login flag, and on nothing else.
	for _, want := range []string{
		"if (ytActive && this.autoCookieReloginRequired?.youtube)",
		"if (twActive && this.autoCookieReloginRequired?.twitch)",
	} {
		if !strings.Contains(code, want) {
			t.Errorf("the status-bar warning is no longer exactly %q — either a condition was added "+
				"to it or the warning was dropped; both change who is told to re-login", want)
		}
	}
	for _, action := range []string{"yt-relogin", "tw-relogin"} {
		if !strings.Contains(code, action) {
			t.Errorf("updateStatusBar no longer pushes the %s warning at all — removing the gate by "+
				"removing the indicator is not the fix", action)
		}
	}

	// The badge's third argument is the same decision one layer down, and it
	// carried the same conjunction. It gets the SAME exact-rendering treatment
	// as the two warning conditions, and that is not belt-and-braces: a
	// containment check ("does the expression still mention
	// autoCookieReloginRequired?") passes for
	//
	//     const relogin = !!(this.autoCookieReloginRequired?.[platform] && this._autoOn);
	//
	// which re-gates the badge on a local the absence check above cannot name,
	// while leaving both warning pushes ungated. The shipped result is the
	// status bar saying "YT: Re-login" in text beside a Re-login badge that
	// never lights — one surface disagreeing with itself about one fact, which
	// is this arc's defect at its smallest. A name-based or substring check is
	// not a guard when the decoy can rename or extend; the exact rendering is.
	const wantRelogin = "const relogin = !!this.autoCookieReloginRequired?.[platform];"
	if !strings.Contains(code, wantRelogin) {
		t.Errorf("the badge's re-login flag is no longer exactly %q (line is %q) — anything else "+
			"conditions the red badge on something the two warning pushes are not conditioned on, "+
			"and the two halves of the status bar stop agreeing",
			wantRelogin, strings.TrimSpace(jsLineContaining(code, "const relogin")))
	}

	// And it must still be what reaches the helper. Taken off the PARSED call,
	// so a mention of the flag in a neighbouring comment cannot stand in for
	// passing it, and so an exactly-rendered local that is then ignored fails.
	args := jsCallArgs(code, "cookieIndicatorState")
	if len(args) != 3 {
		t.Fatalf("updateStatusBar calls cookieIndicatorState with %d arguments (%v), want 3", len(args), args)
	}
	if args[2] != "relogin" {
		t.Errorf("the badge's third argument is %q, not the `relogin` local the assertion above "+
			"pins — the exactly-rendered flag is being computed and then not used", args[2])
	}
}

// jsLineContaining returns the first line of src that contains needle, or "".
// Used to look at the one statement an assertion is about without bracketing a
// whole method around it.
func jsLineContaining(src, needle string) string {
	for _, line := range strings.Split(src, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

// jsWithoutLineComments drops whole-line `//` comments.
//
// Absence assertions are about what the code DOES, and a comment explaining why
// something is not done necessarily names the thing. Deliberately naive: it
// removes only lines whose first non-space characters are `//`, so a trailing
// comment on a code line still counts as code — which is the safe direction,
// since that is where a decoy would hide.
func jsWithoutLineComments(src string) string {
	lines := strings.Split(src, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
