package worker

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

const (
	progressUpdateInterval  = 16 * time.Millisecond // ~60fps, matches TUI tick rate
	progressPersistInterval = 1 * time.Second
	activityUpdateInterval  = 1 * time.Second // throttle activity progress-line writes
	activitySegmentGrace    = 2 * time.Second // show a wait only after this gap with no segment
	// waitingForSegmentGrace suppresses the "Waiting for next segment" line
	// until the quiet gap outgrows a normal segment interval. YouTube's live
	// segments are ~1s, but normal-latency streams publish ~5s segments, so a
	// gap of a few seconds is the healthy steady state at the live edge; showing
	// the wait during it flickers the line every cycle. 5s clears typical
	// cadences while still surfacing a genuine stall ~25s before the 30s
	// "verifying end" escalation. Applies only to ActivityWaitingForSegment —
	// other waits keep the 2s activitySegmentGrace.
	waitingForSegmentGrace = 5 * time.Second
	// activityTickerIdleStop is how many consecutive quiet refresh ticks
	// (nothing to show) the refresh goroutine tolerates before parking
	// itself. Comfortably outlives activitySegmentGrace so a wait recorded
	// during the grace window still gets its first write once the grace
	// passes; any later setActivity restarts the loop.
	activityTickerIdleStop = 10
)

// ProgressTracker tracks download progress and updates the database.
type ProgressTracker struct {
	mu             sync.Mutex
	db             *database.Database
	logger         logger
	jobID          string
	videoSeq       int
	audioSeq       int
	videoReported  bool // a video downloader DELIVERED a progress event
	audioReported  bool // an audio downloader DELIVERED a progress event
	videoTotal     int
	audioTotal     int
	chatCount      int
	bytesTotal     int64
	speedSmooth    *utils.SmoothValue
	lastUpdate     time.Time
	lastPersist    time.Time
	lastVideoBytes int64 // last p.Bytes for video downloader delta accumulation
	lastAudioBytes int64 // last p.Bytes for audio downloader delta accumulation
	speedLastBytes int64 // last bytesTotal snapshot for speed calculation
	lastBytesTime  time.Time

	// Arrival signal (engine OnFetch): media bytes read off the network,
	// including catch-up fetches still sitting in the reorder buffer. The
	// speed readout and wait suppression key on ARRIVAL — the flush-driven
	// counters (bytesTotal, lastSegmentAt) go quiet for a whole clump while
	// a wide worker pool is saturating the connection, which used to render
	// a busy download as "Waiting..." with a blank speed.
	fetchedTotal     int64     // cumulative fetched bytes across both streams
	speedLastFetched int64     // last fetchedTotal snapshot for speed calculation
	lastFetchAt      time.Time // when the most recent fetch landed
	startTime        time.Time // B8: for ETA calculation
	vodPercent       float64   // VOD download progress percentage (from chunked download)
	vodTotalBytes    int64     // Total file size for VOD chunked download (0 if not VOD)
	gaps             []database.Gap

	// Per-stream wait activity. Video and audio downloaders share this tracker
	// but can stall independently, so each keeps its own reason + start; the
	// displayed wait is the longest-running one, shown only when no segment has
	// arrived recently (so a stream that keeps delivering keeps the counter on
	// screen instead of flickering against the other stream's wait message).
	// The orch slot carries orchestrator-level waits (stream-end verification
	// between manifest refreshes, a Twitch connectivity-outage pause) that
	// happen while no downloader is running — set via SetWaitActivity and
	// cleared, like the stream slots, by the next delivered segment.
	videoActivity      engine.DownloadActivity
	videoActivityStart time.Time
	audioActivity      engine.DownloadActivity
	audioActivityStart time.Time
	orchActivity       engine.DownloadActivity
	orchActivityStart  time.Time
	lastSegmentAt      time.Time // last time either stream delivered a segment
	lastActivityWrite  time.Time // throttle for activity DB writes
	activityTickerOn   bool      // refresh goroutine running (guarded by mu)
	closed             bool      // Finalize ran — no further activity writes or tickers
}

