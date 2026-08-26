package cookies

import (
	"context"
	"net/http"
	"sync"
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

	if rs.recordLiveness("youtube", true, now) {
		t.Error("a logged-in observation warranted recovery — positive evidence must be silent")
	}
	if !rs.recordLiveness("youtube", false, now.Add(time.Second)) {
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
// warrant recovery: each one that does spawns its own two-minute
// headless-browser attempt.
func TestRecordLivenessDedupesAcrossChannels(t *testing.T) {
	base := time.Now()

	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	warranted := 0
	for i := range 5 {
		if rs.recordLiveness("youtube", false, base.Add(time.Duration(i)*time.Second)) {
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
	if viaObserve.recordLiveness("youtube", false, time.Now()) {
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

	if !rs.recordLiveness("youtube", false, t0) {
		t.Fatal("premise broken: the first logged-out verdict must warrant recovery")
	}
	if rs.recordLiveness("youtube", false, t0.Add(livenessRefireWindow-time.Second)) {
		t.Error("a verdict inside livenessRefireWindow warranted recovery")
	}
	if !rs.recordLiveness("youtube", false, t0.Add(livenessRefireWindow)) {
		t.Error("a verdict a full livenessRefireWindow later did not warrant recovery — the dedupe latched")
	}
}

// TestRecordLivenessSeparatesPlatforms: the maps are keyed per platform, so a
// YouTube verdict must not dedupe a Twitch one. Nothing produces Twitch
// liveness verdicts today; this pins the shape before something does.
func TestRecordLivenessSeparatesPlatforms(t *testing.T) {
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	now := time.Now()

	if !rs.recordLiveness("youtube", false, now) {
		t.Fatal("premise broken: the first logged-out verdict must warrant recovery")
	}
	if !rs.recordLiveness("twitch", false, now) {
		t.Error("a YouTube verdict deduped a Twitch one")
	}
}

// TestLivenessRecoveryPilotIsDisarmed is the guard on the staged rollout.
//
// OnRecoveryNeeded returns early only when auto-cookies are disabled, so on an
// auto_enabled install — every install that used the setup wizard — one
// logged-out verdict reaching it launches a headless browser and can send a
// "Cookie Auto-Refresh Failed" notification. This signal has never been in the
// health path before, so it lands log-only.
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
	if !premise.recordLiveness("youtube", false, time.Now()) {
		t.Fatal("premise broken: a first logged-out observation must warrant recovery")
	}

	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	var fired []string
	rs.OnRecoveryNeeded = func(platform string) { fired = append(fired, platform) }

	rs.ObserveLiveness("youtube", false)

	if len(fired) != 0 {
		t.Errorf("OnRecoveryNeeded fired %v — the pilot gate must suppress it, or an auto_enabled install starts launching browsers and sending alerts off a signal nobody has field-checked", fired)
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
func TestFallbackInconclusiveMovesNothing(t *testing.T) {
	healthyRefreshSeams(t)

	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	rs.FallbackLiveness = func(context.Context) (bool, bool) { return false, false }

	rs.doRefresh(context.Background())

	if rs.livenessObservedRecently("youtube", time.Now()) {
		t.Error("an inconclusive fallback recorded an observation — it would suppress the next cycle's probe")
	}
	if !rs.recordLiveness("youtube", false, time.Now()) {
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

// TestFallbackObservationAgesOutWithinOneCadence pins livenessFreshWindow
// against the cadence it has to interlock with.
//
// The fallback records its own answer through the same method the membership
// probe uses, so if its own stamp still read as fresh on the next tick the
// probe would suppress itself on alternate cycles — quietly halving a coverage
// nobody decided to halve. The lower bound matters too: a window so short that
// a minutes-old membership observation reads as stale would make the skip
// useless and put a second page fetch on every cycle.
//
// The staleness assertion deliberately sits SHORT of a full cadence. The
// ticker measures tick-to-tick, while the stamp is written at the tail of a
// pass, so consecutive fallback stamps are one cadence apart MINUS however
// long the pass took — up to ~50s of auth checks and page fetch. A window that
// only just cleared defaultRefreshInterval would therefore still self-suppress
// in the field; passingSlack covers that gap.
func TestFallbackObservationAgesOutWithinOneCadence(t *testing.T) {
	const passSlack = 2 * time.Minute

	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	t0 := time.Now()
	rs.recordLiveness("youtube", true, t0)

	if !rs.livenessObservedRecently("youtube", t0.Add(time.Minute)) {
		t.Error("a one-minute-old observation is stale — the fallback would fire on every cycle despite a working membership probe")
	}
	if rs.livenessObservedRecently("youtube", t0.Add(defaultRefreshInterval-passSlack)) {
		t.Errorf("an observation %v old still reads as fresh — the fallback's own stamp would suppress the next cycle's probe", defaultRefreshInterval-passSlack)
	}
}

// TestTierOneRecoveryStampsTheLivenessDedupe: a dead session makes the tier-1
// auth check fire recovery AND makes the fallback probe at the tail of the
// same pass report logged-out. The operator-facing notification coalesces on
// its own cooldown, but each OnRecoveryNeeded call spawns its own two-minute
// headless-browser attempt, so the second one has to be suppressed here.
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
	if rs.recordLiveness("youtube", false, time.Now()) {
		t.Error("a logged-out liveness verdict cleared the dedupe in the same window tier-1 recovery fired in — that is a second headless-browser attempt for one problem")
	}
}
