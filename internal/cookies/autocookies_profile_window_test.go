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
// Both verifiers burn their whole window here, which is the only arrangement
// that can tell the two apart: with one instant verifier a sequential pair and
// a concurrent pair both finish in ≈ one window. Sequential would take 2 ×
// window; the bound is 1.5 × to leave scheduling slack without admitting it.
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

	burnTheWindow := func(ctx context.Context) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}
	s.VerifyYouTubeAuth = burnTheWindow
	s.VerifyTwitchAuth = burnTheWindow

	start := time.Now()
	yt, tw := s.checkPlatformAuth(context.Background())
	elapsed := time.Since(start)

	if elapsed >= 3*window/2 {
		t.Errorf("two full windows cost %v (≥ 1.5×%v): the platforms must be verified at the same time, not one after the other", elapsed, window)
	}
	if yt.state != verifyUnknown || tw.state != verifyUnknown {
		t.Errorf("states = YouTube %v / Twitch %v, want both verifyUnknown — each verifier spent its own full window", yt.state, tw.state)
	}
	if !yt.attempted || !tw.attempted {
		t.Errorf("attempted = YouTube %v / Twitch %v, want both true — both checks timed out mid-request", yt.attempted, tw.attempted)
	}
}
