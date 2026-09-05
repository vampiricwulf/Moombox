package cookies

// refresh_liveness.go — the liveness recovery pilot: recording external
// liveness verdicts, the per-platform re-fire back-off/dedupe, and the
// fallback-probe bookkeeping that feeds OnRecoveryNeeded.

import "time"

// livenessRecordOf maps a conclusive verdict onto its record.
func livenessRecordOf(loggedIn bool) livenessRecord {
	if loggedIn {
		return livenessSignedIn
	}
	return livenessSignedOut
}

// livenessRecoveryArmed gates whether an external liveness verdict may
// actually invoke OnRecoveryNeeded.
//
// ARMED 2026-09-03 by owner ruling — "if our goal is to have it armed, then we
// should arm it" — which also skipped the five-day pre-arming soak the staged
// rollout had asked for. What that means, on BOTH install shapes — cmd/moombox's
// handleRecoveryNeeded splits on cookies.auto_enabled and neither arm is silent
// about a session it cannot restore:
//
//   - auto_enabled = true: a goroutine runs RefreshCookiesDetailed under a
//     2-minute timeout, which drives a headless browser. TWO outcomes are
//     quiet — a successful refresh, and a pass that DECLINED to run at all
//     (the Ran == false half of runCookieRecovery's Unknown branch, which
//     logs and returns). The rest notify: "Cookie Auto-Refresh Failed"
//     (TypeError) for a transport error or a conclusive failure, "Cookie
//     Auto-Refresh Ineffective" (TypeWarning) for a pass that ran without
//     reaching an answer. A spurious verdict is by definition one no refresh
//     can fix, so it lands in a notifying outcome unless it is declined.
//
//     Do not read the decline as a safety net. It is a RACE: the pass is
//     declined only while another one holds the auto-cookie single-flight,
//     which is likely for a verdict produced in the same pass as a tier-1
//     fire (that is what noteRecoveryDecided is for) and not otherwise. A
//     spurious LoggedOut arriving with the slot free runs the browser and
//     notifies exactly as before.
//
//   - auto_enabled = false: no browser, and no quiet case at all — the decline
//     above cannot help here, because handleRecoveryNeeded returns on this arm
//     without calling the refresher, so there is no single-flight to lose. A
//     SYNCHRONOUS "Cookie Re-Authentication Required" (TypeError) naming the
//     cookie file, every time. This arm used to Debug-log and send nothing;
//     Task 7 replaced that silence, so an armed tier 2 now alarms the
//     population this remediation elsewhere identifies as LEAST able to reach
//     the remedy it names — containers, remote dashboards, a loopback-gated
//     setup wizard.
//
// A per-platform 30-minute cooldown in wireMonitorCallbacks bounds how often
// that repeats; it does not withhold the first one.
//
// So the risk carried by this constant is NOT scoped to auto_enabled installs,
// and it was never the browser — the disabled shape is if anything the worse of
// the two, because the operator it pages has no automated attempt that might
// have quietly fixed things first. It is that a false LoggedOut sends an
// operator to re-export credentials that were never wrong, on every install
// shape. That risk is now ACCEPTED rather than deferred.
//
// WHAT BOUNDS A WRONG VERDICT now that this gate no longer does:
//
//   - The per-platform back-off recordLiveness computes. The first re-alarm
//     lands livenessRefireWindow (30 min) after the first alarm; every alarm
//     after that multiplies the window by livenessRefireFactor, up to
//     livenessRefireCap (24 h); only a conclusive signed-in verdict puts the
//     platform back on the base (resetLivenessRefire). A session that reads
//     dead forever therefore pages on a decaying schedule, not 48 times a day.
//   - The tier-1 stamp. Both tier-1 fire paths — refresh's status block and
//     NoteTwitchAuthLoss — call noteRecoveryDecided, which writes
//     lastRecoveryDecided, the same map recordLiveness consults. So tier 2
//     cannot fire a second recovery for a loss tier 1 has already raised
//     WITHIN THAT PLATFORM'S CURRENT WINDOW. That is one window's suppression,
//     not a permanent one: a session still dead when the window expires
//     re-alarms on the back-off above, which is the intended behaviour — a
//     tier-1 fire consumes one tier-2 window without growing the next.
//
// THE WAY BACK IS ANOTHER BUILD. This is a source constant, not a config flag:
// nothing toggles it at runtime and -ldflags cannot reach it. A false verdict
// in the field is reversed by setting it back to false and rebuilding.
const livenessRecoveryArmed = true

