package cookies

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// jarWithTwilightUserOnly is the jar the NARROW gate exists for: Twitch was
// configured (twilight-user survives), the auth-token was pruned on expiry,
// HasAnyTwitchAuthCookie says true and HasTwitchAuthCookies says false. See
// twitchAuthCookieNames for how the state arises.
func jarWithTwilightUserOnly(t *testing.T) *CookieJar {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cookies.txt")
	content := "# Netscape HTTP Cookie File\n" +
		".twitch.tv\tTRUE\t/\tTRUE\t0\ttwilight-user\tfixture-twilight-user\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	return jar
}

// TestTwitchFallbackRunsAndOnlyForAConfiguredToken pairs the two halves that
// make each other mean something.
//
// MUTATION CLOSED (the 1): deleting the whole Twitch block — every "want 0"
// assertion in this file passes without it.
// MUTATION CLOSED (the 0): dropping the jar gate, or widening it to
// HasAnyTwitchAuthCookie. A jar holding only twilight-user has no bearer token,
// so the probe would be sent and declined every tick forever.
func TestTwitchFallbackRunsAndOnlyForAConfiguredToken(t *testing.T) {
	healthyRefreshSeams(t)

	withToken := 0
	rs := NewRefreshService(jarWithTwitchAuth(t), 0, nopLogger{})
	rs.TwitchFallbackLiveness = func(context.Context) (bool, bool) { withToken++; return true, true }
	rs.doRefresh(context.Background())
	if withToken != 1 {
		t.Errorf("probe fired %d times for a jar holding an auth-token, want 1", withToken)
	}

	bare := 0
	rsBare := NewRefreshService(NewCookieJar(), 0, nopLogger{})
	rsBare.TwitchFallbackLiveness = func(context.Context) (bool, bool) { bare++; return true, true }
	rsBare.doRefresh(context.Background())
	if bare != 0 {
		t.Errorf("probe fired %d times for a jar with no Twitch cookies at all, want 0", bare)
	}

	// The arm that actually closes the widening mutation: an empty jar fails
	// HasAnyTwitchAuthCookie too, so only a twilight-user-only jar can tell
	// the narrow gate from the broad one.
	pruned := 0
	rsPruned := NewRefreshService(jarWithTwilightUserOnly(t), 0, nopLogger{})
	rsPruned.TwitchFallbackLiveness = func(context.Context) (bool, bool) { pruned++; return true, true }
	rsPruned.doRefresh(context.Background())
	if pruned != 0 {
		t.Errorf("probe fired %d times for a jar holding twilight-user but no auth-token, want 0 — the gate is the broad HasAnyTwitchAuthCookie, and this install would be asked and declined every tick", pruned)
	}
}

// TestTwitchFallbackObservesOnlyConclusiveAnswers: a conclusive verdict reaches
// ObserveLiveness; an inconclusive one moves nothing — neither the freshness
// stamp (which would suppress the next cycle's probe) nor the recovery dedupe
// (which would swallow the next real signed-out verdict).
//
// MUTATION CLOSED: routing the inconclusive arm through ObserveLiveness, or
// dropping the `if conclusive` entirely.
func TestTwitchFallbackObservesOnlyConclusiveAnswers(t *testing.T) {
	healthyRefreshSeams(t)

	// Conclusive: the observation lands and the freshness stamp is written.
	rs := NewRefreshService(jarWithTwitchAuth(t), 0, nopLogger{})
	rs.TwitchFallbackLiveness = func(context.Context) (bool, bool) { return true, true }
	rs.doRefresh(context.Background())
	if !rs.livenessObservedRecently("twitch", time.Now()) {
		t.Error("a conclusive verdict recorded no observation — the probe's answer never reached ObserveLiveness")
	}

	// Inconclusive: nothing moves.
	called := 0
	rsInc := NewRefreshService(jarWithTwitchAuth(t), 0, nopLogger{})
	rsInc.TwitchFallbackLiveness = func(context.Context) (bool, bool) { called++; return false, false }
	rsInc.doRefresh(context.Background())
	if called != 1 {
		t.Fatalf("probe fired %d times, want 1 — the assertions below say nothing about a probe that never ran", called)
	}
	if rsInc.livenessObservedRecently("twitch", time.Now()) {
		t.Error("an inconclusive probe recorded an observation — it would suppress the next cycle's probe")
	}
	if due, _ := rsInc.recordLiveness("twitch", false, time.Now()); !due {
		t.Error("an inconclusive probe consumed the recovery dedupe — a real signed-out verdict would be swallowed")
	}
}

