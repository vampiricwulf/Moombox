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

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// runLiveStreamDownload runs downloaders with stream-end verification loop (A2, B4).
// Supports quality monitoring: when the available quality changes mid-stream,
// the current download segment is muxed and a new download starts at the new quality.
//
// curStart is the JobContext whose StagingDir is the CURRENT part's staging
// (jobCtx itself for a fresh job; a seg_N copy when ExecuteWithChat's restart
// discovery resumed a previously-split job), and startSegmentIndex is that
// part's index — the pair keeps a restarted job from appending into a part
// finalize already recorded. startPartResumed marks staged data pre-dating
// this session: such a part must never be classified "short" by the
// session-local timer (a quality flap right after a restart would otherwise
// skip muxing hours of footage and append mixed-codec data into it).
//
// Returns the FINAL *DownloadResult — every return site echoes back
// whatever `result` currently points to at that moment. This matters
// because the loop locally reassigns `result` on every quality
// refresh/split (`result = refreshResult`), and Go passes the initial
// result argument by value: without returning the final pointer, the
// caller's own result variable would go stale after the first refresh,
// pointing at cancelled/superseded downloaders instead of the ones that
// actually finished the recording. The caller needs the real final
// downloaders to run diagnoseEvictedStart's post-download checks
// (BytesWritten/HeadSeq) against.
//
// mayResume (built once by ExecuteWithChat via buildMayResume) is installed
// as engine.SegmentDownloader.MayResume on every downloader this function
// attaches progress to — the initial result AND every refresh/split
// replacement, since attachProgress is the single call site for both,
// via attachMayResume. VOD downloads never call this function, so
// MayResume is never set there — only live YouTube downloads get the
// interruption stall. attachMayResume itself further gates on
// jobCtx.Config.InterruptionTimeout > 0 — a job with the stall disabled
// (config contract: 0 = finalize must never wait) gets a nil MayResume,
// not a non-nil one with a zero (i.e. unbounded, in engine semantics)
// ceiling.
//
// The second return value, waitedForResume, is worker-level Tier 2
// evidence — FINALIZE-scoped, not history-scoped: true only when the loop
// is CURRENTLY in (or gives up directly out of) an unresolved
// shouldWaitForResume wait. It exists because that branch can retry,
// exhaust maxConsecutiveLiveChecks, and finalize WITHOUT any engine
// downloader ever reaching its own stallForPossibleResume (the loop dies
// from the outside — repeated refresh failures — not from an engine-side
// budget expiry), so FinalizedDuringInterruption would otherwise stay
// false even though the job deliberately waited for a resume. It latches
// true when the wait branch fires and is CLEARED again at every later
// successful refresh (`result = refreshResult`) — a broadcast that
// resumed, or a transient failure that self-healed, must not permanently
// taint a clean multi-hour finish. The caller threads the final value into
// finalizeIncompleteTail so a genuine gave-up-mid-stall wait is not
// silently discarded, without also mis-flagging a recording that finished
// cleanly after an earlier, since-resolved wait.
func (o *DownloadOrchestrator) runLiveStreamDownload(
	ctx context.Context,
	jobCtx *JobContext,
	curStart *JobContext,
	startSegmentIndex int,
	startPartResumed bool,
	videoInfo *youtube.VideoInfo,
	result *DownloadResult,
	tracker *ProgressTracker,
	mayResume func() bool,
) (*DownloadResult, bool, error) {
	var lastSegTime atomic.Int64
	lastSegTime.Store(time.Now().UnixNano())
	var consecutiveLiveChecks atomic.Int32
	// waitedForResume latches true when shouldWaitForResume's branch fires
	// below and clears again at every later successful refresh — see the
	// doc comment above and resumeWaitLatch's own doc comment for the full
	// finalize-scoped rationale.
	var waitedForResume resumeWaitLatch

	// Quality monitoring state
	segmentIndex := startSegmentIndex
	partResumed := startPartResumed
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
		// Quality probes route through ANDROID_VR by default (cookieless,
		// no POT, no watch-page fetch — bypasses the visitor-data rotation
		// that was causing a sidecar mint every 30s). Streams that
		// genuinely need authentication (members-only, age-restricted,
		// login-required) keep using the authenticated GetVideoInfo path
		// since ANDROID_VR will 401 on those. Same predicate
		// observeYouTubeStatusProbe uses to skip arming the interruption
		// signal on these — kept as one function so the two checks cannot
		// drift apart.
		requiresAuthProbe := isAuthWalledPlayability(videoInfo.PlayabilityError)
		probeFn := o.buildYouTubeProbeFn(jobCtx, requiresAuthProbe)
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
			attachMayResume(res.VideoDownloader, mayResume, jobCtx.Config.InterruptionTimeout)
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
			attachMayResume(res.AudioDownloader, mayResume, jobCtx.Config.InterruptionTimeout)
		}
	}

	attachProgress(result)

	// curCtx tracks the JobContext whose StagingDir is the CURRENT part's
	// directory. For a fresh job it is jobCtx itself (root staging); after a
	// quality split — or when restart discovery resumed into a later part —
	// it points at the seg_N copy. Every refresh-created downloader must use
	// curCtx — using the root jobCtx after a split would append fresh data
	// into part 1's already-muxed staging files.
	curCtx := curStart

	for {
		if ctx.Err() != nil {
			return result, waitedForResume.value(), ctx.Err()
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

		qualityChanged, downloadErr := o.awaitDownloadOrQualityChange(
			downloadDone, qualityChangeCh,
			segmentStartTime, currentQuality, monitor,
			func() {
				if result.VideoDownloader != nil {
					result.VideoDownloader.Cancel()
				}
				if result.AudioDownloader != nil {
					result.AudioDownloader.Cancel()
				}
			},
			"youtube", jobCtx.Job.ID,
		)

		if ctx.Err() != nil {
			return result, waitedForResume.value(), ctx.Err()
		}

		// Check for reactive quality loss (download loop returned ErrQualityLost)
		isQualityLost := errors.Is(downloadErr, engine.ErrQualityLost)

		if qualityChanged || isQualityLost {
			segmentEndTime := time.Now().Unix()
			// A resumed part is never "short" — the timer only measures this
			// session's slice of it.
			shortSegment := !partResumed && time.Since(time.Unix(segmentStartTime, 0)) < minSegmentDuration

			// Re-fetch manifest FIRST to determine if quality actually changed.
			// This must happen before muxing so we can skip the split for same-quality.
			freshInfo, err := jobCtx.YT.GetVideoInfo(ctx, jobCtx.Job.VideoID)
			if err != nil {
				o.logger.Error("failed to refresh video info after quality change", "err", err, "jobID", jobCtx.Job.ID)
				return result, waitedForResume.value(), fmt.Errorf("refresh after quality change: %w", err)
			}
			// Interruption spec Tier 1 evidence: a 403-family interruption
			// (handleGoneError) exits the downloader via ErrQualityLost into
			// exactly this refresh as soon as the engine's own
			// CheckStreamStatus consult confirms "still live" — it does not
			// wait for the engine's internal stall the way a 500-family
			// error (handleHTTPError) does. So THIS player-response fetch,
			// immediately below, is very often the freshest look at the
			// signature this job gets; observe it so shouldWaitForResume
			// (right after refreshDownload) sees it.
			jobCtx.Interruption.observe(freshInfo)

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
			curCtx.VideoStartSeq = oldVideoSeq
			curCtx.AudioStartSeq = oldAudioSeq

			refreshResult, refreshErr := o.refreshDownload(ctx, curCtx, freshInfo, result.IsHls)

			// Clear orchestrator seqs so they don't persist to future iterations
			curCtx.VideoStartSeq = 0
			curCtx.AudioStartSeq = 0

			if refreshErr != nil {
				// The interruption signature (live + zero formats) makes
				// EVERY strategy's format selection fail on freshInfo — so a
				// refresh failure here is exactly what an interruption looks
				// like, not just an ended stream. Without this check, that
				// failure used to be treated as "the stream is done" and
				// finalized immediately, discarding a recording that could
				// still resume. shouldWaitForResume falls back to the old
				// immediate-return behavior when the stall is disabled
				// (config InterruptionTimeout <= 0) or there's no resume
				// evidence at all.
				if shouldWaitForResume(refreshErr, jobCtx.Interruption.fresh(), mayResume, jobCtx.Config.InterruptionTimeout) {
					// Finalize-scoped, not history-scoped: this latches
					// true here, but every later SUCCESSFUL refresh
					// (`result = refreshResult`, below and in the
					// still-live verify branch) clears it again — a
					// broadcast that resumed, or a transient failure that
					// self-healed, must not permanently taint a clean
					// multi-hour finish with incomplete_tail=true. If the
					// loop instead gives up for good with no intervening
					// success (this same branch again on a later
					// iteration, or maxConsecutiveLiveChecks exhausting via
					// the still-live verify branch), this stays true —
					// exactly the Tier 2 evidence finalizeIncompleteTail
					// needs for a genuine gave-up-mid-stall finalize.
					waitedForResume.wait()
					o.logger.Warn("refresh failed but the broadcast may resume — waiting instead of ending the recording",
						"err", refreshErr, "jobID", jobCtx.Job.ID)
					tracker.SetWaitActivity(engine.ActivityWaitingResume)
					utils.Sleep(ctx, streamEndVerifyInterval)
					continue
				}
				o.logger.Error("failed to refresh for new quality", "err", refreshErr, "jobID", jobCtx.Job.ID)
				// Return nil to exit the live loop; muxAndFinalize will process
				// whatever video/audio data was captured before the refresh failed.
				return result, waitedForResume.value(), nil
			}

			newQuality := o.extractQualityFromResult(refreshResult)

			if !newQuality.Changed(currentQuality) {
				// Same quality — transient error, not a real quality change.
				// Continue in the same staging directory with fresh downloaders.
				o.logger.Info("quality unchanged after re-fetch, continuing download",
					"quality", currentQuality.Label, "jobID", jobCtx.Job.ID)

				result = refreshResult
				// Finalize-scoped, not history-scoped: a successful refresh
				// means the broadcast resumed (or the earlier failure was
				// transient) — the earlier wait is resolved, so it must not
				// keep taint incomplete_tail on a clean finish that happens
				// later. A later unresolved wait re-latches this on its own;
				// the engine-side latches (FinalizedDuringInterruption)
				// independently carry evidence for a genuine gave-up-mid-
				// stall finalize, so clearing this here doesn't affect that.
				waitedForResume.resolved()

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

			o.sendQualitySplitNotification(jobCtx, "YouTube", currentQuality, newQuality, segmentIndex, !shortSegment)

			// Mux the old segment in the background (unless too short).
			// No preMux: YouTube chat stays a single whole-job file.
			if !shortSegment {
				// A resumed part's true start pre-dates this session — pass
				// the sentinel so muxSegment derives it from the muxed
				// duration instead of stamping it with the restart time.
				muxStart := segmentStartTime
				if partResumed {
					muxStart = 0
				}
				o.launchBackgroundSegmentMux(jobCtx, &segmentMuxWg, segmentIndex,
					muxStart, segmentEndTime, currentQuality, result, "youtube", nil)
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
				return result, waitedForResume.value(), fmt.Errorf("create segment staging dir: %w", err)
			}
			if shortSegment && segStagingDir == curCtx.StagingDir {
				// Short-span discard reusing the same index/dir: physically
				// remove the span's media and resume sidecars, or the engine
				// would resume-append the NEW quality onto the discarded
				// old-quality data — a mixed-codec file under a stale init
				// segment. Mirrors the Twitch discard in advanceToNewPart.
				// video.ts is the YouTube HLS strategy's staging name;
				// video_stream/audio_stream are the DASH family's.
				for _, name := range []string{"video_stream", "audio_stream", "video.ts"} {
					p := filepath.Join(segStagingDir, name)
					if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
						o.logger.Warn("failed to remove discarded short-span media", "file", p, "err", err)
					}
					if err := os.Remove(p + ".resume.json"); err != nil && !os.IsNotExist(err) {
						o.logger.Warn("failed to remove discarded short-span resume state", "file", p, "err", err)
					}
				}
			}

			segJobCtx := *jobCtx
			segJobCtx.StagingDir = segStagingDir
			segJobCtx.VideoStartSeq = oldVideoSeq
			segJobCtx.AudioStartSeq = oldAudioSeq

			refreshResult, refreshErr = o.refreshDownload(ctx, &segJobCtx, freshInfo, result.IsHls)

			if refreshErr != nil {
				o.logger.Error("failed to create downloaders for new quality", "err", refreshErr, "jobID", jobCtx.Job.ID)
				// Return nil to exit the live loop; muxAndFinalize will process
				// whatever video/audio data was captured in the current staging dir.
				return result, waitedForResume.value(), nil
			}

			// The new segment's staging dir is now the current one for all
			// future refreshes. Clear the seqs — they were only for this
			// downloader creation, and a later still-live refresh must not
			// inherit them as a forced start position.
			segJobCtx.VideoStartSeq = 0
			segJobCtx.AudioStartSeq = 0
			curCtx = &segJobCtx

			currentQuality = newQuality
			result = refreshResult
			// See the identical clear + comment at the same-quality
			// success path above — a successful refresh resolves any
			// earlier wait-for-resume.
			waitedForResume.resolved()
			segmentStartTime = time.Now().Unix()
			partResumed = false // the next span is watched from birth

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

			// Wait and retry. Routed through the tracker (not a direct
			// UpdateJobFields) so the refresh loop keeps the elapsed counter
			// live across the 5-minute sleep instead of freezing it.
			tracker.SetWaitActivity(engine.ActivityVerifyingEnd)
			utils.Sleep(ctx, streamEndVerifyInterval)
			continue
		}

		o.logger.Info("YouTube reports stream status", "status", freshInfo.StreamStatus, "jobID", jobCtx.Job.ID)

		// Interruption spec Tier 1 evidence: this is also a live-loop
		// player-response fetch feeding a refreshDownload call (the
		// StreamLive case just below) — see the identical observe() call at
		// the ErrQualityLost site above for why this path needs it too.
		jobCtx.Interruption.observe(freshInfo)

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

			// Through the tracker — see the verify-retry branch above.
			tracker.SetWaitActivity(engine.ActivityVerifyingEnd)

			utils.Sleep(ctx, streamEndVerifyInterval)

			if ctx.Err() != nil {
				return result, waitedForResume.value(), ctx.Err()
			}

			// Cancel old downloaders before refreshing
			if result.VideoDownloader != nil {
				result.VideoDownloader.Cancel()
			}
			if result.AudioDownloader != nil {
				result.AudioDownloader.Cancel()
			}

			// B4: Refresh manifests and create new downloaders
			refreshResult, refreshErr := o.refreshDownload(ctx, curCtx, freshInfo, result.IsHls)

			if refreshErr != nil {
				o.logger.Warn("failed to refresh manifests", "err", refreshErr, "jobID", jobCtx.Job.ID)
				// Through the tracker — see the verify-retry branch above.
				tracker.SetWaitActivity(engine.ActivityVerifyingEnd)
				utils.Sleep(ctx, streamEndVerifyInterval)
				continue
			}

			// Replace the whole result so all fields (Video/AudioFormat, dimensions,
			// IsHls, paths) reflect the refreshed manifest. Previously only the
			// downloader pointers were swapped, leaving stale VideoFormat metadata
			// that downstream muxing could pick up as a fallback.
			result = refreshResult
			// See the identical clear + comment at the isQualityLost/
			// quality-change success paths above — a successful refresh
			// resolves any earlier wait-for-resume.
			waitedForResume.resolved()

			attachProgress(result)

		default:
			// Unexpected status, treat as ended
			o.logger.Warn("unexpected stream status, treating as ended",
				"status", freshInfo.StreamStatus, "jobID", jobCtx.Job.ID)
			goto streamEnded
		}
	}

