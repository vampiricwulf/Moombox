package cookies

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// passFixture is a browser pass over a profile and a cookies.txt that hold
// the same session, differing in the two SIDCC cookies. The browser is the
// one the rollback tests use: its launch fails, the failure is swallowed and
// the profile is read.
func passFixture(t *testing.T) (*AutoCookieService, string) {
	t.Helper()
	return passFixtureWith(t, "previous-login-info")
}

// passFixtureWith is passFixture with the profile holding loginInfo as its
// LOGIN_INFO: any other value than the file has makes the merged file another
// session.
func passFixtureWith(t *testing.T, loginInfo string) (*AutoCookieService, string) {
	t.Helper()
	profileDir := writeWALCookieProfile(t, []profileTestCookie{
		{name: "SAPISID", value: "previous-sapisid", host: ".youtube.com", path: "/", secure: true},
		{name: "LOGIN_INFO", value: loginInfo, host: ".youtube.com", path: "/", httpOnly: true, secure: true},
		{name: "__Secure-1PSIDCC", value: "1p-profile", host: ".youtube.com", path: "/", secure: true},
		{name: "__Secure-3PSIDCC", value: "3p-profile", host: ".youtube.com", path: "/", secure: true},
	})
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	seed := youtubeOnlyCookieFile +
		".youtube.com\tTRUE\t/\tTRUE\t0\t__Secure-1PSIDCC\t1p-file\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\t__Secure-3PSIDCC\t3p-file\n"
	if err := os.WriteFile(cookiePath, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	t.Cleanup(s.jar.FlushRenewals) // no flush timer writes into a removed TempDir
	s.detectBrowser = func() *DetectedBrowser {
		return &DetectedBrowser{Type: "firefox", Path: "moombox-no-such-browser", Name: "Firefox"}
	}
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
	if err := s.jar.Load(cookiePath); err != nil {
		t.Fatal(err)
	}
	return s, cookiePath
}

func offerRenewal(t *testing.T, jar *CookieJar, name, value string) {
	t.Helper()
	jar.OfferYouTubeReply(replyTo(t, "https://www.youtube.com/x", jar.GetCookieHeader(), 200,
		name+"="+value+"; Domain=.youtube.com; Path=/; Max-Age=31536000"))
}

// KA-2. A browser pass's merge lets the profile win by name, as it always has,
// except for a name YouTube renewed on a reply after the pass began: there
// the value Go was sent stays, in the jar and in the file.
func TestBrowserPassDoesNotUndoARenewalReceivedDuringIt(t *testing.T) {
	s, cookiePath := passFixture(t)
	offerRenewal(t, s.jar, "__Secure-3PSIDCC", "3p-go-before-the-pass")

	calls := 0
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) {
		if calls++; calls == 1 {
			// The pre-write check: the pass is under way.
			offerRenewal(t, s.jar, "__Secure-1PSIDCC", "1p-go-during-the-pass")
		}
		return true, nil
	}
	if _, err := s.RefreshCookies(context.Background()); err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}

	if got := s.jar.GetCookieFor(PlatformYouTube, "__Secure-1PSIDCC"); got != "1p-go-during-the-pass" {
		t.Errorf("jar __Secure-1PSIDCC = %q: the pass put the session back on an older value", got)
	}
	if got := s.jar.GetCookieFor(PlatformYouTube, "__Secure-3PSIDCC"); got != "3p-profile" {
		t.Errorf("jar __Secure-3PSIDCC = %q, want the profile's: a renewal from before the pass does not outrank it", got)
	}
	s.jar.FlushRenewals()
	got := fileText(t, cookiePath)
	if !strings.Contains(got, "\t__Secure-1PSIDCC\t1p-go-during-the-pass") || !strings.Contains(got, "\t__Secure-3PSIDCC\t3p-profile") {
		t.Errorf("cookies.txt after the pass:\n%s", got)
	}
}

