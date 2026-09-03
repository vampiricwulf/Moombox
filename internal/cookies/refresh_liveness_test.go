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
// warrant recovery — see livenessRefireWindow for what the extra ones cost.
// (They no longer cost the operator a wrong message: a declined pass reports
// nothing since runCookieRecovery's Unknown branch started splitting on
// RefreshResult.Ran. What is left is the work.)
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

// TestRecordLivenessRefireWindowDoublesAfterEachAlarm is the FACTOR.
//
// TestRecordLivenessRefiresAfterTheWindow above is the BASE case and is
// unchanged: the first re-alarm lands one livenessRefireWindow after the first
// alarm. This picks up where that stops. "30 min, then double" means the
// doubling begins AFTER the first re-alarm, so a session dead for 90 minutes
// has produced three alarms, not four.
//
// Mutation: drop `next *= livenessRefireFactor` from escalateLivenessRefire.
// Every re-alarm then lands on the flat window and an armed tier 2 pages an
// operator 48 times a day, forever, for one loss.
func TestRecordLivenessRefireWindowDoublesAfterEachAlarm(t *testing.T) {
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	t0 := time.Now()

	if due, _ := rs.recordLiveness("youtube", false, t0); !due {
		t.Fatal("premise broken: the first logged-out verdict must warrant recovery")
	}
	if due, _ := rs.recordLiveness("youtube", false, t0.Add(livenessRefireWindow)); !due {
		t.Fatal("premise broken: the first re-alarm must land one base window later")
	}

	second := t0.Add(livenessRefireWindow)
	if due, _ := rs.recordLiveness("youtube", false, second.Add(livenessRefireWindow)); due {
		t.Error("a verdict one BASE window after the first re-alarm warranted recovery — the window did not double")
	}
	if due, _ := rs.recordLiveness("youtube", false, second.Add(2*livenessRefireWindow)); !due {
		t.Error("a verdict two base windows after the first re-alarm did not warrant recovery — the doubled window is wrong or the schedule latched")
	}
}

// TestRecordLivenessRefireWindowStopsAtTheCap is the CAP. Uncapped doubling
// reaches a fortnight in eleven alarms, and a session that recovered two weeks
// ago is still suppressed.
//
// Mutation: delete the `if next > livenessRefireCap` clamp — the window after
// the sixth alarm is then 32 h.
func TestRecordLivenessRefireWindowStopsAtTheCap(t *testing.T) {
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	now := time.Now()

	// A full DAY between alarms is longer than every window on the way up, so
	// each one fires and each one escalates: 30m, 1h, 2h, 4h, 8h, 16h, 24h.
	for i := range 8 {
		if due, _ := rs.recordLiveness("youtube", false, now.Add(time.Duration(i)*24*time.Hour)); !due {
			t.Fatalf("alarm %d did not fire although a full day had passed — the schedule is not advancing", i)
		}
	}

	rs.mu.RLock()
	got := rs.livenessRefireBackoff["youtube"]
	rs.mu.RUnlock()
	if got != livenessRefireCap {
		t.Errorf("the back-off settled at %v, want the %v cap", got, livenessRefireCap)
	}

	// The cap is a real window, not just a stored number.
	last := now.Add(7 * 24 * time.Hour)
	if due, _ := rs.recordLiveness("youtube", false, last.Add(livenessRefireCap-time.Minute)); due {
		t.Error("a verdict inside the capped window warranted recovery")
	}
	if due, _ := rs.recordLiveness("youtube", false, last.Add(livenessRefireCap)); !due {
		t.Error("a verdict a full cap later did not warrant recovery — the schedule latched at the cap")
	}
}

// TestRecordLivenessRefireResetsWhenAuthReturns is the RESET. A session that
// died, escalated for a day, was repaired and died again is a NEW loss;
// reporting it on the old schedule holds the alarm for up to 24 hours.
//
// The reset touches the ESCALATION only — lastRecoveryDecided is left standing
// (see recordLiveness), so this also states that boundary. The final verdict is
// therefore placed exactly one BASE window after the second alarm's stamp: the
// base schedule fires there and the escalated one (2x base) does not, which is
// the only interval that tells the two apart.
//
// Mutation: drop resetLivenessRefire from the loggedIn branch.
func TestRecordLivenessRefireResetsWhenAuthReturns(t *testing.T) {
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	t0 := time.Now()

	if due, _ := rs.recordLiveness("youtube", false, t0); !due {
		t.Fatal("premise broken: the first logged-out verdict must warrant recovery")
	}
	// Stamps at t0+base and escalates the window to 2x base.
	if due, _ := rs.recordLiveness("youtube", false, t0.Add(livenessRefireWindow)); !due {
		t.Fatal("premise broken: the first re-alarm must land one base window later")
	}

	// Auth comes back a minute later. Silent, and it moves no stamp — only the
	// escalation.
	if due, _ := rs.recordLiveness("youtube", true, t0.Add(livenessRefireWindow+time.Minute)); due {
		t.Fatal("a logged-in observation warranted recovery")
	}

	// One BASE window after the last stamp. On the base schedule that fires;
	// on the escalated one it is still half a window short.
	if due, _ := rs.recordLiveness("youtube", false, t0.Add(2*livenessRefireWindow)); !due {
		t.Error("the re-alarm after a repair still wanted the escalated window — a fresh loss must be reported on the base schedule")
	}
}