streamEnded:
	// Stream is no longer downloading. Flip status to Muxing now so the
	// UI reflects reality during background-mux Wait + the final-segment
	// mux below — the prior single-flip in finalizeMultiSegmentJob /
	// muxAndFinalize fired AFTER all real mux work, leaving the UI on
	// "Downloading" through ~the entire post-stream phase. Audit
	// reports/worker.md F23 / Q4.
	o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
		"status": database.StatusMuxing,
	})

	// Wait for background segment muxes to finish before final mux —
	// their DB records must exist before muxAndFinalize calls GetSegments.
	segmentMuxWg.Wait()

	// If we had quality splits, mux the final segment.
	// Indexing note (per audit reports/worker.md F4): each background mux uses
	// segmentIndex BEFORE the increment, then segmentIndex++ leaves the counter
	// pointing at the NEW staging dir (seg_N). When the loop exits with N splits,
	// segmentIndex == N and that points at the still-unmuxed final segment in seg_N.
	// So total segments produced is N+1 (indices 0..N).
	if segmentIndex > 0 {
		segmentEndTime := time.Now().Unix()
		// Same resumed-part rule as the split path above: the session-local
		// start time mis-stamps a part that pre-dates this session.
		muxStart := segmentStartTime
		if partResumed {
			muxStart = 0
		}
		seg, muxErr := o.muxSegment(ctx, jobCtx, segmentIndex, muxStart, segmentEndTime, currentQuality, result)
		if muxErr != nil {
			o.logger.Error("failed to mux final quality segment", "err", muxErr, "jobID", jobCtx.Job.ID)
		} else if seg != nil {
			o.logger.Info("final quality segment muxed",
				"segment", segmentIndex, "quality", currentQuality.Label,
				"file", seg.Filename, "jobID", jobCtx.Job.ID)
		}
	}

	return result, waitedForResume.value(), nil
}

