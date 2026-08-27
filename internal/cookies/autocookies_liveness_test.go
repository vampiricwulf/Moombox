package cookies

import (
	"context"
	"errors"
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
// and returned refreshDeclined() — so the one platform the pass existed to fix
// got no attempt at all. At the time that also produced a misleading "Cookie
// Auto-Refresh Ineffective" notification; a decline now reports nothing
// (runCookieRecovery splits the Unknown branch on RefreshResult.Ran), which
// makes a regression here silent rather than merely confusing.
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

// TestInconclusiveImportOverADeadPartialSetKeepsItAndSaysWhy covers the one
// behavioural shape this arc genuinely made newly reachable, and the
// attribution bug that shape exposes. Both facts belong to one event, so they
// are asserted together — the same pairing TestRefreshCookiesDoesNotCommitOn-
// InconclusiveVerification uses.
//
// The shape: cookies.txt holds a KNOWN-DEAD partial set and the mounted profile
// holds a complete fresh one, but the post-import check cannot complete. pre is
// now {hasCookies:true, state:verifyFailed} where it used to be
// {false, verifyFailed}, so arm 2 (before.hasCookies && after == verifyUnknown)
// fires and the fresh import is DISCARDED. That is the documented policy —
// identical to what already happens for a full-but-dead set, and bounded,
// because the next pass re-imports once the network is back — but until now
// nothing exercised it.
//
// The bug: the restored dead set then verifies conclusively-false, so the
// post-rollback `inconclusive` is false and the operator was told "the mounted
// browser profile did not verify" about a profile that was never evaluated.
// That message sends a container operator off to re-export a mount that is
// perfectly fine.
func TestInconclusiveImportOverADeadPartialSetKeepsItAndSaysWhy(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(
		"# Netscape HTTP Cookie File\n"+
			".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tdead-sapisid\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	offline := errors.New("dial tcp: no such host")
	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	// Conclusively dead for what is already on disk; unreachable for anything
	// the profile brings. Keyed off the jar's live value so the pre-import
	// check, the post-import check and the post-rollback re-check all answer
	// differently — which is the only way to separate the three.
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) {
		if s.jar.GetSapisid() == "dead-sapisid" {
			return false, nil
		}
		return false, offline
	}
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }

	if _, err := s.RefreshCookies(context.Background()); err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}

	data, err := os.ReadFile(cookiePath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "dead-sapisid") || strings.Contains(got, "sapisid-from-profile") {
		t.Errorf("an unevaluated import was committed over the previous credentials:\n%s", got)
	}

	status := s.GetStatus()
	if status.LastError == nil {
		t.Fatal("an import that was not committed must leave an explanation")
	}
	if !strings.Contains(*status.LastError, "did not complete") {
		t.Errorf("the rollback must name the incomplete check that caused it, got %q", *status.LastError)
	}
	if strings.Contains(*status.LastError, "did not verify") {
		t.Errorf("the rollback blames a profile that was never evaluated: %q", *status.LastError)
	}
	// The credentials actually in force ARE conclusively dead, so this half of
	// the advice is earned and must survive the attribution fix.
	if !status.NeedsManualRelogin["youtube"] {
		t.Error("the restored credentials were conclusively rejected; the user does need to sign in again")
	}
}

// finishSetupService wires the FinishSetup preconditions the way
// TestFinishSetupTreatsEmptyProfileAsNoLogin does: a registered (fake) setup
// process on the Firefox path, already exited so the graceful-close wait is
// skipped.
func finishSetupService(t *testing.T, rows []profileTestCookie, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *AutoCookieService {
	t.Helper()
	profileDir := writeWALCookieProfile(t, rows)
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), logger)
	s.setupProcess = &os.Process{Pid: -1}
	s.setupBrowser = &DetectedBrowser{Type: "firefox", Path: "firefox", Name: "Firefox"}
	s.browserExited = true
	return s
}

// TestFinishSetupDoesNotRecordAnUnverifiedLoginAsVerified is site 3a.
//
// When the verify callback errored, FinishSetup fell back to cookie presence
// and handed that value to PersistPlatforms, which unions into
// cfg.Cookies.Platforms — a set that only ever grows and is never retracted. A
// single 429 during interactive setup therefore recorded "YouTube verified" in
// config, durably, on the strength of two cookie names being present.
//
// Accepting the login is still right (see the return-value assertion below);
// recording the acceptance as a verification is not.
func TestFinishSetupDoesNotRecordAnUnverifiedLoginAsVerified(t *testing.T) {
	s := finishSetupService(t, youtubeAuthRows(), nopAutoCookieLogger{})
	// The shape production produces: refresh.go returns a status code and
	// nothing else, precisely so no credential material can reach a log or the
	// UI through it.
	rateLimited := errors.New("youtube auth check: unexpected status 429")
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return false, rateLimited }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, rateLimited }

	var persistedYT, persistedTW bool
	var persistCalls int
	s.PersistPlatforms = func(yt, tw bool) {
		persistCalls++
		persistedYT, persistedTW = yt, tw
	}

	ytAuth, twAuth, err := s.FinishSetup(context.Background())
	if err != nil {
		t.Fatalf("FinishSetup: %v", err)
	}

	// A sign-in the user just completed is accepted: refusing it over a rate
	// limit is the false-failure direction, and in a container the remedy it
	// would send them to may not even be reachable.
	if !ytAuth {
		t.Error("FinishSetup rejected a login the user just completed because the check could not reach YouTube")
	}
	if twAuth {
		t.Error("no Twitch credential was extracted, so nothing about Twitch may be accepted")
	}

	if persistCalls != 1 {
		t.Fatalf("PersistPlatforms called %d times, want 1", persistCalls)
	}
	if persistedYT {
		t.Error("an unverified acceptance was written into the durable verified-platforms set on the strength of a 429")
	}
	if persistedTW {
		t.Error("Twitch was recorded as verified with no Twitch credential at all")
	}

	meta, err := LoadMeta(s.cookiePath)
	if err != nil {
		t.Fatalf("LoadMeta: %v", err)
	}
	if meta == nil {
		t.Fatal("FinishSetup should still have written the sidecar")
	}
	if len(meta.Platforms) != 0 {
		t.Errorf("cookies.meta.json records %v as verified after a check that did not complete", meta.Platforms)
	}
}

