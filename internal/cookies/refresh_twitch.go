package cookies

// refresh_twitch.go — the Twitch oauth2/validate auth check.

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

func (rs *RefreshService) checkTwitchAuth(ctx context.Context) (bool, error) {
	// Read the token once. It is both the gate and the credential, so asking
	// HasTwitchAuthCookies first and re-reading the value after would leave a
	// window in which a concurrent jar.Reload swaps the map between the two.
	token := rs.jar.GetTwitchAuthToken()
	if token == "" {
		// Deliberately NOT broadened the way the YouTube gate above was, and
		// the reason has nothing to do with what Twitch would answer.
		//
		// Twitch auth is a single bearer token. With no auth-token there is
		// no credential to validate, so a request could not learn anything
		// about THIS install's session whatever came back — which makes "not
		// authenticated" true here rather than inferred. That is the whole
		// difference from a cleared LOGIN_INFO, which says nothing at all
		// about whether the Google session still works.
		//
		// Sending an empty OAuth header just to reach the network therefore
		// buys nothing, and would force the 200/401-only rule below to read
		// the reply as if it were a verdict on a token this install does not
		// have.
		//
		// The "was Twitch ever configured" question, which is what decides
		// whether an alarm fires, is answered by jar.HasAnyTwitchAuthCookie
		// at the doRefresh gate instead: a jar holding twilight-user and no
		// auth-token is a session that plainly was configured and now has no
		// credential, and it reports as configured-and-broken rather than as
		// never-configured. See twitchAuthCookieNames for how that state
		// arises.
		return false, nil
	}

	ctx, cancel := context.WithTimeout(ctx, authCheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, twitchValidateURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "OAuth "+token)

	resp, err := cookiesHTTPClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("twitch auth check: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	// The 401 rule below is only a statement about OUR token if our token is
	// what reached the endpoint. Go strips Authorization on a cross-hostname
	// redirect exactly as it strips Cookie, and the strip is sticky, so an
	// intermediary that bounces this call and answers 401 would otherwise
	// produce a conclusive dead-token verdict about a token it never saw.
	if err := authResponseIsOurs(resp, req, "Authorization"); err != nil {
		return false, fmt.Errorf("twitch auth check: %w", err)
	}

	// Twitch documents exactly two answers for oauth2/validate: 200 for a
	// valid token, 401 for an invalid one. So 401 stays CONCLUSIVE — it is
	// the one status that genuinely means "sign in again", and folding it
	// into the error branch below would suppress recovery and the re-login
	// prompt for every expired token. Everything else is infrastructure (a
	// rate limiter, an outage, an edge block) and says nothing about the
	// token, so it must not be reported as dead credentials — the same
	// mistake the YouTube guide check made, reachable here through the same
	// checkPlatformAuth mapping.
	//
	// The error names the status and nothing else, so a response body echoed
	// back by an intermediary can never be interpolated into it. It does NOT
	// reach AutoCookieService.setError — services.go wires VerifyTwitchAuth to
	// CheckTwitchAuth through checkPlatformAuth, which discards the value. Since
	// Arc 8 Task 12a it DOES reach two per-request screens as
	// AuthStatus.TwitchError: the REST payload's `twitchError` and the TUI's R C
	// result line. See errGuideLoginMarkerUnreadable's doc comment for the full
	// accounting of where these strings go and why the rule above governs it.
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusUnauthorized:
		return false, nil
	default:
		return false, fmt.Errorf("twitch auth check: unexpected status %d", resp.StatusCode)
	}
}