// TestRecordLivenessRefireBackoffIsPerPlatform: the schedule is keyed like the
// three maps beside it. TestRecordLivenessSeparatesPlatforms pins the same
// property for the dedupe STAMP; this pins it for the WINDOW, which a single
// shared time.Duration would collapse.
//
// Mutation: make livenessRefireBackoff a plain time.Duration field.
func TestRecordLivenessRefireBackoffIsPerPlatform(t *testing.T) {
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})
	now := time.Now()

	for i := range 6 {
		rs.recordLiveness("youtube", false, now.Add(time.Duration(i)*24*time.Hour))
	}

	last := now.Add(5 * 24 * time.Hour)
	if due, _ := rs.recordLiveness("twitch", false, last); !due {
		t.Fatal("twitch's first logged-out verdict did not warrant recovery")
	}
	if due, _ := rs.recordLiveness("twitch", false, last.Add(livenessRefireWindow)); !due {
		t.Error("twitch's re-alarm waited on YouTube's escalated window — the back-off is not per platform")
	}
}

// TestLivenessRefireScheduleIsTheRuledNumbers pins the three constants to the
// owner's numbers (2026-08-29): 30 min, then double, capped at 24 h.
//
// The four schedule tests above use the constants SYMBOLICALLY — each one
// passes just as well for a 15-minute base or a 20-hour cap — so this is the
// only thing in the file that fails when a number drifts. It is deliberately a
// literal pin and nothing more: the SHAPE of the schedule is the other four
// tests' job, and restating it here with numbers would be a second copy of the
// same arithmetic to keep in step.
//
// Mutations: livenessRefireWindow = 15 * time.Minute; livenessRefireFactor = 3;
// livenessRefireCap = 20 * time.Hour. Each fails exactly one line below.
func TestLivenessRefireScheduleIsTheRuledNumbers(t *testing.T) {
	if livenessRefireWindow != 30*time.Minute {
		t.Errorf("livenessRefireWindow = %v, want 30m — the owner's base", livenessRefireWindow)
	}
	if livenessRefireFactor != 2 {
		t.Errorf("livenessRefireFactor = %d, want 2 — \"then double\"", livenessRefireFactor)
	}
	if livenessRefireCap != 24*time.Hour {
		t.Errorf("livenessRefireCap = %v, want 24h — \"capped near a day\"", livenessRefireCap)
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

// TestInconclusiveFallbackIsReportedOncePerRun is the other half of
// TestFallbackInconclusiveMovesNothing: moving no state must not mean saying
// nothing.
//
// The pilot is log-only, so the log IS its evidence — and with the
// inconclusive outcome unlogged, "the probe is healthy and has nothing new to
// report" and "the probe has never once been able to answer" produced the
// identical (empty) record. Whether the tier-2 signal is alive is precisely
// the judgement the pilot exists to inform, so it has to be visible.
//
// Deduped, and that is the harder half. An install permanently behind a
// redirecting captive portal or a proxy answering on another host is
// inconclusive on EVERY cycle, forever; a line per cycle would be noise fanned
// out over the WebSocket stream to the Web UI and TUI as well as the log. Same
// granularity rule ObserveLiveness follows: notable on a change of what is
// known about the platform, Debug on a repeat.
func TestInconclusiveFallbackIsReportedOncePerRun(t *testing.T) {
	healthyRefreshSeams(t)

	const line = "liveness fallback probe learned nothing"

	log := &capturingLogger{}
	called := 0
	rs := NewRefreshService(jarWithAuth(t), 0, log)
	rs.FallbackLiveness = func(context.Context) (bool, bool) { called++; return false, false }

	for range 3 {
		rs.doRefresh(context.Background())
	}

	// The probe really did run every cycle: an inconclusive outcome records no
	// observation, so nothing suppresses the next one. Without this, one Info
	// line is equally well explained by the probe having run exactly once.
	if called != 3 {
		t.Fatalf("fallback ran %d times over 3 refreshes, want 3 — the log counts below say nothing otherwise", called)
	}
	if got := countContaining(log.infos, line); got != 1 {
		t.Errorf("%d operator-visible lines about an inconclusive probe over 3 cycles, want exactly 1: %q", got, log.infos)
	}
	if got := countContaining(log.debugs, line); got != 2 {
		t.Errorf("%d debug-level repeats, want 2 — the repeats must still be recorded, just not at Info: %q", got, log.debugs)
	}
}

// TestInconclusiveFallbackIsSilentWhenNothingIsConfigured: the line exists to
// separate "the signal is dead" from "healthy, nothing to say", and an install
// that holds no YouTube cookies at all is neither.
//
// ProbeAccountLiveness returns (Unknown, nil) from its own first gate for that
// install — there is no session to report on — so an ungated line would say
// "the probe learned nothing about this session" once per process on every
// cookie-less deployment, diluting exactly the distinction it was added to
// sharpen.
//
// The second half is the part that would have been easy to get wrong: the gate
// has to cover the RECORD too. Recording silently would leave the entry at
// livenessInconclusive, so the first genuine failure after cookies arrive
// would read as a repeat and be demoted to Debug — the pilot would miss the
// one line it most needs.
func TestInconclusiveFallbackIsSilentWhenNothingIsConfigured(t *testing.T) {
	const line = "liveness fallback probe learned nothing"

	log := &capturingLogger{}
	called := 0
	// A bare jar: no YouTube auth cookie of any kind.
	rs := NewRefreshService(NewCookieJar(), 0, log)
	rs.FallbackLiveness = func(context.Context) (bool, bool) { called++; return false, false }

	rs.doRefresh(context.Background())

	if called != 1 {
		t.Fatalf("fallback ran %d times, want 1 — the silence below says nothing about a probe that never ran", called)
	}
	if got := countContaining(log.msgs, line); got != 0 {
		t.Errorf("an install with no YouTube cookies was told the probe learned nothing about its session: %q", log.msgs)
	}

	// The record must be untouched, or the first real failure after cookies
	// arrive is demoted to a repeat.
	if !rs.recordInconclusiveLiveness("youtube") {
		t.Error("the silent pass recorded livenessInconclusive anyway — the first genuine failure would land at Debug")
	}
}

// TestInconclusiveFallbackIsNotableAgainAfterAVerdict: the dedupe is keyed on
// what is KNOWN about the platform, not on a latch. A probe that starts
// answering and then stops answering has changed state twice, and each change
// is the operator-visible event.
//
// This is what makes sharing one record with the verdicts worth doing rather
// than bolting on a second boolean: "has this changed since last time" has one
// answer covering all three states.
func TestInconclusiveFallbackIsNotableAgainAfterAVerdict(t *testing.T) {
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})

	if !rs.recordInconclusiveLiveness("youtube") {
		t.Fatal("premise broken: the first inconclusive outcome of the process must be notable")
	}
	if rs.recordInconclusiveLiveness("youtube") {
		t.Error("a repeated inconclusive outcome was notable — an install behind a redirecting intermediary would say so every cycle forever")
	}
	if _, notable := rs.recordLiveness("youtube", true, time.Now()); !notable {
		t.Error("the probe recovering to a real verdict was not notable — that is the event that says the signal came back")
	}
	if !rs.recordInconclusiveLiveness("youtube") {
		t.Error("losing the signal again after a verdict was not notable — the dedupe latched instead of tracking the change")
	}
}

