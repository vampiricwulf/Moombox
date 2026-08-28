package main

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/tui"
)

// TestCookieBadgeForSeparatesRejectedFromUnchecked pins the projection the TUI
// status bar renders, for BOTH platforms off one table.
//
// Two defects meet here and the rows are named for them:
//
//   - the unknown rows. An inconclusive check produced authenticated=false and
//     fell into CookieStatusCookiesOnly, the red always-visible alert, so a 503
//     from YouTube looked exactly like a dead session for as long as it lasted.
//   - the Twitch rows. Before AuthStatus carried HasTwitchCookies, the wiring
//     could only ever assign CookieStatusOK for Twitch, which left
//     status_bar.go's Twitch CookiesOnly arm unreachable — a Twitch session
//     whose auth-token had been pruned on expiry rendered identically to one
//     that was never set up.
//
// The table is deliberately shared rather than split per platform: the two arms
// had already diverged once, and one table is what stops them diverging again.
func TestCookieBadgeForSeparatesRejectedFromUnchecked(t *testing.T) {
	for _, tc := range []struct {
		name          string
		authenticated bool
		hasCookies    bool
		verdict       cookies.RefreshVerdict
		want          tui.CookieStatus
	}{
		{
			name: "signed in", authenticated: true, hasCookies: true,
			verdict: cookies.RefreshOK, want: tui.CookieStatusOK,
		},
		{
			// Conclusive: the site was asked and it said no.
			name: "credentials rejected", hasCookies: true,
			verdict: cookies.RefreshFailed, want: tui.CookieStatusCookiesOnly,
		},
		{
			// THE FIX. Same booleans as the row above, different meaning.
			name: "check could not conclude", hasCookies: true,
			verdict: cookies.RefreshUnknown, want: tui.CookieStatusUnknown,
		},
		{
			// Never configured outranks the verdict: the check returns a
			// conclusive "not authenticated" for a platform with no cookies,
			// and reporting that as rejected credentials would invent a
			// sign-in that never happened.
			name: "never configured, conclusively", verdict: cookies.RefreshFailed,
			want: tui.CookieStatusNone,
		},
		{
			name:    "never configured, and the check was inconclusive too",
			verdict: cookies.RefreshUnknown, want: tui.CookieStatusNone,
		},
		{
			// Defensive: the zero RefreshVerdict is RefreshUnknown, so a
			// caller that forgets to populate the field must not be able to
			// produce a red alert.
			name: "verdict left unset", hasCookies: true, want: tui.CookieStatusUnknown,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := cookieBadgeFor(tc.authenticated, tc.hasCookies, tc.verdict)
			if got != tc.want {
				t.Errorf("cookieBadgeFor(auth=%v, cookies=%v, %v) = %v, want %v",
					tc.authenticated, tc.hasCookies, tc.verdict, got, tc.want)
			}
			// THE INVARIANT, asserted on every row rather than only on the
			// unknown ones: CookiesOnly is the red badge that survives every
			// width tier, so nothing but a conclusive RefreshFailed may reach
			// it. A row added later cannot quietly start alarming.
			if got == tui.CookieStatusCookiesOnly && tc.verdict != cookies.RefreshFailed {
				t.Errorf("verdict %v produced the always-visible red alert. Only a check that "+
					"reached the site and was told no may do that", tc.verdict)
			}
		})
	}
}
