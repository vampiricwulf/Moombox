package worker

import (
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

// attachMayResume installs mayResume onto dl as its engine MayResume
// callback — UNLESS interruptionTimeout <= 0, in which case dl.MayResume is
// left nil. This is the config-contract gate: interruption_timeout=0 means
// "finalize must never wait" (config/types.go's doc comment), but the
// engine's own semantics are "InterruptionTimeout=0 means no CEILING" (an
// unbounded stall) and "nil MayResume means no stall at all" — two
// different zero values with opposite meanings. Passing a non-nil
// MayResume through with a zero ceiling would silently invert the config
// contract into an unbounded stall for any job whose chat is still open
// (or whose interruption signal is fresh) — the exact "opted out but
// stalls forever" bug this gate exists to prevent. dl == nil is a safe
// no-op (VOD downloaders / a strategy that didn't build one).
func attachMayResume(dl *engine.SegmentDownloader, mayResume func() bool, interruptionTimeout time.Duration) {
	if dl == nil || interruptionTimeout <= 0 {
		return
	}
	dl.MayResume = mayResume
}

// shouldWaitForResume decides whether a failed live-refresh (refreshErr)
// should wait-and-retry instead of ending the recording immediately.
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
// "finalize must never wait" per the config contract — same gate
// attachMayResume enforces, so a refresh failure falls straight back to
// the old immediate-return behavior) and on some resume evidence: either
// this exact refresh's player-response fetch just showed the interruption
// signature (signalFresh), or the job's chat continuation says the
// broadcast may still resume (mayResume). A nil refreshErr never waits —
// there is nothing to retry.
func shouldWaitForResume(refreshErr error, signalFresh bool, mayResume func() bool, interruptionTimeout time.Duration) bool {
	if refreshErr == nil || interruptionTimeout <= 0 {
		return false
	}
	if signalFresh {
		return true
	}
	return mayResume != nil && mayResume()
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
func observeYouTubeStatusProbe(job *JobContext, info *youtube.VideoInfo, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	job.Interruption.observe(info)
	return info.StreamStatus != youtube.StreamLive, nil
}
