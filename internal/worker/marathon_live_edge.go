package worker

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// marathonLiveEdgeAge is how long a broadcast may have been live when its
// capture starts for a from-start capture still to be tried. YouTube keeps a
// live stream's segments for about 120 hours: yt-dlp's MAX_DURATION, 432000 s
// (extractor/youtube/_video.py _live_adaptive_fragments, from adbc4ec4b
// #888). Past it sq=0 is gone — and with it the only ftyp+moov a manifest-free
// capture has (manifestlessSq0URL), so not even a part started deeper could
// be muxed. Two hours short of the window (owner decision 10) so a stream
// right at its edge, whose start the setup and the first fetches take a
// while to reach, is not tried from a sq=0 the CDN drops under it.
const marathonLiveEdgeAge = 118 * time.Hour

// marathonLiveEdge reports how long the job's broadcast has been live at now,
// and whether its capture records the live HLS playlist instead of from the
// start (audit G11, owner decisions 7 and 10): a live capture (not isVod) on
// a from-start path — manifest-free DASH or a DASH manifest, the two
// setupOrFallback's live arms name — of a broadcast live longer than
// marathonLiveEdgeAge.
//
// The age is read from the row's StreamStartTime, which for a live stream is
// liveBroadcastDetails.startTimestamp (extractScheduledStartTime;
// updateJobMetadata writes it onto the row when the stream is found live) —
// the live_start_time yt-dlp measures its window from (_video.py 4138,
// 4167-4168). A start that does not parse, none at all, or one in the future
// is no age, and the capture runs from the start as it always did.
//
// A part that already holds a DASH capture (dashCaptureStaged) is not a
// capture start: a restart resumed into it appends through its sidecar from
// the position it reached, not from sq=0, and needs no init it does not
// already hold, so it carries on in place. A restart resumed into a FRESH
// part dir at a DB seed has nothing to append to and needs sq=0's init
// (manifestlessSq0URL), which is gone — that one does go to HLS. Whatever a
// resumed capture still cannot fetch, the live loop's own eviction check
// sends to HLS (evictedStartToLiveEdge).
//
// Post-live marathons are not touched: a stream that has ended has no live
// HLS to fall to (yt-dlp skips HLS for post_live, _video.py 3620-3621), and
// starting it at the oldest kept segment instead of sq=0 is the gated Phase D
// (docs/plans/2026-08-05-incomplete-tail-and-marathon-streams.md).
func marathonLiveEdge(job *JobContext, strategy DownloadStrategy, isVod bool, now time.Time) (time.Duration, bool) {
	if isVod || job == nil || job.Job == nil || strategy == nil {
		return 0, false
	}
	if k := strategy.Kind(); k != ManifestlessDashStrategy.Kind() && k != DashStrategy.Kind() {
		return 0, false
	}
	start := parseRFC3339(job.Job.StreamStartTime)
	if start.IsZero() {
		return 0, false
	}
	age := now.Sub(start)
	if age <= marathonLiveEdgeAge || dashCaptureStaged(job.StagingDir) {
		return age, false
	}
	return age, true
}

// dashCaptureStaged reports whether dir holds a non-empty DASH-family
// recording — video_stream or audio_stream, the names every DASH capture
// stages (liveShapeStagingNames).
func dashCaptureStaged(dir string) bool {
	if dir == "" {
		return false
	}
	for _, name := range []string{"video_stream", "audio_stream"} {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && !info.IsDir() && info.Size() > 0 {
			return true
		}
	}
	return false
}

// setupMarathonOr is setupWithFallback's choice of first setup: the live HLS
// playlist for a broadcast too old to record from the start
// (marathonLiveEdge), and setupOrFallback's otherwise. That is yt-dlp's own
// answer for a stream past its window: it warns and records what the CDN
// still keeps (_video.py 2012-2018), and its default for any live stream is
// the HLS playlist from the live edge (_video.py 3617-3627, 3714-3715) —
// Moombox's HLS capture starts at the oldest segment the playlist's window
// holds (StartSeq -1), which is the live edge's neighbourhood, not the
// broadcast's start. The job log says why from-start was dropped (owner
// decision 10), with the age it was read from.
//
// An HLS setup that fails — no master anywhere, the cookieless chain's
// included — leaves the from-start setup to run as it always did, its own
// fallbacks with it: the age could be wrong (a start the row was given by
// the worker's "now" fallback is not, but a stale one could be), and a
// from-start capture that then finds nothing reaches the live loop's
// eviction check, which tries HLS again.
func setupMarathonOr(ctx context.Context, job *JobContext, strategy DownloadStrategy, isVod bool, info *youtube.VideoInfo, deps *StrategyDeps) (*DownloadResult, error) {
	age, marathon := marathonLiveEdge(job, strategy, isVod, time.Now())
	if !marathon {
		return setupOrFallback(ctx, job, strategy, isVod, info, deps)
	}
	job.Logger.Warn("[Live] the stream has been live longer than YouTube keeps its segments (~120 h) — recording from the start cannot run; recording the live HLS playlist instead, from the live edge",
		"jobID", job.Job.ID, "strategy", strategy.Kind(), "liveFor", age.Round(time.Minute).String(),
		"streamStartTime", job.Job.StreamStartTime)
	result, err := HlsStrategy.Download(ctx, job, info, deps)
	if err == nil || ctx.Err() != nil {
		return result, err
	}
	job.Logger.Warn("[Live] the live HLS setup failed — trying the from-start capture anyway",
		"jobID", job.Job.ID, "err", err)
	return setupOrFallback(ctx, job, strategy, isVod, info, deps)
}
