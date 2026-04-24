package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications"
)

// writeDescriptionAtomic writes the description via tmp+rename so a crash
// mid-write can't leave a partially-written .description file that the DB
// row still points at. The tmp file is cleaned up on rename failure.
func writeDescriptionAtomic(finalPath, body string) error {
	tmpPath := finalPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(body), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

func (o *DownloadOrchestrator) muxAndFinalize(ctx context.Context, jobCtx *JobContext, result *DownloadResult) error {
	o.logger.Info("muxing", "jobID", jobCtx.Job.ID)

	// Re-resolve filename template with fresh metadata (matches TS muxFinalize behavior).
	// Title, channel name, and start time may have been updated during stream processing.
	if freshJob, err := o.db.GetJob(jobCtx.Job.ID); err == nil && freshJob != nil {
		template := jobCtx.Config.FilenameTemplate
		var dateStr *string
		if freshJob.StreamStartTime != "" {
			dateStr = &freshJob.StreamStartTime
		} else if freshJob.CreatedAt != "" {
			dateStr = &freshJob.CreatedAt
		}
		templateID := freshJob.VideoID
		if freshJob.Platform == "twitch" {
			templateID = freshJob.ID
		}
		resolved := config.ResolveTemplate(template, config.TemplateVariables{
			Title:   freshJob.Title,
			ID:      templateID,
			Channel: freshJob.ChannelName,
			Date:    dateStr,
		})
		if resolved != "" {
			jobCtx.Filename = resolved
		}
		// Update local job reference for notifications below
		jobCtx.Job = freshJob
	}

	// Multi-segment path: if the job has segments (from quality splitting),
	// the individual segment .mp4 files are already muxed. We just need to
	// handle assets (chat, thumbnail, description) and set the job as finished.
	if segments, err := o.db.GetSegments(jobCtx.Job.ID); err != nil {
		o.logger.Warn("failed to check segments, falling back to single-file mux", "err", err, "jobID", jobCtx.Job.ID)
	} else if len(segments) > 0 {
		return o.finalizeMultiSegmentJob(ctx, jobCtx, segments)
	}

	freshJob := o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
		"status": database.StatusMuxing,
	})
	if freshJob == nil {
		freshJob = jobCtx.Job
	}

	// Send "Muxing Starting" notification with enriched fields (matching TypeScript muxFinalize)
	if o.notifier != nil {
		var muxFields []notifications.Field
		if freshJob.LastVideoSeq != nil {
			muxFields = append(muxFields, notifications.Field{Name: "Video Segments", Value: fmt.Sprintf("%d", *freshJob.LastVideoSeq), Inline: true})
		}
		if freshJob.LastAudioSeq != nil {
			muxFields = append(muxFields, notifications.Field{Name: "Audio Segments", Value: fmt.Sprintf("%d", *freshJob.LastAudioSeq), Inline: true})
		}
		if freshJob.TotalChatMessages != nil {
			muxFields = append(muxFields, notifications.Field{Name: "Chat Messages", Value: fmt.Sprintf("%d", *freshJob.TotalChatMessages), Inline: true})
		}
		if freshJob.DownloadStartedAt != "" {
			if startTime, err := time.Parse(time.RFC3339, freshJob.DownloadStartedAt); err == nil {
				elapsed := time.Since(startTime)
				muxFields = append(muxFields, notifications.Field{Name: "Download Time", Value: formatDurationHuman(elapsed), Inline: true})
			}
		}
		o.notifier.Send("Muxing Starting",
			fmt.Sprintf("Download complete, muxing: %s", jobCtx.Job.Title),
			notifications.TypeMuxing,
			muxFields,
			notifications.SendOptions{
				URL:       jobCtx.Job.URL,
				Thumbnail: jobCtx.Job.ThumbnailURL,
				Event:     "muxing",
			},
		)
	}

	// Resolve output path — template may contain subdirectory (e.g. "${channel}/...")
	filenameBase := jobCtx.Filename
	outputFile := filepath.Join(jobCtx.OutputDir, filenameBase+".mp4")
	outputDir := filepath.Dir(outputFile)
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	// relBase preserves the full relative path (including channel subdir) for DB
	// storage, so the web handler can resolve files via Join(outputDir, filename).
	relBase := filenameBase
	// Strip subdirectory from filenameBase so asset writes (chat, description,
	// thumbnail) use just the filename, not the full template path. outputDir
	// already includes any subdirectory from the template.
	filenameBase = filepath.Base(filenameBase)

	videoPath := result.VideoPath
	audioPath := result.AudioPath

	// Only mux if we have files
	if videoPath != "" {
		if _, err := os.Stat(videoPath); err != nil {
			videoPath = ""
		}
	}
	if audioPath != "" {
		if _, err := os.Stat(audioPath); err != nil {
			audioPath = ""
		}
	}

	if videoPath == "" && audioPath == "" {
		return fmt.Errorf("no media files to mux")
	}

	if err := o.muxer.MuxCopy(ctx, videoPath, audioPath, outputFile); err != nil {
		return fmt.Errorf("mux: %w", err)
	}

	// B5: Run ffprobe to extract actual video metadata
	probeData := o.runFFprobe(ctx, outputFile)

	// Get file info
	info, err := os.Stat(outputFile)
	if err != nil {
		o.logger.Warn("stat output file", "err", err)
	}

	// Update job status (clear progress fields like TS muxFinalize)
	updates := map[string]any{
		"status":      database.StatusFinished,
		"output_file": outputFile,
		"filename":    relBase + ".mp4",
		"progress":    "",
		"percent":     100.0,
		"speed":       "",
		"eta":         "",
	}
	if info != nil {
		updates["file_size"] = info.Size()
	}

	// Prefer ffprobe metadata over format metadata
	if probeData != nil {
		if probeData.Width > 0 {
			updates["video_width"] = probeData.Width
		}
		if probeData.Height > 0 {
			updates["video_height"] = probeData.Height
		}
		if probeData.Fps > 0 {
			updates["video_fps"] = probeData.Fps
		}
		if probeData.DurationSec > 0 {
			updates["length_seconds"] = int(probeData.DurationSec)
		}
	} else if result.VideoFormat != nil {
		// Fallback to format metadata
		if result.VideoFormat.Width != nil {
			updates["video_width"] = *result.VideoFormat.Width
		}
		if result.VideoFormat.Height != nil {
			updates["video_height"] = *result.VideoFormat.Height
		}
		if result.VideoFormat.Fps != nil {
			updates["video_fps"] = *result.VideoFormat.Fps
		}
	}

	// Copy assets (chat, description, thumbnail) to output directory
	o.copyAssets(ctx, jobCtx, outputDir, filenameBase, relBase, updates)

	finishedJob := o.db.UpdateJobFields(jobCtx.Job.ID, updates)

	// Send "Download Finished" notification
	o.sendFinishedNotification(jobCtx, finishedJob, outputFile, probeData, info)

	o.logger.Info("download complete", "jobID", jobCtx.Job.ID, "output", outputFile)
	return nil
}