// KA-2. The rollback of a pass that regressed YouTube restores the previous
// rows, and must not restore a value older than one YouTube has sent since
// the pass read them.
func TestRollbackDoesNotRestoreAValueOlderThanARenewal(t *testing.T) {
	s, cookiePath := passFixture(t)

	calls := 0
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) {
		if calls++; calls == 1 {
			offerRenewal(t, s.jar, "__Secure-1PSIDCC", "1p-go-during-the-pass")
			return true, nil
		}
		return false, nil // the merged cookies did not hold up
	}
	if _, err := s.RefreshCookies(context.Background()); err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}

	if got := s.jar.GetCookieFor(PlatformYouTube, "__Secure-3PSIDCC"); got != "3p-file" {
		t.Fatalf("jar __Secure-3PSIDCC = %q: the rollback this test is about did not happen", got)
	}
	if got := s.jar.GetCookieFor(PlatformYouTube, "__Secure-1PSIDCC"); got != "1p-go-during-the-pass" {
		t.Errorf("jar __Secure-1PSIDCC = %q after the rollback, want the value YouTube sent during the pass", got)
	}
	s.jar.FlushRenewals()
	got := fileText(t, cookiePath)
	if !strings.Contains(got, "\t__Secure-1PSIDCC\t1p-go-during-the-pass") || !strings.Contains(got, "\t__Secure-3PSIDCC\t3p-file") {
		t.Errorf("cookies.txt after the rollback:\n%s", got)
	}
}

// KA-2, the case the same-session test above does not reach: the merge brings a
// different LOGIN_INFO in from the profile, so for the length of the verify the
// file holds another session. The renewal kept during the pass must wait that
// out and be back, in the jar and the file, once the pass has rolled back.
//
// Mutation: let overlayRenewalsLocked drop the waiting renewals on a session
// mismatch while a writer holds the flush.
func TestRollbackFromAnotherSessionKeepsARenewalFromDuringThePass(t *testing.T) {
	s, cookiePath := passFixtureWith(t, "another-login-info")

	calls := 0
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) {
		if calls++; calls == 1 {
			offerRenewal(t, s.jar, "__Secure-1PSIDCC", "1p-go-during-the-pass")
			return true, nil
		}
		return false, nil // the merged cookies did not hold up
	}
	if _, err := s.RefreshCookies(context.Background()); err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}

	if got := s.jar.GetCookieFor(PlatformYouTube, "LOGIN_INFO"); got != "previous-login-info" {
		t.Fatalf("jar LOGIN_INFO = %q: the rollback this test is about did not happen", got)
	}
	if got := s.jar.GetCookieFor(PlatformYouTube, "__Secure-1PSIDCC"); got != "1p-go-during-the-pass" {
		t.Errorf("jar __Secure-1PSIDCC = %q after the rollback, want the value YouTube sent during the pass", got)
	}
	s.jar.FlushRenewals()
	got := fileText(t, cookiePath)
	if !strings.Contains(got, "\t__Secure-1PSIDCC\t1p-go-during-the-pass") || !strings.Contains(got, "\t__Secure-3PSIDCC\t3p-file") {
		t.Errorf("cookies.txt after the rollback:\n%s", got)
	}
}

// The per-pass census line also says what the replies renewed since the pass
// before, by endpoint and client, and how long ago the last one was. The set
// starts over at each pass, so a name that stopped being renewed is missing
// from the next line. Names only.
//
// Mutation: leave TakeReplyRenewals out of recordCensus.
func TestCensusLineCarriesWhatTheRepliesRenewed(t *testing.T) {
	s, _ := passFixture(t)
	logger := &levelTaggedLogger{}
	s.logger = logger
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	offerRenewal(t, s.jar, "__Secure-3PSIDCC", "3p-go-before-the-pass")

	for i, want := range []string{
		"reply_renewed_since_last_pass=membership page (web page): __Secure-3PSIDCC reply_renewed_age=",
		"reply_renewed_since_last_pass=none reply_renewed_age=",
	} {
		if _, err := s.RefreshCookies(context.Background()); err != nil {
			t.Fatalf("pass %d: %v", i+1, err)
		}
		lines := censusLines(logger)
		if len(lines) != i+1 {
			t.Fatalf("after pass %d there are %d census lines, want %d: %v", i+1, len(lines), i+1, lines)
		}
		if !strings.Contains(lines[i], want) {
			t.Errorf("pass %d census line = %q\nwant it to contain %q", i+1, lines[i], want)
		}
		if strings.Contains(lines[i], "3p-go-before-the-pass") {
			t.Errorf("pass %d census line carries a cookie value: %q", i+1, lines[i])
		}
	}
}
