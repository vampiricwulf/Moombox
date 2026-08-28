package cookies

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestRefreshCookiesDetailedReportsPerPlatform reconstructs the 2026-08-20
// 03:40:01 field log inside the package that produced it.
//
// A mounted profile whose YouTube credentials are dead and whose Twitch
// credentials work. Both facts were already computed — checkPlatformAuth
// returns both platforms — and then discarded by a (bool, error) signature,
// so the only thing the caller could see was "something verified". The
// recovery callback, invoked FOR YouTube, read that as success.
func TestRefreshCookiesDetailedReportsPerPlatform(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAndTwitchRows(goodTwitchToken))
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return false, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return true, nil }

	result, err := s.RefreshCookiesDetailed(context.Background())
	if err != nil {
		t.Fatalf("RefreshCookiesDetailed: %v", err)
	}

	if !result.Ran {
		t.Error("Ran = false although the pass imported and verified a profile")
	}
	if got := result.Verdict("youtube"); got != RefreshFailed {
		t.Errorf("YouTube verdict = %v, want failed — YouTube was conclusively rejected", got)
	}
	if got := result.Verdict("twitch"); got != RefreshOK {
		t.Errorf("Twitch verdict = %v, want ok", got)
	}

	// The bool the wrapper hands back is unchanged and still true: the
	// question it answers is whole-service, and the service can in fact do
	// authenticated Twitch work. That answer is not wrong — it is just not
	// the answer a YouTube job needs, which is the entire defect.
	ok, err := s.RefreshCookiesDetailed(context.Background())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if !ok.AnyVerified() {
		t.Error("AnyVerified = false although Twitch verified — the legacy bool semantics moved")
	}
}

// TestRefreshDeclinePathsReportUnknown covers the paths that return without
// finding anything out.
//
// These are the reason a bool cannot carry this answer. `false` from a
// declined pass and `false` from a conclusively dead credential are the same
// value, and only the second means anything is wrong — conflating them is how
// a notification ends up asserting a cause it does not know. Every one of
// these must come back Unknown for BOTH platforms, with Ran false.
func TestRefreshDeclinePathsReportUnknown(t *testing.T) {
	// A profile directory that exists, so the decline under test is the one
	// named rather than the missing-profile error.
	newService := func(t *testing.T) *AutoCookieService {
		t.Helper()
		profileDir := writeWALCookieProfile(t, youtubeAuthRows())
		cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
		s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
		s.VerifyYouTubeAuth = func(context.Context) (bool, error) {
			t.Error("a declined pass must not verify anything")
			return false, nil
		}
		s.VerifyTwitchAuth = s.VerifyYouTubeAuth
		return s
	}

	cases := []struct {
		name  string
		setup func(*AutoCookieService)
	}{
		{
			name:  "setup in progress",
			setup: func(s *AutoCookieService) { s.setupClaimed = true },
		},
		{
			name:  "a refresh is already running",
			setup: func(s *AutoCookieService) { s.refreshCmd = &exec.Cmd{} },
		},
		{
			// A browser IS installed (so this is the browser path, not an
			// import) and the jar holds nothing worth re-fetching.
			name: "no platforms have cookies",
			setup: func(s *AutoCookieService) {
				s.detectBrowser = func() *DetectedBrowser {
					return &DetectedBrowser{Type: "firefox", Path: "moombox-no-such-browser", Name: "Firefox"}
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newService(t)
			tc.setup(s)

			result, err := s.RefreshCookiesDetailed(context.Background())
			if err != nil {
				t.Fatalf("a decline is not an error: %v", err)
			}
			if result.Ran {
				t.Error("Ran = true although the pass declined before doing any work")
			}
			if result.YouTube != RefreshUnknown || result.Twitch != RefreshUnknown {
				t.Errorf("verdicts = %v/%v, want unknown/unknown — nothing was established",
					result.YouTube, result.Twitch)
			}
			if result.AnyVerified() {
				t.Error("a declined pass must not report the legacy bool as true")
			}
		})
	}
}

// TestRefreshAbortPathsReportRanButUnknown is the subtler half of the
// contract: a pass that started work and then failed before it could verify.
//
// It differs from a decline in exactly one bit — Ran — and in nothing else.
// The temptation is to read a returned error as "so the credentials must be
// bad"; it is not. A profile that cannot be read and a cookie file that
// cannot be written both say precisely nothing about whether what is on disk
// still authenticates, and the verdicts must stay Unknown so no caller
// converts an I/O failure into "your cookies are dead".
func TestRefreshAbortPathsReportRanButUnknown(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T) *AutoCookieService
	}{
		{
			// The import path with an unreadable profile: the directory
			// exists (so this is not the missing-profile decline) but holds
			// no cookie database.
			name: "profile import fails",
			build: func(t *testing.T) *AutoCookieService {
				t.Helper()
				s := NewAutoCookieService(t.TempDir(), filepath.Join(t.TempDir(), "cookies.txt"),
					NewCookieJar(), nopAutoCookieLogger{})
				s.detectBrowser = func() *DetectedBrowser { return nil }
				return s
			},
		},
		{
			// Past the read, into the write. The import succeeded and
			// produced real cookies; the file they were going to land in
			// cannot be written.
			name: "the cookie file cannot be written",
			build: func(t *testing.T) *AutoCookieService {
				t.Helper()
				failCookieWriteAfter(t, 1, errors.New("disk on fire"))
				s := NewAutoCookieService(writeWALCookieProfile(t, youtubeAuthRows()),
					filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), nopAutoCookieLogger{})
				s.detectBrowser = func() *DetectedBrowser { return nil }
				return s
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.build(t)
			s.VerifyYouTubeAuth = func(context.Context) (bool, error) {
				t.Error("a pass that aborted before verification must not verify anything")
				return false, nil
			}
			s.VerifyTwitchAuth = s.VerifyYouTubeAuth

			result, err := s.RefreshCookiesDetailed(context.Background())
			if err == nil {
				t.Fatal("fixture is broken — this path was supposed to fail")
			}
			if !result.Ran {
				t.Error("Ran = false although the pass did real work before failing")
			}
			if result.YouTube != RefreshUnknown || result.Twitch != RefreshUnknown {
				t.Errorf("verdicts = %v/%v, want unknown/unknown — an I/O failure says nothing "+
					"about whether the credentials on disk are alive", result.YouTube, result.Twitch)
			}
			if result.HasCredentials("youtube") || result.HasCredentials("twitch") {
				t.Error("a pass that never reached the jar must not report stored credentials")
			}
			if result.AnyVerified() {
				t.Error("an aborted pass must not report the legacy bool as true")
			}
		})
	}
}

