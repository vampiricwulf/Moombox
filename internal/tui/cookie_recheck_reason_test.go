package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// R C's answer used to be a verdict and nothing else, and "could not establish"
// is the same three words for a rate limit, a captive portal and an
// intercepting proxy — only one of which is worth waiting out.
// cookies.AuthStatus already carried the reason in YouTubeError / TwitchError
// and nothing anywhere read them; these tests are that field's TUI reader.
//
// The shared sentence is NOT touched by any of this. cookies.RecheckReport is
// what the Web dashboard renders for the same gesture and both copies are
// pinned against it, so the reason is APPENDED after it — a suffix cannot
// change the string those pins compare, and a check that concluded produces no
// suffix at all.

// recheckFeedback drives one result through the real Update path and returns
// what the operator would read.
func recheckFeedback(t *testing.T, width int, ytActive, twActive bool, msg cookieRecheckResultMsg) string {
	t.Helper()
	app := NewApp()
	app.width = width
	app.statusBar.SetActivePlatforms(ytActive, twActive)
	app.Update(msg)
	return app.feedbackMsg
}

func TestRecheckFeedbackNamesWhyACheckCouldNotConclude(t *testing.T) {
	const ytReason = "youtube auth check: unexpected status 429"
	const twReason = "twitch auth check: unexpected status 503"

	t.Run("one inconclusive platform names its reason", func(t *testing.T) {
		got := recheckFeedback(t, 200, true, false, cookieRecheckResultMsg{
			YouTube:       cookies.RefreshUnknown,
			YouTubeReason: ytReason,
		})
		want := cookies.RecheckReport(
			cookies.RecheckedPlatform{Label: "YouTube", Verdict: cookies.RefreshUnknown},
		) + " (YouTube: " + ytReason + ")"
		if got != want {
			t.Errorf("R C feedback = %q, want %q", got, want)
		}
		// The shared sentence has to survive INTACT as the lead. If the reason
		// were woven into it, the Web dashboard — which renders that sentence
		// from its own copy of RecheckReport — would answer the same gesture
		// with a different sentence again.
		lead := cookies.RecheckReport(
			cookies.RecheckedPlatform{Label: "YouTube", Verdict: cookies.RefreshUnknown},
		)
		if !strings.HasPrefix(got, lead) {
			t.Errorf("the shared sentence was reworded: %q does not start with %q", got, lead)
		}
	})

	t.Run("both inconclusive, and they can disagree", func(t *testing.T) {
		got := recheckFeedback(t, 200, true, true, cookieRecheckResultMsg{
			YouTube: cookies.RefreshUnknown, YouTubeReason: ytReason,
			Twitch: cookies.RefreshUnknown, TwitchReason: twReason,
		})
		// Both named, and each against its own platform. One hedge for two
		// different causes is the misattribution the cookies package already
		// refuses elsewhere (see combinedInconclusiveHedge).
		if !strings.Contains(got, "YouTube: "+ytReason) {
			t.Errorf("YouTube's reason is missing: %q", got)
		}
		if !strings.Contains(got, "Twitch: "+twReason) {
			t.Errorf("Twitch's reason is missing: %q", got)
		}
	})

	t.Run("an OK verdict renders the shared sentence and nothing else", func(t *testing.T) {
		// Unchanged intent, and a RENDERER pin only: the message is built by
		// hand here, so nothing about the producer is exercised — it asserts
		// that an empty reason adds no parenthetical, which is what keeps the
		// widening additive for every install whose cookies are fine.
		//
		// The producer half — that verdictFromCheck cannot return OK beside a
		// non-empty reason, because OK requires a nil error and the reason
		// string IS that error — is what makes this row sufficient rather than
		// a hole. It is a property of internal/cookies and is pinned there.
		got := recheckFeedback(t, 200, true, false, cookieRecheckResultMsg{
			YouTube:       cookies.RefreshOK,
			YouTubeReason: "",
		})
		want := cookies.RecheckReport(
			cookies.RecheckedPlatform{Label: "YouTube", Verdict: cookies.RefreshOK},
		)
		if got != want {
			t.Errorf("feedback = %q, want the shared sentence alone %q", got, want)
		}
	})

	t.Run("a conclusive REFUSAL names its reason", func(t *testing.T) {
		// Arc 10 reversed this row. It rested on "a conclusive verdict has no
		// cause to give", which was already false: verdictFromCheck maps the
		// unsignable-jar sentinel to RefreshFailed with the error recorded, so
		// the old gate had been swallowing that cause since Arc 8. The mark is
		// the second such producer, and its four fixed sentences are the only
		// thing that says WHICH route broke — "the cookie file has a Twitch
		// auth-token but no login cookie beside it" versus "Twitch refused the
		// saved login" are different remedies. Withholding either left the
		// operator with "not authenticated" and no next step.
		//
		// THE MUTATION: narrowing the gate back to
		// `verdict == cookies.RefreshUnknown && reason != ""` in
		// cookieRecheckFeedback (app_update.go). This subtest then fails on the
		// Contains check below — the sentence comes back as the bare
		// RecheckReport with no parenthetical.
		const twReasonMark = "The cookie file has a Twitch auth-token but no login cookie beside it."
		got := recheckFeedback(t, 200, false, true, cookieRecheckResultMsg{
			Twitch:       cookies.RefreshFailed,
			TwitchReason: twReasonMark,
		})
		if !strings.Contains(got, twReasonMark) {
			t.Errorf("feedback = %q, want it to name %q — a conclusive refusal that knows WHY must say so", got, twReasonMark)
		}
		if !strings.Contains(got, "Twitch") {
			t.Errorf("feedback = %q, want the platform label kept beside the reason", got)
		}
	})

	t.Run("no reason supplied leaves the sentence byte-identical", func(t *testing.T) {
		// The both-UIs pin, restated at this site: a wiring that supplies no
		// reason — and every wiring did until this change — must render exactly
		// what it rendered before.
		got := recheckFeedback(t, 200, true, true, cookieRecheckResultMsg{
			YouTube: cookies.RefreshUnknown,
			Twitch:  cookies.RefreshOK,
		})
		want := cookies.RecheckReport(
			cookies.RecheckedPlatform{Label: "YouTube", Verdict: cookies.RefreshUnknown},
			cookies.RecheckedPlatform{Label: "Twitch", Verdict: cookies.RefreshOK},
		)
		if got != want {
			t.Errorf("feedback = %q, want %q", got, want)
		}
	})

	t.Run("an inactive platform contributes no reason", func(t *testing.T) {
		// Same rule the verdicts already follow: only platforms this install
		// monitors are reported on. A reason for a platform the operator does
		// not use is noise about something they cannot act on.
		got := recheckFeedback(t, 200, true, false, cookieRecheckResultMsg{
			YouTube: cookies.RefreshOK,
			Twitch:  cookies.RefreshUnknown, TwitchReason: twReason,
		})
		if strings.Contains(got, "Twitch") {
			t.Errorf("feedback = %q — Twitch is not an active platform here", got)
		}
	})
}