// finalizeMultiSegmentJob handles the finalization path for jobs with quality-split segments.
// Individual segment .mp4 files are already muxed; this method copies assets and updates the job.
func (o *DownloadOrchestrator) finalizeMultiSegmentJob(ctx context.Context, jobCtx *JobContext, segments []database.Segment) error {
	o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
		"status": database.StatusMuxing,
	})

	filenameBase := jobCtx.Filename
	outputDir := filepath.Join(jobCtx.OutputDir, filepath.Dir(filenameBase))
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	filenameBase = filepath.Base(filenameBase)
	relBase := jobCtx.Filename

	// Calculate total file size and duration from segments
	var totalSize int64
	var totalDuration float64
	for _, seg := range segments {
		if seg.FileSize != nil {
			totalSize += *seg.FileSize
		}
		totalDuration += seg.DurationSeconds
	}

	// Use first segment's resolution for the job metadata
	updates := map[string]any{
		"status":   database.StatusFinished,
		"filename": relBase,
		"progress": "",
		"percent":  100.0,
		"speed":    "",
		"eta":      "",
	}

	if totalSize > 0 {
		updates["file_size"] = totalSize
	}
	if totalDuration > 0 {
		updates["length_seconds"] = int(totalDuration)
	}

	// Use the first segment's quality for job-level metadata
	if len(segments) > 0 {
		if segments[0].VideoWidth != nil {
			updates["video_width"] = *segments[0].VideoWidth
		}
		if segments[0].VideoHeight != nil {
			updates["video_height"] = *segments[0].VideoHeight
		}
		if segments[0].VideoFps != nil {
			updates["video_fps"] = *segments[0].VideoFps
		}
		// Set output_file to the first segment (for backward compat with video route)
		updates["output_file"] = segments[0].FilePath
	}

	// Copy assets
	o.copyAssets(ctx, jobCtx, outputDir, filenameBase, relBase, updates)

	o.db.UpdateJobFields(jobCtx.Job.ID, updates)

	// Send notification
	if o.notifier != nil {
		finishedJob, _ := o.db.GetJob(jobCtx.Job.ID)
		if finishedJob == nil {
			finishedJob = jobCtx.Job
		}

		var qualityLabels []string
		for _, seg := range segments {
			qualityLabels = append(qualityLabels, seg.Quality)
		}
		finFields := []notifications.Field{
			{Name: "Segments", Value: fmt.Sprintf("%d segments", len(segments)), Inline: true},
			{Name: "Qualities", Value: strings.Join(qualityLabels, " -> "), Inline: true},
			{Name: "Total Size", Value: formatFileSize(totalSize), Inline: true},
			{Name: "Duration", Value: formatDurationHuman(time.Duration(totalDuration) * time.Second), Inline: true},
		}
		// Resolution from first segment
		if len(segments) > 0 && segments[0].VideoWidth != nil && segments[0].VideoHeight != nil &&
			*segments[0].VideoWidth > 0 && *segments[0].VideoHeight > 0 {
			res := fmt.Sprintf("%dx%d", *segments[0].VideoWidth, *segments[0].VideoHeight)
			if segments[0].VideoFps != nil && *segments[0].VideoFps > 0 {
				res += fmt.Sprintf(" @%dfps", *segments[0].VideoFps)
			}
			finFields = append(finFields, notifications.Field{Name: "Resolution", Value: res, Inline: true})
		}
		// Total time
		if finishedJob.DownloadStartedAt != "" {
			if startedAt, err := time.Parse(time.RFC3339, finishedJob.DownloadStartedAt); err == nil {
				finFields = append(finFields, notifications.Field{
					Name: "Total Time", Value: formatDurationHuman(time.Since(startedAt)), Inline: true,
				})
			}
		}
		// Chat messages
		if finishedJob.TotalChatMessages != nil && *finishedJob.TotalChatMessages > 0 {
			finFields = append(finFields, notifications.Field{
				Name: "Chat Messages", Value: fmt.Sprintf("%d", *finishedJob.TotalChatMessages), Inline: true,
			})
		}
		o.notifier.Send("Download Finished",
			fmt.Sprintf("Successfully archived: %s", jobCtx.Job.Title),
			notifications.TypeSuccess,
			finFields,
			notifications.SendOptions{
				URL:       jobCtx.Job.URL,
				Image:     jobCtx.Job.ThumbnailURL,
				Event:     "finished",
			},
		)
	}

	o.logger.Info("multi-segment download complete",
		"jobID", jobCtx.Job.ID, "segments", len(segments))
	return nil
}