// ObserveLiveness records an external verdict about whether `platform`'s
// stored session is still signed in and — livenessRecoveryArmed having been
// true since 2026-09-03 — fires OnRecoveryNeeded for a signed-out one that
// cleared the per-platform dedupe.
//
// Callers must filter their own inconclusive results out: reaching this method
// means "the platform told us", not "we asked". A consent wall, a rate limit, an
// off-host redirect and a never-configured jar are all silence, and passing
// any of them in as loggedIn=false would report working credentials as dead.
//
// Three producers exist today: YouTube's per-channel membership probe (which
// runs once per configured channel per feed cycle), YouTube's
// channel-independent FallbackLiveness probe, and Twitch's
// TwitchFallbackLiveness probe. The first is why the dedupe is not optional —
// one dead session must raise one alarm, not one per channel.
func (rs *RefreshService) ObserveLiveness(platform string, loggedIn bool) {
	due, notable := rs.recordLiveness(platform, loggedIn, time.Now())

	// This line is the record of what the signal DID, written whether or not
	// the verdict goes on to fire, so the level is chosen to keep every line
	// that reading needs at Info while a healthy install stays quiet. Since
	// arming, wouldFireRecovery=true here is the line the Warn below and a
	// recovery attempt follow.
	//
	// Notable (Info) is every signed-out verdict, every change of verdict, and
	// the first observation of the process. Everything else is a repeat of an
	// answer already on the record, and repeats are the volume problem: this
	// method is called once per configured channel per feed cycle, which at
	// the default 10-minute cadence is 144*N lines a day — every one of them
	// also fanned out over the WebSocket log stream to the Web UI and TUI. A
	// healthy install now emits roughly one line per process instead.
	//
	// A signed-out verdict is never demoted, even when the dedupe already
	// refused it. Losing evidence of a dead session is the one direction this
	// must not fail in, and wouldFireRecovery on the line says which of the
	// burst cleared the dedupe.
	//
	// The line carries the verdict and the two decisions — never anything read
	// off the page the verdict came from.
	logAt := rs.logger.Debug
	if notable {
		logAt = rs.logger.Info
	}
	logAt("liveness observation",
		"platform", platform,
		"loggedIn", loggedIn,
		"wouldFireRecovery", due,
		"armed", livenessRecoveryArmed)

	if !livenessRecoveryArmed || !due {
		return
	}
	if fn := rs.OnRecoveryNeeded; fn != nil {
		// States what this method was told, and stops there. ObserveLiveness
		// has three producers — the per-channel membership probe and the two
		// channel-independent fallbacks — and cannot tell which sent this
		// verdict. This is the line that pages an operator, so it must not
		// name a mechanism it cannot know.
		rs.logger.Warn("a liveness observation reports this platform is signed out, triggering recovery", "platform", platform)
		fn(platform)
	}
}

