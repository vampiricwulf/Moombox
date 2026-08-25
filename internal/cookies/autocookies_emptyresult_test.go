package cookies

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pastExpiry is a unix timestamp comfortably in the past (2001-09-09), so
// mergeCookieFiles prunes any row carrying it.
const pastExpiry int64 = 1000000000

// expiredYouTubeAuthRows is a signed-in YouTube profile whose credentials
// have all expired. CookieJar.Load ignores the expiry field, so a jar built
// from these still reports HasYouTubeAuthCookies — which is exactly the
// disagreement this test exists for.
func expiredYouTubeAuthRows() []profileTestCookie {
	return []profileTestCookie{
		{name: "SAPISID", value: "sapisid-from-profile", host: ".youtube.com", path: "/", expiry: pastExpiry, secure: true},
		{name: "LOGIN_INFO", value: "login-info-from-profile", host: ".youtube.com", path: "/", expiry: pastExpiry, httpOnly: true, secure: true},
	}
}

// expiredPreviousCookieFile is a cookies.txt whose every row is expired.
const expiredPreviousCookieFile = "# Netscape HTTP Cookie File\n" +
	".youtube.com\tTRUE\t/\tTRUE\t1000000000\tSAPISID\tprevious-sapisid\n" +
	"#HttpOnly_.youtube.com\tTRUE\t/\tTRUE\t1000000000\tLOGIN_INFO\tprevious-login-info\n"

// TestRefreshLeavingNoAuthCookiesIsStatedNotCleared is the last silent-empty
// path in the package.
//
// refreshPlatforms() reads the JAR, which ignores expiry; mergeCookieFiles
// prunes ON expiry. So a refresh can pass the "there are cookies worth
// refreshing" gate, merge, and have every single row pruned — leaving an
// EMPTY cookies.txt on disk. The jar then has no auth cookies for either
// platform, `failed` comes out empty, and the "nothing to verify yet" branch
// fires: lastError is CLEARED and the call returns (false, nil).
//
// The user is left with an empty credential file, a status that says
// everything is fine, and no way to find out what happened.
func TestRefreshLeavingNoAuthCookiesIsStatedNotCleared(t *testing.T) {
	profileDir := writeWALCookieProfile(t, expiredYouTubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(expiredPreviousCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	// Dead before AND after, so nothing is rolled back — this is the
	// empty-after-write case on its own, not a rollback in disguise.
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return false, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
	// Production loads the jar from cookies.txt at startup; the refresh gate
	// reads it, so the test must too.
	if err := s.jar.Load(cookiePath); err != nil {
		t.Fatal(err)
	}
	if !s.jar.HasYouTubeAuthCookies() {
		t.Fatal("fixture is broken — the jar must believe it holds YouTube auth before the refresh")
	}

	ok, err := s.RefreshCookies(context.Background())
	if err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}
	if ok {
		t.Fatal("RefreshCookies = true although nothing verified")
	}

	// Establish that the silent case actually happened: the file on disk
	// carries no credentials at all.
	data, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatal(err)
	}
	if countNetscapeCookieRows(string(data)) != 0 {
		t.Fatalf("fixture is broken — the merge was supposed to prune every row:\n%s", data)
	}
	if s.jar.HasYouTubeAuthCookies() || s.jar.HasTwitchAuthCookies() {
		t.Fatal("fixture is broken — the reloaded jar was supposed to be empty")
	}

	status := s.GetStatus()
	if status.LastError == nil {
		t.Fatal("the refresh emptied cookies.txt and said nothing — an empty credential set on disk must be stated")
	}
	if !strings.Contains(*status.LastError, "YouTube") {
		t.Errorf("status must name the platform whose credentials are gone, got %q", *status.LastError)
	}
}

// TestRefreshWithNothingToDoStaysQuiet is the guard on the other side. The
// branch above must not turn a legitimate no-op into a false alarm: when
// there were no credentials to begin with and nothing was written, there is
// nothing to complain about.
func TestRefreshWithNothingToDoStaysQuiet(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	// A browser is installed, so this is the browser path — and the jar is
	// empty, so refreshPlatforms() has nothing to refresh and the call
	// returns before touching cookies.txt.
	s.detectBrowser = func() *DetectedBrowser {
		return &DetectedBrowser{Type: "firefox", Path: "moombox-no-such-browser", Name: "Firefox"}
	}
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return false, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }

	ok, err := s.RefreshCookies(context.Background())
	if err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}
	if ok {
		t.Fatal("RefreshCookies = true although there was nothing to refresh")
	}
	if status := s.GetStatus(); status.LastError != nil {
		t.Errorf("a refresh with nothing to do must not raise an error: %q", *status.LastError)
	}
	if _, err := os.Stat(cookiePath); !os.IsNotExist(err) {
		t.Error("a refresh with nothing to do must not create a cookies.txt")
	}
}

// TestImportOfSignedOutProfileIsStated covers the same invariant from the
// other direction: the import SUCCEEDED and wrote a file, but what it wrote
// authenticates nothing. Nothing was lost, so this is not a regression — but
// a container operator who mounted a profile and got a credential-free
// cookies.txt has to be told, or the mount looks like it worked.
func TestImportOfSignedOutProfileIsStated(t *testing.T) {
	// PREF is an "essential" cookie (it is preserved) but it is not an auth
	// cookie, so the import returns rows while authenticating nothing.
	profileDir := writeWALCookieProfile(t, []profileTestCookie{
		{name: "PREF", value: "f6=40000000", host: ".youtube.com", path: "/"},
	})
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return false, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }

	ok, err := s.RefreshCookies(context.Background())
	if err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}
	if ok {
		t.Fatal("RefreshCookies = true although nothing verified")
	}

	data, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatalf("the import should still have written what it found: %v", err)
	}
	if !strings.Contains(string(data), "PREF") {
		t.Errorf("the imported rows were not written:\n%s", data)
	}

	status := s.GetStatus()
	if status.LastError == nil {
		t.Fatal("an import that produced no auth cookies must say so")
	}
	if !strings.Contains(*status.LastError, "signed in") {
		t.Errorf("status should point at the profile not being signed in, got %q", *status.LastError)
	}
}