// TestTotalExpiryReportsFailedWithNothingStored is the OTHER way a platform
// reaches "conclusively unauthenticated with no credentials behind it", and
// the one that proves that combination is not hypothetical.
//
// The jar ignores expiry; mergeCookieFiles prunes on it. So a platform whose
// every stored row has lapsed passes the "there are cookies worth refreshing"
// gate, gets merged down to nothing, and comes out the far side with a
// conclusive failure and an empty jar — while a live sibling carries the pass
// far enough to reach verification at all.
//
// This is also the trigger that makes the recovery notification's
// no-credentials branch reachable: shouldFireRecovery's cookiesPresent is
// sampled from the pre-merge jar, so recovery fires for exactly this platform.
// The branch is not dead code, and the copy there has to be true both here and
// on an install that never held credentials at all.
func TestTotalExpiryReportsFailedWithNothingStored(t *testing.T) {
	profileDir := writeWALCookieProfile(t, []profileTestCookie{
		{name: "auth-token", value: goodTwitchToken, host: ".twitch.tv", path: "/", httpOnly: true, secure: true},
		{name: "login", value: "someuser", host: ".twitch.tv", path: "/"},
	})
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(partiallyExpiredPreviousCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return false, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) {
		return s.jar.GetTwitchAuthToken() == goodTwitchToken, nil
	}
	if err := s.jar.Load(cookiePath); err != nil {
		t.Fatal(err)
	}
	// The pre-merge jar still believes YouTube is present — which is exactly
	// what shouldFireRecovery reads before deciding to fire.
	if !s.jar.HasYouTubeAuthCookies() {
		t.Fatal("fixture is broken — the jar must believe it holds YouTube auth before the refresh")
	}

	result, err := s.RefreshCookiesDetailed(context.Background())
	if err != nil {
		t.Fatalf("RefreshCookiesDetailed: %v", err)
	}

	if result.Verdict("twitch") != RefreshOK {
		t.Fatalf("fixture is broken — the live Twitch sibling was supposed to verify, got %v",
			result.Verdict("twitch"))
	}
	if got := result.Verdict("youtube"); got != RefreshFailed {
		t.Errorf("YouTube verdict = %v, want failed — every row it had was pruned as expired", got)
	}
	if result.HasCredentials("youtube") {
		t.Error("YouTube reports stored credentials after every one of its rows was pruned — " +
			"this is the pair that decides whether the operator is told to REPLACE cookies or SUPPLY them")
	}
}

// TestRefreshVerdictUnknownIsTheZeroValue is the property every other
// guarantee here rests on: a RefreshResult nobody populated asserts nothing.
func TestRefreshVerdictUnknownIsTheZeroValue(t *testing.T) {
	var zero RefreshResult
	if zero.Ran {
		t.Error("the zero RefreshResult claims it ran")
	}
	if zero.YouTube != RefreshUnknown || zero.Twitch != RefreshUnknown {
		t.Errorf("zero verdicts = %v/%v, want unknown/unknown", zero.YouTube, zero.Twitch)
	}
	var v RefreshVerdict
	if v != RefreshUnknown {
		t.Errorf("the zero RefreshVerdict is %v, want unknown", v)
	}
}

