package worker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// Twitch clip jobs (owner decision D26 and its follow-up 1: "a normal job,
// Twitch VOD-style"). A clip is one whole MP4 Twitch renders once: no
// playlist, no live edge, no chat replay. Its job is a Twitch row like a
// VOD's — platform "twitch", is_vod, its own title, channel, thumbnail and
// published time (stream_start_time, the clip's createdAt) — told apart by
// its ID, "tw_clip_<slug>" (twitch.ClipJobIDPrefix), as a VOD is by "tw_v".
// It downloads the best rendition ShareClipRenderStatus lists
// (twitch.BestClipQuality) as a whole file through the engine's direct
// downloader, the path a YouTube VOD's whole-file formats take, and then runs
// the ordinary finalize (muxAndFinalize: stream copy, thumbnail, metadata,
// Finished). No chat is captured: a clip has none.

// IsTwitchClipJob reports whether job is a clip job (its video ID carries
// twitch.ClipJobIDPrefix).
func IsTwitchClipJob(job *database.Job) bool {
	if job == nil || job.Platform != "twitch" {
		return false
	}
	_, ok := twitch.ClipSlugFromJobID(job.VideoID)
	return ok
}

// RecordPastedClip enters a clip row an add path just created (POST
// /api/jobs, `moombox add`) in the processing history, as the monitor's
// OnClipFound does for the clips its sweep finds. The sweep of a channel
// whose archive_clips is on skips a clip whose job ID is in the history or
// is a job now (TwitchMonitor.sweepClips); a pasted clip was only the
// second, so once its Finished row was deleted the next sweep whose window
// still held it archived it again — the dedupe by slug that archives a clip
// once held for the monitor's clips only. A row of any other kind is left
// out of the history, as every other manual add is.
func RecordPastedClip(db *database.Database, job *database.Job) error {
	if !IsTwitchClipJob(job) {
		return nil
	}
	return db.AddToHistory(job.ID)
}

// TwitchClipDownload is what stream processing hands the clip download: the
// rendition to fetch, and how to read the clip again for a fresh signed URL
// when the CDN refuses the one it has (its playback access token expired
// while the job queued for a download slot, or mid-transfer).
type TwitchClipDownload struct {
	Slug    string
	Quality twitch.TwitchClipQuality
	// Reread is ShareClipRenderStatus again (StreamProcessor.getClip).
	Reread func(ctx context.Context, slug string) (*twitch.TwitchClipInfo, error)
}

// getClip is tw.GetClip, or clipFn when a test set one.
func (sp *StreamProcessor) getClip(ctx context.Context, slug string) (*twitch.TwitchClipInfo, error) {
	if sp.clipFn != nil {
		return sp.clipFn(ctx, slug)
	}
	return sp.tw.GetClip(ctx, slug)
}

// processTwitchClip reads the clip, records its metadata on the row and
// picks the rendition to download. A failure that never reached Twitch is
// returned as an error, as a VOD's is (twitchVodFetchError), so processJob
// waits an outage out and reads the clip again; an answer from Twitch is the
// clip's verdict. A re-hashed clip op is ClipAPIChangedError, whose text is
// the job's error as it stands: "Twitch changed its clip API
// (ShareClipRenderStatus); update Moombox" (owner decision D26).
func (sp *StreamProcessor) processTwitchClip(ctx context.Context, job *database.Job, slug string) (*StreamProcessResult, error) {
	clip, err := sp.getClip(ctx, slug)
	if err != nil {
		if err := twitchVodFetchError(ctx, "twitch clip error", err); err != nil {
			return nil, err
		}
		return &StreamProcessResult{
			ShouldDownload: false,
			IsVod:          true,
			Error:          twitchClipErrorText(err),
			ErrSentinel:    twitchAuthSentinel(err),
		}, nil
	}

	best := twitch.BestClipQuality(clip.Qualities)
	if best == nil {
		return &StreamProcessResult{ShouldDownload: false, IsVod: true, Error: "twitch clip error: the clip lists no downloadable rendition"}, nil
	}

	// No status, as for a VOD: the download-slot wait is still ahead
	// (processTwitchVod). The title is VOD-style, "<channel> — <title>".
	channel := cmp.Or(clip.BroadcasterDisplayName, clip.BroadcasterLogin, job.ChannelName)
	title := cmp.Or(strings.TrimSpace(clip.Title), slug)
	updates := map[string]any{
		"is_vod":         true,
		"title":          channel + " — " + title,
		"channel_name":   channel,
		"twitch_quality": twitchClipQualityLabel(best),
	}
	if clip.ThumbnailURL != "" {
		updates["thumbnail_url"] = clip.ThumbnailURL
	}
	if clip.DurationSeconds > 0 {
		updates["length_seconds"] = clip.DurationSeconds
	}
	if clip.CreatedAt != "" {
		// The clip's published time: the archive's ${start_date} and the
		// rows' sort, as a VOD's broadcast start is.
		updates["stream_start_time"] = clip.CreatedAt
	}
	if clip.GameCategory != "" {
		updates["twitch_category"] = clip.GameCategory
	}
	sp.db.UpdateJobFields(job.ID, updates)
	syncTwitchJobMetadata(job, updates)

	sp.logger.Info("twitch clip ready", "jobID", job.ID, "slug", slug, "quality", best.Quality,
		"fps", best.FrameRate, "portrait", best.Portrait, "renditions", len(clip.Qualities))
	return &StreamProcessResult{
		ShouldDownload: true,
		IsVod:          true,
		TwitchClip:     &TwitchClipDownload{Slug: slug, Quality: *best, Reread: sp.getClip},
	}, nil
}

