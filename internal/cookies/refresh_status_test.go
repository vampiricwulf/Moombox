package cookies

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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

// --- Presence is not liveness: the checks that never reached the network ---

// countingGuide serves one fixed body and counts requests. The counter is
// atomic because the handler runs on the server's own goroutine and the test
// reads it after the client call returns, which is not an ordering the race
// detector can see.
func countingGuide(t *testing.T, body string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

const loggedOutGuideBody = `{"responseContext":{"mainAppWebResponseContext":{"loggedIn":false}}}`

// TestHalfClearedJarStillProbes: with LOGIN_INFO gone but SAPISID intact, the
// check must make a REQUEST and read what YouTube says — not infer death from
// the missing cookie. Presence is not liveness.
//
// This is the state yt-dlp documents as rotation-invalidation: YouTube clears
// LOGIN_INFO and leaves SAPISID behind. Before this change the strict
// HasYouTubeAuthCookies gate returned (false, nil) without a request — a
// verdict of "conclusively logged out" that was really only "a cookie is
// missing", and one shouldFireRecovery could not tell apart from a platform
// nobody ever set up.
func TestHalfClearedJarStillProbes(t *testing.T) {
	srv, hits := countingGuide(t, loggedOutGuideBody)
	pointYouTubeGuideAt(t, srv)

	jar := jarWithAuth(t)
	jar.cookies["LOGIN_INFO"] = "" // the rotation-invalidation state

	rs := NewRefreshService(jar, 0, nopLogger{})
	auth, err := rs.checkAndRefreshYouTube(context.Background())
	if got := hits.Load(); got != 1 {
		t.Fatalf("server hits = %d, want 1 — the check short-circuited instead of probing", got)
	}
	if auth || err != nil {
		t.Errorf("auth=%v err=%v, want false/nil — a real logged-out verdict", auth, err)
	}
}

// TestCheckYouTubeAuthHalfClearedJarStillProbes is the same rule on the other
// entry point. checkYouTubeAuth is what AutoCookieService.VerifyYouTubeAuth
// calls, so its answer decides whether a freshly imported profile is committed
// — that answer has to come from YouTube, not from a name lookup. Here the
// server says logged_in=1, so the half-cleared jar is reported WORKING.
func TestCheckYouTubeAuthHalfClearedJarStillProbes(t *testing.T) {
	srv, hits := countingGuide(t, `{"responseContext":{"serviceTrackingParams":[{"params":[{"key":"logged_in","value":"1"}]}]}}`)
	pointYouTubeGuideAt(t, srv)

	jar := jarWithAuth(t)
	jar.cookies["LOGIN_INFO"] = ""

	rs := NewRefreshService(jar, 0, nopLogger{})
	auth, err := rs.CheckYouTubeAuth(context.Background())
	if got := hits.Load(); got != 1 {
		t.Fatalf("server hits = %d, want 1 — the check short-circuited instead of probing", got)
	}
	if !auth || err != nil {
		t.Errorf("auth=%v err=%v, want true/nil — YouTube said logged_in=1", auth, err)
	}
}

// TestNeverConfiguredYouTubeDoesNotProbe is the other side of that gate, and
// what keeps the change from becoming noise: an install holding no Google auth
// cookie at all has no session to have an opinion about. It stays a silent
// (false, nil) — not an error, which would put a scary string on a fresh
// install, and not a request to youtube.com on behalf of a user who never
// configured the platform.
func TestNeverConfiguredYouTubeDoesNotProbe(t *testing.T) {
	srv, hits := countingGuide(t, loggedOutGuideBody)
	pointYouTubeGuideAt(t, srv)

	rs := NewRefreshService(NewCookieJar(), 0, nopLogger{})
	auth, err := rs.checkAndRefreshYouTube(context.Background())
	if got := hits.Load(); got != 0 {
		t.Errorf("server hits = %d, want 0 — an unconfigured platform must not be probed", got)
	}
	if auth || err != nil {
		t.Errorf("auth=%v err=%v, want false/nil", auth, err)
	}
}

// TestNoSAPISIDIsInconclusiveNotDead covers the remaining short-circuit: a jar
// that was configured (LOGIN_INFO is present) but has lost every SAPISID
// variant cannot produce a SAPISIDHASH, so no request can be made at all. That
// is a check that did not happen — it must read as inconclusive (err != nil),
// never as the conclusive "not authenticated" shouldFireRecovery acts on.
func TestNoSAPISIDIsInconclusiveNotDead(t *testing.T) {
	srv, hits := countingGuide(t, loggedOutGuideBody)
	pointYouTubeGuideAt(t, srv)

	jar := jarWithAuth(t)
	jar.cookies["SAPISID"] = ""

	rs := NewRefreshService(jar, 0, nopLogger{})
	auth, err := rs.checkAndRefreshYouTube(context.Background())
	if got := hits.Load(); got != 0 {
		t.Errorf("server hits = %d, want 0 — no Authorization header could be built", got)
	}
	if auth {
		t.Error("authenticated = true, want false")
	}
	if err == nil {
		t.Fatal("err = nil, want non-nil — a check that could not be made is not a verdict")
	}
	// Same rule as TestTwitchValidateErrorNamesOnlyTheStatus: this string is
	// rendered in the Web UI and TUI, so it may never carry cookie material.
	if strings.Contains(err.Error(), "sapisid-value") || strings.Contains(err.Error(), "login-info-value") {
		t.Errorf("the error carries cookie material: %q", err)
	}
}

// --- Twitch: the fifth short-circuit ---

// TestTwitchSessionWithoutItsTokenFiresRecovery is the Twitch half of the same
// defect. The jar ignores cookie expiry while mergeCookieFiles prunes on it,
// so a lapsed auth-token can be pruned out while twilight-user is written
// back — leaving a jar that plainly WAS a signed-in Twitch session with no
// credential left in it. Because doRefresh asked HasTwitchAuthCookies ("is the
// token here"), that state read as "Twitch was never configured" and the
// auth-loss gate stayed silent forever. Exactly the failure this arc closes.
//
// The check itself still must not fire a request: with no bearer token there
// is no credential to validate, so a probe could not learn anything about this
// install's session whatever Twitch answered.
func TestTwitchSessionWithoutItsTokenFiresRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookies.txt")
	content := "# Netscape HTTP Cookie File\n" +
		".twitch.tv\tTRUE\t/\tTRUE\t0\ttwilight-user\t%7B%22id%22%3A%221%22%7D\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}

	var validateHits atomic.Int64
	tw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		validateHits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(tw.Close)
	pointTwitchValidateAt(t, tw)
	// The jar holds no YouTube cookies so the guide seam is never reached,
	// but pin it anyway — an unpinned seam is one refactor from youtube.com.
	ytSrv, ytHits := countingGuide(t, loggedOutGuideBody)
	pointYouTubeGuideAt(t, ytSrv)

	rs := NewRefreshService(jar, 0, nopLogger{})
	var fired []string
	rs.OnRecoveryNeeded = func(platform string) { fired = append(fired, platform) }

	rs.doRefresh(context.Background())

	if got := validateHits.Load(); got != 0 {
		t.Errorf("oauth2/validate hits = %d, want 0 — there is no bearer token to validate", got)
	}
	if got := ytHits.Load(); got != 0 {
		t.Errorf("guide hits = %d, want 0 — no YouTube cookies were configured", got)
	}
	if len(fired) != 1 || fired[0] != "twitch" {
		t.Errorf("OnRecoveryNeeded fired %v, want [twitch] — a Twitch session with no credential must be reported", fired)
	}
}

// TestNeverConfiguredPlatformsStaySilent is the false-alarm guard that keeps
// the broadened predicates honest. An empty jar configures neither platform,
// so neither may fire recovery: a spurious alarm sends an operator off to
// re-export credentials that were never wrong, and in a container the remedy
// it names may not even be reachable.
func TestNeverConfiguredPlatformsStaySilent(t *testing.T) {
	ytSrv, _ := countingGuide(t, loggedOutGuideBody)
	pointYouTubeGuideAt(t, ytSrv)
	pointTwitchValidateAt(t, statusServer(t, http.StatusUnauthorized))

	rs := NewRefreshService(NewCookieJar(), 0, nopLogger{})
	var fired []string
	rs.OnRecoveryNeeded = func(platform string) { fired = append(fired, platform) }

	rs.doRefresh(context.Background())

	if len(fired) != 0 {
		t.Errorf("OnRecoveryNeeded fired %v on an empty jar, want none", fired)
	}
}
