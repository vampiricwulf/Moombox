package cookies

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gatedBrowser stands in for a browser that is installed on the host and must
// never be executed. Every row that supplies it either declines or returns
// before the launch, so the path never has to exist.
func gatedBrowser() *DetectedBrowser {
	return &DetectedBrowser{Type: "firefox", Path: "moombox-no-such-browser", Name: "Firefox"}
}

// TestBrowserLaunchGateDropsTheBrowserNotTheRefresh is the whole of the
// auto_enabled rule inside this package, asserted as behaviour.
//
// The flag governs one mechanism — executing a headless browser — and reaches a
// refresh only by deciding whether it gets one. It must not refuse the pass:
// R F and the dashboard's shift+click are wired unconditionally precisely so an
// operator who has hand-updated their browser profile can have it imported
// immediately, and a disabled install is exactly the install that does so.
//
// The discriminator needs no process. The two branches differ in one observable
// way: the browser path gates on `len(refreshPlatforms()) == 0` and the import
// path deliberately does not (seeding a container that has no cookies.txt yet
// is its primary use case). So with an EMPTY JAR and a profile present:
//
//   - allowed    -> the browser branch is entered and declines. Ran = false.
//   - disallowed -> the import branch is entered and completes. Ran = true.
//
// That inversion is the assertion. It cannot be satisfied by a gate that
// refuses the pass, nor by no gate at all, and the allowed row is the junction
// guard: without it "Ran = true" could equally mean the browser was never
// consulted.
func TestBrowserLaunchGateDropsTheBrowserNotTheRefresh(t *testing.T) {
	newService := func(t *testing.T, browser *DetectedBrowser, allowed bool) (*AutoCookieService, *int) {
		t.Helper()
		s := NewAutoCookieService(
			writeWALCookieProfile(t, youtubeAuthRows()),
			filepath.Join(t.TempDir(), "cookies.txt"),
			NewCookieJar(), nopAutoCookieLogger{})
		s.detectBrowser = func() *DetectedBrowser { return browser }
		s.BrowserLaunchAllowed = func() bool { return allowed }
		verified := 0
		s.VerifyYouTubeAuth = func(context.Context) (bool, error) { verified++; return true, nil }
		s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
		return s, &verified
	}

	for _, tc := range []struct {
		name     string
		browser  *DetectedBrowser
		allowed  bool
		wantRan  bool
		verified bool
		why      string
	}{
		{
			// THE JUNCTION GUARD. A browser is present and permitted, so the
			// browser branch is taken and its empty-jar gate declines. Without
			// this row the disabled row proves nothing.
			name: "a permitted browser takes the browser branch", browser: gatedBrowser(), allowed: true,
			wantRan: false,
			why:     "an allowed browser must take the browser branch, which declines on an empty jar",
		},
		{
			// THE RULE. Same service, same empty jar, one predicate flipped —
			// and the pass now completes, because the gate dropped the browser
			// rather than the refresh.
			name: "a gated browser falls through to the profile import", browser: gatedBrowser(), allowed: false,
			wantRan: true, verified: true,
			why: "with the headless browser switched off, R F's remaining rung is an immediate import " +
				"from the browser profile — which is the whole point of the gesture on a profile the " +
				"operator has just hand-updated",
		},
		{
			// The flag has no opinion when there is no browser to gate. Both
			// rows must import, or the predicate has grown a second job.
			name: "no browser, allowed", browser: nil, allowed: true,
			wantRan: true, verified: true,
			why: "a browserless host has always imported and must keep doing so",
		},
		{
			name: "no browser, gated", browser: nil, allowed: false,
			wantRan: true, verified: true,
			why: "the flag must not change a pass that was never going to launch anything",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, verified := newService(t, tc.browser, tc.allowed)

			result, err := s.RefreshCookiesDetailed(context.Background())
			if err != nil {
				t.Fatalf("neither branch may error here: %v", err)
			}
			if result.Ran != tc.wantRan {
				t.Errorf("Ran = %v, want %v — %s", result.Ran, tc.wantRan, tc.why)
			}
			if got := *verified > 0; got != tc.verified {
				t.Errorf("verification ran = %v, want %v — %s", got, tc.verified, tc.why)
			}
		})
	}
}

