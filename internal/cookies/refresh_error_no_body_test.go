package cookies

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// interceptedPageBody is what an intercepting intermediary actually answers
// with: a page of markup, and — in the shape that matters most — an echo of
// the request that reached it, credential header included.
//
// Every string in it is a fixture. The "token" is not a credential and never
// was one; it is here so an assertion can name the thing that must not appear.
const interceptedPageBody = `<html><head><title>Access denied</title></head><body>` +
	`<h1>Your organisation has blocked this request</h1>` +
	`<pre>Authorization: OAuth fixture-not-a-real-token
Cookie: SAPISID=fixture-not-a-real-credential</pre>` +
	`</body></html>`

// jarWithBothPlatformsAuth is jarWithAuth and jarWithTwitchAuth in one file, so
// a single pass can reach both checks. Both halves are the minimum each check
// requires to actually leave the process: SAPISID + LOGIN_INFO for the guide
// exchange's cookie header and SAPISIDHASH, auth-token for Twitch's bearer.
func jarWithBothPlatformsAuth(t *testing.T) *CookieJar {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cookies.txt")
	content := "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tfixture-sapisid\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tLOGIN_INFO\tfixture-login-info\n" +
		"#HttpOnly_.twitch.tv\tTRUE\t/\tTRUE\t0\tauth-token\tfixture-auth-token\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	return jar
}

// TestAuthStatusErrorsCarryNoResponseBody is the PRECONDITION for rendering
// AuthStatus.YouTubeError / TwitchError, and it is checked here rather than
// assumed at the two surfaces that now read them.
//
// Until Arc 8 Task 12a those two fields had no reader anywhere in the tree, so
// what they contained was a matter for the log level alone. They are now
// projected onto the wire by CookieStatusPayload / TwitchAuthStatusPayload and
// rendered by the TUI's R C line, which means anything either producer puts in
// them is shown to whoever can see a dashboard.
//
// THE RULE: status and cause only, never a response body. A non-200 on either
// path is exactly when an intercepting proxy, a captive portal or a service
// error page answers instead of the site — and those answers carry markup, and
// in the echo case the request's own credential header. internal/twitch's
// ValidateToken read up to 1 MB of body into its error until Task 9 of this arc
// clamped it; that function is NOT on this path (AuthStatus.TwitchError comes
// only from refresh.go's own checkTwitchAuth), but the same mistake is one edit
// away from being made here, and nothing was watching for it.
//
// Both platforms are driven in ONE pass, against servers answering with the
// same page, so a producer that starts echoing on either side fails.
func TestAuthStatusErrorsCarryNoResponseBody(t *testing.T) {
	intercepted := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(interceptedPageBody))
	}
	ytSrv := httptest.NewServer(http.HandlerFunc(intercepted))
	t.Cleanup(ytSrv.Close)
	twSrv := httptest.NewServer(http.HandlerFunc(intercepted))
	t.Cleanup(twSrv.Close)
	pointYouTubeGuideAt(t, ytSrv)
	pointTwitchValidateAt(t, twSrv)

	rs := NewRefreshService(jarWithBothPlatformsAuth(t), 0, nopLogger{})
	rs.doRefresh(context.Background())
	got := rs.GetStatus()

	// THE PREMISE. With no error recorded, every "does not contain" assertion
	// below passes against an empty string and this test proves nothing.
	if got.YouTubeError == "" || got.TwitchError == "" {
		t.Fatalf("premise lost: a 503 from both endpoints recorded no reason "+
			"(youtube=%q twitch=%q), so the checks concluded normally and there is no string "+
			"here to judge", got.YouTubeError, got.TwitchError)
	}
	// The second half of the premise: the fields have to be the INCONCLUSIVE
	// reason. If a 503 were being read as a verdict, these would be the wrong
	// strings to be reasoning about at all.
	if got.YouTubeVerification != RefreshUnknown || got.TwitchVerification != RefreshUnknown {
		t.Fatalf("premise lost: a 503 concluded something (youtube=%v twitch=%v) — a non-200 says "+
			"nothing about the credentials", got.YouTubeVerification, got.TwitchVerification)
	}

	// What the reason MAY say: the status code. Named so a producer that
	// degrades to a bare "auth check failed" — safe, but useless, and it would
	// satisfy every rule below — fails here too.
	for _, tc := range []struct{ field, value string }{
		{"YouTubeError", got.YouTubeError},
		{"TwitchError", got.TwitchError},
	} {
		if !strings.Contains(tc.value, "503") {
			t.Errorf("%s = %q — it must name the status, or rendering it tells the operator "+
				"nothing they did not already know from the verdict", tc.field, tc.value)
		}
		// What it may NOT say. Each needle is a distinct way the body leaks:
		// markup at all, the page's own words, and the echoed credential.
		for _, forbidden := range []string{"<html", "<h1", "<pre", "Access denied",
			"organisation has blocked", "Authorization:", "OAuth ", "fixture-not-a-real-token",
			"Cookie:", "fixture-not-a-real-credential"} {
			if strings.Contains(tc.value, forbidden) {
				t.Errorf("%s carries %q from the response body: %q\n\n"+
					"This field is rendered on the dashboard and in the TUI. A non-200 is exactly "+
					"when an intermediary answers instead of the site, so the body is the one "+
					"thing that must never reach it — see CookieStatusPayload, which states the "+
					"rule this test enforces.", tc.field, forbidden, tc.value)
			}
		}
	}
}
