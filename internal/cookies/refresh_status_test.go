package cookies

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// jarWithAuth writes a minimal Netscape file with the two cookies
// HasYouTubeAuthCookies requires, and loads it.
func jarWithAuth(t *testing.T) *CookieJar {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	content := "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tsapisid-value\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tLOGIN_INFO\tlogin-info-value\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	return jar
}

// jarWithTwitchAuth is the Twitch counterpart: the one cookie
// HasTwitchAuthCookies requires.
func jarWithTwitchAuth(t *testing.T) *CookieJar {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	content := "# Netscape HTTP Cookie File\n" +
		"#HttpOnly_.twitch.tv\tTRUE\t/\tTRUE\t0\tauth-token\tauth-token-value\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	return jar
}

// pointYouTubeGuideAt redirects BOTH guide seams at srv for the duration of
// the test. Both, always: checkYouTubeAuth reads youtubeGuideURL while
// checkAndRefreshYouTube reads youtubeGuideRefreshURL, so overriding only the
// one a test happens to exercise today leaves the test one refactor away from
// issuing a real request to youtube.com.
func pointYouTubeGuideAt(t *testing.T, srv *httptest.Server) {
	t.Helper()
	origPlain, origRefresh := youtubeGuideURL, youtubeGuideRefreshURL
	youtubeGuideURL, youtubeGuideRefreshURL = srv.URL, srv.URL
	t.Cleanup(func() { youtubeGuideURL, youtubeGuideRefreshURL = origPlain, origRefresh })
}

// pointTwitchValidateAt is the Twitch equivalent. One seam, but the same rule:
// a test that forgets it hits id.twitch.tv for real.
func pointTwitchValidateAt(t *testing.T, srv *httptest.Server) {
	t.Helper()
	orig := twitchValidateURL
	twitchValidateURL = srv.URL
	t.Cleanup(func() { twitchValidateURL = orig })
}

// statusServer serves one fixed status code with an empty body.
func statusServer(t *testing.T, code int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestGuideNon200IsInconclusive: a 429 or 503 must NOT be reported as
// "conclusively not authenticated". shouldFireRecovery keys on checkErr ==
// nil, so returning (false, nil) here makes a rate-limit look like dead
// credentials — and once G6 unblocks the notification, an alarm.
//
// Each status runs as a subtest so the seam override can be restored by
// t.Cleanup: a panic (or a Fatal in a helper) inside the loop body would
// otherwise leave the package vars pointing at a closed server for every
// later test in the package, and this file sorts before refresh_test.go.
func TestGuideNon200IsInconclusive(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusForbidden} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			pointYouTubeGuideAt(t, statusServer(t, code))
			rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})

			auth, err := rs.checkAndRefreshYouTube(context.Background())

			if auth {
				t.Errorf("status %d: authenticated = true, want false", code)
			}
			if err == nil {
				t.Errorf("status %d: err = nil, want non-nil — a non-200 is not an auth verdict", code)
			}
		})
	}
}