// TestTwitchFallbackUsesTheTwitchPlatformKey.
//
// MUTATION CLOSED: typing "youtube" into either the freshness gate or the
// ObserveLiveness call — a copy-paste from the block directly above, and the
// single most likely defect in this change. Either mistake makes a YouTube
// observation suppress the Twitch probe, or files a Twitch verdict under
// YouTube's key where an armed pilot would page about the wrong platform.
func TestTwitchFallbackUsesTheTwitchPlatformKey(t *testing.T) {
	healthyRefreshSeams(t)

	called := 0
	rs := NewRefreshService(jarWithBothPlatformsAuth(t), 0, nopLogger{})
	rs.TwitchFallbackLiveness = func(context.Context) (bool, bool) { called++; return false, true }

	// A fresh YouTube observation must not suppress the Twitch probe.
	rs.ObserveLiveness("youtube", true)
	rs.doRefresh(context.Background())
	if called != 1 {
		t.Fatalf("probe fired %d times after a YouTube observation, want 1 — the gate is reading the wrong platform key", called)
	}

	// And the verdict landed under "twitch": a signed-out verdict consumes the
	// twitch dedupe and leaves youtube's alone.
	if due, _ := rs.recordLiveness("twitch", false, time.Now()); due {
		t.Error("the signed-out verdict did not consume the twitch dedupe — it was filed under another platform")
	}
	if due, _ := rs.recordLiveness("youtube", false, time.Now()); !due {
		t.Error("the Twitch verdict consumed YouTube's dedupe — the platform string is wrong")
	}
}

// TestTwitchFallbackSkippedWhenObservedRecently: a fresh Twitch observation
// must not be paid for twice. The second half is what makes the first mean
// anything — `called == 0` is satisfied just as well by the block not existing,
// so the observation is aged past the window on the SAME service and the SAME
// call must now pay.
//
// MUTATION CLOSED: dropping !livenessObservedRecently from the condition.
func TestTwitchFallbackSkippedWhenObservedRecently(t *testing.T) {
	healthyRefreshSeams(t)

	called := 0
	rs := NewRefreshService(jarWithTwitchAuth(t), 0, nopLogger{})
	rs.TwitchFallbackLiveness = func(context.Context) (bool, bool) { called++; return true, true }

	rs.ObserveLiveness("twitch", true)
	rs.doRefresh(context.Background())
	if called != 0 {
		t.Errorf("probe fired %d times despite a fresh observation, want 0", called)
	}

	rs.mu.Lock()
	rs.lastLivenessObserved["twitch"] = time.Now().Add(-livenessFreshWindow - time.Minute)
	rs.mu.Unlock()

	rs.doRefresh(context.Background())
	if called != 1 {
		t.Errorf("probe fired %d times once the observation aged out, want 1 — the zero above proves nothing if the probe never runs at all", called)
	}
}

