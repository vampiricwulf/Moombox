package twitch

import "testing"

// Arc 10 R6, branch A. These are the exact shapes the Task 0 live probe
// observed, reduced to the one key that answers the question. No fixture here
// carries a real token: the Signature is absent entirely and every value is
// obviously synthetic.

// TestPlaybackTokenSessionReadsTheAuthenticatedAndAnonymousShapes.
//
// The mutation: reading truthiness of the whole document, or treating any
// non-empty Value as authenticated — both of which pass on the authenticated
// fixture and report an anonymous token as signed in, which is the exact
// silence this branch exists to end.
func TestPlaybackTokenSessionReadsTheAuthenticatedAndAnonymousShapes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		value      string
		signedIn   bool
		conclusive bool
	}{
		{"authenticated", `{"user_id":12345678,"expires":1900000000,"channel":"somechannel"}`, true, true},
		{"anonymous null", `{"user_id":null,"expires":1900000000,"channel":"somechannel"}`, false, true},
		{"authenticated with a string id", `{"user_id":"12345678","expires":1900000000}`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signedIn, conclusive := PlaybackTokenSession(tc.value)
			if signedIn != tc.signedIn || conclusive != tc.conclusive {
				t.Errorf("PlaybackTokenSession = (%v, %v), want (%v, %v)", signedIn, conclusive, tc.signedIn, tc.conclusive)
			}
		})
	}
}

// TestPlaybackTokenSessionRefusesToGuess is the over-claim guard, and it is
// the assertion that keeps this from becoming a false-alarm generator.
//
// A key Twitch RENAMES, a document that stops being JSON, an empty value — none
// of those is a statement about the session, and reporting any of them as
// anonymous would mark the platform dead on every capture, for every user, on
// the day of the change.
//
// The mutation: `if _, ok := doc["user_id"]; !ok { return false, true }` —
// folding "the key is gone" into "the token belongs to nobody". That mutation
// passes both cases above and turns a Twitch field rename into a global
// credential alarm.
func TestPlaybackTokenSessionRefusesToGuess(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"the key was renamed", `{"userId":12345678,"expires":1900000000}`},
		{"not an object", `"a signed opaque blob"`},
		{"not JSON at all", `%7B%22user_id%22%3A1%7D`},
		{"empty", ``},
		{"a bare JSON null document", `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signedIn, conclusive := PlaybackTokenSession(tc.value)
			if conclusive {
				t.Errorf("PlaybackTokenSession reported a CONCLUSIVE %v for a document it cannot read", signedIn)
			}
		})
	}
}

// TestPlaybackTokenReportsAnonymousIsTheWholeDecision drives the PRODUCTION
// predicate, not a copy of it.
//
// The decision has to live in a named function for that reason. Written inline
// in GetHLSMasterPlaylist it is unreachable offline — that method makes a GQL
// call against a hardcoded endpoint and then fetches a real Usher playlist,
// neither of which has a seam — so a test could only restate the condition and
// would then pass under every mutation of the real one.
// playbackTokenReportsAnonymous is the whole decision, and
// GetHLSMasterPlaylist's only job is to call it.
//
// Two halves, both load-bearing. A cookieless install gets an anonymous
// playback token BY DESIGN and must never be told its credentials failed —
// the same rule noteMissingLogin's token check enforces on the chat side. And
// an install that DID send a token and got an anonymous document back is the
// case this branch exists for: it is visible even with chat capture off.
//
// The mutations: dropping the `authToken != ""` guard (every cookieless
// install marks Twitch dead on its first capture), reporting on `!conclusive`
// (every Twitch response-shape change does the same, for every user, on the
// day of the change), and inverting `signedIn` (a healthy credential marks
// itself dead). All three are caught here because all three are in this
// function.
func TestPlaybackTokenReportsAnonymousIsTheWholeDecision(t *testing.T) {
	for _, tc := range []struct {
		name       string
		authToken  string
		value      string
		wantReport bool
	}{
		{"cookieless install, anonymous token", "", `{"user_id":null}`, false},
		{"cookieless install, unreadable document", "", `{"userId":1}`, false},
		{"cookieless install, honoured", "", `{"user_id":12345678}`, false},
		{"credentials sent, anonymous token", "test-token-aaaa", `{"user_id":null}`, true},
		{"credentials sent, honoured", "test-token-aaaa", `{"user_id":12345678}`, false},
		{"credentials sent, unreadable document", "test-token-aaaa", `{"userId":1}`, false},
		{"credentials sent, not JSON", "test-token-aaaa", `not json`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := playbackTokenReportsAnonymous(tc.authToken, tc.value); got != tc.wantReport {
				t.Errorf("playbackTokenReportsAnonymous = %v, want %v", got, tc.wantReport)
			}
		})
	}
}
