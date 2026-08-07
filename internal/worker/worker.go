package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// Error-classification sentinels. Producers wrap their errors with these
// via fmt.Errorf("...: %w", sentinel); consumers use errors.Is to detect
// the category. Replaces the prior string-matching helpers (audit
// reports/cross-cutting.md C3 follow-up).

// ErrCookiesRequired marks errors where the job should transition to
// StatusCookies rather than generic StatusError. Producers attach this
// sentinel to player-API "Login required" / "Member-only" failures and
// to any explicit cookies-needed signal.
var ErrCookiesRequired = errors.New("cookies required (auth needed)")

// ErrNonActionable marks errors where there's nothing the user can do
// (age-restricted content, exhausted retry budgets). Notification
// dispatch is suppressed for these to avoid noisy "your stream failed"
// pings about content that was never going to succeed.
var ErrNonActionable = errors.New("non-actionable error")

// ErrCancelled is the sentinel used by the StreamProcessor when a
// download was cancelled mid-flight (ctx.Done before live, user-cancel
// during upcoming wait). Lets the worker pick the cancelled-status
// branch without comparing error strings.
var ErrCancelled = errors.New("cancelled")

// heartbeatInterval is the safety-net poll interval for catching missed jobs.
// Normal job discovery is signal-driven via NotifyNewJob. The backlog
// scheduler reuses it as its sweep heartbeat (spec §10) — its normal path is
// also signal-driven, via Scheduler.Wake.
const heartbeatInterval = 60 * time.Second

// MaxTwitchAutoRetries caps how many times the Twitch monitor's auto-recovery
// will re-enqueue an errored "twitch channel is offline" job before giving up.
// 2 retries (3 total attempts including the original) handles transient GQL
// flaps measured in seconds without looping indefinitely on a persistent
// issue. User-driven Reinit always resets this counter (see ReinitializeJob).
const MaxTwitchAutoRetries = 2

// logger is the anonymous interface for logging — intentionally not exported.
// Each struct repeats this inline per CLAUDE.md convention.
type logger = interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// JobContext holds the context needed to process a single job.
type JobContext struct {
	Job           *database.Job
	Config        *JobConfig
	YT            *youtube.Service
	DB            *database.Database
	StagingDir    string
	OutputDir     string
	Filename      string
	VideoStartSeq int // Orchestrator-set: exact next video seq to download (0 = use DB fallback)
	AudioStartSeq int // Orchestrator-set: exact next audio seq to download (0 = use DB fallback)
	Logger        logger
}

// JobConfig holds per-job configuration derived from the global config.
type JobConfig struct {
	MaxVideoResolution int
	Prefer60fps        bool
	VideoItag          int
	AudioItag          int
	OutputDirectory    string
	StagingDirectory   string
	FilenameTemplate   string
	DownloadChat       bool
	MaximumTimeout     int
}

// DownloadWorker manages the job processing loop.
type DownloadWorker struct {
	db           *database.Database
	yt           *youtube.Service
	tw           *twitch.Service
	cfg          *config.MoomboxConfig // captured for early-init reads before SetConfigStore
	configStore  *config.Store         // shared config store (set via SetConfigStore)
	queue        *JobQueue
	scheduler    *Scheduler
	orchestrator *DownloadOrchestrator
	streamProc   *StreamProcessor
	notifier     *notifications.Manager
	logger       logger
	wg           sync.WaitGroup // tracks in-flight processJob goroutines
	notifyJob    chan struct{}  // signal to re-check for new jobs (non-blocking send)

	// OnCookieRefreshNeeded is called when auth fails and auto-refresh should be attempted.
	// Returns true if cookies were refreshed successfully.
	OnCookieRefreshNeeded func() bool
}

// readConfig runs fn under configStore's read lock when the store has been
// wired (post-SetConfigStore). During the brief early-init window between
// NewDownloadWorker and main.go's SetConfigStore call, fn runs against
// w.cfg directly without locking — at that point no other goroutine holds
// or contends for the cfg mutex.
func (w *DownloadWorker) readConfig(fn func(*config.MoomboxConfig)) {
	if w.configStore != nil {
		w.configStore.Read(fn)
		return
	}
	fn(w.cfg)
}

// DownloadWorkerDeps holds optional dependencies for the download worker.
// Conn carries both IsOnline + OnStateChange — previously two separate func
// fields; merged into a single Connectivity interface so callers (and tests)
// can pass a real *connectivity.Monitor or a fake without the two-func dance
// (audit reports/worker.md F54).
type DownloadWorkerDeps struct {
	// CipherSolver is the legacy *GojaResolver; kept for GetSts,
	// InvalidateSolver, and goja-internal call sites.
	CipherSolver *cipher.GojaResolver

	// RoutedCipherSolver is the composite cipher.Solver (sidecar
	// primary, goja fallback) used for sig/n-param URL decryption in
	// download strategies.  nil falls back to CipherSolver.
	RoutedCipherSolver cipher.Solver

	PotProvider   *bgutils.PotProvider
	TwitchService *twitch.Service
	Notifier      *notifications.Manager
	Conn          Connectivity
}

