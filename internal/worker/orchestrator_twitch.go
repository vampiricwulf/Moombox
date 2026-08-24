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
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// TwitchVariantInfo holds info for a Twitch HLS variant to download.
type TwitchVariantInfo struct {
	URL    string
	Name   string
	Width  int
	Height int
	FPS    float64
	// StreamID is the broadcast's stable identity (live stream ID or VOD
	// ID), passed to the engine so resume state from a DIFFERENT broadcast
	// is discarded rather than appended into — Twitch weaver URLs carry no
	// extractable identity of their own.
	StreamID      string
	CheckStreamFn func(ctx context.Context) (bool, error) // Returns true if stream is still live
	// RecheckStreamFn returns full stream info — used after a connectivity
	// outage to verify the SAME broadcast is still live (StreamID /
	// StartedAt identity) before the job resumes with a new part, rather
	// than silently attaching to a different broadcast.
	RecheckStreamFn func(ctx context.Context) (*twitch.TwitchStreamInfo, error)

	// For quality monitoring: re-fetches the master playlist and selects the best variant.
	// Set by the worker for live streams so the orchestrator can detect quality changes.
	FetchVariantsFn func(ctx context.Context) ([]twitch.TwitchHLSVariant, error)
	QualityPref     string // from channel config, e.g. "1080p60" or "best"
	MaxResolution   int    // from global config
}

