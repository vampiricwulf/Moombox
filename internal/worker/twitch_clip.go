package worker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/jobfilter"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// Twitch clip jobs (owner decision D26 and its follow-up 1: "a normal job,
// Twitch VOD-style"). A clip is one whole MP4 Twitch renders once: no
// playlist, no live edge, no chat replay. Its job is a Twitch row like a
// VOD's — platform "twitch", is_vod, its own title, channel, thumbnail and
// published time (stream_start_time, the clip's createdAt) — told apart by
// its ID, "tw_clip_<slug>" (twitch.ClipJobIDPrefix), as a VOD is by "tw_v".
// It downloads the rendition twitch.SelectClipQuality takes from those
// ShareClipRenderStatus lists, under the job's quality_preference and the
// config's max_video_resolution and prefer_60fps (owner decision FE-29),
// falling back to twitch.BestClipQuality when none fits, as a whole file
// through the engine's direct downloader, the path a YouTube VOD's whole-file formats take, and then runs
// the ordinary finalize (muxAndFinalize: stream copy, thumbnail, metadata,
// Finished). No chat is captured: a clip has none.

// RecordPastedClip enters a clip row an add path just created (POST
// /api/jobs, `moombox add`) in the processing history, as the monitor's
// OnClipFound does for the clips its sweep finds. The sweep of a channel
// whose twitch_archive_clips is on skips a clip whose job ID is in the history or
// is a job now (TwitchMonitor.sweepClips); a pasted clip was only the
// second, so once its Finished row was deleted the next sweep whose window
// still held it archived it again — the dedupe by slug that archives a clip
// once held for the monitor's clips only. A row of any other kind is left
// out of the history, as every other manual add is.
func RecordPastedClip(db *database.Database, job *database.Job) error {
	if !jobfilter.IsClipJob(job) {
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

	// The rendition under the job's quality_preference and the downloader's
	// max_video_resolution and prefer_60fps, by the rule a VOD's selection
	// follows, read fresh from the config as selectTwitchVariant reads them;
	// the best rendition when none fits (owner decision FE-29).
	var maxRes int
	var prefer60fps bool
	sp.readConfig(func(c *config.MoomboxConfig) {
		maxRes = c.Downloader.MaxVideoResolution
		prefer60fps = c.Downloader.Prefer60fps
	})
	best := twitch.SelectClipQuality(clip.Qualities, job.QualityPreference, maxRes, prefer60fps)
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
	if desc := twitchClipDescription(clip); desc != "" {
		updates["description"] = desc
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

// twitchClipDescription is a clip job's description (owner decisions FE-32
// and FQ-3), its parts in this order, a blank line between each:
//
//   - "Clipped by <curator>" — the curator's display name, else the login;
//   - the clip's title;
//   - a link to the source VOD at the clip's start
//     (twitchClipVodLink, https://www.twitch.tv/videos/<id>?t=<h>h<m>m<s>s),
//     when ShareClipRenderStatus names the VOD and an offset into it;
//   - the clip's own description, when it has one.
//
// A part the clip has nothing for is left out. It is the row's description
// column, so job details show it and the finalize writes it beside the
// archive as the .description, as it does a VOD's; no schema change.
func twitchClipDescription(clip *twitch.TwitchClipInfo) string {
	var parts []string
	if curator := cmp.Or(strings.TrimSpace(clip.CuratorDisplayName), clip.CuratorLogin); curator != "" {
		parts = append(parts, "Clipped by "+curator)
	}
	if t := strings.TrimSpace(clip.Title); t != "" {
		parts = append(parts, t)
	}
	if link := twitchClipVodLink(clip); link != "" {
		parts = append(parts, link)
	}
	if d := strings.TrimSpace(clip.Description); d != "" {
		parts = append(parts, d)
	}
	return strings.Join(parts, "\n\n")
}

// twitchClipVodLink is the clip's source VOD at the clip's start, in the
// form Twitch's own share links take — https://www.twitch.tv/videos/<id>
// ?t=<h>h<m>m<s>s, every unit written, 0h0m5s for a clip five seconds in —
// or "" when the answer names no VOD (deleted, or never saved: `video` is
// null), gives no offset, or gives one no VOD has (negative). A VOD ID that
// is not all digits is no Twitch VOD's and makes no link.
func twitchClipVodLink(clip *twitch.TwitchClipInfo) string {
	if clip.VideoID == "" || clip.VideoOffsetSeconds == nil || *clip.VideoOffsetSeconds < 0 {
		return ""
	}
	for _, r := range clip.VideoID {
		if r < '0' || r > '9' {
			return ""
		}
	}
	off := *clip.VideoOffsetSeconds
	return fmt.Sprintf("https://www.twitch.tv/videos/%s?t=%dh%dm%ds", clip.VideoID, off/3600, off%3600/60, off%60)
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
// YouTube progressive format's does: VideoPath alone, HasAudio set). An
// audio_only clip's rendition is written as staging/clip-source.mp4 instead,
// a name discoverStagingMedia does not know: it is not the archive's media
// but what twitchClipExtractAudio takes the audio out of, and a boot re-mux
// or a Mux action that found it as video.mp4 would archive the video the job
// was asked to leave out.
func (o *DownloadOrchestrator) twitchClipDownloadResult(ctx context.Context, jobCtx *JobContext, clip *TwitchClipDownload) *DownloadResult {
	path := filepath.Join(jobCtx.StagingDir, "video.mp4")
	stale := []string{twitchClipSourceFile, twitchClipAudioFile}
	if twitchClipAudioOnly(jobCtx.Job) {
		path = filepath.Join(jobCtx.StagingDir, twitchClipSourceFile)
		stale = []string{"video.mp4"}
	}
	// An attempt under the other shape (quality_preference edited on a
	// failed clip) left its files here, and discoverStagingMedia reads
	// video.mp4 and audio.m4a whenever both exist: a later Mux or boot
	// re-mux would mux a leftover audio.m4a in as a second track beside a
	// video download, or archive a leftover video.mp4 beside the audio an
	// audio_only clip kept. Neither is this shape's to resume, so they go.
	for _, name := range stale {
		if err := os.Remove(filepath.Join(jobCtx.StagingDir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			o.logger.Warn("twitch clip: could not remove another shape's staged file", "jobID", jobCtx.Job.ID, "file", name, "err", err)
		}
	}
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

// twitchClipSourceFile is the staging name of the rendition an audio_only
// clip downloads (twitchClipDownloadResult), and twitchClipAudioFile the
// name of its audio once extracted — audio.m4a, the name a YouTube
// audio-only download stages its track under and discoverStagingMedia reads
// back as the job's audio.
const (
	twitchClipSourceFile = "clip-source.mp4"
	twitchClipAudioFile  = "audio.m4a"
)

// twitchClipAudioOnly reports whether a clip job keeps only its audio: its
// quality_preference is audio_only, read as twitch.SelectClipQuality reads
// it.
func twitchClipAudioOnly(job *database.Job) bool {
	return job != nil && strings.EqualFold(job.QualityPreference, "audio_only")
}

// twitchClipExtractAudio is owner decision FQ-4: an audio_only clip keeps
// only the audio of the rendition it downloaded (twitch.SmallestClipQuality,
// staging/clip-source.mp4), stream-copied into staging/audio.m4a
// (engine.Muxer.ExtractAudioCopy, `-map 0:a -c copy`) and verified like
// every other copy before it takes that name: the copy must hold an audio
// stream and no other, and it goes through verifyMuxedDuration (ENGINE-9's
// check) as every copy does — though that comparison only runs for a source
// of at least muxDurationCheckFloor (a minute), so for a clip, which Twitch
// caps at a minute, it practically never does, and the stream checks are
// what verify the copy. result then carries the audio as
// its primary input, as a YouTube audio-only download's does
// (selectVodFormats: the audio as VideoPath, HasAudio cleared), so the
// ordinary finalize archives it in the container every audio_only job's
// archive takes, an .mp4 with one audio track. A clip with no audio track
// fails with that said; the source stays in staging until the job's own
// cleanup, so a Retry neither re-downloads a whole rendition nor loses it.
//
// BEYOND UPSTREAM: yt-dlp's TwitchClipsIE lists no audio-only format, and
// its -x re-encodes by default; the copy keeps Twitch's AAC as it is.
func (o *DownloadOrchestrator) twitchClipExtractAudio(ctx context.Context, jobCtx *JobContext, result *DownloadResult) error {
	source := result.VideoPath
	audio := filepath.Join(jobCtx.StagingDir, twitchClipAudioFile)
	check := func(ctx context.Context, path string) error {
		probe := o.runFFprobe(ctx, path)
		if probe == nil {
			return nil // a copy ffprobe cannot read passes, as verifiedCopy lets one
		}
		for _, st := range probe.Streams {
			if st.CodecType != "audio" {
				return fmt.Errorf("the extracted audio carries a %s stream", st.CodecType)
			}
		}
		if len(probe.Streams) == 0 {
			return engine.ErrNoAudioTrack
		}
		return o.verifyMuxedDuration(ctx, jobCtx.Job.ID, probe.DurationSec, source)
	}
	if err := o.mux().ExtractAudioCopy(ctx, source, audio, check); err != nil {
		if errors.Is(err, engine.ErrNoAudioTrack) {
			return errors.New("twitch clip error: the clip has no audio track to keep for audio_only")
		}
		return fmt.Errorf("extract the clip's audio: %w", err)
	}
	result.VideoPath = audio
	result.HasAudio = false
	result.VideoHeight, result.VideoFps = 0, 0
	o.logger.Info("twitch clip audio extracted", "jobID", jobCtx.Job.ID, "source", filepath.Base(source))
	return nil
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
	if twitchClipAudioOnly(jobCtx.Job) {
		if err := o.twitchClipExtractAudio(ctx, jobCtx, result); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
	if err := o.muxAndFinalize(ctx, jobCtx, result); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}