// TestTwitchInconclusiveFallbackIsReportedOncePerRun is R2's "says so once at
// Info", pinned as a dedupe rather than a promise. An install with a token and
// no configured channel is inconclusive on EVERY tick, forever: a line per tick
// is noise fanned out over the WebSocket stream to both UIs, and no line makes
// "the signal is dead" indistinguishable from "healthy, nothing to say" — the
// one distinction the pilot's log has to be able to draw about its own signal.
//
// MUTATION CLOSED: logging unconditionally at Info (3 lines), dropping the line
// (0), or recording through recordLiveness instead of
// recordInconclusiveLiveness (which also suppresses the next probe — the
// `called != 3` assertion catches that one).
func TestTwitchInconclusiveFallbackIsReportedOncePerRun(t *testing.T) {
	healthyRefreshSeams(t)

	const line = "liveness fallback probe learned nothing"

	log := &capturingLogger{}
	called := 0
	rs := NewRefreshService(jarWithTwitchAuth(t), 0, log)
	rs.TwitchFallbackLiveness = func(context.Context) (bool, bool) { called++; return false, false }

	for range 3 {
		rs.doRefresh(context.Background())
	}

	if called != 3 {
		t.Fatalf("probe ran %d times over 3 refreshes, want 3 — the log counts below say nothing otherwise", called)
	}
	if got := countContaining(log.infos, line); got != 1 {
		t.Errorf("%d operator-visible lines about an inconclusive probe over 3 cycles, want exactly 1: %q", got, log.infos)
	}
	if got := countContaining(log.debugs, line); got != 2 {
		t.Errorf("%d debug-level repeats, want 2 — the repeats must still be recorded, just not at Info: %q", got, log.debugs)
	}

	// MUTATION CLOSED: typing "youtube" into the recordInconclusiveLiveness
	// call. The dedupe counts above pass under either platform key — this is
	// the only assertion in the file that reads which key was actually
	// touched.
	rs.mu.RLock()
	_, youtubeTouched := rs.lastLivenessKnown["youtube"]
	twitchRecord, twitchTouched := rs.lastLivenessKnown["twitch"]
	rs.mu.RUnlock()
	if youtubeTouched {
		t.Error("an inconclusive Twitch probe wrote YouTube's liveness record — the platform key is wrong, and YouTube's next learned-nothing line would land at Debug")
	}
	if !twitchTouched || twitchRecord != livenessInconclusive {
		t.Errorf("the Twitch inconclusive record is %v (present=%v), want livenessInconclusive under the twitch key", twitchRecord, twitchTouched)
	}
}

// TestTwitchFallbackIsPeriodicOnly: CheckNow runs synchronously on an HTTP
// handler and Start's initial check runs before the web server binds; neither
// may buy a GQL round trip. The doRefresh half is not decoration — without it
// `called == 0` is equally well explained by a block that was never written.
//
// MUTATION CLOSED: dropping allowFallback from the condition.
func TestTwitchFallbackIsPeriodicOnly(t *testing.T) {
	healthyRefreshSeams(t)

	called := 0
	rs := NewRefreshService(jarWithTwitchAuth(t), 0, nopLogger{})
	rs.TwitchFallbackLiveness = func(context.Context) (bool, bool) { called++; return true, true }

	rs.CheckNow(context.Background())
	if called != 0 {
		t.Errorf("probe fired %d times on the CheckNow path, want 0", called)
	}

	rs.doRefresh(context.Background())
	if called != 1 {
		t.Errorf("probe fired %d times on the periodic path, want 1 — the CheckNow zero above proves nothing if the probe never runs at all", called)
	}
}

// TestTwitchFallbackSkipsTheStartupCheck: Start runs its initial check
// SYNCHRONOUSLY on the caller's goroutine, and cmd/moombox's run() blocks on
// it before the web server binds. A GQL round trip — up to the closure's 20 s
// timeout — in front of the dashboard on every start is the trade the YouTube
// twin refused (TestStartupRefreshSkipsFallbackProbe), and it was missed there
// first, which is why the mirror carries it separately from the CheckNow arm.
//
// MUTATION CLOSED: Start's initial check passing allowFallback=true.
func TestTwitchFallbackSkipsTheStartupCheck(t *testing.T) {
	healthyRefreshSeams(t)

	// Atomic because Start spawns the ticker goroutine.
	var called atomic.Int64
	rs := NewRefreshService(jarWithTwitchAuth(t), 0, nopLogger{})
	rs.TwitchFallbackLiveness = func(context.Context) (bool, bool) { called.Add(1); return true, true }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rs.Start(ctx)
	rs.Stop()

	if got := called.Load(); got != 0 {
		t.Errorf("probe fired %d times on the synchronous startup check, want 0", got)
	}

	rs.doRefresh(context.Background())
	if got := called.Load(); got != 1 {
		t.Errorf("probe fired %d times on the ticker path, want 1 — the startup zero proves nothing if the probe never runs at all", got)
	}
}

