package cookies

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// dangerousProfileDir is a path the launch guard refuses in every mode. Windows
// separators because that is what dangerousProfilePathSubstrings matches, and
// they are deliberately NOT being widened (audit G3: forward-slash variants
// would newly break Linux desktop users already pointing at a real profile).
// filepath.Abs prefixes a working directory on Linux; the substring survives
// either way, which is why the existing guard tests use the same literals.
const dangerousProfileDir = `C:\Users\test\AppData\Roaming\Mozilla\Firefox\Profiles\xxxxx.default-release`

// existingDangerousProfileDir creates a directory the guard refuses, for the
// tests that must get PAST the pre-work missing-directory check and reach the
// read-only site. The element carries the separators as a LITERAL: on Windows
// Join splits it into the nested Mozilla\Firefox\Profiles tree, and on Linux a
// backslash is an ordinary filename byte, so the lowercased absolute path
// contains `\mozilla\firefox\profiles` on both.
func existingDangerousProfileDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), `Mozilla\Firefox\Profiles\xxxxx.default-release`)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestLaunchGuardHoldsEveryLaunchSiteInEveryMode is the invariant G3 must not
// break. The guard stops a hostile config driving the user's REAL browser
// headlessly and exfiltrating the session through cookies.txt; that threat does
// not change with the acquisition mode, so neither does the guard. All four
// subprocess entry points, both modes — and nothing launches, because each one
// fast-fails on the cached verdict before it reaches an exec. Each assertion
// checks that the returned error IS the guard's own verdict, not merely that
// some error came back: a site that was bypassed can still fail downstream
// (e.g. a missing cookies.sqlite once it reaches the real profile tree), and
// that failure is not a refusal — asserting only err != nil cannot tell the
// two apart.
func TestLaunchGuardHoldsEveryLaunchSiteInEveryMode(t *testing.T) {
	want := validateBrowserProfileDirForLaunch(dangerousProfileDir)
	if want == nil {
		t.Fatal("fixture is not a dangerous profile dir — the test would prove nothing")
	}

	for _, mode := range []string{AcquisitionAuto, AcquisitionProfile} {
		t.Run(mode, func(t *testing.T) {
			s := NewAutoCookieService(dangerousProfileDir,
				filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), nopAutoCookieLogger{})
			s.AcquisitionMode = func() string { return mode }
			b := gatedBrowser()

			if err := s.startFirefoxSetup(b, "https://example.invalid"); err == nil || err.Error() != want.Error() {
				t.Errorf("startFirefoxSetup: got %v, want the launch guard's own refusal %q — a different error means the guard left the site", err, want)
			}
			if _, _, err := s.refreshFirefox(context.Background(), b); err == nil || err.Error() != want.Error() {
				t.Errorf("refreshFirefox: got %v, want the launch guard's own refusal %q — a different error means the guard left the site", err, want)
			}
			chromium := &DetectedBrowser{Type: "chrome", Path: "moombox-no-such-browser", Name: "Chrome"}
			if err := s.startChromiumSetup(chromium, "https://example.invalid"); err == nil || err.Error() != want.Error() {
				t.Errorf("startChromiumSetup: got %v, want the launch guard's own refusal %q — a different error means the guard left the site", err, want)
			}
			if _, _, err := s.refreshChromium(context.Background(), chromium); err == nil || err.Error() != want.Error() {
				t.Errorf("refreshChromium: got %v, want the launch guard's own refusal %q — a different error means the guard left the site", err, want)
			}
		})
	}
}

// TestReadOnlyImportIsGatedOnTheOptIn is G3 at the site itself.
//
// Reading a real profile is safe and the codebase already argues it:
// snapshotFirefoxCookieDB copies cookies.sqlite AND its -wal sidecar into a
// 0700 temp dir and opens the COPY mode=ro, so SQLite never writes into the
// user's profile. The residual concern is EXFILTRATION — imported cookies land
// in cookies.txt — which is exactly the surface dpapi_fallback treats as
// opt-in, so the relaxation is tied to the opt-in rather than granted
// unconditionally. The refused row is the junction guard: without it "no
// error under profile" could mean the guard was deleted rather than gated.
func TestReadOnlyImportIsGatedOnTheOptIn(t *testing.T) {
	for _, tc := range []struct {
		mode      string
		wantRefus bool
		why       string
	}{
		{AcquisitionAuto, true, "the default must keep refusing — nobody opted in"},
		{AcquisitionProfile, false, "the opt-in is the whole point: a read-only import of a real profile proceeds"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			s := NewAutoCookieService(dangerousProfileDir,
				filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), nopAutoCookieLogger{})
			s.AcquisitionMode = func() string { return tc.mode }

			_, err := s.importProfileCookies()
			refused := errors.Is(err, ErrProfileDirNotOptedIn)
			if refused != tc.wantRefus {
				t.Errorf("refused = %v, want %v — %s (err: %v)", refused, tc.wantRefus, tc.why, err)
			}
			if !tc.wantRefus && err == nil {
				t.Fatal("the path does not exist, so some error is expected — just not the opt-in refusal")
			}
		})
	}
}

