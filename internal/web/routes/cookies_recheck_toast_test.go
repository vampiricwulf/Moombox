package routes

import (
	"strings"
	"testing"

	"github.com/dop251/goja"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// The Web dashboard's refresh button and the TUI's R C chord are THE SAME
// GESTURE. This toast answered it with "Cookies refreshed successfully" or
// "Cookie check completed", keyed on `data.success` — which is
// `youtubeAuthenticated || twitchAuthenticated`, and therefore false for a
// check that never reached the site. It named neither the platform nor the
// finding, in the arc that taught every other surface to say exactly that.
//
// The sentence is now rendered by cookies.RecheckReport in Go and reproduced
// in utils.js, and the tests below compare the two by EXACT EQUALITY rather
// than by matching literals on either side. That is the only assertion shape
// that holds the property actually wanted — "the two UIs answer the same
// question the same way" — because a reword on either side alone fails it,
// while two independent literal tables would both stay green.

// recheckToast runs cookieRecheckToast and unpacks {message, variant}.
func recheckToast(t *testing.T, vm *goja.Runtime, active map[string]any, yt, tw map[string]any, success bool) (message, variant string) {
	t.Helper()
	raw := jsCall(t, vm, "cookieRecheckToast", active, yt, tw, success)
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("cookieRecheckToast returned %T, want the {message, variant} object", raw)
	}
	message, _ = m["message"].(string)
	variant, _ = m["variant"].(string)
	if message == "" || variant == "" {
		t.Fatalf("toast is missing message or variant: %v", m)
	}
	return message, variant
}

func activePlatforms(yt, tw bool) map[string]any {
	return map[string]any{"youtube": yt, "twitch": tw}
}

// TestRecheckToastRendersTheSameSentenceAsTheTUI is the cross-UI parity
// assertion, and it is built the way
// TestCookieIndicatorReadsTheHandlersOwnPayload is: the JS is fed the ACTUAL
// maps the Go handler emits, and the expectation is COMPUTED by the Go
// renderer the TUI uses rather than written out here.
//
// Both halves of that matter. Hand-written expectations would drift from
// RecheckReport the moment either was edited; hand-written payloads would let
// a wire-key rename pass, and a renamed `verification` degrades silently to
// the legacy copy — the very sentence this replaced.
//
// The rows deliberately mix verdicts across the two platforms so a bug that
// reads one platform's status for the other produces a different sentence
// instead of the same one twice.
func TestRecheckToastRendersTheSameSentenceAsTheTUI(t *testing.T) {
	vm := utilsVM(t)

	for _, tc := range []struct {
		name     string
		ytActive bool
		twActive bool
		status   cookies.AuthStatus
	}{
		{
			name: "both configured, both alive", ytActive: true, twActive: true,
			status: cookies.AuthStatus{
				YouTubeAuthenticated: true, TwitchAuthenticated: true,
				HasYouTubeCookies: true, HasTwitchCookies: true,
				YouTubeVerification: cookies.RefreshOK, TwitchVerification: cookies.RefreshOK,
			},
		},
		{
			name:     "both configured, YouTube rejected and Twitch unreachable",
			ytActive: true, twActive: true,
			status: cookies.AuthStatus{
				HasYouTubeCookies: true, HasTwitchCookies: true,
				YouTubeVerification: cookies.RefreshFailed, TwitchVerification: cookies.RefreshUnknown,
			},
		},
		{
			// The state the whole arc exists for, on the surface that was last
			// to learn it.
			name: "neither site could be asked", ytActive: true, twActive: true,
			status: cookies.AuthStatus{
				HasYouTubeCookies: true, HasTwitchCookies: true,
				YouTubeVerification: cookies.RefreshUnknown, TwitchVerification: cookies.RefreshUnknown,
			},
		},
		{
			// Gating. An operator running YouTube only must not be told
			// anything about a Twitch session they never configured — and the
			// Twitch verdict below is deliberately the LOUD one, so a gating
			// bug shows up as a sentence that mentions it.
			name: "youtube only", ytActive: true,
			status: cookies.AuthStatus{
				HasYouTubeCookies:   true,
				YouTubeVerification: cookies.RefreshOK, TwitchVerification: cookies.RefreshFailed,
			},
		},
		{
			name: "twitch only", twActive: true,
			status: cookies.AuthStatus{
				HasTwitchCookies:    true,
				YouTubeVerification: cookies.RefreshFailed, TwitchVerification: cookies.RefreshOK,
			},
		},
		{
			name:   "nothing configured",
			status: cookies.AuthStatus{YouTubeVerification: cookies.RefreshFailed, TwitchVerification: cookies.RefreshFailed},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var expected []cookies.RecheckedPlatform
			if tc.ytActive {
				expected = append(expected, cookies.RecheckedPlatform{
					Label: "YouTube", Verdict: tc.status.YouTubeVerification,
				})
			}
			if tc.twActive {
				expected = append(expected, cookies.RecheckedPlatform{
					Label: "Twitch", Verdict: tc.status.TwitchVerification,
				})
			}
			want := cookies.RecheckReport(expected...)

			message, _ := recheckToast(t, vm,
				activePlatforms(tc.ytActive, tc.twActive),
				CookieStatusPayload(tc.status),
				TwitchAuthStatusPayload(tc.status),
				tc.status.YouTubeAuthenticated || tc.status.TwitchAuthenticated)

			if message != want {
				t.Errorf("toast = %q\n want %q\nThe TUI renders the second one for the same "+
					"gesture. One sentence, two UIs — that is what cookies.RecheckReport is "+
					"exported for", message, want)
			}
		})
	}

	// THE PREMISE. If RecheckReport rendered the same string regardless of
	// verdict, every equality above would hold while proving nothing about
	// what a user is told.
	unknown := cookies.RecheckReport(cookies.RecheckedPlatform{Label: "YouTube", Verdict: cookies.RefreshUnknown})
	failed := cookies.RecheckReport(cookies.RecheckedPlatform{Label: "YouTube", Verdict: cookies.RefreshFailed})
	if unknown == failed {
		t.Fatalf("premise lost: an inconclusive check and a conclusive rejection render "+
			"identically (%q), so the equality checks above distinguish nothing", unknown)
	}
}

