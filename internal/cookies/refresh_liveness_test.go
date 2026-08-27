package cookies

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// loggedInGuideBody is the guide reply that keeps the tier-1 auth check happy,
// so a test about the liveness signal is not also a test about auth loss.
// Mirrors the shape refresh_status_test.go asserts on for the positive case.
const loggedInGuideBody = `{"responseContext":{"serviceTrackingParams":[{"params":[{"key":"logged_in","value":"1"}]}]}}`

// healthyRefreshSeams points both network seams at stubs that report a healthy
// session, so nothing in a fallback-probe test can fire tier-1 recovery or
// reach the real youtube.com / id.twitch.tv.
func healthyRefreshSeams(t *testing.T) {
	t.Helper()
	srv, _ := countingGuide(t, loggedInGuideBody)
	pointYouTubeGuideAt(t, srv)
	pointTwitchValidateAt(t, statusServer(t, http.StatusOK))
}

// TestRecordLivenessOnlyLoggedOutWarrantsRecovery pins the direction rule and
// the two-map split in one place.
//
// LoggedIn is positive evidence and must be silent. It must ALSO leave the
// dedupe untouched: with one map serving both questions, a healthy verdict
// from the first channel of a cycle would swallow a dead verdict from the
// second channel a moment later — the exact N-channel shape the membership
// probe produces.
func TestRecordLivenessOnlyLoggedOutWarrantsRecovery(t *testing.T) {
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	now := time.Now()

	if due, _ := rs.recordLiveness("youtube", true, now); due {
		t.Error("a logged-in observation warranted recovery — positive evidence must be silent")
	}
	if due, _ := rs.recordLiveness("youtube", false, now.Add(time.Second)); !due {
		t.Error("a logged-out observation one second after a logged-in one did not warrant recovery — the healthy verdict swallowed the dead one")
	}
}

// TestRecordLivenessDedupesAcrossChannels is acceptance criterion A5: N
// channels must not mean N alerts.
//
// The membership probe runs once per configured channel per feed cycle, and
// checkChannel walks the channel list serially on the feed monitor's own
// goroutine (internal/monitor/feed.go), so a dead session arrives here as a
// short serial burst of identical logged-out verdicts. Exactly one of them may
// warrant recovery — see livenessRefireWindow for what the extra ones cost:
// the auto-cookie single-flight declines them, a decline reports as
// "Ineffective", and that notification suppresses the real verdict's.
func TestRecordLivenessDedupesAcrossChannels(t *testing.T) {
	base := time.Now()

	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	warranted := 0
	for i := range 5 {
		if due, _ := rs.recordLiveness("youtube", false, base.Add(time.Duration(i)*time.Second)); due {
			warranted++
		}
	}
	if warranted != 1 {
		t.Errorf("%d of 5 same-cycle logged-out verdicts warranted recovery, want 1 — N channels must not mean N alerts", warranted)
	}

	// And the public entry point goes through that same fold rather than
	// around it: after five calls the window is consumed, so a sixth verdict
	// at the same instant is refused.
	viaObserve := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	for range 5 {
		viaObserve.ObserveLiveness("youtube", false)
	}
	if due, _ := viaObserve.recordLiveness("youtube", false, time.Now()); due {
		t.Error("ObserveLiveness left the dedupe window unconsumed — it is not routing through recordLiveness")
	}
}

// TestRecordLivenessRefiresAfterTheWindow: the dedupe is a window, not a
// latch. A session that is still dead an hour later has to be reportable
// again, or the very first suppressed verdict would silence the platform for
// the life of the process.
func TestRecordLivenessRefiresAfterTheWindow(t *testing.T) {
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	t0 := time.Now()

	if due, _ := rs.recordLiveness("youtube", false, t0); !due {
		t.Fatal("premise broken: the first logged-out verdict must warrant recovery")
	}
	if due, _ := rs.recordLiveness("youtube", false, t0.Add(livenessRefireWindow-time.Second)); due {
		t.Error("a verdict inside livenessRefireWindow warranted recovery")
	}
	if due, _ := rs.recordLiveness("youtube", false, t0.Add(livenessRefireWindow)); !due {
		t.Error("a verdict a full livenessRefireWindow later did not warrant recovery — the dedupe latched")
	}
}

// TestRecordLivenessSeparatesPlatforms: the maps are keyed per platform, so a
// YouTube verdict must not dedupe a Twitch one. Nothing produces Twitch
// liveness verdicts today; this pins the shape before something does.
func TestRecordLivenessSeparatesPlatforms(t *testing.T) {
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	now := time.Now()

	if due, _ := rs.recordLiveness("youtube", false, now); !due {
		t.Fatal("premise broken: the first logged-out verdict must warrant recovery")
	}
	if due, _ := rs.recordLiveness("twitch", false, now); !due {
		t.Error("a YouTube verdict deduped a Twitch one")
	}
}

