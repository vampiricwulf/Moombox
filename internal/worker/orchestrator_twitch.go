package worker

import (
	"context"
	"errors"
	"fmt"
	"math"
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
	QualityPref     string // the job's twitch_quality_preference, e.g. "1080p60" or "best"
	MaxResolution   int    // from global config
	Prefer60fps     bool   // from global config (prefer_60fps)
}

// newTwitchVariantInfo builds the variant a Twitch capture runs on from the
// one the stream processor selected, carrying the selection inputs every
// re-selection during the capture repeats (selectFrom) — the job's
// twitch_quality_preference and the job context's snapshot of
// max_video_resolution and prefer_60fps. It read quality_preference until D-T9
// gave the Twitch preference a column of its own, while the capture start read
// twitch_quality: two columns for one input, which agreed only until the
// stream start overwrote one of them.
// The live-only closures are the caller's to wire.
func newTwitchVariantInfo(job *database.Job, v *twitch.TwitchHLSVariant, cfg *JobConfig) *TwitchVariantInfo {
	return &TwitchVariantInfo{
		URL:           v.URL,
		Name:          v.Name,
		Width:         v.Width,
		Height:        v.Height,
		FPS:           v.FPS,
		QualityPref:   job.TwitchQualityPreference,
		MaxResolution: cfg.MaxVideoResolution,
		Prefer60fps:   cfg.Prefer60fps,
	}
}