// TestRecheckToastSeverityFollowsTheVerdict covers the half that is web-only:
// Shoelace variants have no TUI counterpart, so they cannot be asserted by
// comparison and need their own table.
//
// The ranking is the point. A conclusive failure is the thing to act on, so it
// outranks a check that concluded nothing; and the un-concluded case must not
// be green, because "we could not find out" is not reassurance. The invariant
// at the bottom is the guard that survives a fourth arm: `danger` appears if
// and only if some platform was conclusively rejected.
func TestRecheckToastSeverityFollowsTheVerdict(t *testing.T) {
	vm := utilsVM(t)

	verdictStatus := func(v string) map[string]any {
		return map[string]any{"found": true, "authenticated": v == "ok", "verification": v}
	}

	for _, tc := range []struct {
		name   string
		yt, tw string
		want   string
	}{
		{"both alive", "ok", "ok", "success"},
		{"one rejected", "ok", "failed", "danger"},
		{"one unreachable", "ok", "unknown", "warning"},
		{"both unreachable", "unknown", "unknown", "warning"},
		{
			// A conclusive failure beside an inconclusive one is still a
			// conclusive failure, and it is the half the operator can act on.
			"rejected outranks unreachable", "unknown", "failed", "danger",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message, variant := recheckToast(t, vm, activePlatforms(true, true),
				verdictStatus(tc.yt), verdictStatus(tc.tw), tc.yt == "ok" || tc.tw == "ok")
			if variant != tc.want {
				t.Errorf("variant = %q, want %q: %q", variant, tc.want, message)
			}

			// THE INVARIANT, on every row: only a conclusive rejection earns
			// the alarm colour, exactly as only it earns the word.
			conclusive := tc.yt == "failed" || tc.tw == "failed"
			if got := variant == "danger"; got != conclusive {
				t.Errorf("danger = %v, want %v — the colour must follow the verdict for the "+
					"same reason the wording does: %q", got, conclusive, message)
			}
		})
	}
}