// copyAssets copies chat, description, and thumbnail files to the output directory.
// Updates the provided map with file paths for DB storage.
func (o *DownloadOrchestrator) copyAssets(ctx context.Context, jobCtx *JobContext, outputDir, filenameBase, relBase string, updates map[string]any) {
	// Copy chat file to output directory
	chatSrc := filepath.Join(jobCtx.StagingDir, "chat.json")
	if _, err := os.Stat(chatSrc); err == nil {
		chatBaseName := filenameBase + ".chat.json"
		chatDst := filepath.Join(outputDir, chatBaseName)
		if err := copyFile(chatSrc, chatDst); err != nil {
			o.logger.Warn("failed to copy chat file", "err", err)
		} else {
			updates["chat_file"] = chatDst
			updates["chat_filename"] = relBase + ".chat.json"
			updates["chat_status"] = "finished"
		}
	}

	// Save video description as .description (matching TypeScript assetDownloader).
	// Atomic write via tmp+rename: a crash mid-write would otherwise leave a
	// truncated .description file with the DB row pointing at it.
	if jobCtx.Job.Description != "" {
		descPath := filepath.Join(outputDir, filenameBase+".description")
		if err := writeDescriptionAtomic(descPath, jobCtx.Job.Description); err != nil {
			o.logger.Warn("failed to save description", "err", err)
		} else {
			updates["description_file"] = descPath
		}
	}

	// Download thumbnail — check staging first (Twitch pre-downloads while live)
	thumbnailSaved := false
	for _, ext := range []string{".jpg", ".webp", ".png"} {
		stagingThumb := filepath.Join(jobCtx.StagingDir, "thumbnail"+ext)
		if _, err := os.Stat(stagingThumb); err == nil {
			thumbDst := filepath.Join(outputDir, filenameBase+ext)
			if err := copyFile(stagingThumb, thumbDst); err == nil {
				thumbnailSaved = true
				updates["thumbnail_file"] = thumbDst
			}
			break
		}
	}
	if !thumbnailSaved && jobCtx.Job.VideoID != "" {
		// Try YouTube thumbnail quality progression (matching TypeScript assetDownloader)
		thumbQualities := []string{"maxresdefault", "sddefault", "hqdefault", "mqdefault", "default"}
		for _, quality := range thumbQualities {
			thumbURL := fmt.Sprintf("https://i.ytimg.com/vi/%s/%s.jpg", jobCtx.Job.VideoID, quality)
			thumbDst := filepath.Join(outputDir, filenameBase+".jpg")
			if DownloadFileMinSize(ctx, thumbURL, thumbDst, 1000) == nil {
				thumbnailSaved = true
				updates["thumbnail_file"] = thumbDst
				break
			}
		}
	}
	if !thumbnailSaved {
		// Fallback: try explicit thumbnail URL or channel avatar
		thumbURL := jobCtx.Job.ThumbnailURL
		if thumbURL == "" {
			thumbURL = jobCtx.Job.ChannelAvatarURL
		}
		if thumbURL != "" {
			ext := ".jpg"
			if strings.Contains(thumbURL, ".webp") {
				ext = ".webp"
			}
			thumbDst := filepath.Join(outputDir, filenameBase+ext)
			if DownloadFileMinSize(ctx, thumbURL, thumbDst, 1000) == nil {
				updates["thumbnail_file"] = thumbDst
			}
		}
	}
}

