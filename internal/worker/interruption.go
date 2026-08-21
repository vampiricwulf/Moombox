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
func (s *interruptionSignal) observe(info *youtube.VideoInfo) {
	if s == nil || info == nil {
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
func shouldWaitForResume(refreshErr error, evidence bool, interruptionTimeout time.Duration) bool {
	if refreshErr == nil || interruptionTimeout <= 0 {
		return false
	}
	return evidence
}

// noteRefreshFailure is runLiveStreamDownload's full I1-fixed response to
// one failed live-refresh: it ALWAYS latches Tier-2 resume evidence onto
// latch (via latch.wait()) when resumeEvidence holds — regardless of
// whether the config permits an actual stall — and separately reports
// whether the loop should actually wait-and-retry (shouldWaitForResume's
// permission gate). A nil refreshErr is a safe no-op: nothing latches,
// nothing waits.
//
// Extracted (rather than left as three lines inlined at the one call site)
// so the composition itself — latch independent of permission — is
// directly unit-testable at the exact granularity the config-0 fix needs:
// config-0 + evidence still latches even though it never waits; config-0 +
// no evidence does neither.
func noteRefreshFailure(latch *resumeWaitLatch, refreshErr error, signalFresh bool, mayResume func() bool, interruptionTimeout time.Duration) bool {
	if refreshErr == nil {
		return false
	}
	evidence := resumeEvidence(signalFresh, mayResume)
	if evidence {
		latch.wait()
	}
	return shouldWaitForResume(refreshErr, evidence, interruptionTimeout)
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
// Two guards protect this from mis-arming the signal:
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
//   - info.PlayabilityError is members-only / age-restricted /
//     login-required: CheckStreamStatus ALWAYS probes via the COOKIELESS
//     ANDROID_VR client (ProbeVideoStatus's own doc comment), regardless of
//     whether the job itself is authenticated. For an auth-walled stream,
//     classifyStream (internal/youtube/player_api_parsing.go) derives
//     StreamStatus purely from videoDetails.isLive — BEFORE any
//     playability/formats check — so a HEALTHY members-only/age-restricted/
//     login-required live stream reliably probes as StreamStatus=live with
//     zero Formats (this unauthenticated request simply can't see them),
//     which is bit-for-bit the interruption signature. Observing it would
//     self-sustain a perpetually "fresh" signal — re-armed by the engine's
//     own ~30s stall-driven re-probe — for the ENTIRE run of any such
//     stream, silently converting every ordinary MaxTimeout finalize into
//     an up-to-InterruptionTimeout wrongful stall. isAuthWalledPlayability
//     is the same predicate requiresAuthProbe uses to route these to the
//     authenticated probe instead.
func observeYouTubeStatusProbe(job *JobContext, info *youtube.VideoInfo, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	if info == nil {
		return false, errNilStatusProbe
	}
	if !isAuthWalledPlayability(info.PlayabilityError) {
		job.Interruption.observe(info)
	}
	return info.StreamStatus != youtube.StreamLive, nil
}

// errNilStatusProbe is returned by observeYouTubeStatusProbe when
// ProbeVideoStatus returns (nil, nil) — see its doc comment.
var errNilStatusProbe = errors.New("status probe returned nil info without error")
