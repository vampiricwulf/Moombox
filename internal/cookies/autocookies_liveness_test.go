package cookies

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tests in this file all cover one theme: the autocookies subsystem used
// to answer "is the credential set COMPLETE" wherever the question was "was
// this platform ever configured". A jar holding SAPISID with LOGIN_INFO
// cleared — YouTube's own rotation-invalidation state, and equally what an
// exporter that drops HttpOnly rows leaves behind — reads as never-configured
// under the complete-set predicate, so the refresh declined to visit it, the
// verification path returned a verdict it never obtained, and the import
// rollback could not see the session it was overwriting.

// halfClearedYouTubeCookieFile is a cookies.txt that HasAnyYouTubeAuthCookie
// accepts and HasYouTubeAuthCookies rejects: a configured YouTube session with
// its LOGIN_INFO gone.
const halfClearedYouTubeCookieFile = "# Netscape HTTP Cookie File\n" +
	".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tworking-sapisid\n"

// sapisidOnlyRows is the same shape inside a browser profile: the import finds
// a real Google session but no LOGIN_INFO row to go with it.
func sapisidOnlyRows() []profileTestCookie {
	return []profileTestCookie{
		{name: "SAPISID", value: "sapisid-from-profile", host: ".youtube.com", path: "/", secure: true},
		{name: "__Secure-3PAPISID", value: "3papisid-from-profile", host: ".youtube.com", path: "/", secure: true},
	}
}

