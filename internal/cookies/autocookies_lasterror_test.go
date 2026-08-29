package cookies

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// This file owns the lastError WRITE POLICY — the field's doc comment on
// AutoCookieService states it; these are the assertions.
//
// The field is published as AutoCookieStatus.LastError and is meant to sit
// beside the cookie status in both dashboards, where it reads as "your
// recordings will fail". So the dangerous half is not the SET, it is the CLEAR:
// clearing asserts "whatever was recorded is not wrong any more", and a path
// with no basis for that assertion retracts a true report.

// TestCleanupAfterAFailedSetupKeepsLastError is the Q5 pin.
//
// cleanup() runs on EVERY setup exit path, the failed ones included:
// FinishSetup's empty-profile, unreadable-cookie-file and merge-abort exits all
// call setError and then cleanup, in that order and microseconds apart. A clear
// inside cleanup would therefore erase the message the failure had just
// produced — the dialog would report a failure that the settings page had no
// record of, and the operator would have nothing to act on once the dialog was
// dismissed.
//
// White-box and in the real order, deliberately. The behavioural route needs a
// live setup slot with a browser in it, which no test in this package may
// create; and the next implementer restructuring cleanup needs a failure that
// names the FIELD rather than one that reports a missing dialog message.
// TestCleanupDoesNotEraseTheCancelFlag is this test's twin for `cancelled`, and
// exists for the same reason.
func TestCleanupAfterAFailedSetupKeepsLastError(t *testing.T) {
	const failure = "no login detected — sign in before finishing setup"

	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	s.setError(failure)
	if lastErrorSnapshot(s) != failure {
		t.Fatalf("premise lost: setError did not record %q", failure)
	}

	s.cleanup()

	if got := lastErrorSnapshot(s); got != failure {
		t.Fatalf("cleanup() erased the failure message (%q -> %q). Every failing FinishSetup exit "+
			"calls setError and then cleanup, so clearing here retracts the report a moment "+
			"after it is made and the operator is left with a dialog error and a settings page "+
			"that says everything is fine", failure, got)
	}

	// And from any OTHER path too: cleanupLocked is cleanup's body and is
	// reached inline by the abandoned-setup reap, which runs from GetStatus and
	// ReloginStatus — i.e. from an ordinary dashboard poll, on a schedule the
	// operator does not control.
	s.mu.Lock()
	s.cleanupLocked()
	s.mu.Unlock()
	if got := lastErrorSnapshot(s); got != failure {
		t.Errorf("cleanupLocked() erased the failure message (%q -> %q) — the reap runs from a "+
			"status poll, so this would clear the field at an interval nobody asked for",
			failure, got)
	}
}

// TestStartSetupClearsLastError is the one clear that is about INTENT rather
// than evidence, and it is correct: a new setup attempt is beginning, the
// recorded message belongs to an attempt that is over, and leaving it up means
// the wizard opens under a red line about something the user is already fixing.
//
// Driven through the real StartSetup, with a browser path that provably does
// not exist so the launch fails immediately after the clear. The launch failure
// is the point rather than a nuisance: the clear happens at the slot claim, so
// it must survive an attempt that gets no further.
func TestStartSetupClearsLastError(t *testing.T) {
	captureKills(t)

	s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
	unlaunchable := filepath.Join(t.TempDir(), "not-a-browser.exe")
	s.detectBrowser = func() *DetectedBrowser {
		return &DetectedBrowser{Type: "chrome", Path: unlaunchable, Name: "unlaunchable test browser"}
	}
	s.setError("YouTube auth verification failed — manual re-login required")
	if lastErrorSnapshot(s) == "" {
		t.Fatal("premise lost: nothing was recorded, so a clear below proves nothing")
	}

	err := s.StartSetup("youtube")

	// The launch must be what failed. If StartSetup bailed EARLIER — no browser
	// resolved, a profile dir it could not create — it never reached the clear
	// and a nil lastError below would mean something else entirely.
	if err == nil {
		t.Fatal("fixture is broken — a browser path inside a fresh temp dir must not launch")
	}
	if errors.Is(err, ErrNoBrowserFound) || errors.Is(err, ErrSetupCancelled) {
		t.Fatalf("fixture is broken — StartSetup gave up before the slot claim (%v), so the clear "+
			"under test was never reached", err)
	}
	if !strings.Contains(err.Error(), "start browser") {
		t.Fatalf("fixture is broken — expected the launch itself to fail, got %v", err)
	}

	if got := lastErrorSnapshot(s); got != "" {
		t.Errorf("StartSetup left %q on the status. A setup that is starting is the user already "+
			"acting on whatever the message said; showing it beside the wizard reports a problem "+
			"they are in the middle of fixing", got)
	}
}

