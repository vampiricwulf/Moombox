package cookies

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// goodTwitchToken / staleTwitchToken model a Twitch credential that works and
// one that does not. The verify stubs below key off the jar's live value, so
// a check answers differently before and after an import — which is the only
// way to exercise a REGRESSION rather than a flat failure.
const (
	goodTwitchToken  = "good-twitch-token"
	staleTwitchToken = "stale-twitch-token"
)

func youtubeAndTwitchRows(twitchToken string) []profileTestCookie {
	rows := youtubeAuthRows()
	return append(rows,
		profileTestCookie{name: "auth-token", value: twitchToken, host: ".twitch.tv", path: "/", httpOnly: true, secure: true},
		profileTestCookie{name: "login", value: "someuser", host: ".twitch.tv", path: "/"},
	)
}

// previousCookieFile is a healthy cookies.txt: YouTube AND Twitch both work.
const previousCookieFile = "# Netscape HTTP Cookie File\n" +
	".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tprevious-sapisid\n" +
	"#HttpOnly_.youtube.com\tTRUE\t/\tTRUE\t0\tLOGIN_INFO\tprevious-login-info\n" +
	"#HttpOnly_.twitch.tv\tTRUE\t/\tTRUE\t0\tauth-token\t" + goodTwitchToken + "\n" +
	".twitch.tv\tTRUE\t/\tFALSE\t0\tlogin\tsomeuser\n"

// youtubeOnlyCookieFile is the same idea with one platform. The browser-path
// tests use it deliberately: refreshFirefox sleeps firefoxLaunchSpacing (5s)
// BETWEEN platform launches, so a two-platform fixture would buy nothing but
// ten seconds of test time.
const youtubeOnlyCookieFile = "# Netscape HTTP Cookie File\n" +
	".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tprevious-sapisid\n" +
	"#HttpOnly_.youtube.com\tTRUE\t/\tTRUE\t0\tLOGIN_INFO\tprevious-login-info\n"

// TestRefreshCookiesRestoresOnlyTheRegressedPlatform is the exact failure the
// reviewer constructed: a container with a healthy cookies.txt (YouTube AND
// Twitch working) and a mounted profile whose Twitch auth-token is dead.
//
// mergeCookieFiles lets the imported value win by name+domain+path, so without a
// per-platform rollback the good Twitch token is overwritten, ytAuth=true
// suppresses any total-failure rollback, and RefreshCookies reports success
// while Twitch is now broken AND the working credential is gone from disk.
// The startup one-shot then repeats this on every container restart.
func TestRefreshCookiesRestoresOnlyTheRegressedPlatform(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAndTwitchRows(staleTwitchToken))
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(previousCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	// Answers from the jar's live value, so it says "yes" before the import
	// and "no" after it — a genuine regression, not a flat failure.
	s.VerifyTwitchAuth = func(context.Context) (bool, error) {
		return s.jar.GetTwitchAuthToken() == goodTwitchToken, nil
	}

	ok, err := s.RefreshCookies(context.Background())
	if err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}
	if !ok {
		t.Fatal("RefreshCookies = false; YouTube improved and Twitch was restored, so this is a success")
	}

	data, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)

	// Twitch must be back to the credential that actually works.
	if !strings.Contains(got, goodTwitchToken) {
		t.Errorf("the working Twitch token was clobbered by the stale profile:\n%s", got)
	}
	if strings.Contains(got, staleTwitchToken) {
		t.Errorf("the stale Twitch token survived the rollback:\n%s", got)
	}
	// YouTube verified after the import, so the imported values are kept —
	// the rollback is per platform, not all-or-nothing.
	if !strings.Contains(got, "login-info-from-profile") {
		t.Errorf("YouTube import was rolled back even though it verified:\n%s", got)
	}
	if s.jar.GetTwitchAuthToken() != goodTwitchToken {
		t.Errorf("jar holds %q after rollback, want the restored token", s.jar.GetTwitchAuthToken())
	}
}

