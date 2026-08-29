package cookies

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingCookieLogger keeps every message the service logs, so a test can ask
// which BRANCH a pass took without a browser, a network or a fixture.
//
// Messages are compared for EXACT equality, never containment: the two lines
// this file discriminates on differ by their tail, and a substring check
// against the shorter one would be satisfied by the longer.
type recordingCookieLogger struct {
	mu   sync.Mutex
	msgs []string
}

func (l *recordingCookieLogger) record(msg string) {
	l.mu.Lock()
	l.msgs = append(l.msgs, msg)
	l.mu.Unlock()
}

func (l *recordingCookieLogger) Debug(msg string, _ ...any) { l.record(msg) }
func (l *recordingCookieLogger) Info(msg string, _ ...any)  { l.record(msg) }
func (l *recordingCookieLogger) Warn(msg string, _ ...any)  { l.record(msg) }
func (l *recordingCookieLogger) Error(msg string, _ ...any) { l.record(msg) }

// count returns how many times an exact message was logged.
func (l *recordingCookieLogger) count(msg string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, m := range l.msgs {
		if m == msg {
			n++
		}
	}
	return n
}

// lastErrorSnapshot reads the field both dashboards render beside the cookie
// status, under the lock the refresh paths write it with.
func lastErrorSnapshot(s *AutoCookieService) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastError == nil {
		return ""
	}
	return *s.lastError
}

// waitFor polls until cond holds, and reports whether it did within the budget.
// Used only for the direction that must EVENTUALLY happen; the direction that
// must never happen is asserted over a fixed window instead.
func waitFor(cond func() bool, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// TestPeriodicLoopPicksUpAProfileThatAppearsAtRuntime is the two-sided
// acceptance for moving the profile-directory question out of main.go.
//
// It used to be answered ONCE, by an os.Stat wrapped around
// StartPeriodicRefresh at boot. Completing setup is what CREATES that directory
// (StartSetup MkdirAlls it), so an operator who turned cookies.auto_enabled on
// and then ran setup — the exact sequence an "auth lost" notification asks for
// — got no timer at all until the next restart, and nothing said so: no setting
// had changed by then, so even the restart-required labelling could not fire.
//
// Both sides are asserted, because either alone has a trivially wrong fix:
//
//  1. BEFORE the profile exists the loop must be SILENT. Deleting the check
//     outright satisfies side 2 and fails here — a pass with no profile and no
//     browser sets LastError, which is a permanent red line on the settings page
//     of an install whose operator has not been asked to do anything yet.
//  2. AFTER it appears the loop must pick it up with NO restart. A check that
//     answers "no" forever satisfies side 1 and fails here.
//
// The observable is real work: VerifyYouTubeAuth runs only once a pass has
// imported cookies out of the profile. Nothing is stubbed but browser detection
// and the two auth checks; the stats are the real os.Stat, against a directory
// this test genuinely creates midway through.
func TestPeriodicLoopPicksUpAProfileThatAppearsAtRuntime(t *testing.T) {
	// Deliberately a path that does not exist yet, and NOT one t.TempDir hands
	// back already created: "setup has not been run" is the starting state.
	profileDir := filepath.Join(t.TempDir(), "browser-profile")
	log := &recordingCookieLogger{}
	s := NewAutoCookieService(profileDir, filepath.Join(t.TempDir(), "cookies.txt"),
		NewCookieJar(), log)
	// A browserless host, so a pass that runs takes the profile-import branch.
	// The boot seed is a separate entry point (StartProfileSeed) and is not
	// started here at all, so the ticker begins immediately.
	s.detectBrowser = func() *DetectedBrowser { return nil }
	var verified atomic.Int64
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) { verified.Add(1); return true, nil }
	s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const interval = 25 * time.Millisecond
	s.StartPeriodicRefresh(ctx, interval)

	// --- Side 1: no profile yet. Many ticks, no work, nothing to show a user.
	time.Sleep(10 * interval)
	if got := verified.Load(); got != 0 {
		t.Errorf("%d auth verifications ran before the profile existed, want 0 — the loop is running "+
			"passes against a source that is not there", got)
	}
	if e := lastErrorSnapshot(s); e != "" {
		t.Errorf("LastError = %q before setup was ever run. Both dashboards render that beside the "+
			"cookie status, so a flag-on install that has not been set up yet would show a permanent "+
			"error for a state nobody has been asked to fix", e)
	}
	if n := log.count("periodic auto-cookie refresh triggered"); n != 0 {
		t.Errorf("the loop triggered %d passes with no profile directory, want 0", n)
	}
	if log.count("periodic auto-cookie refresh skipped — no browser profile directory yet") == 0 {
		t.Fatal("the loop is not ticking at all, so side 2 below would prove nothing about the " +
			"start condition")
	}

	// --- Side 2: setup completes. No restart happens anywhere in this test.
	writeWALCookieProfileAt(t, profileDir, youtubeAuthRows())

	if !waitFor(func() bool { return verified.Load() > 0 }, 10*time.Second) {
		t.Fatalf("the profile appeared and the running loop never read it (LastError=%q). Completing "+
			"setup at runtime must be picked up by the next tick — requiring a restart is the bug the "+
			"boot-time os.Stat caused", lastErrorSnapshot(s))
	}
}