// refreshDownload re-creates downloaders for an in-progress live stream from
// freshly fetched video info, mirroring the initial strategy selection in
// ExecuteWithChat: HLS jobs stay HLS; DASH jobs prefer manifest-free DASH
// whenever the refreshed response ships split adaptive URLs (the primary
// live path since yt-dlp 8c1f07d81), falling back to the manifest-driven
// downloader otherwise. Without the manifestless branch, a manifestless
// job's first transient quality blip or stall refresh would fail with "no
// DASH manifest URL" and end the recording mid-live.
func (o *DownloadOrchestrator) refreshDownload(ctx context.Context, jobCtx *JobContext, freshInfo *youtube.VideoInfo, isHls bool) (*DownloadResult, error) {
	if isHls {
		return DownloadHls(ctx, jobCtx, freshInfo, o.routedCipher, o.cipherSolver, o.potProvider, connIsOnline(o.conn))
	}
	if HasManifestlessDashFormats(freshInfo.Formats) {
		return DownloadManifestlessDash(ctx, jobCtx, freshInfo, o.routedCipher, o.cipherSolver, o.potProvider, connIsOnline(o.conn))
	}
	return DownloadDash(ctx, jobCtx, freshInfo, o.routedCipher, o.cipherSolver, o.potProvider, connIsOnline(o.conn))
}