// TestRollbackDoesNotLeaveMisleadingStatus covers finding 4: after a restore,
// ytAuth/twAuth used to still describe the DISCARDED merged jar, so a
// rollback always ended with needsRelogin=true and "manual re-login required"
// describing cookies that were restored and never re-verified — an
// instruction that is impossible to follow in a container anyway.
func TestRollbackDoesNotLeaveMisleadingStatus(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAndTwitchRows(staleTwitchToken))
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(previousCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) {
		return s.jar.GetTwitchAuthToken() == goodTwitchToken, nil
	}

	if _, err := s.RefreshCookies(context.Background()); err != nil {
		t.Fatal(err)
	}

	status := s.GetStatus()
	if status.NeedsManualRelogin["twitch"] {
		t.Error("Twitch was restored and re-verified OK; flagging manual re-login is wrong")
	}
	if status.NeedsManualRelogin["youtube"] {
		t.Error("YouTube verified; flagging manual re-login is wrong")
	}
	if status.LastError != nil {
		t.Errorf("a successful per-platform rollback should not leave an error: %q", *status.LastError)
	}
}

// TestRefreshCookiesKeepsImportWhenNothingWasEverGood pins the other half of
// the rule. "Do not clobber a GOOD cookies.txt" says nothing about a file
// that was already dead: replacing dead cookies with other dead cookies costs
// the user nothing, and keeping the fresher set is the better guess.
func TestRefreshCookiesKeepsImportWhenNothingWasEverGood(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(previousCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return false, nil } // dead before AND after
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
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "login-info-from-profile") {
		t.Errorf("import was rolled back over a cookies.txt that never verified:\n%s", data)
	}
}

// TestRefreshCookiesDoesNotCommitOnInconclusiveVerification covers the outage
// case: if the post-import check cannot reach the network we have learned
// nothing, so a platform that previously held credentials must keep them
// rather than have them replaced by an unevaluated set.
func TestRefreshCookiesDoesNotCommitOnInconclusiveVerification(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAndTwitchRows(staleTwitchToken))
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(previousCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	offline := errors.New("dial tcp: no such host")
	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return false, offline }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, offline }

	if _, err := s.RefreshCookies(context.Background()); err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}
	got, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatal(err)
	}
	// The rollback restores ROWS, not the original bytes, so the file is
	// rewritten with our standard header — compare the credentials, not the
	// framing.
	if !strings.Contains(string(got), goodTwitchToken) || !strings.Contains(string(got), "previous-sapisid") {
		t.Fatalf("an unverifiable import overwrote the previous credentials:\n%s", got)
	}
	if strings.Contains(string(got), staleTwitchToken) || strings.Contains(string(got), "sapisid-from-profile") {
		t.Fatalf("unevaluated imported credentials were committed:\n%s", got)
	}
	status := s.GetStatus()
	if status.NeedsManualRelogin["youtube"] || status.NeedsManualRelogin["twitch"] {
		t.Error("a network failure must not tell the user to re-login")
	}
	// A rollback and an inconclusive check are BOTH true here — a check that
	// cannot complete is itself why the previous credentials were kept. The
	// status must not blame the profile for that: sending an operator off to
	// re-export a mount that is perfectly fine is the exact misattribution
	// verifyUnknown was added to prevent.
	if status.LastError == nil {
		t.Fatal("an import that was not committed must leave an explanation")
	}
	if !strings.Contains(*status.LastError, "did not complete") {
		t.Errorf("status must name the incomplete check as the cause, got %q", *status.LastError)
	}
	if strings.Contains(*status.LastError, "did not verify") {
		t.Errorf("status blames the profile for a network failure: %q", *status.LastError)
	}
}