// NewDownloadWorker creates a new download worker.
func NewDownloadWorker(
	db *database.Database,
	yt *youtube.Service,
	cfg *config.MoomboxConfig,
	logger logger,
	deps *DownloadWorkerDeps,
) *DownloadWorker {
	queue := NewJobQueue(cfg.Downloader.NumParallelDownloads)
	queue.SetLogger(logger)

	var cs *cipher.GojaResolver
	var routedCs cipher.Solver
	var pp *bgutils.PotProvider
	var tw *twitch.Service
	var nm *notifications.Manager
	var conn Connectivity
	if deps != nil {
		cs = deps.CipherSolver
		routedCs = deps.RoutedCipherSolver
		pp = deps.PotProvider
		tw = deps.TwitchService
		nm = deps.Notifier
		conn = deps.Conn
	}

	sp := NewStreamProcessor(yt, tw, cfg, db, logger)
	if nm != nil {
		sp.SetNotifier(nm)
	}
	if conn != nil {
		sp.SetIsOnline(conn.IsOnline)
	}

	sched := newScheduler(db, queue, logger)
	// Slot-release flips (spec §10) free an archive slot mid-flight — a
	// backlog job going Live or entering the upcoming wait stops counting in
	// M. The wake lets the scheduler admit the channel's next backlog VOD
	// promptly instead of on the heartbeat.
	sp.SetWakeScheduler(sched.Wake)

	return &DownloadWorker{
		db:           db,
		yt:           yt,
		tw:           tw,
		cfg:          cfg,
		queue:        queue,
		scheduler:    sched,
		orchestrator: NewDownloadOrchestrator(db, queue, cfg.Paths.FfmpegPath, logger, cs, routedCs, pp, nm, conn),
		streamProc:   sp,
		notifier:     nm,
		logger:       logger,
		notifyJob:    make(chan struct{}, 1),
	}
}

// Start begins the worker loop, processing jobs from the queue.
func (w *DownloadWorker) Start(ctx context.Context) {
	w.logger.Info("download worker started")

	// Enqueue existing pending jobs
	w.enqueueExistingJobs()

	// Poll for new jobs periodically
	go w.pollForJobs(ctx)

	// Backlog admission (spec §10). The scheduler owns the only path out of
	// Queued — pollForJobs never touches those rows (ShouldProcess is false
	// for Queued by design) — so Run carries its own restart-on-panic wrapper.
	go w.scheduler.Run(ctx)

	// Process jobs from queue
	for {
		jobID, jobCtx, ok := w.queue.Dequeue(ctx)
		if !ok {
			return // Context cancelled
		}

		w.wg.Go(func() {
			defer func() {
				if r := recover(); r != nil {
					w.logger.Error("panic in processJob", "jobID", jobID, "panic", fmt.Sprint(r))
					w.db.UpdateJobFields(jobID, map[string]any{
						"status": database.StatusError,
						"error":  fmt.Sprintf("internal panic: %v", r),
					})
				}
			}()
			w.processJob(jobCtx, jobID)
		})
	}
}

// EnqueueJob adds a job to the processing queue.
// Looks up the job from DB to determine its priority.
func (w *DownloadWorker) EnqueueJob(jobID string) {
	job, err := w.db.GetJob(jobID)
	if err != nil || job == nil {
		w.queue.Enqueue(jobID, database.StatusUpcoming)
	} else {
		w.queue.Enqueue(jobID, job.Status)
	}
	// Signal the poll loop to re-check (non-blocking)
	select {
	case w.notifyJob <- struct{}{}:
	default:
	}
}

// Scheduler returns the worker's archive-slots scheduler. Creation sites
// call Scheduler().Wake() after inserting a Queued backlog job instead of
// EnqueueJob — the scheduler, not the queue, admits backlog work.
func (w *DownloadWorker) Scheduler() *Scheduler {
	return w.scheduler
}

// SetArchiveSlotsResolver injects the per-channel archive_slots resolver the
// backlog scheduler consults on every admission sweep (spec §10). The host
// (cmd/moombox) builds it against the live config store so config edits take
// effect without restart. Must be called before Start — the field is only
// read from the scheduler's Run goroutine, which Start launches.
func (w *DownloadWorker) SetArchiveSlotsResolver(fn func(channelID string) int) {
	w.scheduler.resolveSlots = fn
}

// StashTwitchStreamInfo forwards a fresh Twitch stream info hint to the
// underlying StreamProcessor. Called by cmd/moombox's OnStreamFound /
// OnStreamRecover monitor callbacks so the processor doesn't re-fetch what
// the monitor just successfully fetched.
func (w *DownloadWorker) StashTwitchStreamInfo(jobID string, info *twitch.TwitchStreamInfo) {
	if w.streamProc != nil {
		w.streamProc.StashTwitchStreamInfo(jobID, info)
	}
}

// TwitchHintStats forwards the underlying StreamProcessor's hint cache
// counters. Returns zero values if streamProc isn't wired (early-init test
// harness paths). Exposed so the /api/stats endpoint can render hit/miss
// rates without reaching through the StreamProcessor directly.
func (w *DownloadWorker) TwitchHintStats() TwitchHintStats {
	if w.streamProc == nil {
		return TwitchHintStats{}
	}
	return w.streamProc.TwitchHintStats()
}

// CancelJob cancels a running job and updates its status.
// CancelJob cancels a job. Returns true when an actively-processing run was
// flagged — that run's handleCancellation emits the "cancelled"
// notification, so callers that notify should skip their own emission.
func (w *DownloadWorker) CancelJob(jobID string) bool {
	flagged := w.queue.Cancel(jobID)
	w.db.UpdateJobFields(jobID, map[string]any{
		"status": database.StatusCancelled,
	})
	return flagged
}