// TestLivenessRecoveryPilotIsDisarmed is the guard on the staged rollout.
//
// One logged-out verdict reaching OnRecoveryNeeded notifies the operator on
// EITHER install shape — there is no quiet arm. cmd/moombox's
// handleRecoveryNeeded either runs a headless refresh and reports "Cookie
// Auto-Refresh Failed"/"Ineffective" (auto_enabled = true), or sends "Cookie
// Re-Authentication Required" synchronously (auto_enabled = false, since Task 7
// replaced that arm's silence). The second shape is the one least able to act
// on the remedy the notification names: a container or a remote dashboard
// cannot reach the loopback-gated Settings wizard.
//
// That is why this signal lands log-only, and why the gate is not a statement
// about browsers: a false LoggedOut costs an operator a re-export of
// credentials that were never wrong, whatever auto_enabled says.
//
// The premise runs on its own service ON PURPOSE. "OnRecoveryNeeded was not
// called" sits downstream of a junction: a logged-in verdict, a consumed
// dedupe window, or a nil callback each satisfy it just as well as the gate
// does. Establishing on an identical service that this exact observation DOES
// warrant recovery removes every one of those explanations.
func TestLivenessRecoveryPilotIsDisarmed(t *testing.T) {
	if livenessRecoveryArmed {
		t.Fatal("livenessRecoveryArmed is true — the liveness verdicts must land log-only; arming them is a separate, deliberate change")
	}

	premise := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	if due, _ := premise.recordLiveness("youtube", false, time.Now()); !due {
		t.Fatal("premise broken: a first logged-out observation must warrant recovery")
	}

	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	var fired []string
	rs.OnRecoveryNeeded = func(platform string) { fired = append(fired, platform) }

	rs.ObserveLiveness("youtube", false)

	if len(fired) != 0 {
		t.Errorf("OnRecoveryNeeded fired %v — the pilot gate must suppress it, or every install shape starts notifying its operator off a signal nobody has field-checked", fired)
	}
}

// TestObserveLivenessIsSafeUnderConcurrentProducers: the per-cycle fold really
// is serial — checkChannel walks the channel list one at a time on the feed
// monitor's own goroutine — but the PRODUCERS are not. The fallback probe
// reports from the cookie-refresh goroutine, and CheckNow can drive a refresh
// from an HTTP handler at the same time. Both maps therefore live under rs.mu,
// and this is what would catch that being relaxed. Run with -race.
func TestObserveLivenessIsSafeUnderConcurrentProducers(t *testing.T) {
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic in concurrent liveness producer: %v", r)
				}
			}()
			for range 50 {
				rs.ObserveLiveness("youtube", i%2 == 0)
				rs.livenessObservedRecently("youtube", time.Now())
			}
		}()
	}
	wg.Wait()
}

// TestFallbackSkippedWhenMembershipIsFresh: an install with channels already
// gets a liveness verdict for free from the membership probe every feed cycle.
// Buying a second full page fetch on top of it, every cycle, forever, is the
// cost this skip exists to avoid.
//
// The second half is what makes the first half mean anything. `called == 0`
// alone is satisfied just as well by the fallback block not existing at all,
// so the observation is then aged past the window on the SAME service and the
// SAME call must now pay for the probe. That pins the zero to the freshness
// gate specifically.
func TestFallbackSkippedWhenMembershipIsFresh(t *testing.T) {
	healthyRefreshSeams(t)

	called := 0
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	rs.FallbackLiveness = func(context.Context) (bool, bool) { called++; return true, true }

	rs.ObserveLiveness("youtube", true) // a fresh membership observation
	rs.doRefresh(context.Background())

	if called != 0 {
		t.Errorf("fallback fired %d times despite a fresh observation, want 0", called)
	}

	rs.mu.Lock()
	rs.lastLivenessObserved["youtube"] = time.Now().Add(-livenessFreshWindow - time.Minute)
	rs.mu.Unlock()

	rs.doRefresh(context.Background())
	if called != 1 {
		t.Errorf("fallback fired %d times once the observation aged out, want 1 — the zero above proves nothing if the probe never runs at all", called)
	}
}

// TestFallbackRunsWhenNothingHasObserved is the converse, and it is what stops
// the test above from passing for the wrong reason: with no observation on
// record the probe must actually run, or the coverage gap it was built to
// close (no YouTube channels, or membership discovery off everywhere) stays
// open and every skip test passes vacuously.
func TestFallbackRunsWhenNothingHasObserved(t *testing.T) {
	healthyRefreshSeams(t)

	called := 0
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	rs.FallbackLiveness = func(context.Context) (bool, bool) { called++; return true, true }

	rs.doRefresh(context.Background())

	if called != 1 {
		t.Errorf("fallback fired %d times with nothing observed, want 1", called)
	}
}

