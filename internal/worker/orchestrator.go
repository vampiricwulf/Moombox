package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/chat"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

const (
	streamEndVerifyInterval  = 5 * time.Minute
	streamSegmentTimeout     = 10 * time.Minute
	maxConsecutiveLiveChecks = 6
	chatWaitTimeout          = 2 * time.Minute // TypeScript uses 2 minutes for all chat waits
	qualityMonitorInterval   = 30 * time.Second
	minSegmentDuration       = 10 * time.Second // Don't split segments shorter than this
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
	// - not_a_stream or VOD without DASH -> direct format download
	// - post_live/VOD WITH DASH -> use DASH segments (TS comment: format URLs may only serve individual segments)
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
