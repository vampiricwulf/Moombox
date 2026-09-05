package cookies

// autocookies_setup_slot.go — the setup slot lifecycle: one liveness probe,
// three predicates and the inline reaper that decide whether an interactive
// setup is still in use, without the answer ever being "yes, forever".

import (
	"time"
)

// --- setup slot lifecycle ---
//
// One liveness probe, three predicates and one reaper — the predicates and the
// reaper all requiring s.mu — which together answer "is the setup slot still in
// use?" without the answer being "yes, forever".
//
// It used to be "forever". The wait goroutines set browserExited when the user
// closed the browser or walked away, and NOTHING ever cleared setupProcess, so
// SetupInProgress stayed true and StartSetup, RefreshCookies and GetStatus all
// kept refusing until the process restarted. One abandoned wizard wedged every
// form of cookie acquisition for the lifetime of the run.
//
// The correction that shapes all of it: browserExited is a statement about the
// PROCESS MOOMBOX SPAWNED, not about the browser. Where the two differ — every
// Firefox-family launch, and any Chromium behind a shim — only the Job Object
// knows, and where there is no Job Object nothing knows. See setupBrowserGone.

// setupBrowserGone reports whether every process the setup's Job Object was
// tracking is gone, and — separately — whether anything could actually say.
//
// PROCESS EXIT IS A HINT; AN EMPTY JOB IS THE FACT. This is Arc 0's finding,
// applied to the setup slot. `cmd.Wait()` returning tells us the process
// MOOMBOX SPAWNED exited, which for a launcher is not the browser: Firefox and
// its forks hand off and exit in ~170ms (measured — see drainJob's doc, where
// closing the job at that moment was found to kill the browser mid-load), and
// a Chromium binary behind a shim, a `.bat`, `msedge_proxy.exe`, a snap or any
// custom path accepted through Settings does the same. Believing the hint
// there would have the reap close a Job Object with the user's live login
// inside it 60 seconds after they started typing.
//
// `known` false means NO ANSWER, and the caller must read that as "still
// running" — never as "gone". drainJob draws the identical line on the same
// syscall: a zero from a platform that cannot count is "nothing was waited on",
// which is a different statement from "the browser finished".
//
// WHERE THAT LEAVES THE REAP, stated so nobody has to derive it:
//
//   - Windows + Chromium-family — answerable, and the reap works.
//   - Windows + Firefox-family — answerable too, since startFirefoxSetup
//     creates and stores a job of its own. That was the last Windows path where
//     the reap could not fire, and it was the common one: knownBrowsers lists
//     the Firefox family ahead of every Chromium entry.
//   - Linux and Docker — answerable since the process-group reap landed.
//     configureCmdSysProcAttr sets Setpgid, so every browser leads its own
//     group; queryable() is true once that group was adopted, and
//     activeProcesses counts its members from /proc. One case still answers
//     "no idea", honestly: a container whose /proc cannot be walked. One
//     answers WRONGLY: a browser that called setsid() and left the group
//     reads as gone, and the reap releases the slot with it still on screen
//     (no kill — the group it would signal is empty). Which packagings do
//     that is unmeasured. NOT FIELD-VERIFIED — built and unit-tested against
//     a fake process table, with a user's bug report as the gate.
//   - darwin and every other target — no primitive at all (job_other.go is
//     still a no-op stub), so nothing is answerable and the reap never fires.
//     The client-side cancel (the unload beacon, Skip, Escape, the TUI
//     countdown) is what clears an abandoned setup there.
//
// Three answerable targets and one that is not. Wherever it is not — and on
// Windows and Linux wherever newProcessJob or its assign failed — the
// client-side cancel (the unload beacon, Skip, Escape, and the TUI countdown)
// is what clears an abandoned setup; the gap is specifically "no client
// survived to say anything".
//
// A package variable so tests can supply the answer a real Job Object gives on
// a machine where no browser may be launched — the same seam convention as
// detectBrowser, killProcessTree and writeCookieFile. Nothing in production
// reassigns it.
var setupBrowserGone = realSetupBrowserGone

// realSetupBrowserGone is setupBrowserGone's implementation — see there for the
// contract and for why `known` is the half that matters. Split out and named
// only so a test that has stubbed the seam can put the genuine probe back for
// one case; nothing else should call it directly.
func realSetupBrowserGone(job *processJob) (gone, known bool) {
	return browserGoneFrom(job)
}

// browserGoneFrom is realSetupBrowserGone's body, written against the two
// methods it actually uses rather than against *processJob.
//
// The split buys exactly one thing, and it is the thing the owner's ruling
// asked for. The Linux processJob forwards both methods to a pgroupJob, so a
// test on Windows can hand THIS function a pgroupJob backed by a fake process
// table and execute the real pairing — including the branch that matters most,
// a table that cannot be read answering "cannot say" rather than "gone". No
// Linux box, no browser, and no second copy of the rule to drift.
//
// queryable, not `job != nil`. activeProcesses answers 0 for three different
// situations and only one of them is "the job is empty": a nil job (a launch
// where newProcessJob failed, or where the assign failed and the launcher
// dropped the untrackable job rather than let it lie), an already-closed handle
// or a forgotten group, and a platform whose processJob cannot count at all.
// The type knows which it is; this does not.
//
// Passing a nil *processJob through the interface parameter still answers
// (false, false): all three platform implementations nil-check their receiver,
// which is the same property that makes queryable() the right question.
func browserGoneFrom(job interface {
	queryable() bool
	activeProcesses() (int, error)
}) (gone, known bool) {
	if !job.queryable() {
		return false, false
	}
	active, err := job.activeProcesses()
	if err != nil {
		return false, false
	}
	return active == 0, true
}