// recordLiveness folds one conclusive observation into the liveness maps and
// reports two independent things about it:
//
//   - recoveryDue: it is signed out and cleared the dedupe, so it warrants
//     firing OnRecoveryNeeded.
//   - notable: it is worth an operator-visible log line — signed out, or a
//     change from this platform's previous verdict, or the first observation
//     of the process. See ObserveLiveness for why the distinction exists.
//
// Split out of ObserveLiveness so both decisions are testable on their own,
// upstream of the pilot gate and of the callback it guards.
//
// `now` is a parameter so a test can drive the windows without sleeping
// through them.
//
// The lock is released before ObserveLiveness invokes the callback, following
// doRefresh's convention: OnRecoveryNeeded reaches out into cmd/moombox and
// must not run under this service's mutex.
func (rs *RefreshService) recordLiveness(platform string, loggedIn bool, now time.Time) (recoveryDue, notable bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	if rs.lastLivenessObserved == nil {
		rs.lastLivenessObserved = make(map[string]time.Time)
	}
	if rs.lastLivenessKnown == nil {
		rs.lastLivenessKnown = make(map[string]livenessRecord)
	}

	// Read what was last known BEFORE this observation overwrites it. The
	// missing entry reads as livenessNever, which differs from both verdicts,
	// so a platform's first observation is notable on its own: it is the
	// record that the signal started producing.
	record := livenessRecordOf(loggedIn)
	notable = !loggedIn || rs.lastLivenessKnown[platform] != record

	// Both directions. This map answers "did anything tell us recently", and
	// a healthy answer settles that question exactly as well as a dead one.
	rs.lastLivenessObserved[platform] = now
	rs.lastLivenessKnown[platform] = record

	if loggedIn {
		// Positive evidence is silent, and must not touch lastRecoveryDecided:
		// stamping it here would let a healthy verdict swallow a dead one
		// arriving a moment later from another channel in the same cycle.
		//
		// It DOES clear the back-off. "Auth came back" is the reset condition
		// the owner named, and a session that dies again after a repair is a
		// new loss to report on the base schedule rather than yesterday's
		// escalated one. See resetLivenessRefire for why this is the
		// escalation only and not the stamp.
		rs.resetLivenessRefire(platform)
		return false, notable
	}
	if last, ok := rs.lastRecoveryDecided[platform]; ok && now.Sub(last) < rs.livenessRefireWindowFor(platform) {
		return false, notable
	}
	if rs.lastRecoveryDecided == nil {
		rs.lastRecoveryDecided = make(map[string]time.Time)
	}
	rs.lastRecoveryDecided[platform] = now
	// After the window CHECK above (its position relative to the stamp is
	// immaterial), so this verdict was judged against the window in force when
	// it arrived. Escalating before the check would judge every alarm against
	// the window meant for the one after it — and a SUPPRESSED verdict would
	// grow the window too, so the base re-alarm would never land at all.
	rs.escalateLivenessRefire(platform)
	return true, notable
}

// livenessRefireWindowFor returns how long must pass since this platform's last
// cleared dedupe before another signed-out verdict may clear it again: the base
// until something has fired, the escalated window after. Callers hold rs.mu.
func (rs *RefreshService) livenessRefireWindowFor(platform string) time.Duration {
	if w := rs.livenessRefireBackoff[platform]; w > 0 {
		return w
	}
	return livenessRefireWindow
}

// escalateLivenessRefire advances one platform's back-off after a verdict that
// cleared the dedupe.
//
// The FIRST call sets the base rather than doubling it, and that is what puts
// the first re-alarm 30 minutes after the first alarm — the owner's "30 min,
// then double". Doubling on the first call would put it at an hour and lose
// the responsive window entirely. Callers hold rs.mu for writing.
func (rs *RefreshService) escalateLivenessRefire(platform string) {
	if rs.livenessRefireBackoff == nil {
		rs.livenessRefireBackoff = make(map[string]time.Duration)
	}
	next := rs.livenessRefireBackoff[platform]
	if next == 0 {
		next = livenessRefireWindow
	} else {
		next *= livenessRefireFactor
	}
	if next > livenessRefireCap {
		next = livenessRefireCap
	}
	rs.livenessRefireBackoff[platform] = next
}

// resetLivenessRefire puts one platform back on the base schedule.
//
// One caller — recordLiveness's conclusive-LoggedIn branch — and it clears the
// ESCALATION only. Clearing lastRecoveryDecided here would let a healthy
// verdict from one channel swallow a dead verdict from the next in the same
// cycle, which is the whole reason those are two maps.
//
// A tier-1 recovery does NOT reach here: noteRecoveryDecided stamps the dedupe
// and nothing else, by the one-directional rule it has always followed. On an
// install with channels the membership probe delivers a conclusive LoggedIn
// within one feed cycle of auth returning. Callers hold rs.mu for writing.
func (rs *RefreshService) resetLivenessRefire(platform string) {
	delete(rs.livenessRefireBackoff, platform)
}