// TestRefreshPlatformsCountsAConfiguredPlatformWithAnIncompleteSet is site 1 at
// unit scale. refreshPlatforms drives the RefreshCookiesDetailed gate, the
// Firefox launch loop and the Chromium navigation loop, so a platform missing
// from it is never visited by anything.
func TestRefreshPlatformsCountsAConfiguredPlatformWithAnIncompleteSet(t *testing.T) {
	cases := []struct {
		name string
		file string
		want []string
	}{
		{
			name: "youtube session with LOGIN_INFO cleared",
			file: halfClearedYouTubeCookieFile,
			want: []string{"youtube"},
		},
		{
			name: "twitch session whose auth-token was dropped",
			file: "# Netscape HTTP Cookie File\n" +
				".twitch.tv\tTRUE\t/\tFALSE\t0\ttwilight-user\t%7B%22id%22%3A%221%22%7D\n",
			want: []string{"twitch"},
		},
		{
			name: "no credential of any kind is still nothing to refresh",
			file: "# Netscape HTTP Cookie File\n" +
				".youtube.com\tTRUE\t/\tFALSE\t0\tPREF\tf6=40000000\n",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
			if err := os.WriteFile(cookiePath, []byte(tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
			jar := NewCookieJar()
			if err := jar.Load(cookiePath); err != nil {
				t.Fatal(err)
			}
			s := NewAutoCookieService(t.TempDir(), cookiePath, jar, nopAutoCookieLogger{})

			got := s.refreshPlatforms()
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("refreshPlatforms() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRefreshDoesNotDeclineTheRecoveryItWasCalledToPerform is site 1 end to
// end, and it is the chain the review reconstructed: doRefresh fires
// OnRecoveryNeeded("youtube") for a half-cleared jar, monitor_callbacks runs
// runCookieRecovery, and RefreshCookiesDetailed found refreshPlatforms() empty
// and returned refreshDeclined() — a RefreshUnknown verdict, which the
// operator reads as "Cookie Auto-Refresh Ineffective … it either declined to
// run or found nothing usable" about the one platform the pass existed to fix.
func TestRefreshDoesNotDeclineTheRecoveryItWasCalledToPerform(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(halfClearedYouTubeCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	// A browser that cannot launch: refreshFirefox swallows the launch failure
	// and reads the profile, which is the browser path without a real browser.
	s.detectBrowser = func() *DetectedBrowser {
		return &DetectedBrowser{Type: "firefox", Path: "moombox-no-such-browser", Name: "Firefox"}
	}
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
	// Production loads the jar from cookies.txt at startup and the gate reads
	// it, so the test must too.
	if err := s.jar.Load(cookiePath); err != nil {
		t.Fatal(err)
	}
	if s.jar.HasYouTubeAuthCookies() {
		t.Fatal("fixture is broken — this test needs a jar the complete-set predicate rejects")
	}

	result, err := s.RefreshCookiesDetailed(context.Background())
	if err != nil {
		t.Fatalf("RefreshCookiesDetailed: %v", err)
	}
	if !result.Ran {
		t.Fatal("the refresh declined to run for the very platform recovery named")
	}
	if got := result.Verdict("youtube"); got != RefreshOK {
		t.Errorf("Verdict(youtube) = %v, want RefreshOK — the browser visited the platform and the check passed", got)
	}
	data, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "login-info-from-profile") {
		t.Errorf("the refresh never re-fetched the missing credential:\n%s", data)
	}
}

// TestImportReportsTheCredentialsItPlainlyHolds is site 2. checkPlatformAuth
// mapped an incomplete set to verifyFailed WITHOUT calling the verify callback
// at all, so RefreshResult.YouTubeStored came out false for a cookies.txt that
// visibly holds SAPISID — and monitor_callbacks then told the operator
// "Moombox now holds no youtube cookies at all", flatly contradicting
// AuthStatus.HasYouTubeCookies on the dashboard at the same instant.
func TestImportReportsTheCredentialsItPlainlyHolds(t *testing.T) {
	profileDir := writeWALCookieProfile(t, sapisidOnlyRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")

	verifyCalls := 0
	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil } // container: no browser
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) {
		verifyCalls++
		return true, nil
	}
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }

	result, err := s.RefreshCookiesDetailed(context.Background())
	if err != nil {
		t.Fatalf("RefreshCookiesDetailed: %v", err)
	}

	// The premise: the file on disk really does hold a Google credential.
	data, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "sapisid-from-profile") {
		t.Fatalf("fixture is broken — the import was supposed to write SAPISID:\n%s", data)
	}

	if verifyCalls == 0 {
		t.Error("a verdict was reported for YouTube without the check ever being made")
	}
	if !result.HasCredentials("youtube") {
		t.Error("YouTubeStored = false while cookies.txt holds SAPISID — the notification built on this says Moombox holds no youtube cookies at all")
	}
	if got := result.Verdict("youtube"); got != RefreshOK {
		t.Errorf("Verdict(youtube) = %v, want RefreshOK — the wired callback said the session is alive", got)
	}
}

// TestHalfClearedWorkingSessionIsNotSilentlyOverwritten is the data-loss
// corollary, and the highest-stakes case in this file.
//
// A working-but-incomplete YouTube session entered platformsToRestore as
// {hasCookies:false, state:verifyFailed}: not ok(), so the REGRESSION arm could
// not fire, and no hasCookies, so the INCONCLUSIVE arm could not either. A
// stale mounted profile was therefore committed straight over a credential that
// worked, mergeCookieFiles let the imported value win by name, and the startup
// one-shot repeated it on every container restart.
//
// It now enters as {true, verifyOK} and the dead import trips the REGRESSION
// arm.
func TestHalfClearedWorkingSessionIsNotSilentlyOverwritten(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(halfClearedYouTubeCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	// Only the credential already on disk works — an incomplete set that
	// YouTube still honours. Answers from the jar's live value so it says yes
	// before the import and no after it: a regression, not a flat failure.
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) {
		return s.jar.GetSapisid() == "working-sapisid", nil
	}
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }

	ok, err := s.RefreshCookies(context.Background())
	if err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}

	// The data-loss assertion comes first and is not fatal, so a run that also
	// gets the return value wrong still reports what happened to the file.
	data, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "working-sapisid") {
		t.Errorf("a working session was destroyed by a stale import because its cookie set was incomplete:\n%s", got)
	}
	if strings.Contains(got, "sapisid-from-profile") {
		t.Errorf("the rejected import survived the rollback:\n%s", got)
	}
	if !ok {
		t.Error("RefreshCookies = false; the working credential was restored, so the end state is healthy")
	}
	if s.jar.GetSapisid() != "working-sapisid" {
		t.Errorf("jar holds %q after the rollback, want the restored credential", s.jar.GetSapisid())
	}
	status := s.GetStatus()
	if status.NeedsManualRelogin["youtube"] {
		t.Error("the restored session re-verified; telling the user to sign in again is wrong")
	}
	if status.LastError != nil {
		t.Errorf("a successful rollback should not leave an error: %q", *status.LastError)
	}
}

// TestHalfClearedDeadSessionStillLetsTheImportThrough is the control on the
// test above: the rollback must protect a session that WORKS, not merely one
// that is incomplete. Nothing here is worth keeping, so the fresher set is the
// better guess for the next attempt and must be committed.
//
// Passes before and after the change — it exists to pin that the new pre-import
// verdict did not turn every partial cookies.txt into a wall that legitimate
// imports cannot get past.
func TestHalfClearedDeadSessionStillLetsTheImportThrough(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(
		"# Netscape HTTP Cookie File\n"+
			".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tdead-sapisid\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) {
		return s.jar.GetSapisid() == "sapisid-from-profile", nil
	}
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }

	ok, err := s.RefreshCookies(context.Background())
	if err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}
	if !ok {
		t.Fatal("RefreshCookies = false although the imported credentials verified")
	}
	data, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "sapisid-from-profile") {
		t.Errorf("a working import was rolled back over a dead partial set:\n%s", data)
	}
}
