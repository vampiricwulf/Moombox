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
	streamEndVerifyInterval = 5 * time.Minute
	streamSegmentTimeout    = 10 * time.Minute
	// maxConsecutiveLiveChecks pairs with streamEndVerifyInterval (5 min):
	// 6 × 5 min = 30-minute "stream actually ended?" verification window
	// before we accept that the live stream is over (audit reports/worker.md F40).
	maxConsecutiveLiveChecks = 6
	chatWaitTimeout          = 2 * time.Minute
	qualityMonitorInterval   = 30 * time.Second
	minSegmentDuration       = 10 * time.Second // Don't split segments shorter than this
)

// Connectivity is the subset of *connectivity.Monitor the orchestrator uses.
// Defined as an interface so tests can pass a fake and so callers don't have
// to unbundle the monitor's methods into separate func-pointer parameters
// (audit reports/worker.md F54).
type Connectivity interface {
	IsOnline() bool
	OnStateChange(fn func(online bool)) func()
}

// connIsOnline returns a func that calls c.IsOnline, or nil when c is nil.
// Strategies take a func() bool parameter for online checks; this adapts a
// Connectivity value back to that shape without leaking nil pointer calls.
func connIsOnline(c Connectivity) func() bool {
	if c == nil {
		return nil
	}
	return c.IsOnline
}

// DownloadOrchestrator coordinates the full download lifecycle for a job.
type DownloadOrchestrator struct {
	muxer         *engine.Muxer
	ffmpegPath    string
	db            *database.Database
	queue         *JobQueue
	cipherSolver  *cipher.GojaResolver
	routedCipher  cipher.Solver
	potProvider   *bgutils.PotProvider
	notifier      *notifications.Manager
	conn          Connectivity
	logger        logger
}

// NewDownloadOrchestrator creates a new orchestrator.
// routedCs is the composite cipher.Solver (sidecar primary, goja fallback)
// used for sig/n-param URL decryption in download strategies.  cs
// (*GojaResolver) is kept for GetSts, InvalidateSolver, and other
// goja-internal operations.
func NewDownloadOrchestrator(db *database.Database, queue *JobQueue, ffmpegPath string, logger logger, cs *cipher.GojaResolver, routedCs cipher.Solver, pp *bgutils.PotProvider, nm *notifications.Manager, conn Connectivity) *DownloadOrchestrator {
	return &DownloadOrchestrator{
		muxer:        engine.NewMuxer(ffmpegPath, logger),
		ffmpegPath:   ffmpegPath,
		db:           db,
		queue:        queue,
		cipherSolver: cs,
		routedCipher: routedCs,
		potProvider:  pp,
		notifier:     nm,
		conn:         conn,
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

	// Job row vanished — either DeleteJob fired directly or UpdateJobFields
	// observed sql.ErrNoRows on its post-write read-back (delete-during-mux
	// race window). Both paths call notifyJobDeleted, so a single listener
	// here covers every UpdateJobFields call site without opt-in plumbing.
	// Duplicate-fire (DeleteJob + in-flight UpdateJobFields hitting ErrNoRows)
	// is benign: cancel() is idempotent.
	unsubscribeDel := o.db.OnJobDeleted(func(deleted *database.JobDeleted) {
		if deleted.JobID == jobCtx.Job.ID {
			o.logger.Debug("job row deleted; cancelling orchestrator", "jobID", jobCtx.Job.ID)
			cancel()
		}
	})
	defer unsubscribeDel()

	ctx = jobCtx2

	// Update status
	updates := map[string]any{
		"status": database.StatusDownloading,
	}
	if jobCtx.Job.DownloadStartedAt == "" {
		updates["download_started_at"] = time.Now().UTC().Format(time.RFC3339)
	}
	o.db.UpdateJobFields(jobCtx.Job.ID, updates)

	// Send "Download Starting" notification
	if o.notifier != nil {
		dlType := "Live Stream"
		desc := fmt.Sprintf("Now live — beginning download: %s", jobCtx.Job.Title)
		if isVod {
			dlType = "VOD"
			desc = fmt.Sprintf("Beginning download: %s", jobCtx.Job.Title)
		}
		startFields := []notifications.Field{
			{Name: "Channel", Value: jobCtx.Job.ChannelName, Inline: true},
			{Name: "Type", Value: dlType, Inline: true},
		}
		// Include scheduled start time if available
		if jobCtx.Job.StreamStartTime != "" {
			if t, err := time.Parse(time.RFC3339, jobCtx.Job.StreamStartTime); err == nil {
				startFields = append(startFields, notifications.Field{
					Name: "Scheduled For", Value: fmt.Sprintf("<t:%d:f>", t.Unix()), Inline: true,
				})
			}
		}
		// Include format selection details with human-readable labels
		if jobCtx.Job.SelectedVideoItag != nil {
			label := "None"
			if *jobCtx.Job.SelectedVideoItag >= 0 {
				itag := *jobCtx.Job.SelectedVideoItag
				label = fmt.Sprintf("itag %d", itag)
				if videoInfo != nil {
					for _, f := range videoInfo.Formats {
						if f.Itag == itag && f.QualityLabel != "" {
							label = f.QualityLabel
							break
						}
					}
				}
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
		o.notifier.Send("YouTube Download Starting",
			desc,
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
	deps := &StrategyDeps{
		CipherSolver:       o.cipherSolver,
		RoutedCipherSolver: o.routedCipher,
		PotProvider:        o.potProvider,
		IsOnline:           connIsOnline(o.conn),
	}
	var strategy DownloadStrategy
	switch {
	case useDirectVod && len(videoInfo.Formats) > 0:
		strategy = VodStrategy
	case videoInfo.DashManifestURL != "":
		strategy = DashStrategy
	case videoInfo.HlsManifestURL != "":
		strategy = HlsStrategy
	case len(videoInfo.Formats) > 0:
		// Fallback: no DASH/HLS manifest but formats exist — download directly.
		strategy = VodStrategy
	default:
		return fmt.Errorf("no download strategy available")
	}
	result, err = strategy.Download(ctx, jobCtx, videoInfo, deps)

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
		chatDl.SetOnProgress(func(p chat.ChatProgress) {
			tracker.SetChatCount(p.MessageCount)
		})
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

	// Set streamEndTime fallback for live streams only — VODs/premieres that were never
	// live don't have a meaningful stream end time, so leave it empty.
	if !isVod && jobCtx.Job.StreamEndTime == "" {
		endTime := computeStreamEndFallback(jobCtx.Job)
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
				_, trimErr := trimService.CreateTrim(ctx, freshJob, startSec, endSec, nil)
				if trimErr != nil {
					o.logger.Error("post-download trim failed", "err", trimErr, "jobID", jobCtx.Job.ID)
					if o.notifier != nil {
						o.notifier.Send("Trim Failed",
							fmt.Sprintf("Failed to create trim for \"%s\"", jobCtx.Job.Title),
							notifications.TypeError,
							[]notifications.Field{
								{Name: "Channel", Value: jobCtx.Job.ChannelName, Inline: true},
								{Name: "Video ID", Value: jobCtx.Job.VideoID, Inline: true},
								{Name: "Error", Value: trimErr.Error()},
							},
							notifications.SendOptions{
								URL:       jobCtx.Job.URL,
								Thumbnail: jobCtx.Job.ThumbnailURL,
								Event:     "trim_error",
							},
						)
					}
				}
			}
		}
	}

	return nil
}
