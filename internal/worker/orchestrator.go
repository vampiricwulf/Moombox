package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/chat"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

const (
	streamEndVerifyInterval   = 5 * time.Minute
	streamSegmentTimeout      = 10 * time.Minute
	maxConsecutiveLiveChecks  = 6
	chatWaitTimeout           = 2 * time.Minute // TypeScript uses 2 minutes for all chat waits
	qualityMonitorInterval    = 30 * time.Second
	minSegmentDuration        = 10 * time.Second // Don't split segments shorter than this
)

// DownloadOrchestrator coordinates the full download lifecycle for a job.
type DownloadOrchestrator struct {
	muxer        *engine.Muxer
	ffmpegPath   string
	db           *database.Database
	queue        *JobQueue
	cipherSolver *cipher.Solver
	potProvider  *bgutils.PotProvider
	notifier     *notifications.Manager
	logger       logger
}

// NewDownloadOrchestrator creates a new orchestrator.
func NewDownloadOrchestrator(db *database.Database, queue *JobQueue, ffmpegPath string, logger logger, cs *cipher.Solver, pp *bgutils.PotProvider, nm *notifications.Manager) *DownloadOrchestrator {
	return &DownloadOrchestrator{
		muxer:        engine.NewMuxer(ffmpegPath, logger),
		ffmpegPath:   ffmpegPath,
		db:           db,
		queue:        queue,
		cipherSolver: cs,
		potProvider:  pp,
		notifier:     nm,
		logger:       logger,
	}
}

// formatFileSize formats bytes into human-readable string.
var formatFileSize = utils.FormatFileSize

// formatDurationHuman formats a time.Duration into a human-readable string (e.g. "1h 23m", "5m 30s").
var formatDurationHuman = utils.FormatDurationHuman

// Execute runs the full download pipeline for a YouTube job.
func (o *DownloadOrchestrator) Execute(ctx context.Context, jobCtx *JobContext, videoInfo *youtube.VideoInfo, isVod bool) error {
	return o.ExecuteWithChat(ctx, jobCtx, videoInfo, isVod, nil)
}