// TestGatedBrowserIsNeverReportedAsAMissingOne pins the one place the gate
// changes what an operator is told.
//
// Dropping the browser routes a gated pass into the arm written for "no
// supported browser is installed". For an operator with Firefox sitting right
// there, that is an unearned cause with an actively wrong remedy — they would
// go and install a browser. The SENTINEL is deliberately shared (every
// consumer's branch is genuinely the same: there is no browser to use, and both
// states are R F's no-profile rung), but the sentence is not.
//
// Asserted in both directions and on both renderings — the returned error,
// which reaches the worker log and the TUI, and LastError, which is what the
// two dashboards show beside the cookie status. A one-directional check would
// pass on copy that merely says less.
func TestGatedBrowserIsNeverReportedAsAMissingOne(t *testing.T) {
	// No profile directory: this arm is only reachable when there is nothing to
	// import from either.
	newService := func(t *testing.T, browser *DetectedBrowser, allowed bool) *AutoCookieService {
		t.Helper()
		s := NewAutoCookieService(
			filepath.Join(t.TempDir(), "no-such-profile"),
			filepath.Join(t.TempDir(), "cookies.txt"),
			NewCookieJar(), nopAutoCookieLogger{})
		s.detectBrowser = func() *DetectedBrowser { return browser }
		s.BrowserLaunchAllowed = func() bool { return allowed }
		return s
	}

	// The two renderings are written separately and legitimately differ — the
	// error carries the wrapped sentinel's own words, LastError does not — so
	// each carries its own expectations rather than one set applied to both.
	for _, tc := range []struct {
		name         string
		browser      *DetectedBrowser
		allowed      bool
		wantErr      error
		errPrefix    string
		errUnsaid    []string
		statusSaid   []string
		statusUnsaid []string
	}{
		{
			name: "a browser is installed but gated", browser: gatedBrowser(), allowed: false,
			wantErr: ErrNoBrowserFound,
			// LEADS with the true cause. Asserted as a prefix rather than a
			// substring because the wrapped sentinel puts "no supported browser
			// found" at the tail, and what decides how this reads is which of
			// the two the operator meets first.
			errPrefix: "cookies.auto_enabled is false so no headless browser was launched",
			// The instruction this operator must never be given: they have one.
			errUnsaid:    []string{"install"},
			statusSaid:   []string{"auto_enabled"},
			statusUnsaid: []string{"no browser found for refresh"},
		},
		{
			name: "no browser is installed at all", browser: nil, allowed: true,
			wantErr:      ErrNoBrowserFound,
			errPrefix:    "no supported browser found",
			errUnsaid:    []string{"auto_enabled"},
			statusSaid:   []string{"no browser found for refresh"},
			statusUnsaid: []string{"auto_enabled"},
		},
		{
			// The third state, unchanged by the gate and included so a
			// regression cannot quietly collapse it into either of the others.
			name: "a permitted browser with no profile", browser: gatedBrowser(), allowed: true,
			wantErr:      ErrProfileNotFound,
			errPrefix:    "run setup first",
			errUnsaid:    []string{"auto_enabled"},
			statusSaid:   []string{"run setup first"},
			statusUnsaid: []string{"auto_enabled"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newService(t, tc.browser, tc.allowed)

			result, err := s.RefreshCookiesDetailed(context.Background())
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want one matching %v — the TUI's R F fallback and the Web "+
					"route both branch on the sentinel", err, tc.wantErr)
			}
			if result.Ran {
				t.Error("Ran = true although the pass found no cookie source at all")
			}
			// Not a nil-error decline. R F's fallback and the Web's 404/424
			// branch both depend on this arriving as an error.
			if err == nil {
				t.Fatal("this arm returned a nil-error decline, which every consumer renders as " +
					"RefreshDeclinedCauses — a cause list that names none of the real reason")
			}

			lastError := ""
			if p := s.GetStatus().LastError; p != nil {
				lastError = *p
			}
			if lastError == "" {
				t.Fatal("nothing was recorded in LastError, so the dashboards show no cause at all")
			}
			if !strings.HasPrefix(strings.ToLower(err.Error()), tc.errPrefix) {
				t.Errorf("the returned error does not LEAD with %q, so the first thing the operator "+
					"reads is not the cause: %q", tc.errPrefix, err)
			}
			for _, unwanted := range tc.errUnsaid {
				if strings.Contains(strings.ToLower(err.Error()), unwanted) {
					t.Errorf("the returned error says %q, which is not true of this state: %q", unwanted, err)
				}
			}
			lower := strings.ToLower(lastError)
			for _, want := range tc.statusSaid {
				if !strings.Contains(lower, want) {
					t.Errorf("LastError does not say %q: %q", want, lastError)
				}
			}
			for _, unwanted := range tc.statusUnsaid {
				if strings.Contains(lower, unwanted) {
					t.Errorf("LastError says %q, which is not true of this state: %q", unwanted, lastError)
				}
			}
		})
	}
}

