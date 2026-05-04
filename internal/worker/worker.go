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
// Normal job discovery is signal-driven via NotifyNewJob.
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
	MaxVideoResolution      int
	Prefer60fps             bool
	VideoItag               int
	AudioItag               int
	OutputDirectory         string
	StagingDirectory        string
	FilenameTemplate        string
	DownloadChat            bool
	SegmentRetryDelayCap    int
	SegmentLiveCheckRetries int
}

// DownloadWorker manages the job processing loop.
type DownloadWorker struct {
	db           *database.Database
	yt           *youtube.Service
	tw           *twitch.Service
	cfg          *config.MoomboxConfig // captured for early-init reads before SetConfigStore
	configStore  *config.Store         // shared config store (set via SetConfigStore)
	queue        *JobQueue
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
	CipherSolver  *cipher.Solver
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

	var cs *cipher.Solver
	var pp *bgutils.PotProvider
	var tw *twitch.Service
	var nm *notifications.Manager
	var conn Connectivity
	if deps != nil {
		cs = deps.CipherSolver
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

	return &DownloadWorker{
		db:           db,
		yt:           yt,
		tw:           tw,
		cfg:          cfg,
		queue:        queue,
		orchestrator: NewDownloadOrchestrator(db, queue, cfg.Paths.FfmpegPath, logger, cs, pp, nm, conn),
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

// StashTwitchStreamInfo forwards a fresh Twitch stream info hint to the
// underlying StreamProcessor. Called by cmd/moombox's OnStreamFound /
// OnStreamRecover monitor callbacks so the processor doesn't re-fetch what
// the monitor just successfully fetched.
func (w *DownloadWorker) StashTwitchStreamInfo(jobID string, info *twitch.TwitchStreamInfo) {
	if w.streamProc != nil {
		w.streamProc.StashTwitchStreamInfo(jobID, info)
	}
}

// CancelJob cancels a running job and updates its status.
func (w *DownloadWorker) CancelJob(jobID string) {
	w.queue.Cancel(jobID)
	w.db.UpdateJobFields(jobID, map[string]any{
		"status": database.StatusCancelled,
	})
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

func (w *DownloadWorker) processJob(ctx context.Context, jobID string) {
	defer w.queue.Complete(jobID)

	job, err := w.db.GetJob(jobID)
	if err != nil {
		w.logger.Error("get job failed", "jobID", jobID, "err", err)
		return
	}

	// Check if job is already in a terminal state (stale check)
	if isTerminalStatus(job.Status) {
		w.logger.Debug("skipping terminal job", "jobID", jobID, "status", job.Status)
		return
	}

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

	// Acquire download slot — blocks until a slot is available.
	// Lifecycle slot (from Dequeue) allows stream processing to proceed without
	// consuming download slots; actual downloading requires a separate download slot.
	if !w.queue.AcquireDownloadSlot(ctx, jobID) {
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

	// Clean up staging directory after successful download + mux
	if jobCtx.StagingDir != "" {
		if err := os.RemoveAll(jobCtx.StagingDir); err != nil {
			w.logger.Warn("failed to remove staging directory", "path", jobCtx.StagingDir, "err", err)
		} else {
			w.logger.Debug("removed staging directory", "path", jobCtx.StagingDir)
		}
	}
}

// handleCancellation handles a cancelled/shutdown job.
// User-initiated cancels update status to Cancelled.
// Shutdown cancels preserve original status so jobs resume on restart (matches TS).
func (w *DownloadWorker) handleCancellation(job *database.Job, stagingDir string) {
	if w.queue.WasCancelled(job.ID) {
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
					{Name: "Video ID", Value: job.VideoID, Inline: true},
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
		cfgMaxRes, cfgRetryCap, cfgLiveRetries   int
		cfgPrefer60, cfgChat                     bool
	)
	w.readConfig(func(c *config.MoomboxConfig) {
		cfgOutputDir = c.Paths.OutputDirectory
		cfgStagingDir = c.Paths.EffectiveStagingDir()
		cfgTemplate = c.Downloader.OutputTemplate
		cfgMaxRes = c.Downloader.MaxVideoResolution
		cfgPrefer60 = c.Downloader.Prefer60fps
		cfgChat = c.Downloader.DownloadChat
		cfgRetryCap = c.Downloader.SegmentRetryDelayCap
		cfgLiveRetries = c.Downloader.SegmentLiveCheckRetries
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
			MaxVideoResolution:      cfgMaxRes,
			Prefer60fps:             cfgPrefer60,
			OutputDirectory:         outputDir,
			StagingDirectory:        stagingDir,
			FilenameTemplate:        template,
			DownloadChat:            cfgChat,
			SegmentRetryDelayCap:    cfgRetryCap,
			SegmentLiveCheckRetries: cfgLiveRetries,
		},
		YT:         w.yt,
		StagingDir: stagingDir,
		OutputDir:  outputDir,
		Filename:   filename,
		Logger:     w.logger,
	}
}

func (w *DownloadWorker) setJobError(job *database.Job, err error) {
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
	suppressNotification := errors.Is(err, ErrNonActionable)

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
					{Name: "Video ID", Value: job.VideoID, Inline: true},
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
			w.notifier.Send("Job Failed",
				fmt.Sprintf("Job failed for: %s", job.Title),
				notifications.TypeError,
				[]notifications.Field{
					{Name: "Channel", Value: job.ChannelName, Inline: true},
					{Name: "Video ID", Value: job.VideoID, Inline: true},
					{Name: "Error", Value: errMsg},
				},
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
// Preserves staging files, progress, and seq numbers.
func (w *DownloadWorker) ResumeJob(jobID string) {
	w.db.UpdateJobFields(jobID, map[string]any{
		"status": database.StatusDownloading,
		"error":  "",
	})
	w.EnqueueJob(jobID)
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
