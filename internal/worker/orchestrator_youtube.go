package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

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
	var lastSegTime atomic.Int64
	lastSegTime.Store(time.Now().UnixNano())
	var consecutiveLiveChecks atomic.Int32

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

	// Track segment activity via progress callbacks.
	// Only reset safety counters when actual bytes are written — catch-up
	// bookend events and deferred progress from failed downloads report
	// Bytes=0, which must NOT reset consecutiveLiveChecks or lastSegTime.
	onSegmentProgress := func(p engine.DownloadProgress) {
		if p.Bytes > 0 {
			lastSegTime.Store(time.Now().UnixNano())
			consecutiveLiveChecks.Store(0)
		}
	}

	attachProgress := func(res *DownloadResult) {
		if res.VideoDownloader != nil {
			tracker.AttachVideoDownloader(res.VideoDownloader)
			origOnProgress := res.VideoDownloader.OnProgress
			res.VideoDownloader.OnProgress = func(p engine.DownloadProgress) {
				onSegmentProgress(p)
				if origOnProgress != nil {
					origOnProgress(p)
				}
			}
		}
		if res.AudioDownloader != nil {
			tracker.AttachAudioDownloader(res.AudioDownloader)
			origOnProgress := res.AudioDownloader.OnProgress
			res.AudioDownloader.OnProgress = func(p engine.DownloadProgress) {
				onSegmentProgress(p)
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

		// Run segment downloaders in a goroutine so we can also listen for quality changes.
		// Compute the error before sending so a panic inside runDownloaders is delivered
		// once via the deferred recover without risking a double-send on the buffered channel.
		downloadDone := make(chan error, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					downloadDone <- fmt.Errorf("runDownloaders panic: %v", r)
				}
			}()
			err := o.runDownloaders(ctx, result)
			downloadDone <- err
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

			// Re-fetch manifest FIRST to determine if quality actually changed.
			// This must happen before muxing so we can skip the split for same-quality.
			freshInfo, err := jobCtx.YT.GetVideoInfo(ctx, jobCtx.Job.VideoID)
			if err != nil {
				o.logger.Error("failed to refresh video info after quality change", "err", err, "jobID", jobCtx.Job.ID)
				return fmt.Errorf("refresh after quality change: %w", err)
			}

			// Cancel old downloaders (safe to call multiple times)
			if result.VideoDownloader != nil {
				result.VideoDownloader.Cancel()
			}
			if result.AudioDownloader != nil {
				result.AudioDownloader.Cancel()
			}

			// Capture old downloader sequences before replacement
			var oldVideoSeq, oldAudioSeq int
			if result.VideoDownloader != nil {
				oldVideoSeq = result.VideoDownloader.CurrentSeq()
			}
			if result.AudioDownloader != nil {
				oldAudioSeq = result.AudioDownloader.CurrentSeq()
			}

			// Create fresh downloaders in the current staging dir to check quality.
			// ForceStartSeq ensures they continue from where the old ones left off.
			jobCtx.VideoStartSeq = oldVideoSeq
			jobCtx.AudioStartSeq = oldAudioSeq

			var refreshResult *DownloadResult
			var refreshErr error
			if result.IsHls {
				refreshResult, refreshErr = DownloadHls(ctx, jobCtx, freshInfo, o.potProvider, o.isOnline)
			} else {
				refreshResult, refreshErr = DownloadDash(ctx, jobCtx, freshInfo, o.cipherSolver, o.potProvider, o.isOnline)
			}

			// Clear orchestrator seqs so they don't persist to future iterations
			jobCtx.VideoStartSeq = 0
			jobCtx.AudioStartSeq = 0

			if refreshErr != nil {
				o.logger.Error("failed to refresh for new quality", "err", refreshErr, "jobID", jobCtx.Job.ID)
				// Return nil to exit the live loop; muxAndFinalize will process
				// whatever video/audio data was captured before the refresh failed.
				return nil
			}

			newQuality := o.extractQualityFromResult(refreshResult)

			if !newQuality.Changed(currentQuality) {
				// Same quality — transient error, not a real quality change.
				// Continue in the same staging directory with fresh downloaders.
				o.logger.Info("quality unchanged after re-fetch, continuing download",
					"quality", currentQuality.Label, "jobID", jobCtx.Job.ID)

				result = refreshResult

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

			// Quality actually changed — split into a new segment.
			o.logger.Info("quality split",
				"from", currentQuality.Label, "to", newQuality.Label,
				"segment", segmentIndex+1, "jobID", jobCtx.Job.ID)

			if o.notifier != nil {
				o.notifier.Send("YouTube Quality Split",
					fmt.Sprintf("Stream quality changed during download: %s", jobCtx.Job.Title),
					notifications.TypeDownload,
					[]notifications.Field{
						{Name: "Channel", Value: jobCtx.Job.ChannelName, Inline: true},
						{Name: "From", Value: currentQuality.Label, Inline: true},
						{Name: "To", Value: newQuality.Label, Inline: true},
						{Name: "Segment", Value: fmt.Sprintf("%d", segmentIndex+1), Inline: true},
					},
					notifications.SendOptions{
						URL:       jobCtx.Job.URL,
						Thumbnail: jobCtx.Job.ThumbnailURL,
						Event:     "quality_split",
					},
				)
			}

			// Mux the old segment in the background (unless too short)
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

			// Create downloaders in the NEW staging dir. The refreshResult created above points
			// to the old staging dir and was used only to check quality — discard it.
			segStagingDir := filepath.Join(jobCtx.StagingDir, fmt.Sprintf("seg_%d", segmentIndex))
			if err := os.MkdirAll(segStagingDir, 0o755); err != nil {
				return fmt.Errorf("create segment staging dir: %w", err)
			}

			segJobCtx := *jobCtx
			segJobCtx.StagingDir = segStagingDir
			segJobCtx.VideoStartSeq = oldVideoSeq
			segJobCtx.AudioStartSeq = oldAudioSeq

			if result.IsHls {
				refreshResult, refreshErr = DownloadHls(ctx, &segJobCtx, freshInfo, o.potProvider, o.isOnline)
			} else {
				refreshResult, refreshErr = DownloadDash(ctx, &segJobCtx, freshInfo, o.cipherSolver, o.potProvider, o.isOnline)
			}

			if refreshErr != nil {
				o.logger.Error("failed to create downloaders for new quality", "err", refreshErr, "jobID", jobCtx.Job.ID)
				// Return nil to exit the live loop; muxAndFinalize will process
				// whatever video/audio data was captured in the current staging dir.
				return nil
			}

			currentQuality = newQuality
			result = refreshResult
			segmentStartTime = time.Now().Unix()

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
		timeSinceLastSeg := time.Since(time.Unix(0, lastSegTime.Load()))
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
			checks := consecutiveLiveChecks.Add(1)
			if checks >= maxConsecutiveLiveChecks {
				o.logger.Warn("YouTube reported live too many times with no segments, forcing end",
					"checks", checks, "jobID", jobCtx.Job.ID)
				goto streamEnded
			}

			o.logger.Info("stream still live, refreshing manifests",
				"check", checks, "max", maxConsecutiveLiveChecks, "jobID", jobCtx.Job.ID)

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
				refreshResult, refreshErr = DownloadHls(ctx, jobCtx, freshInfo, o.potProvider, o.isOnline)
			} else {
				refreshResult, refreshErr = DownloadDash(ctx, jobCtx, freshInfo, o.cipherSolver, o.potProvider, o.isOnline)
			}

			if refreshErr != nil {
				o.logger.Warn("failed to refresh manifests", "err", refreshErr, "jobID", jobCtx.Job.ID)
				utils.Sleep(ctx, streamEndVerifyInterval)
				continue
			}

			// Replace the whole result so all fields (Video/AudioFormat, dimensions,
			// IsHls, paths) reflect the refreshed manifest. Previously only the
			// downloader pointers were swapped, leaving stale VideoFormat metadata
			// that downstream muxing could pick up as a fallback.
			result = refreshResult

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