// TestImportIsNotCommittedWhenTheRealCheckIsRateLimited wires the REAL
// verification callback instead of a stub, because the stubs above cannot see
// the cross-subsystem consequence of making a non-200 guide response
// inconclusive.
//
// Production wires cmd/moombox's AutoCookieService.VerifyYouTubeAuth to
// RefreshService.CheckYouTubeAuth. While a non-200 returned (false, nil) that
// callback reported a YouTube 429 as a CONCLUSIVE failure, so
// checkPlatformAuth recorded verifyFailed on both sides of the import,
// platformsToRestore found no reason to roll back, and an unevaluated profile
// was committed over credentials that may well have been fine — then the
// operator was told "manual re-login required" on the strength of a rate
// limit. With the non-200 inconclusive, both checks land on verifyUnknown, the
// previous rows are restored, and the status names the incomplete check.
func TestImportIsNotCommittedWhenTheRealCheckIsRateLimited(t *testing.T) {
	pointYouTubeGuideAt(t, statusServer(t, http.StatusTooManyRequests))

	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(youtubeOnlyCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	// The real check, over the same jar the service reloads — exactly the
	// shape cmd/moombox/services.go builds.
	s.VerifyYouTubeAuth = NewRefreshService(s.jar, 0, nopLogger{}).CheckYouTubeAuth

	if _, err := s.RefreshCookies(context.Background()); err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}

	got, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "previous-login-info") {
		t.Errorf("a rate-limited check let an unevaluated import overwrite the previous credentials:\n%s", got)
	}
	if strings.Contains(string(got), "login-info-from-profile") {
		t.Errorf("imported credentials were committed on the strength of a 429:\n%s", got)
	}

	status := s.GetStatus()
	if status.NeedsManualRelogin["youtube"] {
		t.Error("a 429 must not tell the user their YouTube login is dead")
	}
	if status.LastError == nil {
		t.Fatal("an import that was not committed must leave an explanation")
	}
	if !strings.Contains(*status.LastError, "did not complete") {
		t.Errorf("status must name the incomplete check as the cause, got %q", *status.LastError)
	}
}

// twitchOnlyRows / twitchOnlyCookieFile keep the Twitch rollback test to a
// single platform. A YouTube row on either side would drag the YouTube verdict
// into the same call, and a platform that verifies routes RefreshCookies down
// the success branch where the "did not complete" message is never built.
func twitchOnlyRows(token string) []profileTestCookie {
	return []profileTestCookie{
		{name: "auth-token", value: token, host: ".twitch.tv", path: "/", httpOnly: true, secure: true},
		{name: "login", value: "someuser", host: ".twitch.tv", path: "/"},
	}
}

const twitchOnlyCookieFile = "# Netscape HTTP Cookie File\n" +
	"#HttpOnly_.twitch.tv\tTRUE\t/\tTRUE\t0\tauth-token\t" + goodTwitchToken + "\n" +
	".twitch.tv\tTRUE\t/\tFALSE\t0\tlogin\tsomeuser\n"

// TestImportIsNotCommittedWhenTheRealTwitchCheckIsRateLimited is the Twitch
// mirror of TestImportIsNotCommittedWhenTheRealCheckIsRateLimited, and it
// pins the same data-loss scenario on the other platform.
//
// checkPlatformAuth runs both platforms through one three-state mapping
// (autocookies_profile.go:571-572), so while checkTwitchAuth answered every
// non-200 with (false, nil) an id.twitch.tv rate limit was recorded as a
// CONCLUSIVE failure on both sides of the import. platformsToRestore then saw
// neither a regression nor an unknown, a stale profile token was committed
// over a working one, and the operator was told to re-login on the strength of
// a 429.
func TestImportIsNotCommittedWhenTheRealTwitchCheckIsRateLimited(t *testing.T) {
	pointTwitchValidateAt(t, statusServer(t, http.StatusTooManyRequests))

	profileDir := writeWALCookieProfile(t, twitchOnlyRows(staleTwitchToken))
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(twitchOnlyCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	// The real check over the service's own jar — the shape
	// cmd/moombox/services.go:602 builds.
	s.VerifyTwitchAuth = NewRefreshService(s.jar, 0, nopLogger{}).CheckTwitchAuth

	if _, err := s.RefreshCookies(context.Background()); err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}

	got, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), goodTwitchToken) {
		t.Errorf("a rate-limited check let an unevaluated import overwrite the working Twitch token:\n%s", got)
	}
	if strings.Contains(string(got), staleTwitchToken) {
		t.Errorf("the stale profile token was committed on the strength of a 429:\n%s", got)
	}
	if s.jar.GetTwitchAuthToken() != goodTwitchToken {
		t.Errorf("jar holds %q after rollback, want the restored token", s.jar.GetTwitchAuthToken())
	}

	status := s.GetStatus()
	if status.NeedsManualRelogin["twitch"] {
		t.Error("a 429 must not tell the user their Twitch token is dead")
	}
	if status.LastError == nil {
		t.Fatal("an import that was not committed must leave an explanation")
	}
	if !strings.Contains(*status.LastError, "did not complete") {
		t.Errorf("status must name the incomplete check as the cause, got %q", *status.LastError)
	}
}