// streamKind identifies which downloader an activity/progress event came from.
// streamOrch is the orchestrator itself (waits between downloader sessions).
type streamKind int

const (
	streamVideo streamKind = iota
	streamAudio
	streamOrch
)

// NewProgressTracker creates a new progress tracker for a job.
func NewProgressTracker(db *database.Database, jobID string, logger logger) *ProgressTracker {
	now := time.Now()
	return &ProgressTracker{
		db:            db,
		logger:        logger,
		jobID:         jobID,
		speedSmooth:   utils.NewSmoothValue(0.7),
		lastUpdate:    now,
		lastPersist:   now,
		lastBytesTime: now,
		startTime:     now,
	}
}

// AttachVideoDownloader attaches progress callbacks to a video segment downloader.
func (pt *ProgressTracker) AttachVideoDownloader(dl *engine.SegmentDownloader) {
	dl.OnProgress = func(p engine.DownloadProgress) {
		pt.mu.Lock()
		pt.videoActivity = engine.ActivityNone // a real video segment arrived
		pt.orchActivity = engine.ActivityNone  // any orchestrator-level wait is over
		pt.lastSegmentAt = time.Now()
		// Reported on DELIVERY, not at attach: between attach and the first
		// video event (a post-restart 403 hunt can hold this window open for
		// minutes), a chat tick or audio event must not persist videoSeq=0
		// over the prior session's continuation position.
		pt.videoReported = true
		pt.videoSeq = p.Seq
		if p.HeadSeq > 0 {
			pt.videoTotal = p.HeadSeq
		}
		if p.Total > 0 {
			pt.videoTotal = p.Total
		}
		// Total must never be smaller than the last downloaded segment
		if pt.videoTotal > 0 && pt.videoTotal < pt.videoSeq {
			pt.videoTotal = pt.videoSeq
		}
		// Track VOD chunked download progress
		if p.TotalBytes > 0 {
			pt.vodTotalBytes = p.TotalBytes
		}
		if p.Percent > 0 {
			pt.vodPercent = p.Percent
		}
		// p.Bytes is per-downloader cumulative. When a fresh downloader is
		// attached after a quality split (new file, counter restarts near 0)
		// the delta against the previous downloader's total would be hugely
		// negative — re-baseline instead. Same-file continuations are fine:
		// the engine seeds the new downloader's counter with the file size.
		if p.Bytes < pt.lastVideoBytes {
			pt.lastVideoBytes = p.Bytes
		}
		pt.bytesTotal += p.Bytes - pt.lastVideoBytes
		pt.lastVideoBytes = p.Bytes
		pt.mu.Unlock()
		pt.maybeUpdate()
	}

	dl.OnGap = func(g engine.DownloadGap) {
		pt.logger.Warn(fmt.Sprintf("[DownloadOrchestrator] video segment gap: seq %d–%d", g.From, g.To))
		pt.mu.Lock()
		pt.gaps = append(pt.gaps, database.Gap{
			JobID:  pt.jobID,
			From:   g.From,
			To:     g.To,
			Stream: "video",
		})
		pt.mu.Unlock()
	}

	dl.OnActivity = func(a engine.DownloadActivity) { pt.setActivity(streamVideo, a) }
	dl.OnFetch = pt.noteFetch
}

