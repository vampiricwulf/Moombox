package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// AutoCookieStatus.LastError reaching the TUI, which had no reader for it at
// all.
//
// The field is the last thing a cookie pass — a browser refresh or an
// interactive setup — concluded that the operator has to act on. Both
// dashboards were supposed to show it beside the cookie status; the Web showed
// it only inside the setup dialog's abort report and the TUI showed it nowhere,
// so a browser refresh that had been failing for days was invisible to an
// operator whose cookies.txt the in-process session refresh was still keeping
// alive.
//
// R C is the surface, and deliberately: it is the key an operator presses to
// ask what their cookies are doing, and it is a PER-REQUEST path. The status bar
// is not — it is fed by RefreshService.OnAuthChange pushes, whose change gate
// (refresh.go's authStatusChanged) does not watch every field, so a panel
// rendering off it can hold a value across a change that produced no push. See
// the note on renderCookieStatus.

const recordedCookieError = "the browser profile contained no cookies — sign in and run setup again"

// TestRecheckLineCarriesTheRecordedCookieError is the render, asserted as the
// EXACT line rather than as a substring.
//
// The important row is the first: both verdicts conclusive and healthy, and a
// recorded error beside them. That combination is not a contradiction — it is
// the whole reason the field is worth showing — and it is the one a renderer
// that gated the clause on a bad verdict would silently drop.
func TestRecheckLineCarriesTheRecordedCookieError(t *testing.T) {
	t.Run("shown beside a healthy verdict", func(t *testing.T) {
		got := recheckFeedback(t, 200, true, false, cookieRecheckResultMsg{
			YouTube:   cookies.RefreshOK,
			LastError: recordedCookieError,
		})
		want := cookies.RecheckReport(
			cookies.RecheckedPlatform{Label: "YouTube", Verdict: cookies.RefreshOK},
		) + " | Last cookie error: " + recordedCookieError
		if got != want {
			t.Errorf("R C feedback = %q, want %q.\n\nA working cookies.txt and a broken refresh "+
				"mechanism coexist routinely — the session refresh keeps the file alive — and this "+
				"is the only place in the TUI that can say the second half", got, want)
		}
	})

	t.Run("appended after the reasons, not before them", func(t *testing.T) {
		const reason = "youtube auth check: unexpected status 429"
		got := recheckFeedback(t, 400, true, false, cookieRecheckResultMsg{
			YouTube:       cookies.RefreshUnknown,
			YouTubeReason: reason,
			LastError:     recordedCookieError,
		})
		lead := cookies.RecheckReport(
			cookies.RecheckedPlatform{Label: "YouTube", Verdict: cookies.RefreshUnknown},
		)
		want := lead + " (YouTube: " + reason + ") | Last cookie error: " + recordedCookieError
		if got != want {
			t.Errorf("R C feedback = %q, want %q", got, want)
		}
		// The ordering claim, stated independently of the exact string: the
		// clamp eats the tail first, so the least important fact has to BE the
		// tail. Reverse the two and a narrow terminal loses the verdict's reason
		// to keep a message about a different service.
		if strings.Index(got, "Last cookie error") < strings.Index(got, reason) {
			t.Errorf("the recorded error precedes the check's own reason: %q. On a narrow terminal "+
				"the clamp would drop the reason and keep this", got)
		}
	})

	t.Run("nothing recorded leaves the line byte-identical", func(t *testing.T) {
		// The both-UIs pin restated: an install with nothing recorded, and every
		// wiring before this change, must render exactly what it rendered before.
		got := recheckFeedback(t, 200, true, true, cookieRecheckResultMsg{
			YouTube: cookies.RefreshUnknown,
			Twitch:  cookies.RefreshOK,
		})
		want := cookies.RecheckReport(
			cookies.RecheckedPlatform{Label: "YouTube", Verdict: cookies.RefreshUnknown},
			cookies.RecheckedPlatform{Label: "Twitch", Verdict: cookies.RefreshOK},
		)
		if got != want {
			t.Errorf("feedback = %q, want %q — an empty LastError must add nothing at all, not an "+
				"empty clause", got, want)
		}
	})

	t.Run("still fits the panel", func(t *testing.T) {
		// Same trap the reason strings brought: addOverlayMessage pads rather
		// than clips, so an over-long line wraps and shifts every row of the
		// frame below it for three seconds. LastError's length is decided by
		// whichever cookie pass wrote it, which is exactly as unbounded.
		huge := "restore failed: " + strings.Repeat("C:/very/long/path/segment/", 40) + "cookies.txt"
		for _, width := range []int{40, 80, 120} {
			got := recheckFeedback(t, width, true, false, cookieRecheckResultMsg{
				YouTube:   cookies.RefreshOK,
				LastError: huge,
			})
			if w := lipgloss.Width(got); w > width-2 {
				t.Errorf("at width %d the feedback is %d columns wide: %q", width, w, got)
			}
		}
	})
}

// TestRecheckCommandAsksForTheRecordedError covers the other half — the R C
// command actually calling the callback — because the renderer above is fed a
// message the test builds itself and would pass on a chord that never asked.
//
// The nil case is the additive contract at this seam: an App with no
// auto-cookie wiring (every test App, and any future embedding without the
// service) must produce the message it produced before, not a panic and not an
// empty clause.
func TestRecheckCommandAsksForTheRecordedError(t *testing.T) {
	runRecheck := func(t *testing.T, wire func(a *App)) cookieRecheckResultMsg {
		t.Helper()
		app := NewApp()
		app.OnRecheckCookies = func() (cookies.RefreshVerdict, cookies.RefreshVerdict, string, string) {
			return cookies.RefreshOK, cookies.RefreshOK, "", ""
		}
		wire(app)
		cmd := app.recheckCookiesCmd()
		if cmd == nil {
			t.Fatal("recheckCookiesCmd returned nil with OnRecheckCookies wired")
		}
		msg, ok := cmd().(cookieRecheckResultMsg)
		if !ok {
			t.Fatalf("R C produced %T, want cookieRecheckResultMsg", cmd())
		}
		return msg
	}

	t.Run("wired", func(t *testing.T) {
		asked := 0
		msg := runRecheck(t, func(a *App) {
			a.OnAutoCookieLastError = func() string {
				asked++
				return recordedCookieError
			}
		})
		if asked != 1 {
			t.Errorf("OnAutoCookieLastError was called %d times, want 1 — R C is the gesture that "+
				"asks, and a renderer fed by nothing renders nothing", asked)
		}
		if msg.LastError != recordedCookieError {
			t.Errorf("LastError = %q, want %q", msg.LastError, recordedCookieError)
		}
	})

	t.Run("not wired", func(t *testing.T) {
		msg := runRecheck(t, func(*App) {})
		if msg.LastError != "" {
			t.Errorf("LastError = %q with no callback wired, want empty", msg.LastError)
		}
	})
}
