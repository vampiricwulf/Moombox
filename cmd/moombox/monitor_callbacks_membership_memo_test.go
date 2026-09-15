package main

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestMembershipConfirmedNonMemberRequiresARecognisedSession is the gate the
// feed monitor's 6 h memo hangs on.
//
// YouTube serves a signed-out session the SAME page it serves a genuine
// non-member: the channel Home fallback, no selected sponsorships tab, no
// error. hasAccess reads false on both. Memoizing on that bit alone would mark
// every configured channel a non-member the moment cookies expire, switching
// members-only discovery off across the whole install for six hours — and,
// because the memo outlives the repair, a fresh cookie import would not bring
// it back.
//
// THE MUTATION: `return !hasAccess`, dropping the verdict. Both the
// logged-out and the unknown rows then report a confirmed non-member.
func TestMembershipConfirmedNonMemberRequiresARecognisedSession(t *testing.T) {
	cases := []struct {
		name      string
		verdict   youtube.SessionAuthState
		hasAccess bool
		want      bool
		why       string
	}{
		{
			name:    "recognised session, no membership tab",
			verdict: youtube.SessionAuthLoggedIn, hasAccess: false, want: true,
			why: "the only answer that is actually an answer",
		},
		{
			name:    "recognised session, member",
			verdict: youtube.SessionAuthLoggedIn, hasAccess: true, want: false,
			why: "a member is never memoized — their tab is the only place a members-only live stream is listed",
		},
		{
			name:    "dead session gets the same home-fallback page",
			verdict: youtube.SessionAuthLoggedOut, hasAccess: false, want: false,
			why: "expired cookies must not read as 'not a member of anything'",
		},
		{
			name:    "unknown verdict answers nothing",
			verdict: youtube.SessionAuthUnknown, hasAccess: false, want: false,
			why: "a consent wall or a rate limit is not evidence about membership either",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := membershipConfirmedNonMember(tc.verdict, tc.hasAccess); got != tc.want {
				t.Errorf("membershipConfirmedNonMember(%q, %v) = %v, want %v — %s",
					tc.verdict, tc.hasAccess, got, tc.want, tc.why)
			}
		})
	}
}

// TestAuthRecoveryClearsTheMembershipMemo drives the registered callback.
//
// A YouTube repair has to drop the non-member memos, or the operator who just
// fixed their cookies keeps getting the suppressed behaviour for up to
// membershipMemoTTL on the one discovery path RSS cannot replace.
//
// THE MUTATION: dropping the clearYouTubeMembershipMemo call from the
// OnAuthRecovered closure — the first subtest then counts 0.
func TestAuthRecoveryClearsTheMembershipMemo(t *testing.T) {
	t.Run("youtube", func(t *testing.T) {
		s, _, memoClears := repairCallbackState(t)
		s.cookieRefresh.OnAuthRecovered("youtube")
		if *memoClears != 1 {
			t.Errorf("OnAuthRecovered(\"youtube\") cleared the membership memo %d times, want 1", *memoClears)
		}
	})

	// The memo is YouTube-only state. A Twitch repair says nothing about
	// whether a Google account is a member of anything, and clearing on it
	// would throw away a cycle of suppression for no reason.
	//
	// THE MUTATION: dropping the platform gate in clearYouTubeMembershipMemo.
	t.Run("twitch does not", func(t *testing.T) {
		s, _, memoClears := repairCallbackState(t)
		s.cookieRefresh.OnAuthRecovered("twitch")
		if *memoClears != 0 {
			t.Errorf("OnAuthRecovered(\"twitch\") cleared the YouTube membership memo %d times, want 0", *memoClears)
		}
	})
}

// TestClearMembershipMemoWithNoFeedMonitorIsSafe. wireCredentialRepairCallbacks
// runs during startup and a harness may build a runState without a feed
// monitor; the moment an operator repairs their credentials is the worst
// possible time for a nil deref. Mirrors TestBroadcastWithNoWorkerIsSafe.
//
// THE MUTATION: dropping the nil guard.
func TestClearMembershipMemoWithNoFeedMonitorIsSafe(t *testing.T) {
	if got := clearYouTubeMembershipMemo("youtube", nil); got != 0 {
		t.Errorf("a clear with no feed monitor returned %d, want 0", got)
	}
}