// AttachAudioDownloader attaches progress callbacks to an audio segment downloader.
func (pt *ProgressTracker) AttachAudioDownloader(dl *engine.SegmentDownloader) {
	dl.OnProgress = func(p engine.DownloadProgress) {
		pt.mu.Lock()
		pt.audioActivity = engine.ActivityNone // a real audio segment arrived
		pt.orchActivity = engine.ActivityNone  // any orchestrator-level wait is over
		pt.lastSegmentAt = time.Now()
		// See AttachVideoDownloader — reported on delivery, not attach.
		pt.audioReported = true
		pt.audioSeq = p.Seq
		if p.HeadSeq > 0 {
			pt.audioTotal = p.HeadSeq
		}
		if p.Total > 0 {
			pt.audioTotal = p.Total
		}
		// Total must never be smaller than the last downloaded segment
		if pt.audioTotal > 0 && pt.audioTotal < pt.audioSeq {
			pt.audioTotal = pt.audioSeq
		}
		// Re-baseline on downloader replacement — see AttachVideoDownloader.
		if p.Bytes < pt.lastAudioBytes {
			pt.lastAudioBytes = p.Bytes
		}
		pt.bytesTotal += p.Bytes - pt.lastAudioBytes
		pt.lastAudioBytes = p.Bytes
		pt.mu.Unlock()
		pt.maybeUpdate()
	}

	dl.OnGap = func(g engine.DownloadGap) {
		pt.logger.Warn(fmt.Sprintf("[DownloadOrchestrator] audio segment gap: seq %d–%d", g.From, g.To))
		pt.mu.Lock()
		pt.gaps = append(pt.gaps, database.Gap{
			JobID:  pt.jobID,
			From:   g.From,
			To:     g.To,
			Stream: "audio",
		})
		pt.mu.Unlock()
	}

	dl.OnActivity = func(a engine.DownloadActivity) { pt.setActivity(streamAudio, a) }
	dl.OnFetch = pt.noteFetch
}

// noteFetch accumulates the engine's arrival signal (see the fetchedTotal
// field docs). Called concurrently from catch-up worker goroutines — the
// engine documents OnFetch that way — so all state moves under pt.mu.
func (pt *ProgressTracker) noteFetch(n int64) {
	pt.mu.Lock()
	if pt.closed {
		pt.mu.Unlock()
		return
	}
	pt.fetchedTotal += n
	pt.lastFetchAt = time.Now()
	pt.mu.Unlock()
	pt.maybeUpdate()
}

// SetChatCount updates the chat message count.
func (pt *ProgressTracker) SetChatCount(count int) {
	pt.mu.Lock()
	pt.chatCount = count
	pt.mu.Unlock()
	pt.maybeUpdate()
}

// SetWaitActivity records an orchestrator-level wait (stream-end verification
// between manifest refreshes, a Twitch connectivity-outage pause) in the
// progress line. Unlike the per-stream activities — which the engine emits
// from inside its download loops — these waits happen while no downloader is
// running, so the orchestrator sets them directly; the refresh loop keeps the
// elapsed counter live for the whole wait, and the next delivered segment
// clears it like any other activity.
func (pt *ProgressTracker) SetWaitActivity(a engine.DownloadActivity) {
	pt.setActivity(streamOrch, a)
}

// setActivity records a wait reason for one stream (or the orchestrator) and,
// when no segment has arrived recently across either stream, surfaces the
// longest-running wait in the progress line (blanking speed/eta). Tracking per
// stream keeps one stream's delivered segment from resetting the other stalled
// stream's elapsed clock. DB writes are throttled.
func (pt *ProgressTracker) setActivity(stream streamKind, a engine.DownloadActivity) {
	pt.mu.Lock()
	if pt.closed {
		pt.mu.Unlock()
		return
	}
	now := time.Now()
	switch stream {
	case streamVideo:
		if a != pt.videoActivity {
			pt.videoActivity = a
			pt.videoActivityStart = now
		}
	case streamAudio:
		if a != pt.audioActivity {
			pt.audioActivity = a
			pt.audioActivityStart = now
		}
	case streamOrch:
		if a != pt.orchActivity {
			pt.orchActivity = a
			pt.orchActivityStart = now
		}
	}
	if a != engine.ActivityNone {
		// The refresh loop keeps the elapsed counter live when the engine
		// blocks without re-emitting (waitForConnectivity can hold one
		// emission for an entire outage) and retries the write below if the
		// segment grace suppresses it — without the loop, a wait whose only
		// emission landed inside the grace window never appeared at all.
		pt.ensureActivityTickerLocked()
	}
	act, start := pt.dominantActivity(now)
	if act == engine.ActivityNone {
		pt.mu.Unlock()
		return // a stream delivered recently — the normal counter is current
	}
	if now.Sub(pt.lastActivityWrite) < activityUpdateInterval {
		pt.mu.Unlock()
		return
	}
	pt.lastActivityWrite = now
	msg := activityMessage(act, pt.activityElapsedLocked(act, start, now))
	pt.mu.Unlock()

	pt.db.UpdateJobFields(pt.jobID, map[string]any{
		"progress": msg,
		"speed":    "",
		"eta":      "",
	})
}

