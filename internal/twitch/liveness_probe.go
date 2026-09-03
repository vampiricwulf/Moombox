package twitch

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrLivenessProbeNotAttempted means ProbeSessionLiveness declined to make a
// request at all: the jar holds no Twitch auth-token, or the channel login is
// not something GetStreamAccessToken can ask about.
//
// It is a sentinel rather than a plain error because the caller's log line
// distinguishes "the probe could not be attempted" (an install that is simply
// not set up for it) from "the probe ran and learned nothing" (a signal that
// may be broken). Same distinction internal/cookies draws with
// ErrAuthCheckNotAttempted.
var ErrLivenessProbeNotAttempted = errors.New("twitch liveness probe: not attempted")

// ProbeSessionLiveness asks whether the stored Twitch session is still signed
// in, using the playback access token as the evidence.
//
// The Twitch twin of YouTube's channel-independent liveness probe, and it
// exists because oauth2/validate cannot answer the question: validate returns
// 200 for a token that is valid but no longer entitled to authenticated
// playback, so RefreshService.checkTwitchAuth reads a dead session as healthy.
// The playback access token DOES say, because Twitch mints it for a session —
// user_id present is signed in, JSON null is nobody (PlaybackTokenSession, and
// Arc 10 Task 0's live measurement behind it). The request is exactly the one
// GetHLSMasterPlaylist already makes; nothing here fetches a playlist, so the
// Usher URL is never built.
//
// THE RETURN CARRIES NO TOKEN. Two booleans and an error, and the error is
// synthesised rather than passed through: gqlRequest interpolates the response
// body into its 4xx and auth errors, an intermediary's error page can echo the
// Authorization header, and the caller logs this into a stream that reaches the
// Web UI and the TUI.
//
// conclusive == false is SILENCE — never a signed-out session. That includes
// the 401/403 arm: gqlRequest raises ErrTwitchAuthExpired for both statuses,
// 403 is an edge block as often as a credential verdict, and whether a
// genuinely expired auth-token even surfaces this way is unmeasured
// (platform-services.md § Anonymous Fallback; field-test gate 18). The sentinel
// is wrapped so that measurement can find the arm.
//
// The auth token is read ONCE, into one local used for both the guard and the
// request — the discipline GetHLSMasterPlaylist documents.
func (s *Service) ProbeSessionLiveness(ctx context.Context, channelLogin string) (signedIn, conclusive bool, err error) {
	authToken := s.Auth.GetAuthToken()
	if authToken == "" {
		// A cookieless install gets an ANONYMOUS token by design. Sending the
		// request anyway would produce a conclusive "signed out" about a
		// session that does not exist — the same guard
		// playbackTokenReportsAnonymous applies first, for the same reason.
		return false, false, fmt.Errorf("%w: no twitch auth-token in the jar", ErrLivenessProbeNotAttempted)
	}
	// GetStreamAccessToken runs safeLoginRe over the login, so an empty or
	// punctuation-only value becomes an empty channelName in the query — a
	// request that cannot answer. Refuse it here instead, using the same rule
	// so the two cannot drift.
	if safeLoginRe.ReplaceAllString(strings.ToLower(channelLogin), "") == "" {
		return false, false, fmt.Errorf("%w: no usable twitch channel login to probe", ErrLivenessProbeNotAttempted)
	}

	token, err := s.API.GetStreamAccessToken(ctx, channelLogin, authToken)
	if err != nil {
		if errors.Is(err, ErrTwitchAuthExpired) {
			// Named, not classified. See the doc comment: this is inconclusive.
			return false, false, fmt.Errorf("twitch liveness probe: twitch refused the credentials on the playback-token request: %w", ErrTwitchAuthExpired)
		}
		// The error TYPE only — never the upstream text, which may carry the
		// response body.
		return false, false, fmt.Errorf("twitch liveness probe: the playback-token request failed (error type %T)", err)
	}

	signedIn, conclusive = PlaybackTokenSession(token.Value)
	if !conclusive {
		// The document was unreadable or carried no user_id. A renamed key is
		// a response-shape change, not a verdict; the message says so without
		// quoting a single byte of the document.
		return false, false, errors.New("twitch liveness probe: the playback token did not say which session it was issued to")
	}
	return signedIn, true, nil
}