// TestFallbackInconclusiveMovesNothing: `conclusive == false` is a consent
// wall, a rate limit or a transport failure. It is silence, and silence must
// neither record an observation (which would suppress the next cycle's probe)
// nor consume the dedupe (which would swallow the next real logged-out
// verdict).
//
// `called` is asserted alongside the two negatives on purpose: both of them
// also hold if the probe never ran, so without it this test passes with the
// entire fallback block deleted.
func TestFallbackInconclusiveMovesNothing(t *testing.T) {
	healthyRefreshSeams(t)

	called := 0
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	rs.FallbackLiveness = func(context.Context) (bool, bool) { called++; return false, false }

	rs.doRefresh(context.Background())

	if called != 1 {
		t.Fatalf("fallback fired %d times, want 1 — the assertions below say nothing about a probe that never ran", called)
	}
	if rs.livenessObservedRecently("youtube", time.Now()) {
		t.Error("an inconclusive fallback recorded an observation — it would suppress the next cycle's probe")
	}
	if due, _ := rs.recordLiveness("youtube", false, time.Now()); !due {
		t.Error("an inconclusive fallback consumed the dedupe — a real logged-out verdict would be swallowed")
	}
}

// TestCheckNowSkipsFallbackProbe: POST /api/cookies/recheck runs the refresh
// synchronously on the HTTP handler goroutine, already paying for a 15s auth
// check. Adding a 20s page fetch to a button press is a bad trade, and the
// periodic path owns that probe anyway.
//
// The doRefresh half of this test is not decoration: without it, `called == 0`
// is equally well explained by a fresh observation, a nil callback, or the
// probe never having been wired at all.
func TestCheckNowSkipsFallbackProbe(t *testing.T) {
	healthyRefreshSeams(t)

	called := 0
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	rs.FallbackLiveness = func(context.Context) (bool, bool) { called++; return true, true }

	rs.CheckNow(context.Background())
	if called != 0 {
		t.Errorf("fallback fired %d times on the CheckNow path, want 0", called)
	}

	// Same service, same (empty) observation state: the periodic path does
	// pay for it, so the zero above is attributable to the path and nothing
	// else.
	rs.doRefresh(context.Background())
	if called != 1 {
		t.Errorf("fallback fired %d times on the periodic path, want 1 — the CheckNow zero above proves nothing if the probe never runs at all", called)
	}
}

// TestStartupRefreshSkipsFallbackProbe: Start runs its initial check
// SYNCHRONOUSLY on the caller's goroutine, and cmd/moombox's run() blocks on it
// before the web server binds. Nothing has observed liveness at that point, so
// the freshness skip cannot help — every install holding a YouTube auth cookie
// would put a full page fetch, up to a 20s timeout, in front of the dashboard
// coming up, on every start. Config changes restart the process, so that is one
// delayed startup per settings tweak.
//
// Same reasoning that excludes CheckNow, and it was missed there first.
func TestStartupRefreshSkipsFallbackProbe(t *testing.T) {
	healthyRefreshSeams(t)

	// Atomic because Start spawns the ticker goroutine. It cannot fire inside
	// this test at a 30-minute interval, but the counter must not depend on
	// that to be race-free.
	var called atomic.Int64
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	rs.FallbackLiveness = func(context.Context) (bool, bool) { called.Add(1); return true, true }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rs.Start(ctx)
	rs.Stop()

	if got := called.Load(); got != 0 {
		t.Errorf("fallback fired %d times on the synchronous startup check, want 0", got)
	}

	// Same service, still nothing observed: the ticker path does pay for it,
	// so the zero above is attributable to the startup path and not to the
	// probe being unreachable.
	rs.doRefresh(context.Background())
	if got := called.Load(); got != 1 {
		t.Errorf("fallback fired %d times on the ticker path, want 1 — the startup zero proves nothing if the probe never runs at all", got)
	}
}