// ExecuteWithChat runs the full download pipeline, optionally with a pre-started chat downloader.
func (o *DownloadOrchestrator) ExecuteWithChat(ctx context.Context, jobCtx *JobContext, videoInfo *youtube.VideoInfo, isVod bool, existingChat *chat.ChatDownloader) error {
	o.logger.Info("starting download", "videoID", jobCtx.Job.VideoID, "isVod", isVod)

	// Pre-execution cancellation check — if job was cancelled between queuing and execution
	if freshJob, err := o.db.GetJob(jobCtx.Job.ID); err == nil && freshJob.Status == database.StatusCancelled {
		if existingChat != nil {
			existingChat.Stop()
		}
		return nil
	}

	// Subscribe to job status changes for cancellation
	jobCtx2, cancel := context.WithCancel(ctx)
	defer cancel()

	unsubscribe := o.db.OnJobUpdate(func(updatedJob *database.Job) {
		if updatedJob.ID == jobCtx.Job.ID && updatedJob.Status == database.StatusCancelled {
			o.logger.Info("cancel detected via DB listener", "jobID", jobCtx.Job.ID)
			cancel()
		}
	})
	defer unsubscribe()

	ctx = jobCtx2

	// Update status
	downloadStartedAt := time.Now().UTC().Format(time.RFC3339)
	o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
		"status":              database.StatusDownloading,
		"download_started_at": downloadStartedAt,
	})

	// Send "Download Starting" notification
	if o.notifier != nil {
		dlType := "Live Stream"
		if isVod {
			dlType = "VOD"
		}
		startFields := []notifications.Field{
			{Name: "Channel", Value: jobCtx.Job.ChannelName, Inline: true},
			{Name: "Type", Value: dlType, Inline: true},
		}
		// Include format selection details (matching TypeScript)
		if jobCtx.Job.SelectedVideoItag != nil {
			label := "None"
			if *jobCtx.Job.SelectedVideoItag >= 0 {
				label = fmt.Sprintf("itag %d", *jobCtx.Job.SelectedVideoItag)
			}
			startFields = append(startFields, notifications.Field{Name: "Video", Value: label, Inline: true})
		}
		if jobCtx.Job.SelectedAudioItag != nil {
			label := "None"
			if *jobCtx.Job.SelectedAudioItag >= 0 {
				label = fmt.Sprintf("itag %d", *jobCtx.Job.SelectedAudioItag)
			}
			startFields = append(startFields, notifications.Field{Name: "Audio", Value: label, Inline: true})
		}
		// Include post-download trim info
		if jobCtx.Job.StartTime != nil || jobCtx.Job.EndTime != nil {
			startStr := "00:00"
			endStr := "end"
			if jobCtx.Job.StartTime != nil {
				startStr = FormatSecondsToTimestamp(*jobCtx.Job.StartTime)
			}
			if jobCtx.Job.EndTime != nil {
				endStr = FormatSecondsToTimestamp(*jobCtx.Job.EndTime)
			}
			startFields = append(startFields, notifications.Field{
				Name: "Post-Download Trim", Value: startStr + " - " + endStr,
			})
		}
		o.notifier.Send("Download Starting",
			fmt.Sprintf("Beginning download: %s", jobCtx.Job.Title),
			notifications.TypeDownload,
			startFields,
			notifications.SendOptions{
				URL:       jobCtx.Job.URL,
				Thumbnail: jobCtx.Job.ThumbnailURL,
				Event:     "downloading",
			},
		)
	}

	// Create staging directory
	if err := os.MkdirAll(jobCtx.StagingDir, 0o755); err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}

	// Update early chat output file now that staging dir exists
	if existingChat != nil {
		chatPath := filepath.Join(jobCtx.StagingDir, "chat.json")
		existingChat.SetOutputFile(chatPath)
	}

	// Select download strategy (A1: pass cipher/pot to strategies)
	var result *DownloadResult
	var err error

	// Strategy selection (matches TS):
	// - not_a_stream or VOD without DASH → direct format download
	// - post_live/VOD WITH DASH → use DASH segments (TS comment: format URLs may only serve individual segments)
	useDirectVod := isVod && (videoInfo.StreamStatus == youtube.StreamNotAStream || videoInfo.DashManifestURL == "")
	o.logger.Debug("download strategy selection",
		"isVod", isVod,
		"streamStatus", videoInfo.StreamStatus,
		"hasDash", videoInfo.DashManifestURL != "",
		"hasHls", videoInfo.HlsManifestURL != "",
		"formatCount", len(videoInfo.Formats),
		"useDirectVod", useDirectVod)
	if useDirectVod && len(videoInfo.Formats) > 0 {
		result, err = DownloadVod(ctx, jobCtx, videoInfo, o.cipherSolver, o.potProvider)
	} else if videoInfo.DashManifestURL != "" {
		result, err = DownloadDash(ctx, jobCtx, videoInfo, o.cipherSolver, o.potProvider)
	} else if videoInfo.HlsManifestURL != "" {
		result, err = DownloadHls(ctx, jobCtx, videoInfo, o.potProvider)
	} else if len(videoInfo.Formats) > 0 {
		// Fallback: no DASH/HLS manifest but formats exist — download directly
		result, err = DownloadVod(ctx, jobCtx, videoInfo, o.cipherSolver, o.potProvider)
	} else {
		return fmt.Errorf("no download strategy available")
	}

	if err != nil {
		return fmt.Errorf("setup download: %w", err)
	}

	// Check abort immediately after download setup — avoid false progress updates
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Setup progress tracking
	tracker := NewProgressTracker(o.db, jobCtx.Job.ID, o.logger)

	if result.VideoDownloader != nil {
		tracker.AttachVideoDownloader(result.VideoDownloader)
	}
	if result.AudioDownloader != nil {
		tracker.AttachAudioDownloader(result.AudioDownloader)
	}

	// A3: Start chat downloader in parallel
	chatDl := existingChat
	var chatDone chan struct{}
	if !jobCtx.Config.DownloadChat && chatDl != nil {
		// download_chat is disabled but we inherited a pre-started chat — stop it
		chatDl.Stop()
		chatDl = nil
	}
	if chatDl == nil && jobCtx.Config.DownloadChat {
		chatDl = o.setupChatDownloader(ctx, jobCtx, videoInfo, isVod)
	}
	if chatDl != nil {
		chatDone = make(chan struct{})
		chatDl.OnProgress = func(p chat.ChatProgress) {
			tracker.SetChatCount(p.MessageCount)
		}
		go func() {
			defer close(chatDone)
			defer func() {
				if r := recover(); r != nil {
					o.logger.Error("panic in YouTube chat downloader", "jobID", jobCtx.Job.ID, "panic", fmt.Sprint(r))
				}
			}()
			chatDl.Start(ctx)
		}()
	}

	// Run download loop
	if isVod {
		// VOD: run downloaders once
		err = o.runDownloaders(ctx, result)

		// Set 100% progress after VOD download completes (finishVodWithChat equivalent)
		// Include audio percentage and chat count to match TS format
		if err == nil {
			progressStr := "V:100% A:100%"
			if chatDl != nil {
				chatCount := chatDl.MessageCount()
				if chatCount > 0 {
					progressStr += fmt.Sprintf(" C: %d", chatCount)
				}
			}
			o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
				"progress": progressStr,
				"percent":  100.0,
			})
		}
	} else {
		// Live: run with stream-end verification (A2)
		err = o.runLiveStreamDownload(ctx, jobCtx, videoInfo, result, tracker)
	}

	if err != nil {
		if ctx.Err() != nil {
			// Shutdown: stop chat but preserve staging dir for resume
			if chatDl != nil {
				chatDl.Stop()
				if chatDone != nil {
					chatTimer := time.NewTimer(2 * time.Second)
					select {
					case <-chatDone:
						chatTimer.Stop()
					case <-chatTimer.C:
					}
				}
			}
			return ctx.Err()
		}
		o.cleanup(jobCtx, chatDl, chatDone)
		return fmt.Errorf("download: %w", err)
	}

	tracker.Finalize()

	// Sync total seq counts to last downloaded values so progress shows
	// matching values (e.g., "V: 150/150 A: 150/150") before muxing starts.
	syncUpdates := map[string]any{}
	if result.VideoDownloader != nil {
		if lastSeq := result.VideoDownloader.LastSeq(); lastSeq > 0 {
			syncUpdates["total_video_seq"] = lastSeq
		}
	}
	if result.AudioDownloader != nil {
		if lastSeq := result.AudioDownloader.LastSeq(); lastSeq > 0 {
			syncUpdates["total_audio_seq"] = lastSeq
		}
	}
	if len(syncUpdates) > 0 {
		o.db.UpdateJobFields(jobCtx.Job.ID, syncUpdates)
	}

	// Signal chat to finish and wait.
	// For live streams, MarkStreamEnded() wakes up the polling loop so it exits cleanly.
	// For VODs, chat replay should finish naturally (all pages fetched) — don't cancel it.
	if chatDl != nil {
		if !isVod {
			chatDl.MarkStreamEnded()
		}
		o.waitForChat(chatDl, chatDone, chatWaitTimeout)

		// Update chat status on job
		chatCount := chatDl.MessageCount()
		chatStatus := "finished"
		if chatCount == 0 {
			chatStatus = "unavailable"
		}
		o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
			"chat_status":         chatStatus,
			"total_chat_messages": chatCount,
		})
	}

	// Check cancellation between download and mux — preserve staging for resume
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Set streamEndTime fallback if not already set
	if jobCtx.Job.StreamEndTime == "" {
		endTime := time.Now().UTC().Format(time.RFC3339)
		o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
			"stream_end_time": endTime,
		})
		jobCtx.Job.StreamEndTime = endTime
	}

	// Release download slot before muxing — mux is CPU-bound, not a download
	if o.queue != nil {
		o.queue.ReleaseDownloadSlot(jobCtx.Job.ID)
	}

	// Mux and finalize (B5: includes ffprobe metadata)
	if err := o.muxAndFinalize(ctx, jobCtx, result); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}

	// Post-download trim: if job has startTime/endTime, create a trimmed version
	if (jobCtx.Job.StartTime != nil || jobCtx.Job.EndTime != nil) && ctx.Err() == nil {
		o.logger.Info("creating post-download trim",
			"jobID", jobCtx.Job.ID,
			"startTime", jobCtx.Job.StartTime,
			"endTime", jobCtx.Job.EndTime)

		trimService := NewTrimService(o.db, o.ffmpegPath, o.logger)
		if o.notifier != nil {
			trimService.SetNotifier(o.notifier)
		}
		startSec := 0.0
		if jobCtx.Job.StartTime != nil {
			startSec = *jobCtx.Job.StartTime
		}
		endSec := 0.0
		if jobCtx.Job.EndTime != nil {
			endSec = *jobCtx.Job.EndTime
		} else if jobCtx.Job.LengthSeconds != nil {
			endSec = float64(*jobCtx.Job.LengthSeconds)
		}
		if endSec > startSec {
			// Re-fetch job to get updated fields (output_file set by muxAndFinalize)
			freshJob, _ := o.db.GetJob(jobCtx.Job.ID)
			if freshJob != nil && freshJob.Status == database.StatusFinished {
				_, trimErr := trimService.CreateTrim(ctx, freshJob, startSec, endSec)
				if trimErr != nil {
					o.logger.Error("post-download trim failed", "err", trimErr, "jobID", jobCtx.Job.ID)
					if o.notifier != nil {
						o.notifier.Send("Trim Failed",
							fmt.Sprintf("Failed to create trim for \"%s\": %v", jobCtx.Job.Title, trimErr),
							notifications.TypeError,
							nil,
							notifications.SendOptions{
								URL:   jobCtx.Job.URL,
								Event: "trim_error",
							},
						)
					}
				}
			}
		}
	}

	return nil
}