// TestInconclusiveFallbackDoesNotSuppressTheNextProbe guards the one way this
// logging could have done damage: recordInconclusiveLiveness shares a map with
// the verdict path, and writing the WRONG map would make an inconclusive
// outcome look like an observation — silencing the probe for a full
// livenessFreshWindow precisely while it is failing.
//
// TestFallbackInconclusiveMovesNothing asserts the same property through
// doRefresh; this asserts it against the recording method directly, so a
// future caller of that method inherits the pin.
func TestInconclusiveFallbackDoesNotSuppressTheNextProbe(t *testing.T) {
	rs := NewRefreshService(jarWithAuth(t), 0, nopLogger{})

	rs.recordInconclusiveLiveness("youtube")

	if rs.livenessObservedRecently("youtube", time.Now()) {
		t.Error("an inconclusive outcome recorded an observation — the next cycle's probe would be skipped for being 'fresh'")
	}
	if due, _ := rs.recordLiveness("youtube", false, time.Now()); !due {
		t.Error("an inconclusive outcome consumed the recovery dedupe — a real logged-out verdict would be swallowed")
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
// same pass report logged-out. The second fire is pure waste: the auto-cookie
// single-flight declines it while the first attempt is still running, so it
// buys a goroutine and a 2-minute timeout context to be told no.
//
// It used to be worse than waste — a decline notified "Ineffective" and burned
// the platform's 30-minute cooldown, so the real verdict's actionable message
// was dropped. That is fixed at the source now (runCookieRecovery splits the
// Unknown branch on RefreshResult.Ran), and this stamp is no longer the only
// thing standing between the operator and the wrong message. It stays because
// firing twice for one problem was never right on its own terms.
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
		t.Error("a logged-out liveness verdict cleared the dedupe in the same window tier-1 recovery fired in — the second attempt would be declined by the auto-cookie single-flight, having spent a goroutine and a two-minute timeout on a problem the first one is already working")
	}
}
