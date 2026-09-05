package cookies

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// bothPlatformsCookieFile configures BOTH platforms, which is what makes the
// budget question askable at all: checkPlatformAuth only runs a verifier for a
// platform that HasAny*AuthCookie reports as configured, so a single-platform
// jar can never observe one window being shared.
const bothPlatformsCookieFile = "# Netscape HTTP Cookie File\n" +
	".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\tfixture-sapisid\n" +
	"#HttpOnly_.twitch.tv\tTRUE\t/\tTRUE\t0\tauth-token\tfixture-token\n"

// shortAuthVerifyWindow shortens the verification budget for one test and puts
// it back afterwards. The seam exists so this file can assert on a window it
// can afford to spend twice.
func shortAuthVerifyWindow(t *testing.T, d time.Duration) {
	t.Helper()
	previous := authVerifyWindow
	authVerifyWindow = d
	t.Cleanup(func() { authVerifyWindow = previous })
}

// TestEachPlatformGetsItsOwnAuthVerifyWindow pins the budget as PER PLATFORM.
//
// checkPlatformAuth used to build ONE context.WithTimeout(ctx,
// authVerifyTimeout) and hand the same deadline to both verifiers, which run
// one after the other. A YouTube check that spent the whole window therefore
// handed Twitch a context that was already dead, and Twitch — whose real
// verifier is an HTTP round trip, and an HTTP round trip on an expired context
// fails before a packet leaves — was recorded as verifyUnknown. That is not a
// statement about the Twitch credentials; it is a statement about YouTube's
// latency, and everything downstream (credentialAccepted, the setup badge,
// platformsToRestoreOnRegression) reads it as the former.
//
// The Twitch fake here answers `true` instantly but consults its context
// first, exactly as an http.Client.Do would. That is the whole point: a fake
// that ignored the deadline would pass under both the old and the new code and
// would pin nothing.
//
// The wall-time bound is the second half of the proof. "Give both platforms
// more room" has a wrong answer that satisfies the state assertion alone —
// doubling the SHARED window — and under it the YouTube verifier would burn
// 2 × window before Twitch was asked at all. Two windows, one per platform,
// with only YouTube spending its own, costs ≈ one window in total.
func TestEachPlatformGetsItsOwnAuthVerifyWindow(t *testing.T) {
	const window = 300 * time.Millisecond
	shortAuthVerifyWindow(t, window)

	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(bothPlatformsCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewAutoCookieService("", cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	if err := s.jar.Load(cookiePath); err != nil {
		t.Fatalf("jar.Load: %v", err)
	}
	if !s.jar.HasAnyYouTubeAuthCookie() || !s.jar.HasAnyTwitchAuthCookie() {
		t.Fatal("fixture must configure both platforms or the shared window is unobservable")
	}

	// YouTube spends its entire window and then reports what a timed-out round
	// trip reports.
	s.VerifyYouTubeAuth = func(ctx context.Context) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}

	var twitchRemaining time.Duration
	s.VerifyTwitchAuth = func(ctx context.Context) (bool, error) {
		if deadline, ok := ctx.Deadline(); ok {
			twitchRemaining = time.Until(deadline)
		}
		if err := ctx.Err(); err != nil {
			// What an HTTP verifier answers when handed a spent deadline: the
			// request never starts, so nothing was learned.
			return false, err
		}
		return true, nil
	}

	start := time.Now()
	yt, tw := s.checkPlatformAuth(context.Background())
	elapsed := time.Since(start)

	if tw.state != verifyOK {
		t.Errorf("Twitch state = %v, want verifyOK — a slow YouTube check must not spend Twitch's window", tw.state)
	}
	if twitchRemaining < window/2 {
		t.Errorf("Twitch was given %v of its %v window; it must get a fresh one, not YouTube's remainder", twitchRemaining, window)
	}
	if elapsed >= 2*window {
		t.Errorf("checkPlatformAuth took %v (≥ 2×%v): the windows must be two separate budgets, not one doubled one", elapsed, window)
	}

	// YouTube's own verdict is unchanged by the split — it still spent its
	// window and still learned nothing, and it still counts as attempted.
	if yt.state != verifyUnknown {
		t.Errorf("YouTube state = %v, want verifyUnknown (its own window is still bounded)", yt.state)
	}
	if !yt.attempted {
		t.Error("a YouTube check that timed out mid-request was attempted")
	}
}

// TestBothPlatformAuthChecksRunAtOnce pins the windows as CONCURRENT, which
// is the half of the fix that keeps them affordable.
//
// Per-platform windows taken in sequence cost 2 × the window per call, and
// checkPlatformAuth is called twice per refresh pass — three times when the
// pass rolls back — inside one refreshOverallBudget. At 12 s a window that
// priced one call at 24 s put the pass at ≈125 s against a 120 s cap, i.e.
// the split would have been paid for out of a budget it does not own. Run
// together, a call costs ONE window however many platforms are configured,
// so the pass is ≈101 s (≈113 s with the rollback re-verify).
//
// The proof is a BARRIER, not a stopwatch. Each fake announces its own start
// and then waits for the other's, bounded by its own context. Run together,
// both waits are satisfied at once. Run one after the other, the first
// verifier can never see the second start — the second has not been called yet
// — so it waits out its whole window and reports that it saw nothing, and the
// second sees a start channel that was closed in a window that has already
// ended. The earlier form asserted `elapsed < 1.5 × window` against a green
// path of ≈1 × window; a 150 ms scheduling stall on a loaded CI runner was
// enough to fail it (review F7). The barrier discriminates the same two
// arrangements while reading no clock at all.
//
// Both verifiers still burn their whole window, which is what makes the
// sequential arrangement observable: with one instant verifier, sequential and
// concurrent both finish in ≈ one window and neither the old bound nor this
// one would see anything.
func TestBothPlatformAuthChecksRunAtOnce(t *testing.T) {
	const window = 300 * time.Millisecond
	shortAuthVerifyWindow(t, window)

	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(bothPlatformsCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewAutoCookieService("", cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	if err := s.jar.Load(cookiePath); err != nil {
		t.Fatalf("jar.Load: %v", err)
	}

	// Results travel on buffered channels rather than shared variables: the
	// two fakes run on goroutines checkPlatformAuth owns, and a channel is
	// race-free by construction rather than by argument about wg.Wait.
	ytStarted, twStarted := make(chan struct{}), make(chan struct{})
	ytSawOther, twSawOther := make(chan bool, 1), make(chan bool, 1)
	barrier := func(mine, theirs chan struct{}, sawOther chan<- bool) func(context.Context) (bool, error) {
		return func(ctx context.Context) (bool, error) {
			close(mine)
			select {
			case <-theirs:
				sawOther <- true
			case <-ctx.Done():
				sawOther <- false
			}
			<-ctx.Done() // spend the whole window either way
			return false, ctx.Err()
		}
	}
	s.VerifyYouTubeAuth = barrier(ytStarted, twStarted, ytSawOther)
	s.VerifyTwitchAuth = barrier(twStarted, ytStarted, twSawOther)

	yt, tw := s.checkPlatformAuth(context.Background())

	// Fatal, not Error: the receives below would block forever on a verifier
	// that was never called.
	if !yt.attempted || !tw.attempted {
		t.Fatalf("attempted = YouTube %v / Twitch %v, want both true — both checks timed out mid-request", yt.attempted, tw.attempted)
	}
	if !<-ytSawOther {
		t.Error("the YouTube verifier's window ended without the Twitch verifier having started: the two run one after the other, not at once")
	}
	if !<-twSawOther {
		t.Error("the Twitch verifier's window ended without the YouTube verifier having started: the two run one after the other, not at once")
	}
	if yt.state != verifyUnknown || tw.state != verifyUnknown {
		t.Errorf("states = YouTube %v / Twitch %v, want both verifyUnknown — each verifier spent its own full window", yt.state, tw.state)
	}
}

// TestAuthVerifyBudgetsStayUnderTheirCaps re-derives autocookies.go's two
// comment tables from the constants they name and asserts the caps they claim.
//
// The tables carry hand-summed figures (≈101 s, ≈113 s, ≈42.3 s) and the
// margins are thin — ~7 s on the refresh pass, ~17.7 s on the setup grace — so
// a one-line edit to any term can put a pass over its cap while the comment
// still reads as if it fits. Every term below is a named constant for exactly
// that reason; nothing here restates a number.
//
// A checkPlatformAuth call costs ONE authVerifyTimeout however many platforms
// are configured, because the platforms are verified concurrently. That is a
// property of the code, not of these constants, and it is pinned by
// TestBothPlatformAuthChecksRunAtOnce above — this test only spends the number
// that property buys.
func TestAuthVerifyBudgetsStayUnderTheirCaps(t *testing.T) {
	// The one call FinishSetup makes, on the binding (Chromium) column of the
	// setupAbandonGrace table.
	if got := cdpExtractTimeout + taskkillDrainDelay + authVerifyTimeout; got >= setupAbandonGrace {
		t.Errorf("Chromium finish column = %v, want < setupAbandonGrace (%v). "+
			"Serialising checkPlatformAuth or raising authVerifyTimeout re-opens the overrun the J5a/J5b rulings closed; "+
			"this constant and BOTH clients' own 60 s FinishSetup caps have to move together.", got, setupAbandonGrace)
	}

	// The refresh pass, on the two-platform Firefox column of the
	// processTimeout table. Per-pass fixed cost first, then the
	// checkPlatformAuth calls.
	fixed := 2*(processTimeout+postKillReapGrace) +
		firefoxLaunchSpacing +
		(cookieDBReadRetries-1)*cookieDBReadRetryBackoff
	for _, tc := range []struct {
		calls int
		why   string
	}{
		// The pre-write snapshot (snapshotPlatformAuth) and the post-write
		// verify.
		{2, "a pass that does not roll back"},
		// Plus the rollback arm's re-verify after restorePreviousCookies. A
		// fourth is unreachable: the snapshot is taken once per pass, and the
		// post-verify and the rollback re-verify are the two arms of one
		// decision.
		{3, "a pass that rolls back"},
	} {
		if got := fixed + time.Duration(tc.calls)*authVerifyTimeout; got >= refreshOverallBudget {
			t.Errorf("refresh pass with %d checkPlatformAuth calls (%s) = %v, want < refreshOverallBudget (%v)",
				tc.calls, tc.why, got, refreshOverallBudget)
		}
	}
}
