package worker

import (
	"errors"
	"sync/atomic"
	"time"

	"github.com/vampiricwulf/Moombox/internal/chat"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// atomicTimeValue wraps atomic.Int64 to store a time.Time as UnixNano for
// lock-free access across goroutines. The zero value represents a zero
// time (IsZero() returns true from Load()). A local copy of engine's
// atomicTime (internal/engine/downloader.go) — kept unexported on both
// sides rather than exported from engine, since this is the only
// worker-side user.
type atomicTimeValue struct{ v atomic.Int64 }

func (a *atomicTimeValue) Store(t time.Time) { a.v.Store(t.UnixNano()) }
func (a *atomicTimeValue) Load() time.Time {
	n := a.v.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}
func (a *atomicTimeValue) StoreNow() { a.v.Store(time.Now().UnixNano()) }

// segmentProgressResetsStallCounters reports whether p represents genuine
// NEW progress for one stream (video or audio) relative to lastBytes — the
// last cumulative Bytes value already observed for that exact stream — and,
// if so, updates lastBytes to p.Bytes (I3 fix).
//
// Extracted from runLiveStreamDownload's onSegmentProgress closure purely
// so this echo-suppression decision is directly unit-testable without a
// live download loop: Bytes>0 alone is NOT sufficient, because
// re-running an already-CANCELLED SegmentDownloader — which
// runLiveStreamDownload's noteRefreshFailure wait branch does on every
// retry, since a failed refresh leaves `result` pointing at the SAME
// cancelled downloaders and the outer loop's next iteration calls
// runDownloaders on them again — makes it immediately hit its own
// deferred end-of-Start final OnProgress a second (or Nth) time,
// reporting the EXACT SAME cumulative Bytes as its last live report (no
// new bytes were written since). Treating that echo as fresh progress
// used to reset consecutiveLiveChecks/lastSegTime on every such retry,
// defeating maxConsecutiveLiveChecks entirely — a stuck-live abandoned
// broadcast could livelock the job in Downloading forever.
//
// Callers key lastBytes PER STREAM (a video and an audio downloader each
// get their own counter, since they're independent cumulative totals) and
// reset it to 0 whenever a brand-new downloader instance is attached for
// that stream, so a genuinely fresh downloader's first real report (delta
// from 0) still counts as progress.
func segmentProgressResetsStallCounters(p engine.DownloadProgress, lastBytes *atomic.Int64) bool {
	if p.Bytes <= 0 || p.Bytes <= lastBytes.Load() {
		return false
	}
	lastBytes.Store(p.Bytes)
	return true
}

// interruptionSignalStaleAfter bounds trust in a signature the observation
// sites stopped re-confirming. The cadences actually feeding observe(),
// fastest to slowest:
//   - The strategies' CheckStreamStatus closures (job.YT.ProbeVideoStatus) —
//     consulted by the engine's OWN retry loop, throttled to at most once
//     per streamStatusCheckInterval (30s, internal/engine/downloader.go).
//     This is the only site that fires WHILE the engine is still internally
//     retrying/stalling on a 403/410 or other HTTP-error burst — every other
//     site below only runs after the downloader has already exited.
//   - refreshGvsCredentials' player-response re-fetch, fired from each
//     downloader's OnCredentialRefresh callback — but only while
//     behindHeadTailPending() holds (segments demonstrably exist below
//     head); once the retry budget is exhausted this stops firing
//     entirely, so it cannot be relied on to keep the signal fresh through
//     a live-edge stall (see downloader_dash.go's behindHeadTailPending
//     gate on d.refreshCredentials()).
//   - The live loop's own GetVideoInfo re-fetches (orchestrator_youtube.go,
//     the ErrQualityLost/quality-change branch and the normal-stop verify
//     branch) — these only run AFTER the downloader has already exited
//     (Start returned), not during an in-engine stall.
//
// 90s comfortably outlasts the 30s CheckStreamStatus throttle (the
// steady-state source during an actual stall) with room for one missed/
// errored check.
const interruptionSignalStaleAfter = 90 * time.Second

// interruptionSignal records the last time a player-response fetch showed
// the broadcast-interrupted signature: streamStatus "live" with zero
// formats. YouTube removes streaming data while ingestion is down but
// keeps the page live; a genuinely ended stream keeps post-live formats.
// Shared by pointer on JobContext so strategyCtx value-copies (segCtx :=
// *jobCtx, segJobCtx := *jobCtx) observe one truth across downloader
// re-creation.
type interruptionSignal struct{ lastSeen atomicTimeValue }

// observe records a fresh interruption-signature observation. nil-safe on
// both the receiver (a job whose JobContext.Interruption was never
// initialized — e.g. many test fixtures — is simply a no-op) and info (a
// failed re-fetch passes nil here and leaves the previous freshness, or
// lack of it, unchanged).
//
// Guarded by isAuthWalledPlayability (I4 fix): a members-only,
// age-restricted, or login-required response is bit-identical to the
// interruption signature (live + zero formats + that PlayabilityError) but
// means something completely different depending on HOW it was fetched.
// An unauthenticated (cookieless) probe against a healthy auth-walled
// stream reliably produces exactly this shape — not an interruption, just
// a probe that can't see formats it was never entitled to — which is why
// observeYouTubeStatusProbe (the cookieless CheckStreamStatus path) always
// needed this guard. But every AUTHENTICATED call site that fetches with
// live cookies (job.YT.GetVideoInfo from the quality-refresh path, the
// still-live re-verification path, and the credential-refresh re-probe)
// can hit the SAME shape for a different reason: the cookies died mid-
// stream. That is a real, distinct failure — but it is not resume
// evidence, and arming the signal on it would tell shouldWaitForResume to
// wait out a broadcast that was never actually interrupted. The guard
// lives HERE, centrally, rather than at each call site, so every present
// and future observe() caller is covered by construction instead of by
// remembering to duplicate the check.
func (s *interruptionSignal) observe(info *youtube.VideoInfo) {
	if s == nil || info == nil {
		return
	}
	if isAuthWalledPlayability(info.PlayabilityError) {
		return
	}
	if info.StreamStatus == youtube.StreamLive && len(info.Formats) == 0 {
		s.lastSeen.StoreNow()
	}
}

// fresh reports whether the interruption signature was observed within
// interruptionSignalStaleAfter. nil-safe (a nil signal is never fresh).
func (s *interruptionSignal) fresh() bool {
	if s == nil {
		return false
	}
	t := s.lastSeen.Load()
	return !t.IsZero() && time.Since(t) < interruptionSignalStaleAfter
}

// buildMayResume builds the MayResume closure installed on every live
// YouTube downloader (interruption spec Tier 1 evidence — engine
// DownloaderOptions.MayResume): true when the interruption signal is fresh
// (a recent player-response fetch showed the broadcast-interrupted
// signature), OR the job's chat downloader still has its LIVE continuation
// open (the chat endpoint keeps issuing continuations even while ingestion
// is stalled). chatDl nil — chat disabled, unavailable, or not a YouTube
// job — falls back to the signal alone.
//
// A single shared function so ExecuteWithChat and this package's tests
// build the exact same closure rather than a test-only reimplementation of
// its logic.
//
// The engine consults the returned closure from its own download-loop
// goroutine (see stallForPossibleResume). Both reads here are safe without
// any additional locking in the closure: interruptionSignal.fresh() is
// atomic.Int64-backed and chat.ChatDownloader.LiveContinuationOpen() is
// mutex-guarded internally.
func buildMayResume(sig *interruptionSignal, chatDl *chat.ChatDownloader) func() bool {
	return func() bool {
		if sig.fresh() {
			return true
		}
		return chatDl != nil && chatDl.LiveContinuationOpen()
	}
}

// engineInterruptionTimeout maps the worker's config-facing
// InterruptionTimeout (JobConfig.InterruptionTimeout: 0 = "stall disabled"
// per the config contract, > 0 = the stall ceiling) onto the value passed
// to engine.DownloaderOptions.InterruptionTimeout at construction time
// (I1 fix). A positive value passes through unchanged as the engine's
// ceiling. A non-positive value — 0 from a normally-loaded config, or a
// negative value defensively, though config validation already clamps
// negative InterruptionTimeout back to the default before it can reach
// here — maps onto engine.InterruptionNoStall rather than straight
// through: engine's own zero means "no ceiling" (unbounded), the exact
// opposite of the config contract's "disabled," so passing 0 unmapped
// would silently turn a disabled stall into an unbounded one for any job
// whose resume evidence holds. Every LIVE YouTube strategy site that also
// wires SegmentWorkers + CheckStreamStatus (and, via attachMayResume,
// MayResume) must route job.Config.InterruptionTimeout through this
// rather than passing it straight through.
func engineInterruptionTimeout(configured time.Duration) time.Duration {
	if configured > 0 {
		return configured
	}
	return engine.InterruptionNoStall
}

// attachMayResume installs mayResume onto dl as its engine MayResume
// callback whenever dl is non-nil — REGARDLESS of the configured
// InterruptionTimeout (I1 fix; this used to gate on interruptionTimeout > 0
// and is the one piece of this fix that inverts an existing test's
// assertion, deliberately — see TestAttachMayResumeAlwaysInstalls).
// Installing MayResume no longer needs to depend on the config value: the
// config→engine mapping now happens once, at construction time, via
// engineInterruptionTimeout (see its doc comment) — a disabled-stall
// (config 0) job's downloader is built with
// InterruptionTimeout: engine.InterruptionNoStall, and the engine's own
// InterruptionNoStall branch of stallForPossibleResume consults MayResume
// exactly once per call and never actually stalls. So a disabled-stall job
// still gets its Tier-2 evidence latched (FinalizedDuringInterruption)
// without ever blocking finalize's latency, and attachMayResume itself no
// longer needs to know the config value at all. dl == nil is a safe no-op
// (VOD downloaders / a strategy that didn't build one).
func attachMayResume(dl *engine.SegmentDownloader, mayResume func() bool) {
	if dl == nil {
		return
	}
	dl.MayResume = mayResume
}

// resumeEvidence reports whether interruption resume evidence holds —
// EITHER this exact refresh's player-response fetch just showed the
// interruption signature (signalFresh), OR the job's chat continuation
// says the broadcast may still resume (mayResume) — independent of
// whether the config permits the loop to actually wait on it.
//
// Split out from shouldWaitForResume (I1 fix): the config contract is
// "interruption_timeout=0 disables the STALL," not "disables Tier-2
// preservation" (config/types.go's doc comment) — a genuinely-interrupted
// config-0 job must still be able to latch resumeWaitLatch even though
// shouldWaitForResume(..., 0) below always returns false for it. See
// noteRefreshFailure, which composes this with shouldWaitForResume exactly
// that way.
func resumeEvidence(signalFresh bool, mayResume func() bool) bool {
	return signalFresh || (mayResume != nil && mayResume())
}

// waitDeadline bounds ONE stall episode's total wait-branch lifetime to
// the job's InterruptionTimeout ceiling (I3 fix).
//
// Why this exists: maxConsecutiveLiveChecks bounds a DIFFERENT branch — the
// still-live re-verification loop in runLiveStreamDownload's "normal
// download stop" section — and never applies to this one. The wait branch
// (shouldWaitForResume returning true) retries on its own five-minute
// sleep for as long as resume evidence keeps holding, with no counter of
// its own; a stuck-live abandoned broadcast whose evidence never lapses
// (e.g. a chat continuation that never closes) can hold this branch
// forever, livelocking the job in Downloading. This gives the branch its
// own independent ceiling.
//
// start is latched by exceeded() the first time it is called for an
// episode (zero value) and is NOT touched by later calls — the episode's
// budget is fixed at first entry, not extended by every retry. reset()
// clears it, called wherever a later refresh SUCCEEDS (the same moment
// resumeWaitLatch.resolved() fires): a resumed broadcast, or an unrelated
// LATER stall episode, must get its own fresh budget rather than
// inheriting an already-expired clock from a resolved earlier one.
type waitDeadline struct{ start time.Time }

// exceeded reports whether the episode's budget (interruptionTimeout) has
// run out as of now, latching start on first call. Callers only reach
// this once interruptionTimeout is already known > 0 (shouldWaitForResume
// refuses to wait at all at config <= 0, so the deadline is simply never
// consulted there — "unreachable," not defensively zero-checked here).
func (d *waitDeadline) exceeded(now time.Time, interruptionTimeout time.Duration) bool {
	if d.start.IsZero() {
		d.start = now
		return false
	}
	return now.Sub(d.start) >= interruptionTimeout
}

// reset clears the latched episode start.
func (d *waitDeadline) reset() { d.start = time.Time{} }

// shouldWaitForResume decides whether a failed live-refresh (refreshErr)
// should wait-and-retry instead of ending the recording immediately — the
// PERMISSION half of the decision, now that resume evidence (see
// resumeEvidence) is a separate concern (I1 fix).
//
// Why this exists: when the interruption signature (live status + zero
// formats) appears, EVERY download strategy's format selection fails on
// that same response — refreshDownload returns a non-nil error. Without
// this gate, the orchestrator's ErrQualityLost/quality-change refresh path
// treated any refresh failure as "the stream is done" and finalized
// immediately, discarding the recording mid-interruption instead of
// waiting for the broadcast to resume — even though the failure's own
// cause (zero formats on a still-live response) IS the resume evidence.
//
// Gated on the stall being enabled at all (interruptionTimeout <= 0 means
// "finalize must never wait" per the config contract — evidence may still
// hold and get latched elsewhere, see noteRefreshFailure, but this
// function itself never permits an actual wait for it) and on evidence
// holding. A nil refreshErr never waits — there is nothing to retry.
//
// I3 fix: also refuses once deadline reports the episode's budget
// (interruptionTimeout, the same duration used as the enable/disable gate)
// has run out — closing the gap where evidence holding indefinitely (a
// stuck-live abandoned broadcast) retried this branch forever with no
// bound of its own. A refused wait falls through to the caller's normal
// bounded verify path exactly like a permission-denied (evidence=false)
// call would.
func shouldWaitForResume(refreshErr error, evidence bool, interruptionTimeout time.Duration, deadline *waitDeadline, now time.Time) bool {
	if refreshErr == nil || interruptionTimeout <= 0 {
		return false
	}
	if !evidence {
		return false
	}
	if deadline.exceeded(now, interruptionTimeout) {
		return false
	}
	return true
}

// noteRefreshFailure is runLiveStreamDownload's full I1+I3-fixed response
// to one failed live-refresh: it ALWAYS latches Tier-2 resume evidence onto
// latch (via latch.wait()) when resumeEvidence holds — regardless of
// whether the config permits an actual stall — and separately reports
// whether the loop should actually wait-and-retry (shouldWaitForResume's
// permission gate, now also bounded by deadline).
//
// A nil refreshErr is a safe no-op: nothing latches, nothing waits, and
// deadline is left untouched.
//
// Extracted (rather than left as a few lines inlined at the one call site)
// so the composition itself — latch independent of permission — is
// directly unit-testable at the exact granularity the config-0 fix needs:
// config-0 + evidence still latches even though it never waits; config-0 +
// no evidence does neither.
func noteRefreshFailure(latch *resumeWaitLatch, deadline *waitDeadline, refreshErr error, signalFresh bool, mayResume func() bool, interruptionTimeout time.Duration, now time.Time) bool {
	if refreshErr == nil {
		return false
	}
	evidence := resumeEvidence(signalFresh, mayResume)
	if evidence {
		latch.wait()
	}
	return shouldWaitForResume(refreshErr, evidence, interruptionTimeout, deadline, now)
}

// resumeWaitLatch tracks runLiveStreamDownload's worker-level Tier 2
// evidence for one live-loop call — FINALIZE-scoped, not history-scoped:
// true only while the loop is CURRENTLY in (or gives up directly out of)
// an unresolved wait-for-resume, not "a wait fired at some point during
// this call."
//
// wait() latches it true — called by noteRefreshFailure whenever resume
// evidence holds on a failed refresh (regardless of whether
// shouldWaitForResume itself permitted an actual wait for it — I1 fix).
// resolved() clears it — called at every point a SUBSEQUENT
// refreshDownload SUCCEEDS (`result = refreshResult`), because a
// broadcast that actually resumed, or a transient failure that
// self-healed, must not permanently taint a clean multi-hour finish with
// incomplete_tail=true. A later unresolved wait calls wait() again on its
// own. value() reads the final state for runLiveStreamDownload's return —
// threaded into finalizeIncompleteTail by the caller.
//
// The zero value starts not-waiting (false), matching a call that never
// takes the wait branch at all.
type resumeWaitLatch struct{ waiting bool }

func (l *resumeWaitLatch) wait()       { l.waiting = true }
func (l *resumeWaitLatch) resolved()   { l.waiting = false }
func (l *resumeWaitLatch) value() bool { return l.waiting }

// isAuthWalledPlayability reports whether err is one of the playability
// errors that make YouTube withhold formats/streamingData from an
// UNAUTHENTICATED (cookieless) request even for an otherwise perfectly
// healthy live stream. Shared with runLiveStreamDownload's requiresAuthProbe
// predicate (orchestrator_youtube.go) — kept as one function so the two
// checks cannot drift apart.
func isAuthWalledPlayability(err youtube.PlayabilityError) bool {
	return err == youtube.PlayabilityMembersOnly ||
		err == youtube.PlayabilityAgeRestricted ||
		err == youtube.PlayabilityLoginRequired
}

// observeYouTubeStatusProbe is the shared decision behind every YouTube
// strategy's CheckStreamStatus closure (HLS, DASH, and manifestless-DASH
// all wire job.YT.ProbeVideoStatus into this exact shape). Split out from
// the closures themselves so the decision — observe the interruption
// signature, then report whether the stream ended — is unit-testable
// against an already-fetched probe result, without a network-capable
// *youtube.Service.
//
// This is the ONE observation site that actually fires WHILE the engine is
// still internally retrying/stalling on a segment-fetch failure (the
// engine throttles its own consult to ~30s — streamStatusCheckInterval,
// internal/engine/downloader.go); every other observe() call site in this
// package only runs after the downloader has already exited. See
// interruptionSignalStaleAfter's doc comment for the full cadence picture.
//
// One guard protects this from mis-arming the signal on a bad probe:
//
//   - info == nil: ProbeVideoStatus can return (nil, nil) in a shutdown
//     race (every Innertube client failing without surfacing a hard error —
//     the same defensive case buildYouTubeProbeFn already guards). Reported
//     as an error (not "not ended") so the engine's CheckStreamStatus
//     caller treats it as an inconclusive check (checkErr != nil: defers
//     the verdict, does not latch anything) rather than as a false
//     "confirmed still live" — which would itself trigger an unwarranted
//     ErrQualityLost in handleGoneError.
//
// A second guard — auth-walled PlayabilityError — used to live here too,
// since CheckStreamStatus ALWAYS probes via the COOKIELESS ANDROID_VR
// client (ProbeVideoStatus's own doc comment) regardless of whether the
// job itself is authenticated, and classifyStream
// (internal/youtube/player_api_parsing.go) derives StreamStatus purely
// from videoDetails.isLive — BEFORE any playability/formats check — so a
// HEALTHY members-only/age-restricted/login-required live stream reliably
// probes as StreamStatus=live with zero Formats here, bit-for-bit the
// interruption signature. That guard now lives centrally inside
// interruptionSignal.observe itself (I4 fix) — every call site, this one
// included, is covered by construction rather than by each site
// remembering to duplicate the check. isAuthWalledPlayability is the same
// predicate requiresAuthProbe uses to route these to the authenticated
// probe instead.
func observeYouTubeStatusProbe(job *JobContext, info *youtube.VideoInfo, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	if info == nil {
		return false, errNilStatusProbe
	}
	job.Interruption.observe(info)
	return info.StreamStatus != youtube.StreamLive, nil
}

// errNilStatusProbe is returned by observeYouTubeStatusProbe when
// ProbeVideoStatus returns (nil, nil) — see its doc comment.
var errNilStatusProbe = errors.New("status probe returned nil info without error")