// TestTwitchFallbackWritesNoAuthStatus: the probe is an OBSERVATION producer.
// Arc 10's capture-time mark stays the only writer of
// rs.status.TwitchAuthenticated outside doRefresh's own status block.
//
// MUTATION CLOSED: adding `rs.status.TwitchAuthenticated = loggedIn` (or a
// NoteTwitchAuthLoss call) to the new block. With the tier-1 seam answering a
// healthy 200, a signed-out tier-2 verdict must leave the status green; the
// recorded observation is asserted alongside so a probe that never ran cannot
// pass this.
func TestTwitchFallbackWritesNoAuthStatus(t *testing.T) {
	healthyRefreshSeams(t)

	rs := NewRefreshService(jarWithTwitchAuth(t), 0, nopLogger{})
	rs.TwitchFallbackLiveness = func(context.Context) (bool, bool) { return false, true }
	rs.doRefresh(context.Background())

	if !rs.livenessObservedRecently("twitch", time.Now()) {
		t.Fatal("the signed-out verdict never reached ObserveLiveness — the assertion below would pass vacuously")
	}
	if got := rs.GetStatus(); !got.TwitchAuthenticated {
		t.Error("a tier-2 signed-out verdict flipped AuthStatus.TwitchAuthenticated — the probe is an OBSERVATION producer, and Arc 10's mark stays the only writer outside doRefresh's status block")
	}
}

// TestTwitchFallbackFiresRecoveryWhenArmed is the armed twin of the pin above,
// and the Twitch half of the 2026-09-03 flip.
//
// The YouTube path's armed pin (TestLivenessRecoveryPilotIsArmed) drives
// ObserveLiveness directly. This one drives the whole Twitch tier-2 route the
// way production does — the periodic refresh calls TwitchFallbackLiveness,
// which routes a conclusive verdict into ObserveLiveness("twitch", …) — so a
// gate, a platform key or a conclusiveness check wrong anywhere on that route
// shows up here.
//
// MUTATIONS CLOSED: deleting `fn(platform)` in ObserveLiveness, or the constant
// back to false (0 fires); passing the probe's verdict for the wrong platform
// (the platform assertion); reporting an INCONCLUSIVE probe as a verdict (the
// second half — `false, false` must reach nothing at all).
func TestTwitchFallbackFiresRecoveryWhenArmed(t *testing.T) {
	healthyRefreshSeams(t)

	rs := NewRefreshService(jarWithTwitchAuth(t), 0, nopLogger{})
	var fired []string
	rs.OnRecoveryNeeded = func(platform string) { fired = append(fired, platform) }
	rs.TwitchFallbackLiveness = func(context.Context) (bool, bool) { return false, true }

	rs.doRefresh(context.Background())

	if len(fired) != 1 || fired[0] != "twitch" {
		t.Fatalf("OnRecoveryNeeded calls = %v, want exactly one for \"twitch\" — a due signed-out entitlement verdict must raise the alarm now the pilot is armed", fired)
	}

	// An inconclusive probe on a fresh service must reach nothing: a 401/403,
	// a rate limit or an unconfigured channel is not a dead session, and the
	// one above proves nothing if every probe outcome fires.
	inconclusive := NewRefreshService(jarWithTwitchAuth(t), 0, nopLogger{})
	var quiet []string
	inconclusive.OnRecoveryNeeded = func(platform string) { quiet = append(quiet, platform) }
	inconclusive.TwitchFallbackLiveness = func(context.Context) (bool, bool) { return false, false }

	inconclusive.doRefresh(context.Background())

	if len(quiet) != 0 {
		t.Errorf("OnRecoveryNeeded fired %v for an INCONCLUSIVE probe — 401/403 and a missing channel are silence, not a verdict", quiet)
	}
}