// WaitForJobExit blocks until the job's orchestrator goroutine has returned,
// or the timeout expires. Returns true if the job exited cleanly within the
// timeout. Used by delete paths to ensure the worker drains before the DB
// row is removed (prevents orphaned goroutines that spam UpdateJobFields
// against a deleted row).
//
// Signal-driven: the queue closes a per-job channel when processJob returns,
// so this select wakes immediately on exit rather than polling every 100ms.
func (w *DownloadWorker) WaitForJobExit(jobID string, timeout time.Duration) bool {
	select {
	case <-w.queue.Done(jobID):
		return true
	case <-time.After(timeout):
		return !w.queue.IsProcessing(jobID)
	}
}

func (w *DownloadWorker) enqueueExistingJobs() {
	jobs, err := w.db.GetAllJobs()
	if err != nil {
		w.logger.Error("failed to get existing jobs", "err", err)
		return
	}

	for _, job := range jobs {
		// Reset Muxing jobs to Downloading — muxing was interrupted by shutdown
		// and is idempotent (partial output is overwritten). Clear any stale
		// error string so the UI doesn't show a prior error alongside the fresh
		// Downloading state (per audit reports/worker.md Finding 24).
		if job.Status == database.StatusMuxing {
			w.logger.Info("resetting interrupted mux job", "jobID", job.ID)
			w.db.UpdateJobFields(job.ID, map[string]any{
				"status": database.StatusDownloading,
				"error":  "",
			})
			job.Status = database.StatusDownloading
		}
		if ShouldProcess(job) {
			w.queue.Enqueue(job.ID, job.Status)
		}
	}
}

// pollForJobs is signal-driven: wakes on NotifyNewJob signals or a 60s safety heartbeat.
// Most job discovery happens via explicit EnqueueJob calls; this is a catch-all.
// Wraps the ticker loop in a restart-on-panic pattern so a single panic doesn't
// permanently kill the heartbeat poller.
func (w *DownloadWorker) pollForJobs(ctx context.Context) {
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					w.logger.Error("pollForJobs panic, restarting", "panic", fmt.Sprint(r))
				}
			}()

			ticker := time.NewTicker(heartbeatInterval)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-w.notifyJob:
				case <-ticker.C:
				}

				jobs, err := w.db.GetAllJobs()
				if err != nil {
					continue
				}
				for _, job := range jobs {
					if ShouldProcess(job) && !w.queue.IsProcessing(job.ID) {
						w.queue.Enqueue(job.ID, job.Status)
					}
				}
			}
		}()

		// Check if context is done before restarting.
		// ctx-aware sleep so shutdown during the pause returns promptly.
		if err := utils.Sleep(ctx, time.Second); err != nil {
			return
		}
	}
}

// acquireDownloadSlot is the download pool's single acquire site. The pool
// (num_parallel_downloads) gates VODs only (spec §10/§14): a broadcast is
// never made to wait for a slot — missing a slot on a VOD delays a file
// that already exists; missing it on a broadcast loses the recording. The
// predicate is the probe result's IsVod, NOT job status, which disagrees in
// two cases: not_a_stream + AllowNonStream/ManuallyAdded downloads a
// non-stream upload as a VOD (handleStreamStatus), and a Twitch recovery
// job can carry a live status while its download is the VOD
// (processTwitchVod routes by the tw_v VideoID prefix). Twitch live is
// therefore unbounded by this pool — intended (§14). No matching release
// guard is needed: ReleaseDownloadSlot and Complete are both keyed by
// holdingDlSlot, so a never-acquired broadcast's release calls are no-ops.
// Returns false only when ctx was cancelled while waiting.
func (w *DownloadWorker) acquireDownloadSlot(ctx context.Context, jobID string, isVod bool) bool {
	if !isVod {
		return true
	}
	return w.queue.AcquireDownloadSlot(ctx, jobID)
}

