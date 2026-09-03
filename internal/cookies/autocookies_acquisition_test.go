package cookies

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// TestAcquisitionModeSelectsTheRefreshPath is the whole of cookies.acquisition
// inside this package, asserted as behaviour rather than as a field read.
//
// The discriminator is the one TestBrowserLaunchGateDropsTheBrowserNotTheRefresh
// already uses, and for the same reason: it needs no process. On an EMPTY JAR
// the browser branch gates on `len(refreshPlatforms()) == 0` and declines,
// while the import branch deliberately does not gate and completes. So with a
// profile present, a browser resolvable, and nothing in the jar: "auto" ->
// Ran = false, "profile" -> Ran = true.
//
// The Ran=false rows are the junction guard. Without them "Ran = true" for
// "profile" could equally mean the mode was never consulted and the host simply
// had no browser.
func TestAcquisitionModeSelectsTheRefreshPath(t *testing.T) {
	newService := func(t *testing.T, mode string) (*AutoCookieService, *int) {
		t.Helper()
		s := NewAutoCookieService(
			writeWALCookieProfile(t, youtubeAuthRows()),
			filepath.Join(t.TempDir(), "cookies.txt"),
			NewCookieJar(), nopAutoCookieLogger{})
		// A browser IS resolvable on every row. gatedBrowser's path does not
		// exist, so nothing can execute even if a branch tried.
		s.detectBrowser = func() *DetectedBrowser { return gatedBrowser() }
		s.AcquisitionMode = func() string { return mode }
		verified := 0
		s.VerifyYouTubeAuth = func(context.Context) (bool, error) { verified++; return true, nil }
		s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
		return s, &verified
	}

	for _, tc := range []struct {
		name     string
		mode     string
		wantRan  bool
		verified bool
		why      string
	}{
		{
			name: "auto keeps the browser path", mode: AcquisitionAuto, wantRan: false,
			why: "auto is today's rule exactly: a resolvable browser takes the launch path, " +
				"which declines on an empty jar",
		},
		{
			name: "profile takes the import path", mode: AcquisitionProfile, wantRan: true, verified: true,
			why: "the whole point of the setting: a desktop with a browser installed reads the " +
				"configured profile read-only instead of launching anything",
		},
		{
			name: "an unrecognised mode falls back to auto", mode: "headless", wantRan: false,
			why: "resolvedAcquisition normalises, so a value that slipped past config validation " +
				"cannot put the service in an undefined state",
		},
		{
			name: "the dropped third value falls back to auto", mode: "browser", wantRan: false,
			why: "\"browser\" is not a mode (ruling 2026-09-02); a stale config carrying it must " +
				"behave as auto rather than as a fourth thing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, verified := newService(t, tc.mode)
			result, err := s.RefreshCookiesDetailed(context.Background())
			if err != nil {
				t.Fatalf("no row here may error: %v", err)
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

// TestAcquisitionModeNilDefaultsToAuto pins the nil contract, the same way
// TestBrowserLaunchAllowedDefaultsToPermissive pins its neighbour's. Every
// existing caller and every test that builds the service by struct literal
// must keep today's behaviour without knowing this field exists.
func TestAcquisitionModeNilDefaultsToAuto(t *testing.T) {
	s := NewAutoCookieService("", "", nil, nopAutoCookieLogger{})
	if s.AcquisitionMode != nil {
		t.Fatal("the constructor now sets AcquisitionMode, so the nil default is no longer what callers get")
	}
	if got := s.resolvedAcquisition(); got != AcquisitionAuto {
		t.Errorf("resolvedAcquisition() with a nil callback = %q, want %q", got, AcquisitionAuto)
	}
}

// TestAcquisitionModeIsReadPerPass is the hot-reload assertion. The callback
// exists instead of a string copied in at construction for exactly one reason:
// the operator can change the setting while the process runs, and the next
// press of R F has to see it. A cached read passes every other test in this
// file and fails this one.
func TestAcquisitionModeIsReadPerPass(t *testing.T) {
	mode := AcquisitionAuto
	calls := 0
	s := NewAutoCookieService(
		writeWALCookieProfile(t, youtubeAuthRows()),
		filepath.Join(t.TempDir(), "cookies.txt"),
		NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return gatedBrowser() }
	s.AcquisitionMode = func() string { calls++; return mode }
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }

	first, err := s.RefreshCookiesDetailed(context.Background())
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if first.Ran {
		t.Fatal("under auto with a resolvable browser and an empty jar the pass must decline")
	}

	mode = AcquisitionProfile
	second, err := s.RefreshCookiesDetailed(context.Background())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if !second.Ran {
		t.Error("the second pass did not see the changed mode — the callback is being cached, " +
			"which makes the setting restart-required without anything saying so")
	}
	if calls < 2 {
		t.Errorf("the callback was consulted %d times across two passes, want at least 2", calls)
	}
}

// TestAcquisitionModeKeepsTheNoSourceOutcomes pins what the mode does NOT
// change: the pre-work missing-directory block runs BEFORE the decision point,
// so "profile mode with nowhere to read from" and "auto with nothing to
// launch" keep the sentinels rung 3 is built on. It is the guard against
// solving G4 by moving that block — a mode that produced a different sentinel
// here would dead-end R F on the install with the least to fall back on.
func TestAcquisitionModeKeepsTheNoSourceOutcomes(t *testing.T) {
	newService := func(t *testing.T, mode string, browser *DetectedBrowser) *AutoCookieService {
		t.Helper()
		s := NewAutoCookieService(
			filepath.Join(t.TempDir(), "no-such-profile"),
			filepath.Join(t.TempDir(), "cookies.txt"),
			NewCookieJar(), nopAutoCookieLogger{})
		s.detectBrowser = func() *DetectedBrowser { return browser }
		s.AcquisitionMode = func() string { return mode }
		return s
	}

	t.Run("profile mode with no profile dir keeps ErrProfileNotFound", func(t *testing.T) {
		s := newService(t, AcquisitionProfile, gatedBrowser())
		_, err := s.RefreshCookiesDetailed(context.Background())
		if !errors.Is(err, ErrProfileNotFound) {
			t.Errorf("err = %v, want ErrProfileNotFound — the operator asked to read a profile "+
				"and there is none, which is 'run setup first', not a browser problem", err)
		}
	})

	t.Run("auto with no browser keeps ErrNoBrowserFound", func(t *testing.T) {
		s := newService(t, AcquisitionAuto, nil)
		_, err := s.RefreshCookiesDetailed(context.Background())
		if !errors.Is(err, ErrNoBrowserFound) {
			t.Errorf("err = %v, want ErrNoBrowserFound", err)
		}
	})
}

// TestStartSetupIgnoresAcquisitionMode is R1's last clause, pinned. StartSetup
// is ACQUISITION — an explicit gesture in a visible window, and the thing that
// populates the profile every other path then reads. Gating it on a setting
// that says "do not launch a browser for a REFRESH" would make the setting
// unreachable from a fresh install in profile mode: no profile to import, and
// no way to create one. Asserted by the sentinel it does NOT return;
// gatedBrowser's path does not exist, so nothing executes (the same shape as
// TestStartSetupIgnoresTheBrowserLaunchGate).
func TestStartSetupIgnoresAcquisitionMode(t *testing.T) {
	s := NewAutoCookieService(
		filepath.Join(t.TempDir(), "profile"),
		filepath.Join(t.TempDir(), "cookies.txt"),
		NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return gatedBrowser() }
	s.AcquisitionMode = func() string { return AcquisitionProfile }
	t.Cleanup(func() { _ = s.CancelSetup() })

	err := s.StartSetup("youtube")
	if err == nil {
		t.Fatal("a browser at a path that does not exist must not launch")
	}
	if errors.Is(err, ErrNoBrowserFound) {
		t.Error("StartSetup refused for want of a browser in profile mode — the interactive " +
			"login is acquisition and must never consult cookies.acquisition, or a fresh " +
			"install in profile mode has no way to create the profile it is told to read")
	}
}