// TestImportIsCommittedWhenTwitchConclusivelyRejectsTheToken is the control
// that keeps the rule above from swallowing the case it must not. A 401 is
// Twitch's documented invalid-token verdict, so both sides of the import are
// conclusively dead — nothing is worth protecting, the fresher set is the
// better guess for the next attempt, and the re-login prompt is correct.
func TestImportIsCommittedWhenTwitchConclusivelyRejectsTheToken(t *testing.T) {
	pointTwitchValidateAt(t, statusServer(t, http.StatusUnauthorized))

	profileDir := writeWALCookieProfile(t, twitchOnlyRows(staleTwitchToken))
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(twitchOnlyCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	s.VerifyTwitchAuth = NewRefreshService(s.jar, 0, nopLogger{}).CheckTwitchAuth

	if _, err := s.RefreshCookies(context.Background()); err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}

	got, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), staleTwitchToken) {
		t.Errorf("a conclusively-dead credential was protected from replacement:\n%s", got)
	}
	status := s.GetStatus()
	if !status.NeedsManualRelogin["twitch"] {
		t.Error("401 is a real verdict — the user must still be told to sign in again")
	}
	if status.LastError == nil || !strings.Contains(*status.LastError, "manual re-login required") {
		t.Errorf("a conclusive rejection must keep saying so, got %v", status.LastError)
	}
}

// TestRestorePlatformRows unit-tests the row surgery the rollback depends on.
func TestRestorePlatformRows(t *testing.T) {
	merged := "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tnew-yt\n" +
		"#HttpOnly_.twitch.tv\tTRUE\t/\tTRUE\t0\tauth-token\tnew-tw\n" +
		".example.com\tTRUE\t/\tFALSE\t0\tunrelated\tkeep-me\n"
	previous := "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\told-yt\n" +
		"#HttpOnly_.twitch.tv\tTRUE\t/\tTRUE\t0\tauth-token\told-tw\n"

	got := restorePlatformRows(merged, previous, map[string]bool{"twitch": true})

	if !strings.Contains(got, "new-yt") {
		t.Error("YouTube rows must be untouched when only Twitch is restored")
	}
	if !strings.Contains(got, "old-tw") || strings.Contains(got, "new-tw") {
		t.Errorf("Twitch rows were not restored:\n%s", got)
	}
	if strings.Contains(got, "old-yt") {
		t.Errorf("previous YouTube rows leaked into a Twitch-only restore:\n%s", got)
	}
	if !strings.Contains(got, "keep-me") {
		t.Errorf("rows belonging to neither platform must survive:\n%s", got)
	}
}

func TestCookieRowPlatform(t *testing.T) {
	cases := map[string]string{
		".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tv":            "youtube",
		"#HttpOnly_.google.com\tTRUE\t/\tTRUE\t0\tHSID\tv":      "youtube",
		"#HttpOnly_.twitch.tv\tTRUE\t/\tTRUE\t0\tauth-token\tv": "twitch",
		".example.com\tTRUE\t/\tFALSE\t0\tunrelated\tv":         "",
		"malformed": "",
	}
	for row, want := range cases {
		if got := cookieRowPlatform(row); got != want {
			t.Errorf("cookieRowPlatform(%q) = %q, want %q", row, got, want)
		}
	}
}