// sendFinishedNotification sends a "Download Finished" notification with enriched fields.
func (o *DownloadOrchestrator) sendFinishedNotification(jobCtx *JobContext, finishedJob *database.Job, outputFile string, probeData *ffprobeData, info os.FileInfo) {
	if o.notifier == nil {
		return
	}
	if finishedJob == nil {
		finishedJob = jobCtx.Job
	}
	var finFields []notifications.Field
	finFields = append(finFields, notifications.Field{
		Name: "File", Value: filepath.Base(outputFile),
	})
	if probeData != nil && probeData.Width > 0 && probeData.Height > 0 {
		res := fmt.Sprintf("%dx%d", probeData.Width, probeData.Height)
		if probeData.Fps > 0 {
			res += fmt.Sprintf(" @%dfps", probeData.Fps)
		}
		finFields = append(finFields, notifications.Field{Name: "Resolution", Value: res, Inline: true})
	}
	if info != nil {
		sizeStr := formatFileSize(info.Size())
		finFields = append(finFields, notifications.Field{Name: "File Size", Value: sizeStr, Inline: true})
	}
	if finishedJob.LengthSeconds != nil && *finishedJob.LengthSeconds > 0 {
		finFields = append(finFields, notifications.Field{Name: "Duration", Value: formatDurationHuman(time.Duration(*finishedJob.LengthSeconds) * time.Second), Inline: true})
	}
	if finishedJob.DownloadStartedAt != "" {
		if startTime, err := time.Parse(time.RFC3339, finishedJob.DownloadStartedAt); err == nil {
			elapsed := time.Since(startTime)
			finFields = append(finFields, notifications.Field{Name: "Total Time", Value: formatDurationHuman(elapsed), Inline: true})
		}
	}
	if finishedJob.LastVideoSeq != nil {
		segStr := fmt.Sprintf("V: %d", *finishedJob.LastVideoSeq)
		if finishedJob.LastAudioSeq != nil {
			segStr += fmt.Sprintf(" A: %d", *finishedJob.LastAudioSeq)
		}
		finFields = append(finFields, notifications.Field{Name: "Segments", Value: segStr, Inline: true})
	}
	if finishedJob.TotalChatMessages != nil {
		finFields = append(finFields, notifications.Field{Name: "Chat Messages", Value: fmt.Sprintf("%d", *finishedJob.TotalChatMessages), Inline: true})
	}
	// Format selection (matching TS muxFinalize notification enrichment)
	if finishedJob.SelectedVideoItag != nil || finishedJob.SelectedAudioItag != nil {
		var formatInfo string
		if finishedJob.SelectedVideoItag != nil {
			if *finishedJob.SelectedVideoItag == -1 {
				formatInfo = "Video: None"
			} else {
				formatInfo = fmt.Sprintf("Video: itag %d", *finishedJob.SelectedVideoItag)
			}
		}
		if finishedJob.SelectedAudioItag != nil {
			if formatInfo != "" {
				formatInfo += ", "
			}
			if *finishedJob.SelectedAudioItag == -1 {
				formatInfo += "Audio: None"
			} else {
				formatInfo += fmt.Sprintf("Audio: itag %d", *finishedJob.SelectedAudioItag)
			}
		}
		finFields = append(finFields, notifications.Field{Name: "Format Selection", Value: formatInfo})
	}
	// Trimmed range
	if finishedJob.StartTime != nil || finishedJob.EndTime != nil {
		startStr := "0:00"
		endStr := "end"
		if finishedJob.StartTime != nil {
			startStr = FormatSecondsToTimestamp(*finishedJob.StartTime)
		}
		if finishedJob.EndTime != nil {
			endStr = FormatSecondsToTimestamp(*finishedJob.EndTime)
		}
		finFields = append(finFields, notifications.Field{Name: "Trimmed Range", Value: fmt.Sprintf("%s - %s", startStr, endStr), Inline: true})
	}
	// Description excerpt — Discord embeds cap field values around 1024
	// chars, but we keep the notification short. 297 + "..." == 300 total
	// (audit reports/worker.md F40).
	const descMaxLen = 300
	if finishedJob.Description != "" {
		desc := finishedJob.Description
		if len(desc) > descMaxLen {
			desc = desc[:descMaxLen-3] + "..."
		}
		finFields = append(finFields, notifications.Field{Name: "Description", Value: desc})
	}
	o.notifier.Send("Download Finished",
		fmt.Sprintf("Successfully archived: %s", jobCtx.Job.Title),
		notifications.TypeSuccess,
		finFields,
		notifications.SendOptions{
			URL:   jobCtx.Job.URL,
			Image: jobCtx.Job.ThumbnailURL,
			Event: "finished",
		},
	)
}