func (w *DownloadWorker) processJob(ctx context.Context, jobID string) {
	defer func() {
		w.queue.Complete(jobID)
		// Every job exit funnels through here — Finished, Error, Cancelled,
		// COOKIES? — and all of those drop out of the scheduler's M
		// allow-list, so a freed archive slot may exist. Wake the scheduler
		// to admit the channel's next backlog VOD now rather than on its
		// heartbeat. Coalesced + non-blocking; harmless when nothing freed.
		w.scheduler.Wake()
	}()

	job, err := w.db.GetJob(jobID)
	if err != nil {
		w.logger.Error("get job failed", "jobID", jobID, "err", err)
		return
	}
	if job == nil {
		// Row deleted between enqueue and processing — nothing to do.
		w.logger.Debug("job vanished before processing", "jobID", jobID)
		return
	}

	// Check if job is already in a terminal state (stale check)
	if isTerminalStatus(job.Status) {
		w.logger.Debug("skipping terminal job", "jobID", jobID, "status", job.Status)
		return
	}

	// Create a per-job context so we can cancel the full lifecycle (stream
	// processing, slot wait, download, mux) on a job-deleted event — not just
	// the post-AcquireDownloadSlot phases that ExecuteWithChat covers.
	// cancel() is idempotent; ExecuteWithChat and ExecuteTwitch derive their
	// own child contexts from this one, so the cancellation propagates.
	jobLifecycleCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Subscribe to job deletion for the full lifecycle. Job row vanishing
	// (DeleteJob or UpdateJobFields hitting sql.ErrNoRows mid-write) calls
	// notifyJobDeleted; this covers every delete path rather than just the
	// UI's cancel-then-delete sequence. Duplicate fires are benign: cancel() is idempotent.
	unsubscribeDel := w.db.OnJobDeleted(func(deleted *database.JobDeleted) {
		if deleted.JobID == jobID {
			w.logger.Debug("job row deleted; cancelling processJob", "jobID", jobID)
			cancel()
		}
	})
	defer unsubscribeDel()

	ctx = jobLifecycleCtx

	w.logger.Info("processing job", "jobID", jobID, "videoID", job.VideoID)

	// Process stream (probe, wait for live, etc.)
	result, err := w.streamProc.Process(ctx, job)
	if err != nil {
		if ctx.Err() != nil {
			w.handleCancellation(job, "")
			return
		}
		w.setJobError(job, err)
		return
	}

	if !result.ShouldDownload {
		if result.Error == "cancelled" {
			// "cancelled" comes from waitForLive on ctx.Done() or DB status change.
			// Route through handleCancellation so shutdown preserves state.
			w.handleCancellation(job, "")
			return
		}
		if result.Error != "" {
			// AsError preserves any ErrSentinel attached by the producer
			// (e.g. ErrCookiesRequired from checkPlayability) so
			// setJobError's errors.Is checks fire correctly. Without
			// this wrap, the prior code's errors.New stripped the
			// sentinel and forced setJobError back to string-matching.
			w.setJobError(job, result.AsError())
		}
		return
	}

	// Check cancellation between stream processing and download
	if ctx.Err() != nil {
		w.handleCancellation(job, "")
		return
	}

	// Acquire download slot — for VODs, blocks until a slot is available;
	// broadcasts pass through ungated (see acquireDownloadSlot).
	// Lifecycle slot (from Dequeue) allows stream processing to proceed without
	// consuming download slots; actual downloading requires a separate download slot.
	if !w.acquireDownloadSlot(ctx, jobID, result.IsVod) {
		// Context cancelled while waiting for download slot
		w.handleCancellation(job, "")
		return
	}

	// Build job context
	jobCtx := w.buildJobContext(job)

	// Route to platform-specific orchestrator
	var maxRes int
	w.readConfig(func(c *config.MoomboxConfig) {
		maxRes = c.Downloader.MaxVideoResolution
	})
	var dlErr error
	if job.Platform == "twitch" && result.TwitchVariant != nil {
		variant := &TwitchVariantInfo{
			URL:           result.TwitchVariant.URL,
			Name:          result.TwitchVariant.Name,
			Width:         result.TwitchVariant.Width,
			Height:        result.TwitchVariant.Height,
			FPS:           result.TwitchVariant.FPS,
			QualityPref:   job.QualityPreference,
			MaxResolution: maxRes,
		}
		// Stable broadcast identity for engine resume validation: the live
		// stream ID when known, else the job's video/VOD ID.
		if result.TwitchStreamInfo != nil && result.TwitchStreamInfo.StreamID != "" {
			variant.StreamID = result.TwitchStreamInfo.StreamID
		} else {
			variant.StreamID = job.VideoID
		}
		// For live streams, provide a stream-end check function and quality probe
		if !result.IsVod && result.TwitchStreamInfo != nil && w.tw != nil {
			login := result.TwitchStreamInfo.ChannelLogin
			variant.CheckStreamFn = func(innerCtx context.Context) (bool, error) {
				info, err := w.tw.GetStreamInfo(innerCtx, login)
				if err != nil {
					return false, err
				}
				return info != nil && info.IsLive, nil
			}
			variant.RecheckStreamFn = func(innerCtx context.Context) (*twitch.TwitchStreamInfo, error) {
				return w.tw.GetStreamInfo(innerCtx, login)
			}
			variant.FetchVariantsFn = func(innerCtx context.Context) ([]twitch.TwitchHLSVariant, error) {
				return w.tw.GetHLSMasterPlaylist(innerCtx, login)
			}
		}
		// Determine which Twitch chat downloader to use
		var twitchChat ChatSource
		if result.TwitchChatDownloader != nil {
			twitchChat = result.TwitchChatDownloader
		} else if result.TwitchVodChatDl != nil {
			twitchChat = result.TwitchVodChatDl
		}
		dlErr = w.orchestrator.ExecuteTwitch(ctx, jobCtx, variant, result.IsVod, twitchChat)
	} else {
		// YouTube path
		dlErr = w.orchestrator.ExecuteWithChat(ctx, jobCtx, result.VideoInfo, result.IsVod, result.ChatDownloader)
	}

	if dlErr != nil {
		if ctx.Err() != nil {
			w.handleCancellation(job, jobCtx.StagingDir)
			return
		}
		w.setJobError(job, dlErr)
		return
	}

	// Clean up staging directory after successful download + mux — UNLESS a
	// part's captured media is still unmuxed (both the stream-end mux and the
	// finalize backstop failed for it). Deleting it then would silently drop
	// footage from a job now marked Finished; preserve it so the Mux action
	// can recover it.
	if jobCtx.StagingDir != "" {
		fresh, _ := w.db.GetJob(job.ID)
		preserveForTail := fresh != nil && fresh.IncompleteTail
		if w.hasUnmuxedParts(job.ID, jobCtx.StagingDir) {
			w.logger.Warn("preserving staging dir: a captured part is still unmuxed after finalize; recover via the Mux action",
				"path", jobCtx.StagingDir, "jobID", job.ID)
		} else if preserveForTail {
			w.logger.Warn("preserving staging dir: recording tail incomplete; Retry will resume from the sidecar",
				"path", jobCtx.StagingDir, "jobID", job.ID)
		} else if err := os.RemoveAll(jobCtx.StagingDir); err != nil {
			w.logger.Warn("failed to remove staging directory", "path", jobCtx.StagingDir, "err", err)
		} else {
			w.logger.Debug("removed staging directory", "path", jobCtx.StagingDir)
		}
	}
}

