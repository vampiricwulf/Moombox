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
	// maxVodRefreshAttempts bounds the post-live URL-refresh loop. Googlevideo
	// URLs live ~6h; a marathon post-live backfill can need several refreshes
	// to finish. 4 attempts ≈ 24h of wall clock, past which the incomplete_tail
	// flag + manual retry take over.
	maxVodRefreshAttempts = 4
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
	muxer        *engine.Muxer
	ffmpegPath   string
	db           *database.Database
	queue        *JobQueue
	cipherSolver *cipher.GojaResolver
	routedCipher cipher.Solver
	potProvider  *bgutils.PotProvider
	notifier     *notifications.Manager
	conn         Connectivity
	logger       logger
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

	// Pre-execution cancellation check — if job was cancelled between queuing and execution.
	// GetJob returns (nil, nil) when the row was deleted out from under us.
	if freshJob, err := o.db.GetJob(jobCtx.Job.ID); err == nil && freshJob != nil && freshJob.Status == database.StatusCancelled {
		if existingChat != nil {
			existingChat.Stop()
		}
		return nil
	}

	// Subscribe to job status changes for cancellation. Job deletion is
	// handled upstream in processJob (OnJobDeleted registered there covers the
	// full job lifecycle including this function); cancel() here propagates
	// through the parent context automatically.
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

	// Resume into the correct part after a restart (mirrors ExecuteTwitch's
	// discovery): a live job restarted after a quality split must not append
	// into staging whose part index is already muxed — finalize skips
	// recorded indices, so everything captured after the restart would be
	// silently dropped when staging is cleaned up. strategyCtx carries the
	// part's staging dir into the downloaders; jobCtx (root) keeps flowing
	// to chat and finalize.
	strategyCtx := jobCtx
	startSegmentIndex := 0
	startPartResumed := false
	if !isVod {
		var resumeDir string
		startSegmentIndex, resumeDir = o.discoverResumeSegment(jobCtx)
		if resumeDir != jobCtx.StagingDir {
			if mkErr := os.MkdirAll(resumeDir, 0o755); mkErr != nil {
				return fmt.Errorf("create resume part staging dir: %w", mkErr)
			}
			segCtx := *jobCtx
			segCtx.StagingDir = resumeDir
			strategyCtx = &segCtx
			o.logger.Info("resuming YouTube job in part staging",
				"part", startSegmentIndex+1, "dir", resumeDir, "jobID", jobCtx.Job.ID)
		}
		// Staged data pre-dating this session: the short-segment rule must
		// not treat the resumed part as a discardable <10s span (the
		// session-local timer doesn't measure the part's true age).
		startPartResumed = discoverStagingMedia(strategyCtx.StagingDir) != nil
	}

	// Select download strategy (A1: pass cipher/pot to strategies)
	var result *DownloadResult
	// waitedForResume is worker-level Tier 2 evidence, finalize-scoped: true
	// when the live loop's noteRefreshFailure evidence-latch is CURRENTLY
	// armed (or gave up directly out of an unresolved one) — this fires
	// whenever resume evidence held on a failed refresh, whether or not
	// shouldWaitForResume itself actually permitted a wait for it (I1 fix:
	// interruption_timeout=0 disables the WAIT, not this latch) — cleared
	// again by runLiveStreamDownload at every later successful refresh, so
	// a broadcast that actually resumed doesn't permanently taint a clean
	// finish. Set by runLiveStreamDownload below; always false on the VOD
	// branch.
	var waitedForResume bool

	strategy, err := selectDownloadStrategy(isVod, videoInfo)
	if err != nil {
		return err
	}
	o.logger.Debug("download strategy selection",
		"isVod", isVod,
		"streamStatus", videoInfo.StreamStatus,
		"hasDash", videoInfo.DashManifestURL != "",
		"hasHls", videoInfo.HlsManifestURL != "",
		"formatCount", len(videoInfo.Formats),
		"strategy", strategy.Kind())
	deps := &StrategyDeps{
		CipherSolver:       o.cipherSolver,
		RoutedCipherSolver: o.routedCipher,
		PotProvider:        o.potProvider,
		IsOnline:           connIsOnline(o.conn),
	}

	// Seed the continuation position from the DB sequences when restart
	// discovery routed into a non-root part dir: a FRESH dir has no resume
	// sidecar, and the strategies' DB fallback resets to segment 0 when the
	// output file is missing — re-downloading the whole broadcast into the
	// new part. last_video_seq/last_audio_seq hold the LAST-WRITTEN seq
	// uniformly (the HLS downloader was normalized to the DASH convention),
	// so +1 is the next segment regardless of which strategy persisted it
	// or which one runs now. A valid sidecar (resuming into an unmuxed
	// part) still takes priority in the engine.
	if strategyCtx != jobCtx {
		vSeed, aSeed := 0, 0
		if jobCtx.Job.LastVideoSeq != nil && *jobCtx.Job.LastVideoSeq > 0 {
			vSeed = *jobCtx.Job.LastVideoSeq + 1
		}
		if jobCtx.Job.LastAudioSeq != nil && *jobCtx.Job.LastAudioSeq > 0 {
			aSeed = *jobCtx.Job.LastAudioSeq + 1
		}
		// YouTube live A/V share one segment timeline, so the two seeds are
		// numerically close in any healthy row. A missing or wildly diverged
		// audio seed means it's stale from a previous session SHAPE — an HLS
		// interlude carries no separate audio downloader (and old builds
		// zeroed the column outright) — and force-starting audio there would
		// rewind hours of audio into this part. Align it to the video seed.
		const avSeedTolerance = 50 // ~2-4 min of segments
		if vSeed > 0 && (aSeed == 0 || aSeed < vSeed-avSeedTolerance || aSeed > vSeed+avSeedTolerance) {
			// Log before overwriting so a rare mis-seed (audio legitimately
			// lagging video past the tolerance after a long outage) is
			// diagnosable from the job log rather than silently realigned.
			o.logger.Info("restart seed: audio seq diverged from video, realigning to video seed",
				"audioSeed", aSeed, "videoSeed", vSeed, "tolerance", avSeedTolerance, "jobID", jobCtx.Job.ID)
			aSeed = vSeed
		}
		strategyCtx.VideoStartSeq = vSeed
		strategyCtx.AudioStartSeq = aSeed
	}

	result, err = strategy.Download(ctx, strategyCtx, videoInfo, deps)

	// The restart seeds are single-use — the initial downloaders above
	// consumed them. Later refreshDownload call sites manage these fields
	// set-then-clear around each refresh; a stale forced seq surviving on
	// strategyCtx (== curCtx in the live loop) would rewind a stall-refresh
	// hours later to the restart position, duplicating media in the part.
	strategyCtx.VideoStartSeq = 0
	strategyCtx.AudioStartSeq = 0

	if err != nil {
		return fmt.Errorf("setup download: %w", err)
	}

	// Check abort immediately after download setup — avoid false progress updates
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Setup progress tracking. The deferred Close covers the exit paths
	// that never reach Finalize (download error, ctx cancel) — without it a
	// pending wait activity would keep the refresh loop rewriting the
	// terminal job's progress line every second for the process lifetime.
	tracker := NewProgressTracker(o.db, jobCtx.Job.ID, o.logger)
	defer tracker.Close()

	o.attachTrackerAndProgress(tracker, result)

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

	// Interruption spec Tier 1 evidence (built once chatDl is resolved so it
	// closes over the final chatDl, not a pre-resolution nil/stale value).
	// Only consulted by the live branch below — VOD downloads never set
	// engine.SegmentDownloader.MayResume, so building this unconditionally
	// here is harmless for the VOD branch (simply unused).
	mayResume := buildMayResume(jobCtx.Interruption, chatDl)

	// Run download loop
	if isVod {
		// VOD: run downloaders, refreshing URLs across the ~6h googlevideo
		// lifetime for post-live jobs whose wall clock outlives one grant.
		result, err = o.runVodDownloadWithRefresh(ctx, jobCtx, result, tracker)

		if err == nil {
			// A zero-byte YouTube finish with a deep HeadSeq is checked for
			// confirmed segment eviction (marathon stream past the ~120h
			// retention window) before the ordinary incomplete-tail
			// accounting below, which assumes a ranging-but-incomplete
			// download rather than a from-the-start failure. Setting err
			// here routes through the normal err!=nil handling further
			// down in this function (chat cleanup, "download: %w" wrap) —
			// the two guards below then no-op for a non-nil err.
			err = o.diagnoseEvictedStart(ctx, jobCtx, videoInfo, result)
		}

		var incomplete bool
		var vSeq, vHead, aSeq, aHead int
		if err == nil {
			// false: the VOD refresh loop never takes the live loop's
			// wait-for-resume branch — that evidence only applies to
			// runLiveStreamDownload's own call site below.
			incomplete, vSeq, vHead, aSeq, aHead = o.finalizeIncompleteTail(jobCtx.Job.ID, result, false)
		}

		// Set 100% progress after VOD download completes (finishVodWithChat equivalent)
		// Include audio percentage and chat count to match TS format. A
		// recording known to be missing its tail (incomplete) must NOT claim
		// 100% here — write an honest progress string instead (FIX 4).
		if err == nil {
			chatCount := 0
			if chatDl != nil {
				chatCount = chatDl.MessageCount()
			}
			var progressStr string
			var percent float64
			if incomplete {
				progressStr, percent = incompleteProgressString(vSeq, vHead, aSeq, aHead, chatCount)
			} else {
				progressStr = "V:100% A:100%"
				percent = 100.0
				if chatCount > 0 {
					progressStr += fmt.Sprintf(" C: %d", chatCount)
				}
			}
			o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
				"progress": progressStr,
				"percent":  percent,
			})
		}
	} else {
		// Live: run with stream-end verification (A2). runLiveStreamDownload
		// returns the FINAL *DownloadResult (see its doc comment) — the loop
		// reassigns result locally on every quality refresh/split, so the
		// pre-call `result` variable would otherwise go stale the moment a
		// single refresh happens.
		result, waitedForResume, err = o.runLiveStreamDownload(ctx, jobCtx, strategyCtx, startSegmentIndex, startPartResumed, videoInfo, result, tracker, mayResume)

		if err == nil {
			// Same eviction guard as the VOD branch (see its call site
			// comment): a marathon channel that's still nominally "live" but
			// has already scrolled its earliest segments out of the ~120h
			// retention window hits this via a different route — the fresh
			// downloader's first-segment hunt exhausts, runLiveStreamDownload
			// treats that as "no progress" and gives up after
			// maxConsecutiveLiveChecks still-live re-verifications, and
			// finalizes cleanly with zero bytes. Setting err here routes
			// through the same err!=nil handling below as every other
			// download failure in this function.
			err = o.diagnoseEvictedStart(ctx, jobCtx, videoInfo, result)
		}

		// Mirror the VOD branch's flag write (see finalizeIncompleteTail):
		// a live YouTube job whose MaxTimeout backstop fires behind head
		// finishes silently otherwise — the staging + resume sidecar it
		// needs for Resume to append the missing tail would be cleaned up
		// like any other cleanly-Finished job. Gated on the SAME err == nil
		// check as the eviction guard above (now possibly re-set by
		// diagnoseEvictedStart) so an eviction error still takes precedence
		// and is never overwritten by a flag write.
		if err == nil {
			o.finalizeIncompleteTail(jobCtx.Job.ID, result, waitedForResume)
		}
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
		// Re-fetch job FIRST: muxAndFinalize set output_file and probed
		// length_seconds after jobCtx.Job was last refreshed, so for a live
		// recording with only StartTime set, the stale row would compute
		// endSec == 0 and silently skip the requested trim.
		freshJob, _ := o.db.GetJob(jobCtx.Job.ID)
		if freshJob != nil && freshJob.Status == database.StatusFinished {
			endSec := 0.0
			if jobCtx.Job.EndTime != nil {
				endSec = *jobCtx.Job.EndTime
			} else if freshJob.LengthSeconds != nil {
				endSec = float64(*freshJob.LengthSeconds)
			}
			if endSec > startSec {
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

// attachTrackerAndProgress attaches the progress tracker to whatever
// downloaders `result` holds. Mechanical extraction of the block that used
// to run once inline in ExecuteWithChat before the isVod/live branch split —
// the VOD-refresh loop needs to re-run it after every rebuilt downloader
// pair, not just at job start.
func (o *DownloadOrchestrator) attachTrackerAndProgress(tracker *ProgressTracker, result *DownloadResult) {
	if result.VideoDownloader != nil {
		tracker.AttachVideoDownloader(result.VideoDownloader)
	}
	if result.AudioDownloader != nil {
		tracker.AttachAudioDownloader(result.AudioDownloader)
	}
}

// shouldRefreshVodDownload is the pure decision for one more refresh pass:
// the run must have ended knowing a tail is missing, must have actually
// advanced (a zero-progress rebuild would spin extraction calls against a
// dead stream), must have budget left, and the fresh extraction must still
// offer segment-addressable formats (a stream that finished processing into
// a true VOD is handled by the flag + retry path instead).
func shouldRefreshVodDownload(behindHead, progressed bool, attempt int, manifestlessStillAvailable bool) bool {
	return behindHead && progressed && attempt < maxVodRefreshAttempts && manifestlessStillAvailable
}

// runVodDownloadWithRefresh runs the VOD-branch downloaders, and — for
// YouTube post-live jobs that finalize behind head (URL expiry, POT decay)
// — re-extracts fresh URLs and continues from the last written segment,
// bounded by maxVodRefreshAttempts. The live branch has had this recovery
// via ErrQualityLost→refreshDownload since the beginning; the VOD branch
// ran exactly once, so any download whose wall clock outlived the ~6h URL
// lifetime was truncated.
func (o *DownloadOrchestrator) runVodDownloadWithRefresh(ctx context.Context, jobCtx *JobContext, result *DownloadResult, tracker *ProgressTracker) (*DownloadResult, error) {
	for attempt := 1; ; attempt++ {
		preVideo, preAudio := downloaderSeq(result.VideoDownloader), downloaderSeq(result.AudioDownloader)
		if err := o.runDownloaders(ctx, result); err != nil {
			return result, err
		}
		if jobCtx.Job.Platform != "youtube" || ctx.Err() != nil {
			return result, nil
		}
		behindHead := downloaderBehindHead(result.VideoDownloader) || downloaderBehindHead(result.AudioDownloader)
		progressed := downloaderSeq(result.VideoDownloader) > preVideo || downloaderSeq(result.AudioDownloader) > preAudio
		if !behindHead {
			return result, nil
		}
		freshInfo, err := jobCtx.YT.GetVideoInfo(ctx, jobCtx.Job.VideoID)
		if err != nil || freshInfo == nil {
			o.logger.Warn("VOD refresh: re-extraction failed; keeping incomplete result", "err", err, "jobID", jobCtx.Job.ID)
			return result, nil
		}
		if !shouldRefreshVodDownload(behindHead, progressed, attempt, HasManifestlessDashFormats(freshInfo.Formats)) {
			return result, nil
		}
		if result.VideoDownloader != nil {
			jobCtx.VideoStartSeq = downloaderSeq(result.VideoDownloader)
		}
		if result.AudioDownloader != nil {
			jobCtx.AudioStartSeq = downloaderSeq(result.AudioDownloader)
		}
		o.logger.Info("VOD refresh: tail incomplete, re-extracting and continuing",
			"attempt", attempt, "videoSeq", jobCtx.VideoStartSeq, "audioSeq", jobCtx.AudioStartSeq, "jobID", jobCtx.Job.ID)
		fresh, err := o.refreshDownload(ctx, jobCtx, freshInfo, false)
		// Single-use seeds, same invariant as every other refreshDownload call
		// site in this file: a stale forced seq surviving on jobCtx would
		// rewind a later attempt to this attempt's start position.
		jobCtx.VideoStartSeq = 0
		jobCtx.AudioStartSeq = 0
		if err != nil {
			o.logger.Warn("VOD refresh: rebuild failed; keeping incomplete result", "err", err, "jobID", jobCtx.Job.ID)
			return result, nil
		}
		if !refreshFormatMatches(result, fresh) {
			// The fresh extraction picked a different itag/resolution/fps than
			// the one already on disk (SelectBestDashStream silently falls
			// back when the pinned itag vanished from the refreshed pool).
			// Appending fresh's segments now would splice a different codec
			// under the existing init — a silent, unrecoverable mixed-codec
			// file. Keep the old (incomplete) result instead; the
			// incomplete_tail flag path above reports the truncation
			// truthfully so Retry can pick it up later.
			o.logger.Warn("refresh selected a different format; keeping incomplete result rather than appending mixed codecs",
				"attempt", attempt, "jobID", jobCtx.Job.ID,
				"oldWidth", result.VideoWidth, "oldHeight", result.VideoHeight, "oldFps", result.VideoFps,
				"freshWidth", fresh.VideoWidth, "freshHeight", fresh.VideoHeight, "freshFps", fresh.VideoFps)
			return result, nil
		}
		o.attachTrackerAndProgress(tracker, fresh)
		result = fresh
	}
}

// downloaderSeq returns d.CurrentSeq(), or 0 for a nil downloader (audio-only
// or video-only jobs leave the other slot nil).
func downloaderSeq(d *engine.SegmentDownloader) int {
	if d == nil {
		return 0
	}
	return d.CurrentSeq()
}

// downloaderBehindHead reports whether d finalized knowing more segments
// were available at the head — false (not true) for a nil downloader.
func downloaderBehindHead(d *engine.SegmentDownloader) bool {
	return d != nil && d.FinalizedBehindHead()
}

// downloaderHead returns d.HeadSeq(), or -1 for a nil downloader — used only
// for diagnostic logging alongside downloaderSeq.
func downloaderHead(d *engine.SegmentDownloader) int {
	if d == nil {
		return -1
	}
	return d.HeadSeq()
}

// downloaderInterrupted reports whether d finalized after latching a Tier-2
// interruption finalize (engine.SegmentDownloader.FinalizedDuringInterruption:
// stallForPossibleResume deferred at least once believing the broadcast
// could resume, then gave up when MayResume flipped false or the
// InterruptionTimeout ceiling expired) — false (not true) for a nil
// downloader.
func downloaderInterrupted(d *engine.SegmentDownloader) bool {
	return d != nil && d.FinalizedDuringInterruption()
}

// computeIncompleteTail: the job is incomplete if EITHER stream finalized
// behind head — video and audio are independent downloaders with
// independent head tracking, and a missing tail on one truncates the mux —
// OR either stream finalized during an interruption (Tier 2): the resume
// sidecar was deliberately preserved for that case, and treating the job as
// complete would let ordinary cleanup discard it before the broadcast gets
// a chance to resume.
func computeIncompleteTail(videoBehind, audioBehind, interrupted bool) bool {
	return videoBehind || audioBehind || interrupted
}

// finalizeIncompleteTail computes and unconditionally persists the
// incomplete_tail flag once a download (VOD or live) has returned with
// err == nil. Shared by both branches of ExecuteWithChat's isVod switch — a
// downloader can only report FinalizedBehindHead()==true on the run that
// naturally finalized: quality/gap splits always Cancel() the prior
// downloader, and a cancelled downloader returns via cancelErr before ever
// reaching the finalize path, so it can never set that flag. That means the
// *DownloadResult each branch holds at its call site — VOD's after its
// refresh loop, live's after runLiveStreamDownload — is always the right,
// final downloader to inspect, for both branches alike.
//
// Returns the computed flag plus the seq/head values used, so a caller that
// also needs to render an honest (non-100%) progress string for an
// incomplete finish (see incompleteProgressString) can reuse them without
// re-deriving.
//
// workerWaitedForResume is worker-level Tier 2 evidence (only ever true
// from the live branch), FINALIZE-scoped: true when the live loop's
// noteRefreshFailure evidence-latch is still unresolved at the moment the
// loop gives up — NOT "fired at some point during this call"
// (runLiveStreamDownload clears it again at every later successful
// refresh, so a broadcast that actually resumed doesn't taint a clean
// finish). Latches whenever resume evidence held on a failed refresh, even
// when shouldWaitForResume itself never permitted an actual wait
// (interruption_timeout=0 disables the WAIT, not Tier-2 preservation — I1
// fix). This is deliberately SEPARATE from downloaderInterrupted's
// engine-latched evidence (FinalizedDuringInterruption) — a live loop that
// repeatedly hit ErrQualityLost/refresh-failure and waited (or, config
// permitting, evidenced) via noteRefreshFailure, then eventually gave up
// when maxConsecutiveLiveChecks exhausted without an intervening
// successful refresh, never once reaches the engine's own
// stallForPossibleResume on any downloader (the loop dies from the
// OUTSIDE, not from an engine-side budget expiry), so
// FinalizedDuringInterruption stays false on every downloader even though
// the job genuinely waited for a resume that never came. Without ORing
// this in, that case would self-clear incomplete_tail and let ordinary
// cleanup discard staging + the resume sidecar right after deliberately
// waiting for exactly the resume that data was preserved for.
func (o *DownloadOrchestrator) finalizeIncompleteTail(jobID string, result *DownloadResult, workerWaitedForResume bool) (incomplete bool, vSeq, vHead, aSeq, aHead int) {
	vSeq = downloaderSeq(result.VideoDownloader)
	vHead = downloaderHead(result.VideoDownloader)
	aSeq = downloaderSeq(result.AudioDownloader)
	aHead = downloaderHead(result.AudioDownloader)
	videoBehind := downloaderBehindHead(result.VideoDownloader)
	audioBehind := downloaderBehindHead(result.AudioDownloader)
	interrupted := downloaderInterrupted(result.VideoDownloader) || downloaderInterrupted(result.AudioDownloader) || workerWaitedForResume
	incomplete = computeIncompleteTail(videoBehind, audioBehind, interrupted)
	// Unconditional write: a retry that completes cleanly clears the flag by
	// writing false through the same path.
	o.db.UpdateJobFields(jobID, map[string]any{"incomplete_tail": incomplete})
	if incomplete {
		o.logger.Warn("recording finished with an unfetched tail — staging preserved; Resume will append the missing segments",
			"jobID", jobID,
			"videoSeq", vSeq, "videoHead", vHead,
			"audioSeq", aSeq, "audioHead", aHead,
			"videoBehindHead", videoBehind, "audioBehindHead", audioBehind,
			"interrupted", interrupted, "workerWaitedForResume", workerWaitedForResume)
	}
	return incomplete, vSeq, vHead, aSeq, aHead
}

// incompleteProgressString builds an honest, non-100% progress string +
// percent for a job whose download finalized with a known-incomplete tail —
// used in place of the flat "V:100% A:100%"/100.0 write so a knowingly-
// truncated recording doesn't show a full bar while muxing. Mirrors the
// DASH-style "(V: x/y A: x/y)" shape ProgressTracker.buildProgressString
// (progress.go) already uses — and that app.js's dashMatch regex already
// parses — omitting the "/head" part when head <= 0 (downloaderHead returns
// -1 for a nil downloader).
func incompleteProgressString(vSeq, vHead, aSeq, aHead, chatCount int) (string, float64) {
	vPart := fmt.Sprintf("%d", vSeq)
	if vHead > 0 {
		vPart = fmt.Sprintf("%d/%d", vSeq, vHead)
	}
	aPart := fmt.Sprintf("%d", aSeq)
	if aHead > 0 {
		aPart = fmt.Sprintf("%d/%d", aSeq, aHead)
	}
	s := fmt.Sprintf("(V: %s A: %s", vPart, aPart)
	if chatCount > 0 {
		s += fmt.Sprintf(" C: %d", chatCount)
	}
	s += ")"

	percent := 0.0
	if vHead > 0 {
		percent = float64(vSeq) / float64(vHead) * 100
	}
	return s, percent
}

// refreshFormatMatches guards against appending a DIFFERENT codec/quality
// into the same staging file across a VOD refresh pass. Each attempt
// re-runs format selection against a freshly re-extracted pool
// (runVodDownloadWithRefresh -> refreshDownload -> DownloadManifestlessDash
// / DownloadDash / DownloadHls -> SelectBestDashStream), which silently
// falls back to a different itag when the previously pinned one has
// disappeared from the pool. The engine's ForceStartSeq append path has no
// codec awareness — it will happily write new-codec fragments under the
// old init, producing a silently corrupt mixed-codec file. Returns false
// (never a match) whenever that can't be ruled out, so the caller's
// "discard fresh, keep the old incomplete result" fallback is the default,
// not the exception.
//
// Identity comparison is necessarily conservative: DownloadResult carries
// no itag/codec identity for the three strategies this loop actually
// refreshes through — DownloadHls, DownloadDash, and DownloadManifestlessDash
// all populate only VideoWidth/VideoHeight/VideoFps (VideoFormat/AudioFormat
// are exclusively set by the whole-file VOD strategy, which never reaches
// this loop — see each strategy's Download function). So video identity is
// compared on the width/height/fps tuple, a workable proxy since a real
// itag swap virtually always changes the encoded resolution or frame rate.
// VideoFormat/AudioFormat.Itag are compared too whenever BOTH sides happen
// to carry them (cheap struct-field reads, no new plumbing) so a future
// strategy that does populate them gets the stronger check for free.
func refreshFormatMatches(old, fresh *DownloadResult) bool {
	if old == nil || fresh == nil {
		return false
	}
	if old.HasVideo != fresh.HasVideo || old.HasAudio != fresh.HasAudio {
		return false
	}
	if old.HasVideo {
		if old.VideoWidth != fresh.VideoWidth || old.VideoHeight != fresh.VideoHeight || old.VideoFps != fresh.VideoFps {
			return false
		}
		if old.VideoFormat != nil && fresh.VideoFormat != nil && old.VideoFormat.Itag != fresh.VideoFormat.Itag {
			return false
		}
	}
	if old.HasAudio && old.AudioFormat != nil && fresh.AudioFormat != nil && old.AudioFormat.Itag != fresh.AudioFormat.Itag {
		return false
	}
	return true
}