// muxSegment muxes a single quality segment and persists it to the database.
// Called during quality splits to finalize the current segment before starting a new one.
func (o *DownloadOrchestrator) muxSegment(
	ctx context.Context,
	jobCtx *JobContext,
	segIdx int,
	unixStart, unixEnd int64,
	quality QualityInfo,
	result *DownloadResult,
) (*database.Segment, error) {
	// Build segment filename: {unixStart}_{unixEnd}_{qualityLabel}.mp4
	segFilename := fmt.Sprintf("%d_%d_%s.mp4", unixStart, unixEnd, quality.Label)

	// Re-resolve output directory using the same template logic as muxAndFinalize
	filenameBase := jobCtx.Filename
	outputDir := filepath.Join(jobCtx.OutputDir, filepath.Dir(filenameBase))
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("create segment output dir: %w", err)
	}

	outputPath := filepath.Join(outputDir, segFilename)

	videoPath := result.VideoPath
	audioPath := result.AudioPath

	// Verify files exist
	if videoPath != "" {
		if _, err := os.Stat(videoPath); err != nil {
			videoPath = ""
		}
	}
	if audioPath != "" {
		if _, err := os.Stat(audioPath); err != nil {
			audioPath = ""
		}
	}

	if videoPath == "" && audioPath == "" {
		return nil, fmt.Errorf("no media files to mux for segment %d", segIdx)
	}

	// MuxCopy (no re-encoding)
	if err := o.muxer.MuxCopy(ctx, videoPath, audioPath, outputPath); err != nil {
		return nil, fmt.Errorf("mux segment %d: %w", segIdx, err)
	}

	// FFprobe for metadata
	probeData := o.runFFprobe(ctx, outputPath)

	// Get file info
	info, _ := os.Stat(outputPath)

	// Build segment record
	seg := &database.Segment{
		JobID:        jobCtx.Job.ID,
		SegmentIndex: segIdx,
		UnixStart:    unixStart,
		UnixEnd:      unixEnd,
		Quality:      quality.Label,
		Filename:     segFilename,
		FilePath:     outputPath,
	}

	if info != nil {
		size := info.Size()
		seg.FileSize = &size
	}
	if probeData != nil {
		if probeData.Width > 0 {
			seg.VideoWidth = &probeData.Width
		}
		if probeData.Height > 0 {
			seg.VideoHeight = &probeData.Height
		}
		if probeData.Fps > 0 {
			seg.VideoFps = &probeData.Fps
		}
		seg.DurationSeconds = probeData.DurationSec
	}

	// Persist to database
	if err := o.db.AddSegment(seg); err != nil {
		o.logger.Error("failed to persist segment", "err", err, "segment", segIdx)
		return seg, fmt.Errorf("persist segment: %w", err)
	}

	return seg, nil
}