// hasUnmuxedParts reports whether any quality/gap-split part still has
// recognized media in staging with no corresponding segment row — i.e.
// muxUnrecordedSegments failed to mux it at finalize. Mirrors that function's
// staging-dir→part-index mapping (root is index 0 unless a seg_0 dir exists).
// Returns false for single-file jobs (no seg_N dirs), whose root media was
// muxed via the normal path.
func (w *DownloadWorker) hasUnmuxedParts(jobID, stagingDir string) bool {
	return hasUnmuxedPartsForJob(w.db, jobID, stagingDir)
}

// hasUnmuxedPartsForJob is the free-function core of hasUnmuxedParts, split
// out so the orphan scanner (internal/worker/orphans.go) can reuse the exact
// same check — it only has a *database.Database, not a *DownloadWorker.
func hasUnmuxedPartsForJob(db *database.Database, jobID, stagingDir string) bool {
	segDirs := stagedSegDirs(stagingDir)
	if len(segDirs) == 0 {
		return false // no part splits — single-file cleanup is safe
	}
	segs, err := db.GetSegments(jobID)
	if err != nil {
		// Can't verify what's recorded — preserve rather than risk deleting
		// footage that was never persisted.
		return true
	}
	recorded := make(map[int]bool, len(segs))
	for _, s := range segs {
		recorded[s.SegmentIndex] = true
	}
	if segDirs[0].idx != 0 && !recorded[0] && discoverStagingMedia(stagingDir) != nil {
		return true // root is part 0 and it was never recorded
	}
	for _, sd := range segDirs {
		if !recorded[sd.idx] && discoverStagingMedia(sd.dir) != nil {
			return true
		}
	}
	return false
}

// handleCancellation handles a cancelled/shutdown job.
// User-initiated cancels update status to Cancelled.
// Shutdown cancels preserve original status so jobs resume on restart (matches TS).
func (w *DownloadWorker) handleCancellation(job *database.Job, stagingDir string) {
	// Consume the user-cancel flag BEFORE Complete — Complete clears any
	// leftover flag as part of slot cleanup. Reading the flag is lock-only
	// (no DB write), so the free-slot-before-DB-writes ordering below holds.
	userCancelled := w.queue.WasCancelled(job.ID)

	// Free the queue slot before any DB writes — symmetric with setJobError
	// (see I2 race comment there). Idempotent against the deferred Complete.
	w.queue.Complete(job.ID)

	if userCancelled {
		// User-initiated cancel: update status, notify
		w.logger.Info("job cancelled by user", "jobID", job.ID)

		w.db.UpdateJobFields(job.ID, map[string]any{
			"status": database.StatusCancelled,
		})

		if w.notifier != nil {
			w.notifier.Send("Download Cancelled",
				fmt.Sprintf("Cancelled: %s", job.Title),
				notifications.TypeCancelled,
				[]notifications.Field{
					{Name: "Channel", Value: job.ChannelName, Inline: true},
					{Name: notifications.IDLabel(job.Platform), Value: job.VideoID, Inline: true},
				},
				notifications.SendOptions{
					URL:       job.URL,
					Thumbnail: job.ThumbnailURL,
					Event:     "cancelled",
				},
			)
		}
	} else {
		// Shutdown: preserve existing status so job resumes on restart
		w.logger.Info("job interrupted by shutdown, preserving state", "jobID", job.ID)
	}
}

func isTerminalStatus(status database.JobStatus) bool {
	switch status {
	case database.StatusFinished, database.StatusError, database.StatusCancelled:
		return true
	default:
		return false
	}
}