// TestFinishSetupSaysWhenItCouldNotVerify is site 3b. The nil-callback branch
// has always warned that it is reporting on presence alone; the errored branch
// emitted nothing — no log, no status — so once a non-200 became inconclusive
// a 429, a 503 and a captive portal all read as a clean verified pass.
func TestFinishSetupSaysWhenItCouldNotVerify(t *testing.T) {
	log := &capturingLogger{}
	s := finishSetupService(t, youtubeAuthRows(), log)
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) {
		return false, errors.New("youtube auth check: unexpected status 429")
	}

	ytAuth, _, err := s.FinishSetup(context.Background())
	if err != nil {
		t.Fatalf("FinishSetup: %v", err)
	}

	if !log.contains("did not complete") {
		t.Errorf("an inconclusive setup check said nothing at all: %v", log.msgs)
	}
	// It must not read as a failure either — a check that could not reach the
	// site has earned neither verdict. Asserting on the value AND on the
	// "neither platform is authenticated" line, because both are live: an
	// over-correction that rejected the login would flip the first and emit the
	// second, and a string that can never appear guards nothing.
	if !ytAuth {
		t.Error("a 429 is not evidence against a login the user just completed")
	}
	if log.contains("neither platform is authenticated") {
		t.Errorf("an incomplete check was reported as a failure to authenticate: %v", log.msgs)
	}
	if status := s.GetStatus(); status.LastError != nil {
		t.Errorf("an inconclusive check must not raise the Settings error, which reads as \"recordings will fail\": %q", *status.LastError)
	}
}

// TestFinishSetupDoesNotReportASignInThatWasNeverChecked is I1.
//
// checkYouTubeAuth's gates at refresh.go:596-604 return an error BY DESIGN when
// the jar holds something but cannot build a request out of it — a structural
// failure must not read to shouldFireRecovery as dead credentials. Routing
// FinishSetup through checkPlatformAuth newly exposed that path here, and
// "inconclusive" alone could not tell it apart from a rate limit, so a leftover
// Google remnant with no SAPISID was reported as a completed YouTube sign-in.
//
// The web setup wizard turns that value into a green "YouTube cookies
// configured" badge and an entry in active_platforms, so the user who signed in
// to Twitch only would be told YouTube was configured too.
func TestFinishSetupDoesNotReportASignInThatWasNeverChecked(t *testing.T) {
	// A Google session remnant with no SAPISID and no __Secure-3PAPISID:
	// HasAnyYouTubeAuthCookie accepts it, GenerateAuthorizationHeader cannot
	// build a SAPISIDHASH from it, so the real check errors before any request.
	remnant := []profileTestCookie{
		{name: "__Secure-1PSID", value: "1psid-remnant", host: ".youtube.com", path: "/", httpOnly: true, secure: true},
		{name: "SID", value: "sid-remnant", host: ".youtube.com", path: "/", secure: true},
		{name: "auth-token", value: goodTwitchToken, host: ".twitch.tv", path: "/", httpOnly: true, secure: true},
	}
	s := finishSetupService(t, remnant, nopAutoCookieLogger{})
	// The REAL check over the service's own jar — the shape
	// cmd/moombox/services.go:601 builds. A stub could not reproduce this:
	// the gate lives inside the production callback.
	s.VerifyYouTubeAuth = NewRefreshService(s.jar, 0, nopLogger{}).CheckYouTubeAuth
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return true, nil }

	ytAuth, twAuth, err := s.FinishSetup(context.Background())
	if err != nil {
		t.Fatalf("FinishSetup: %v", err)
	}

	if s.jar.GenerateAuthorizationHeader("https://www.youtube.com") != "" {
		t.Fatal("fixture is broken — this test needs a jar that cannot build a SAPISIDHASH")
	}
	if !s.jar.HasAnyYouTubeAuthCookie() {
		t.Fatal("fixture is broken — the remnant must still read as a configured platform")
	}

	if ytAuth {
		t.Error("YouTube was reported as a completed sign-in although no request was ever made — the setup wizard lights a green badge off this")
	}
	if !twAuth {
		t.Error("the Twitch login the user actually completed must still be accepted")
	}
}

// TestFinishSetupStillRecordsAVerifiedLogin is the control for both site-3
// tests: the ordinary success path must keep writing the platform down, or
// SetExpectedPlatforms loses its startup baseline entirely.
func TestFinishSetupStillRecordsAVerifiedLogin(t *testing.T) {
	s := finishSetupService(t, youtubeAuthRows(), nopAutoCookieLogger{})
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }

	var persistedYT bool
	s.PersistPlatforms = func(yt, tw bool) { persistedYT = yt }

	ytAuth, _, err := s.FinishSetup(context.Background())
	if err != nil {
		t.Fatalf("FinishSetup: %v", err)
	}
	if !ytAuth {
		t.Error("FinishSetup = false for a login YouTube confirmed")
	}
	if !persistedYT {
		t.Error("a verified login was not persisted")
	}
}
