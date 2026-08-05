package worker

import (
	"context"
	"fmt"

	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// minEvictionHead is the HeadSeq threshold beyond which a zero-byte VOD
// finish becomes a plausible eviction rather than an ordinary failed start
// (dead cipher/POT, network outage, wrong itag — the whole zoo of reasons a
// download can end without a single byte written). At roughly 1s/segment,
// 100_000 segments is ~27.8h into the broadcast: comfortably below
// YouTube's ~120h retention window, so a real eviction is never missed by
// this floor, while ordinary failed-start jobs — whose HeadSeq() lands in
// the tens or low hundreds at most, since the head is learned from the
// first few fetch attempts — never reach it. The guard keeps every
// unrelated failure from paying for a bisection.
const minEvictionHead = 100_000

// diagnoseEvictedStart runs after a YouTube manifestless VOD download
// finishes having written zero bytes (see the minEvictionHead guard for why
// most zero-byte finishes never reach this point at all). It bisects for
// the true oldest segment the CDN still serves, fetches that boundary
// segment, runs engine.InspectSegment on it, logs the full diagnosis at
// Warn, and — only when eviction is CONFIRMED (the oldest available
// segment is > 0) — returns a descriptive error so the caller fails the
// job with a truthful message instead of the generic empty-download
// failure.
//
// Two outcomes deliberately return nil (no eviction error), leaving the
// caller's ordinary empty-download failure path to run unchanged:
//   - FindOldestAvailableSeq itself errors: the head segment is
//     unavailable, which per its doc comment means the URL is dead — a
//     completely different failure class than eviction.
//   - The bisection lands on 0: segment 0 is still available, so nothing
//     was evicted; whatever else stopped the download at zero bytes is
//     unrelated to retention.
func (o *DownloadOrchestrator) diagnoseEvictedStart(ctx context.Context, jobCtx *JobContext, videoInfo *youtube.VideoInfo, result *DownloadResult) error {
	if jobCtx.Job.Platform != "youtube" || result == nil || result.VideoDownloader == nil {
		return nil
	}
	if totalBytesWritten(result) != 0 {
		return nil
	}
	head := result.VideoDownloader.HeadSeq()
	if head <= minEvictionHead {
		return nil
	}

	videoDl := result.VideoDownloader
	probe := func(ctx context.Context, seq int) (bool, error) {
		avail, _, err := videoDl.ProbeSegmentAvailable(ctx, seq)
		return avail, err
	}
	oldest, err := engine.FindOldestAvailableSeq(ctx, head, probe)
	if err != nil {
		o.logger.Warn("eviction bisection aborted — URL problem, not eviction; leaving the empty-download failure path to run",
			"err", err, "jobID", jobCtx.Job.ID, "videoID", jobCtx.Job.VideoID, "head", head)
		return nil
	}
	if oldest == 0 {
		return nil
	}

	var inspection engine.SegmentInspection
	if _, body, probeErr := videoDl.ProbeSegmentAvailable(ctx, oldest); probeErr != nil {
		o.logger.Warn("eviction boundary segment re-fetch failed; diagnosing without segment inspection",
			"err", probeErr, "jobID", jobCtx.Job.ID, "oldestAvailableSeq", oldest)
	} else {
		inspection = engine.InspectSegment(body)
	}

	targetDuration, durationUnknown := selectedVideoTargetDuration(jobCtx, videoInfo)
	evictedHours := float64(oldest) * float64(targetDuration) / 3600

	o.logger.Warn("stream exceeds YouTube's retention window — segments evicted from the CDN",
		"jobID", jobCtx.Job.ID, "videoID", jobCtx.Job.VideoID,
		"head", head, "oldestAvailableSeq", oldest, "evictedSegments", oldest,
		"targetDurationSec", targetDuration, "durationUnknown", durationUnknown,
		"evictedHours", evictedHours,
		"hasFtyp", inspection.HasFtyp, "hasMoov", inspection.HasMoov, "hasSidx", inspection.HasSidx,
		"sidxTimescale", inspection.SidxTimescale, "firstMediaBox", inspection.FirstMediaBox,
		"boxes", inspection.Boxes, "spsPpsHeuristic", inspection.SPSPPSHeuristic)

	return evictionError(oldest, evictedHours)
}

// totalBytesWritten sums BytesWritten across whichever downloaders result
// holds (nil-safe per slot — audio-only/video-only jobs leave the other
// nil).
func totalBytesWritten(result *DownloadResult) int64 {
	var total int64
	if result.VideoDownloader != nil {
		total += result.VideoDownloader.BytesWritten()
	}
	if result.AudioDownloader != nil {
		total += result.AudioDownloader.BytesWritten()
	}
	return total
}

// selectedVideoTargetDuration looks up the per-segment duration YouTube
// advertised for the video format this job selected: an exact itag match
// against the job's SelectedVideoItag when set, falling back to any video
// format in the pool that carries the field. The fallback exists because
// the manifestless DASH strategy (the only path that reaches
// diagnoseEvictedStart) doesn't preserve the chosen itag on DownloadResult
// — every OTF video format in the same player-API response shares the same
// per-segment cadence in practice, so any one of them is a fine stand-in.
// Returns (1, true) when no format carries the field at all, so the hours-
// evicted math still produces a number instead of dividing by zero; the
// `unknown` flag marks that number as a rough guess for the log line.
func selectedVideoTargetDuration(jobCtx *JobContext, videoInfo *youtube.VideoInfo) (sec int, unknown bool) {
	if videoInfo == nil {
		return 1, true
	}
	if jobCtx.Job.SelectedVideoItag != nil {
		itag := *jobCtx.Job.SelectedVideoItag
		for i := range videoInfo.Formats {
			if f := &videoInfo.Formats[i]; f.Itag == itag && f.TargetDurationSec > 0 {
				return f.TargetDurationSec, false
			}
		}
	}
	for i := range videoInfo.Formats {
		if f := &videoInfo.Formats[i]; f.IsVideo() && f.TargetDurationSec > 0 {
			return f.TargetDurationSec, false
		}
	}
	return 1, true
}

// evictionError builds the truthful-failure message a confirmed eviction
// sets as the job's error. oldest is the first sequence the CDN still
// serves, so segments 0..oldest-1 are the evicted range. Phase D
// (docs/plans/2026-08-05-incomplete-tail-and-marathon-streams.md) is the
// gated future work that would let from-start archiving of a marathon
// stream survive this instead of failing outright.
func evictionError(oldest int, evictedHours float64) error {
	return fmt.Errorf(
		"stream exceeds YouTube's ~120h retention window: segments 0..%d are evicted (~%.0fh of the broadcast). "+
			"From-start archiving of marathon streams requires init recovery — see docs/plans/2026-08-05-incomplete-tail-and-marathon-streams.md Phase D",
		oldest-1, evictedHours)
}