// TestRefreshThatRenewedClearsLastError is the other legitimate clear, and the
// only one that is evidence-based: this pass produced the credentials it then
// verified, so a previously recorded failure genuinely is not true any more.
//
// Its sibling `default` arm — verified, but NOT renewed — must not clear, and
// that asymmetry is the whole reason the switch has two arms. The second half
// of this test pins it: a pass whose browser did nothing has established that
// the credentials on disk work, not that the refresh mechanism does, and
// retracting "the browser profile contained no cookies to refresh from" off
// that is how a twice-broken refresh presents a clean bill of health.
//
// Both halves run the browser-free import path, which is the one a container
// uses. Renewed is `importedFromProfile || browserActed`, so the import half is
// renewed by construction and the browser half is not once its launch reports
// nothing happened.
func TestRefreshThatRenewedClearsLastError(t *testing.T) {
	const stale = "the browser profile contained no cookies to refresh from"

	t.Run("renewed — clears", func(t *testing.T) {
		s := NewAutoCookieService(writeWALCookieProfile(t, youtubeAuthRows()),
			filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), nopAutoCookieLogger{})
		s.detectBrowser = func() *DetectedBrowser { return nil } // browserless: the import path
		s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
		s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
		s.setError(stale)

		result, err := s.RefreshCookiesDetailed(context.Background())
		if err != nil {
			t.Fatalf("RefreshCookiesDetailed: %v", err)
		}
		if !result.Renewed || result.YouTube != RefreshOK {
			t.Fatalf("premise lost: renewed=%v youtube=%v — this arm needs a pass that produced "+
				"the credentials it verified", result.Renewed, result.YouTube)
		}
		if got := lastErrorSnapshot(s); got != "" {
			t.Errorf("a pass that produced and verified fresh credentials left %q up. This is the "+
				"one clear the field's policy calls evidence-based: whatever the old message "+
				"said, this pass just demonstrated otherwise", got)
		}
	})

	t.Run("verified but not renewed — keeps", func(t *testing.T) {
		cookiePath := ytAuthCookieFile(t)
		jar := NewCookieJar()
		if err := jar.Load(cookiePath); err != nil {
			t.Fatalf("load the fixture cookie file: %v", err)
		}
		s := NewAutoCookieService(writeWALCookieProfile(t, youtubeAuthRows()), cookiePath, jar,
			nopAutoCookieLogger{})
		s.detectBrowser = func() *DetectedBrowser {
			return &DetectedBrowser{Type: "chrome", Path: "moombox-no-such-browser", Name: "Chrome"}
		}
		s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return true, nil }
		s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
		// A browser that reported no navigation: the read "worked" off a profile
		// the previous session populated, and nothing about this pass renewed
		// anything.
		stubBrowserRefresh(t, signedInBrowserRows, false, nil)
		s.setError(stale)

		result, err := s.RefreshCookiesDetailed(context.Background())
		if err != nil {
			t.Fatalf("RefreshCookiesDetailed: %v", err)
		}
		if result.Renewed || result.YouTube != RefreshOK {
			t.Fatalf("premise lost: renewed=%v youtube=%v — this arm needs a pass that verified "+
				"without renewing", result.Renewed, result.YouTube)
		}
		if got := lastErrorSnapshot(s); got != stale {
			t.Errorf("LastError = %q, want the previous message kept. Clearing is an assertion "+
				"— \"whatever was wrong is not wrong any more\" — and a pass whose browser did "+
				"nothing has no basis for it: the credentials on disk verifying says nothing "+
				"about whether the refresh mechanism works, which is what the old message was "+
				"about", got)
		}
	})
}
