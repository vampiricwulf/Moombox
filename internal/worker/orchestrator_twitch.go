package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

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
	FetchVariantsFn func(ctx context.Context) ([]twitch.TwitchHLSVariant, error)
	QualityPref     string // from channel config, e.g. "1080p60" or "best"
	MaxResolution   int    // from global config
}

// TwitchChatDownloader is the interface for Twitch chat downloaders (IRC or VOD).
type TwitchChatDownloader interface {
	Start(ctx context.Context) error
	Stop()
	MarkStreamEnded()
	MessageCount() int
	IsRunning() bool
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

	// Register connectivity callback for immediate bail on offline
	var offlineCancelled atomic.Bool
	var unregisterConn func()
	if o.conn != nil {
		unregisterConn = o.conn.OnStateChange(func(online bool) {
			if !online {
				offlineCancelled.Store(true)
				cancel() // cancel download context
			}
		})
		defer unregisterConn()
	}

	o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
		"status":              database.StatusDownloading,
		"download_started_at": time.Now().UTC().Format(time.RFC3339),
	})

	// Send "Twitch Download Starting" notification
	if o.notifier != nil {
		dlType := "Live Stream"
		desc := fmt.Sprintf("Now live — beginning download: %s", jobCtx.Job.Title)
		if isVod {
			dlType = "VOD"
			desc = fmt.Sprintf("Beginning download: %s", jobCtx.Job.Title)
		}
		qualityLabel := variant.Name
		if variant.Height > 0 {
			fpsStr := ""
			if variant.FPS > 0 {
				fpsStr = fmt.Sprintf("%g", variant.FPS)
			}
			qualityLabel = fmt.Sprintf("%s (%dp%s)", variant.Name, variant.Height, fpsStr)
		}
		startFields := []notifications.Field{
			{Name: "Channel", Value: jobCtx.Job.ChannelName, Inline: true},
			{Name: "Quality", Value: qualityLabel, Inline: true},
			{Name: "Type", Value: dlType, Inline: true},
		}
		if jobCtx.Job.TwitchCategory != "" {
			startFields = append(startFields, notifications.Field{Name: "Category", Value: jobCtx.Job.TwitchCategory, Inline: true})
		}
		o.notifier.Send("Twitch Download Starting",
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
			thumbCtx, thumbCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer thumbCancel()
			thumbPath := filepath.Join(jobCtx.StagingDir, "thumbnail.jpg")
			if strings.Contains(jobCtx.Job.ThumbnailURL, ".webp") {
				thumbPath = filepath.Join(jobCtx.StagingDir, "thumbnail.webp")
			}
			DownloadFileMinSize(thumbCtx, jobCtx.Job.ThumbnailURL, thumbPath, 1000, o.logger)
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
			Logger:     o.logger,
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
		// Set recording start time for IRC chat offset calculation (matches TS).
		// Per audit reports/worker.md Finding 59 — type assertion directly on the
		// concrete type since *twitch.ChatDownloader is the only implementation;
		// no abstraction is providing real value here.
		if irc, ok := twitchChatDl.(*twitch.ChatDownloader); ok {
			irc.SetRecordingStartTime(time.Now().UTC().Format(time.RFC3339))
		}
		// Wire OnProgress for DB updates via SetOnProgress — avoids the race
		// surface on public-field reassignment (audit reports/worker.md F3).
		if irc, ok := twitchChatDl.(*twitch.ChatDownloader); ok {
			irc.SetOnProgress(func(count int) { tracker.SetChatCount(count) })
		}
		if vod, ok := twitchChatDl.(*twitch.VodChatDownloader); ok {
			vod.SetOnProgress(func(count int) { tracker.SetChatCount(count) })
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
	for ctx.Err() == nil {

		// Run HLS downloader in goroutine to listen for quality changes.
		// Compute the error before sending so a panic inside Start is delivered once
		// via the deferred recover without risking a double-send on the buffered channel.
		downloadDone := make(chan error, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					downloadDone <- fmt.Errorf("twitch download panic: %v", r)
				}
			}()
			err := videoDl.Start(ctx)
			downloadDone <- err
		}()

		var dlErr error
		var qualityChanged bool

		if !isVod && variant.FetchVariantsFn != nil {
			qualityChanged, dlErr = o.awaitDownloadOrQualityChange(
				downloadDone, qualityChangeCh,
				segmentStartTime, currentQuality, twitchMonitor,
				func() { videoDl.Cancel() },
				"twitch", jobCtx.Job.ID,
			)
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

			// Re-fetch master playlist FIRST to determine if quality actually changed.
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

			if !newQuality.Changed(currentQuality) {
				// Same quality — transient error, create fresh downloader in same staging dir.
				// ForceStartSeq ensures it appends to the existing file.
				o.logger.Info("Twitch quality unchanged after re-fetch, continuing download",
					"quality", currentQuality.Label, "jobID", jobCtx.Job.ID)

				oldSeq := videoDl.CurrentSeq()
				videoPath = filepath.Join(jobCtx.StagingDir, "video_stream")
				videoDl = engine.NewSegmentDownloader(engine.DownloaderOptions{
					BaseURL:       newVariant.URL,
					OutputFile:    videoPath,
					StartSeq:      oldSeq,
					ForceStartSeq: true,
					IsHls:         true,
					Logger:        o.logger,
					CheckStreamStatus: func(ctx context.Context) (bool, error) {
						if isVod {
							return false, nil
						}
						info, err := variant.CheckStreamFn(ctx)
						if err != nil {
							return false, err
						}
						return !info, nil
					},
				})
				tracker.AttachVideoDownloader(videoDl)

				if twitchMonitor != nil {
					select {
					case <-qualityChangeCh:
					default:
					}
					twitchMonitor.UpdateBaseline(currentQuality)
				}
				continue
			}

			// Quality actually changed — split into a new segment.
			o.logger.Info("Twitch quality split",
				"from", currentQuality.Label, "to", newQuality.Label,
				"segment", segmentIndex+1, "jobID", jobCtx.Job.ID)

			o.sendQualitySplitNotification(jobCtx, "Twitch", currentQuality, newQuality, segmentIndex)

			// Mux the old segment in the background (unless too short)
			if !shortSegment {
				muxResult := &DownloadResult{HasVideo: true, VideoPath: videoPath}
				o.launchBackgroundSegmentMux(jobCtx, &segmentMuxWg, segmentIndex,
					segmentStartTime, segmentEndTime, currentQuality, muxResult, "twitch")
				segmentIndex++
			} else {
				o.logger.Debug("skipping short segment mux",
					"duration", time.Since(time.Unix(segmentStartTime, 0)).Round(time.Second),
					"jobID", jobCtx.Job.ID)
			}

			// Create new staging subdirectory for the different-quality segment
			segStagingDir := filepath.Join(jobCtx.StagingDir, fmt.Sprintf("seg_%d", segmentIndex))
			if err := os.MkdirAll(segStagingDir, 0o755); err != nil {
				o.logger.Error("failed to create segment staging dir", "err", err)
				break
			}

			currentQuality = newQuality
			videoDl, videoPath = createDownloader(newVariant.URL, segStagingDir)
			tracker.AttachVideoDownloader(videoDl)
			segmentStartTime = time.Now().Unix()

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
		if offlineCancelled.Load() {
			// Connectivity loss: mux what we have and finalize
			o.logger.Warn("Twitch download interrupted by connectivity loss, muxing captured data", "jobID", jobCtx.Job.ID)

			if o.notifier != nil {
				o.notifier.Send("Twitch Download Split — Connectivity Lost",
					fmt.Sprintf("Internet connectivity lost during download: %s", jobCtx.Job.Title),
					notifications.TypeDownload,
					[]notifications.Field{
						{Name: "Channel", Value: jobCtx.Job.ChannelName, Inline: true},
						{Name: "Quality", Value: currentQuality.Label, Inline: true},
						{Name: "Segment", Value: fmt.Sprintf("%d", segmentIndex+1), Inline: true},
					},
					notifications.SendOptions{
						URL:       jobCtx.Job.URL,
						Thumbnail: jobCtx.Job.ThumbnailURL,
						Event:     "connectivity_split",
					},
				)
			}

			// Stop chat
			if twitchChatDl != nil {
				twitchChatDl.Stop()
			}
			// IMPORTANT: Fall through to muxing logic below.
		} else {
			// Shutdown/user cancel: preserve staging dir for resume
			if twitchChatDl != nil {
				twitchChatDl.Stop()
			}
			return ctx.Err()
		}
	}

	// After the fall-through from connectivity loss, use a fresh context for muxing
	muxCtx := ctx
	if offlineCancelled.Load() {
		muxCtx = context.Background()
	}

	// If we had quality splits, mux the final segment.
	// Indexing note (per audit reports/worker.md F4): segmentIndex was incremented
	// AFTER each background mux, so it now points at the still-unmuxed final
	// segment in seg_N. With N splits the total produced count is N+1 (indices 0..N).
	if segmentIndex > 0 {
		segmentEndTime := time.Now().Unix()
		result := &DownloadResult{HasVideo: true, VideoPath: videoPath}
		seg, muxErr := o.muxSegment(muxCtx, jobCtx, segmentIndex, segmentStartTime, segmentEndTime, currentQuality, result)
		if muxErr != nil {
			o.logger.Error("failed to mux final Twitch quality segment", "err", muxErr, "jobID", jobCtx.Job.ID)
		} else if seg != nil {
			o.logger.Info("final Twitch quality segment muxed",
				"segment", segmentIndex, "quality", currentQuality.Label,
				"file", seg.Filename, "jobID", jobCtx.Job.ID)
		}
	}

	// Signal chat to finish.
	// Per audit reports/worker.md F5: skip MarkStreamEnded if chat was already
	// Stop()'d above (connectivity-loss path) — racing with the goroutine's
	// shutdown can panic or deadlock inside the chat downloader.
	if twitchChatDl != nil && twitchChatDl.IsRunning() {
		twitchChatDl.MarkStreamEnded()
		if chatDone != nil {
			chatEndTimer := time.NewTimer(chatWaitTimeout)
			select {
			case <-chatDone:
				chatEndTimer.Stop()
			case <-chatEndTimer.C:
				twitchChatDl.Stop()
			}
		}
	}
	if twitchChatDl != nil {
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

	// Set stream end time for live streams only — Twitch VODs don't have a meaningful
	// stream end time from our perspective, so leave it empty.
	if !isVod && jobCtx.Job.StreamEndTime == "" {
		endTime := computeStreamEndFallback(jobCtx.Job)
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
	return o.muxAndFinalize(muxCtx, jobCtx, dlResult)
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