// selectFrom picks this capture's variant out of a fresh master playlist, by
// the same rule and the same inputs the capture started on — the quality
// probe and refreshBestVariant both call it, so a re-selection cannot drift
// from the first selection on any of the three.
func (v *TwitchVariantInfo) selectFrom(variants []twitch.TwitchHLSVariant) *twitch.TwitchHLSVariant {
	return twitch.SelectBestVariant(variants, v.QualityPref, v.MaxResolution, v.Prefer60fps)
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
	// The ID is read once, here: the callback runs on whichever goroutine
	// wrote the row, and muxAndFinalize replaces jobCtx.Job with a fresh read
	// while those writes continue, so reading jobCtx.Job inside it was a race.
	jobID := jobCtx.Job.ID
	unsubscribe := o.db.OnJobUpdate(func(updatedJob *database.Job) {
		if updatedJob.ID == jobID && updatedJob.Status == database.StatusCancelled {
			userCancelled.Store(true)
			callCancel()
		}
	})
	defer unsubscribe()

	// Register connectivity callback: offline cancels the current session so
	// the downloader stops fetching against a dead network. The session loop
	// below then waits for restoration and resumes the SAME job instead of
	// finalizing it (one job per broadcast).
	//
	// Live only. A VOD has no live edge to lose and no broadcast to
	// re-verify, so the engine's IsOnline wiring waits an outage out in place
	// and the download carries on from where it stopped, as on the YouTube
	// paths. Cancelling it ended the job in Error, and the only way on from
	// there — Retry — downloaded the whole VOD again.
	var offlineCancelled atomic.Bool
	// finalizing switches the offline cancel off once the session loop is
	// over: muxing and the chat drain need no network, and a blip there used
	// to cancel the final mux — a complete recording landing in Error, a
	// split job finishing without its last part.
	var finalizing atomic.Bool
	if o.conn != nil && !isVod {
		unregisterConn := o.conn.OnStateChange(func(online bool) {
			if !online && !finalizing.Load() {
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
		desc := fmt.Sprintf("Now live — beginning download: %s", notifications.EscapeMarkdown(jobCtx.Job.Title))
		if isVod {
			dlType = "VOD"
			desc = fmt.Sprintf("Beginning download: %s", notifications.EscapeMarkdown(jobCtx.Job.Title))
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
			{Name: "Channel", Value: notifications.EscapeMarkdown(jobCtx.Job.ChannelName), Inline: true},
			{Name: "Quality", Value: qualityLabel, Inline: true},
			{Name: "Type", Value: dlType, Inline: true},
		}
		if jobCtx.Job.TwitchCategory != "" {
			startFields = append(startFields, notifications.Field{Name: "Category", Value: notifications.EscapeMarkdown(jobCtx.Job.TwitchCategory), Inline: true})
		}
		// The YouTube twin of this send (orchestrator.go) carries the same
		// three identity fields off the same mapper, for the same reason:
		// `downloading` is a lifecycle event and Manager.planLifecycle keys
		// on Opts.JobID. NotifyFacts leaves a Twitch row's URL alone — the
		// watch-URL fallback is YouTube-only, because there is nothing to
		// guess here.
		f := NotifyFacts(jobCtx.Job)
		o.notifier.Send("Twitch Download Starting",
			desc,
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

	// Pre-download Twitch thumbnail to staging while stream is still live
	// (Twitch live preview URLs 404 after stream ends, so muxFinalize would be too late)
	if jobCtx.Job.ThumbnailURL != "" {
		// Read here, not in the goroutine: it is not waited for, and
		// muxAndFinalize replaces jobCtx.Job when a short VOD gets there first.
		thumbURL, stagingDir := jobCtx.Job.ThumbnailURL, jobCtx.StagingDir
		go func() {
			defer func() {
				if r := recover(); r != nil {
					o.logger.Error("panic in thumbnail download", "panic", fmt.Sprint(r), "jobID", jobID)
				}
			}()
			thumbCtx, thumbCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer thumbCancel()
			thumbPath := filepath.Join(stagingDir, "thumbnail.jpg")
			if strings.Contains(thumbURL, ".webp") {
				thumbPath = filepath.Join(stagingDir, "thumbnail.webp")
			}
			DownloadFileMinSize(thumbCtx, thumbURL, thumbPath, 1000, o.logger)
		}()
	}

	// Quality monitoring state (live streams only)
	segmentIndex := 0
	segmentStartTime := time.Now().Unix()
	var segmentMuxWg sync.WaitGroup // tracks background segment mux goroutines
	defer segmentMuxWg.Wait()       // ensure all background muxes finish before returning
	// Rounded, not truncated: a 59.94 variant is "1080p60" here as it is in
	// the playlist's own naming (see qualityInfoFromVariant).
	fps := int(math.Round(variant.FPS))
	currentQuality := QualityInfo{
		Width:  variant.Width,
		Height: variant.Height,
		FPS:    fps,
		Label:  FormatQualityLabel(variant.Height, fps),
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

	// irc is the live IRC chat downloader when present (nil for VOD chat or
	// chat-off) — per-part chat rolling below is live-IRC only. Direct
	// concrete assertion per audit reports/worker.md Finding 59.
	irc, _ := twitchChatDl.(*twitch.ChatDownloader)

	chatPathFor := func(stagingDir string) string { return filepath.Join(stagingDir, "chat.json") }

	// Helper to create the HLS downloader for a variant — the single
	// construction point for the engine options, so per-field drift between
	// branches can't happen. startSeq -1 means "resume state / playlist
	// window decides"; forceStartSeq passes an exact orchestrator-captured
	// position (same-quality recovery appends from oldSeq; gap splits seed
	// the next part from CurrentSeq so nothing already written re-downloads).
	//
	// The live IRC chat's part base is pinned here too (D-T8): a downloader
	// that starts its part's video file reports the program date-time of the
	// first segment it writes, and that is the part's chat base — the chat
	// file of the SAME part directory, so a late report from a downloader
	// since replaced can only reach the part it belonged to.
	createDownloader := func(variantURL, stagingDir string, startSeq int, forceStartSeq bool) (*engine.SegmentDownloader, string) {
		videoPath := filepath.Join(stagingDir, "video_stream")
		var onFirstSegment func(time.Time)
		if irc != nil {
			partChat := chatPathFor(stagingDir)
			onFirstSegment = func(pdt time.Time) { irc.SettlePartBase(partChat, pdt) }
		}
		dl := engine.NewSegmentDownloader(engine.DownloaderOptions{
			OnFirstSegment: onFirstSegment,
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
		best := variant.selectFrom(variants)
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

	// recordVariant keeps twitch_quality naming the variant the capture is
	// recording (D-T9). The stream processor wrote the start's pick; every
	// adoption of a refreshed variant below — a quality split, a gap or
	// init-change split, a same-quality restart, a post-outage resume — goes
	// through here, and only a different name is written. A quality split
	// used to leave the row naming the variant the job had split away from.
	recordedVariant := variant.Name
	recordVariant := func(v *twitch.TwitchHLSVariant) {
		if v.Name == recordedVariant {
			return
		}
		recordedVariant = v.Name
		o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{"twitch_quality": v.Name})
	}

	videoDl, videoPath := createDownloader(currentVariantURL, curStagingDir, -1, false)

	// Deferred Close mirrors ExecuteWithChat: any exit that skips the
	// post-loop Finalize must still stop the activity refresh loop.
	tracker := NewProgressTracker(o.db, jobCtx.Job.ID, o.logger, jobCtx.Config.ProgressInterval)
	defer tracker.Close()
	tracker.AttachVideoDownloader(videoDl)

	// Register the live IRC downloader so a Twitch credential change can reach
	// it MID-JOB (Arc 10 R5). Until now this object was reachable only through
	// this goroutine's call stack.
	//
	// Deferred here rather than at any of the Stop / MarkStreamEnded sites
	// below: this defer covers EVERY exit — finish, error, user cancel,
	// shutdown, connectivity finalize and panic — so there is no exit path
	// that has to remember to unregister, which is exactly how a registry
	// keyed to long-running jobs leaks. Same shape as the OnJobUpdate and
	// OnStateChange unsubscribes above.
	//
	// Only the IRC downloader. The VOD chat downloader re-reads its bearer
	// token per GQL page, so it needs no signal.
	if irc != nil {
		unregisterChat := o.twitchChats.add(irc)
		defer unregisterChat()
	}

	// startChat launches (or relaunches, after an outage killed the IRC
	// reconnect loop) the chat downloader. Runs on parentCtx so a session
	// restart doesn't tear chat down — shutdown/user-cancel paths Stop() it
	// explicitly.
	var chatDone chan struct{}
	// chatRec carries Start's terminal error to the verdict below — a Twitch
	// VOD whose cursor paging stalls returns one, with a SHORT archive on disk.
	// Declared out here because startChat may run several times (the relaunch
	// after a connectivity outage) and the LAST run's outcome is the job's.
	var chatRec chatOutcome
	startChat := func() {
		// Wire OnProgress for DB updates via SetOnProgress — avoids the race
		// surface on public-field reassignment (audit reports/worker.md F3).
		if irc != nil {
			irc.SetOnProgress(func(count int) { tracker.SetChatCount(count) })
		}
		if vod, ok := twitchChatDl.(*twitch.VodChatDownloader); ok {
			vod.SetOnProgress(func(count int) { tracker.SetChatCount(count) })
			vod.SetIsOnline(connIsOnline(o.conn))
		}
		// The stream processor wrote "pending"; this is where the capture
		// actually starts, as the YouTube downloader's OnStart marks it.
		o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{
			"chat_status": "downloading",
		})
		done := make(chan struct{})
		chatDone = done
		go func() {
			defer close(done)
			defer func() {
				if r := recover(); r != nil {
					// A panic is an outcome too: a downloader that died
					// mid-capture has not finished, and recording nothing here
					// would leave a previous run's verdict standing.
					chatRec.record(fmt.Errorf("panic in Twitch chat downloader: %v", r))
					o.logger.Error("panic in Twitch chat downloader", "jobID", jobID, "panic", fmt.Sprint(r))
				}
			}()
			chatRec.record(twitchChatDl.Start(parentCtx))
		}()
	}
	if twitchChatDl != nil {
		if irc != nil {
			// Recording start time for IRC chat offset calculation (matches
			// TS) — provisional for a part whose video starts fresh, below.
			irc.SetRecordingStartTime(time.Now().UTC().Format(time.RFC3339))
			// A job resumed into a later part keeps chat aligned with video:
			// redirect chat output (created at the staging root by the stream
			// processor) to the current part's staging BEFORE Start loads
			// resume state, so it continues that part's file.
			if curStagingDir != jobCtx.StagingDir {
				irc.RollFile(chatPathFor(curStagingDir), time.Now().UTC().Format(time.RFC3339))
			}
			// A part with no staged video starts its file with the first
			// segment the downloader writes, and that segment's program
			// date-time becomes the chat's base (D-T8): the local clock here
			// is later than the part's first frame by however far behind the
			// live edge the playlist window starts. A RESUMED part's video
			// started long ago and reports nothing; its chat keeps the base
			// its file was written with (adoptPartRecordingBase).
			if !partResumed {
				irc.AwaitPartBase()
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
	//
	// Returns the reason the advance failed — os.MkdirAll is the only one —
	// so the caller can latch it. A part advance that cannot create its next
	// staging dir must never let the job finalize Finished (sweep-2 R2).
	advanceToNewPart := func(muxCurrent bool, segmentEndTime int64) error {
		nextIdx := segmentIndex
		if muxCurrent {
			nextIdx = segmentIndex + 1
		}
		nextDir := filepath.Join(jobCtx.StagingDir, fmt.Sprintf("seg_%d", nextIdx))
		if err := os.MkdirAll(nextDir, 0o755); err != nil {
			o.logger.Error("failed to create part staging dir", "err", err, "dir", nextDir, "jobID", jobCtx.Job.ID)
			return fmt.Errorf("create part staging dir %s: %w", nextDir, err)
		}
		var closedChat string
		var enrich func(context.Context)
		if irc != nil && nextDir != curStagingDir {
			// The next part's video always starts a fresh file, so its chat
			// waits for that file's first segment (D-T8).
			closedChat = irc.RollFileAwaitingBase(chatPathFor(nextDir), time.Now().UTC().Format(time.RFC3339))
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
		return nil
	}

	// outageFinalize marks "finalize what was captured" exits: the broadcast
	// ended (or became unreachable/another broadcast) while connectivity was
	// down. The post-loop dispatch uses it to pick
	// the finalize path over the terminal preserve-staging path — the
	// offlineCancelled flag can't serve that role since the outage handler
	// consumes it on entry.
	outageFinalize := false

	// unconfirmedEndErr latches the HLS loop's unknown-verdict exit: the
	// download failed while nothing said the broadcast was over. Finalizing
	// there marked the job Finished and processJob then deleted the staging
	// dir — with the resume sidecar in it (sweep-2 ENGINE-7). Returning the
	// error instead leaves the job in Error with staging intact, so Mux can
	// still archive what was captured. Nothing continues the capture from
	// there — /resume refuses every non-YouTube job, Retry starts over with
	// fresh staging and the monitor recovers offline flaps only — which is
	// why the in-loop variant refresh retries before taking this exit. What
	// the monitor does do, for a LIVE capture's latch (markEndUnconfirmed), is
	// mux that staging itself once it confirms the broadcast over (D-T4,
	// AutoMuxEndedBroadcast), so the archive no longer waits on the operator.
	var unconfirmedEndErr error

	// latchIfUnconfirmed takes that latch unless the broadcast is CONFIRMED
	// over, and reports whether it did. Every inner-loop exit that leaves on
	// a failure routes through this one rule (owner decision O-C, sweep-2
	// R2): the consult is the worker's two-sample CheckStreamFn closure, so
	// "still live" and "the check itself failed" both mean the verdict is
	// unknown and the job must land in Error with its staging and resume
	// sidecar intact. Only a confirmed end falls through to finalize.
	//
	// The guards keep the sites honest: a nil cause is not a failure, a dead
	// ctx means we are shutting down rather than judging a broadcast, and an
	// unwired CheckStreamFn can contradict nothing. A VOD latches on any
	// failure without asking: it has no live end to confirm — its only
	// confirmed end is the download completing — so finalizing a failed one
	// marked a truncated file Finished and then deleted its staging.
	latchIfUnconfirmed := func(ctx context.Context, cause error) bool {
		if cause == nil || ctx.Err() != nil {
			return false
		}
		if isVod {
			o.logger.Warn("Twitch VOD download failed before its end — keeping staging for recovery",
				"jobID", jobCtx.Job.ID, "err", cause)
			unconfirmedEndErr = cause
			return true
		}
		if variant.CheckStreamFn == nil {
			return false
		}
		stillLive, checkErr := variant.CheckStreamFn(ctx)
		if checkErr == nil && !stillLive {
			return false
		}
		o.logger.Warn("Twitch download ended without a confirmed stream end — keeping staging for recovery",
			"jobID", jobCtx.Job.ID, "stillLive", stillLive, "checkErr", checkErr)
		// Marked (D-T4): the row this leaves in Error is the one the Twitch
		// monitor muxes automatically once it confirms the broadcast over.
		unconfirmedEndErr = markEndUnconfirmed(cause)
		return true
	}

	// latchPartFailure routes a failed part advance to the same
	// Error-with-staging exit. os.MkdirAll is advanceToNewPart's only failure
	// mode, so the staging this capture needs in order to continue is
	// unusable — the job must not be advertised Finished whatever the
	// broadcast is doing. No re-verify: this is a local I/O failure, not a
	// statement about the stream, so it would be a GQL call spent on a
	// question nobody asked.
	latchPartFailure := func(err error) {
		o.logger.Error("Twitch part advance failed — keeping staging, finishing in Error",
			"err", err, "jobID", jobCtx.Job.ID)
		unconfirmedEndErr = fmt.Errorf("advance to new part: %w", err)
	}

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
			// A gap split that actually lost segments is the only one the
			// operator is notified about — see gapSplitLostData.
			gapLostData := gapSplitLostData(dlErr)

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
				confirmedOver := false
				if fetchErr != nil && !isGap && !isInitChange {
					// Nothing else carries this capture on: the exit below
					// leaves the job in Error, and neither Retry (it wipes
					// staging) nor the monitor (it recovers offline flaps
					// only) continues it. A usher 5xx or a token blip
					// outlasts a single request often enough to be worth
					// riding out — footage the window drops meanwhile is
					// recorded as a gap split once the successor starts.
					newVariant, confirmedOver, fetchErr = o.retryVariantRefresh(ctx, refreshBestVariant, fetchErr,
						func(c context.Context) bool {
							if variant.CheckStreamFn == nil {
								return false
							}
							live, err := variant.CheckStreamFn(c)
							return err == nil && !live
						}, jobCtx.Job.ID)
				}
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
						if gapLostData {
							o.sendGapSplitNotification(jobCtx, segmentIndex, currentQuality)
						}
						nextSeq := videoDl.CurrentSeq()
						if err := advanceToNewPart(true, segmentEndTime); err != nil {
							latchPartFailure(err)
							break
						}
						videoDl, videoPath = createDownloader(currentVariantURL, curStagingDir, nextSeq, nextSeq > 0)
						tracker.AttachVideoDownloader(videoDl)
						drainQualityCh()
						continue
					}
					// A master-playlist refresh failing here says NOTHING about
					// whether the broadcast is over — a usher 5xx, a rate
					// limit and an access-token blip all look the same as the
					// 404 an ended stream returns. And the round that brought
					// us here was ErrQualityLost, which post-O-C means the
					// engine's consult had just CONFIRMED the broadcast live.
					// Finalizing on that was sweep-2 R2: the row went
					// Finished mid-broadcast and the monitor, seeing a
					// finished job for that stream ID, never re-archived the
					// rest. Re-verify and only fall through when the end is
					// confirmed.
					//
					// downloadErr is what ended the inner loop and brought us
					// into this branch — usually ErrQualityLost, but nil when
					// the quality monitor (not an error) triggered the
					// refresh. Logged beside fetchErr because the pair is the
					// whole story of why this job is about to stop, and
					// neither half is logged anywhere else on this path.
					if ctx.Err() != nil {
						break // an outage or a shutdown mid-retry, not a verdict
					}
					o.logger.Error("failed to refresh Twitch variants",
						"err", fetchErr, "downloadErr", dlErr, "jobID", jobCtx.Job.ID)
					if confirmedOver {
						// The retry's own status check already confirmed the
						// end; a second one would only delay the finalize.
						o.logger.Info("Twitch broadcast confirmed over during the variant refresh retries; finalizing captured parts",
							"jobID", jobCtx.Job.ID)
					} else if !latchIfUnconfirmed(ctx, fmt.Errorf("refresh Twitch variants: %w", fetchErr)) {
						o.logger.Info("Twitch broadcast confirmed over after the failed variant refresh; finalizing captured parts",
							"jobID", jobCtx.Job.ID)
					}
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
					if err := advanceToNewPart(true, segmentEndTime); err != nil {
						latchPartFailure(err)
						break
					}
					currentQuality = newQuality
					currentVariantURL = newVariant.URL
					recordVariant(newVariant)
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
					if gapLostData {
						o.logger.Warn("Twitch gap split — starting new part",
							"closedPart", segmentIndex+1, "quality", currentQuality.Label, "jobID", jobCtx.Job.ID)
						o.sendGapSplitNotification(jobCtx, segmentIndex, currentQuality)
					} else {
						// engine.ErrTruncateBlocked: the resume truncate was
						// refused, so the engine split rather than destroy the
						// staged part. Its own message, and no notification —
						// nothing expired from the CDN.
						o.logger.Warn("Twitch split after a blocked resume truncate — starting new part; no segments were lost",
							"closedPart", segmentIndex+1, "quality", currentQuality.Label, "jobID", jobCtx.Job.ID)
					}

					nextSeq := videoDl.CurrentSeq()
					if err := advanceToNewPart(true, segmentEndTime); err != nil {
						latchPartFailure(err)
						break
					}
					currentQuality = newQuality
					currentVariantURL = newVariant.URL
					recordVariant(newVariant)
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
					recordVariant(newVariant)
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
				// A kept part ends exactly where its data stops, so the
				// successor is seeded from CurrentSeq like the gap and
				// init-change splits — Twitch's variants share one sequence
				// numbering. Seeding -1 replayed the playlist window, so the
				// first seconds of the new part repeated the closed part's
				// last ones. A discarded short part leaves nothing to
				// repeat: the window replay keeps whatever it still holds of
				// the dropped span, at the new quality.
				nextSeq, forceSeq := -1, false
				if !shortSegment {
					nextSeq = videoDl.CurrentSeq()
					forceSeq = nextSeq > 0
				}
				if err := advanceToNewPart(!shortSegment, segmentEndTime); err != nil {
					latchPartFailure(err)
					break
				}
				currentQuality = newQuality
				currentVariantURL = newVariant.URL
				recordVariant(newVariant)
				videoDl, videoPath = createDownloader(currentVariantURL, curStagingDir, nextSeq, forceSeq)
				tracker.AttachVideoDownloader(videoDl)
				drainQualityCh()
				continue
			}

			// Normal stop. A nil error is the loop's clean end. A non-nil one
			// is the unknown-verdict exit: re-verify once (the closure takes
			// two samples ~5 s apart) and only fall through to finalize when
			// the broadcast is CONFIRMED over.
			if dlErr != nil && ctx.Err() == nil {
				o.logger.Error("Twitch HLS download error", "err", dlErr, "jobID", jobCtx.Job.ID)
				latchIfUnconfirmed(ctx, dlErr)
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
		// ---- Connectivity outage (live): pause, then resume the same job ----
		// The flag is consumed HERE; a re-drop at any later point in the
		// recovery sets it again, and the checks below route back to the
		// wait instead of finalizing mid-broadcast.
		offlineCancelled.Store(false)
		// Each session decides its own verdict, and the outage ends this
		// one's. A latch it took is stale: the check that failed did so
		// because the monitor cancelled the session from inside the
		// re-verify's own 5-35 s window. Discarded HERE, on entry, rather
		// than when the next session starts, because the recovery below has
		// two ways out that never start one — the broadcast ended during the
		// outage, or a failed refresh whose re-verify confirms it over — and
		// both finalize. A latch carried into the resumed session marked a
		// capture it completed cleanly Error and skipped its final mux (fix
		// round 1); carried into those finalize exits, it turned a
		// broadcast the recovery confirmed over into a marked Error exit
		// that never muxed its last part. Only a latch the recovery itself
		// takes, or the resumed session's own, reaches the exit below.
		unconfirmedEndErr = nil
		// The pause instant, stamped where the flag is consumed. NOT sent:
		// a "download paused, connectivity lost" embed has no connectivity to
		// travel over, so its three attempts and ~7 s of backoff delivered it
		// only when the outage was shorter than the send itself (audit C8 —
		// the same fallacy the connectivity_lost removal fixed). The resume
		// embed below carries it, and an outage that never resumes is covered
		// by connectivity_split.
		pausedAt := time.Now()

		o.logger.Warn("Twitch download paused — connectivity lost; waiting to resume same broadcast",
			"part", segmentIndex+1, "jobID", jobCtx.Job.ID)
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
			stillLive, info, recheckErr := o.recheckTwitchBroadcast(parentCtx, variant)
			if parentCtx.Err() != nil || userCancelled.Load() {
				break sessionLoop
			}
			if o.conn != nil && !o.conn.IsOnline() {
				continue recoverLoop // dropped again mid-recheck
			}
			if recheckErr != nil {
				// Online, and still no answer about the broadcast: the
				// verdict is unknown, so the job keeps its staging and lands
				// in Error rather than finishing over a capture that may be
				// half of a broadcast still running.
				o.logger.Warn("Twitch broadcast could not be rechecked after the outage — keeping staging for recovery",
					"err", recheckErr, "jobID", jobCtx.Job.ID)
				unconfirmedEndErr = markEndUnconfirmed(fmt.Errorf("recheck Twitch broadcast after outage: %w", recheckErr))
				break sessionLoop
			}
			if !stillLive || !o.sameTwitchBroadcast(jobCtx.Job.ID, info) {
				o.logger.Info("Twitch broadcast ended or changed during outage; finalizing captured parts",
					"jobID", jobCtx.Job.ID)
				outageFinalize = true
				break sessionLoop
			}

			// Variant URLs are short-lived — refresh before resuming. A failed
			// refresh says nothing about the broadcast (a usher 5xx, a token
			// blip): retry, then re-verify exactly as the in-loop refresh
			// does (sweep-2 R2) — only a confirmed end finalizes.
			var fetchErr error
			for attempt := range postOutageRefreshAttempts {
				if newVariant, fetchErr = refreshBestVariant(parentCtx); fetchErr == nil || parentCtx.Err() != nil {
					break
				}
				if o.conn != nil && !o.conn.IsOnline() {
					break
				}
				if attempt < postOutageRefreshAttempts-1 {
					utils.Sleep(parentCtx, time.Duration(attempt+1)*postOutageRetryDelay)
				}
			}
			if fetchErr != nil {
				if parentCtx.Err() != nil || userCancelled.Load() {
					break sessionLoop
				}
				if o.conn != nil && !o.conn.IsOnline() {
					continue recoverLoop // dropped again mid-refresh
				}
				o.logger.Error("failed to refresh Twitch variants after outage", "err", fetchErr, "jobID", jobCtx.Job.ID)
				if !latchIfUnconfirmed(parentCtx, fmt.Errorf("refresh Twitch variants after outage: %w", fetchErr)) {
					o.logger.Info("Twitch broadcast confirmed over after the failed post-outage refresh; finalizing captured parts",
						"jobID", jobCtx.Job.ID)
					outageFinalize = true
				}
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
			if err := advanceToNewPart(true, time.Now().Unix()); err != nil {
				latchPartFailure(err)
				break sessionLoop
			}
			currentQuality = newQuality
		}
		// Same quality: resume IN the current part's staging dir — the
		// engine's resume + StopOnGap decides between seamless continuation
		// (short outage, playlist still covers our position: zero loss, no
		// split) and ErrGapDetected (the inner loop splits to a new part).
		currentVariantURL = newVariant.URL
		recordVariant(newVariant)
		videoDl, videoPath = createDownloader(currentVariantURL, curStagingDir, -1, false)
		tracker.AttachVideoDownloader(videoDl)
		drainQualityCh()
		if twitchChatDl != nil && !twitchChatDl.IsRunning() {
			startChat()
		} else if r, ok := twitchChatDl.(interface{ RetryNow() }); ok {
			// Still running, so possibly waiting out a reconnect backoff that
			// the outage stretched to its slow cadence: reconnect now.
			r.RetryNow()
		}

		o.logger.Info("Twitch download resumed after connectivity outage",
			"part", segmentIndex+1, "quality", currentQuality.Label, "jobID", jobCtx.Job.ID)
		o.sendTwitchSessionNotification(jobCtx, "Twitch Download Resumed",
			fmt.Sprintf("Connectivity restored, resuming download: %s", notifications.EscapeMarkdown(jobCtx.Job.Title)),
			notifications.TypeDownload, "connectivity_resume", currentQuality, segmentIndex+1,
			twitchOutageField(pausedAt))
	}

	tracker.Finalize()

	// Unknown-verdict exit: stop chat, record its verdict so the row is
	// honest, and return the download error. processJob's setJobError path
	// returns before os.RemoveAll(jobCtx.StagingDir), so staging AND the
	// resume sidecar survive.
	//
	// It sits AHEAD of the `status: Muxing` write below because this job is
	// on its way to Error and never muxes its final part — advertising it as
	// Muxing first would be a lie the UI shows (fix round 1). The background
	// part muxes still have to land, so their Wait happens here too.
	//
	// The chat wait is deliberately short and asymmetric with the finalize
	// path's (which drains via MarkStreamEnded on a chatWaitTimeout bound):
	// chat was Stop()'d, the file on disk is complete through its last flush,
	// and the job is going to Error with staging intact, so waiting out
	// chatWaitTimeout buys nothing. A Retry re-runs the whole capture.
	//
	// Gated on the JOB's context and the user's cancel, not on the session's:
	// the post-outage exits (a recheck that never answers, a refresh that
	// keeps failing on a live broadcast) latch from the recovery loop, where
	// the session context is the one the outage cancelled. Gated on that,
	// they fell through to the shutdown path and returned "context canceled"
	// — the row read Error with that text, and lost the end-unconfirmed mark
	// the automatic mux keys on (D-T4).
	if unconfirmedEndErr != nil && parentCtx.Err() == nil && !userCancelled.Load() {
		segmentMuxWg.Wait()
		if twitchChatDl != nil {
			if twitchChatDl.IsRunning() {
				twitchChatDl.Stop()
			}
			outcome := o.resolveChatOutcome(twitchChatDl, &chatRec, chatDone, 2*time.Second, 2*time.Second)
			o.recordChatOutcome(jobCtx, twitchChatDl.MessageCount(), outcome)
		}
		return unconfirmedEndErr
	}

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
				// This exit never reaches resolveChatOutcome, the only other
				// place that waits on chatDone — and the chat goroutine's
				// deferred teardown (flush, then the resume-sidecar write) is
				// still running. Returning here left ResumeStore.Save mid-
				// rename inside the staging directory the caller is about to
				// hand to a resume, a /retry (which DELETES staging) or a
				// t.TempDir cleanup. Bounded, like every other wait on this
				// channel: a wedged downloader must not hold a shutdown open.
				if !waitForChatShutdown(chatDone, chatShutdownGrace) {
					o.logger.Warn("Twitch chat downloader still running after the shutdown grace; "+
						"its resume sidecar may be incomplete", "jobID", jobCtx.Job.ID)
				}
			}
			return ctx.Err()
		}
		// The broadcast ended or changed while connectivity was down: finalize
		// everything captured up to the outage. Live only — outageFinalize is
		// set by the session loop's outage branch, which a VOD never reaches
		// (its offline cancel is not registered; the engine waits the outage
		// out instead).
		o.logger.Warn("Twitch download cut off by connectivity outage, finalizing captured data", "jobID", jobCtx.Job.ID)

		desc := fmt.Sprintf("Broadcast ended while connectivity was down: %s", notifications.EscapeMarkdown(jobCtx.Job.Title))
		o.sendTwitchSessionNotification(jobCtx, "Twitch Download Finalizing — Connectivity Lost",
			desc, notifications.TypeDownload, "connectivity_split", currentQuality, segmentIndex+1)

		// Stop chat (resume state is preserved by the interrupted-exit path;
		// the file on disk is complete through the last flush). That exit's
		// verdict is what resolveChatOutcome records below, so it carries a
		// batch a part roll spilled (rollUnwritten), and one its own final
		// flush could not write (spilled there), exactly as the stream-end
		// drain's does: incomplete, and the cleanup keeps the spill.
		if twitchChatDl != nil {
			twitchChatDl.Stop()
		}
		// IMPORTANT: Fall through to muxing logic below.
	}

	// After the fall-through from connectivity loss, use the mux root for
	// muxing: the job's own context is cancelled, but a shutdown must still be
	// able to reach this FFmpeg (owner decision O-E). context.Background()
	// here used to survive the child's exit and keep writing into a staging
	// dir the respawned child re-muxes with -y.
	// The finalize runs on a session of its own: only a user cancel or the
	// job's own context (shutdown, deletion) can end it, never an offline
	// transition (finalizing, above) — it needs no network. The fresh
	// session is what covers the moment between the session loop's exit and
	// the flag: an offline cancel landing there cancelled the loop's session,
	// which nothing below uses.
	finalizing.Store(true)
	finalCtx, _ := newSession()
	muxCtx := finalCtx
	if outageFinalize {
		muxCtx = o.muxRoot()
	}

	// Signal chat to finish BEFORE muxing the final part: the drain flushes
	// the tail of the chat and runs emote enrichment, and the final part's
	// chat file is copied beside its video by the mux below — copying before
	// the drain would ship a truncated, unenriched file.
	// Per audit reports/worker.md F5: skip MarkStreamEnded if chat was already
	// Stop()'d above (connectivity-loss path) — racing with the goroutine's
	// shutdown can panic or deadlock inside the chat downloader.
	if twitchChatDl != nil {
		if twitchChatDl.IsRunning() {
			twitchChatDl.MarkStreamEnded()
		}
		// resolveChatOutcome waits on chatDone UNCONDITIONALLY — not gated on
		// IsRunning() the way this used to be. running is cleared by a defer
		// INSIDE Start, strictly before the wrapper goroutine calls
		// chatRec.record(), so an IsRunning()-gated wait could observe
		// running==false and skip straight to a verdict that had not landed
		// yet (fix round 1, Important 3) — exactly the window a stalled
		// Twitch VOD chat could fall into, writing "finished" over a stall. A
		// wait that times out returns an explicit incomplete rather than
		// letting a stale nil verdict read as "finished".
		//
		// Owner decision O-A: a VOD waits for its chat to finish paging on a
		// bound scaled to the video's own length, with the download slot
		// released first (resolveVodChatOutcome does both) so the pool is not
		// held through a wait that is no longer downloading anything — this
		// site sits AHEAD of the final part's mux, so the hold would be
		// longer still. A live chat ends when the broadcast does, so the live
		// path keeps the two-minute cut.
		var outcome error
		if isVod {
			outcome = o.resolveVodChatOutcome(finalCtx, twitchChatDl, &chatRec, chatDone, jobCtx.Job)
		} else {
			outcome = o.resolveChatOutcome(twitchChatDl, &chatRec, chatDone, chatWaitTimeout, chatShutdownGrace)
		}
		o.recordChatOutcome(jobCtx, twitchChatDl.MessageCount(), outcome)
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
	// math.Round, not a bare int conversion: Twitch advertises NTSC rates
	// (59.94, 29.97), and truncating labelled that content "1080p59" while
	// the playlist's own variant name (internal/twitch/hls.go) says 1080p60.
	fps := int(math.Round(v.FPS))
	return QualityInfo{
		Width:  v.Width,
		Height: v.Height,
		FPS:    fps,
		Label:  FormatQualityLabel(v.Height, fps),
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

		best := variant.selectFrom(variants)
		if best == nil {
			return nil, fmt.Errorf("no variant found")
		}

		qi := qualityInfoFromVariant(best)
		return &qi, nil
	}
}

// twitchOutageField renders the outage an embed is reporting the end of: when
// it began, as a Discord relative timestamp that keeps counting in the client,
// and how long it lasted. This is what the retired connectivity_pause embed
// used to carry, folded into the one embed that can actually be delivered.
func twitchOutageField(pausedAt time.Time) notifications.Field {
	return notifications.Field{
		Name: "Paused",
		Value: fmt.Sprintf("<t:%d:R> · resumed after %s",
			pausedAt.Unix(), formatDurationHuman(time.Since(pausedAt))),
		Inline: true,
	}
}

// sendTwitchSessionNotification delivers a session-lifecycle notification
// (outage resume, finalize-after-outage) with the shared channel/quality/part
// field shape, plus any extra fields the caller adds. No-op when the notifier
// is nil.
func (o *DownloadOrchestrator) sendTwitchSessionNotification(
	jobCtx *JobContext,
	title, desc string,
	ntype notifications.NotificationType,
	event string,
	quality QualityInfo,
	partNo int,
	extra ...notifications.Field,
) {
	if o.notifier == nil {
		return
	}
	fields := []notifications.Field{
		{Name: "Channel", Value: notifications.EscapeMarkdown(jobCtx.Job.ChannelName), Inline: true},
		{Name: "Quality", Value: quality.Label, Inline: true},
		{Name: "Part", Value: fmt.Sprintf("%d", partNo), Inline: true},
	}
	fields = append(fields, extra...)
	// The event stays the caller's — this helper serves connectivity_resume
	// and connectivity_split, and only the caller knows which outage shape it
	// is reporting. The identity, though, is the row's: both keys are
	// lifecycle events, and Manager.planLifecycle keys on Opts.JobID, so
	// without these three an edit-mode target posts them beside the job's
	// message instead of into it.
	f := NotifyFacts(jobCtx.Job)
	o.notifier.Send(title, desc, ntype, fields,
		notifications.SendOptions{
			URL:       f.URL,
			Thumbnail: f.ThumbnailURL,
			Event:     event,
			Author:    notifyAuthor(f),
			Platform:  f.Platform,
			JobID:     f.ID,
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

// gapSplitLostData reports whether a gap split actually lost data, i.e.
// whether the operator should be told about it. engine.ErrGapDetected is the
// engine's one "close this part and continue in a fresh one" signal, and it
// now carries a second case that loses nothing: engine.ErrTruncateBlocked, a
// resume truncate an antivirus scanner or the search indexer refused.
// Splitting is still the right recovery there — it is what keeps the staged
// recording instead of truncating it — but no segment expired from the CDN,
// so the "segments were lost" notification would be a lie. Same precedent as
// ErrInitSegmentChanged, which splits without a notification for the same
// reason (fix round 1, Minor 4).
func gapSplitLostData(err error) bool {
	return errors.Is(err, engine.ErrGapDetected) && !errors.Is(err, engine.ErrTruncateBlocked)
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
// routes settle. err is non-nil when no attempt got an answer (or ctx ended):
// the verdict is UNKNOWN, which the caller must not read as "offline" — that
// reading finalized a still-live broadcast as Finished, and the monitor then
// never re-archived the rest of it. With no check wired at all the answer is
// a plain "not live", as latchIfUnconfirmed treats it.
func (o *DownloadOrchestrator) recheckTwitchBroadcast(ctx context.Context, variant *TwitchVariantInfo) (live bool, info *twitch.TwitchStreamInfo, err error) {
	const attempts = 4
	var lastErr error
	for i := range attempts {
		if ctx.Err() != nil {
			return false, nil, ctx.Err()
		}
		switch {
		case variant.RecheckStreamFn != nil:
			info, err := variant.RecheckStreamFn(ctx)
			if err == nil {
				return info != nil && info.IsLive, info, nil
			}
			lastErr = err
			o.logger.Debug("post-outage stream recheck failed, retrying", "attempt", i+1, "err", err)
		case variant.CheckStreamFn != nil:
			live, err := variant.CheckStreamFn(ctx)
			if err == nil {
				return live, nil, nil
			}
			lastErr = err
			o.logger.Debug("post-outage stream check failed, retrying", "attempt", i+1, "err", err)
		default:
			return false, nil, nil
		}
		// No sleep after the final attempt — there is no retry left to wait
		// for, and the caller is deciding what to do with the job.
		if i < attempts-1 {
			utils.Sleep(ctx, time.Duration(i+1)*postOutageRetryDelay)
		}
	}
	return false, nil, lastErr
}

// retryVariantRefresh retries a master-playlist refresh that failed inside
// the download loop with err, pausing longer before each attempt
// (liveRefreshAttempts tries in all, counting the one that already failed),
// and returns the last error when none succeeds. It gives up at once when
// ctx ends — an outage or a shutdown is the session loop's to handle — and
// when ended (nil-safe) confirms the broadcast over before a pause: a
// variant list that is gone because the stream ended is the commonest way
// here, and riding out the whole schedule only held an ended capture in
// Downloading for another ~100 s. confirmedOver reports that exit.
func (o *DownloadOrchestrator) retryVariantRefresh(ctx context.Context,
	refresh func(context.Context) (*twitch.TwitchHLSVariant, error), err error,
	ended func(context.Context) bool, jobID string) (best *twitch.TwitchHLSVariant, confirmedOver bool, _ error) {
	for attempt := 1; attempt < liveRefreshAttempts; attempt++ {
		if ended != nil && ended(ctx) {
			return nil, true, err
		}
		o.logger.Warn("Twitch variant refresh failed, retrying",
			"attempt", attempt, "of", liveRefreshAttempts, "err", err, "jobID", jobID)
		if sleepErr := utils.Sleep(ctx, time.Duration(attempt)*liveRefreshRetryDelay); sleepErr != nil {
			return nil, false, sleepErr
		}
		if best, err = refresh(ctx); err == nil {
			o.logger.Info("Twitch variant refresh recovered", "attempt", attempt+1, "jobID", jobID)
			return best, false, nil
		}
		if ctx.Err() != nil {
			return nil, false, err
		}
	}
	return nil, false, err
}

// liveRefreshAttempts bounds retryVariantRefresh: with liveRefreshRetryDelay
// steps of 10 s the last attempt lands about 100 s after the first failure.
const liveRefreshAttempts = 5

// liveRefreshRetryDelay is retryVariantRefresh's backoff step: the n-th retry
// waits n steps. A variable so tests need not sleep it out.
var liveRefreshRetryDelay = 10 * time.Second

// postOutageRefreshAttempts is how many times the recovery tries the master
// playlist once the broadcast is confirmed live again — the same settling
// window recheckTwitchBroadcast gives the liveness check.
const postOutageRefreshAttempts = 3

// postOutageRetryDelay is the backoff step of both post-outage retry loops:
// the n-th retry waits n steps. A variable so tests need not sleep it out.
var postOutageRetryDelay = 3 * time.Second

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