func (w *DownloadWorker) buildJobContext(job *database.Job) *JobContext {
	// Snapshot all config fields under lock
	var (
		cfgOutputDir, cfgStagingDir, cfgTemplate string
		cfgMaxRes, cfgMaxTimeout                 int
		cfgPrefer60, cfgChat                     bool
	)
	w.readConfig(func(c *config.MoomboxConfig) {
		cfgOutputDir = c.Paths.OutputDirectory
		cfgStagingDir = c.Paths.EffectiveStagingDir()
		cfgTemplate = c.Downloader.OutputTemplate
		cfgMaxRes = c.Downloader.MaxVideoResolution
		cfgPrefer60 = c.Downloader.Prefer60fps
		cfgChat = c.Downloader.DownloadChat
		cfgMaxTimeout = c.Downloader.MaximumTimeout
	})

	outputDir := cfgOutputDir
	if job.OutputDirectory != "" {
		outputDir = job.OutputDirectory
	}
	if outputDir == "" {
		outputDir = "./output"
	}

	// Use config staging directory (defaults to ./staging)
	stagingBase := cfgStagingDir
	stagingDir := filepath.Join(stagingBase, job.ID)

	// Resolve filename from output_template config
	template := cfgTemplate
	if template == "" {
		template = "${title} [${id}]"
	}
	var dateStr *string
	if job.StreamStartTime != "" {
		dateStr = &job.StreamStartTime
	} else if job.CreatedAt != "" {
		dateStr = &job.CreatedAt
	}
	// Use job.ID for Twitch (VideoID is "tw_{login}", not the stream ID). Matches TS.
	templateID := job.VideoID
	if job.Platform == "twitch" {
		templateID = job.ID
	}
	filename := config.ResolveTemplate(template, config.TemplateVariables{
		Title:   job.Title,
		ID:      templateID,
		Channel: job.ChannelName,
		Date:    dateStr,
	})
	if filename == "" {
		filename = job.VideoID
	}

	return &JobContext{
		Job: job,
		DB:  w.db,
		Config: &JobConfig{
			MaxVideoResolution: cfgMaxRes,
			Prefer60fps:        cfgPrefer60,
			OutputDirectory:    outputDir,
			StagingDirectory:   stagingDir,
			FilenameTemplate:   template,
			DownloadChat:       cfgChat,
			MaximumTimeout:     cfgMaxTimeout,
		},
		YT:         w.yt,
		StagingDir: stagingDir,
		OutputDir:  outputDir,
		Filename:   filename,
		Logger:     w.logger,
	}
}

func (w *DownloadWorker) setJobError(job *database.Job, err error) {
	// Free the queue slot BEFORE committing the error to DB so a concurrent
	// monitor-driven AutoReinitializeJob can re-enqueue without hitting the
	// IsProcessing dedup. processJob's deferred Complete is idempotent and
	// remains as a safety net (covers panics that bypass this helper).
	// Closes the I2 race documented in the v2.6.10 final review.
	w.queue.Complete(job.ID)

	errMsg := err.Error()
	w.logger.Error("job error", "jobID", job.ID, "err", errMsg)

	// Both worker.ErrCookiesRequired (player-API member/login flags) and
	// twitch.ErrTwitchAuthExpired (GQL 401/403 after token rotation) mean
	// the user needs to refresh their cookie session — route both to
	// StatusCookies so the UI can prompt for re-auth instead of
	// presenting them as generic failures. Audit reports/twitch.md #8.
	status := database.StatusError
	if errors.Is(err, ErrCookiesRequired) || errors.Is(err, twitch.ErrTwitchAuthExpired) {
		status = database.StatusCookies
	}

	w.db.UpdateJobFields(job.ID, map[string]any{
		"status": status,
		"error":  errMsg,
	})

	// Suppress notifications for non-actionable errors (matches TS behavior):
	// - Age-restricted content: nothing user can do
	// - Probe timeout: transient, stream may have ended naturally
	// - Twitch monitor-driven retries STILL WITHIN budget: the monitor will
	//   silently AutoReinitializeJob on its next poll, so a failure embed
	//   would be noise on the same job.
	//
	// A TERMINAL failure on a retried job (budget exhausted, or an error
	// shape the recovery predicate rejects) DOES notify: previously any
	// AutoRetryCount>0 was suppressed, so the operator couldn't distinguish
	// "still retrying" from "gave up for good" — the exact moment that needs
	// attention. retryLikely mirrors monitor.isRecoverableTwitchError
	// (KEEP IN SYNC — the import would be cyclic): Error status, the exact
	// offline-flap message, no delivered segments, budget remaining.
	retryLikely := status == database.StatusError &&
		errMsg == TwitchOfflineErrMsg &&
		job.LastVideoSeq == nil &&
		job.AutoRetryCount < MaxTwitchAutoRetries
	suppressNotification := errors.Is(err, ErrNonActionable) || (job.AutoRetryCount > 0 && retryLikely)

	// Send error/auth notification
	if w.notifier != nil && !suppressNotification {
		if status == database.StatusCookies {
			reason := errMsg
			if reason == "" {
				reason = "Members-only content"
			}
			w.notifier.Send("Authentication Required",
				fmt.Sprintf("Cookies needed: %s", job.Title),
				notifications.TypeWarning,
				[]notifications.Field{
					{Name: "Channel", Value: job.ChannelName, Inline: true},
					{Name: notifications.IDLabel(job.Platform), Value: job.VideoID, Inline: true},
					{Name: "Reason", Value: reason, Inline: false},
				},
				notifications.SendOptions{
					URL:       job.URL,
					Thumbnail: job.ThumbnailURL,
					Event:     "auth",
				},
			)

			// Attempt automatic cookie refresh if configured
			if w.OnCookieRefreshNeeded != nil {
				w.logger.Info("attempting automatic cookie refresh...")
				if w.OnCookieRefreshNeeded() {
					w.logger.Info("cookie refresh succeeded, retrying job")
					// Set to Upcoming so StreamProcessor.Process re-probes and
					// correctly classifies the stream (live/VOD/upcoming). Using
					// Live was wrong when the stream had transitioned to post-live
					// or had not yet started (per audit reports/worker.md Finding 21).
					w.db.UpdateJobFields(job.ID, map[string]any{
						"status": database.StatusUpcoming,
						"error":  "",
					})
					w.queue.Enqueue(job.ID, database.StatusUpcoming)
					return
				}
				w.logger.Warn("auto cookie refresh failed — re-run setup from Settings")
			}
		} else {
			// URL fallback: use stored URL, or construct YouTube URL (matches TS)
			notifURL := job.URL
			if notifURL == "" && job.VideoID != "" {
				notifURL = "https://www.youtube.com/watch?v=" + job.VideoID
			}
			fields := notifications.NewFieldBuilder().
				AddInline("Channel", job.ChannelName).
				AddInline(notifications.IDLabel(job.Platform), job.VideoID).
				Add("Error", errMsg).
				// Terminal-after-retries: say the automation gave up so the
				// operator knows this needs a manual look.
				AddIf(job.AutoRetryCount > 0, "Automatic Retries",
					fmt.Sprintf("gave up after %d/%d", job.AutoRetryCount, MaxTwitchAutoRetries)).
				Build()
			w.notifier.Send("Job Failed",
				fmt.Sprintf("Job failed for: %s", job.Title),
				notifications.TypeError,
				fields,
				notifications.SendOptions{
					URL:       notifURL,
					Thumbnail: job.ThumbnailURL,
					Event:     "error",
				},
			)
		}
	}
}

