package twitch

import "encoding/json"

// PlaybackTokenSession reports what a playback access token says about the
// session it was issued to: (signedIn, conclusive).
//
// TwitchAccessToken.Value is a JSON document Twitch signs and Usher verifies.
// Nothing in Moombox has ever looked inside it — it is passed through verbatim
// as a URL parameter by BuildUsherLiveURL, which percent-encodes it at that
// moment, so the field itself is plain JSON and needs no unescaping here.
//
// What is inside is the entitlement the stream will actually be served under,
// and that is the fact a dead auth-token hides: GetStreamAccessToken succeeds
// either way and returns the same two-field shape, so an expired token
// silently yields an ANONYMOUS playback token. The capture then takes stitched
// ads (skipped, correctly, leaving a timestamp jump) and is refused outright on
// subscriber-only content — with nothing above Info in the log to explain it.
//
// user_id is the discriminator, confirmed by the Arc 10 Task 0 live probe: an
// authenticated reply carries it, an anonymous one carries null. No other
// field is read, and NOTHING read here leaves the function: the return is two
// booleans.
//
// The test is on the RAW JSON TYPE rather than a typed decode. Decoding
// straight into *int64 or *json.Number would ERROR rather than classify if
// Twitch ever rendered this id as a string, and an error there would most
// likely be folded into "unknown" or, worse, into "anonymous".
//
// This is an anonymous-vs-signed-in signal and NEVER an entitlement one. The
// Task 0 probe's authenticated reply carried a numeric user_id beside
// subscriber=false, turbo=false and privileged=false: a signed-in token that
// cannot fetch subscriber-only content still reads signedIn here, by design.
//
// conclusive == false means this learned nothing, and the caller must treat it
// as silence. THE ABSENT-KEY CASE IS DELIBERATELY INCONCLUSIVE, not anonymous:
// a key Twitch renames is a response-shape change, and folding it into "the
// token belongs to nobody" would mark the platform dead on every capture for
// every user on the day of that change. An explicit null is the only anonymous
// verdict this will give.
func PlaybackTokenSession(value string) (signedIn, conclusive bool) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &doc); err != nil {
		return false, false
	}
	raw, ok := doc["user_id"]
	if !ok {
		return false, false
	}
	if string(raw) == "null" {
		return false, true
	}
	return true, true
}

// playbackTokenReportsAnonymous is the WHOLE decision behind the HLS-side
// credential mark: should this playback token be reported as proof that the
// saved Twitch credentials are dead?
//
// It is a named function rather than three conditions inline in
// GetHLSMasterPlaylist because inline it would be untestable — that method
// makes a GQL call against a hardcoded endpoint and then fetches a real Usher
// playlist, so a test could only restate the condition, and a restated
// condition passes under every mutation of the real one.
//
// Three conditions, each doing its own work:
//
//   - authToken != "" — credentials were actually SENT. A cookieless install
//     gets an anonymous token by design and must never be told its credentials
//     failed; this is the same guard noteMissingLogin's token check applies on
//     the chat side, and dropping it marks Twitch dead on every install that
//     never configured it.
//   - conclusive — the document was readable. A renamed key is a response
//     shape change, not a verdict; see PlaybackTokenSession.
//   - !signedIn — Twitch says this token belongs to nobody.
//
// It does not cover every dead credential, and must not be read as if it did.
// An auth-token Twitch rejects outright fails earlier, inside gqlRequest, as
// ErrTwitchAuthExpired; this predicate only sees the arm where the GQL call
// SUCCEEDS and hands back a token minted for nobody. Both arms exist and the
// Task 0 probe observed only the anonymous-by-design one.
func playbackTokenReportsAnonymous(authToken, tokenValue string) bool {
	if authToken == "" {
		return false
	}
	signedIn, conclusive := PlaybackTokenSession(tokenValue)
	return conclusive && !signedIn
}