// TestPeriodicLoopKeepsItsBrowserWhenTheFlagFlipsOff is F2.
//
// The browser gate reads cookies.auto_enabled LIVE, which is right for the
// manual triggers — R F on a hand-updated profile must import the moment the
// operator switches the browser off — and wrong for this loop. Boot with the
// flag on, switch it off without restarting, and the timer kept ticking while
// silently changing mechanism: a browser-free re-import of a browser profile,
// on a schedule, forever. That is the one behaviour the periodic loop is
// deliberately NOT given (nothing changes the profile between ticks, so it
// re-reads identical bytes), and arriving at it by accident is worse than
// arriving at it on purpose.
//
// So the loop is exempt from the gate: the flag IS the timer, main.go answered
// it at boot, and both settings pages label the flag restart-required.
//
// The discriminator is the same inversion TestBrowserLaunchGateDropsTheBrowser-
// NotTheRefresh uses, driven through the real goroutine. With an EMPTY JAR and
// a populated profile:
//
//   - browser kept (exempt, correct) -> browser branch -> declines on the empty
//     jar, logging its own line. No import, no verification, no launch.
//   - browser dropped (gated, the bug) -> import branch -> imports and verifies.
//
// Both halves are asserted, and the second service is the junction guard: the
// SAME service state driven through the exported API — which every manual
// trigger uses — must import, or "no verification" here would prove only that
// the observable is dead.
func TestPeriodicLoopKeepsItsBrowserWhenTheFlagFlipsOff(t *testing.T) {
	newService := func(t *testing.T) (*AutoCookieService, *recordingCookieLogger, *atomic.Int64) {
		t.Helper()
		log := &recordingCookieLogger{}
		s := NewAutoCookieService(
			writeWALCookieProfile(t, youtubeAuthRows()),
			filepath.Join(t.TempDir(), "cookies.txt"),
			NewCookieJar(), log)
		// Installed and working. Never executed: every path below either
		// declines before the launch or drops the browser first.
		s.detectBrowser = func() *DetectedBrowser { return gatedBrowser() }
		// The operator switched cookies.auto_enabled off after boot.
		s.BrowserLaunchAllowed = func() bool { return false }
		var verified atomic.Int64
		s.VerifyYouTubeAuth = func(context.Context) (bool, error) { verified.Add(1); return true, nil }
		s.VerifyTwitchAuth = func(context.Context) (bool, error) { return false, nil }
		return s, log, &verified
	}

	// The line the BROWSER branch logs when it declines on an empty jar. It is
	// produced nowhere else, so counting it proves both that the loop ran and
	// which branch it took.
	const browserBranchDeclined = "skipping cookie refresh — no platforms have cookies"

	t.Run("the running loop", func(t *testing.T) {
		s, log, verified := newService(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		const interval = 20 * time.Millisecond
		s.StartPeriodicRefresh(ctx, interval)
		// Wait for EITHER branch to show itself, so a loop that took the wrong
		// one fails on the assertion that names the policy rather than on a
		// liveness timeout that names nothing.
		if !waitFor(func() bool {
			return log.count(browserBranchDeclined) > 0 || verified.Load() > 0
		}, 10*time.Second) {
			t.Fatalf("the loop never reached a refresh pass, so nothing below is measuring the "+
				"policy (LastError=%q)", lastErrorSnapshot(s))
		}
		// A few more ticks, to catch a policy that is only right on the first.
		time.Sleep(5 * interval)

		if got := verified.Load(); got != 0 {
			t.Errorf("the periodic loop imported from the browser profile %d times after the flag "+
				"was switched off. Flipping cookies.auto_enabled off must not leave the timer "+
				"running on a different mechanism — re-importing an unchanged profile on a schedule "+
				"is exactly what the flag being off is supposed to prevent", got)
		}
		if n := log.count(browserBranchDeclined); n == 0 {
			t.Error("the loop ran passes that took neither branch")
		}
	})

	t.Run("the junction guard: the same state through the exported API", func(t *testing.T) {
		s, log, verified := newService(t)
		result, err := s.RefreshCookiesDetailed(context.Background())
		if err != nil {
			t.Fatalf("the manual path must not error here: %v", err)
		}
		if !result.Ran {
			t.Error("RefreshCookiesDetailed declined with the flag off. Every caller of it is a live " +
				"operator gesture, and those must honour the setting by dropping the browser and " +
				"importing — see BrowserLaunchAllowed")
		}
		if verified.Load() == 0 {
			t.Error("the exported path did not import either, so the loop's zero above proves nothing " +
				"— the observable is dead, not the mechanism")
		}
		if n := log.count(browserBranchDeclined); n != 0 {
			t.Errorf("the exported path took the browser branch %d times with the flag off — the gate "+
				"is no longer reaching it", n)
		}
	})
}

// TestPeriodicRefreshHasSourceIsTheProfileDirectory pins the precondition
// itself, at the level a mutation is most likely to land: a constant.
func TestPeriodicRefreshHasSourceIsTheProfileDirectory(t *testing.T) {
	missing := NewAutoCookieService(filepath.Join(t.TempDir(), "nope"),
		filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), nopAutoCookieLogger{})
	if missing.periodicRefreshHasSource() {
		t.Error("a periodic tick with no browser profile directory would run a pass that can only " +
			"fail, and set LastError on every interval forever")
	}

	present := NewAutoCookieService(t.TempDir(),
		filepath.Join(t.TempDir(), "cookies.txt"), NewCookieJar(), nopAutoCookieLogger{})
	if !present.periodicRefreshHasSource() {
		t.Error("a periodic tick would skip forever on an install whose profile directory exists — " +
			"which is every install that has completed setup")
	}
}