// recordInconclusiveLiveness folds a fallback probe that learned NOTHING into
// the same per-platform record a real verdict goes into, and reports whether
// that is worth an operator-visible line.
//
// It exists because the liveness pilot's log is read as evidence, and silence
// was ambiguous: an install whose probe is permanently refused — a redirecting
// captive portal, a proxy answering on another host, a rate limit that never
// clears — produced exactly the same log as a perfectly healthy install with
// nothing new to say. That is the one distinction the pilot has to be able to
// make about its own signal.
//
// Deliberately touches NONE of the other three maps:
//
//   - not lastLivenessObserved, because recording an observation would make
//     the next cycle's freshness check skip the probe — silencing the signal
//     for as long as it keeps failing, which is backwards.
//   - not lastRecoveryDecided, because that window belongs to real signed-out
//     verdicts and consuming it here would swallow the next one.
//   - not livenessRefireBackoff, because "inconclusive" is not the reset
//     condition — only a conclusive signed-in verdict is (recordLiveness).
//     Resetting the back-off here would let an install stuck behind a
//     captive portal, proxy or rate limit — inconclusive on EVERY cycle —
//     clear its own escalation every cycle too, so tier 2 would page every
//     base window forever: exactly the failure mode the schedule exists to
//     prevent, reintroduced through the one door silence is supposed to leave
//     shut.
//
// TestFallbackInconclusiveMovesNothing pins all three.
//
// `notable` follows the same rule ObserveLiveness uses: notable on a change of
// what is known, or on the first thing known about the platform in this
// process; a repeat is Debug. An install stuck behind an intermediary
// therefore says so once and then goes quiet, rather than every cycle forever.
func (rs *RefreshService) recordInconclusiveLiveness(platform string) (notable bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.lastLivenessKnown == nil {
		rs.lastLivenessKnown = make(map[string]livenessRecord)
	}
	notable = rs.lastLivenessKnown[platform] != livenessInconclusive
	rs.lastLivenessKnown[platform] = livenessInconclusive
	return notable
}

// noteRecoveryDecided stamps the dedupe map for a recovery that the tier-1
// auth check is about to fire.
//
// One-directional on purpose: the refresh stamps the map so a liveness verdict
// arriving in the same window cannot fire recovery for a problem the tier-1
// check is already working on, but it does not CONSULT the map. Suppressing
// the tier-1 fire would change behaviour that predates this signal entirely,
// and that check is the one with the longest field record.
//
// The second fire would not launch a second browser — RefreshCookiesDetailed
// single-flights — it would be DECLINED, and since runCookieRecovery's Unknown
// branch started splitting on RefreshResult.Ran a decline reports nothing. So
// what this stamp saves is the redundant goroutine and its 2-minute timeout,
// not an operator-visible mistake; livenessRefireWindow has the accounting.
//
// It stamps and does NOT escalate. The back-off counts TIER-2 alarms; a tier-1
// fire consumes one tier-2 window without growing the next, which is the
// conservative direction — the platform stays on the shorter schedule.
func (rs *RefreshService) noteRecoveryDecided(platform string, now time.Time) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.lastRecoveryDecided == nil {
		rs.lastRecoveryDecided = make(map[string]time.Time)
	}
	rs.lastRecoveryDecided[platform] = now
}

// livenessObservedRecently reports whether any conclusive liveness observation
// for `platform` landed within livenessFreshWindow — the sole gate on paying
// for the FallbackLiveness probe.
func (rs *RefreshService) livenessObservedRecently(platform string, now time.Time) bool {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	last, ok := rs.lastLivenessObserved[platform]
	return ok && now.Sub(last) < livenessFreshWindow
}