// runDownloaders runs video and audio downloaders using errgroup (B7: fixes goroutine leak).
func (o *DownloadOrchestrator) runDownloaders(ctx context.Context, result *DownloadResult) error {
	g, gctx := errgroup.WithContext(ctx)

	if result.VideoDownloader != nil {
		g.Go(func() error {
			return result.VideoDownloader.Start(gctx)
		})
	}

	if result.AudioDownloader != nil {
		g.Go(func() error {
			return result.AudioDownloader.Start(gctx)
		})
	}

	return g.Wait()
}

// runLiveStreamDownload runs downloaders with stream-end verification loop (A2, B4).
// Supports quality monitoring: when the available quality changes mid-stream,
// the current download segment is muxed and a new download starts at the new quality.
func (o *DownloadOrchestrator) runLiveStreamDownload(
	ctx context.Context,
	jobCtx *JobContext,
	videoInfo *youtube.VideoInfo,
	result *DownloadResult,
	tracker *ProgressTracker,
) error {
	var lastSegmentTime = time.Now()
	consecutiveLiveChecks := 0

	// Quality monitoring state
	segmentIndex := 0
	segmentStartTime := time.Now().Unix()
	currentQuality := o.extractQualityFromResult(result)
	qualityChangeCh := make(chan QualityInfo, 1)
	var segmentMuxWg sync.WaitGroup // tracks background segment mux goroutines
	defer segmentMuxWg.Wait()       // ensure all background muxes finish before returning

	// Only monitor quality if user hasn't manually selected itags and isn't audio-only
	monitoringEnabled := jobCtx.Job.SelectedVideoItag == nil && jobCtx.Job.QualityPreference != "audio_only"

	// Start proactive quality monitor (30s timer)
	var monitorCancel context.CancelFunc
	var monitor *QualityMonitor
	if monitoringEnabled {
		monitorCtx, mc := context.WithCancel(ctx)
		monitorCancel = mc
		probeFn := o.buildYouTubeProbeFn(jobCtx)
		monitor = NewQualityMonitor(qualityMonitorInterval, currentQuality, probeFn, o.logger)
		go monitor.Run(monitorCtx, qualityChangeCh)
	}
	defer func() {
		if monitorCancel != nil {
			monitorCancel()
		}
	}()

	// Track segment activity via progress callbacks
	onSegmentProgress := func() {
		lastSegmentTime = time.Now()
		consecutiveLiveChecks = 0
	}

	attachProgress := func(res *DownloadResult) {
		if res.VideoDownloader != nil {
			tracker.AttachVideoDownloader(res.VideoDownloader)
			origOnProgress := res.VideoDownloader.OnProgress
			res.VideoDownloader.OnProgress = func(p engine.DownloadProgress) {
				onSegmentProgress()
				if origOnProgress != nil {
					origOnProgress(p)
				}
			}
		}
		if res.AudioDownloader != nil {
			tracker.AttachAudioDownloader(res.AudioDownloader)
			origOnProgress := res.AudioDownloader.OnProgress
			res.AudioDownloader.OnProgress = func(p engine.DownloadProgress) {
				onSegmentProgress()
				if origOnProgress != nil {
					origOnProgress(p)
				}
			}
		}
	}

	attachProgress(result)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Run segment downloaders in a goroutine so we can also listen for quality changes
		downloadDone := make(chan error, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					downloadDone <- fmt.Errorf("runDownloaders panic: %v", r)
				}
			}()
			downloadDone <- o.runDownloaders(ctx, result)
		}()

		var downloadErr error
		qualityChanged := false

	awaitYTDownload:
		for {
			select {
			case newQ := <-qualityChangeCh:
				// Proactive quality change while download is running
				if time.Since(time.Unix(segmentStartTime, 0)) < minSegmentDuration {
					// Too soon — don't split. Reset monitor baseline so it
					// re-detects the change once we're past minSegmentDuration.
					o.logger.Debug("quality change ignored (min segment duration)",
						"from", currentQuality.Label, "to", newQ.Label,
						"jobID", jobCtx.Job.ID)
					if monitor != nil {
						monitor.UpdateBaseline(currentQuality)
					}
					continue
				}
				qualityChanged = true
				o.logger.Info("proactive quality change detected, stopping downloaders",
					"from", currentQuality.Label, "to", newQ.Label,
					"jobID", jobCtx.Job.ID)
				// Cancel current downloaders
				if result.VideoDownloader != nil {
					result.VideoDownloader.Cancel()
				}
				if result.AudioDownloader != nil {
					result.AudioDownloader.Cancel()
				}
				downloadErr = <-downloadDone
				break awaitYTDownload
			case downloadErr = <-downloadDone:
				// Download stopped on its own
				break awaitYTDownload
			}
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Check for reactive quality loss (download loop returned ErrQualityLost)
		isQualityLost := errors.Is(downloadErr, engine.ErrQualityLost)

		if qualityChanged || isQualityLost {
			segmentEndTime := time.Now().Unix()
			shortSegment := time.Since(time.Unix(segmentStartTime, 0)) < minSegmentDuration

			// Mux this segment in the background so the new download starts immediately.
			// All values are captured by the goroutine — no shared mutable state.
			if !shortSegment {
				muxIdx := segmentIndex
				muxStart := segmentStartTime
				muxEnd := segmentEndTime
				muxQuality := currentQuality
				muxResult := result
				segmentMuxWg.Add(1)
				go func() {
					defer func() {
						if r := recover(); r != nil {
							o.logger.Error("panic in mux segment goroutine", "panic", fmt.Sprint(r), "jobID", jobCtx.Job.ID)
						}
					}()
					defer segmentMuxWg.Done()
					// Use background context — data is already downloaded, let FFmpeg finish
					// even during cancellation to avoid orphaned partial output files.
					seg, muxErr := o.muxSegment(context.Background(), jobCtx, muxIdx, muxStart, muxEnd, muxQuality, muxResult)
					if muxErr != nil {
						o.logger.Error("failed to mux quality segment", "err", muxErr, "jobID", jobCtx.Job.ID)
					} else if seg != nil {
						o.logger.Info("quality segment muxed",
							"segment", muxIdx, "quality", muxQuality.Label,
							"file", seg.Filename, "jobID", jobCtx.Job.ID)
					}
				}()
				segmentIndex++
			} else {
				o.logger.Debug("skipping short segment mux",
					"duration", time.Since(time.Unix(segmentStartTime, 0)).Round(time.Second),
					"jobID", jobCtx.Job.ID)
			}

			// Re-fetch manifest and select new best quality (concurrent with background mux)
			freshInfo, err := jobCtx.YT.GetVideoInfo(ctx, jobCtx.Job.VideoID)
			if err != nil {
				o.logger.Error("failed to refresh video info after quality change", "err", err, "jobID", jobCtx.Job.ID)
				return fmt.Errorf("refresh after quality change: %w", err)
			}

			// Cancel old downloaders
			if result.VideoDownloader != nil {
				result.VideoDownloader.Cancel()
			}
			if result.AudioDownloader != nil {
				result.AudioDownloader.Cancel()
			}

			// Create new staging subdirectory for the new segment
			segStagingDir := filepath.Join(jobCtx.StagingDir, fmt.Sprintf("seg_%d", segmentIndex))
			if err := os.MkdirAll(segStagingDir, 0o755); err != nil {
				return fmt.Errorf("create segment staging dir: %w", err)
			}

			// Create a temporary job context pointing to the new staging dir
			segJobCtx := *jobCtx
			segJobCtx.StagingDir = segStagingDir

			var refreshResult *DownloadResult
			var refreshErr error
			if result.IsHls {
				refreshResult, refreshErr = DownloadHls(ctx, &segJobCtx, freshInfo, o.potProvider)
			} else {
				refreshResult, refreshErr = DownloadDash(ctx, &segJobCtx, freshInfo, o.cipherSolver, o.potProvider)
			}

			if refreshErr != nil {
				o.logger.Error("failed to refresh for new quality", "err", refreshErr, "jobID", jobCtx.Job.ID)
				return nil // Can't continue, finalize what we have
			}

			newQuality := o.extractQualityFromResult(refreshResult)
			o.logger.Info("quality split",
				"from", currentQuality.Label, "to", newQuality.Label,
				"segment", segmentIndex, "jobID", jobCtx.Job.ID)

			currentQuality = newQuality
			result = refreshResult
			segmentStartTime = time.Now().Unix()

			// Update monitor baseline so it doesn't re-detect the same change
			if monitor != nil {
				select {
				case <-qualityChangeCh:
				default:
				}
				monitor.UpdateBaseline(currentQuality)
			}

			attachProgress(result)
			continue
		}

		// Normal download stop — verify stream ended (existing logic)
		timeSinceLastSeg := time.Since(lastSegmentTime)
		o.logger.Info("segment downloaders stopped",
			"timeSinceLastSeg", timeSinceLastSeg.Round(time.Second),
			"jobID", jobCtx.Job.ID)

		// Verify stream status with YouTube API
		freshInfo, err := jobCtx.YT.GetVideoInfo(ctx, jobCtx.Job.VideoID)
		if err != nil {
			o.logger.Warn("failed to verify stream status", "err", err, "jobID", jobCtx.Job.ID)

			if timeSinceLastSeg >= streamSegmentTimeout {
				o.logger.Info("no segments for too long and API failed, assuming ended", "jobID", jobCtx.Job.ID)
				break
			}

			// Wait and retry
			utils.Sleep(ctx, streamEndVerifyInterval)
			continue
		}

		o.logger.Info("YouTube reports stream status", "status", freshInfo.StreamStatus, "jobID", jobCtx.Job.ID)

		switch freshInfo.StreamStatus {
		case youtube.StreamPostLive, youtube.StreamVOD, youtube.StreamNotAStream:
			// Stream confirmed ended
			o.logger.Info("stream confirmed ended", "status", freshInfo.StreamStatus, "jobID", jobCtx.Job.ID)
			goto streamEnded

		case youtube.StreamLive:
			consecutiveLiveChecks++
			if consecutiveLiveChecks >= maxConsecutiveLiveChecks {
				o.logger.Warn("YouTube reported live too many times with no segments, forcing end",
					"checks", consecutiveLiveChecks, "jobID", jobCtx.Job.ID)
				goto streamEnded
			}

			o.logger.Info("stream still live, refreshing manifests",
				"check", consecutiveLiveChecks, "max", maxConsecutiveLiveChecks, "jobID", jobCtx.Job.ID)

			o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
				"progress": "Waiting for stream to end...",
			})

			utils.Sleep(ctx, streamEndVerifyInterval)

			if ctx.Err() != nil {
				return ctx.Err()
			}

			// Cancel old downloaders before refreshing
			if result.VideoDownloader != nil {
				result.VideoDownloader.Cancel()
			}
			if result.AudioDownloader != nil {
				result.AudioDownloader.Cancel()
			}

			// B4: Refresh manifests and create new downloaders
			var refreshResult *DownloadResult
			var refreshErr error
			if result.IsHls {
				refreshResult, refreshErr = DownloadHls(ctx, jobCtx, freshInfo, o.potProvider)
			} else {
				refreshResult, refreshErr = DownloadDash(ctx, jobCtx, freshInfo, o.cipherSolver, o.potProvider)
			}

			if refreshErr != nil {
				o.logger.Warn("failed to refresh manifests", "err", refreshErr, "jobID", jobCtx.Job.ID)
				utils.Sleep(ctx, streamEndVerifyInterval)
				continue
			}

			// Replace downloaders with refreshed versions
			result.VideoDownloader = refreshResult.VideoDownloader
			result.AudioDownloader = refreshResult.AudioDownloader

			attachProgress(result)

		default:
			// Unexpected status, treat as ended
			o.logger.Warn("unexpected stream status, treating as ended",
				"status", freshInfo.StreamStatus, "jobID", jobCtx.Job.ID)
			goto streamEnded
		}
	}