// dominantActivity returns the wait reason + start to display, or ActivityNone
// when a segment arrived within activitySegmentGrace (the normal counter should
// show). When several slots wait, the longest-running wait wins so the elapsed
// reflects the true stall, not the most recent slot to stall. Caller holds mu.
func (pt *ProgressTracker) dominantActivity(now time.Time) (engine.DownloadActivity, time.Time) {
	last := pt.lastDeliveryLocked()
	haveSeg := !last.IsZero()
	sinceSeg := now.Sub(last)
	if haveSeg && sinceSeg < activitySegmentGrace {
		return engine.ActivityNone, time.Time{}
	}
	act, start := engine.ActivityNone, time.Time{}
	for _, s := range [...]struct {
		a engine.DownloadActivity
		t time.Time
	}{
		{pt.videoActivity, pt.videoActivityStart},
		{pt.audioActivity, pt.audioActivityStart},
		{pt.orchActivity, pt.orchActivityStart},
	} {
		if s.a == engine.ActivityNone {
			continue
		}
		// "Waiting for next segment" is the normal live-edge gap; hold it back
		// until the quiet stretch outgrows a typical segment interval so a
		// healthy multi-second-segment stream doesn't flicker the wait line.
		if s.a == engine.ActivityWaitingForSegment && haveSeg && sinceSeg < waitingForSegmentGrace {
			continue
		}
		if act == engine.ActivityNone || s.t.Before(start) {
			act, start = s.a, s.t
		}
	}
	return act, start
}

// activityElapsedLocked picks the elapsed duration shown for an activity.
// VerifyingEnd and WaitingForSegment show time since the last delivered segment
// — the number that actually says how long the stream has been quiet (and, for
// VerifyingEnd, the same clock the orchestrator's verification loop reasons
// with) — while the other waits show how long the wait itself has been running.
// Caller holds mu.
func (pt *ProgressTracker) activityElapsedLocked(act engine.DownloadActivity, start, now time.Time) time.Duration {
	if act == engine.ActivityVerifyingEnd || act == engine.ActivityWaitingForSegment {
		if last := pt.lastDeliveryLocked(); !last.IsZero() {
			return now.Sub(last)
		}
	}
	return now.Sub(start)
}

// lastDeliveryLocked is the most recent time stream data ARRIVED — a flushed
// segment (lastSegmentAt) or a fetched-but-not-yet-flushed one (lastFetchAt).
// Wait display keys on arrival: during a wide catch-up clump the flush stamp
// goes quiet for many seconds while the network is saturated, and one
// worker's transient-retry activity surfacing there read as a frozen job.
// Caller holds mu.
func (pt *ProgressTracker) lastDeliveryLocked() time.Time {
	if pt.lastFetchAt.After(pt.lastSegmentAt) {
		return pt.lastFetchAt
	}
	return pt.lastSegmentAt
}

// pendingActivityLocked reports whether any slot holds a wait. Caller holds mu.
func (pt *ProgressTracker) pendingActivityLocked() bool {
	return pt.videoActivity != engine.ActivityNone ||
		pt.audioActivity != engine.ActivityNone ||
		pt.orchActivity != engine.ActivityNone
}