// TestFallbackSkipCoversTheDefaultFeedCadence pins livenessFreshWindow's LOWER
// bound at the configuration it was chosen for. monitors.feed_check_interval
// defaults to 10 minutes, so a membership observation is at most that old when
// the next refresh looks at it; if that read as stale, a perfectly healthy
// install would pay for the fallback on every cycle — the exact cost the skip
// exists to remove.
//
// This is an assumption about configuration, not an invariant, and the test
// can only pin the default. feed_check_interval validates to 1..1440 minutes,
// so anything above ~25 breaks the assumption and costs one extra page fetch
// on roughly every other cycle. See livenessFreshWindow.
func TestFallbackSkipCoversTheDefaultFeedCadence(t *testing.T) {
	// internal/config: Monitors.FeedCheckInterval default, in minutes.
	const defaultFeedCadence = 10 * time.Minute

	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	t0 := time.Now()
	rs.recordLiveness("youtube", true, t0)

	if !rs.livenessObservedRecently("youtube", t0.Add(defaultFeedCadence)) {
		t.Errorf("an observation %v old — one default feed cycle — reads as stale, so a healthy install would pay for the fallback on every cycle", defaultFeedCadence)
	}
}

// TestLivenessLogLevelIsNotableOnlyOnChangeOrSignedOut pins the log-level rule.
//
// ObserveLiveness is called once per configured channel per feed cycle, so
// logging every verdict at Info puts 144*N lines a day into the log AND over
// the WebSocket stream to the Web UI and TUI — all of them identical on a
// healthy install. Notable means: signed out (never demoted, because losing
// evidence of a dead session is the one direction this must not fail in), a
// change of verdict, or the first observation of the process.
func TestLivenessLogLevelIsNotableOnlyOnChangeOrSignedOut(t *testing.T) {
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	t0 := time.Now()

	steps := []struct {
		loggedIn bool
		want     bool
		why      string
	}{
		{true, true, "the first observation of the process is the record that the signal started producing"},
		{true, false, "a repeat of a healthy verdict already on record is the volume problem"},
		{false, true, "a signed-out verdict is always notable"},
		{false, true, "a repeated signed-out verdict must not be demoted even though the dedupe refused it"},
		{true, true, "recovery back to healthy is a change and must be visible"},
		{true, false, "and the repeats after it are not"},
	}
	for i, s := range steps {
		_, notable := rs.recordLiveness("youtube", s.loggedIn, t0.Add(time.Duration(i)*time.Second))
		if notable != s.want {
			t.Errorf("step %d (loggedIn=%v): notable = %v, want %v — %s", i, s.loggedIn, notable, s.want, s.why)
		}
	}
}

// TestFallbackObservationAgesOutWithinOneCadence pins livenessFreshWindow's
// UPPER bound against the cadence it has to interlock with.
//
// The fallback records its own answer through the same method the membership
// probe uses, so if its own stamp still read as fresh on the next tick the
// probe would suppress itself on alternate cycles — quietly halving a coverage
// nobody decided to halve.
//
// The staleness assertion deliberately sits SHORT of a full cadence. The
// ticker measures tick-to-tick, while the stamp is written at the tail of a
// pass, so consecutive fallback stamps are one cadence apart MINUS however
// long the pass took — up to ~50s of auth checks and page fetch. A window that
// only just cleared defaultRefreshInterval would therefore still self-suppress
// in the field; passSlack covers that gap.
func TestFallbackObservationAgesOutWithinOneCadence(t *testing.T) {
	const passSlack = 2 * time.Minute

	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	t0 := time.Now()
	rs.recordLiveness("youtube", true, t0)

	if rs.livenessObservedRecently("youtube", t0.Add(defaultRefreshInterval-passSlack)) {
		t.Errorf("an observation %v old still reads as fresh — the fallback's own stamp would suppress the next cycle's probe", defaultRefreshInterval-passSlack)
	}
}

// TestTierOneRecoveryStampsTheLivenessDedupe: a dead session makes the tier-1
// auth check fire recovery AND makes the fallback probe at the tail of the
// same pass report logged-out. The second fire is the harmful one: the
// auto-cookie single-flight declines it while the first attempt is still
// running, a decline reports as RefreshUnknown, and the "Ineffective" warning
// that produces stamps the notification cooldown — so the real verdict's
// actionable message, two minutes later, is never sent.
func TestTierOneRecoveryStampsTheLivenessDedupe(t *testing.T) {
	srv, _ := countingGuide(t, loggedOutGuideBody)
	pointYouTubeGuideAt(t, srv)
	pointTwitchValidateAt(t, statusServer(t, http.StatusUnauthorized))

	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	var fired []string
	rs.OnRecoveryNeeded = func(platform string) { fired = append(fired, platform) }

	rs.doRefresh(context.Background())

	if len(fired) != 1 || fired[0] != "youtube" {
		t.Fatalf("premise broken: tier-1 recovery fired %v, want [youtube]", fired)
	}
	if due, _ := rs.recordLiveness("youtube", false, time.Now()); due {
		t.Error("a logged-out liveness verdict cleared the dedupe in the same window tier-1 recovery fired in — the declined second attempt would report Ineffective and suppress the real verdict's notification")
	}
}