// TestRealProfileTreeReadsOnlyOnTheOptIn is G3 driven through the real entry
// point, on a profile tree that EXISTS and holds a cookie database — the
// desktop shape the setting was built for. The pre-work missing-directory
// block would otherwise answer before the read-only site is reached, which is
// why the sibling test above calls the site directly and this one does not.
//
// The auto row gates the browser (auto_enabled = false), which is the ONLY
// way a desktop reached the import path before this arc — and it is refused,
// with the setting named. The profile row reads it and verifies.
func TestRealProfileTreeReadsOnlyOnTheOptIn(t *testing.T) {
	newService := func(t *testing.T, mode string) (*AutoCookieService, *int) {
		t.Helper()
		dir := existingDangerousProfileDir(t)
		writeWALCookieProfileAt(t, dir, youtubeAuthRows())
		s := NewAutoCookieService(dir, filepath.Join(t.TempDir(), "cookies.txt"),
			NewCookieJar(), nopAutoCookieLogger{})
		s.detectBrowser = func() *DetectedBrowser { return gatedBrowser() }
		s.AcquisitionMode = func() string { return mode }
		verified := 0
		s.VerifyYouTubeAuth = func(context.Context) (bool, error) { verified++; return true, nil }
		s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
		return s, &verified
	}

	t.Run("auto with the browser gated is refused and names the setting", func(t *testing.T) {
		s, verified := newService(t, AcquisitionAuto)
		s.BrowserLaunchAllowed = func() bool { return false }
		_, err := s.RefreshCookiesDetailed(context.Background())
		if !errors.Is(err, ErrProfileDirNotOptedIn) {
			t.Fatalf("err = %v, want ErrProfileDirNotOptedIn — a real profile tree must not be "+
				"read without the opt-in, even on the browser-free path", err)
		}
		if !strings.Contains(err.Error(), "acquisition") {
			t.Errorf("the refusal does not name the setting that lifts it: %q", err.Error())
		}
		if *verified != 0 {
			t.Error("verification ran on a pass that refused to read")
		}
	})

	t.Run("profile reads it read-only and verifies", func(t *testing.T) {
		s, verified := newService(t, AcquisitionProfile)
		result, err := s.RefreshCookiesDetailed(context.Background())
		if err != nil {
			t.Fatalf("the opt-in did not lift the guard: %v", err)
		}
		if !result.Ran || *verified == 0 {
			t.Errorf("Ran = %v, verified = %d — the import did not happen", result.Ran, *verified)
		}
	})
}

// TestReadOnlyRefusalDoesNotClaimALaunch pins the message split. The launch
// guard says "refusing to launch a headless session against it" — which, on a
// path that launches nothing and reads a copy, describes an event that did not
// happen and sends the operator looking for a browser process. Asserted in both
// directions: the launch sentence must still say it, the read-only one must not.
func TestReadOnlyRefusalDoesNotClaimALaunch(t *testing.T) {
	launchErr := validateBrowserProfileDirForLaunch(dangerousProfileDir)
	if launchErr == nil {
		t.Fatal("the launch guard stopped refusing a real browser profile tree")
	}
	if !strings.Contains(launchErr.Error(), "refusing to launch") {
		t.Errorf("the launch refusal no longer says what it refused: %q", launchErr.Error())
	}

	s := NewAutoCookieService(dangerousProfileDir,
		filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), nopAutoCookieLogger{})
	s.AcquisitionMode = func() string { return AcquisitionAuto }
	_, readErr := s.importProfileCookies()
	if readErr == nil {
		t.Fatal("the read-only site stopped refusing under the default mode")
	}
	if strings.Contains(readErr.Error(), "refusing to launch") {
		t.Errorf("the read-only refusal claims a launch that never happens: %q", readErr.Error())
	}
	if !strings.Contains(readErr.Error(), "acquisition") {
		t.Errorf("the read-only refusal does not name the setting that lifts it, so the operator "+
			"has no next step: %q", readErr.Error())
	}
}