// ensureActivityTickerLocked starts the refresh goroutine when none is
// running. Caller holds mu.
func (pt *ProgressTracker) ensureActivityTickerLocked() {
	if pt.activityTickerOn || pt.closed {
		return
	}
	pt.activityTickerOn = true
	go pt.activityRefreshLoop()
}

// activityRefreshLoop re-renders the active wait message once per second so
// the elapsed counter stays live through long blocking waits — the emit
// points only fire when the engine reaches a retry/check seam, which can be
// a 60s rate-limit sleep, a 5-minute orchestrator verify sleep, or a
// waitForConnectivity block spanning an entire outage. The goroutine is
// lazy: it parks itself after activityTickerIdleStop quiet ticks, so a
// normally-downloading job carries no ticker at all; the next setActivity
// restarts it.
func (pt *ProgressTracker) activityRefreshLoop() {
	defer func() {
		if r := recover(); r != nil {
			pt.logger.Error("panic in activity refresh loop", "panic", fmt.Sprint(r))
			pt.mu.Lock()
			pt.activityTickerOn = false
			pt.mu.Unlock()
		}
	}()
	ticker := time.NewTicker(activityUpdateInterval)
	defer ticker.Stop()
	idleTicks := 0
	for range ticker.C {
		pt.mu.Lock()
		if pt.closed {
			pt.activityTickerOn = false
			pt.mu.Unlock()
			return
		}
		now := time.Now()
		act, start := engine.ActivityNone, time.Time{}
		if pt.pendingActivityLocked() {
			act, start = pt.dominantActivity(now)
		}
		if act == engine.ActivityNone {
			// Nothing to show: no wait pending, or segments are arriving
			// (grace). Park after a quiet stretch rather than immediately —
			// a wait recorded during the grace window still needs its first
			// write once the grace passes.
			idleTicks++
			if idleTicks >= activityTickerIdleStop {
				pt.activityTickerOn = false
				pt.mu.Unlock()
				return
			}
			pt.mu.Unlock()
			continue
		}
		idleTicks = 0
		// Throttle against engine-emission writes; the 9/10 slack keeps the
		// ~1s ticker from beating against its own last write and skipping
		// alternate ticks.
		if now.Sub(pt.lastActivityWrite) < activityUpdateInterval*9/10 {
			pt.mu.Unlock()
			continue
		}
		pt.lastActivityWrite = now
		msg := activityMessage(act, pt.activityElapsedLocked(act, start, now))
		pt.mu.Unlock()

		pt.db.UpdateJobFields(pt.jobID, map[string]any{
			"progress": msg,
			"speed":    "",
			"eta":      "",
		})
	}
}

