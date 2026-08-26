package cookies

import (
	"context"
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
	if !ok.anyVerified() {
		t.Error("anyVerified = false although Twitch verified — the legacy bool semantics moved")
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
			if result.anyVerified() {
				t.Error("a declined pass must not report the legacy bool as true")
			}
		})
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
	}
	for _, tc := range cases {
		if got := r.Verdict(tc.platform); got != tc.want {
			t.Errorf("Verdict(%q) = %v, want %v", tc.platform, got, tc.want)
		}
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
			got := RefreshResult{Ran: true, YouTube: yt, Twitch: tw}.anyVerified()
			if got != want {
				t.Errorf("anyVerified(%v, %v) = %v, want %v", yt, tw, got, want)
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
	if le := s.GetStatus().LastError; le != nil {
		t.Errorf("LastError = %q, want cleared — this pass fetched the credentials it verified", *le)
	}
}