// TestGuide200LoggedOutIsConclusive: a real 200 that says logged_in=0 IS a
// conclusive auth failure and must keep returning a nil error.
func TestGuide200LoggedOutIsConclusive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"responseContext":{"mainAppWebResponseContext":{"loggedIn":false}}}`))
	}))
	t.Cleanup(srv.Close)
	pointYouTubeGuideAt(t, srv)

	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	auth, err := rs.checkAndRefreshYouTube(context.Background())
	if auth {
		t.Error("authenticated = true, want false")
	}
	if err != nil {
		t.Errorf("err = %v, want nil — a 200 saying logged-out is a real verdict", err)
	}
}

// TestCheckYouTubeAuthNon200IsInconclusive is the same rule for the other
// entry point. checkYouTubeAuth is what cmd/moombox wires into
// AutoCookieService.VerifyYouTubeAuth (via the exported CheckYouTubeAuth), so
// its non-200 answer decides whether a profile import is committed or rolled
// back — see TestImportIsNotCommittedWhenTheRealCheckIsRateLimited.
func TestCheckYouTubeAuthNon200IsInconclusive(t *testing.T) {
	pointYouTubeGuideAt(t, statusServer(t, http.StatusTooManyRequests))

	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	auth, err := rs.CheckYouTubeAuth(context.Background())
	if auth {
		t.Error("authenticated = true, want false")
	}
	if err == nil {
		t.Error("err = nil, want non-nil — a 429 is not an auth verdict")
	}
}

// --- Twitch: the same rule, with one deliberate exception ---

// TestTwitchValidateNon200IsInconclusive mirrors the YouTube fix on
// id.twitch.tv/oauth2/validate, which had the same
// `return resp.StatusCode == http.StatusOK, nil` shape: every non-200
// collapsed to a conclusive "not authenticated".
//
// Twitch documents exactly two responses for this endpoint — 200 for a valid
// token and 401 for an invalid one. Anything else is therefore infrastructure
// (a rate limiter, an outage, an edge block), never a statement about the
// token, so it must not be reported as one.
func TestTwitchValidateNon200IsInconclusive(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusForbidden, http.StatusBadGateway} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			pointTwitchValidateAt(t, statusServer(t, code))
			rs := NewRefreshService(jarWithTwitchAuth(t), 0, nopLogger{})

			auth, err := rs.checkTwitchAuth(context.Background())

			if auth {
				t.Errorf("status %d: authenticated = true, want false", code)
			}
			if err == nil {
				t.Errorf("status %d: err = nil, want non-nil — a non-200 is not an auth verdict", code)
			}
		})
	}
}

// TestTwitchValidate401IsConclusive is the boundary, and the reason the
// YouTube rule could not simply be copied. 401 from oauth2/validate is
// Twitch's documented "invalid access token" — a real, actionable verdict.
// Folding it into the inconclusive bucket would turn the one status that
// genuinely means "sign in again" into a shrug, suppressing recovery and the
// re-login prompt for every expired Twitch token.
func TestTwitchValidate401IsConclusive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"status":401,"message":"invalid access token"}`))
	}))
	t.Cleanup(srv.Close)
	pointTwitchValidateAt(t, srv)

	rs := NewRefreshService(jarWithTwitchAuth(t), 0, nopLogger{})
	auth, err := rs.checkTwitchAuth(context.Background())
	if auth {
		t.Error("authenticated = true, want false")
	}
	if err != nil {
		t.Errorf("err = %v, want nil — 401 is Twitch's documented invalid-token verdict", err)
	}
}

// TestTwitchValidate200IsAuthenticated guards the happy path against the
// three-way split above being written the wrong way round.
func TestTwitchValidate200IsAuthenticated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"client_id":"abc","login":"someuser","user_id":"1","expires_in":3600}`))
	}))
	t.Cleanup(srv.Close)
	pointTwitchValidateAt(t, srv)

	rs := NewRefreshService(jarWithTwitchAuth(t), 0, nopLogger{})
	auth, err := rs.checkTwitchAuth(context.Background())
	if !auth {
		t.Error("authenticated = false, want true — 200 is a valid token")
	}
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}

// TestTwitchValidateErrorNamesOnlyTheStatus pins that the inconclusive error
// carries the status code and nothing from the response body. The sibling
// implementation in internal/twitch/auth.go interpolates the body into its
// error; here the string reaches AutoCookieService.setError and is rendered in
// the Web UI and TUI, so an intermediary (proxy, captive portal, WAF) echoing
// the request back would put it on screen. The status alone is enough to act
// on.
func TestTwitchValidateErrorNamesOnlyTheStatus(t *testing.T) {
	const secretish = "auth-token-value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		// A hostile/naive intermediary reflecting the request headers.
		w.Write([]byte("blocked request: Authorization: OAuth " + secretish))
	}))
	t.Cleanup(srv.Close)
	pointTwitchValidateAt(t, srv)

	rs := NewRefreshService(jarWithTwitchAuth(t), 0, nopLogger{})
	_, err := rs.checkTwitchAuth(context.Background())
	if err == nil {
		t.Fatal("err = nil, want non-nil")
	}
	if strings.Contains(err.Error(), secretish) {
		t.Errorf("the error carries credential material from the response body: %q", err)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("the error should name the status it could not interpret, got %q", err)
	}
}