// setupBrowserLiveLocked reports whether a setup browser is still running.
//
// Three cases, in the order they are cheap:
//
//   - no process registered            → not live;
//   - the spawned process has not exited → live, on the hint alone, which is
//     sufficient in that direction: a running launcher means a running setup;
//   - the spawned process HAS exited    → ask the Job Object, and treat "no
//     answer" as live. See setupBrowserGone for why the hint cannot be trusted
//     in this direction and what the consequence of trusting it would be.
//
// Caller must hold s.mu.
func (s *AutoCookieService) setupBrowserLiveLocked() bool {
	if s.setupProcess == nil {
		return false
	}
	if !s.browserExited {
		return true
	}
	gone, known := setupBrowserGone(s.setupJob)
	return !(known && gone)
}

// setupRetainedLocked reports whether a setup whose spawned process has ALREADY
// exited is still being held. It is the grace window, and its only purpose is
// to let a FinishSetup that is in flight finish: see setupAbandonGrace.
//
// The distinction from setupBrowserLiveLocked matters because the two decay
// differently — "live" ends when the browser is observed to be gone, "retained"
// ends on a clock — and because only the retained state is ever reaped. The two
// overlap while a launcher has exited but its browser has not; that is
// deliberate, since both disjuncts of setupInProgressLocked say "hands off" and
// the reap tests them in order.
//
// Caller must hold s.mu.
func (s *AutoCookieService) setupRetainedLocked() bool {
	return s.setupProcess != nil && s.browserExited &&
		time.Since(s.setupRetainedSince) < setupAbandonGrace
}

// setupInProgressLocked is the predicate the three ACQUISITION consumers share:
// GetStatus publishes it as SetupInProgress, StartSetup refuses on it and
// RefreshCookiesDetailed declines on it. Keeping it in one place is the point —
// all three used to spell `setupProcess != nil || setupClaimed` out
// individually, and a fix applied to two of them would have been silent.
//
// CancelSetup is the fourth reader of that old expression and is DELIBERATELY
// NOT MIGRATED; see its doc for why the two predicates are now different, and
// in which direction.
//
// The claim is in it for the reason CancelSetup's doc gives: between
// StartSetup's gate and the browser launch there is no process yet, but there
// IS a setup in flight.
//
// Caller must hold s.mu.
func (s *AutoCookieService) setupInProgressLocked() bool {
	return s.setupClaimed || s.setupBrowserLiveLocked() || s.setupRetainedLocked()
}

// reapAbandonedSetupLocked releases a setup whose browser is gone and whose
// grace window has run out. Called INLINE by the three consumers that are about
// to test setupInProgressLocked, each already holding s.mu.
//
// IT NEVER CLOSES A JOB OBJECT THAT STILL HAS PROCESSES IN IT. The liveness
// test is setupBrowserLiveLocked, which asks the job rather than trusting the
// spawned process's exit, and which answers "live" whenever nothing can say.
// Read its doc before changing the order of the tests below.
//
// THERE IS DELIBERATELY NO REAPER GOROUTINE, and one must never be added. A
// reaper that sleeps and then takes the lock decides what to reap at a moment
// it did not observe: by the time it wakes, the slot it sampled can hold a
// NEWER attempt, and cleanupLocked closes the setup Job Object —
// KILL_ON_JOB_CLOSE — which would terminate the browser window the user is
// signed into right now. Reaping inline, under the lock the caller already
// holds, means the predicate and the cleanup see the same instant and no such
// gap exists. (The cancel-on-timeout that DOES need a clock is a client
// concern; the TUI wizard's countdown at tui/setup_wizard.go is where it
// lives, and it POSTs a cancel rather than reaching into this state.)
//
// Caller must hold s.mu.
func (s *AutoCookieService) reapAbandonedSetupLocked() {
	if s.setupProcess == nil || s.setupClaimed {
		return // nothing registered, or a StartSetup owns the slot right now
	}
	if s.setupBrowserLiveLocked() {
		// Re-arm the grace from the last moment the setup was OBSERVED alive,
		// not from the moment its launcher exited. Without this a browser that
		// outlives its launcher — the whole reason the job is consulted above —
		// would burn its entire window while still running, and be reaped the
		// instant it closed with no grace left for the finish the user is about
		// to ask for. Best-effort, because it only advances when someone looks;
		// FinishSetup's own stamp is what guarantees a finish its full window.
		s.setupRetainedSince = time.Now()
		return
	}
	if s.setupRetainedLocked() {
		return
	}
	if s.logger != nil {
		s.logger.Info("releasing an abandoned cookie setup — its browser is gone and no finish followed",
			"last_seen_alive", time.Since(s.setupRetainedSince).Round(time.Second).String()+" ago",
			"platform", s.targetPlatform)
	}
	s.cleanupLocked()
}