// twitchClipErrorText is the job's error for a clip Twitch answered about: a
// re-hashed op's own message, verbatim; the rest prefixed as the VOD path
// prefixes its own.
func twitchClipErrorText(err error) string {
	var changed *twitch.ClipAPIChangedError
	if errors.As(err, &changed) {
		return changed.Error()
	}
	return fmt.Sprintf("twitch clip error: %v", err)
}

// twitchClipQualityLabel is the twitch_quality a clip records: "1080p60",
// "720p", "portrait 1920p30" — Twitch's quality label as a height, the frame
// rate when it is known, and the portrait crop named (yt-dlp's
// "portrait-<quality>" format ID).
func twitchClipQualityLabel(q *twitch.TwitchClipQuality) string {
	label := q.Quality + "p"
	if q.FrameRate > 0 {
		label += fmt.Sprintf("%.0f", q.FrameRate)
	}
	if q.Portrait {
		label = "portrait " + label
	}
	return label
}

// twitchClipStreamID is the resume identity of a clip download: the job and
// the rendition. A partial left by one rendition is never continued with
// another (resumeIdentityMismatch) — a refresh that came back with a
// different best rendition starts the file over.
func twitchClipStreamID(videoID string, q twitch.TwitchClipQuality) string {
	return fmt.Sprintf("%s/quality=%s/portrait=%t", videoID, q.Quality, q.Portrait)
}

// twitchClipURLRefresh answers a refused whole-file chunk (403/410: the
// playback access token the URL was signed with expired) by reading the clip
// again and returning the same rendition's freshly signed URL — the same
// quality label from the same asset, so the file being appended to stays one
// rendition. Nothing comes back when the clip is gone or no longer lists the
// rendition; the engine's refusal then stands and the job fails with it.
func twitchClipURLRefresh(ctx context.Context, clip *TwitchClipDownload, log logger) func() (string, string) {
	return func() (string, string) {
		if clip.Reread == nil {
			return "", ""
		}
		fresh, err := clip.Reread(ctx, clip.Slug)
		if err != nil {
			log.Warn("twitch clip URL refresh failed", "slug", clip.Slug, "err", err)
			return "", ""
		}
		for _, q := range fresh.Qualities {
			if q.Quality == clip.Quality.Quality && q.Portrait == clip.Quality.Portrait {
				return q.URL, ""
			}
		}
		log.Warn("twitch clip URL refresh: the rendition is gone from the clip", "slug", clip.Slug, "quality", clip.Quality.Quality)
		return "", ""
	}
}