// --- finding 3: refreshFirefox inherits the new hard error ---

// TestBrowserRefreshWithEmptyProfileKeepsWorkingCookies pins the decision on
// finding 3. A desktop Firefox set to clear cookies on close leaves an empty
// profile; before this package that produced a header-only file, a no-op
// merge, and a successful refresh off the still-good cookies.txt. Turning
// that into a hard error would fire "Cookie Auto-Refresh Failed — recordings
// will fail" at a user whose recordings are fine.
func TestBrowserRefreshWithEmptyProfileKeepsWorkingCookies(t *testing.T) {
	profileDir := writeWALCookieProfile(t, []profileTestCookie{
		{name: "sessionid", value: "x", host: ".example.com", path: "/"},
	})
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(youtubeOnlyCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	// A browser that cannot launch: refreshFirefox swallows the launch
	// failure and reads the profile, which is the shape this test needs.
	s.detectBrowser = func() *DetectedBrowser {
		return &DetectedBrowser{Type: "firefox", Path: "moombox-no-such-browser", Name: "Firefox"}
	}
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return true, nil }
	// The jar is loaded from cookies.txt at startup in production; the
	// refreshPlatforms() gate reads it, so the test must too.
	if err := s.jar.Load(cookiePath); err != nil {
		t.Fatal(err)
	}

	ok, err := s.RefreshCookies(context.Background())
	if err != nil {
		t.Fatalf("an empty profile must not fail the refresh while cookies.txt still verifies: %v", err)
	}
	if !ok {
		t.Fatal("RefreshCookies = false although the existing cookies verified")
	}
	got, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "previous-login-info") {
		t.Errorf("existing cookies were damaged by an empty profile read:\n%s", got)
	}
}

// TestBrowserRefreshWithEmptyProfileSurfacesItWhenAuthIsDead is the other
// half of that decision. Downgrading the empty read must not make it
// INVISIBLE: when the existing cookies do not verify either, the operator has
// to learn that the refresh is producing nothing, otherwise a browser that
// silently stopped saving cookies looks like an ordinary expiry.
func TestBrowserRefreshWithEmptyProfileSurfacesItWhenAuthIsDead(t *testing.T) {
	profileDir := writeWALCookieProfile(t, []profileTestCookie{
		{name: "sessionid", value: "x", host: ".example.com", path: "/"},
	})
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(youtubeOnlyCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser {
		return &DetectedBrowser{Type: "firefox", Path: "moombox-no-such-browser", Name: "Firefox"}
	}
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return false, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
	if err := s.jar.Load(cookiePath); err != nil {
		t.Fatal(err)
	}

	ok, err := s.RefreshCookies(context.Background())
	if err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}
	if ok {
		t.Fatal("RefreshCookies = true although nothing verified")
	}
	status := s.GetStatus()
	if status.LastError == nil || !strings.Contains(*status.LastError, "no cookies") {
		t.Errorf("the empty profile read must be named in the status, got %v", status.LastError)
	}
}

// --- finding 1: sidecar stat errors and torn copies ---

func TestIsMissingSidecar(t *testing.T) {
	if !isMissingSidecar(fs.ErrNotExist) {
		t.Error("a genuinely absent sidecar is the normal checkpointed case")
	}
	for _, err := range []error{fs.ErrPermission, errors.New("input/output error")} {
		if isMissingSidecar(err) {
			t.Errorf("%v is not absence — treating it as absence silently drops the WAL", err)
		}
	}
}