// fetchURL is a helper to download a URL's body.
func fetchURL(ctx context.Context, url string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := workerHTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10MB cap
	return data, resp.StatusCode, err
}

// Stop signals the worker to stop processing new jobs and waits for in-flight
// jobs to finish (up to 10 seconds) so downloads aren't interrupted mid-write.
func (w *DownloadWorker) Stop() {
	w.logger.Info("download worker stopping")
	if w.streamProc != nil {
		w.streamProc.Stop()
	}

	// Wait for in-flight jobs with a timeout
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				w.logger.Error("panic waiting for in-flight jobs", "panic", fmt.Sprint(r))
			}
		}()
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		w.logger.Info("download worker: all in-flight jobs finished")
	case <-time.After(10 * time.Second):
		w.logger.Warn("download worker: timed out waiting for in-flight jobs")
	}
}

// SetConfigStore wires the shared *config.Store on the worker and the
// underlying StreamProcessor. Called once during startup after the Store
// has been constructed; safe to call before Run because no read sites
// fire until the queue has work to process.
func (w *DownloadWorker) SetConfigStore(store *config.Store) {
	w.configStore = store
	w.streamProc.SetConfigStore(store)
}

// SetParallelDownloads updates the max parallel downloads at runtime.
func (w *DownloadWorker) SetParallelDownloads(n int) {
	w.queue.SetMaxParallel(n)
}

// ResumeJob resumes a cancelled/errored YouTube job from its saved state.
// Preserves staging files, progress, and seq numbers. Resets auto_retry_count
// so any future error fires its notification — Resume is user-driven, so the
// "suppress retry-failure notifications" guard in setJobError must not apply.
func (w *DownloadWorker) ResumeJob(jobID string) {
	w.db.UpdateJobFields(jobID, map[string]any{
		"status":           database.StatusDownloading,
		"error":            "",
		"auto_retry_count": 0,
	})
	w.EnqueueJob(jobID)
}

// clearJobParts removes a job's persisted parts for a fresh restart: the
// on-disk part files (video + per-part chat, best-effort) and then the
// segment/gap rows. Called only by user-initiated Reinitialize.
func (w *DownloadWorker) clearJobParts(jobID string) {
	if segs, err := w.db.GetSegments(jobID); err == nil {
		for _, s := range segs {
			if s.FilePath != "" {
				if rmErr := os.Remove(s.FilePath); rmErr != nil && !os.IsNotExist(rmErr) {
					w.logger.Warn("reinit: failed to remove stale part file", "path", s.FilePath, "err", rmErr)
				}
			}
			if s.ChatFile != "" {
				if rmErr := os.Remove(s.ChatFile); rmErr != nil && !os.IsNotExist(rmErr) {
					w.logger.Warn("reinit: failed to remove stale part chat", "path", s.ChatFile, "err", rmErr)
				}
			}
		}
	}
	if err := w.db.ClearJobSegmentsAndGaps(jobID); err != nil {
		w.logger.Warn("reinit: failed to clear segment/gap rows", "jobID", jobID, "err", err)
	}
}

// ReinitializeJob resets a job to a fresh state and re-enqueues it.
// Clears all progress fields and deletes the staging directory.
func (w *DownloadWorker) ReinitializeJob(jobID string) {
	// Read config for staging path
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) {
		stagingBase = c.Paths.EffectiveStagingDir()
	})

	// Delete staging directory
	stagingDir := filepath.Join(stagingBase, jobID)
	if err := os.RemoveAll(stagingDir); err != nil {
		w.logger.Warn("failed to remove staging directory on reinitialize", "path", stagingDir, "err", err)
	}

	// Fresh start: discard any parts from a prior quality/gap-split attempt.
	// Without this the stale segment rows survive the reset and muxAndFinalize
	// would finalize the clean re-download as multi-part from the OLD part
	// files, silently discarding the freshly-downloaded media. (AutoReinit
	// deliberately does NOT do this — see that method.)
	w.clearJobParts(jobID)

	// Clear all non-input fields. auto_retry_count resets here because
	// user-driven reinit grants the job a fresh budget; auto-recovery
	// uses AutoReinitializeJob (sibling method) which increments instead.
	// KEEP IN SYNC with AutoReinitializeJob below — same reset map, the
	// only difference is the auto_retry_count value (0 vs newCount).
	w.db.UpdateJobFields(jobID, map[string]any{
		"status":              database.StatusUpcoming,
		"error":               "",
		"progress":            "",
		"percent":             0,
		"speed":               "",
		"eta":                 "",
		"last_video_seq":      nil,
		"last_audio_seq":      nil,
		"total_video_seq":     nil,
		"total_audio_seq":     nil,
		"chat_status":         "",
		"total_chat_messages": nil,
		"download_started_at": "",
		"stream_end_time":     "",
		"output_file":         "",
		"filename":            "",
		"file_size":           nil,
		"chat_file":           "",
		"chat_filename":       "",
		"description_file":    "",
		"thumbnail_file":      "",
		"video_width":         nil,
		"video_height":        nil,
		"video_fps":           nil,
		"length_seconds":      nil,
		"selected_video_itag": nil,
		"selected_audio_itag": nil,
		"auto_retry_count":    0,
		"incomplete_tail":     false,
	})
	w.EnqueueJob(jobID)
}