func (pt *ProgressTracker) maybeUpdate() {
	pt.mu.Lock()

	now := time.Now()
	if now.Sub(pt.lastUpdate) < progressUpdateInterval {
		pt.mu.Unlock()
		return
	}
	pt.lastUpdate = now

	// Calculate instantaneous speed (bytes delta / time delta, matching TS).
	// Source is the ARRIVAL counter (fetchedTotal) once any fetch has been
	// reported: the flush counter (bytesTotal) sawtooths with catch-up's
	// ordered-flush clumps — zero for seconds, then a whole clump at once —
	// while arrival tracks the network. The bytesTotal fallback keeps a
	// non-zero readout for any path that never reports fetches. A side
	// benefit: resume-seeded file bytes inflate bytesTotal's first delta but
	// never the arrival counter.
	elapsed := now.Sub(pt.lastBytesTime).Seconds()
	if elapsed > 0 {
		src, last := pt.bytesTotal, pt.speedLastBytes
		if pt.fetchedTotal > 0 {
			src, last = pt.fetchedTotal, pt.speedLastFetched
		}
		speed := float64(src-last) / elapsed
		pt.speedSmooth.Update(speed)
		pt.speedLastBytes = pt.bytesTotal
		pt.speedLastFetched = pt.fetchedTotal
		pt.lastBytesTime = now
	}

	// Build progress string (A3: includes chat count like "V:1234 A:1234 C:5678")
	progress := pt.buildProgressString()

	// Calculate percent
	percent := 0.0
	if pt.vodTotalBytes > 0 {
		percent = pt.vodPercent
	} else if pt.videoTotal > 0 {
		percent = float64(pt.videoSeq) / float64(pt.videoTotal) * 100
	}

	// B8: Calculate ETA
	eta := pt.calculateETA()

	// While a wait activity is showing, this path (chat ticks are its main
	// trigger during a stall) must not stamp the frozen segment counter and
	// stale speed over the activity message — render the wait here too, so
	// the two writers agree instead of alternating once a second. The seq
	// columns and chat count below still persist normally.
	speed := utils.FormatSpeed(pt.speedSmooth.Value())
	activityShown := false
	if act, start := pt.dominantActivity(now); act != engine.ActivityNone {
		progress = activityMessage(act, pt.activityElapsedLocked(act, start, now))
		speed = ""
		eta = ""
		activityShown = true
		pt.lastActivityWrite = now // spare the refresh loop a duplicate write
	}

	// Snapshot values for DB update. The seq columns are persisted only for
	// stream kinds that have actually REPORTED this session: an audio-less
	// session (HLS delivers muxed A+V) used to overwrite last_audio_seq
	// with a constant 0 — and any session's first chat/audio tick used to
	// do the same to last_video_seq before video's first event — destroying
	// the prior session's continuation position that restart seeding needs.
	updates := map[string]any{
		"progress":            progress,
		"percent":             percent,
		"speed":               speed,
		"total_chat_messages": pt.chatCount,
	}
	if pt.videoReported {
		updates["last_video_seq"] = pt.videoSeq
		updates["total_video_seq"] = pt.videoTotal
	}
	if pt.audioReported {
		updates["last_audio_seq"] = pt.audioSeq
		updates["total_audio_seq"] = pt.audioTotal
	}
	if eta != "" {
		updates["eta"] = eta
	} else if activityShown {
		// Blank a lingering ETA explicitly — the activity writers do the
		// same, and skipping the key here would leave the stale value.
		updates["eta"] = ""
	}

	// Snapshot gaps for persistence
	var gapsToSave []database.Gap
	shouldPersist := now.Sub(pt.lastPersist) >= progressPersistInterval
	if shouldPersist {
		pt.lastPersist = now
		gapsToSave = make([]database.Gap, len(pt.gaps))
		copy(gapsToSave, pt.gaps)
		pt.gaps = nil
	}

	pt.mu.Unlock()

	// DB operations outside the lock to reduce contention
	pt.db.UpdateJobFields(pt.jobID, updates)

	for _, gap := range gapsToSave {
		pt.db.AddGap(gap.JobID, gap.From, gap.To, gap.Stream)
	}
}

// buildProgressString builds the progress display string.
// Format matches TypeScript: "(A: X V: Y C: Z)" for DASH, "Seq: X" for HLS, "V:95.3%" for VOD.
func (pt *ProgressTracker) buildProgressString() string {
	// VOD chunked download: show percentage instead of segment counts
	if pt.vodTotalBytes > 0 {
		s := fmt.Sprintf("V:%.1f%%", pt.vodPercent)
		if pt.chatCount > 0 {
			s += fmt.Sprintf(" C: %d", pt.chatCount)
		}
		return s
	}

	if pt.audioTotal > 0 || pt.audioSeq > 0 {
		vPart := strconv.Itoa(pt.videoSeq)
		if pt.videoTotal > 0 {
			vPart = strconv.Itoa(pt.videoSeq) + "/" + strconv.Itoa(pt.videoTotal)
		}
		aPart := strconv.Itoa(pt.audioSeq)
		if pt.audioTotal > 0 {
			aPart = strconv.Itoa(pt.audioSeq) + "/" + strconv.Itoa(pt.audioTotal)
		}
		s := fmt.Sprintf("(A: %s V: %s", aPart, vPart)
		if pt.chatCount > 0 {
			s += fmt.Sprintf(" C: %d", pt.chatCount)
		}
		return s + ")"
	}
	if pt.chatCount > 0 {
		return fmt.Sprintf("Seq: %d C: %d", pt.videoSeq, pt.chatCount)
	}
	return fmt.Sprintf("Seq: %d", pt.videoSeq)
}

