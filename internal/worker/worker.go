package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// heartbeatInterval is the safety-net poll interval for catching missed jobs.
// Normal job discovery is signal-driven via NotifyNewJob.
const heartbeatInterval = 60 * time.Second

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
	Job        *database.Job
	Config     *JobConfig
	YT         *youtube.Service
	DB         *database.Database
	StagingDir string
	OutputDir  string
	Filename   string
	Logger     logger
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
	cfg          *config.MoomboxConfig
	cfgMu        *sync.RWMutex // shared config mutex (set via SetCfgMu)
	queue        *JobQueue
	orchestrator *DownloadOrchestrator
	streamProc   *StreamProcessor
	notifier     *notifications.Manager
	logger       logger
	wg           sync.WaitGroup // tracks in-flight processJob goroutines
	notifyJob    chan struct{}   // signal to re-check for new jobs (non-blocking send)

	// OnCookieRefreshNeeded is called when auth fails and auto-refresh should be attempted.
	// Returns true if cookies were refreshed successfully.
	OnCookieRefreshNeeded func() bool
}

// DownloadWorkerDeps holds optional dependencies for the download worker.
type DownloadWorkerDeps struct {
	CipherSolver  *cipher.Solver
	PotProvider   *bgutils.PotProvider
	TwitchService *twitch.Service
	Notifier      *notifications.Manager
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
	if deps != nil {
		cs = deps.CipherSolver
		pp = deps.PotProvider
		tw = deps.TwitchService
		nm = deps.Notifier
	}

	sp := NewStreamProcessor(yt, tw, cfg, db, logger)
	if nm != nil {
		sp.SetNotifier(nm)
	}

	return &DownloadWorker{
		db:           db,
		yt:           yt,
		tw:           tw,
		cfg:          cfg,
		queue:        queue,
		orchestrator: NewDownloadOrchestrator(db, queue, cfg.Paths.FfmpegPath, logger, cs, pp, nm),
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

		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
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
		}()
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

		// Check if context is done before restarting
		select {
		case <-ctx.Done():
			return
		default:
			time.Sleep(time.Second) // Brief pause before restart
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
			w.setJobError(job, errors.New(result.Error))
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
	if w.cfgMu != nil {
		w.cfgMu.RLock()
	}
	maxRes := w.cfg.Downloader.MaxVideoResolution
	if w.cfgMu != nil {
		w.cfgMu.RUnlock()
	}
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
		var twitchChat TwitchChatDownloader
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
				notifications.TypeWarning,
				nil,
				notifications.SendOptions{
					URL:   job.URL,
					Event: "cancelled",
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
	if w.cfgMu != nil {
		w.cfgMu.RLock()
	}
	cfgOutputDir := w.cfg.Paths.OutputDirectory
	cfgStagingDir := w.cfg.Paths.StagingDirectory
	cfgTemplate := w.cfg.Downloader.OutputTemplate
	cfgMaxRes := w.cfg.Downloader.MaxVideoResolution
	cfgPrefer60 := w.cfg.Downloader.Prefer60fps
	cfgChat := w.cfg.Downloader.DownloadChat
	cfgRetryCap := w.cfg.Downloader.SegmentRetryDelayCap
	cfgLiveRetries := w.cfg.Downloader.SegmentLiveCheckRetries
	if w.cfgMu != nil {
		w.cfgMu.RUnlock()
	}

	outputDir := cfgOutputDir
	if job.OutputDirectory != "" {
		outputDir = job.OutputDirectory
	}
	if outputDir == "" {
		outputDir = "./output"
	}

	// Use config staging directory, falling back to ./staging/{jobID}
	stagingBase := cfgStagingDir
	if stagingBase == "" {
		stagingBase = "./staging"
	}
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

	status := database.StatusError
	errLower := strings.ToLower(errMsg)
	if strings.HasPrefix(errLower, "login required") ||
		strings.HasPrefix(errLower, "member-only") ||
		strings.HasPrefix(errLower, "members only") ||
		strings.Contains(errLower, "cookies?") {
		status = database.StatusCookies
	}

	w.db.UpdateJobFields(job.ID, map[string]any{
		"status": status,
		"error":  errMsg,
	})

	// Suppress notifications for non-actionable errors (matches TS behavior):
	// - Age-restricted content: nothing user can do
	// - Probe timeout: transient, stream may have ended naturally
	suppressNotification := strings.Contains(errLower, "age restricted") ||
		strings.HasPrefix(errLower, "max probe errors")

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
					w.db.UpdateJobFields(job.ID, map[string]any{
						"status": database.StatusLive,
						"error":  "",
					})
					w.queue.Enqueue(job.ID, database.StatusLive)
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

var filenameReplacer = strings.NewReplacer(
	"/", "_", "\\", "_", ":", "_", "*", "_", "?", "_",
	"\"", "_", "<", "_", ">", "_", "|", "_",
)

// sanitizeFilename removes invalid characters from a filename.
func sanitizeFilename(name string) string {
	result := filenameReplacer.Replace(name)
	// Truncate by rune count to avoid splitting multi-byte UTF-8 characters
	runes := []rune(result)
	if len(runes) > 200 {
		result = string(runes[:200])
	}
	return result
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

// SetCfgMu sets the shared config mutex for synchronized config access.
func (w *DownloadWorker) SetCfgMu(mu *sync.RWMutex) {
	w.cfgMu = mu
	w.streamProc.SetCfgMu(mu)
}

// SetParallelDownloads updates the max parallel downloads at runtime.
func (w *DownloadWorker) SetParallelDownloads(n int) {
	w.queue.SetMaxParallel(n)
}