// TestRefreshResultVerdictLookup pins the platform-key contract.
//
// The unrecognised and empty keys are the load-bearing rows: callers turn
// RefreshFailed into "your cookies are dead, recordings will fail", and a
// typo'd or absent platform string must not fire that off a programming
// error. Unknown is the only safe answer to a question we cannot parse.
func TestRefreshResultVerdictLookup(t *testing.T) {
	r := RefreshResult{Ran: true, YouTube: RefreshOK, Twitch: RefreshFailed}

	cases := []struct {
		platform string
		want     RefreshVerdict
	}{
		{"youtube", RefreshOK},
		{"twitch", RefreshFailed},
		// Case-insensitive: the platform string reaches this from job rows,
		// config and callbacks, and its casing has never been normalised.
		{"YouTube", RefreshOK},
		{"TWITCH", RefreshFailed},
		{"", RefreshUnknown},
		{"kick", RefreshUnknown},
		{"you tube", RefreshUnknown},
		// Surrounding whitespace is NOT trimmed, deliberately. Nothing in the
		// codebase normalises a platform string, so inventing a trim contract
		// here would be a guess; Unknown is the safe answer to a key we
		// cannot parse, and this row pins that it stays the answer.
		{"youtube ", RefreshUnknown},
		{" twitch", RefreshUnknown},
	}
	for _, tc := range cases {
		if got := r.Verdict(tc.platform); got != tc.want {
			t.Errorf("Verdict(%q) = %v, want %v", tc.platform, got, tc.want)
		}
	}
}

// TestRefreshResultHasCredentials pins the presence predicate that decides
// how a RefreshFailed is WORDED.
//
// It is not a health signal and must never be used as one — a stored cookie
// that does not work is worth nothing. Its whole job is to stop "the stored
// cookies for this platform are dead" being said about a platform that has
// none: on a YouTube-only install, a subscriber-only Twitch VOD produces a
// conclusive Twitch failure with nothing stored behind it.
func TestRefreshResultHasCredentials(t *testing.T) {
	r := RefreshResult{
		Ran:     true,
		YouTube: RefreshOK, YouTubeStored: true,
		Twitch: RefreshFailed, TwitchStored: false,
	}

	cases := []struct {
		platform string
		want     bool
	}{
		{"youtube", true},
		{"YouTube", true},
		{"twitch", false},
		{"TWITCH", false},
		// Same contract as Verdict: an unparseable key claims nothing, and
		// false is the answer that cannot license a cause.
		{"", false},
		{"kick", false},
		{"youtube ", false},
	}
	for _, tc := range cases {
		if got := r.HasCredentials(tc.platform); got != tc.want {
			t.Errorf("HasCredentials(%q) = %v, want %v", tc.platform, got, tc.want)
		}
	}

	var zero RefreshResult
	if zero.HasCredentials("youtube") || zero.HasCredentials("twitch") {
		t.Error("the zero RefreshResult claims to hold credentials")
	}
}

// TestYouTubeOnlyInstallReportsTwitchAsUnconfigured is the data half of the
// worker-side wording fix, end to end through a real refresh.
//
// The install holds working YouTube cookies and nothing for Twitch. A
// subscriber-only Twitch VOD asks attemptCookieRefresh for a Twitch refresh —
// there is no cookies-present gate there, and Usher's 403 cannot tell an
// anonymous session from an un-entitled one. YouTube's cookies keep the
// refresh from declining, so it really runs and really concludes: Twitch is
// conclusively unauthenticated, and it holds nothing.
//
// Both facts have to survive the trip, because only the pair distinguishes
// "your cookies died" from "you never had any".
func TestYouTubeOnlyInstallReportsTwitchAsUnconfigured(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) {
		t.Error("Twitch has no cookies — verification must not be attempted")
		return false, nil
	}

	result, err := s.RefreshCookiesDetailed(context.Background())
	if err != nil {
		t.Fatalf("RefreshCookiesDetailed: %v", err)
	}

	if result.Verdict("youtube") != RefreshOK || !result.HasCredentials("youtube") {
		t.Fatalf("fixture is broken — YouTube was supposed to verify off a stored credential, got %v/%v",
			result.Verdict("youtube"), result.HasCredentials("youtube"))
	}
	if got := result.Verdict("twitch"); got != RefreshFailed {
		t.Errorf("Twitch verdict = %v, want failed — nothing there will authenticate a request", got)
	}
	if result.HasCredentials("twitch") {
		t.Error("Twitch reports stored credentials on an install that has none — this is what licenses " +
			"telling the operator their Twitch cookies died")
	}
}