// AutoReinitializeJob is the auto-recovery sibling of ReinitializeJob: same
// state reset (clears progress fields, deletes staging dir, sets status to
// Upcoming, re-enqueues), but increments auto_retry_count instead of
// resetting it. Called by the Twitch monitor's OnStreamRecover callback
// when an errored job's underlying broadcast is still live and the error
// matches a recoverable shape.
//
// Capped at MaxTwitchAutoRetries (the caller is expected to pre-check the
// budget; this method blindly increments).
//
// Unlike ReinitializeJob, this deliberately does NOT clear the job's segment
// rows: auto-recovery only fires for the SAME still-live broadcast (the caller
// guards on sameBroadcastStart), so already-captured parts 0..N are real
// footage from this broadcast and the recovered capture continues at part N+1
// (discoverResumeSegment returns maxRecorded+1). Clearing here would throw away
// captured footage of a live broadcast — the opposite of recovery.
func (w *DownloadWorker) AutoReinitializeJob(jobID string) {
	prev, err := w.db.GetJob(jobID)
	if err != nil || prev == nil {
		w.logger.Warn("AutoReinitializeJob: job not found", "jobID", jobID, "err", err)
		return
	}
	newCount := prev.AutoRetryCount + 1

	// Read config for staging path
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) {
		stagingBase = c.Paths.EffectiveStagingDir()
	})

	// Delete staging directory
	stagingDir := filepath.Join(stagingBase, jobID)
	if err := os.RemoveAll(stagingDir); err != nil {
		w.logger.Warn("AutoReinitializeJob: staging cleanup failed", "path", stagingDir, "err", err)
	}

	// Same field reset as ReinitializeJob, but auto_retry_count INCREMENTS.
	// KEEP IN SYNC with ReinitializeJob above if reset fields are added.
	w.db.UpdateJobFields(jobID, map[string]any{
		"status":              database.StatusUpcoming,
		"error":               "",
		"progress":            "",
		"percent":             0,
		"speed":               "",
		"eta":                 "",
		"last_video_seq":      nil,
		"last_audio_seq":      nil,
		"total_video_seq":     nil,
		"total_audio_seq":     nil,
		"chat_status":         "",
		"total_chat_messages": nil,
		"download_started_at": "",
		"stream_end_time":     "",
		"output_file":         "",
		"filename":            "",
		"file_size":           nil,
		"chat_file":           "",
		"chat_filename":       "",
		"description_file":    "",
		"thumbnail_file":      "",
		"video_width":         nil,
		"video_height":        nil,
		"video_fps":           nil,
		"length_seconds":      nil,
		"selected_video_itag": nil,
		"selected_audio_itag": nil,
		"auto_retry_count":    newCount,
		"incomplete_tail":     false,
	})
	w.EnqueueJob(jobID)
}

// MuxJob force-muxes a cancelled/errored job's staging files.
// Bypasses the download queue — runs directly in a wg-tracked goroutine.
func (w *DownloadWorker) MuxJob(jobID string) error {
	// Read config for staging check
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) {
		stagingBase = c.Paths.EffectiveStagingDir()
	})

	if !HasSegmentFiles(stagingBase, jobID) {
		return fmt.Errorf("no segment files found in staging")
	}

	w.db.UpdateJobFields(jobID, map[string]any{
		"status": database.StatusMuxing,
	})

	w.wg.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				w.logger.Error("panic in MuxJob", "jobID", jobID, "panic", fmt.Sprint(r))
				w.db.UpdateJobFields(jobID, map[string]any{
					"status": database.StatusError,
					"error":  fmt.Sprintf("internal panic: %v", r),
				})
			}
		}()

		job, err := w.db.GetJob(jobID)
		if err != nil {
			w.logger.Error("MuxJob: get job failed", "jobID", jobID, "err", err)
			w.db.UpdateJobFields(jobID, map[string]any{
				"status": database.StatusError,
				"error":  fmt.Sprintf("mux setup failed: %v", err),
			})
			return
		}
		if job == nil {
			// Row deleted while the mux was queued — nothing to do (and no
			// row left to flag as errored).
			w.logger.Debug("MuxJob: job vanished before muxing", "jobID", jobID)
			return
		}

		jobCtx := w.buildJobContext(job)
		ctx := context.Background()

		if err := w.orchestrator.muxFromStaging(ctx, jobCtx); err != nil {
			w.logger.Error("MuxJob failed", "jobID", jobID, "err", err)
			w.db.UpdateJobFields(jobID, map[string]any{
				"status": database.StatusError,
				"error":  err.Error(),
			})
		}
	})

	return nil
}
