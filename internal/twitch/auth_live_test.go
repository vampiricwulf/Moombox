package twitch

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// TestLiveAuthenticatedTokenValidate is the Twitch mirror of
// youtube.TestLiveAuthenticatedAccountProbe
// (internal/youtube/liveness_markers_live_test.go).
//
// This arc split the cookie jar into separate YouTube and Twitch in-memory
// jars. A unit test cannot tell a token Twitch accepts from one it does not,
// so proving the Twitch half of that split still works needs a live check
// against the real service — the same reason the YouTube half has one and,
// until now, Twitch had none.
//
// This exercises the whole chain the partition put at risk: the cookie file
// -> Load's domain-first admission -> the Twitch jar -> GetCookie("auth-token")
// -> Auth.GetAuthToken -> a bearer token Twitch's oauth2/validate answers 200
// to. A wrongly-routed GetCookie (reading the YouTube jar, or reading the
// wrong cookie name) fails here at the HasAuthToken step, loudly, rather than
// silently degrading chat to an anonymous login the way chat_irc.go's own
// fallback would.
//
// Enable with MOOMBOX_LIVE_TWITCH_COOKIES=<path to a Netscape cookie file for
// a signed-in Twitch session>. The path alone is the opt-in; no second flag.
// The cookie file is read by the jar and never printed: this test asserts a
// verdict and reports presence, never any cookie value, and Auth is built with
// nopLogger so the probe's own logging is discarded too. ValidateToken's
// success line no longer names the account's login — it logs the opaque
// user_id alone — but nopLogger stays, because a live test has no business
// deciding which of a subject's log lines are safe to print.
//
// The unbounded-body hazard this test used to defend against is fixed at the
// source: an unexpected (non-200, non-401) status now yields the status, the
// media type, and at most a 200-byte prefix of a text/plain or
// application/json body (validateErrorDetail). The truncation below stays
// anyway — it costs one verb and it is the last line of defence if that rule
// is ever widened.
func TestLiveAuthenticatedTokenValidate(t *testing.T) {
	path := os.Getenv("MOOMBOX_LIVE_TWITCH_COOKIES")
	if path == "" {
		t.Skip("set MOOMBOX_LIVE_TWITCH_COOKIES=<path to a signed-in Netscape cookie file> to run the authenticated token validation check")
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

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ok, err := auth.ValidateToken(ctx)
	if err != nil {
		// Bounded on purpose: see the doc comment above. validateErrorDetail
		// already bounds what the error can carry; this is belt and braces.
		t.Fatalf("validate token against %s errored: %.300s", constants.TwitchURLs.OAuthValidate, err)
	}
	if !ok {
		t.Fatal("ValidateToken = false: the token was rejected by Twitch (401) — the cookie " +
			"file's auth-token has expired or been revoked; if the cookies really are fresh, " +
			"something in the chain (wrong cookie routed, wrong header, endpoint changed) is " +
			"broken and needs investigating before it is a plumbing failure and not a real expiry")
	}
}