// TestSnapshotFailsLoudlyOnUnreadableSidecar covers the silent-stale path: a
// -wal that exists but cannot be copied must never be skipped, because the
// main file alone may hold a stale checkpointed set that then returns with no
// error at all.
func TestSnapshotFailsLoudlyOnUnreadableSidecar(t *testing.T) {
	profileDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(profileDir, "cookies.sqlite"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory where the -wal should be: stat succeeds, the copy cannot.
	if err := os.Mkdir(filepath.Join(profileDir, "cookies.sqlite-wal"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, cleanup, err := snapshotFirefoxCookieDB(profileDir)
	cleanup()
	if err == nil {
		t.Fatal("an uncopyable -wal must fail the snapshot, not be skipped")
	}
	if !strings.Contains(err.Error(), "-wal") {
		t.Errorf("error should name the sidecar it could not copy: %v", err)
	}
}

// TestCookieDBFingerprintDetectsWrites proves the torn-copy detector actually
// notices a concurrent writer. A copy is not atomic, so a main file and a -wal
// grabbed either side of a flush can disagree and yield a PARTIAL set with no
// error — which the retry loop would never retry, because it only retries when
// an error came back.
func TestCookieDBFingerprintDetectsWrites(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	dbPath := filepath.Join(profileDir, "cookies.sqlite")

	before := fingerprintCookieDB(dbPath)
	if fingerprintsDiffer(before, fingerprintCookieDB(dbPath)) {
		t.Fatal("two fingerprints of an idle database must match")
	}

	// Append to the -wal the way a live Firefox would.
	f, err := os.OpenFile(dbPath+"-wal", os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if !fingerprintsDiffer(before, fingerprintCookieDB(dbPath)) {
		t.Fatal("a -wal that grew during the copy must be detected as a torn snapshot")
	}
}

// --- housekeeping ---

// TestSweepStaleCookieSnapshots covers the SIGKILL leak: each abandoned
// snapshot dir holds the user's live session cookies.
func TestSweepStaleCookieSnapshots(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, "moombox-cookiedb-stale")
	fresh := filepath.Join(root, "moombox-cookiedb-fresh")
	unrelated := filepath.Join(root, "somebody-elses-tempdir")
	for _, d := range []string{stale, fresh, unrelated} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	sweepStaleCookieSnapshots(root, time.Hour)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("an abandoned snapshot older than the cutoff must be removed — it holds session cookies")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a recent snapshot may belong to a concurrent read and must survive")
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Error("the sweep must only touch its own prefix")
	}
}

// TestCorruptDBFailsFastWithoutRetrying pins the retry gate: a corrupt
// database is permanent, and retrying it five times at 500ms only delays the
// error the operator needs.
func TestCorruptDBFailsFastWithoutRetrying(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cookies.sqlite"), []byte("this is not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, _, err := readFirefoxCookies(dir)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrCookieDBUnreadable) {
		t.Fatalf("want ErrCookieDBUnreadable, got %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("corrupt DB took %v — the retry loop should not retry a permanent error", elapsed)
	}
}

// TestBrowserRefreshRestoresARegressedPlatform is A2's narrowed form (Q13
// revised): the browser path now rolls back too, for the regression arm only.
//
// A twin of TestRefreshCookiesRestoresOnlyTheRegressedPlatform above, driven
// through the BROWSER path — a Firefox that cannot launch, so refreshFirefox
// swallows the failure and reads the profile, the only way to reach this
// branch without a real browser. YouTube alone, because refreshFirefox sleeps
// firefoxLaunchSpacing (5 s) BETWEEN platform launches.
//
// The premise is that a browser refresh CAN regress a platform, and it is not
// exotic: a profile the desktop browser signed out of, or one an extension
// cleared, hands back rows that win the merge by name+domain+path and do not
// work — the credential that worked is gone from disk under a refresh that
// reported success. The scoping comment this replaces asserted the opposite
// ("cannot be staler than what was on disk"), which is true of the values' AGE
// and says nothing about whether they authenticate.
//
// Mutation: skip the restore on the browser path entirely.
//
// VerifyYouTubeAuth answers true only on its FIRST call (the pre-write check)
// and false on every call after — including the RE-verification that runs on
// the restored file. A stub tied to the jar's live value instead would answer
// true again once "previous-sapisid" is back on disk, RefreshCookies would
// take the success branch (restore-then-reverify-OK, exactly what
// TestRollbackDoesNotLeaveMisleadingStatus pins), and LastError would stay
// nil — the outcome switch that builds the operator-facing string this test
// pins below is unreachable whenever the restored credential re-verifies OK.
// Answering false throughout models the credential having gone bad a moment
// after the pre-check (a global sign-out mid-refresh, say): still a genuine
// regression — the restore still happens and the file assertions below still
// hold — but the restored value is ALSO currently broken, which is what it
// takes to reach "kept the previous cookies for X — Y did not verify" at all.
func TestBrowserRefreshRestoresARegressedPlatform(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(youtubeOnlyCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser {
		return &DetectedBrowser{Type: "firefox", Path: "moombox-no-such-browser", Name: "Firefox"}
	}
	verifyCalls := 0
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) {
		verifyCalls++
		return verifyCalls == 1, nil
	}
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
	if err := s.jar.Load(cookiePath); err != nil {
		t.Fatal(err)
	}

	if _, err := s.RefreshCookies(context.Background()); err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}

	data, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "previous-sapisid") {
		t.Errorf("the working YouTube credential was clobbered by a browser refresh that regressed it:\n%s", got)
	}
	if strings.Contains(got, "sapisid-from-profile") {
		t.Errorf("the regressed browser-refresh value survived the rollback:\n%s", got)
	}
	if s.jar.GetSapisid() != "previous-sapisid" {
		t.Errorf("jar holds %q after rollback, want the restored value", s.jar.GetSapisid())
	}

	// Pinned by exact equality, the same way the import arm's sibling
	// sentence is pinned three times over in
	// autocookies_relogin_status_test.go: this string reaches
	// GetStatus().LastError, i.e. the operator, and until this assertion
	// nothing in the package caught it reverting to the import wording
	// ("the mounted browser profile did not verify") on a path that never
	// mounted one.
	status := s.GetStatus()
	if status.LastError == nil {
		t.Fatal("a rolled-back browser refresh must leave an explanation")
	}
	const wantErr = "kept the previous cookies for youtube — the refreshed browser cookies did not verify"
	if *status.LastError != wantErr {
		t.Errorf("LastError = %q, want %q", *status.LastError, wantErr)
	}
}

