package cookies

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStartSetupRejectsAnUnknownPlatform is COOKIES-8 at the service boundary,
// which is where the non-HTTP callers live (the TUI's E L chord and the
// first-run wizard both call StartSetup directly).
//
// Before this, StartSetup forwarded any string: the login URL fell through to
// YouTube's because only the literal "twitch" selects the other one, while
// targetPlatform kept the wrong value — so the Chromium finish skipped
// cdpEnsurePageTarget and the wizard judged BOTH platforms as if youtube had
// been asked. A wrong input silently accepted, and accepted far enough to open
// a browser on it.
//
// The rows are the shapes a caller actually gets wrong: a platform Moombox does
// not support, the right word in the wrong case, the right word with trailing
// whitespace from a copy-paste, and a path-ish value. None is normalised on
// purpose — StartSetup is not the place to guess what was meant, and the two
// accepted values are the two the finish branches can act on.
//
// Mutants:
//   - drop the check -> err is nil (or a browser error) and the service claims
//     the setup slot for a platform no finish branch handles.
//   - check AFTER the lock, the stopped gate or the slot claim -> the stopped
//     assertion at the end fails: a wrong value is wrong whatever state the
//     service is in, and a claim taken for an input that was never going to
//     work is a claim the caller with a usable one waits behind.
//   - check relocated BELOW the browser resolution or the launch -> the
//     resolution counter is 1 and the profile dir exists. The five value rows
//     cannot see that relocation on their own (the launcher is made to fail, so
//     the sentinel still comes back), which is why the three state assertions
//     exist rather than more values.
//   - accept case-insensitively or trim -> the "YouTube" and "youtube " rows
//     fail; both would reach the finish branches, which compare the literal.
//   - reject "" -> TestStartSetupTreatsAnAbsentPlatformAsYouTube fails; an
//     absent field is not a wrong one.
func TestStartSetupRejectsAnUnknownPlatform(t *testing.T) {
	// A profile dir that does NOT exist yet. StartSetup's os.MkdirAll runs
	// between the slot claim and the launch, so its absence afterwards is a
	// state-level statement that neither ever happened.
	profileDir := filepath.Join(t.TempDir(), "profile")
	s := NewAutoCookieService(profileDir, filepath.Join(t.TempDir(), "cookies.txt"),
		NewCookieJar(), nopAutoCookieLogger{})

	// Browser guard, not decoration: if the validation regresses, StartSetup
	// falls through to browser detection and a machine with a real browser
	// installed OPENS ONE from this test. ConfiguredBrowserOverride is the
	// exported seam resolvedBrowser consults first, so a path inside a fresh
	// temp dir substitutes a browser that provably cannot launch.
	//
	// It is also the COUNTER. resolvedBrowser is the only way this service can
	// name a browser and it runs immediately before startChromiumSetup /
	// startFirefoxSetup, the only two calls that open a window — so one
	// consultation is what every launch is preceded by, and zero consultations
	// is the closest this package can come to "the launcher was never called"
	// without a seam inside the launchers themselves.
	unlaunchable := filepath.Join(t.TempDir(), "not-a-browser.exe")
	resolutions := 0
	s.ConfiguredBrowserOverride = func() (string, string) {
		resolutions++
		return unlaunchable, "chrome"
	}

	for _, p := range []string{"mastodon", "YouTube", "youtube ", "../youtube", "kick"} {
		err := s.StartSetup(p)
		if !errors.Is(err, ErrUnsupportedPlatform) {
			t.Errorf("StartSetup(%q) err = %v, want ErrUnsupportedPlatform — an unknown platform is "+
				"driven with the YouTube login URL and then judged as YouTube", p, err)
		}
		// The value is in the message: the operator typed it, and both
		// dashboards render sentinel text verbatim.
		if err != nil && !strings.Contains(err.Error(), p) {
			t.Errorf("StartSetup(%q) err = %q, want it to name the value it refused", p, err)
		}
	}

	// THE ORDER, from state rather than from one carefully chosen service
	// state. Each of the three names a different waypoint of StartSetup, and
	// the rows above cannot see any of them: ConfiguredBrowserOverride makes
	// the launcher fail, so a check relocated BELOW the launch still answers
	// ErrUnsupportedPlatform and every row stays green.
	if resolutions != 0 {
		t.Errorf("a rejected platform resolved a browser %d time(s) — the rule ran below "+
			"resolvedBrowser, which is immediately before the launch, so on a host with a real "+
			"browser installed a wrong value opens a window before it is refused", resolutions)
	}
	if _, err := os.Stat(profileDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the browser profile dir exists after a rejected platform (stat err = %v) — "+
			"StartSetup's MkdirAll sits between the slot claim and the launch, so the rule ran "+
			"below the claim", err)
	}
	s.mu.Lock()
	claimed := s.setupClaimed
	target := s.targetPlatform
	s.mu.Unlock()
	if claimed {
		t.Error("the setup slot was claimed for a rejected platform — validate before the claim, " +
			"or a wrong input locks out the right one")
	}
	if target != "" {
		t.Errorf("targetPlatform = %q after a rejected platform — the rule ran after the slot was "+
			"populated, which is after the browser was prepared", target)
	}

	// The ORDER, made observable: a stopped service is the one state this
	// package can force whose gate sits inside the lock, right where a
	// late-validating StartSetup would answer instead. ErrServiceStopped here
	// would mean the platform rule had moved below the claim.
	s.Stop()
	if err := s.StartSetup("mastodon"); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Errorf("StartSetup(\"mastodon\") on a stopped service = %v, want ErrUnsupportedPlatform — "+
			"the value is wrong whatever state the service is in, and \"try again shortly\" sends "+
			"the caller back with the same unusable input", err)
	}
}

// TestStartSetupTreatsAnAbsentPlatformAsYouTube pins the half of COOKIES-8 that
// must NOT change. The empty string is what a caller that omits the field sends
// — the dashboard's start handler and the first-run wizard both do — and it has
// always meant YouTube.
//
// Asserted by what the rejection is NOT: a browser must never launch here, so
// the service is pointed at an unlaunchable path and the assertion is that ""
// gets past the platform rule and fails on the BROWSER instead.
//
// Mutant: fold "" into the refusal -> this fails, and every omitted-field
// caller in the tree stops working.
func TestStartSetupTreatsAnAbsentPlatformAsYouTube(t *testing.T) {
	s := NewAutoCookieService(t.TempDir(), filepath.Join(t.TempDir(), "cookies.txt"),
		NewCookieJar(), nopAutoCookieLogger{})
	unlaunchable := filepath.Join(t.TempDir(), "not-a-browser.exe")
	s.ConfiguredBrowserOverride = func() (string, string) { return unlaunchable, "chrome" }

	err := s.StartSetup("")
	if errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("StartSetup(\"\") = %v — an absent platform is not a wrong one", err)
	}

	s.mu.Lock()
	target := s.targetPlatform
	s.mu.Unlock()
	if target != "youtube" {
		t.Errorf("targetPlatform after StartSetup(\"\") = %q, want %q", target, "youtube")
	}
}
