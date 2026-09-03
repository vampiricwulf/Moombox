package twitch

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// TestLiveOfflineChannelPlaybackToken is Arc 12b Task 0: the ONE live
// measurement that decides which branch Task 3 takes.
//
// THE QUESTION. R2 says the tier-2 probe may target any configured Twitch
// channel, live or not. Arc 10's Task 0 measured only a LIVE channel
// (playback_token_live_test.go requires MOOMBOX_LIVE_TWITCH_CHANNEL to be live,
// and its comment asserts without measuring that "an offline channel yields no
// stream playback token at all"). If an offline channel DOES answer with a
// document carrying user_id, the probe costs one GQL request per tick and needs
// no liveness state; if it does not, Task 3 pays for a GetStreamInfoBatch first.
//
// WHAT THIS PRINTS, absolutely: field NAMES, JSON TYPES, booleans, and the two
// PlaybackTokenSession verdicts. Never a string value, never a number, never
// the Signature, never the auth-token, never a byte of the cookie file. The
// token document is a signed entitlement carrying a device id and a user ip,
// and this output goes to a terminal, a CI log and a pasted report.
//
// Enable with:
//
//	MOOMBOX_LIVE_TWITCH_PROBE=1
//	MOOMBOX_LIVE_TWITCH_COOKIES=<path to a Netscape cookie file for a
//	                             signed-in Twitch session>
//	MOOMBOX_LIVE_TWITCH_OFFLINE_CHANNEL=<login of a channel that is OFFLINE
//	                                     right now>
//
// The arming var is separate from the credential var so running Arc 10's probe
// (which keys on the cookie path alone) does not also fire this one; the
// channel var is separate from MOOMBOX_LIVE_TWITCH_CHANNEL because that one
// means the opposite thing. Always run with -count=1.
func TestLiveOfflineChannelPlaybackToken(t *testing.T) {
	if os.Getenv("MOOMBOX_LIVE_TWITCH_PROBE") != "1" {
		t.Skip("set MOOMBOX_LIVE_TWITCH_PROBE=1 (plus MOOMBOX_LIVE_TWITCH_COOKIES and MOOMBOX_LIVE_TWITCH_OFFLINE_CHANNEL) to run the Arc 12b offline-channel measurement")
	}
	path := os.Getenv("MOOMBOX_LIVE_TWITCH_COOKIES")
	if path == "" {
		t.Skip("set MOOMBOX_LIVE_TWITCH_COOKIES=<path to a signed-in Netscape cookie file> to run the offline-channel measurement")
	}
	channel := os.Getenv("MOOMBOX_LIVE_TWITCH_OFFLINE_CHANNEL")
	if channel == "" {
		t.Skip("set MOOMBOX_LIVE_TWITCH_OFFLINE_CHANNEL=<login of a channel that is OFFLINE right now> to run the offline-channel measurement")
	}

	jar := cookies.NewCookieJar()
	// Load reports only the path on failure, never file contents.
	if err := jar.Load(path); err != nil {
		t.Fatalf("load cookie file: %v", err)
	}
	auth := NewAuth(jar, nopLogger{})
	if !auth.HasAuthToken() {
		t.Fatal("that cookie file carries no Twitch auth-token cookie — check the export, or that the path is the right file")
	}

	api := NewAPI(nopLogger{})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// PREMISE CHECK, and it must run first: the channel has to actually be
	// offline, or this measures the case Arc 10 already measured.
	if info, err := api.GetStreamInfo(ctx, channel, auth.GetAuthToken()); err != nil {
		t.Fatalf("could not confirm the channel is offline (error type %T) — pick another login", err)
	} else if info != nil {
		t.Fatalf("%q is LIVE right now; this measurement needs an OFFLINE channel", channel)
	}
	t.Logf("premise: %q is offline", channel)

	// The authenticated reply. GetAuthToken() is read once and passed straight
	// through; it is never assigned to a variable this test prints.
	authed, authedErr := api.GetStreamAccessToken(ctx, channel, auth.GetAuthToken())
	if authedErr != nil {
		// The error TYPE only. gqlRequest interpolates the response body on
		// 4xx/5xx and an intermediary's error page can echo the Authorization
		// header.
		t.Logf("AUTHENTICATED reply for an offline channel FAILED (error type %T)", authedErr)
		t.Log("FINDING: BRANCH B — an offline channel cannot be probed. " +
			"Task 3 uses the configuredTwitchLogins + firstLiveTwitchLogin form.")
		return
	}

	// The anonymous control. An empty token makes doGQLOnce omit the
	// Authorization header entirely — the same request a cookieless install
	// makes.
	anon, anonErr := api.GetStreamAccessToken(ctx, channel, "")
	if anonErr != nil {
		t.Logf("ANONYMOUS control reply FAILED (error type %T) — the set difference below is unavailable", anonErr)
	}

	authedSignedIn, authedConclusive := PlaybackTokenSession(authed.Value)
	t.Logf("PlaybackTokenSession(authenticated) = signedIn:%v conclusive:%v", authedSignedIn, authedConclusive)

	// The branch, decided and LOGGED before the shapes are described:
	// describePlaybackToken Fatals on a Value that is not a JSON object, and
	// that case is a finding (branch B), not a crash without one. Only the
	// AUTHENTICATED reply decides it: the probe's whole job is to say whether
	// OUR session is still honoured, and the anonymous reply is a control on
	// the discriminator, not an input to the verdict.
	if authedConclusive && authedSignedIn {
		t.Log("FINDING: BRANCH A — an offline channel answers, and user_id identifies the session. " +
			"Task 3 uses pickTwitchProbeChannel's first-configured-login form.")
	} else {
		t.Logf("FINDING: BRANCH B — the offline reply decoded but did not identify the session "+
			"(signedIn:%v conclusive:%v). Task 3 uses the configuredTwitchLogins + firstLiveTwitchLogin form.",
			authedSignedIn, authedConclusive)
	}

	// The shapes, for the record: field names, JSON types and booleans only.
	authedShape := describePlaybackToken(t, "authenticated", authed.Value)
	if anonErr == nil {
		anonSignedIn, anonConclusive := PlaybackTokenSession(anon.Value)
		t.Logf("PlaybackTokenSession(anonymous) = signedIn:%v conclusive:%v", anonSignedIn, anonConclusive)
		anonShape := describePlaybackToken(t, "anonymous", anon.Value)
		reportKeyDifference(t, authedShape, anonShape)
	}
}