// TestAnyVerifiedPinsTheLegacyBool keeps RefreshCookies' contract exactly
// where it was: true iff at least one platform is conclusively authenticated.
// Several existing tests assert against that bool, and the four callers that
// do not have the per-platform defect still use it.
func TestAnyVerifiedPinsTheLegacyBool(t *testing.T) {
	all := []RefreshVerdict{RefreshUnknown, RefreshFailed, RefreshOK}
	for _, yt := range all {
		for _, tw := range all {
			want := yt == RefreshOK || tw == RefreshOK
			got := RefreshResult{Ran: true, YouTube: yt, Twitch: tw}.AnyVerified()
			if got != want {
				t.Errorf("AnyVerified(%v, %v) = %v, want %v", yt, tw, got, want)
			}
		}
	}
}

// TestRefreshThatRenewedNothingKeepsThePriorError is the second carried
// ledger item from A0.4, verified reachable and then closed.
//
// The clearing of lastError sat OUTSIDE the `renewed` gate that A0.4 added
// for lastRefresh and the meta sidecar. So a pass whose browser did nothing
// at all — the credentials on disk verify only because the independent
// 30-minute RefreshService keeps them alive — would still retract a
// previously recorded problem. Since the recorded problem is typically ABOUT
// the refresh mechanism ("the browser profile contained no cookies to refresh
// from"), a twice-broken refresh cleared its own report and presented a clean
// bill of health.
func TestRefreshThatRenewedNothingKeepsThePriorError(t *testing.T) {
	// Nothing relevant in the profile, so the read contributes nothing and
	// the existing cookies.txt is what verifies.
	profileDir := writeWALCookieProfile(t, []profileTestCookie{
		{name: "sessionid", value: "x", host: ".example.com", path: "/"},
	})
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(youtubeOnlyCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	// A browser that cannot launch is the simplest not-acted case: no
	// screenshot, and refreshFirefox degrades rather than failing the refresh.
	s.detectBrowser = func() *DetectedBrowser {
		return &DetectedBrowser{Type: "firefox", Path: "moombox-no-such-browser", Name: "Firefox"}
	}
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
	if err := s.jar.Load(cookiePath); err != nil {
		t.Fatal(err)
	}

	const prior = "the browser profile contained no cookies to refresh from"
	s.setError(prior)

	result, err := s.RefreshCookiesDetailed(context.Background())
	if err != nil {
		t.Fatalf("RefreshCookiesDetailed: %v", err)
	}
	if result.Verdict("youtube") != RefreshOK {
		t.Fatalf("fixture is broken — the cookies on disk were supposed to still verify, got %v",
			result.Verdict("youtube"))
	}
	if lr := s.GetStatus().LastRefresh; lr != nil {
		t.Fatalf("fixture is broken — this pass was supposed to renew nothing, got lastRefresh %q", *lr)
	}
	// The same fact the gates below consume, now visible to a caller. Without
	// it the manual-refresh button in Settings and the TUI's R F chord report
	// this pass as an unqualified success.
	if result.Renewed {
		t.Error("Renewed = true although the browser never ran — this is the bit the UI branches on")
	}

	status := s.GetStatus()
	if status.LastError == nil {
		t.Fatal("a pass whose browser did nothing retracted a previously recorded problem — " +
			"it established that the CREDENTIALS work, not that the REFRESH does")
	}
	if *status.LastError != prior {
		t.Errorf("LastError = %q, want the untouched prior error %q", *status.LastError, prior)
	}
}

// TestRefreshThatRenewedClearsThePriorError is the control for the test
// above: gating the clear must not make a stale error permanent. A pass that
// really did fetch credentials — here the browserless profile import, the
// container path — has the standing to retract an earlier report.
func TestRefreshThatRenewedClearsThePriorError(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(youtubeOnlyCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
	if err := s.jar.Load(cookiePath); err != nil {
		t.Fatal(err)
	}
	s.setError("something that used to be wrong")

	result, err := s.RefreshCookiesDetailed(context.Background())
	if err != nil {
		t.Fatalf("RefreshCookiesDetailed: %v", err)
	}
	if result.Verdict("youtube") != RefreshOK {
		t.Fatalf("fixture is broken — the import was supposed to verify, got %v", result.Verdict("youtube"))
	}
	if s.GetStatus().LastRefresh == nil {
		t.Fatal("fixture is broken — an import renews, so this pass should have stamped lastRefresh")
	}
	if !result.Renewed {
		t.Error("Renewed = false although this pass read the profile it verified — the UI would " +
			"downgrade a genuine success to 'could not confirm'")
	}
	if le := s.GetStatus().LastError; le != nil {
		t.Errorf("LastError = %q, want cleared — this pass fetched the credentials it verified", *le)
	}
}