// TestBrowserRefreshKeepsFreshCookiesWhenTheCheckIsInconclusive is the OTHER
// half of the narrowing, and the reason this is not simply "call
// platformsToRestore on both paths".
//
// The inconclusive arm exists for a mounted profile of unknown age. A browser
// refresh has just re-fetched from the live site, so a check that then could
// not reach the network says something about the NETWORK — restoring there
// discards a fresher set on every DNS blip, on the path a desktop install runs
// every thirty minutes. Its import-path twin
// (TestRefreshCookiesDoesNotCommitOnInconclusiveVerification) asserts the
// opposite outcome for the opposite reason; both must hold.
//
// Mutation: have the browser path call platformsToRestore (both arms).
func TestBrowserRefreshKeepsFreshCookiesWhenTheCheckIsInconclusive(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(youtubeOnlyCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser {
		return &DetectedBrowser{Type: "firefox", Path: "moombox-no-such-browser", Name: "Firefox"}
	}
	// Conclusive before the write, unreachable after it.
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) {
		if s.jar.GetSapisid() == "previous-sapisid" {
			return true, nil
		}
		return false, errors.New("dial tcp: i/o timeout")
	}
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
	if err := s.jar.Load(cookiePath); err != nil {
		t.Fatal(err)
	}

	if _, err := s.RefreshCookies(context.Background()); err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}

	data, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !strings.Contains(got, "sapisid-from-profile") {
		t.Errorf("a browser refresh's freshly fetched cookies were rolled back over an unreachable "+
			"network — the inconclusive arm must not apply to the browser path:\n%s", got)
	}
}