// twitchClipDownloadResult builds the clip's download: one whole-file
// downloader writing staging/video.mp4, the MP4 carrying its own audio (as a
// YouTube progressive format's does: VideoPath alone, HasAudio set).
func (o *DownloadOrchestrator) twitchClipDownloadResult(ctx context.Context, jobCtx *JobContext, clip *TwitchClipDownload) *DownloadResult {
	path := filepath.Join(jobCtx.StagingDir, "video.mp4")
	result := &DownloadResult{
		HasVideo:    true,
		HasAudio:    true,
		VideoPath:   path,
		VideoHeight: clip.Quality.Height,
		VideoFps:    int(clip.Quality.FrameRate + 0.5),
	}
	result.VideoDownloader = newSegmentDownloader(engine.DownloaderOptions{
		BaseURL:             clip.Quality.URL,
		OutputFile:          path,
		IsDirectURL:         true,
		StreamID:            twitchClipStreamID(jobCtx.Job.VideoID, clip.Quality),
		SegmentWorkers:      jobCtx.Config.SegmentWorkers,
		IsOnline:            connIsOnline(o.conn),
		OnCredentialRefresh: twitchClipURLRefresh(ctx, clip, o.logger),
		Logger:              newScopedLogger(jobCtx.Logger, "jobID", jobCtx.Job.ID, "stream", "clip"),
	})
	return result
}

// ExecuteTwitchClip downloads a clip and finalizes it — ExecuteWithChat's VOD
// branch for the one shape a clip has: the status written on a row with no
// outcome only (writeUnlessOutcome), "Twitch Download Starting", the whole
// file into staging with progress, Muxing, the download slot released, then
// the ordinary finalize. A Cancel stops it as it stops a VOD (the listener
// below), staging kept for a Retry that resumes the partial.
func (o *DownloadOrchestrator) ExecuteTwitchClip(ctx context.Context, jobCtx *JobContext, clip *TwitchClipDownload) error {
	o.logger.Info("starting Twitch clip download", "jobID", jobCtx.Job.ID, "slug", clip.Slug, "quality", clip.Quality.Quality)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobID := jobCtx.Job.ID
	unsubscribe := o.db.OnJobUpdate(func(updatedJob *database.Job) {
		if updatedJob.ID == jobID && updatedJob.Status == database.StatusCancelled {
			o.logger.Info("cancel detected via DB listener", "jobID", jobID)
			cancel()
		}
	})
	defer unsubscribe()

	updates := map[string]any{"status": database.StatusDownloading}
	if jobCtx.Job.DownloadStartedAt == "" {
		updates["download_started_at"] = time.Now().UTC().Format(time.RFC3339)
	}
	if writeUnlessOutcome(o.db, jobID, updates) {
		return ErrCancelled
	}

	if o.notifier != nil {
		startFields := []notifications.Field{
			{Name: "Channel", Value: notifications.EscapeMarkdown(jobCtx.Job.ChannelName), Inline: true},
			{Name: "Quality", Value: twitchClipQualityLabel(&clip.Quality), Inline: true},
			{Name: "Type", Value: "Clip", Inline: true},
		}
		if jobCtx.Job.TwitchCategory != "" {
			startFields = append(startFields, notifications.Field{Name: "Category", Value: notifications.EscapeMarkdown(jobCtx.Job.TwitchCategory), Inline: true})
		}
		f := NotifyFacts(jobCtx.Job)
		o.notifier.Send("Twitch Download Starting",
			fmt.Sprintf("Beginning download: %s", notifications.EscapeMarkdown(jobCtx.Job.Title)),
			notifications.TypeDownload,
			startFields,
			notifications.SendOptions{
				URL:       f.URL,
				Thumbnail: f.ThumbnailURL,
				Event:     "downloading",
				Author:    notifyAuthor(f),
				Platform:  f.Platform,
				JobID:     f.ID,
			},
		)
	}

	if err := os.MkdirAll(jobCtx.StagingDir, 0o755); err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}

	result := o.twitchClipDownloadResult(ctx, jobCtx, clip)
	tracker := NewProgressTracker(o.db, jobID, o.logger, jobCtx.Config.ProgressInterval)
	defer tracker.Close()
	o.attachTrackerAndProgress(tracker, result)

	if err := o.runDownloaders(ctx, result); err != nil {
		if ctx.Err() != nil {
			// A Cancel (the listener) or shutdown: staging and the resume
			// sidecar stay for a Retry.
			return ctx.Err()
		}
		return fmt.Errorf("download: %w", err)
	}
	tracker.Close()
	if writeUnlessOutcome(o.db, jobID, map[string]any{
		"status":   database.StatusMuxing,
		"progress": "V:100%",
		"percent":  100.0,
	}) {
		return ErrCancelled
	}
	tracker.Finalize()
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Release the download slot before muxing — the mux is CPU-bound.
	if o.queue != nil {
		o.queue.ReleaseDownloadSlot(jobID)
	}
	if err := o.muxAndFinalize(ctx, jobCtx, result); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}
