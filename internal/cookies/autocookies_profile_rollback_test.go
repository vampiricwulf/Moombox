package cookies

import (
	"context"
	"errors"
	"io/fs"
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
// mergeCookieFiles lets the imported value win by name+domain, so without a
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
	_, err := readFirefoxCookies(dir)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrCookieDBUnreadable) {
		t.Fatalf("want ErrCookieDBUnreadable, got %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("corrupt DB took %v — the retry loop should not retry a permanent error", elapsed)
	}
}