// TestPlatformsToRestoreAfterBrowserRefreshIgnoresAPlatformAbsentFromPre pins
// mutant 8 from the arc-close review at the unit level: the loop ranges over
// `pre`, not `post`, on purpose. A platform present in `post` as verifyFailed
// but absent from `pre` — the shape of every platform on a fresh install,
// where the file did not exist yet and the pre-write snapshot was never taken
// — must never be restored.
//
// The stakes are not academic: a twin that iterated `post` instead (or that
// restored on absence rather than on regressedAfterWrite) would make
// restore = {youtube: true} here, and restorePlatformRows would then discard
// every YouTube row this pass just fetched — on the very first refresh a
// fresh install ever runs. TestBrowserRefreshWithNoPreviousCookiesKeepsTheFetchedRows
// below is the same claim through the whole RefreshCookies call; this is the
// same claim with nothing else in the way.
func TestPlatformsToRestoreAfterBrowserRefreshIgnoresAPlatformAbsentFromPre(t *testing.T) {
	pre := map[string]platformAuth{}
	post := map[string]platformAuth{
		"youtube": {hasCookies: true, state: verifyFailed, attempted: true},
	}
	if restore := platformsToRestoreAfterBrowserRefresh(pre, post); len(restore) != 0 {
		t.Errorf("a platform absent from pre must never be restored, got %v", restore)
	}
}

// TestBrowserRefreshWithNoPreviousCookiesKeepsTheFetchedRows is
// TestPlatformsToRestoreAfterBrowserRefreshIgnoresAPlatformAbsentFromPre's
// twin through the whole RefreshCookies call, closing the gap the arc-close
// review's probe found: no test anywhere in the package reached the browser
// path's restore decision with an empty `pre` snapshot.
//
// A literally-empty jar cannot reach that decision at all: refreshPlatforms()
// (autocookies.go:615-624) gates the ENTIRE pass on the jar already holding
// something worth re-fetching, so a truly untouched fresh install declines
// before the browser is even asked to launch — that path needs no test here,
// it is refreshDeclined() every time. What DOES reach the restore decision
// with an empty pre is the jar holding a credential while cookies.txt itself
// does not exist — the shape of an operator deleting the file externally
// while the process keeps running, or the first pass after a jar was
// populated in-memory before any write ever landed on disk. This test seeds
// the jar from a THROWAWAY file, never from cookiePath, so
// `previousCookies == ""` stays true and `pre` stays empty exactly as it
// would in that situation.
//
// Same Firefox-cannot-launch fixture as the two tests above (so
// refreshFirefox swallows the launch failure and reads the mounted profile).
//
// Mutation: the browser twin restoring a platform ABSENT from `pre` (mutant 8
// above) leaves nothing in cookies.txt but the header, since
// restorePlatformRows has an empty previousCookies to restore FROM.
func TestBrowserRefreshWithNoPreviousCookiesKeepsTheFetchedRows(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser {
		return &DetectedBrowser{Type: "firefox", Path: "moombox-no-such-browser", Name: "Firefox"}
	}
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }

	// Seed the JAR only — never cookiePath — so refreshPlatforms() sees a
	// platform worth refreshing while cookies.txt still does not exist.
	seedPath := filepath.Join(t.TempDir(), "seed.txt")
	if err := os.WriteFile(seedPath, []byte(youtubeOnlyCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.jar.Load(seedPath); err != nil {
		t.Fatal(err)
	}

	if _, statErr := os.Stat(cookiePath); !os.IsNotExist(statErr) {
		t.Fatalf("fixture is broken — cookies.txt must not exist yet, stat err = %v", statErr)
	}

	if _, err := s.RefreshCookies(context.Background()); err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}

	data, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatalf("a browser refresh with nothing to roll back to must still have written cookies.txt: %v", err)
	}
	if !strings.Contains(string(data), "sapisid-from-profile") {
		t.Errorf("the fetched YouTube credential was dropped when there was no previous cookies.txt to roll back to:\n%s", data)
	}
	if status := s.GetStatus(); status.LastError != nil {
		t.Errorf("a refresh with nothing to restore must not report a rollback, got LastError = %q", *status.LastError)
	}
}