// TestBrowserLaunchAllowedDefaultsToPermissive pins the nil contract.
//
// Every existing caller, and every test in this package written before the
// field existed, builds the service without it. If nil ever read as "gated",
// the whole package would silently stop using browsers.
func TestBrowserLaunchAllowedDefaultsToPermissive(t *testing.T) {
	s := NewAutoCookieService(t.TempDir(), filepath.Join(t.TempDir(), "cookies.txt"),
		NewCookieJar(), nopAutoCookieLogger{})
	if s.BrowserLaunchAllowed != nil {
		t.Fatal("the constructor now sets BrowserLaunchAllowed, so the nil default is no longer what callers get")
	}
	want := s.resolvedBrowser()
	if got := s.refreshBrowser(gateApplies); got != want {
		t.Errorf("refreshBrowser(gateApplies) = %v with a nil predicate, want resolvedBrowser()'s %v — "+
			"every caller that never heard of this field just lost its browser", got, want)
	}
}

// TestStartSetupIgnoresTheBrowserLaunchGate is the boundary between liveness
// and ACQUISITION, and the reason the gate is applied at
// RefreshCookiesDetailed's call site rather than inside resolvedBrowser itself.
//
// StartSetup resolves a browser too. Setup is how an operator SUPPLIES cookies,
// it is an explicit gesture in a visible window, and completing it is what
// turns auto_enabled on — so gating it would make the setting unreachable on
// the one install that most needs it (a fresh one, where the flag is false by
// definition), and the failure would present as "no supported browser found" on
// a machine that has one.
//
// The assertion is that it gets PAST browser resolution. It is not run to
// completion: the path is deliberately not a real browser, so the launch fails
// after this point, and ErrNoBrowserFound is the one answer that would mean the
// gate had reached it.
func TestStartSetupIgnoresTheBrowserLaunchGate(t *testing.T) {
	s := NewAutoCookieService(filepath.Join(t.TempDir(), "profile"),
		filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return gatedBrowser() }
	s.BrowserLaunchAllowed = func() bool { return false }
	t.Cleanup(func() { _ = s.CancelSetup() })

	if err := s.StartSetup("youtube"); errors.Is(err, ErrNoBrowserFound) {
		t.Fatalf("the browser-launch gate reached StartSetup: %v. Interactive setup is acquisition, "+
			"not liveness — gating it makes auto_enabled impossible to turn on, because finishing "+
			"setup is what turns it on", err)
	}
}