// TestRecheckFeedbackFitsThePanel is the trap this change had to clear.
//
// addOverlayMessage writes the feedback into a FIXED ROW of an already-composed
// frame, as "  "+msg padded out to the width — it does not clip. A line wider
// than the terminal therefore wraps, and the wrap pushes every row below it
// down: the whole dashboard shifts for three seconds.
//
// Nothing needed this before, because everything else reaching setFeedback is
// composed from bounded vocabulary. The reason is the first string whose length
// is decided elsewhere — a resolver's wording, a proxy's host name — so it is
// the first one that can do this.
func TestRecheckFeedbackFitsThePanel(t *testing.T) {
	huge := "resolve " + strings.Repeat("very-long-hostname-segment.", 40) + "example: no such host"

	for _, width := range []int{40, 80, 120} {
		got := recheckFeedback(t, width, true, false, cookieRecheckResultMsg{
			YouTube:       cookies.RefreshUnknown,
			YouTubeReason: huge,
		})
		// -2 for addOverlayMessage's leading indent, which is what the line has
		// to fit INSIDE.
		if w := lipgloss.Width(got); w > width-2 {
			t.Errorf("at width %d the feedback is %d columns wide: %q\n\n"+
				"addOverlayMessage pads rather than clips, so this wraps and shifts every row "+
				"of the frame below it", width, w, got)
		}
	}

	// Below the first WindowSizeMsg there is no width to clamp to, and no frame
	// to break either. Clamping to a width nobody has reported would cut every
	// message to nothing.
	got := recheckFeedback(t, 0, true, false, cookieRecheckResultMsg{
		YouTube:       cookies.RefreshUnknown,
		YouTubeReason: huge,
	})
	if !strings.Contains(got, huge) {
		t.Errorf("with no width reported the line was truncated anyway: %q", got)
	}
}