// buildYouTubeProbeFn creates a quality probe function for YouTube streams.
// The probe re-fetches the DASH manifest and selects the best stream, returning
// the quality that would be selected under current preferences.
//
// requiresAuth: when true, the probe uses GetVideoInfo (full authenticated
// path with cookies + POT) — required for members-only, age-restricted, and
// login-required streams. When false, the probe uses ProbeVideoStatus
// (ANDROID_VR, cookieless, no POT) which is much cheaper and avoids the
// per-probe sidecar mint. Either response works: the common case is a
// split-adaptive format pool (the manifest-free primary path — in-memory
// selection below), with DashManifestURL as the manifest-parsing fallback.
func (o *DownloadOrchestrator) buildYouTubeProbeFn(jobCtx *JobContext, requiresAuth bool) func(context.Context) (*QualityInfo, error) {
	maxRes := jobCtx.Config.MaxVideoResolution
	videoItag := jobCtx.Config.VideoItag
	qualityPref := jobCtx.Job.QualityPreference

	return func(ctx context.Context) (*QualityInfo, error) {
		var info *youtube.VideoInfo
		var err error
		if requiresAuth {
			info, err = jobCtx.YT.GetVideoInfo(ctx, jobCtx.Job.VideoID)
		} else {
			info, err = jobCtx.YT.ProbeVideoStatus(ctx, jobCtx.Job.VideoID)
		}
		if err != nil {
			return nil, err
		}
		// Defensive: GetVideoInfo / ProbeVideoStatus can return (nil, nil)
		// when fetchWatchPage and every Innertube client fail without
		// surfacing a hard error (notably during a context-cancel shutdown
		// race). Treat that as a transient probe error rather than letting
		// the next dereference panic the goroutine.
		if info == nil {
			return nil, fmt.Errorf("probe returned nil info without error")
		}

		// Recovery: if the cookieless probe returned neither a DASH manifest
		// nor a usable split-adaptive format pool, fall back once to the
		// authenticated path so quality changes don't go silently undetected.
		// (A manifest-less response WITH adaptive formats is the normal
		// manifest-free case and needs no fallback.) Auth-required streams
		// already use GetVideoInfo, so the fallback is a no-op for them.
		if info.DashManifestURL == "" && !HasManifestlessDashFormats(info.Formats) && !requiresAuth {
			info, err = jobCtx.YT.GetVideoInfo(ctx, jobCtx.Job.VideoID)
			if err != nil {
				return nil, fmt.Errorf("probe fallback to authenticated: %w", err)
			}
			if info == nil {
				return nil, fmt.Errorf("probe fallback returned nil info")
			}
		}

		// Manifestless DASH path — the primary live path (yt-dlp
		// 8c1f07d81): when the format pool contains split video+audio
		// adaptive entries, pick the best video format directly from
		// the pool. Skips the manifest fetch entirely — the format
		// metadata (width/height/fps) is already in videoInfo.Formats[],
		// so a quality probe on a manifest-free DASH stream is just an
		// in-memory selection (and one fewer MPD round-trip every probe
		// interval).
		if HasManifestlessDashFormats(info.Formats) {
			// Build the pool via partitionManifestlessFormats — NOT an ad-hoc
			// loop — so the probe inherits its ContentLength exclusion. A
			// whole-file format slipping into a contentLength-blind
			// SelectBestDashStream has caused two prior bugs (the premiere
			// disk-runaway class); an unguarded probe pool here would
			// mis-report quality and churn refresh cycles every probe tick.
			videoPool, _ := partitionManifestlessFormats(info.Formats)
			best := SelectBestDashStream(videoPool, videoItag, maxRes, true, qualityPref)
			if best == nil {
				return nil, fmt.Errorf("manifestless DASH probe: no video stream selected")
			}
			return &QualityInfo{
				Width:  best.Width,
				Height: best.Height,
				FPS:    best.FPS,
				Label:  FormatQualityLabel(best.Height, best.FPS),
			}, nil
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