// TestUnreadableProfileIsNotTheLadderBottomRung is the Docker failure the
// manual refresh must not swallow.
//
// A mounted profile that is present but cannot be stat'd — EACCES from a uid
// mismatch between the host directory and compose's `user:`, ENOTDIR from a
// path component that is a file, an over-long path on Windows — used to come
// back wrapping ErrProfileNotFound. That is the ladder's bottom rung, so R F
// and the dashboard's shift+click both reported "No browser profile found",
// ran a plain recheck instead, and never showed the permissions sentence. The
// mounted-profile-plus-R F path is the designated Docker workflow, which makes
// this the likeliest failure on the route operators will actually use.
//
// Driven through RefreshCookiesDetailed, not importProfileCookies, because the
// bug was about which of TWO stats classified the failure: the outer gate sees
// a non-ENOENT error and proceeds, and the import is what names the cause. The
// seam covers both, so this is the real sequence.
func TestUnreadableProfileIsNotTheLadderBottomRung(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
	}{
		// The compose uid mismatch, and the reason this finding matters.
		{"permission denied", fs.ErrPermission},
		// A path component that is itself a file. Distinct errno, same remedy
		// shape, and it must not fall back either.
		{"not a directory", errors.New("not a directory")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A path that does not exist on disk, with the seam reporting the
			// failure instead. That is the faithful model: a directory whose
			// stat fails with EACCES fails it for BOTH callers, so both must go
			// through the seam. Backing it with a real directory would let the
			// gate's own stat succeed for real, and the test would then pass
			// even with the gate reading os.Stat directly — which a mutation
			// run showed it did.
			profileDir := filepath.Join(t.TempDir(), "mounted-profile")
			failProfileStat(t, profileDir, tc.cause)

			s := NewAutoCookieService(profileDir, filepath.Join(t.TempDir(), "cookies.txt"),
				NewCookieJar(), nopAutoCookieLogger{})
			s.detectBrowser = func() *DetectedBrowser { return nil }
			s.VerifyYouTubeAuth = func(context.Context) (bool, error) {
				t.Error("a pass that could not read the profile must not verify anything")
				return false, nil
			}
			s.VerifyTwitchAuth = s.VerifyYouTubeAuth

			result, err := s.RefreshCookiesDetailed(context.Background())

			if !errors.Is(err, ErrProfileDirUnreadable) {
				t.Fatalf("error = %v, want one matching ErrProfileDirUnreadable", err)
			}
			// THE ASSERTION THIS TEST EXISTS FOR. Both manual surfaces route on
			// this predicate; a true here is the fallback that loses the message.
			if IsNoBrowserProfile(err) {
				t.Error("an unreadable profile reads as the ladder's bottom rung, so R F and the " +
					"dashboard's shift+click both report \"No browser profile found\" and run a plain " +
					"recheck — throwing away the one sentence that would have fixed it")
			}
			// Ran, unlike a genuine rung 3. The pass got past the gate and did
			// look; that is the structural difference between the two states.
			if !result.Ran {
				t.Error("Ran = false, but this pass reached the import — a caller cannot tell it " +
					"apart from a decline")
			}
			if result.YouTube != RefreshUnknown || result.Twitch != RefreshUnknown {
				t.Errorf("verdicts = %v/%v, want unknown/unknown — an unreadable profile says nothing "+
					"about the credentials on disk", result.YouTube, result.Twitch)
			}

			// The message is the whole point: it has to survive to the operator,
			// and it has to name what they can change.
			lastError := ""
			if p := s.GetStatus().LastError; p != nil {
				lastError = *p
			}
			for _, text := range []struct{ what, got string }{
				{"the returned error", err.Error()},
				{"LastError", lastError},
			} {
				lower := strings.ToLower(text.got)
				for _, want := range []string{"permission", "profile"} {
					if !strings.Contains(lower, want) {
						t.Errorf("%s does not mention %q, so a container operator has nothing to act "+
							"on: %q", text.what, want, text.got)
					}
				}
				if strings.Contains(lower, "not found") {
					t.Errorf("%s says the profile was not found. It was found — it could not be read, "+
						"and those have different remedies: %q", text.what, text.got)
				}
			}
		})
	}
}

// failProfileStat makes statProfileDir fail for exactly one path, with a cause
// that is NOT fs.ErrNotExist.
//
// Scoped to the one path so the rest of the pass stats normally, and asserted
// to be non-ENOENT because an ENOENT would take the gate's own missing-profile
// branch and test the opposite of what this file is about.
func failProfileStat(t *testing.T, path string, cause error) {
	t.Helper()
	if os.IsNotExist(cause) {
		t.Fatalf("failProfileStat was given a not-exist cause (%v), which is the OTHER branch — "+
			"this helper exists to reach the one that means the profile is there", cause)
	}
	real := statProfileDir
	t.Cleanup(func() { statProfileDir = real })
	statProfileDir = func(name string) (os.FileInfo, error) {
		if name == path {
			return nil, &os.PathError{Op: "stat", Path: name, Err: cause}
		}
		return real(name)
	}
}