// activityMessage renders a downloader wait reason into the progress-line text.
// ASCII punctuation matches the codebase's existing "..." progress strings.
func activityMessage(a engine.DownloadActivity, elapsed time.Duration) string {
	e := utils.FormatDurationHuman(elapsed)
	switch a {
	case engine.ActivityVerifyingEnd:
		return fmt.Sprintf("Verifying stream ended... (%s)", e)
	case engine.ActivityReconnecting:
		return fmt.Sprintf("Connection lost - reconnecting... (%s)", e)
	case engine.ActivityRateLimited:
		return fmt.Sprintf("Rate-limited - backing off... (%s)", e)
	case engine.ActivityFindingFirstSegment:
		return fmt.Sprintf("Waiting for first segment... (%s)", e)
	case engine.ActivityWaitingForSegment:
		return fmt.Sprintf("Waiting for next segment... (%s)", e)
	case engine.ActivityRetrying:
		return fmt.Sprintf("Segment fetch failing - retrying... (%s)", e)
	default:
		return ""
	}
}

// calculateETA estimates time remaining based on segment or byte progress (B8).
func (pt *ProgressTracker) calculateETA() string {
	elapsed := time.Since(pt.startTime).Seconds()
	if elapsed < 5 {
		return "" // Too early for meaningful estimate
	}

	var remaining float64

	if pt.vodTotalBytes > 0 && pt.bytesTotal > 0 {
		// VOD chunked download: bytes-based ETA
		bytesPerSec := float64(pt.bytesTotal) / elapsed
		if bytesPerSec <= 0 {
			return ""
		}
		remaining = float64(pt.vodTotalBytes-pt.bytesTotal) / bytesPerSec
	} else if pt.videoTotal > 0 && pt.videoSeq > 0 {
		// Segment-based ETA
		segsPerSec := float64(pt.videoSeq) / elapsed
		if segsPerSec <= 0 {
			return ""
		}
		remaining = float64(pt.videoTotal-pt.videoSeq) / segsPerSec
	} else {
		return ""
	}

	if remaining <= 0 {
		return ""
	}

	d := time.Duration(remaining) * time.Second
	if d > 24*time.Hour {
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
	if d > time.Hour {
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	if d > time.Minute {
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

// Close stops activity writes and the refresh loop without flushing gap
// state. The orchestrators defer it right after constructing the tracker so
// no exit path — error, cancel, panic — leaves the refresh goroutine alive
// rewriting a terminal job's progress line every second. Idempotent;
// Finalize implies it.
func (pt *ProgressTracker) Close() {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	pt.closeLocked()
}

// closeLocked marks the tracker closed and clears the wait slots so neither
// the refresh loop nor maybeUpdate can render an activity again. Caller
// holds mu.
func (pt *ProgressTracker) closeLocked() {
	pt.closed = true
	pt.videoActivity = engine.ActivityNone
	pt.audioActivity = engine.ActivityNone
	pt.orchActivity = engine.ActivityNone
}

// Finalize saves any remaining state and stops the activity refresh loop —
// after this, no activity write may land over the finalize-phase progress
// (Muxing status, mux percent lines).
func (pt *ProgressTracker) Finalize() {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	pt.closeLocked()

	for _, gap := range pt.gaps {
		pt.db.AddGap(gap.JobID, gap.From, gap.To, gap.Stream)
	}
	pt.gaps = nil
}