// TestReadOnlyRefusalIsNotTheLadderBottomRung protects the property the six
// profile-import sentinels beside it already have. Rung 3 means there is NO
// profile, and both manual surfaces fall through to the in-process refresh on
// it. This refusal means the profile IS there and the config says do not read
// it — a diagnosable state with a one-line remedy, and folding it into a
// fallback would replace that remedy with a recheck that cannot apply it.
func TestReadOnlyRefusalIsNotTheLadderBottomRung(t *testing.T) {
	s := NewAutoCookieService(dangerousProfileDir,
		filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), nopAutoCookieLogger{})
	s.AcquisitionMode = func() string { return AcquisitionAuto }
	_, err := s.importProfileCookies()
	if IsNoBrowserProfile(err) {
		t.Error("the opt-in refusal landed on rung 3, which drops the only sentence naming the fix")
	}
}

// TestStartupSeedFollowsTheOptIn covers the second read-only site.
//
// decideStartupSeed has TWO short-circuits this arc touches, and both are
// wrong in profile mode for the same reason: they encode "a browser is
// available, so the browser path owns this install". In profile mode the
// operator has said it does not.
func TestStartupSeedFollowsTheOptIn(t *testing.T) {
	t.Run("a browser present no longer stands down in profile mode", func(t *testing.T) {
		s := NewAutoCookieService(
			writeWALCookieProfile(t, youtubeAuthRows()),
			filepath.Join(t.TempDir(), "cookies.txt"),
			NewCookieJar(), nopAutoCookieLogger{})
		s.detectBrowser = func() *DetectedBrowser { return gatedBrowser() }

		s.AcquisitionMode = func() string { return AcquisitionAuto }
		if got := s.decideStartupSeed(); got != autoImportBrowserPresent {
			t.Errorf("auto: verdict = %v, want autoImportBrowserPresent — a desktop's refresh path "+
				"owns the profile and an unsolicited startup import would launch a browser "+
				"nobody asked for", got)
		}

		s.AcquisitionMode = func() string { return AcquisitionProfile }
		if got := s.decideStartupSeed(); got != autoImportOK {
			t.Errorf("profile: verdict = %v, want autoImportOK — the operator asked for the "+
				"profile to be the source, and the boot import has nothing to lose here", got)
		}
	})

	t.Run("the guard still stands down under the default mode", func(t *testing.T) {
		s := NewAutoCookieService(dangerousProfileDir, filepath.Join(t.TempDir(), "cookies.txt"),
			NewCookieJar(), nopAutoCookieLogger{})
		s.detectBrowser = func() *DetectedBrowser { return nil }
		s.AcquisitionMode = func() string { return AcquisitionAuto }
		if got := s.decideStartupSeed(); got != autoImportNotConfigured {
			t.Errorf("verdict = %v, want autoImportNotConfigured", got)
		}
	})
}

// TestPeriodicTickInProfileModeObeysTheImportGuard is the third host-inferring
// site, and the twin of TestPeriodicTickWithABrowserIgnoresTheImportGuard.
//
// The tick decides "is this a browser-free import?" by refreshBrowser() == nil.
// In profile mode a browser resolves, so that answer is "no" — and the pass it
// then runs is an IMPORT, because the mode forces one. Without this arc's
// change the timer would re-read the operator's real profile over a live
// cookies.txt on every tick, which is exactly the automatic import the owner
// ruled out and the ONE rule automaticImportGuard exists to hold. Same fixture
// as the browser test: a populated cookies.txt, a browser present.
func TestPeriodicTickInProfileModeObeysTheImportGuard(t *testing.T) {
	cookiePath := ytAuthCookieFile(t)
	jar := NewCookieJar()
	if err := jar.Load(cookiePath); err != nil {
		t.Fatalf("load the fixture cookie file: %v", err)
	}

	log := &recordingCookieLogger{}
	s := NewAutoCookieService(writeWALCookieProfile(t, youtubeAuthRows()), cookiePath, jar, log)
	s.detectBrowser = func() *DetectedBrowser { return gatedBrowser() }
	s.AcquisitionMode = func() string { return AcquisitionProfile }
	var verified atomic.Int64
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { verified.Add(1); return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartPeriodicRefresh(ctx, 20*time.Millisecond)

	if !waitForStandDowns(log, standDownsObserved) {
		t.Fatalf("the periodic timer never stood a profile-mode tick down on a populated cookies.txt "+
			"(imports=%d, stand-downs=%d) — the tick still infers the import from the host, so a "+
			"desktop in profile mode re-reads its real profile over live credentials on a schedule",
			verified.Load(), log.count(guardSkipped))
	}
	if got := verified.Load(); got != 0 {
		t.Errorf("the timer imported %d time(s) over a populated cookies.txt in profile mode — "+
			"an automatic browser-free import may only run when there is nothing to lose", got)
	}
}