streamEnded:
	// Wait for background segment muxes to finish before final mux —
	// their DB records must exist before muxAndFinalize calls GetSegments.
	segmentMuxWg.Wait()

	// If we had quality splits, mux the final segment
	if segmentIndex > 0 {
		segmentEndTime := time.Now().Unix()
		seg, muxErr := o.muxSegment(ctx, jobCtx, segmentIndex, segmentStartTime, segmentEndTime, currentQuality, result)
		if muxErr != nil {
			o.logger.Error("failed to mux final quality segment", "err", muxErr, "jobID", jobCtx.Job.ID)
		} else if seg != nil {
			o.logger.Info("final quality segment muxed",
				"segment", segmentIndex, "quality", currentQuality.Label,
				"file", seg.Filename, "jobID", jobCtx.Job.ID)
		}
	}

	return nil
}

// setupChatDownloader creates a chat downloader for a YouTube job (A3).
// Fetches the watch page to extract the chat continuation token, visitor data,
// and determines whether chat is live or replay. Returns nil if chat is unavailable.
func (o *DownloadOrchestrator) setupChatDownloader(ctx context.Context, jobCtx *JobContext, videoInfo *youtube.VideoInfo, isVod bool) *chat.ChatDownloader {
	// Fetch watch page to get chat continuation and visitor data
	cookieHeader := ""
	if jobCtx.YT != nil && jobCtx.YT.Auth != nil {
		cookieHeader = jobCtx.YT.Auth.GetCookieHeader()
	}

	watchResult, err := youtube.FetchWatchPage(ctx, jobCtx.Job.VideoID, cookieHeader)
	if err != nil {
		o.logger.Warn("failed to fetch watch page for chat", "err", err, "videoID", jobCtx.Job.VideoID)
		o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
			"chat_status": "unavailable",
		})
		return nil
	}

	// Extract chat continuation from the watch page HTML
	continuation, isReplay, err := chat.ExtractChatContinuation(watchResult.HTML)
	if err != nil || continuation == "" {
		o.logger.Debug("no chat continuation available", "videoID", jobCtx.Job.VideoID, "err", err)
		o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
			"chat_status": "unavailable",
		})
		return nil
	}

	// Extract visitor data from ytcfg
	visitorData := ""
	if watchResult.Ytcfg != nil {
		visitorData = watchResult.Ytcfg.VisitorData
	}

	chatPath := filepath.Join(jobCtx.StagingDir, "chat.json")
	opts := chat.ChatDownloaderOptions{
		VideoID:             jobCtx.Job.VideoID,
		VideoTitle:          jobCtx.Job.Title,
		ChannelName:         jobCtx.Job.ChannelName,
		OutputFile:          chatPath,
		InitialContinuation: continuation,
		ApiKey:              constants.DefaultAPIKey,
		VisitorData:         visitorData,
		CookieHeader:        cookieHeader,
		IsReplay:            isReplay,
		IsLiveOrUpcoming:    videoInfo.IsLive || videoInfo.IsUpcoming,
	}
	if jobCtx.YT != nil && jobCtx.YT.Auth != nil {
		opts.GenerateAuth = jobCtx.YT.Auth.GenerateAuthorizationHeader
	}

	if videoInfo.ScheduledStartTime != "" {
		opts.StreamStartTime = videoInfo.ScheduledStartTime
	}

	dl := chat.NewChatDownloader(opts)
	dl.OnError = func(err error) {
		o.logger.Warn("[Chat] Chat API error", "jobID", jobCtx.Job.ID, "err", err)
	}
	o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
		"chat_status": "pending",
	})

	// Transition from "pending" → "downloading" when chat actually starts (matches TS "start" event)
	dl.OnStart = func(messageCount int, resuming bool) {
		o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
			"chat_status": "downloading",
		})
		if resuming {
			o.logger.Info("[Chat] Resuming chat download", "jobID", jobCtx.Job.ID, "messages", messageCount)
		} else {
			o.logger.Info("[Chat] Started downloading chat", "jobID", jobCtx.Job.ID)
		}
	}

	return dl
}