// ExecuteTwitch runs the Twitch download pipeline (B3).
// Twitch HLS delivers pre-muxed MPEG-TS, so only one segment downloader is needed.
// twitchChatDl is the unified ChatSource — concrete type is *twitch.ChatDownloader
// (live IRC) or *twitch.VodChatDownloader (VOD GQL); ExecuteTwitch type-asserts on
// the concrete to pull progress wiring.
func (o *DownloadOrchestrator) ExecuteTwitch(ctx context.Context, jobCtx *JobContext, variant *TwitchVariantInfo, isVod bool, twitchChatDl ChatSource) error {
	o.logger.Info("starting Twitch download", "jobID", jobCtx.Job.ID, "isVod", isVod)

	// Cancellation scaffolding. Three distinct interrupt signals with
	// different lifecycles share it:
	//   - parent ctx death (shutdown, job deleted) — terminal, staging preserved
	//   - user cancel (DB status flips to Cancelled) — terminal
	//   - connectivity loss — NOT terminal: the session pauses and the same
	//     job resumes once the same broadcast is reachable again
	// Outage recovery creates a fresh session context per resume attempt, so
	// the long-lived listeners cancel through a swappable holder instead of
	// capturing one context's cancel func.
	parentCtx := ctx
	var cancelMu sync.Mutex
	var cancelCurrent context.CancelFunc
	callCancel := func() {
		cancelMu.Lock()
		c := cancelCurrent
		cancelMu.Unlock()
		if c != nil {
			c()
		}
	}
	newSession := func() (context.Context, context.CancelFunc) {
		sctx, scancel := context.WithCancel(parentCtx)
		cancelMu.Lock()
		cancelCurrent = scancel
		cancelMu.Unlock()
		return sctx, scancel
	}
	defer callCancel()

	// DB listener for cancellation (B6)
	var userCancelled atomic.Bool
	unsubscribe := o.db.OnJobUpdate(func(updatedJob *database.Job) {
		if updatedJob.ID == jobCtx.Job.ID && updatedJob.Status == database.StatusCancelled {
			userCancelled.Store(true)
			callCancel()
		}
	})
	defer unsubscribe()

	// Register connectivity callback: offline cancels the current session so
	// the downloader stops fetching against a dead network. The session loop
	// below then waits for restoration and resumes the SAME job instead of
	// finalizing it (one job per broadcast).
	var offlineCancelled atomic.Bool
	if o.conn != nil {
		unregisterConn := o.conn.OnStateChange(func(online bool) {
			if !online {
				offlineCancelled.Store(true)
				callCancel()
			}
		})
		defer unregisterConn()
	}

	ctx, _ = newSession()

	// Preserve the original start timestamp when a Downloading job re-enters
	// the pipeline after a daemon restart (mirrors the YouTube path) — the
	// notification "Download Time" fields would otherwise only cover the
	// post-restart span.
	twitchUpdates := map[string]any{
		"status": database.StatusDownloading,
	}
	if jobCtx.Job.DownloadStartedAt == "" {
		twitchUpdates["download_started_at"] = time.Now().UTC().Format(time.RFC3339)
	}
	o.db.UpdateJobFields(jobCtx.Job.ID, twitchUpdates)

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

	// Start proactive quality monitor for live streams. Runs on parentCtx —
	// it must survive outage-driven session restarts (probe failures while
	// offline are tolerated by the monitor's own error handling).
	var monitorCancel context.CancelFunc
	var twitchMonitor *QualityMonitor
	if !isVod && variant.FetchVariantsFn != nil {
		monitorCtx, mc := context.WithCancel(parentCtx)
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

	// Helper to create the HLS downloader for a variant — the single
	// construction point for the engine options, so per-field drift between
	// branches can't happen. startSeq -1 means "resume state / playlist
	// window decides"; forceStartSeq passes an exact orchestrator-captured
	// position (same-quality recovery appends from oldSeq; gap splits seed
	// the next part from CurrentSeq so nothing already written re-downloads).
	createDownloader := func(variantURL, stagingDir string, startSeq int, forceStartSeq bool) (*engine.SegmentDownloader, string) {
		videoPath := filepath.Join(stagingDir, "video_stream")
		dl := engine.NewSegmentDownloader(engine.DownloaderOptions{
			BaseURL:        variantURL,
			OutputFile:     videoPath,
			StartSeq:       startSeq,
			ForceStartSeq:  forceStartSeq,
			IsHls:          true,
			StreamID:       variant.StreamID,
			SegmentWorkers: jobCtx.Config.SegmentWorkers,
			// Deliberately NOT routed through engineInterruptionTimeout
			// (unlike the YouTube live strategies): Twitch downloaders never
			// get MayResume attached (attachMayResume is only ever called
			// from runLiveStreamDownload's attachProgress, a YouTube-only
			// live path), so stallForPossibleResume's nil-MayResume branch
			// makes this value inert either way — 0 and
			// engine.InterruptionNoStall behave identically (return false,
			// never latch) when MayResume is nil.
			InterruptionTimeout: jobCtx.Config.InterruptionTimeout,
			// Twitch live has no DVR: segments that left the playlist window
			// are gone. Stop at real gaps so every part file stays internally
			// gapless — the loop below muxes the part and starts the next.
			StopOnGap: !isVod,
			// Connectivity awareness: the engine rides out offline blips
			// (waits instead of erroring) so a flap right after an outage
			// resume can't burn the retry budget and end the session.
			IsOnline: connIsOnline(o.conn),
			Logger:   newScopedLogger(jobCtx.Logger, "jobID", jobCtx.Job.ID, "stream", "video"),
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

	// refreshBestVariant re-fetches the master playlist and picks the best
	// variant per the job's preference. Variant URLs are short-lived, so
	// every (re)start of a downloader goes through this.
	refreshBestVariant := func(ctx context.Context) (*twitch.TwitchHLSVariant, error) {
		variants, err := variant.FetchVariantsFn(ctx)
		if err != nil {
			return nil, err
		}
		best := twitch.SelectBestVariant(variants, variant.QualityPref, variant.MaxResolution)
		if best == nil {
			return nil, fmt.Errorf("no suitable Twitch variant")
		}
		return best, nil
	}

	// curStagingDir is the staging directory of the CURRENT part: the job
	// root before any split, seg_N afterwards. Same-quality recovery must
	// create its replacement downloader here — using the root dir after a
	// split would append fresh data into part 1's already-muxed file. After
	// a daemon restart, discovery maps existing seg_N dirs + recorded
	// segment rows back to the right part (and never resumes into a part
	// that was already muxed — the appended data would be silently dropped
	// at finalize).
	curStagingDir := jobCtx.StagingDir
	// partResumed marks a part whose staged data PRE-DATES this session
	// (restart resumed into it). The short-segment rule must never treat
	// such a part as a discardable <10s span: segmentStartTime measures the
	// SESSION's view of the part, not its true age — a quality flap in the
	// first 10s after a restart used to classify hours of unmuxed footage
	// as "short" and the same-dir discard would have deleted it.
	partResumed := false
	if !isVod {
		segmentIndex, curStagingDir = o.discoverResumeSegment(jobCtx)
		if curStagingDir != jobCtx.StagingDir {
			if err := os.MkdirAll(curStagingDir, 0o755); err != nil {
				return fmt.Errorf("create resume part staging dir: %w", err)
			}
			o.logger.Info("resuming Twitch job in part staging",
				"part", segmentIndex+1, "dir", curStagingDir, "jobID", jobCtx.Job.ID)
		}
		partResumed = discoverStagingMedia(curStagingDir) != nil
	}

	// currentVariantURL is the variant playlist URL the active downloader was
	// built against — the gap branch falls back to it when the post-gap
	// variant refresh fails (typically because the broadcast just ended:
	// Usher refuses, but the final playlist window still serves the tail).
	currentVariantURL := variant.URL

	videoDl, videoPath := createDownloader(currentVariantURL, curStagingDir, -1, false)

	// Deferred Close mirrors ExecuteWithChat: any exit that skips the
	// post-loop Finalize must still stop the activity refresh loop.
	tracker := NewProgressTracker(o.db, jobCtx.Job.ID, o.logger)
	defer tracker.Close()
	tracker.AttachVideoDownloader(videoDl)

	// irc is the live IRC chat downloader when present (nil for VOD chat or
	// chat-off) — per-part chat rolling below is live-IRC only. Direct
	// concrete assertion per audit reports/worker.md Finding 59.
	irc, _ := twitchChatDl.(*twitch.ChatDownloader)
	chatPathFor := func(stagingDir string) string { return filepath.Join(stagingDir, "chat.json") }

	// startChat launches (or relaunches, after an outage killed the IRC
	// reconnect loop) the chat downloader. Runs on parentCtx so a session
	// restart doesn't tear chat down — shutdown/user-cancel paths Stop() it
	// explicitly.
	var chatDone chan struct{}
	startChat := func() {
		// Wire OnProgress for DB updates via SetOnProgress — avoids the race
		// surface on public-field reassignment (audit reports/worker.md F3).
		if irc != nil {
			irc.SetOnProgress(func(count int) { tracker.SetChatCount(count) })
		}
		if vod, ok := twitchChatDl.(*twitch.VodChatDownloader); ok {
			vod.SetOnProgress(func(count int) { tracker.SetChatCount(count) })
		}
		done := make(chan struct{})
		chatDone = done
		go func() {
			defer close(done)
			defer func() {
				if r := recover(); r != nil {
					o.logger.Error("panic in Twitch chat downloader", "jobID", jobCtx.Job.ID, "panic", fmt.Sprint(r))
				}
			}()
			twitchChatDl.Start(parentCtx)
		}()
	}
	if twitchChatDl != nil {
		if irc != nil {
			// Recording start time for IRC chat offset calculation (matches TS).
			irc.SetRecordingStartTime(time.Now().UTC().Format(time.RFC3339))
			// A job resumed into a later part keeps chat aligned with video:
			// redirect chat output (created at the staging root by the stream
			// processor) to the current part's staging BEFORE Start loads
			// resume state, so it continues that part's file.
			if curStagingDir != jobCtx.StagingDir {
				irc.RollFile(chatPathFor(curStagingDir), time.Now().UTC().Format(time.RFC3339))
			}
		}
		startChat()
	}

	// drainQualityCh resets the monitor baseline after the pipeline handled
	// (or deliberately absorbed) a quality transition, dropping any stale
	// pending signal.
	drainQualityCh := func() {
		if twitchMonitor == nil {
			return
		}
		select {
		case <-qualityChangeCh:
		default:
		}
		twitchMonitor.UpdateBaseline(currentQuality)
	}

	// advanceToNewPart closes the current part and points the pipeline at the
	// next one: rolls the chat file (live IRC only), optionally muxes the
	// closed part in the background — with its chat copied beside it and
	// emote-enriched — and moves segmentIndex/curStagingDir/segmentStartTime
	// forward. The caller creates the next downloader against the new dir.
	//
	// muxCurrent=false reuses the same index for the next dir (the
	// quality-split short-segment rule: the flapping span's media is
	// dropped); gap splits always mux — that data is irreplaceable. When the
	// reused dir IS the current dir, the chat deliberately keeps its file:
	// rolling onto the same path would rewrite it from scratch.
	advanceToNewPart := func(muxCurrent bool, segmentEndTime int64) bool {
		nextIdx := segmentIndex
		if muxCurrent {
			nextIdx = segmentIndex + 1
		}
		nextDir := filepath.Join(jobCtx.StagingDir, fmt.Sprintf("seg_%d", nextIdx))
		if err := os.MkdirAll(nextDir, 0o755); err != nil {
			o.logger.Error("failed to create part staging dir", "err", err, "jobID", jobCtx.Job.ID)
			return false
		}
		var closedChat string
		var enrich func(context.Context)
		if irc != nil && nextDir != curStagingDir {
			closedChat = irc.RollFile(chatPathFor(nextDir), time.Now().UTC().Format(time.RFC3339))
			if closedChat != "" {
				closed := closedChat
				enrich = func(c context.Context) { irc.EnrichFile(c, closed) }
			}
		}
		if muxCurrent {
			// A resumed part's true start pre-dates this session — pass the
			// sentinel so muxSegment derives it from the muxed duration
			// instead of stamping hours of footage with the restart time.
			muxStart := segmentStartTime
			if partResumed {
				muxStart = 0
			}
			muxResult := &DownloadResult{HasVideo: true, VideoPath: videoPath, ChatPath: closedChat}
			o.launchBackgroundSegmentMux(jobCtx, &segmentMuxWg, segmentIndex,
				muxStart, segmentEndTime, currentQuality, muxResult, "twitch", enrich)
		} else if nextDir == curStagingDir {
			// Short-segment discard, reusing the same index/dir: the dropped
			// span's media and its resume sidecar must actually be removed —
			// the engine's resume path would otherwise trust the sidecar
			// (same StreamID, opaque weaver URLs carry no identity) and
			// APPEND the new quality's data onto the discarded old-quality
			// span instead of starting the part fresh.
			oldVideo := filepath.Join(curStagingDir, "video_stream")
			if err := os.Remove(oldVideo); err != nil && !os.IsNotExist(err) {
				o.logger.Warn("failed to remove discarded short-segment media", "err", err, "jobID", jobCtx.Job.ID)
			}
			if err := os.Remove(oldVideo + ".resume.json"); err != nil && !os.IsNotExist(err) {
				o.logger.Warn("failed to remove discarded short-segment resume state", "err", err, "jobID", jobCtx.Job.ID)
			}
		}
		segmentIndex = nextIdx
		curStagingDir = nextDir
		segmentStartTime = time.Now().Unix()
		partResumed = false // the next span is watched from birth
		return true
	}

	// outageFinalize marks "finalize what was captured" exits: the broadcast
	// ended (or became unreachable/another broadcast) while connectivity was
	// down, or a VOD hit an outage. The post-loop dispatch uses it to pick
	// the finalize path over the terminal preserve-staging path — the
	// offlineCancelled flag can't serve that role since the outage handler
	// consumes it on entry.
	outageFinalize := false

	// Session loop: each iteration is one connectivity session. Connectivity
	// loss cancels the inner download via the session context; the outage
	// handler at the bottom waits for restoration and resumes the SAME job
	// against the same broadcast — the job only finalizes when the stream
	// ends, the broadcast changes, or a terminal interrupt arrives.
sessionLoop:
	for {
		// Quality- and gap-aware download loop
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
			isGap := errors.Is(dlErr, engine.ErrGapDetected)
			// Init-segment change (fMP4/CMAF): the transcode restarted and new
			// fragments reference a different moov — handled like a gap split
			// (mux the current part, seed the successor at CurrentSeq) except
			// nothing was lost, so no gap notification is sent.
			isInitChange := errors.Is(dlErr, engine.ErrInitSegmentChanged)

			// FetchVariantsFn guard: only live jobs have it wired. A VOD can
			// still surface ErrQualityLost (playlist 404 with the VOD-shaped
			// CheckStreamStatus reporting "not ended") — without the guard
			// the refresh below would nil-deref; with it, the error falls
			// through to the normal-stop path and captured data still muxes.
			if (qualityChanged || isQualityLost || isGap || isInitChange) && variant.FetchVariantsFn != nil {
				segmentEndTime := time.Now().Unix()
				// A resumed part is never "short": the timer only measures
				// this session's slice of it (see partResumed above).
				shortSegment := !partResumed && time.Since(time.Unix(segmentStartTime, 0)) < minSegmentDuration

				// Re-fetch master playlist FIRST to determine if quality actually changed.
				newVariant, fetchErr := refreshBestVariant(ctx)
				if fetchErr != nil {
					if isGap || isInitChange {
						// Refresh failing right after a gap (or an init-segment
						// change) usually means the broadcast just ended — but
						// the final playlist window keeps serving the tail for
						// a while. Split and let the successor continue on the
						// CURRENT variant URL: a dead URL ends the successor
						// cleanly (empty part, nothing lost), a live one
						// captures the rest of the tail.
						o.logger.Warn("Twitch variant refresh failed after gap/init change; continuing tail on current variant",
							"err", fetchErr, "jobID", jobCtx.Job.ID)
						if isGap {
							o.sendGapSplitNotification(jobCtx, segmentIndex, currentQuality)
						}
						nextSeq := videoDl.CurrentSeq()
						if !advanceToNewPart(true, segmentEndTime) {
							break
						}
						videoDl, videoPath = createDownloader(currentVariantURL, curStagingDir, nextSeq, nextSeq > 0)
						tracker.AttachVideoDownloader(videoDl)
						drainQualityCh()
						continue
					}
					o.logger.Error("failed to refresh Twitch variants", "err", fetchErr, "jobID", jobCtx.Job.ID)
					break
				}
				newQuality := qualityInfoFromVariant(newVariant)

				if isInitChange {
					// Same mechanics as the gap split below — close the part,
					// seed the successor at the exact segment that triggered
					// the change — but the data stream is continuous: the
					// successor's fresh file starts with the NEW init segment
					// and re-requests the segment the engine refused to write.
					o.logger.Info("Twitch init segment changed — starting new part",
						"closedPart", segmentIndex+1, "quality", newQuality.Label, "jobID", jobCtx.Job.ID)
					// A transcode restart often changes quality too; adopting
					// it silently would hide the change from the operator, so
					// fire the same notification the quality path sends. The
					// closed part always muxes here (continuous data).
					if newQuality.Changed(currentQuality) {
						o.sendQualitySplitNotification(jobCtx, "Twitch", currentQuality, newQuality, segmentIndex, true)
					}

					nextSeq := videoDl.CurrentSeq()
					if !advanceToNewPart(true, segmentEndTime) {
						break
					}
					currentQuality = newQuality
					currentVariantURL = newVariant.URL
					videoDl, videoPath = createDownloader(currentVariantURL, curStagingDir, nextSeq, nextSeq > 0)
					tracker.AttachVideoDownloader(videoDl)
					drainQualityCh()
					continue
				}

				if isGap {
					// Unrecoverable gap: segments expired from the CDN before
					// we could fetch them. The current file ends exactly where
					// data stopped — mux it as a finished part (regardless of
					// duration; this data has no second chance) and continue
					// in a new part. The next part is seeded from CurrentSeq
					// (engine left it at the first sequence NOT in the file):
					// window gaps then skip forward to the window start via
					// the empty-file rule, while a stuck-segment gap retries
					// the stuck sequence once more with an empty file and
					// skips it via the same rule if it still fails — either
					// way the gap is recorded exactly once, by the successor.
					o.logger.Warn("Twitch gap split — starting new part",
						"closedPart", segmentIndex+1, "quality", currentQuality.Label, "jobID", jobCtx.Job.ID)
					o.sendGapSplitNotification(jobCtx, segmentIndex, currentQuality)

					nextSeq := videoDl.CurrentSeq()
					if !advanceToNewPart(true, segmentEndTime) {
						break
					}
					currentQuality = newQuality
					currentVariantURL = newVariant.URL
					videoDl, videoPath = createDownloader(currentVariantURL, curStagingDir, nextSeq, nextSeq > 0)
					tracker.AttachVideoDownloader(videoDl)
					drainQualityCh()
					continue
				}

				if !newQuality.Changed(currentQuality) {
					// Same quality — transient error, create fresh downloader in same staging dir.
					// ForceStartSeq ensures it appends to the existing file; if the
					// playlist moved past oldSeq in the meantime, the engine's
					// StopOnGap turns the next iteration into a gap split.
					o.logger.Info("Twitch quality unchanged after re-fetch, continuing download",
						"quality", currentQuality.Label, "jobID", jobCtx.Job.ID)

					currentVariantURL = newVariant.URL
					videoDl, videoPath = createDownloader(currentVariantURL, curStagingDir, videoDl.CurrentSeq(), true)
					tracker.AttachVideoDownloader(videoDl)
					drainQualityCh()
					continue
				}

				// Quality actually changed — split into a new part.
				o.logger.Info("Twitch quality split",
					"from", currentQuality.Label, "to", newQuality.Label,
					"segment", segmentIndex+1, "jobID", jobCtx.Job.ID)

				o.sendQualitySplitNotification(jobCtx, "Twitch", currentQuality, newQuality, segmentIndex, !shortSegment)

				if shortSegment {
					o.logger.Debug("skipping short segment mux",
						"duration", time.Since(time.Unix(segmentStartTime, 0)).Round(time.Second),
						"jobID", jobCtx.Job.ID)
				}
				if !advanceToNewPart(!shortSegment, segmentEndTime) {
					break
				}
				currentQuality = newQuality
				currentVariantURL = newVariant.URL
				videoDl, videoPath = createDownloader(currentVariantURL, curStagingDir, -1, false)
				tracker.AttachVideoDownloader(videoDl)
				drainQualityCh()
				continue
			}

			// Normal stop — Twitch doesn't need stream-end verification like YouTube
			if dlErr != nil && ctx.Err() == nil {
				o.logger.Error("Twitch HLS download error", "err", dlErr, "jobID", jobCtx.Job.ID)
			}
			break
		}

		// Inner loop exited. Anything other than a live-stream connectivity
		// outage — natural end of stream, shutdown, user cancel, job
		// deletion — ends the session loop and proceeds to finalization.
		isOutage := ctx.Err() != nil && parentCtx.Err() == nil &&
			!userCancelled.Load() && offlineCancelled.Load()
		if !isOutage {
			break sessionLoop
		}
		if isVod {
			// VODs keep the pre-existing finalize-on-outage behavior: a VOD
			// has no live edge to chase, and an exact resume is possible on
			// a later retry — holding a download slot through an unbounded
			// wait buys nothing.
			outageFinalize = true
			break sessionLoop
		}

		// ---- Connectivity outage (live): pause, then resume the same job ----
		// The flag is consumed HERE; a re-drop at any later point in the
		// recovery sets it again, and the checks below route back to the
		// wait instead of finalizing mid-broadcast.
		offlineCancelled.Store(false)

		o.logger.Warn("Twitch download paused — connectivity lost; waiting to resume same broadcast",
			"part", segmentIndex+1, "jobID", jobCtx.Job.ID)
		o.sendTwitchSessionNotification(jobCtx, "Twitch Download Paused — Connectivity Lost",
			fmt.Sprintf("Waiting for internet to resume download: %s", jobCtx.Job.Title),
			notifications.TypeWarning, "connectivity_pause", currentQuality, segmentIndex+1)
		// Progress line for the pause: the downloader is dead (session
		// cancelled), so nothing else writes it — without this the job card
		// freezes on the last segment counter for the whole outage. The
		// tracker's refresh loop keeps the elapsed live through the wait and
		// the recovery steps; the first segment after resume clears it.
		tracker.SetWaitActivity(engine.ActivityReconnecting)

		// Recovery: wait for connectivity, then revalidate the broadcast and
		// refresh the variant. Every network step is retried back through
		// the wait when the connection drops again mid-recovery — only an
		// ONLINE failure (broadcast gone, no variant) finalizes.
		var newVariant *twitch.TwitchHLSVariant
	recoverLoop:
		for {
			// Wait on a fresh cancellable session so shutdown / user cancel
			// abort it through the swappable cancel holder. A non-terminal
			// cancellation of the wait itself (another offline transition
			// during a flap rides the same holder) just re-enters the wait.
			for {
				waitCtx, waitCancel := newSession()
				waitErr := o.waitForOnline(waitCtx)
				waitCancel()
				if parentCtx.Err() != nil || userCancelled.Load() {
					break sessionLoop
				}
				if waitErr == nil {
					break // online
				}
			}

			// Back online — is the SAME broadcast still live? A broadcast
			// that ended (or restarted) during the outage finalizes this
			// job; the monitor picks a new broadcast up as its own job.
			stillLive, info := o.recheckTwitchBroadcast(parentCtx, variant)
			if parentCtx.Err() != nil || userCancelled.Load() {
				break sessionLoop
			}
			if o.conn != nil && !o.conn.IsOnline() {
				continue recoverLoop // dropped again mid-recheck
			}
			if !stillLive || !o.sameTwitchBroadcast(jobCtx.Job.ID, info) {
				o.logger.Info("Twitch broadcast ended or changed during outage; finalizing captured parts",
					"jobID", jobCtx.Job.ID)
				outageFinalize = true
				break sessionLoop
			}

			// Variant URLs are short-lived — refresh before resuming.
			var fetchErr error
			newVariant, fetchErr = refreshBestVariant(parentCtx)
			if fetchErr != nil {
				if o.conn != nil && !o.conn.IsOnline() {
					continue recoverLoop // dropped again mid-refresh
				}
				o.logger.Error("failed to refresh Twitch variants after outage", "err", fetchErr, "jobID", jobCtx.Job.ID)
				outageFinalize = true
				break sessionLoop
			}
			break // recovered
		}
		newQuality := qualityInfoFromVariant(newVariant)

		ctx, _ = newSession()
		// Re-check terminal signals AFTER the new session is installed: a
		// user cancel landing during the recheck/refresh above fired
		// callCancel against the already-spent wait session — the flag is
		// the only trace of it. Once cancelCurrent points at the new
		// session, any later cancel reaches the download directly. Cancel
		// the fresh session before breaking so the post-loop dispatch sees
		// a cancelled ctx and takes the terminal path (preserve staging),
		// not the finalize path. A connectivity re-drop in the same window
		// needs no special handling: the engine's IsOnline wiring makes the
		// resumed downloader wait out the blip itself.
		if parentCtx.Err() != nil || userCancelled.Load() {
			callCancel()
			break sessionLoop
		}

		if newQuality.Changed(currentQuality) {
			// Quality moved while we were gone — mixed-codec appends are
			// never safe, so close the current part now.
			if !advanceToNewPart(true, time.Now().Unix()) {
				break sessionLoop
			}
			currentQuality = newQuality
		}
		// Same quality: resume IN the current part's staging dir — the
		// engine's resume + StopOnGap decides between seamless continuation
		// (short outage, playlist still covers our position: zero loss, no
		// split) and ErrGapDetected (the inner loop splits to a new part).
		currentVariantURL = newVariant.URL
		videoDl, videoPath = createDownloader(currentVariantURL, curStagingDir, -1, false)
		tracker.AttachVideoDownloader(videoDl)
		drainQualityCh()
		if twitchChatDl != nil && !twitchChatDl.IsRunning() {
			startChat()
		}

		o.logger.Info("Twitch download resumed after connectivity outage",
			"part", segmentIndex+1, "quality", currentQuality.Label, "jobID", jobCtx.Job.ID)
		o.sendTwitchSessionNotification(jobCtx, "Twitch Download Resumed",
			fmt.Sprintf("Connectivity restored, resuming download: %s", jobCtx.Job.Title),
			notifications.TypeDownload, "connectivity_resume", currentQuality, segmentIndex+1)
	}

	tracker.Finalize()

	// Stream is no longer downloading. Flip status to Muxing now so the
	// UI reflects reality during background-mux Wait + the final-segment
	// mux below — the prior single-flip in finalizeMultiSegmentJob /
	// muxAndFinalize fired AFTER all real mux work, leaving the UI on
	// "Downloading" through ~the entire post-stream phase. Audit
	// reports/worker.md F23 / Q4.
	o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
		"status": database.StatusMuxing,
	})

	// Wait for any background segment muxes to finish
	segmentMuxWg.Wait()

	if ctx.Err() != nil {
		if !outageFinalize || parentCtx.Err() != nil || userCancelled.Load() {
			// Shutdown/user cancel (including either arriving during an
			// outage wait): preserve staging dir for resume
			if twitchChatDl != nil {
				twitchChatDl.Stop()
			}
			return ctx.Err()
		}
		// The stream became unrecoverable while connectivity was down — the
		// broadcast ended/changed (live) or the VOD download was cut off:
		// finalize everything captured up to the outage.
		o.logger.Warn("Twitch download cut off by connectivity outage, finalizing captured data", "jobID", jobCtx.Job.ID)

		desc := fmt.Sprintf("Connectivity lost during download: %s", jobCtx.Job.Title)
		if !isVod {
			desc = fmt.Sprintf("Broadcast ended while connectivity was down: %s", jobCtx.Job.Title)
		}
		o.sendTwitchSessionNotification(jobCtx, "Twitch Download Finalizing — Connectivity Lost",
			desc, notifications.TypeDownload, "connectivity_split", currentQuality, segmentIndex+1)

		// Stop chat (resume state is preserved by the interrupted-exit path;
		// the file on disk is complete through the last flush).
		if twitchChatDl != nil {
			twitchChatDl.Stop()
		}
		// IMPORTANT: Fall through to muxing logic below.
	}

	// After the fall-through from connectivity loss, use a fresh context for muxing
	muxCtx := ctx
	if outageFinalize {
		muxCtx = context.Background()
	}

	// Signal chat to finish BEFORE muxing the final part: the drain flushes
	// the tail of the chat and runs emote enrichment, and the final part's
	// chat file is copied beside its video by the mux below — copying before
	// the drain would ship a truncated, unenriched file.
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

	// Mux the final part with the live-tracked quality/timestamps (with its chat
	// alongside). Indexing note (per audit reports/worker.md F4): segmentIndex
	// was incremented AFTER each background mux, so it now points at the
	// still-unmuxed final part in seg_N. With N splits the total produced count
	// is N+1 (indices 0..N). Also run at index 0 when a restart resumed the job
	// into a seg_N part dir (curStagingDir != root): letting that part fall
	// through to muxUnrecordedSegments would re-derive its quality/times from
	// ffprobe+mtime instead of the precise values tracked here. (A failed mux
	// here is non-fatal — muxUnrecordedSegments still backstops it.)
	if segmentIndex > 0 || curStagingDir != jobCtx.StagingDir {
		segmentEndTime := time.Now().Unix()
		// A part that survived a restart started before this session —
		// segmentStartTime only measures the session's slice of it. Pass the
		// sentinel so muxSegment derives the start from the muxed duration.
		muxStart := segmentStartTime
		if partResumed {
			muxStart = 0
		}
		result := &DownloadResult{HasVideo: true, VideoPath: videoPath}
		if irc != nil {
			result.ChatPath = chatPathFor(curStagingDir)
		}
		seg, muxErr := o.muxSegment(muxCtx, jobCtx, segmentIndex, muxStart, segmentEndTime, currentQuality, result)
		if muxErr != nil {
			o.logger.Error("failed to mux final Twitch part", "err", muxErr, "jobID", jobCtx.Job.ID)
		} else if seg != nil {
			o.logger.Info("final Twitch part muxed",
				"segment", segmentIndex, "quality", currentQuality.Label,
				"file", seg.Filename, "jobID", jobCtx.Job.ID)
		}
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

// qualityInfoFromVariant converts a selected HLS variant into the
// orchestrator's QualityInfo shape — the one place the label formatting and
// FPS rounding for Twitch variants live.
func qualityInfoFromVariant(v *twitch.TwitchHLSVariant) QualityInfo {
	return QualityInfo{
		Width:  v.Width,
		Height: v.Height,
		FPS:    int(v.FPS),
		Label:  FormatQualityLabel(v.Height, int(v.FPS)),
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

		qi := qualityInfoFromVariant(best)
		return &qi, nil
	}
}

// sendTwitchSessionNotification delivers a session-lifecycle notification
// (outage pause/resume, finalize-after-outage) with the shared
// channel/quality/part field shape. No-op when the notifier is nil.
func (o *DownloadOrchestrator) sendTwitchSessionNotification(
	jobCtx *JobContext,
	title, desc string,
	ntype notifications.NotificationType,
	event string,
	quality QualityInfo,
	partNo int,
) {
	if o.notifier == nil {
		return
	}
	o.notifier.Send(title, desc, ntype,
		[]notifications.Field{
			{Name: "Channel", Value: jobCtx.Job.ChannelName, Inline: true},
			{Name: "Quality", Value: quality.Label, Inline: true},
			{Name: "Part", Value: fmt.Sprintf("%d", partNo), Inline: true},
		},
		notifications.SendOptions{
			URL:       jobCtx.Job.URL,
			Thumbnail: jobCtx.Job.ThumbnailURL,
			Event:     event,
		},
	)
}

// discoverResumeSegment maps existing on-disk staging state and recorded
// segment rows to the part index + staging dir a restarted job should
// continue in. Mirrors muxUnrecordedSegments' mapping: the staging root is
// part index 0 (unless seg_0 exists — a short-skipped root), seg_N is index
// N. Never returns a location whose index already has a segment row:
// appending into an already-muxed part's staging file would be silently
// dropped at finalize (the multi-segment path never re-muxes recorded
// indices) — in that case the next free index gets a fresh dir. The caller
// MkdirAlls the returned dir when it isn't the staging root.
func (o *DownloadOrchestrator) discoverResumeSegment(jobCtx *JobContext) (int, string) {
	maxRecorded := -1
	if segs, err := o.db.GetSegments(jobCtx.Job.ID); err == nil {
		for _, s := range segs {
			if s.SegmentIndex > maxRecorded {
				maxRecorded = s.SegmentIndex
			}
		}
	}

	// Highest-index staging location: seg_N dirs win over the root.
	candidateIdx, candidateDir := -1, ""
	if segDirs := stagedSegDirs(jobCtx.StagingDir); len(segDirs) > 0 {
		last := segDirs[len(segDirs)-1]
		candidateIdx, candidateDir = last.idx, last.dir
	}
	if candidateIdx < 0 {
		if discoverStagingMedia(jobCtx.StagingDir) != nil {
			candidateIdx, candidateDir = 0, jobCtx.StagingDir
		} else if maxRecorded < 0 {
			// Fresh job: nothing staged, nothing recorded.
			return 0, jobCtx.StagingDir
		}
	}

	if candidateIdx > maxRecorded {
		// The newest staged part was never muxed — resume into it.
		return candidateIdx, candidateDir
	}
	// Everything staged is already recorded (the restart landed after the
	// background mux persisted the current part) — start the next part.
	next := maxRecorded + 1
	if next == 0 {
		return 0, jobCtx.StagingDir
	}
	return next, filepath.Join(jobCtx.StagingDir, fmt.Sprintf("seg_%d", next))
}

// waitForOnline blocks until the connectivity monitor reports online, or ctx
// dies. Returns immediately when no monitor is wired (nothing to wait on) or
// the monitor already reports online.
func (o *DownloadOrchestrator) waitForOnline(ctx context.Context) error {
	if o.conn == nil {
		return nil
	}
	online := make(chan struct{}, 1)
	unregister := o.conn.OnStateChange(func(isOnline bool) {
		if isOnline {
			select {
			case online <- struct{}{}:
			default:
			}
		}
	})
	defer unregister()
	// Check AFTER subscribing — the state may have flipped between the
	// outage cancel and this call, and checking first would miss the event.
	if o.conn.IsOnline() {
		return nil
	}
	select {
	case <-online:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// recheckTwitchBroadcast fetches stream liveness with brief retries — the
// first requests after connectivity restoration commonly fail while DNS and
// routes settle. Returns (false, nil) when the stream is offline or info
// stays unreachable through the retry budget.
func (o *DownloadOrchestrator) recheckTwitchBroadcast(ctx context.Context, variant *TwitchVariantInfo) (bool, *twitch.TwitchStreamInfo) {
	const attempts = 4
	for i := range attempts {
		if ctx.Err() != nil {
			return false, nil
		}
		switch {
		case variant.RecheckStreamFn != nil:
			info, err := variant.RecheckStreamFn(ctx)
			if err == nil {
				return info != nil && info.IsLive, info
			}
			o.logger.Debug("post-outage stream recheck failed, retrying", "attempt", i+1, "err", err)
		case variant.CheckStreamFn != nil:
			live, err := variant.CheckStreamFn(ctx)
			if err == nil {
				return live, nil
			}
			o.logger.Debug("post-outage stream check failed, retrying", "attempt", i+1, "err", err)
		default:
			return false, nil
		}
		// No sleep after the final attempt — there is no retry left to wait
		// for, and the caller is deciding whether to finalize the job.
		if i < attempts-1 {
			utils.Sleep(ctx, time.Duration(3*(i+1))*time.Second)
		}
	}
	return false, nil
}

// sameTwitchBroadcast reports whether info refers to the broadcast this job
// has been recording, by the shared sameBroadcastStart identity rule against
// the job's fresh stream_start_time. Missing identity on either side trusts
// the match — the engine's resume-identity check still guards the file
// itself.
func (o *DownloadOrchestrator) sameTwitchBroadcast(jobID string, info *twitch.TwitchStreamInfo) bool {
	if info == nil {
		return true
	}
	job, err := o.db.GetJob(jobID)
	if err != nil || job == nil {
		return true
	}
	return sameBroadcastStart(job.StreamStartTime, info.StartedAt)
}
