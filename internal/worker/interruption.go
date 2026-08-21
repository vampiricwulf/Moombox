package worker

import (
	"sync/atomic"
	"time"

	"github.com/vampiricwulf/Moombox/internal/chat"
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

// interruptionSignalStaleAfter bounds trust in a signature the refresh path
// stopped re-confirming — the observation sites (refreshGvsCredentials'
// player-response re-fetch, and the live loop's own GetVideoInfo calls on
// the ErrQualityLost / stream-stopped paths) re-confirm on a cadence of
// roughly 20-30s under a stall, well inside this window.
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