// TestRecheckToastDegradesToTheUnqualifiedCopy is the additive contract, and
// the direction it degrades in is the whole assertion.
//
// An older binary emits no `verification` key at all. The rule this arc set —
// in cookieSetupAcceptedToast, then cookieIndicatorState — is that a missing
// field degrades to the UNQUALIFIED copy those users already see, never to the
// hedged one: hedging about every recheck on every older build would replace
// one wrong answer with another. Written with a negative comparison
// (`!== "ok"`) instead of the switch's positive cases, every one of these rows
// would produce "could not establish".
//
// The missing-activePlatforms row is a different absence with the same rule.
// An absent map means we do not know which platforms are configured; only an
// EMPTY one means none are, and only that may be reported as such.
func TestRecheckToastDegradesToTheUnqualifiedCopy(t *testing.T) {
	vm := utilsVM(t)

	legacyStatus := map[string]any{"found": true, "authenticated": false}
	modern := map[string]any{"found": true, "authenticated": true, "verification": "ok"}

	for _, tc := range []struct {
		name        string
		active      map[string]any
		yt, tw      map[string]any
		success     bool
		wantMessage string
		wantVariant string
	}{
		{
			name:   "older binary, nothing authenticated",
			active: activePlatforms(true, true), yt: legacyStatus, tw: legacyStatus,
			wantMessage: "Cookie check completed", wantVariant: "primary",
		},
		{
			name:   "older binary, something authenticated",
			active: activePlatforms(true, true), yt: legacyStatus, tw: legacyStatus, success: true,
			wantMessage: "Cookies refreshed successfully", wantVariant: "success",
		},
		{
			// One modern payload does not license wording the other. The
			// fallback is per-response, not per-platform.
			name:   "one platform on the old shape",
			active: activePlatforms(true, true), yt: modern, tw: legacyStatus, success: true,
			wantMessage: "Cookies refreshed successfully", wantVariant: "success",
		},
		{
			name:   "the active-platform map never arrived",
			active: nil, yt: modern, tw: modern, success: true,
			wantMessage: "Cookies refreshed successfully", wantVariant: "success",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message, variant := recheckToast(t, vm, tc.active, tc.yt, tc.tw, tc.success)
			if message != tc.wantMessage {
				t.Errorf("toast = %q, want the legacy %q — a missing field must degrade to the "+
					"copy those users already see, not to the hedged one", message, tc.wantMessage)
			}
			if variant != tc.wantVariant {
				t.Errorf("variant = %q, want %q", variant, tc.wantVariant)
			}
			if strings.Contains(message, "could not establish") {
				t.Errorf("the legacy fallback hedges: %q. Written negatively, every recheck "+
					"against every older build would say this", message)
			}
		})
	}

	// The premise for the whole table: with `verification` present, the same
	// helper does NOT reach the legacy copy. Otherwise these rows would pass
	// against a function that had simply stopped reading the field.
	message, _ := recheckToast(t, vm, activePlatforms(true, false), modern, modern, true)
	if message == "Cookies refreshed successfully" {
		t.Fatal("premise lost: a payload WITH a verification field still renders the legacy " +
			"copy, so the fallback rows above are not testing a fallback")
	}
}

// TestRecheckToastIsWhereTheDashboardGetsItsWords pins the one thing execution
// cannot: that app.js still delegates.
//
// Bracketed to recheckCookies AND asserting the absence of the sentences that
// would replace it — bracketing alone is not enough when the decoy can sit
// inside the bracket, and re-inlining the old chain necessarily brings its two
// literals back with it. jsCallArgs reads the call's parsed arguments rather
// than matching text, so a comment mentioning the helper cannot satisfy it.
func TestRecheckToastIsWhereTheDashboardGetsItsWords(t *testing.T) {
	app := readEmbeddedModule(t, "public/app.js")

	if !strings.Contains(app, "cookieRecheckToast") {
		t.Fatal("app.js no longer imports cookieRecheckToast")
	}

	body := jsMethodBody(t, app, "recheckCookies")

	// jsCallArgs splits on top-level commas, so a trailing comma in a
	// multi-line call yields a final empty entry. That is a parser artifact,
	// not an argument — dropped here rather than in the shared helper, which
	// another assertion depends on unchanged.
	args := jsCallArgs(body, "cookieRecheckToast")
	if n := len(args); n > 0 && args[n-1] == "" {
		args = args[:n-1]
	}
	if len(args) != 4 {
		t.Fatalf("recheckCookies calls cookieRecheckToast with %d arguments (%v), want 4 "+
			"(activePlatforms, cookieStatus, twitchAuthStatus, success)", len(args), args)
	}
	for _, want := range []string{"this.activePlatforms", "data.cookieStatus", "data.twitchAuthStatus"} {
		if !strings.Contains(body, want) {
			t.Errorf("recheckCookies no longer feeds %s to the toast — it would word the answer "+
				"from nothing", want)
		}
	}
	for _, legacy := range []string{"Cookies refreshed successfully", "Cookie check completed"} {
		if strings.Contains(body, legacy) {
			t.Errorf("recheckCookies words the toast itself again (%q). That copy names neither "+
				"the platform nor the finding, and it is the half no test can execute — the "+
				"decision belongs in cookieRecheckToast", legacy)
		}
	}
}