// waitForChat waits for chat to finish with a timeout.
func (o *DownloadOrchestrator) waitForChat(chatDl *chat.ChatDownloader, chatDone chan struct{}, timeout time.Duration) {
	if chatDone == nil {
		return
	}

	timer := time.NewTimer(timeout)
	select {
	case <-chatDone:
		timer.Stop()
		// Chat finished naturally
	case <-timer.C:
		// Timeout — force stop
		if chatDl.IsRunning() {
			chatDl.Stop()
		}
		// Wait a bit more for cleanup
		cleanupTimer := time.NewTimer(2 * time.Second)
		select {
		case <-chatDone:
			cleanupTimer.Stop()
		case <-cleanupTimer.C:
		}
	}
}

// cleanup handles cancellation cleanup.
func (o *DownloadOrchestrator) cleanup(jobCtx *JobContext, chatDl *chat.ChatDownloader, chatDone chan struct{}) {
	if chatDl != nil {
		chatDl.Stop()
		if chatDone != nil {
			cleanupTimer := time.NewTimer(2 * time.Second)
			select {
			case <-chatDone:
				cleanupTimer.Stop()
			case <-cleanupTimer.C:
			}
		}
	}
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

	o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
		"status": database.StatusMuxing,
	})

	// Send "Muxing Starting" notification with enriched fields (matching TypeScript muxFinalize)
	if o.notifier != nil {
		freshJob, _ := o.db.GetJob(jobCtx.Job.ID)
		if freshJob == nil {
			freshJob = jobCtx.Job
		}
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

	// Save video description as .description (matching TypeScript assetDownloader)
	if jobCtx.Job.Description != "" {
		descPath := filepath.Join(outputDir, filenameBase+".description")
		if err := os.WriteFile(descPath, []byte(jobCtx.Job.Description), 0o644); err != nil {
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

	o.db.UpdateJobFields(jobCtx.Job.ID, updates)

	// Send "Download Finished" notification with enriched fields (matching TypeScript muxFinalize)
	if o.notifier != nil {
		finishedJob, _ := o.db.GetJob(jobCtx.Job.ID)
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
			startSec := 0.0
			if finishedJob.StartTime != nil {
				startSec = *finishedJob.StartTime
			}
			startMin := int(startSec) / 60
			startRemSec := int(startSec) % 60
			startStr := fmt.Sprintf("%d:%02d", startMin, startRemSec)
			endStr := "end"
			if finishedJob.EndTime != nil {
				endMin := int(*finishedJob.EndTime) / 60
				endRemSec := int(*finishedJob.EndTime) % 60
				endStr = fmt.Sprintf("%d:%02d", endMin, endRemSec)
			}
			finFields = append(finFields, notifications.Field{Name: "Trimmed Range", Value: fmt.Sprintf("%s - %s", startStr, endStr), Inline: true})
		}
		// Description excerpt
		if finishedJob.Description != "" {
			desc := finishedJob.Description
			if len(desc) > 300 {
				desc = desc[:297] + "..."
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

	// Save description
	if jobCtx.Job.Description != "" {
		descPath := filepath.Join(outputDir, filenameBase+".description")
		if err := os.WriteFile(descPath, []byte(jobCtx.Job.Description), 0o644); err != nil {
			o.logger.Warn("failed to save description", "err", err)
		} else {
			updates["description_file"] = descPath
		}
	}

	// Copy thumbnail from staging
	for _, ext := range []string{".jpg", ".webp", ".png"} {
		stagingThumb := filepath.Join(jobCtx.StagingDir, "thumbnail"+ext)
		if _, err := os.Stat(stagingThumb); err == nil {
			thumbDst := filepath.Join(outputDir, filenameBase+ext)
			if err := copyFile(stagingThumb, thumbDst); err == nil {
				updates["thumbnail_file"] = thumbDst
			}
			break
		}
	}

	o.db.UpdateJobFields(jobCtx.Job.ID, updates)

	// Send notification
	if o.notifier != nil {
		segInfo := fmt.Sprintf("%d segments", len(segments))
		var qualityLabels []string
		for _, seg := range segments {
			qualityLabels = append(qualityLabels, seg.Quality)
		}
		o.notifier.Send("Download Finished",
			fmt.Sprintf("Successfully archived: %s", jobCtx.Job.Title),
			notifications.TypeSuccess,
			[]notifications.Field{
				{Name: "Segments", Value: segInfo, Inline: true},
				{Name: "Qualities", Value: strings.Join(qualityLabels, " → "), Inline: true},
				{Name: "Total Size", Value: formatFileSize(totalSize), Inline: true},
				{Name: "Duration", Value: formatDurationHuman(time.Duration(totalDuration) * time.Second), Inline: true},
			},
			notifications.SendOptions{
				URL:       jobCtx.Job.URL,
				Thumbnail: jobCtx.Job.ThumbnailURL,
				Event:     "finished",
			},
		)
	}

	o.logger.Info("multi-segment download complete",
		"jobID", jobCtx.Job.ID, "segments", len(segments))
	return nil
}

// ffprobeData holds metadata extracted by ffprobe.
type ffprobeData struct {
	Width       int
	Height      int
	Fps         int
	DurationSec float64
}

// runFFprobe extracts video metadata using ffprobe (B5).
func (o *DownloadOrchestrator) runFFprobe(ctx context.Context, filePath string) *ffprobeData {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, o.muxer.FFprobePath(),
		"-v", "quiet",
		"-print_format", "json",
		"-show_streams",
		"-show_format",
		filePath,
	)

	out, err := cmd.Output()
	if err != nil {
		o.logger.Debug("ffprobe failed", "err", err)
		return nil
	}

	var probe struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			RFrameRate string `json:"r_frame_rate"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}

	if err := json.Unmarshal(out, &probe); err != nil {
		o.logger.Debug("ffprobe parse failed", "err", err)
		return nil
	}

	data := &ffprobeData{}

	// Find video stream
	for _, s := range probe.Streams {
		if s.CodecType == "video" {
			data.Width = s.Width
			data.Height = s.Height
			if s.RFrameRate != "" {
				data.Fps = parseFpsString(s.RFrameRate)
			}
			break
		}
	}

	// Duration
	if probe.Format.Duration != "" {
		data.DurationSec, _ = strconv.ParseFloat(probe.Format.Duration, 64)
	}

	return data
}

// parseFpsString parses ffprobe's r_frame_rate format (e.g. "30/1" or "30000/1001").
func parseFpsString(fps string) int {
	parts := strings.SplitN(fps, "/", 2)
	if len(parts) != 2 {
		v, _ := strconv.Atoi(fps)
		return v
	}
	num, _ := strconv.ParseFloat(parts[0], 64)
	den, _ := strconv.ParseFloat(parts[1], 64)
	if den == 0 {
		return 0
	}
	return int(num / den)
}

// ExecuteTwitch runs the Twitch download pipeline (B3).
// Twitch HLS delivers pre-muxed MPEG-TS, so only one segment downloader is needed.
func (o *DownloadOrchestrator) ExecuteTwitch(ctx context.Context, jobCtx *JobContext, variant *TwitchVariantInfo, isVod bool, twitchChatDl TwitchChatDownloader) error {
	o.logger.Info("starting Twitch download", "jobID", jobCtx.Job.ID, "isVod", isVod)

	// DB listener for cancellation (B6)
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()

	unsubscribe := o.db.OnJobUpdate(func(updatedJob *database.Job) {
		if updatedJob.ID == jobCtx.Job.ID && updatedJob.Status == database.StatusCancelled {
			cancel()
		}
	})
	defer unsubscribe()
	ctx = ctx2

	o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
		"status":              database.StatusDownloading,
		"download_started_at": time.Now().UTC().Format(time.RFC3339),
	})

	// Send "Twitch Download Starting" notification
	if o.notifier != nil {
		dlType := "Live Stream"
		if isVod {
			dlType = "VOD"
		}
		qualityLabel := variant.Name
		if variant.Height > 0 {
			fpsStr := ""
			if variant.FPS > 0 {
				fpsStr = fmt.Sprintf("%g", variant.FPS)
			}
			qualityLabel = fmt.Sprintf("%s (%dp%s)", variant.Name, variant.Height, fpsStr)
		}
		o.notifier.Send("Twitch Download Starting",
			fmt.Sprintf("Beginning download: %s", jobCtx.Job.Title),
			notifications.TypeDownload,
			[]notifications.Field{
				{Name: "Channel", Value: jobCtx.Job.ChannelName, Inline: true},
				{Name: "Quality", Value: qualityLabel, Inline: true},
				{Name: "Type", Value: dlType, Inline: true},
			},
			notifications.SendOptions{
				URL:       jobCtx.Job.URL,
				Thumbnail: jobCtx.Job.ThumbnailURL,
				Event:     "downloading",
			},
		)
	}

	if err := os.MkdirAll(jobCtx.StagingDir, 0o755); err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}

	// Pre-download Twitch thumbnail to staging while stream is still live
	// (Twitch live preview URLs 404 after stream ends, so muxFinalize would be too late)
	if jobCtx.Job.ThumbnailURL != "" {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					o.logger.Error("panic in thumbnail download", "panic", fmt.Sprint(r), "jobID", jobCtx.Job.ID)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			thumbPath := filepath.Join(jobCtx.StagingDir, "thumbnail.jpg")
			if strings.Contains(jobCtx.Job.ThumbnailURL, ".webp") {
				thumbPath = filepath.Join(jobCtx.StagingDir, "thumbnail.webp")
			}
			DownloadFileMinSize(ctx, jobCtx.Job.ThumbnailURL, thumbPath, 1000)
		}()
	}

	// Quality monitoring state (live streams only)
	segmentIndex := 0
	segmentStartTime := time.Now().Unix()
	var segmentMuxWg sync.WaitGroup // tracks background segment mux goroutines
	defer segmentMuxWg.Wait()       // ensure all background muxes finish before returning
	currentQuality := QualityInfo{
		Width:  variant.Width,
		Height: variant.Height,
		FPS:    int(variant.FPS),
		Label:  FormatQualityLabel(variant.Height, int(variant.FPS)),
	}
	qualityChangeCh := make(chan QualityInfo, 1)

	// Start proactive quality monitor for live streams
	var monitorCancel context.CancelFunc
	var twitchMonitor *QualityMonitor
	if !isVod && variant.FetchVariantsFn != nil {
		monitorCtx, mc := context.WithCancel(ctx)
		monitorCancel = mc
		probeFn := o.buildTwitchProbeFn(variant)
		twitchMonitor = NewQualityMonitor(qualityMonitorInterval, currentQuality, probeFn, o.logger)
		go twitchMonitor.Run(monitorCtx, qualityChangeCh)
	}
	defer func() {
		if monitorCancel != nil {
			monitorCancel()
		}
	}()

	// Helper to create HLS downloader for a variant
	createDownloader := func(variantURL string, stagingDir string) (*engine.SegmentDownloader, string) {
		videoPath := filepath.Join(stagingDir, "video_stream")
		dl := engine.NewSegmentDownloader(engine.DownloaderOptions{
			BaseURL:    variantURL,
			OutputFile: videoPath,
			StartSeq:   -1,
			IsHls:      true,
			CheckStreamStatus: func(ctx context.Context) (bool, error) {
				if isVod {
					return false, nil
				}
				info, err := variant.CheckStreamFn(ctx)
				if err != nil {
					return false, err
				}
				return !info, nil // Returns true when stream ended (NOT live)
			},
		})
		return dl, videoPath
	}

	videoDl, videoPath := createDownloader(variant.URL, jobCtx.StagingDir)

	tracker := NewProgressTracker(o.db, jobCtx.Job.ID, o.logger)
	tracker.AttachVideoDownloader(videoDl)

	// Start Twitch chat in parallel
	var chatDone chan struct{}
	if twitchChatDl != nil {
		// Set recording start time for IRC chat offset calculation (matches TS)
		if rta, ok := twitchChatDl.(TwitchRecordingTimeAware); ok {
			rta.SetRecordingStartTime(time.Now().UTC().Format(time.RFC3339))
		}
		// Wire OnProgress for DB updates (matches TS chat progress tracking)
		if irc, ok := twitchChatDl.(*twitch.ChatDownloader); ok {
			irc.OnProgress = func(count int) { tracker.SetChatCount(count) }
		}
		if vod, ok := twitchChatDl.(*twitch.VodChatDownloader); ok {
			vod.OnProgress = func(count int) { tracker.SetChatCount(count) }
		}
		chatDone = make(chan struct{})
		go func() {
			defer close(chatDone)
			defer func() {
				if r := recover(); r != nil {
					o.logger.Error("panic in Twitch chat downloader", "jobID", jobCtx.Job.ID, "panic", fmt.Sprint(r))
				}
			}()
			twitchChatDl.Start(ctx)
		}()
	}

	// Quality-aware download loop
	for {
		if ctx.Err() != nil {
			break
		}

		// Run HLS downloader in goroutine to listen for quality changes
		downloadDone := make(chan error, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					downloadDone <- fmt.Errorf("twitch download panic: %v", r)
				}
			}()
			downloadDone <- videoDl.Start(ctx)
		}()

		var dlErr error
		qualityChanged := false

		if !isVod && variant.FetchVariantsFn != nil {
		awaitTwitchDownload:
			for {
				select {
				case newQ := <-qualityChangeCh:
					if time.Since(time.Unix(segmentStartTime, 0)) < minSegmentDuration {
						// Too soon — don't split. Reset monitor baseline so it
						// re-detects the change once we're past minSegmentDuration.
						o.logger.Debug("quality change ignored (min segment duration)",
							"from", currentQuality.Label, "to", newQ.Label,
							"jobID", jobCtx.Job.ID)
						if twitchMonitor != nil {
							twitchMonitor.UpdateBaseline(currentQuality)
						}
						continue
					}
					qualityChanged = true
					o.logger.Info("proactive quality change detected",
						"from", currentQuality.Label, "to", newQ.Label,
						"jobID", jobCtx.Job.ID)
					videoDl.Cancel()
					dlErr = <-downloadDone
					break awaitTwitchDownload
				case dlErr = <-downloadDone:
					break awaitTwitchDownload
				}
			}
		} else {
			dlErr = <-downloadDone
		}

		if ctx.Err() != nil {
			break
		}

		isQualityLost := errors.Is(dlErr, engine.ErrQualityLost)

		if qualityChanged || isQualityLost {
			segmentEndTime := time.Now().Unix()
			shortSegment := time.Since(time.Unix(segmentStartTime, 0)) < minSegmentDuration

			// Mux this segment in the background so the new download starts immediately.
			if !shortSegment {
				muxIdx := segmentIndex
				muxStart := segmentStartTime
				muxEnd := segmentEndTime
				muxQuality := currentQuality
				muxVideoPath := videoPath
				segmentMuxWg.Add(1)
				go func() {
					defer func() {
						if r := recover(); r != nil {
							o.logger.Error("panic in Twitch mux segment goroutine", "panic", fmt.Sprint(r), "jobID", jobCtx.Job.ID)
						}
					}()
					defer segmentMuxWg.Done()
					muxResult := &DownloadResult{HasVideo: true, VideoPath: muxVideoPath}
					// Use background context — data is already downloaded, let FFmpeg finish
					// even during cancellation to avoid orphaned partial output files.
					seg, muxErr := o.muxSegment(context.Background(), jobCtx, muxIdx, muxStart, muxEnd, muxQuality, muxResult)
					if muxErr != nil {
						o.logger.Error("failed to mux Twitch quality segment", "err", muxErr, "jobID", jobCtx.Job.ID)
					} else if seg != nil {
						o.logger.Info("Twitch quality segment muxed",
							"segment", muxIdx, "quality", muxQuality.Label,
							"file", seg.Filename, "jobID", jobCtx.Job.ID)
					}
				}()
				segmentIndex++
			} else {
				o.logger.Debug("skipping short segment mux",
					"duration", time.Since(time.Unix(segmentStartTime, 0)).Round(time.Second),
					"jobID", jobCtx.Job.ID)
			}

			// Re-fetch master playlist and select new best variant
			variants, fetchErr := variant.FetchVariantsFn(ctx)
			if fetchErr != nil {
				o.logger.Error("failed to refresh Twitch variants", "err", fetchErr, "jobID", jobCtx.Job.ID)
				break
			}

			newVariant := twitch.SelectBestVariant(variants, variant.QualityPref, variant.MaxResolution)
			if newVariant == nil {
				o.logger.Error("no suitable Twitch variant after quality change", "jobID", jobCtx.Job.ID)
				break
			}

			newQuality := QualityInfo{
				Width:  newVariant.Width,
				Height: newVariant.Height,
				FPS:    int(newVariant.FPS),
				Label:  FormatQualityLabel(newVariant.Height, int(newVariant.FPS)),
			}

			o.logger.Info("Twitch quality split",
				"from", currentQuality.Label, "to", newQuality.Label,
				"segment", segmentIndex, "jobID", jobCtx.Job.ID)

			// Create new staging subdirectory
			segStagingDir := filepath.Join(jobCtx.StagingDir, fmt.Sprintf("seg_%d", segmentIndex))
			if err := os.MkdirAll(segStagingDir, 0o755); err != nil {
				o.logger.Error("failed to create segment staging dir", "err", err)
				break
			}

			currentQuality = newQuality
			videoDl, videoPath = createDownloader(newVariant.URL, segStagingDir)
			tracker.AttachVideoDownloader(videoDl)
			segmentStartTime = time.Now().Unix()

			// Update monitor baseline so it doesn't re-detect the same change
			if twitchMonitor != nil {
				select {
				case <-qualityChangeCh:
				default:
				}
				twitchMonitor.UpdateBaseline(currentQuality)
			}

			continue
		}

		// Normal stop — Twitch doesn't need stream-end verification like YouTube
		if dlErr != nil && ctx.Err() == nil {
			o.logger.Error("Twitch HLS download error", "err", dlErr, "jobID", jobCtx.Job.ID)
		}
		break
	}

	tracker.Finalize()

	// Wait for any background segment muxes to finish
	segmentMuxWg.Wait()

	if ctx.Err() != nil {
		// Shutdown: stop chat but preserve staging dir for resume
		if twitchChatDl != nil {
			twitchChatDl.Stop()
		}
		return ctx.Err()
	}

	// If we had quality splits, mux the final segment
	if segmentIndex > 0 {
		segmentEndTime := time.Now().Unix()
		result := &DownloadResult{HasVideo: true, VideoPath: videoPath}
		seg, muxErr := o.muxSegment(ctx, jobCtx, segmentIndex, segmentStartTime, segmentEndTime, currentQuality, result)
		if muxErr != nil {
			o.logger.Error("failed to mux final Twitch quality segment", "err", muxErr, "jobID", jobCtx.Job.ID)
		} else if seg != nil {
			o.logger.Info("final Twitch quality segment muxed",
				"segment", segmentIndex, "quality", currentQuality.Label,
				"file", seg.Filename, "jobID", jobCtx.Job.ID)
		}
	}

	// Signal chat to finish
	if twitchChatDl != nil {
		twitchChatDl.MarkStreamEnded()
		if chatDone != nil {
			chatEndTimer := time.NewTimer(2 * time.Minute)
			select {
			case <-chatDone:
				chatEndTimer.Stop()
			case <-chatEndTimer.C:
				twitchChatDl.Stop()
			}
		}

		// Update chat status
		chatCount := twitchChatDl.MessageCount()
		chatStatus := "finished"
		if chatCount == 0 {
			chatStatus = "unavailable"
		}
		o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
			"chat_status":         chatStatus,
			"total_chat_messages": chatCount,
		})
	}

	// Set stream end time if not already set
	if jobCtx.Job.StreamEndTime == "" {
		endTime := time.Now().UTC().Format(time.RFC3339)
		o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
			"stream_end_time": endTime,
		})
	}

	// Release download slot before muxing
	if o.queue != nil {
		o.queue.ReleaseDownloadSlot(jobCtx.Job.ID)
	}

	// Mux Twitch .ts → .mp4
	dlResult := &DownloadResult{
		HasVideo:  true,
		VideoPath: videoPath,
	}
	return o.muxAndFinalize(ctx, jobCtx, dlResult)
}

// TwitchVariantInfo holds info for a Twitch HLS variant to download.
type TwitchVariantInfo struct {
	URL           string
	Name          string
	Width         int
	Height        int
	FPS           float64
	CheckStreamFn func(ctx context.Context) (bool, error) // Returns true if stream is still live

	// For quality monitoring: re-fetches the master playlist and selects the best variant.
	// Set by the worker for live streams so the orchestrator can detect quality changes.
	FetchVariantsFn   func(ctx context.Context) ([]twitch.TwitchHLSVariant, error)
	QualityPref       string // from channel config, e.g. "1080p60" or "best"
	MaxResolution     int    // from global config
}

// TwitchChatDownloader is the interface for Twitch chat downloaders (IRC or VOD).
type TwitchChatDownloader interface {
	Start(ctx context.Context) error
	Stop()
	MarkStreamEnded()
	MessageCount() int
	IsRunning() bool
}

// TwitchRecordingTimeAware is an optional interface for chat downloaders that support recording start time.
type TwitchRecordingTimeAware interface {
	SetRecordingStartTime(isoString string)
}

// copyFile copies a file from src to dst using streaming I/O.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// extractQualityFromResult derives QualityInfo from a DownloadResult.
// Prefers VideoFormat (set by VOD strategy) but falls back to the direct
// VideoWidth/VideoHeight fields (set by DASH and HLS strategies).
func (o *DownloadOrchestrator) extractQualityFromResult(result *DownloadResult) QualityInfo {
	qi := QualityInfo{Label: "unknown"}
	if result.VideoFormat != nil {
		if result.VideoFormat.Width != nil {
			qi.Width = *result.VideoFormat.Width
		}
		if result.VideoFormat.Height != nil {
			qi.Height = *result.VideoFormat.Height
		}
		if result.VideoFormat.Fps != nil {
			qi.FPS = *result.VideoFormat.Fps
		}
	}
	// Fallback to direct fields (DASH/HLS strategies set these, not VideoFormat)
	if qi.Width == 0 && result.VideoWidth > 0 {
		qi.Width = result.VideoWidth
	}
	if qi.Height == 0 && result.VideoHeight > 0 {
		qi.Height = result.VideoHeight
	}
	if qi.FPS == 0 && result.VideoFps > 0 {
		qi.FPS = result.VideoFps
	}
	if qi.Height > 0 {
		qi.Label = FormatQualityLabel(qi.Height, qi.FPS)
	}
	return qi
}

// buildYouTubeProbeFn creates a quality probe function for YouTube streams.
// The probe re-fetches the DASH manifest and selects the best stream, returning
// the quality that would be selected under current preferences.
func (o *DownloadOrchestrator) buildYouTubeProbeFn(jobCtx *JobContext) func(context.Context) (*QualityInfo, error) {
	maxRes := jobCtx.Config.MaxVideoResolution
	videoItag := jobCtx.Config.VideoItag
	qualityPref := jobCtx.Job.QualityPreference

	return func(ctx context.Context) (*QualityInfo, error) {
		info, err := jobCtx.YT.GetVideoInfo(ctx, jobCtx.Job.VideoID)
		if err != nil {
			return nil, err
		}

		if info.DashManifestURL == "" {
			return nil, fmt.Errorf("no DASH manifest URL")
		}

		// Fetch and parse DASH manifest (simplified — no cipher/PO token needed for probing)
		manifestData, _, err := fetchURL(ctx, info.DashManifestURL)
		if err != nil {
			return nil, fmt.Errorf("fetch DASH manifest: %w", err)
		}

		streams, err := engine.ParseDash(string(manifestData), info.DashManifestURL)
		if err != nil {
			return nil, fmt.Errorf("parse DASH manifest: %w", err)
		}

		var streamInfos []DashStreamInfo
		for _, s := range streams {
			streamInfos = append(streamInfos, DashStreamInfo{
				Itag:      s.Itag,
				MimeType:  s.MimeType,
				Width:     s.Width,
				Height:    s.Height,
				FPS:       s.FPS,
				Bandwidth: s.Bandwidth,
			})
		}

		// Select best video stream using same criteria as the download
		best := SelectBestDashStream(streamInfos, videoItag, maxRes, true, qualityPref)
		if best == nil {
			return nil, fmt.Errorf("no video stream found")
		}

		return &QualityInfo{
			Width:  best.Width,
			Height: best.Height,
			FPS:    best.FPS,
			Label:  FormatQualityLabel(best.Height, best.FPS),
		}, nil
	}
}

// buildTwitchProbeFn creates a quality probe function for Twitch streams.
// The probe re-fetches the master playlist and selects the best variant.
func (o *DownloadOrchestrator) buildTwitchProbeFn(variant *TwitchVariantInfo) func(context.Context) (*QualityInfo, error) {
	return func(ctx context.Context) (*QualityInfo, error) {
		variants, err := variant.FetchVariantsFn(ctx)
		if err != nil {
			return nil, err
		}

		best := twitch.SelectBestVariant(variants, variant.QualityPref, variant.MaxResolution)
		if best == nil {
			return nil, fmt.Errorf("no variant found")
		}

		return &QualityInfo{
			Width:  best.Width,
			Height: best.Height,
			FPS:    int(best.FPS),
			Label:  FormatQualityLabel(best.Height, int(best.FPS)),
		}, nil
	}
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
