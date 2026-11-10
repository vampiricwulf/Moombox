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

// stagingPreservedRefusal reports the engine refusals that end a download run
// with the staged recording AND its resume sidecar untouched: the no-truncate
// guard (engine.ErrStagedMediaPresent) and a resume truncate the retry ladder
// could not push through (engine.ErrTruncateBlocked). Both are decided before
// the first segment is fetched and both depend only on what is on disk, so
// re-running the same downloader would refuse identically.
//
// Without this the live loop reads them as an ordinary "downloaders stopped",
// re-verifies a stream YouTube still reports live, and burns
// maxConsecutiveLiveChecks × streamEndVerifyInterval — half an hour — before
// force-ending the job, with the real reason visible only in the engine log.
// Returning the error instead fails the job fast with everything on disk
// preserved, which is exactly what a later Resume needs.
func stagingPreservedRefusal(err error) bool {
	return errors.Is(err, engine.ErrStagedMediaPresent) || errors.Is(err, engine.ErrTruncateBlocked)
}

// liveVideoInfo and runLiveDownloaders are the live loop's player-response
// re-fetch ((*youtube.Service).GetVideoInfo) and its run of the part's
// downloaders ((*DownloadOrchestrator).runDownloaders) behind package vars,
// so a test can drive the loop's refresh branches — a path switch into a new
// part, above all — without a network or a running engine: job.YT is the
// concrete *youtube.Service, and a downloader fetches through the engine's
// own client. Production never writes them.
var (
	liveVideoInfo      = (*youtube.Service).GetVideoInfo
	runLiveDownloaders = (*DownloadOrchestrator).runDownloaders
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
// replacement, since attachProgress is the single call site for both, via
// attachMayResume. VOD downloads never call this function, so MayResume is
// never set there — only live YouTube downloads get the interruption
// stall. attachMayResume installs MayResume unconditionally (I1 fix) — the
// config→engine mapping for a disabled stall (config contract: 0 =
// finalize must never wait) now happens once, at construction time, in
// each strategy's engine.DownloaderOptions.InterruptionTimeout via
// engineInterruptionTimeout, which maps 0 onto engine.InterruptionNoStall
// rather than passing 0 straight through (engine's own zero means
// "unbounded," the opposite of "disabled"). A disabled-stall job's
// downloader is thus built with the sentinel, still gets MayResume
// installed, and still latches Tier-2 evidence — it just never actually
// stalls (see engine.SegmentDownloader.stallForPossibleResume's
// InterruptionNoStall branch).
//
// lastSegTime is the shared last-new-segment clock created alongside
// mayResume by ExecuteWithChat (both close over/observe the SAME clock):
// this loop is its writer — re-armed at entry and advanced by the
// echo-suppressed onSegmentProgress below — while the mayResume closure
// reads it to judge the chat-open signal's joint-idle release (see
// chatSignalJointIdleAfter, interruption.go). It doubles as this loop's own
// streamSegmentTimeout clock, which is exactly the point: one clock, no
// drift between "what the verify branch considers segment activity" and
// "what the chat gate considers segment activity".
//
// The second return value, waitedForResume, is worker-level Tier 2
// evidence — FINALIZE-scoped, not history-scoped: true only when the loop
// is CURRENTLY in (or gives up directly out of) an unresolved
// noteRefreshFailure evidence-latch — which fires whenever resume evidence
// holds on a failed refresh, whether or not shouldWaitForResume actually
// permitted a wait for it (I1 fix: interruptionTimeout<=0 disables the
// WAIT, not this latch). It exists because the wait branch can retry and
// eventually give up WITHOUT any engine downloader ever reaching its own
// stallForPossibleResume (the loop dies from the outside — repeated
// refresh failures — not from an engine-side budget expiry), so
// FinalizedDuringInterruption would otherwise stay false even though the
// job deliberately waited for (or, config permitting, evidenced) a
// resume. The wait is bounded by waitEpisode (I3 fix) — its ceiling is
// jobCtx.Config.InterruptionTimeout, NOT maxConsecutiveLiveChecks. The
// quality-loss branch waits once; the downloaders it cancelled bring every
// later look to the "normal download stop" still-live re-verification
// branch, which asks the same episode and, while it may keep waiting,
// refunds the live check it spent — that counter bounds only the verify
// branch's own retries outside a wait. waitedForResume latches true when
// noteRefreshFailure's evidence check fires and is CLEARED again at every
// later successful refresh (`result = refreshResult`) — a broadcast that
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
	lastSegTime *atomicTimeValue,
) (*DownloadResult, bool, error) {
	lastSegTime.StoreNow()
	var consecutiveLiveChecks atomic.Int32
	// waitedForResume latches true when shouldWaitForResume's branch fires
	// below and clears again at every later successful refresh — see the
	// doc comment above and resumeWaitLatch's own doc comment for the full
	// finalize-scoped rationale.
	var waitedForResume resumeWaitLatch
	// waitEpisode bounds the wait branch below to the job's
	// InterruptionTimeout (I3 fix) — see waitDeadline's doc comment. Reset
	// alongside every waitedForResume.resolved() call so a later,
	// independent stall episode gets its own fresh budget.
	var waitEpisode waitDeadline
	// verifyWalled is whether the stream-end verify's last refresh failed on
	// a credential wall. The second one in a row parks the job (see
	// refreshWhileLiveWith for why one is not believed).
	var verifyWalled bool

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
		// login-required) route through the authenticated TV_SAMSUNG
		// probe instead — one cookied player call, not the full cascade
		// (owner decision O-H) — since ANDROID_VR will 401 on those. Same
		// predicate observeYouTubeStatusProbe uses to skip arming the
		// interruption signal on these — kept as one function so the two
		// checks cannot drift apart.
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

	// Track segment activity via progress callbacks. See
	// segmentProgressResetsStallCounters' doc comment (interruption.go) for
	// why Bytes>0 alone is not sufficient (I3 fix) and why lastBytes is
	// tracked PER STREAM, reset to 0 by attachProgress whenever a brand-new
	// downloader instance is attached for that stream so a genuinely fresh
	// downloader's first real report still counts.
	onSegmentProgress := func(p engine.DownloadProgress, lastBytes *atomic.Int64) {
		noteSegmentProgress(p, lastBytes, lastSegTime, &consecutiveLiveChecks)
	}

	var lastVideoBytes, lastAudioBytes atomic.Int64

	// rebuilds counts the ErrQualityLost rebuilds at one sequence; see
	// sameSeqRebuildCap.
	var rebuilds sameSeqRebuilds

	attachProgress := func(res *DownloadResult) {
		if res.VideoDownloader != nil {
			tracker.AttachVideoDownloader(res.VideoDownloader)
			lastVideoBytes.Store(0)
			origOnProgress := res.VideoDownloader.OnProgress
			res.VideoDownloader.OnProgress = func(p engine.DownloadProgress) {
				onSegmentProgress(p, &lastVideoBytes)
				if origOnProgress != nil {
					origOnProgress(p)
				}
			}
			attachMayResume(res.VideoDownloader, mayResume)
		}
		if res.AudioDownloader != nil {
			tracker.AttachAudioDownloader(res.AudioDownloader)
			lastAudioBytes.Store(0)
			origOnProgress := res.AudioDownloader.OnProgress
			res.AudioDownloader.OnProgress = func(p engine.DownloadProgress) {
				onSegmentProgress(p, &lastAudioBytes)
				if origOnProgress != nil {
					origOnProgress(p)
				}
			}
			attachMayResume(res.AudioDownloader, mayResume)
		}
		// Every DASH downloader this loop runs can have its part closed at a
		// segment YouTube keeps refusing (owner decision D8; the split is the
		// ErrSegmentRefused branch below), and the ones a capped rebuild built
		// are told where (sameSeqRebuildCap). The HLS loop never reads either.
		for _, d := range []*engine.SegmentDownloader{res.VideoDownloader, res.AudioDownloader} {
			if d != nil {
				d.SplitOnRefusedSegment = true
			}
		}
		rebuilds.arm(res)
	}

	attachProgress(result)

	// curCtx tracks the JobContext whose StagingDir is the CURRENT part's
	// directory. For a fresh job it is jobCtx itself (root staging); after a
	// quality split — or when restart discovery resumed into a later part —
	// it points at the seg_N copy. Every refresh-created downloader must use
	// curCtx — using the root jobCtx after a split would append fresh data
	// into part 1's already-muxed staging files.
	curCtx := curStart

	// splitPart closes the current part where the stream changed quality and
	// opens the next in a fresh seg_N directory, continuing from the old
	// downloaders' next sequences (oldVideoSeq/oldAudioSeq). It is the one
	// split both paths that can discover a new quality take: a quality change
	// (monitor or ErrQualityLost) and a still-live stall refresh. exit reports
	// that the loop must return result with err; otherwise the new part's
	// downloaders are in result and the loop continues.
	//
	// It is also the split at a refused segment (hole >= 0, the sequence the
	// engine's ErrSegmentRefused named; -1 otherwise): the part is closed at
	// the hole whatever the quality, and the caller has already moved the
	// stopped streams past it. That split is the job log's alone: it is not a
	// quality change, so it sends no quality-split notification unless the
	// quality changed as well.
	splitPart := func(newQuality QualityInfo, freshInfo *youtube.VideoInfo, oldVideoSeq, oldAudioSeq int, shortSegment bool, segmentEndTime int64, hole int) (exit bool, err error) {
		if hole >= 0 {
			o.logger.Warn("YouTube kept refusing a segment its stream had moved well past — closing the part at it and continuing in a new one",
				"seq", hole, "segment", segmentIndex+1, "jobID", jobCtx.Job.ID)
		}
		// A span too short to be a part is set aside (setAsideShortSpan:
		// muxed into its own file beside the archive at finalize, never
		// deleted — PR-R9), and one that cannot be set aside is closed as a
		// part like any other. Decided before the notification, which says
		// which of the two the span became: sent first, it said "set aside"
		// of a span then muxed as a part.
		setAside := shortSegment && o.setAsideShortSpan(curCtx.StagingDir, jobCtx.Job.ID)
		if hole < 0 || newQuality.Changed(currentQuality) {
			o.logger.Info("quality split",
				"from", currentQuality.Label, "to", newQuality.Label,
				"segment", segmentIndex+1, "jobID", jobCtx.Job.ID)

			o.sendQualitySplitNotification(jobCtx, "YouTube", currentQuality, newQuality, segmentIndex, !setAside)
		}

		// Mux the old segment in the background, unless it was set aside.
		// No preMux: YouTube chat stays a single whole-job file.
		if !setAside {
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
		}

		// Create downloaders in the NEW staging dir. The caller's refresh points
		// at the old staging dir and was used only to check quality — it is
		// discarded unrun. A short span reuses the index, and its dir when
		// that was the current one: setAsideShortSpan has cleared it of the
		// span's media and resume state, so the engine starts the new quality
		// fresh instead of resume-appending it onto the old.
		segStagingDir := filepath.Join(jobCtx.StagingDir, fmt.Sprintf("seg_%d", segmentIndex))
		if err := os.MkdirAll(segStagingDir, 0o755); err != nil {
			return true, fmt.Errorf("create segment staging dir: %w", err)
		}

		segJobCtx := *jobCtx
		segJobCtx.StagingDir = segStagingDir
		segJobCtx.VideoStartSeq = oldVideoSeq
		segJobCtx.AudioStartSeq = oldAudioSeq

		// refreshOrSwitch is the new part's refresh over info: when it fails
		// on the player's JS challenges (liveSwitchWanted) the part is a new
		// one already, so the path switch builds into it (switchLivePath;
		// switchPart below says why). The first refresh and every still-live
		// retry of it go through here: a first refresh that failed for
		// another reason — a transient fetch, an interruption's empty
		// extraction — and a retry that then met the challenges used to be
		// retried on the same formats until the budget ran out, and the
		// loop finalized a stream YouTube still called live.
		refreshOrSwitch := func(info *youtube.VideoInfo) (*DownloadResult, error) {
			res, err := o.refreshDownload(ctx, &segJobCtx, info, result.IsHls)
			if !liveSwitchWanted(info, err, result.IsHls) || ctx.Err() != nil {
				return res, err
			}
			switched, path, swErr := switchLivePath(ctx, &segJobCtx, info, o.strategyDeps(), err)
			if swErr != nil {
				return nil, swErr
			}
			o.logger.Info("live path switched at a quality split", "path", path, "part", segmentIndex+1, "jobID", jobCtx.Job.ID)
			newQuality = o.extractQualityFromResult(switched)
			return switched, nil
		}
		refreshResult, refreshErr := refreshOrSwitch(freshInfo)
		if refreshErr != nil {
			// The same retry the quality-loss refresh gets: the part was
			// already closed, and ending the job here finalized a stream
			// YouTube still calls live.
			refreshResult, refreshErr = o.refreshWhileLive(ctx, jobCtx, freshInfo, refreshErr, &consecutiveLiveChecks, tracker, refreshOrSwitch)
		}

		if cookiesStatusError(refreshErr) {
			o.logger.Error("the capture's credentials stopped working — stopping with staging kept",
				"err", refreshErr, "jobID", jobCtx.Job.ID)
			return true, refreshErr
		}
		if refreshErr != nil {
			o.logger.Error("failed to create downloaders for new quality", "err", refreshErr, "jobID", jobCtx.Job.ID)
			// Return nil to exit the live loop; muxAndFinalize will process
			// whatever video/audio data was captured in the current staging dir.
			return true, nil
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
		// See the identical clear + comment at the loop's same-quality
		// success path — a successful refresh resolves any earlier
		// wait-for-resume.
		waitedForResume.resolved()
		waitEpisode.reset() // this stall episode is over; a later one gets a fresh budget
		// A healthy read: the verify branch's next wall is the first again.
		verifyWalled = false
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
		return false, nil
	}

	// switchPart answers a mid-stream refresh of the current part that failed
	// with cause — on the player's JS challenges, or, on the no-JS path, with
	// no split pair left (liveSwitchWanted) — by switching the capture's path
	// in a NEW part (the owner's decision on W8's second follow-up): first to
	// the no-JS from-start path, else to the live HLS playlist, each at most
	// once a run (switchLivePath). The refresh would have continued the
	// current part's files, and the path switched to is another client's
	// rendition, or another shape: written into them it would append one
	// recording's fragments under the other's init segment. So the current
	// part is closed where it stands and muxed into its own file in the
	// background, as a quality split's is — never written over, never
	// discarded however short — and the new downloaders record into the next
	// part's dir, from the position the stopped ones reached (oldVideoSeq,
	// oldAudioSeq; a YouTube live stream's segment numbers are one timeline
	// across its shapes and clients). A current part that holds no media yet
	// is reused instead. Reports whether it switched; when it did not — the
	// error is not one a switch answers, or no path is left, or the switch's
	// own setup failed — the caller handles the failure as it always has,
	// and an empty new dir is removed. The engine's own URL refresh
	// (refreshGvsCredentials) cannot switch a path; a capture whose fresh
	// URLs it cannot solve stops, and the loop's refresh after it is the one
	// that switches.
	switchPart := func(freshInfo *youtube.VideoInfo, cause error, oldVideoSeq, oldAudioSeq int) bool {
		if !liveSwitchWanted(freshInfo, cause, result.IsHls) || ctx.Err() != nil {
			return false
		}
		hasMedia := discoverStagingMedia(curCtx.StagingDir) != nil
		nextIndex, segStagingDir := segmentIndex, curCtx.StagingDir
		if hasMedia {
			nextIndex = segmentIndex + 1
			segStagingDir = filepath.Join(jobCtx.StagingDir, fmt.Sprintf("seg_%d", nextIndex))
			if err := os.MkdirAll(segStagingDir, 0o755); err != nil {
				o.logger.Error("could not create the part a path switch records into", "err", err, "jobID", jobCtx.Job.ID)
				return false
			}
		}
		segJobCtx := *jobCtx
		segJobCtx.StagingDir = segStagingDir
		segJobCtx.VideoStartSeq, segJobCtx.AudioStartSeq = oldVideoSeq, oldAudioSeq
		switched, path, err := switchLivePath(ctx, &segJobCtx, freshInfo, o.strategyDeps(), cause)
		segJobCtx.VideoStartSeq, segJobCtx.AudioStartSeq = 0, 0
		if err != nil {
			o.logger.Warn("live path switch could not run", "err", err, "jobID", jobCtx.Job.ID)
			if hasMedia {
				_ = os.Remove(segStagingDir) // only when empty
			}
			return false
		}
		if hasMedia {
			muxStart := segmentStartTime
			if partResumed {
				muxStart = 0
			}
			o.launchBackgroundSegmentMux(jobCtx, &segmentMuxWg, segmentIndex,
				muxStart, time.Now().Unix(), currentQuality, result, "youtube", nil)
			segmentIndex = nextIndex
		}
		o.logger.Info("live path switched", "path", path, "part", segmentIndex+1, "jobID", jobCtx.Job.ID)
		curCtx = &segJobCtx
		result = switched
		currentQuality = o.extractQualityFromResult(result)
		waitedForResume.resolved()
		waitEpisode.reset()
		verifyWalled = false
		segmentStartTime = time.Now().Unix()
		partResumed = false
		if monitor != nil {
			select {
			case <-qualityChangeCh:
			default:
			}
			monitor.UpdateBaseline(currentQuality)
		}
		attachProgress(result)
		return true
	}

	// switchBack is the other direction (owner decision PR-R3): a capture a
	// fallback put on the formats that need no player JS, or on the live HLS
	// playlist, goes back to the from-start path that needs the JS once it
	// can be solved again — at most once a run (runRecord.canSwitchBack). It
	// is tried at the refreshes that would otherwise rebuild the fallback
	// path (the quality-loss and still-live-verify branches): the from-start
	// setup on the fresh extraction (manifest-free DASH, else the DASH
	// manifest), which fails as it did while the challenges stay unsolvable,
	// and then nothing changes and the fallback's own refresh runs. When it
	// builds, the switch is made as switchPart makes one: in a NEW part from
	// the position the stopped downloaders reached, the fallback part closed
	// where it stands and muxed as its own file in the background — kept
	// beside the archive, never written over, never deleted — and a current
	// part that holds no media yet reused. yt-dlp has nothing to switch back
	// from: it picks a path once per extraction. Beyond it, as the switches
	// themselves are: a fallback the run took for a transient cause (a
	// sidecar restart longer than its grace, a player CDN outage) no longer
	// costs the rest of a broadcast its from-start, highest-quality path. The
	// switch back re-arms the mid-stream switch that put the capture on the
	// fallback it leaves (runRecord.markSwitchedBack, owner decision PE-3), so
	// the same cause striking again later falls back again in a new part
	// rather than stopping the capture on a spent switch.
	//
	// A capture on the live HLS playlist has no audio downloader — its one
	// stream is the muxed playlist — so oldAudioSeq is 0 there. Left at 0,
	// the from-start pair's audio was not forced and fell back to the job's
	// resume position, which nothing moved while HLS recorded: sq 0 for a
	// setup fallback, a stale one otherwise, so the new part's audio covered
	// another stretch of the broadcast than its video and re-downloaded all
	// of it. Its audio starts where the playlist's video stopped instead —
	// one timeline across shapes, as switchPart relies on.
	switchBack := func(freshInfo *youtube.VideoInfo, oldVideoSeq, oldAudioSeq int) bool {
		if !jobCtx.Run.canSwitchBack() || ctx.Err() != nil || freshInfo == nil {
			return false
		}
		if result.IsHls && result.AudioDownloader == nil {
			oldAudioSeq = oldVideoSeq
		}
		var strategy DownloadStrategy
		switch {
		case HasManifestlessDashFormats(freshInfo.Formats):
			strategy = ManifestlessDashStrategy
		case freshInfo.DashManifestURL != "":
			strategy = DashStrategy
		default:
			return false
		}
		hasMedia := discoverStagingMedia(curCtx.StagingDir) != nil
		nextIndex, segStagingDir := segmentIndex, curCtx.StagingDir
		if hasMedia {
			nextIndex = segmentIndex + 1
			segStagingDir = filepath.Join(jobCtx.StagingDir, fmt.Sprintf("seg_%d", nextIndex))
			if err := os.MkdirAll(segStagingDir, 0o755); err != nil {
				o.logger.Error("could not create the part a switch back records into", "err", err, "jobID", jobCtx.Job.ID)
				return false
			}
		}
		segJobCtx := *jobCtx
		segJobCtx.StagingDir = segStagingDir
		segJobCtx.VideoStartSeq, segJobCtx.AudioStartSeq = oldVideoSeq, oldAudioSeq
		back, err := strategy.Download(ctx, &segJobCtx, freshInfo, o.strategyDeps())
		if err == nil {
			err = setAsideOtherLiveShape(&segJobCtx, back)
		}
		segJobCtx.VideoStartSeq, segJobCtx.AudioStartSeq = 0, 0
		if err != nil {
			o.logger.Debug("the from-start path still cannot be set up; the capture stays on its fallback path", "err", err, "jobID", jobCtx.Job.ID)
			if hasMedia {
				_ = os.Remove(segStagingDir) // only when empty
			}
			return false
		}
		jobCtx.Run.markSwitchedBack()
		if hasMedia {
			muxStart := segmentStartTime
			if partResumed {
				muxStart = 0
			}
			o.launchBackgroundSegmentMux(jobCtx, &segmentMuxWg, segmentIndex,
				muxStart, time.Now().Unix(), currentQuality, result, "youtube", nil)
			segmentIndex = nextIndex
		}
		o.logger.Warn("[Cipher] live: the player's JS challenges can be solved again — switching back, in a new part, to recording from the start on the "+strategy.Kind()+" formats; the fallback recording so far is kept as its own file",
			"part", segmentIndex+1, "videoItag", back.VideoItag, "audioItag", back.AudioItag, "jobID", jobCtx.Job.ID)
		curCtx = &segJobCtx
		result = back
		currentQuality = o.extractQualityFromResult(result)
		waitedForResume.resolved()
		waitEpisode.reset()
		verifyWalled = false
		segmentStartTime = time.Now().Unix()
		partResumed = false
		if monitor != nil {
			select {
			case <-qualityChangeCh:
			default:
			}
			monitor.UpdateBaseline(currentQuality)
		}
		attachProgress(result)
		return true
	}

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
			err := runLiveDownloaders(o, ctx, result)
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

		// A refusal that left the staged recording intact can never make
		// progress on a retry inside this run — surface it now rather than
		// letting it fall through to the "stream may have ended" path below
		// (fix round 1, Minor 6).
		if stagingPreservedRefusal(downloadErr) {
			o.logger.Error("download refused to start over staged media — staging and resume state kept",
				"err", downloadErr, "jobID", jobCtx.Job.ID)
			return result, waitedForResume.value(), downloadErr
		}
		// Nor can one that cannot write its staging (a full disk, a
		// permission): every refresh would fail on its first write. Read as a
		// possible stream end, it was re-verified for up to an hour, logged
		// nowhere, and then finished as if the stream had ended — footage of
		// a live broadcast lost behind a Finished row.
		if errors.Is(downloadErr, engine.ErrLocalWrite) {
			o.logger.Error("the capture cannot be written to staging — stopping with staging and resume state kept",
				"err", downloadErr, "jobID", jobCtx.Job.ID)
			return result, waitedForResume.value(), downloadErr
		}
		// Nor can one whose playlist the engine refused as encrypted (the HLS
		// the DRM fallback fell to, drmHlsFallback, or one that turned
		// encrypted mid-stream): every refresh would fetch the same key.
		// Read as a possible stream end, it would be re-verified and then
		// finished over whatever was recorded before the key appeared, with
		// nothing said (owner decision FE-1).
		if errors.Is(downloadErr, engine.ErrDRMProtected) {
			o.logger.Error("the stream is DRM-protected — stopping",
				"err", downloadErr, "jobID", jobCtx.Job.ID)
			return result, waitedForResume.value(), drmUnrecordable(downloadErr)
		}

		// Check for reactive quality loss (download loop returned ErrQualityLost)
		isQualityLost := errors.Is(downloadErr, engine.ErrQualityLost)
		// A segment YouTube kept refusing through fresh credentials while the
		// stream moved on (engine.ErrSegmentRefused): the part is closed at
		// it and the next starts past it (owner decision D8).
		var refused *engine.SegmentRefusedError
		isRefused := errors.As(downloadErr, &refused)

		// The same-seq rebuild cap (sameSeqRebuildCap): past it, an
		// ErrQualityLost at the sequence the last rebuilds were all stuck at
		// is not rebuilt again — it falls through to the stream-end verify
		// below, which re-checks the stream and refreshes on its own paced
		// budget. A monitor-signalled quality change is not a stuck rebuild.
		rebuildCapped := false
		if isQualityLost && !qualityChanged {
			if n := rebuilds.note(result); n > sameSeqRebuildCap {
				rebuildCapped = true
				o.logger.Warn("rebuilt at the same segment as often as allowed — verifying the stream instead of rebuilding again",
					"rebuilds", n-1, "cap", sameSeqRebuildCap, "jobID", jobCtx.Job.ID)
			}
		}

		if (qualityChanged || isQualityLost || isRefused) && !rebuildCapped {
			segmentEndTime := time.Now().Unix()
			// A resumed part is never "short" — the timer only measures this
			// session's slice of it.
			shortSegment := !partResumed && time.Since(time.Unix(segmentStartTime, 0)) < minSegmentDuration

			// Re-fetch manifest FIRST to determine if quality actually changed.
			// This must happen before muxing so we can skip the split for same-quality.
			freshInfo, err := qualityChangeInfo(ctx,
				func(c context.Context) (*youtube.VideoInfo, error) {
					return liveVideoInfo(jobCtx.YT, c, jobCtx.Job.VideoID)
				},
				func() time.Duration {
					return qualityChangeQuiet(waitEpisode.active(time.Now(), jobCtx.Config.InterruptionTimeout),
						time.Since(lastSegTime.Load()))
				},
				func(c context.Context, err error) {
					o.logger.Warn("failed to refresh video info after quality change, retrying",
						"err", err, "retryIn", streamEndVerifyInterval, "jobID", jobCtx.Job.ID)
					tracker.SetWaitActivity(engine.ActivityRetrying)
					utils.Sleep(c, streamEndVerifyInterval)
				},
			)
			if ctx.Err() != nil {
				return result, waitedForResume.value(), ctx.Err()
			}
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
			// (right after refreshDownload) sees it. This is an
			// AUTHENTICATED fetch (jobCtx.YT.GetVideoInfo, live cookies) —
			// if those cookies die mid-stream this can return the same
			// live+zero-formats+auth-walled shape a healthy members-only/
			// age-restricted/login-required stream would; observe() itself
			// guards against arming on that (I4 fix), so no guard is needed
			// at this call site.
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

			// A capture on a fallback path goes back to the from-start
			// path, in a new part, once the challenges can be solved again
			// (switchBack, PR-R3). At a refused segment the switch is also
			// the split at it (owner decision D8): the new part starts each
			// stream stopped at the hole past it, and the stopped
			// downloaders record the gap rows for it, exactly as the
			// isRefused split below does. Built at the hole itself, the new
			// downloaders asked for the refused segment again, no gap row
			// was written, and the refusal loop restarted from its top.
			backVideoSeq, backAudioSeq := oldVideoSeq, oldAudioSeq
			var backVideoMoved, backAudioMoved bool
			if isRefused {
				backVideoSeq, backAudioSeq, backVideoMoved, backAudioMoved = pastRefusedSegment(refused.Seq, oldVideoSeq, oldAudioSeq)
			}
			stopped := result
			if switchBack(freshInfo, backVideoSeq, backAudioSeq) {
				if isRefused {
					noteRefusedHole(stopped, refused.Seq, backVideoMoved, backAudioMoved)
				}
				continue
			}

			// Create fresh downloaders in the current staging dir to check quality.
			// ForceStartSeq ensures they continue from where the old ones left off.
			curCtx.VideoStartSeq = oldVideoSeq
			curCtx.AudioStartSeq = oldAudioSeq

			refreshResult, refreshErr := o.refreshDownload(ctx, curCtx, freshInfo, result.IsHls)

			// Clear orchestrator seqs so they don't persist to future iterations
			curCtx.VideoStartSeq = 0
			curCtx.AudioStartSeq = 0

			// A refresh that failed on the player's JS challenges switches
			// the capture's path in a new part (switchPart).
			if refreshErr != nil && switchPart(freshInfo, refreshErr, oldVideoSeq, oldAudioSeq) {
				continue
			}

			if refreshErr != nil {
				// The interruption signature (live + zero formats) makes
				// EVERY strategy's format selection fail on freshInfo — so a
				// refresh failure here is exactly what an interruption looks
				// like, not just an ended stream. Without this check, that
				// failure used to be treated as "the stream is done" and
				// finalized immediately, discarding a recording that could
				// still resume. noteRefreshFailure (I1 fix) ALWAYS latches
				// waitedForResume when resume evidence holds, even when the
				// stall is disabled (config InterruptionTimeout <= 0) and it
				// therefore returns false below — a disabled-stall job must
				// never actually wait, but a genuinely-interrupted one must
				// still preserve staging at finalize. See noteRefreshFailure
				// and resumeEvidence's doc comments.
				//
				// I3 fix: this branch has no counter of its own (unlike the
				// still-live verify branch's maxConsecutiveLiveChecks) — a
				// refresh that keeps failing while evidence keeps holding
				// (a stuck-live abandoned broadcast) retried this branch's
				// sleep-and-continue forever. waitEpisode gives it an
				// independent hard ceiling at jobCtx.Config.InterruptionTimeout,
				// latched from this episode's first wait and cleared
				// wherever a later refresh succeeds (waitedForResume.resolved(),
				// below and at the other two success sites) — see
				// waitDeadline's doc comment.
				if noteRefreshFailure(&waitedForResume, &waitEpisode, refreshErr, jobCtx.Interruption.fresh(), mayResume, jobCtx.Config.InterruptionTimeout, time.Now()) {
					// Finalize-scoped, not history-scoped: this latches
					// true here, but every later SUCCESSFUL refresh
					// (`result = refreshResult`, below and in the
					// still-live verify branch) clears it again — a
					// broadcast that resumed, or a transient failure that
					// self-healed, must not permanently taint a clean
					// multi-hour finish with incomplete_tail=true. If the
					// loop instead gives up for good with no intervening
					// success (this same branch again on a later
					// iteration, maxConsecutiveLiveChecks exhausting via
					// the still-live verify branch, or waitEpisode's own
					// deadline expiring), this stays true — exactly the
					// Tier 2 evidence finalizeIncompleteTail needs for a
					// genuine gave-up-mid-stall finalize.
					o.logger.Warn("refresh failed but the broadcast may resume — waiting instead of ending the recording",
						"err", refreshErr, "jobID", jobCtx.Job.ID)
					tracker.SetWaitActivity(engine.ActivityWaitingResume)
					utils.Sleep(ctx, streamEndVerifyInterval)
					continue
				}
				// Either there's no resume evidence, the stall is disabled,
				// or (I3) waitEpisode's deadline has expired for this
				// episode — refuse to wait further and fall through to the
				// normal bounded exit below, exactly like a permission-
				// denied call: log and return, letting muxAndFinalize
				// process whatever was captured.
				// No resume evidence — but YouTube answered this very refresh
				// with "live", and finalizing a stream it still calls live
				// marked the job Finished mid-broadcast over what is often a
				// transient manifest or cipher fetch. Retry on the still-live
				// verify branch's cadence and budget first, from the same
				// forced position.
				//
				// A retry that fails on the player's JS challenges switches
				// the capture's path in a new part, as the first refresh's
				// failure does (switchPart), and stops the retries
				// (errLivePathSwitched): the first one may have failed for
				// another reason — a transient fetch, an interruption's
				// empty extraction — and retrying the same formats after
				// the challenges failed ran the budget out and finalized a
				// stream YouTube still called live. refreshCtx is the part
				// the retries refresh; a switch moves curCtx past it.
				refreshCtx := curCtx
				refreshCtx.VideoStartSeq, refreshCtx.AudioStartSeq = oldVideoSeq, oldAudioSeq
				refreshResult, refreshErr = o.refreshWhileLive(ctx, jobCtx, freshInfo, refreshErr, &consecutiveLiveChecks, tracker,
					func(info *youtube.VideoInfo) (*DownloadResult, error) {
						res, err := o.refreshDownload(ctx, refreshCtx, info, result.IsHls)
						if err != nil && switchPart(info, err, oldVideoSeq, oldAudioSeq) {
							return nil, errLivePathSwitched
						}
						return res, err
					})
				refreshCtx.VideoStartSeq, refreshCtx.AudioStartSeq = 0, 0
				if errors.Is(refreshErr, errLivePathSwitched) {
					continue
				}
				if cookiesStatusError(refreshErr) {
					o.logger.Error("the capture's credentials stopped working — stopping with staging kept",
						"err", refreshErr, "jobID", jobCtx.Job.ID)
					return result, waitedForResume.value(), refreshErr
				}
				if refreshErr != nil {
					o.logger.Error("failed to refresh for new quality", "err", refreshErr, "jobID", jobCtx.Job.ID)
					// Return nil to exit the live loop; muxAndFinalize will process
					// whatever video/audio data was captured before the refresh failed.
					return result, waitedForResume.value(), nil
				}
			}

			newQuality := o.extractQualityFromResult(refreshResult)

			// A refused segment closes the part at it whatever quality the
			// refresh came back at: the refresh was only the look, and its
			// downloaders — in the current part's directory — are discarded
			// unrun, as a quality split discards them. The streams stopped at
			// the hole start the next part past it (pastRefusedSegment), each
			// with a gap row for the sequence it steps over. The closed part
			// is never "short": its footage is real, and the short-span rule
			// would discard it.
			if isRefused {
				hole := refused.Seq
				var videoMoved, audioMoved bool
				oldVideoSeq, oldAudioSeq, videoMoved, audioMoved = pastRefusedSegment(hole, oldVideoSeq, oldAudioSeq)
				noteRefusedHole(result, hole, videoMoved, audioMoved)
				if exit, err := splitPart(newQuality, freshInfo, oldVideoSeq, oldAudioSeq, false, segmentEndTime, hole); exit {
					return result, waitedForResume.value(), err
				}
				continue
			}

			if !newQuality.Changed(currentQuality) && !streamIdentityChanged(result, refreshResult) {
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
				waitEpisode.reset() // this stall episode is over; a later one gets a fresh budget
				// A healthy read: the verify branch's next wall is the first again.
				verifyWalled = false

				if monitor != nil {
					select {
					case <-qualityChangeCh:
					default:
					}
					monitor.ReconcileSameQuality(currentQuality)
				}

				attachProgress(result)
				continue
			}

			// Quality actually changed — split into a new segment.
			if exit, err := splitPart(newQuality, freshInfo, oldVideoSeq, oldAudioSeq, shortSegment, segmentEndTime, -1); exit {
				return result, waitedForResume.value(), err
			}
			continue
		}

		// Normal download stop — verify stream ended (existing logic)
		timeSinceLastSeg := time.Since(lastSegTime.Load())
		o.logger.Info("segment downloaders stopped",
			"timeSinceLastSeg", timeSinceLastSeg.Round(time.Second),
			"err", downloadErr, // nil for a clean stop; otherwise logged nowhere else
			"jobID", jobCtx.Job.ID)

		// Verify stream status with YouTube API
		freshInfo, err := liveVideoInfo(jobCtx.YT, ctx, jobCtx.Job.VideoID)
		if err != nil {
			o.logger.Warn("failed to verify stream status", "err", err, "jobID", jobCtx.Job.ID)

			// A failed look is not a verdict, so it spends the same budget a
			// still-live answer does rather than ending the job. The old
			// shortcut — give up once timeSinceLastSeg reached
			// streamSegmentTimeout — always held after the engine's own
			// maximum_timeout finalize (both default to ten minutes), so one
			// bot-wall, 429 or 5xx there finished the job, staging deleted.
			// Inside a resume wait an unreadable look is one more wait tick,
			// not a spent check: the downloaders are cancelled, so quiet time
			// always reads "too long" here, and six flaky fetches would end a
			// wait interruption_timeout still allows.
			if waitEpisode.active(time.Now(), jobCtx.Config.InterruptionTimeout) {
				tracker.SetWaitActivity(engine.ActivityWaitingResume)
				utils.Sleep(ctx, streamEndVerifyInterval)
				continue
			}
			if checks := consecutiveLiveChecks.Add(1); unreadableStatusEndsCapture(checks, timeSinceLastSeg) {
				o.logger.Info("no segments for too long and the stream status stayed unreadable, assuming ended",
					"checks", checks, "jobID", jobCtx.Job.ID)
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
		// the ErrQualityLost site above for why this path needs it too,
		// including why the auth-wall guard doesn't need repeating here
		// (I4 fix: it lives centrally inside observe() itself).
		jobCtx.Interruption.observe(freshInfo)

		switch freshInfo.StreamStatus {
		case youtube.StreamPostLive, youtube.StreamVOD, youtube.StreamNotAStream:
			// Stream confirmed ended
			o.logger.Info("stream confirmed ended", "status", freshInfo.StreamStatus, "jobID", jobCtx.Job.ID)
			goto streamEnded

		case youtube.StreamLive:
			// A start the CDN no longer holds never comes back: a capture
			// that wrote nothing from a position past YouTube's ~120 h window
			// records the live HLS playlist into this part now, rather than
			// spending the still-live checks re-seeding the same evicted
			// position (evictedStartToLiveEdge, audit G11). The stopped
			// downloaders' empty DASH files are cleared before HLS writes
			// (setAsideOtherLiveShape).
			if hlsResult, ok := o.evictedStartToLiveEdge(ctx, jobCtx, freshInfo, result, func() (*DownloadResult, error) {
				r, err := o.refreshDownload(ctx, curCtx, freshInfo, true)
				if err == nil {
					err = setAsideOtherLiveShape(curCtx, r)
				}
				if err != nil {
					return nil, err
				}
				return r, nil
			}); ok {
				result = hlsResult
				currentQuality = o.extractQualityFromResult(result)
				if monitor != nil {
					select {
					case <-qualityChangeCh:
					default:
					}
					monitor.UpdateBaseline(currentQuality)
				}
				waitedForResume.resolved()
				waitEpisode.reset()
				verifyWalled = false
				attachProgress(result)
				continue
			}
			checks := consecutiveLiveChecks.Add(1)
			if checks >= maxConsecutiveLiveChecks {
				o.logger.Warn("YouTube reported live too many times with no segments, forcing end",
					"checks", checks, "jobID", jobCtx.Job.ID)
				goto streamEnded
			}

			o.logger.Info("stream still live, refreshing manifests",
				"check", checks, "max", maxConsecutiveLiveChecks, "jobID", jobCtx.Job.ID)

			// Through the tracker — see the verify-retry branch above. Inside
			// a wait for an interrupted broadcast this sleep IS the wait, and
			// says so.
			if waitEpisode.active(time.Now(), jobCtx.Config.InterruptionTimeout) {
				tracker.SetWaitActivity(engine.ActivityWaitingResume)
			} else {
				tracker.SetWaitActivity(engine.ActivityVerifyingEnd)
			}

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

			// B4: Refresh manifests and create new downloaders — from the
			// position the stopped ones reached, as the quality-loss refresh
			// does. Without a forced start they resumed through the sidecar,
			// and a sidecar the engine had already cleared (one cookieless
			// probe read "ended" that YouTube's full check then contradicted)
			// left them to the job's start-of-run DB seq: on a fresh job
			// seq 0, refused over the staged media (Error); on a restarted
			// one a stale position, re-fetching and appending footage the
			// file already held. A usable sidecar still wins over the forced
			// position in the engine.
			if result.VideoDownloader != nil {
				curCtx.VideoStartSeq = result.VideoDownloader.CurrentSeq()
			}
			if result.AudioDownloader != nil {
				curCtx.AudioStartSeq = result.AudioDownloader.CurrentSeq()
			}
			stoppedVideoSeq, stoppedAudioSeq := curCtx.VideoStartSeq, curCtx.AudioStartSeq
			if switchBack(freshInfo, stoppedVideoSeq, stoppedAudioSeq) {
				// curCtx is the new part's now: the stopped part's context,
				// and the forced start on it, are not read again.
				continue
			}
			refreshResult, refreshErr := o.refreshDownload(ctx, curCtx, freshInfo, result.IsHls)
			curCtx.VideoStartSeq, curCtx.AudioStartSeq = 0, 0

			// As at the quality-loss refresh: a refresh that failed on the
			// player's JS challenges switches the path in a new part.
			if refreshErr != nil && switchPart(freshInfo, refreshErr, stoppedVideoSeq, stoppedAudioSeq) {
				continue
			}

			if refreshErr != nil {
				credErr := liveCredentialFailure(freshInfo)
				if credErr != nil && verifyWalled {
					o.logger.Error("the capture's credentials stopped working — stopping with staging kept",
						"err", credErr, "jobID", jobCtx.Job.ID)
					return result, waitedForResume.value(), credErr
				}
				verifyWalled = credErr != nil
				if verifyWalled {
					o.logger.Warn("YouTube refused this capture's credentials — reading once more before parking the job",
						"err", credErr, "jobID", jobCtx.Job.ID)
				}
				// The quality-loss branch's wait, continued here. That branch
				// waits once: the downloaders it cancelled bring every later
				// look to this branch, which spent a live check on each, so an
				// interruption's wait ended after maxConsecutiveLiveChecks
				// (about half an hour) instead of the interruption_timeout its
				// waitEpisode is bounded by. A wait the episode allows spends
				// no live check; its cadence is the sleep above.
				if noteRefreshFailure(&waitedForResume, &waitEpisode, refreshErr, jobCtx.Interruption.fresh(), mayResume, jobCtx.Config.InterruptionTimeout, time.Now()) {
					consecutiveLiveChecks.Add(-1)
					o.logger.Warn("refresh failed but the broadcast may resume — still waiting",
						"err", refreshErr, "jobID", jobCtx.Job.ID)
					continue
				}
				o.logger.Warn("failed to refresh manifests", "err", refreshErr, "jobID", jobCtx.Job.ID)
				// Through the tracker — see the verify-retry branch above.
				tracker.SetWaitActivity(engine.ActivityVerifyingEnd)
				utils.Sleep(ctx, streamEndVerifyInterval)
				continue
			}

			// The stream may have come back at a different quality — an
			// encoder restart during the stall is the usual cause. The
			// refreshed downloaders continue the current part (through its
			// sidecar, or from the forced position above): run as-is they
			// would APPEND the new rendition's fragments under the old init
			// segment, and
			// the mixed tail would be muxed into this part before the
			// monitor's next tick noticed. Split exactly as a quality change
			// does instead; the refresh above was only the look.
			if newQuality := o.extractQualityFromResult(refreshResult); newQuality.Changed(currentQuality) || streamIdentityChanged(result, refreshResult) {
				var oldVideoSeq, oldAudioSeq int
				if result.VideoDownloader != nil {
					oldVideoSeq = result.VideoDownloader.CurrentSeq()
				}
				if result.AudioDownloader != nil {
					oldAudioSeq = result.AudioDownloader.CurrentSeq()
				}
				shortSegment := !partResumed && time.Since(time.Unix(segmentStartTime, 0)) < minSegmentDuration
				if exit, err := splitPart(newQuality, freshInfo, oldVideoSeq, oldAudioSeq, shortSegment, time.Now().Unix(), -1); exit {
					return result, waitedForResume.value(), err
				}
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
			waitEpisode.reset() // this stall episode is over; a later one gets a fresh budget
			verifyWalled = false

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
	//
	// On a row with no outcome only: an operator's Cancel whose row write
	// landed before this run's listener heard it (a slower subscriber ahead
	// of it holds the notification chain) was written over, and the final
	// part muxed. A refusal stops here, the final part left in staging; the
	// deferred Wait still lets the background part muxes land.
	if writeUnlessOutcome(o.db, jobCtx.Job.ID, map[string]any{
		"status": database.StatusMuxing,
	}) {
		return result, waitedForResume.value(), ErrCancelled
	}

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
		seg, muxErr := o.muxFinalPart(ctx, jobCtx, segmentIndex, muxStart, segmentEndTime, currentQuality, result)
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

// unreadableStatusEndsCapture reports whether the live loop's stream-end
// verification gives up on a stream whose status it cannot read: only once
// the failed looks have spent the still-live budget (checks, counted with the
// still-live answers) AND no segment has arrived for streamSegmentTimeout.
func unreadableStatusEndsCapture(checks int32, quietFor time.Duration) bool {
	return checks >= maxConsecutiveLiveChecks && quietFor >= streamSegmentTimeout
}

// credentialVerdict reports whether a playability verdict is a credential
// failure (sentinel ErrCookiesRequired or ErrNotAMember) on a player response
// that also has nothing to download — the shape that parks a job at COOKIES?
// rather than one a refresh could get past.
func credentialVerdict(info *youtube.VideoInfo, sentinel error) bool {
	return probeFormatCount(info) == 0 &&
		(errors.Is(sentinel, ErrCookiesRequired) || errors.Is(sentinel, ErrNotAMember))
}

// liveCredentialFailure returns the COOKIES?-routing error for a mid-capture
// player response whose cookies died (or whose membership wall went up), or
// nil. Without it a failed refresh on that response was retried and then
// finished like an ended stream, never reaching COOKIES? or its alert.
func liveCredentialFailure(info *youtube.VideoInfo) error {
	if info == nil {
		return nil
	}
	if msg, sentinel := playabilityVerdict(info); msg != "" && credentialVerdict(info, sentinel) {
		return fmt.Errorf("%s: %w", msg, sentinel)
	}
	return nil
}

// liveRefreshProber is the slice of the YouTube service refreshWhileLive needs.
type liveRefreshProber interface {
	GetVideoInfo(ctx context.Context, videoID string) (*youtube.VideoInfo, error)
}

// refreshWhileLive retries a live capture's failed refresh for as long as
// YouTube keeps reporting the stream live: it waits streamEndVerifyInterval,
// re-reads the player response, and refreshes again, spending the still-live
// verify branch's budget (checks against maxConsecutiveLiveChecks). It returns
// the first refresh that succeeds, or the last refresh error once the stream
// is no longer reported live, the budget is spent, or ctx ends. A failed
// re-read is not a verdict: it costs one check and the loop goes on.
//
// The live loop's refresh sites without resume evidence used to finalize on a
// single failed refresh — a transient manifest or cipher fetch — and so marked
// a stream YouTube had just called live Finished mid-broadcast.
func (o *DownloadOrchestrator) refreshWhileLive(ctx context.Context, jobCtx *JobContext, info *youtube.VideoInfo, err error,
	checks *atomic.Int32, tracker *ProgressTracker, refresh func(*youtube.VideoInfo) (*DownloadResult, error)) (*DownloadResult, error) {
	// Through the live loop's seam (liveVideoInfo), as its other player
	// reads are, so a test can drive the retries.
	var prober liveRefreshProber
	if jobCtx.YT != nil {
		yt := jobCtx.YT
		prober = liveRefreshProberFunc(func(ctx context.Context, videoID string) (*youtube.VideoInfo, error) {
			return liveVideoInfo(yt, ctx, videoID)
		})
	}
	return refreshWhileLiveWith(ctx, prober, jobCtx.Job.VideoID, info, err, checks,
		func() {
			if tracker != nil {
				tracker.SetWaitActivity(engine.ActivityRetrying)
			}
		}, liveRefreshRetryWait, refresh, o.logger, jobCtx.Job.ID)
}

// liveRefreshProberFunc is a func as a liveRefreshProber.
type liveRefreshProberFunc func(ctx context.Context, videoID string) (*youtube.VideoInfo, error)

func (f liveRefreshProberFunc) GetVideoInfo(ctx context.Context, videoID string) (*youtube.VideoInfo, error) {
	return f(ctx, videoID)
}

// errLivePathSwitched is a refresh retry's answer once its failure switched
// the capture's path in a new part (switchPart): refreshWhileLiveWith stops
// retrying on it, and the live loop goes on with the switched downloaders.
var errLivePathSwitched = errors.New("the live capture switched path in a new part")

// liveRefreshRetryWait is refreshWhileLive's pause between attempts; a
// variable so tests need not sleep it out.
var liveRefreshRetryWait = streamEndVerifyInterval

// refreshWhileLiveWith is refreshWhileLive with its collaborators passed in.
func refreshWhileLiveWith(ctx context.Context, prober liveRefreshProber, videoID string, info *youtube.VideoInfo, err error,
	checks *atomic.Int32, waiting func(), wait time.Duration, refresh func(*youtube.VideoInfo) (*DownloadResult, error),
	lg logger, jobID string) (*DownloadResult, error) {
	// walled is whether the last player read was a credential wall. One wall
	// is re-read before it is believed: a single walled answer to a healthy
	// session parked a live capture for good — a membership park waits for
	// an account change nothing will make. Two reads in a row are dead
	// credentials, and no retry gets past those.
	walled := liveCredentialFailure(info) != nil
	for prober != nil && info != nil && info.StreamStatus == youtube.StreamLive {
		if checks.Add(1) >= maxConsecutiveLiveChecks {
			return nil, err
		}
		lg.Warn("refresh failed while YouTube reports the stream live — retrying instead of ending the recording",
			"err", err, "retryIn", wait, "jobID", jobID)
		waiting()
		if sleepErr := utils.Sleep(ctx, wait); sleepErr != nil {
			return nil, sleepErr
		}
		fresh, getErr := prober.GetVideoInfo(ctx, videoID)
		if getErr != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lg.Warn("re-reading the stream status for the refresh retry failed", "err", getErr, "jobID", jobID)
			continue
		}
		info = fresh
		if info.StreamStatus != youtube.StreamLive {
			return nil, err
		}
		if credErr := liveCredentialFailure(info); credErr != nil {
			if walled {
				return nil, credErr
			}
			walled = true
			lg.Warn("YouTube refused this capture's credentials — reading once more before parking the job",
				"err", credErr, "jobID", jobID)
			continue
		}
		walled = false
		r, refreshErr := refresh(info)
		if refreshErr == nil {
			return r, nil
		}
		if errors.Is(refreshErr, errLivePathSwitched) {
			return nil, refreshErr
		}
		err = refreshErr
	}
	return nil, err
}

// qualityChangeInfo fetches the player response a quality change is judged
// against, retrying a failed fetch on the verify branch's cadence. Both
// downloaders are already stopped when it runs, so a single failure used to end
// a still-live job in Error over what is usually a transient API fault — while
// the verify branch's own failed status fetch, a few lines further down the
// loop, waits and asks again. It gives up on the same clock that branch does:
// once segments have been quiet for streamSegmentTimeout the error is returned
// (staging intact, so the job can be resumed). quietFor reads that clock;
// pause logs and waits before the next attempt.
func qualityChangeInfo(
	ctx context.Context,
	fetch func(context.Context) (*youtube.VideoInfo, error),
	quietFor func() time.Duration,
	pause func(context.Context, error),
) (*youtube.VideoInfo, error) {
	for {
		info, err := fetch(ctx)
		if err == nil || ctx.Err() != nil || quietFor() >= streamSegmentTimeout {
			return info, err
		}
		pause(ctx, err)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
}

// qualityChangeQuiet is the quiet time qualityChangeInfo gives up on. Inside a
// resume wait the downloaders are cancelled, so the time since the last
// segment always reads "too long" there, and the first failed fetch ended a
// wait interruption_timeout still allowed, the job in Error. The wait's own
// deadline bounds the retries instead (waiting is waitEpisode.active), as it
// bounds the verify branch's unreadable looks.
func qualityChangeQuiet(waiting bool, sinceLastSeg time.Duration) time.Duration {
	if waiting {
		return 0
	}
	return sinceLastSeg
}

// refreshDownload re-creates downloaders for an in-progress live stream from
// freshly fetched video info, mirroring the initial strategy selection in
// ExecuteWithChat: HLS jobs stay HLS; DASH jobs prefer manifest-free DASH
// whenever the refreshed response ships split adaptive URLs (the primary
// live path since yt-dlp 8c1f07d81), falling back to the manifest-driven
// downloader otherwise. Without the manifestless branch, a manifestless
// job's first transient quality blip or stall refresh would fail with "no
// DASH manifest URL" and end the recording mid-live.
//
// A capture the run moved onto the formats that need no player JS — the
// challenge-free setup fallback, or a mid-stream path switch
// (runRecord.noJS) — refreshes on them (noJSSetup: the fresh extraction's
// copies that need none, and one cookieless re-extract's), the owner's
// decision on W8's second follow-up: the fresh extraction ranks a tv copy
// with an n first, and a refresh of the whole pool picked it and failed on it
// while the challenges still could not be solved.
func (o *DownloadOrchestrator) refreshDownload(ctx context.Context, jobCtx *JobContext, freshInfo *youtube.VideoInfo, isHls bool) (*DownloadResult, error) {
	if isHls {
		return DownloadHls(ctx, jobCtx, freshInfo, o.routedCipher, o.cipherSolver, o.potProvider, connIsOnline(o.conn))
	}
	if jobCtx.Run.noJS() {
		return noJSSetup(ctx, jobCtx, freshInfo, o.strategyDeps(), &cookielessReExtract{}, errNoJSRefresh, "no-JS refresh")
	}
	if HasManifestlessDashFormats(freshInfo.Formats) {
		return DownloadManifestlessDash(ctx, jobCtx, freshInfo, o.routedCipher, o.cipherSolver, o.potProvider, connIsOnline(o.conn))
	}
	return DownloadDash(ctx, jobCtx, freshInfo, o.routedCipher, o.cipherSolver, o.potProvider, connIsOnline(o.conn))
}

// strategyDeps is the StrategyDeps a setup outside ExecuteWithChat's own —
// a refresh's, a path switch's — builds from: the orchestrator's solvers,
// minter and connectivity, as ExecuteWithChat hands its setup.
func (o *DownloadOrchestrator) strategyDeps() *StrategyDeps {
	return &StrategyDeps{
		CipherSolver:       o.cipherSolver,
		RoutedCipherSolver: o.routedCipher,
		PotProvider:        o.potProvider,
		IsOnline:           connIsOnline(o.conn),
	}
}

// youtubeProbeClient is the narrow slice of *youtube.Service the quality probe
// uses. Named as an interface so the probe's ROUTING — which is all owner
// decision O-H changes — is testable without a network round trip;
// *youtube.Service satisfies it.
type youtubeProbeClient interface {
	ProbeVideoStatus(ctx context.Context, videoID string) (*youtube.VideoInfo, error)
	ProbeVideoStatusAuthenticated(ctx context.Context, videoID string) (*youtube.VideoInfo, error)
	GetVideoInfo(ctx context.Context, videoID string) (*youtube.VideoInfo, error)
	// HasAuthCookies gates the cookied recovery hop and labels the Debug
	// line: the TV probe sends whatever the jar holds and never checks, so
	// without this the probe would spend a bot-walled call per tick on a
	// cookieless install and claim credentials it never sent.
	HasAuthCookies() bool
}

// isSubstitution reports whether a probe error is YouTube answering about a
// DIFFERENT video — the substitute an IP-blocked or rate-limited source is
// served (youtube.VideoIDMismatchError, upstream's "invalid player response").
//
// It is the one probe error that must NOT fall through to the cascade. Every
// other failure is client-specific and the cascade routinely answers around
// it; a substitution is IP-level, so all seven clients are substituted alike
// and the cascade can only re-ask the same blocked address seven more times
// — 9 player calls per 30 s tick for the whole episode, toward a service
// that is already rate-limiting this install.
func isSubstitution(err error) bool {
	var mismatch *youtube.VideoIDMismatchError
	return errors.As(err, &mismatch)
}

// tvEscalationAsked reports whether a cookieless status probe already made
// the call hop 2 would: its signed-in escalation, the authenticated TV probe
// (youtube.StatusProbeEscalationTVAuth), asked once inside ProbeVideoStatus —
// whether that answered (the answer is tagged with it) or settled nothing
// (the probe's *youtube.StatusProbeInconclusiveError names it).
func tvEscalationAsked(info *youtube.VideoInfo, err error) bool {
	if info != nil && info.StatusProbeEscalation == youtube.StatusProbeEscalationTVAuth {
		return true
	}
	inconclusive, ok := errors.AsType[*youtube.StatusProbeInconclusiveError](err)
	return ok && inconclusive.Escalation == youtube.StatusProbeEscalationTVAuth
}

// probeIsSelectable reports whether a probe answer carries something the
// quality monitor can actually select from: a DASH manifest to parse, or a
// split-adaptive pool to pick in memory. Anything else — nil, an error shell,
// a playability wall, a whole-file-only pool — means the tick has no quality
// to compare and must fall through to the full cascade.
func probeIsSelectable(info *youtube.VideoInfo) bool {
	return info != nil && (info.DashManifestURL != "" || HasManifestlessDashFormats(info.Formats))
}

// probeVideoInfo fetches the VideoInfo one quality-monitor tick selects from.
//
// Owner decision O-H. Before it, an auth-walled stream ran the FULL
// authenticated cascade every 30 s — a 1-5 MB cookied watch page plus three to
// seven player calls, roughly 120 pages and 360+ player calls per hour per job
// — and the dominant waste was the OTHER branch: a public stream whose
// cookieless android_vr probe returned neither DASH nor split-adaptive paid
// that probe AND the whole cascade, every tick.
//
// Both kinds now start cheap: ANDROID_VR when nothing is walled (cookieless,
// no POT, no watch page), TV_SAMSUNG-with-cookies when something is
// (ProbeVideoStatusAuthenticated — one player call, no watch page, no STS, no
// POT; android_vr would 401 on members-only content).
//
// O-H's second clause — "auth-walled/RECOVERY streams" — is the cookied hop:
// when the cookieless probe comes back with nothing selectable and this
// install HAS cookies, one TV-with-cookies call is tried before the cascade.
// That is the branch the verifier called dominant (requiresAuth is true only
// while our cookies do NOT grant access, which is rare mid-download), so
// without the hop the decision would have saved almost nothing in the field.
// It is gated on HasAuthCookies because a cookieless TV call is bot-walled
// ("Sign in to confirm you're not a bot", Step 0's live evidence) and would be
// a wasted call every tick.
//
// The full cascade remains as a ONE-SHOT fallback behind every branch, fired
// when nothing cheaper produced a DASH manifest or a split-adaptive pool —
// including when a probe ERRORS. That last part is the "never make recovery
// worse" ruling: the cascade pools three to seven clients and routinely
// answers when one of them 403s, so one failing player call must not end the
// tick (QualityMonitor.Run only logs a probe error at Debug and skips, so an
// auth-walled stream whose TV client started failing would have lost quality
// monitoring for the rest of the broadcast, silently).
//
// TWO errors are exceptions. A cancelled context: the cascade cannot succeed
// during a shutdown race, so nothing is bought there. And a substitution
// (*youtube.VideoIDMismatchError, isSubstitution): it ends the tick at the hop
// that saw it — no hop 2 after a hop-1 mismatch, no cascade after either —
// because a substitute is an IP-level positive signal that the cascade can
// only repeat seven more times against a server that is already blocking this
// address (close-review Finding 1; it was an ordinary error here until then).
//
// At most ONE cascade per tick, always — never a loop, never a retry ladder.
// The worst case a tick can cost is therefore probe + hop + cascade, i.e. two
// player calls on top of what it cost before O-H, and only for a cookied
// install whose stream answers nothing at every hop. The probe in that sum
// counts its own escalation (G8): a cookieless refusal that says nothing
// about the broadcast is asked once more inside ProbeVideoStatus — signed
// out, a web_embedded call plus its embed page, so android_vr + embed page +
// web_embedded + cascade; signed in, the TV call that is hop 2, which is then
// not asked again (tvEscalationAsked), so android_vr + TV + cascade.
//
// The monitor only needs the format pool in memory; nothing on this path
// consumes the watch-page metadata the cascade used to refresh (O-H's
// acknowledged, visible change: less log volume and a smaller request
// signature toward YouTube).
//
// lg may be nil (the routing tests pass nil); it carries the Debug line that
// names the branch each tick took — the only way an operator can tell a tick
// that resolved on one player call from one that still paid for the cascade.
func probeVideoInfo(ctx context.Context, yt youtubeProbeClient, videoID string, requiresAuth bool, lg logger) (*youtube.VideoInfo, error) {
	debug := func(msg string, args ...any) {
		if isNilLogger(lg) {
			return
		}
		lg.Debug(msg, args...)
	}

	cookied := yt.HasAuthCookies()
	tvKind := "tv_samsung"
	if cookied {
		tvKind += "+cookies"
	}

	// Hop 1 — the cheap probe. ANDROID_VR when nothing is walled, TV with
	// cookies when something is (android_vr 401s on members-only content).
	var info *youtube.VideoInfo
	var err error
	probeKind := "android_vr"
	if requiresAuth {
		probeKind = tvKind
		info, err = yt.ProbeVideoStatusAuthenticated(ctx, videoID)
	} else {
		info, err = yt.ProbeVideoStatus(ctx, videoID)
	}
	if err == nil && probeIsSelectable(info) {
		debug("quality probe answered on one player call",
			"videoID", videoID, "probe", probeKind, "escalation", info.StatusProbeEscalation,
			"splitAdaptive", HasManifestlessDashFormats(info.Formats),
			"dashManifest", info.DashManifestURL != "")
		return info, nil
	}
	if isSubstitution(err) {
		return nil, err
	}
	probeErr := err
	// probeHop names the hop probeErr came from, which is NOT always the
	// probe the tick started with: hop 2's error was attributed to hop 1's
	// client in the fallback line ("probe android_vr reason \"hop 403\"").
	probeHop := probeKind

	// Hop 2 — the cookied recovery hop (O-H's "recovery streams"). Only for
	// the cookieless branch: the auth-walled branch already made this exact
	// call above.
	//
	// Hop 1 may have made it already: a cookieless refusal that says nothing
	// about the broadcast is escalated inside ProbeVideoStatus, to this very
	// call when the jar is signed in (G8, internal/youtube/status_probe.go).
	// Its answer, when it gave one, came back as hop 1's and was not
	// selectable, or it settled nothing; asked again here it would say the
	// same, one TV call per tick for as long as the wall stood, so the tick
	// goes on to the cascade, as it would after a hop 2 that found nothing
	// (tvEscalationAsked).
	if !requiresAuth && cookied && ctx.Err() == nil && !tvEscalationAsked(info, err) {
		hopInfo, hopErr := yt.ProbeVideoStatusAuthenticated(ctx, videoID)
		if hopErr == nil && probeIsSelectable(hopInfo) {
			debug("quality probe answered on the cookied recovery hop",
				"videoID", videoID, "probe", tvKind,
				"splitAdaptive", HasManifestlessDashFormats(hopInfo.Formats),
				"dashManifest", hopInfo.DashManifestURL != "")
			return hopInfo, nil
		}
		if isSubstitution(hopErr) {
			return nil, hopErr
		}
		if probeErr == nil {
			probeErr = hopErr
			probeHop = tvKind
		}
	}

	// A shutdown race buys nothing: every call the cascade would make is
	// waste on the way out. Report the probe's own error when it had one so
	// the reason is not replaced by a bare context error.
	if cerr := ctx.Err(); cerr != nil {
		if probeErr != nil {
			return nil, probeErr
		}
		return nil, cerr
	}

	// Hop 3 — the one-shot cascade.
	reason := "the cheap probe returned nothing selectable"
	if probeErr != nil {
		reason = probeErr.Error()
	}
	debug("quality probe fell back to the full fetch",
		"videoID", videoID, "probe", probeKind, "hop", probeHop, "reason", reason,
		"status", probeStatus(info), "formats", probeFormatCount(info))
	info, err = yt.GetVideoInfo(ctx, videoID)
	if err != nil {
		return nil, fmt.Errorf("probe fallback to the full fetch: %w", err)
	}
	if info == nil {
		// Defensive: the cascade can return (nil, nil) when fetchWatchPage
		// and every Innertube client fail without surfacing a hard error.
		// Treat that as a transient probe error rather than letting the next
		// dereference panic the monitor goroutine.
		return nil, fmt.Errorf("probe fallback returned nil info")
	}
	return info, nil
}

// probeStatus and probeFormatCount read a possibly-nil probe answer for the
// fallback's Debug line — the probe may have returned (nil, nil), which is
// exactly the case worth logging.
func probeStatus(info *youtube.VideoInfo) youtube.StreamStatus {
	if info == nil {
		return ""
	}
	return info.StreamStatus
}

func probeFormatCount(info *youtube.VideoInfo) int {
	if info == nil {
		return 0
	}
	return len(info.Formats)
}

// buildYouTubeProbeFn creates a quality probe function for YouTube streams.
// The probe re-fetches the format pool and selects the best stream, returning
// the quality that would be selected under current preferences.
//
// requiresAuth: when true the probe uses ProbeVideoStatusAuthenticated
// (TV_SAMSUNG with cookies) — required for members-only, age-restricted and
// login-required streams, where the cookieless ANDROID_VR probe 401s. When
// false it uses ProbeVideoStatus (ANDROID_VR, cookieless, no POT), and a
// cookied install retries once through the TV probe before paying for the
// cascade. See probeVideoInfo for that recovery hop, for the one-shot
// full-fetch fallback behind every branch (an erroring probe included), and
// for owner decision O-H — why the full cascade is no longer the FIRST call on
// either kind.
func (o *DownloadOrchestrator) buildYouTubeProbeFn(jobCtx *JobContext, requiresAuth bool) func(context.Context) (*QualityInfo, error) {
	maxRes := jobCtx.Config.MaxVideoResolution
	videoItag := jobCtx.Config.VideoItag
	qualityPref := jobCtx.Job.QualityPreference
	// The download's own selection inputs, prefer_60fps included: a probe
	// that ranked frame rates differently from the downloader would see a
	// "quality change" on every tick and split the recording every 30 s.
	prefer60fps := jobCtx.Config.Prefer60fps
	preferHDR := jobCtx.Config.PreferHDR
	probeLog := newScopedLogger(o.logger, "jobID", jobCtx.Job.ID)

	return func(ctx context.Context) (*QualityInfo, error) {
		info, err := probeVideoInfo(ctx, jobCtx.YT, jobCtx.Job.VideoID, requiresAuth, probeLog)
		if err != nil {
			return nil, err
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
			best := SelectBestDashStream(videoPool, videoItag, maxRes, true, qualityPref, prefer60fps, preferHDR)
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

		// The download's own conversion, so the probe ranks what it ranks.
		streamInfos := manifestStreamInfos(streams, info.AudioLanguages, info.Formats)

		// Select best video stream using same criteria as the download
		best := SelectBestDashStream(streamInfos, videoItag, maxRes, true, qualityPref, prefer60fps, preferHDR)
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
