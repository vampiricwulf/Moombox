package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// Arc 10 R6. Service.GetHLSMasterPlaylist answers a second question beside the
// variant list — "did Twitch honour the credentials this install sent?" — and
// noteAnonymousPlayback is everything processTwitchLive does with the answer.
//
// processTwitchLive itself is unreachable offline — sp.tw is a concrete
// *twitch.Service, so there is nothing to substitute at that call site. So the
// decision it makes with that bool lives in a named method, and these drive
// that method. (The method it calls, Service.GetHLSMasterPlaylist, IS covered
// offline: internal/twitch/service_hls_playback_token_test.go swaps the
// package-level twitchHTTPClient.)
//
// Every test name here carries "PlaybackToken" on purpose: it is the -run
// filter this arc's briefs use, and the three tests were once named
// TestNoteAnonymousPlayback*, which that filter silently skipped — turning two
// mutation runs into false greens. Keep the prefix.

// TestPlaybackTokenMarkFiresOnlyWhenTwitchIgnoredTheCredentials.
//
// Two mutations, both silent and both fatal in opposite directions:
//
//   - dropping the `if !anonymousPlayback { return }` guard — EVERY capture on
//     EVERY install then marks Twitch as needing re-authorization, including
//     the healthy ones and the cookieless ones, which turns the platform badge
//     into a light that is always on.
//   - marking with a different member of the vocabulary
//     (twitch.AuthDowngradeLoginRefused is the one a copy-paste lands on) —
//     the platform is marked, so a bare "something was marked" assertion still
//     passes, and the operator is told their login was refused by a chat
//     handshake that never ran.
func TestPlaybackTokenMarkFiresOnlyWhenTwitchIgnoredTheCredentials(t *testing.T) {
	for _, tc := range []struct {
		name              string
		anonymousPlayback bool
		want              []string
	}{
		{"twitch honoured the credentials", false, nil},
		{"twitch ignored the credentials", true, []string{twitch.AuthDowngradePlaybackTokenAnonymous}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var marked []string
			sp := &StreamProcessor{}
			sp.SetOnTwitchAuthLoss(func(reason string) { marked = append(marked, reason) })

			sp.noteAnonymousPlayback(tc.anonymousPlayback)

			if len(marked) != len(tc.want) {
				t.Fatalf("marks = %v, want %v", marked, tc.want)
			}
			for i, reason := range tc.want {
				if marked[i] != reason {
					t.Errorf("mark %d = %q, want %q", i, marked[i], reason)
				}
			}
		})
	}
}

// TestPlaybackTokenMarkWithNoSeamWiredIsInert. Every install that has no
// refresh service wired — every test in this package included — reaches this
// method on the ordinary capture path.
//
// The mutation: `sp.onTwitchAuthLoss(...)` called without the nil check, which
// panics on the job goroutine and takes the capture down instead of starting
// it. A degradation report must never be able to cost more than the
// degradation.
func TestPlaybackTokenMarkWithNoSeamWiredIsInert(t *testing.T) {
	sp := &StreamProcessor{}
	sp.noteAnonymousPlayback(true)
	sp.noteAnonymousPlayback(false)
}

// TestPlaybackTokenMarkReachesTheOperatorSentence drives the whole route
// this task added — worker method → the platform mark → AuthStatus — through
// the REAL RefreshService rather than a recording stub.
//
// The stub test above pins which token is sent; this pins that the token sent
// is one internal/cookies has an arm for. They are different failures: a
// reason token whose value drifts from its cookies-side twin, or one added to
// twitch's vocabulary with no arm written beside it, still delivers a mark and
// still passes every assertion that only counts marks — and lands the operator
// on "the saved Twitch login could not be used", which names a chat handshake
// that never ran and no remedy for the thing that actually happened.
//
// No network: NoteTwitchAuthLoss makes no request, and on an EMPTY jar
// shouldFireRecovery declines, so no callback fires either.
func TestPlaybackTokenMarkReachesTheOperatorSentence(t *testing.T) {
	rs := cookies.NewRefreshService(cookies.NewCookieJar(), 0, nopWorkerLogger{})
	sp := &StreamProcessor{}
	sp.SetOnTwitchAuthLoss(rs.NoteTwitchAuthLoss)

	sp.noteAnonymousPlayback(true)

	got := rs.GetStatus().TwitchError
	if got == "" {
		t.Fatal("the platform carries no reason at all after an anonymous playback token")
	}
	// Discovered, not hardcoded, so this cannot drift from the default arm's
	// wording.
	if generic := twitchAuthLossSentence(t, "a-token-no-arm-was-ever-written-for"); got == generic {
		t.Errorf("the operator sentence is the generic fallback %q — internal/cookies has no arm for the playback-token route", got)
	}
	if status := rs.GetStatus(); status.TwitchAuthenticated {
		t.Error("the platform still reads authenticated after an anonymous playback token")
	}
}