// muxFromStaging discovers segment files in the staging directory and runs
// the full mux pipeline. Used for the "Mux" action on cancelled/errored jobs
// where no DownloadResult exists from the download pipeline.
func (o *DownloadOrchestrator) muxFromStaging(ctx context.Context, jobCtx *JobContext) error {
	stagingDir := jobCtx.StagingDir

	// Discover segment files in priority order (DASH > HLS > VOD)
	result := &DownloadResult{}

	// DASH segments
	if fileExists(filepath.Join(stagingDir, "video_stream")) {
		result.VideoPath = filepath.Join(stagingDir, "video_stream")
		result.HasVideo = true
	}
	if fileExists(filepath.Join(stagingDir, "audio_stream")) {
		result.AudioPath = filepath.Join(stagingDir, "audio_stream")
		result.HasAudio = true
	}

	// HLS (single muxed stream)
	if !result.HasVideo && fileExists(filepath.Join(stagingDir, "video.ts")) {
		result.VideoPath = filepath.Join(stagingDir, "video.ts")
		result.HasVideo = true
		result.IsHls = true
	}

	// VOD
	if !result.HasVideo && fileExists(filepath.Join(stagingDir, "video.mp4")) {
		result.VideoPath = filepath.Join(stagingDir, "video.mp4")
		result.HasVideo = true
	}
	if !result.HasAudio && fileExists(filepath.Join(stagingDir, "audio.m4a")) {
		result.AudioPath = filepath.Join(stagingDir, "audio.m4a")
		result.HasAudio = true
	}

	if !result.HasVideo && !result.HasAudio {
		return fmt.Errorf("no segment files found in staging directory")
	}

	return o.muxAndFinalize(ctx, jobCtx, result)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
