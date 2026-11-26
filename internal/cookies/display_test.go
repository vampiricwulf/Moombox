package cookies

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// On Linux with no display the browser login is refused up front with a
// sentence naming the browser-free routes (owner ruling R4-7). Launched
// anyway, Firefox exited at once while both UIs said a window had opened,
// and Chromium waited 15 s for a CDP endpoint nothing would open.
//
// Mutants it kills: the check dropped (a browser is resolved and the
// profile prepared); the check moved below the slot claim or the browser
// resolution (the state assertions fail).
func TestStartSetupRefusesAHostWithNoDisplay(t *testing.T) {
	t.Cleanup(SetHostLacksDisplayForTesting(true))
	profileDir := filepath.Join(t.TempDir(), "profile")
	s := NewAutoCookieService(profileDir, filepath.Join(t.TempDir(), "cookies.txt"),
		NewCookieJar(), nopAutoCookieLogger{})
	// A browser that provably cannot launch, and the counter of every launch.
	resolutions := 0
	s.ConfiguredBrowserOverride = func() (string, string) {
		resolutions++
		return filepath.Join(t.TempDir(), "not-a-browser.exe"), "chrome"
	}

	for _, p := range []string{"youtube", "twitch"} {
		if err := s.StartSetup(p); !errors.Is(err, ErrNoDisplay) {
			t.Errorf("StartSetup(%q) with no display: err = %v, want ErrNoDisplay", p, err)
		}
	}
	if resolutions != 0 {
		t.Errorf("a browser was resolved %d time(s) on a host with no display", resolutions)
	}
	if _, err := os.Stat(profileDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the profile dir was prepared on a host with no display (stat err = %v)", err)
	}
	s.mu.Lock()
	claimed := s.setupClaimed
	s.mu.Unlock()
	if claimed {
		t.Error("the setup slot was claimed on a host with no display")
	}
}

// A browser gone before its CDP endpoint answered ends the wait at once with
// ErrBrowserExitedEarly, not after the whole timeout with a bare "timeout
// waiting for CDP endpoint". The answer comes from the caller's liveness
// check (the Job Object or process group, never an exit code); while that
// says the browser is alive, the wait runs on.
//
// Mutant it kills: waitForCDP ignoring browserGone (the 5 s timeout runs out
// and the error is the timeout's).
func TestWaitForCDPStopsWhenTheBrowserIsGone(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // nothing listens: every poll fails, as before a browser opens

	polls := 0
	start := time.Now()
	err = waitForCDP(t.Context(), port, 5*time.Second, func() bool {
		polls++
		return polls >= 2 // alive on the first poll, gone on the second
	})
	if !errors.Is(err, ErrBrowserExitedEarly) {
		t.Fatalf("err = %v, want ErrBrowserExitedEarly", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v to notice the browser was gone, want well under the 5 s timeout", elapsed)
	}
	if polls != 2 {
		t.Errorf("liveness asked %d times, want 2 — a live browser must be waited for", polls)
	}
}
