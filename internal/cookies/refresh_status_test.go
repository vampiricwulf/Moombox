package cookies

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// TestGuideNon200IsInconclusive: a 429 or 503 must NOT be reported as
// "conclusively not authenticated". shouldFireRecovery keys on checkErr ==
// nil, so returning (false, nil) here makes a rate-limit look like dead
// credentials — and once G6 unblocks the notification, an alarm.
func TestGuideNon200IsInconclusive(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		// Built BEFORE the override: jarWithAuth can t.Fatal, and a Fatal
		// between the assignment and the restore below would leave the
		// package pointed at a closed server for every later test.
		rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
		origRefresh, origPlain := youtubeGuideRefreshURL, youtubeGuideURL
		youtubeGuideRefreshURL, youtubeGuideURL = srv.URL, srv.URL

		auth, err := rs.checkAndRefreshYouTube(context.Background())

		youtubeGuideRefreshURL, youtubeGuideURL = origRefresh, origPlain
		srv.Close()

		if auth {
			t.Errorf("status %d: authenticated = true, want false", code)
		}
		if err == nil {
			t.Errorf("status %d: err = nil, want non-nil — a non-200 is not an auth verdict", code)
		}
	}
}

// TestGuide200LoggedOutIsConclusive: a real 200 that says logged_in=0 IS a
// conclusive auth failure and must keep returning a nil error.
func TestGuide200LoggedOutIsConclusive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"responseContext":{"mainAppWebResponseContext":{"loggedIn":false}}}`))
	}))
	defer srv.Close()
	origRefresh := youtubeGuideRefreshURL
	youtubeGuideRefreshURL = srv.URL
	defer func() { youtubeGuideRefreshURL = origRefresh }()

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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	origPlain := youtubeGuideURL
	youtubeGuideURL = srv.URL
	t.Cleanup(func() { youtubeGuideURL = origPlain })

	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	auth, err := rs.CheckYouTubeAuth(context.Background())
	if auth {
		t.Error("authenticated = true, want false")
	}
	if err == nil {
		t.Error("err = nil, want non-nil — a 429 is not an auth verdict")
	}
}
