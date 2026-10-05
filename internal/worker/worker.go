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

// ErrNotAMember marks a members-only failure that YouTube returned to a
// session it confirmed was SIGNED IN. That is a membership problem, not a
// credential problem: the cookies are demonstrably alive, they just belong
// to an account that does not hold the channel's membership (commonly the
// wrong one of several Google accounts in the exporting browser).
//
// It still routes to StatusCookies — supplying credentials for an account
// that DOES hold the membership is the fix, and the user must be able to see
// and act on that — but it suppresses both automatic paths that assume the
// SAME credentials can be made to work:
//
//   - the automatic cookie refresh (cookieRefreshWorthAttempting), because
//     rotating a live session cannot add a membership to it, and doing so
//     printed "re-run setup" advice at an operator whose cookies were fine;
//   - the auth-recovered sweep, via the persisted database.ParkReasonMembership
//     that parkReasonForError records. That sweep fires on a
//     not-authenticated → authenticated transition, which by definition
//     cannot be the event that fixes a membership problem (the session was
//     already authenticated when it failed). Resuming on it bought a
//     guaranteed-identical failure and a full extraction attempt per auth
//     cycle, forever. What resumes these instead is a different ACCOUNT: the
//     job records the identity it was refused under (database.ParkIdentity)
//     and cmd/moombox's credential sweep compares against it.
//
// Note the cost of a misclassification. The signed-in determination comes
// from the watch page's session state, and checkPlayability already notes
// that signal can in principle disagree with the player response. A failure
// wrongly classified here is excluded from the auth-recovered sweep for the
// rest of that job's life — it will only ever resume on an account change or
// a manual retry. That asymmetry is deliberate (the alternative is the
// forever-retry loop), but it is why the classification is driven off an
// explicit SessionAuthLoggedIn verdict and never off a guess.
var ErrNotAMember = errors.New("not a channel member")

// ErrNonActionable marks errors where there's nothing the user can do
// (age-restricted content, exhausted retry budgets). Notification
// dispatch is suppressed for these to avoid noisy "your stream failed"
// pings about content that was never going to succeed.
var ErrNonActionable = errors.New("non-actionable error")

// ErrCancelled marks a run that was cancelled rather than failed: the
// StreamProcessor's waits return it (cancelledResult — ctx done, or the row
// cancelled or deleted while upcoming), and ExecuteWithChat returns it when
// the row was already Cancelled as it started. processJob routes it to
// handleCancellation instead of comparing error strings.
var ErrCancelled = errors.New("cancelled")

// The three ways the off-queue staging verbs refuse. All three are the
// caller's problem to report, never a job failure: the job's own status is
// untouched by a refusal, and the REST layer maps each of them to 409.
var (
	// ErrRecoveryJobActive: the job is Downloading, Muxing, Live or Upcoming,
	// so its staging dir is being written and its output directory is about to
	// be.
	ErrRecoveryJobActive = errors.New("job is active; set-aside recovery would race the download")
	// ErrStagingBusy: another off-queue operation already holds this job's
	// staging directory. Shared by MuxJob and RecoverAsides, because the
	// collision is symmetric.
	ErrStagingBusy = errors.New("another operation is already working this job's staging directory")
	// ErrNoAsides: nothing in staging was ever set aside. Recovery is not a
	// second Mux button, so this is a refusal rather than a no-op success.
	ErrNoAsides = errors.New("no set-aside recordings in staging")
)

// The operations that can hold a job's staging claim. Named so a refusal can
// say what is in the way.
const (
	opMux           = "mux"
	opRecoverAsides = "set-aside recovery"
)

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
	// Interruption tracks the broadcast-interrupted signature (interruption
	// spec Tier 1) observed across every player-response fetch for this
	// job. Shared by POINTER — strategyCtx/segCtx/segJobCtx value-copies
	// of JobContext (segCtx := *jobCtx and friends throughout orchestrator*.go)
	// must all observe the SAME signal, not independent copies. Set once in
	// buildJobContext; nil in JobContext literals built directly by tests
	// (interruptionSignal's methods and buildMayResume are all nil-safe, so
	// this is never a required field).
	Interruption *interruptionSignal
	// ChatStatus is the terminal chat_status this job's chat downloader EARNED
	// — written by recordChatOutcome once the downloader has exited, and empty
	// for a job that ran none (including the standalone Mux action, which
	// builds a JobContext without ever starting chat). The mux path reads it
	// through chatFileStatus so copying a chat FILE cannot re-report a capture
	// that stopped short as finished. Set long after every value-copy of
	// JobContext in the download paths (segCtx := *jobCtx and friends) has been
	// made, and only ever read through the pointer the orchestrator hands to
	// muxAndFinalize.
	ChatStatus string
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
	// SegmentWorkers is the configured concurrent-segment-fetch count
	// (config.Downloader.SegmentWorkers), threaded into engine.DownloaderOptions
	// by every strategy that builds a segment downloader. Zero means the
	// strategies pass it straight through to the engine unchanged, which
	// falls back to engine.ParallelDownloads — see DownloaderOptions.SegmentWorkers.
	SegmentWorkers int
	// InterruptionTimeout (config.Downloader.InterruptionTimeout, minutes,
	// converted once here via FlexDuration.AsDuration) is the RAW config
	// value: 0 means "stall disabled" per the config contract, > 0 is the
	// stall ceiling in minutes. Every LIVE strategy that also wires
	// SegmentWorkers + CheckStreamStatus threads it into
	// engine.DownloaderOptions.InterruptionTimeout through
	// engineInterruptionTimeout (internal/worker/interruption.go), NOT
	// straight through — that helper maps 0 onto engine.InterruptionNoStall
	// so a disabled-stall job still latches Tier-2 evidence without ever
	// colliding with the engine's own "0 = unbounded" meaning for a literal
	// zero (I1 fix). attachMayResume installs MayResume unconditionally on
	// every downloader it's called for, regardless of this value — the
	// VOD-only strategy simply never calls it, so an unset zero value there
	// is inert either way.
	InterruptionTimeout time.Duration
	// ProgressInterval (config.Downloader.ProgressIntervalMS, milliseconds,
	// converted once in buildJobContext) is the minimum gap between this
	// job's progress reports. Handed to NewProgressTracker at both
	// construction sites; a non-positive value there falls back to
	// progressUpdateInterval, so a JobContext literal built directly by a
	// test needs no value. Snapshotted at job start like MaximumTimeout
	// above, so a config save applies to the next job, not a running one.
	ProgressInterval time.Duration
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
	// twitchChats is the same registry the orchestrator holds. Kept here so
	// cmd/moombox has an exported path to it without reaching through the
	// orchestrator, which is otherwise entirely internal to this package.
	twitchChats *twitchChatRegistry
	streamProc  *StreamProcessor
	notifier    notifications.Sender
	logger      logger
	wg          sync.WaitGroup // tracks in-flight processJob goroutines
	notifyJob   chan struct{}  // signal to re-check for new jobs (non-blocking send)

	// stagingClaimMu guards stagingClaims, the per-job claim BOTH off-queue
	// verbs take before they start: MuxJob and RecoverAsides. Each writes into
	// one staging directory and one output directory, and each calls
	// muxStagedAsides with its OWN asideOutputPath `used` map — so two of them
	// running together both pick the plain <stem>.restart-<ts>.mp4 and write
	// over each other. The value is the operation holding it, so the refusal
	// can say which. Lazily allocated, so a zero-value worker (the tests build
	// several) needs no constructor change.
	stagingClaimMu sync.Mutex
	stagingClaims  map[string]string

	// afterExitPending holds the jobs with an afterJobExit reset waiting on
	// their previous run, so a second click does not queue a second reset.
	afterExitPending sync.Map

	// OnCookieRefreshNeeded is called when auth fails and auto-refresh should
	// be attempted. Returns true if THE NAMED PLATFORM ended up authenticated.
	//
	// The platform argument is not decoration. Without it the callback could
	// only answer "did any platform end up authenticated", so a healthy Twitch
	// told a YouTube job to retry — spending a probe attempt and a slot on a
	// request that had just conclusively failed, on every cycle.
	OnCookieRefreshNeeded func(platform string) bool

	// CurrentCredentialIdentity returns an opaque fingerprint of the account
	// the platform's cookies currently belong to (cookies.CookieJar's
	// YouTubeIdentity), or "" when it cannot be determined. Recorded on a
	// membership park so the credential sweep can tell later whether the
	// account has actually changed. Optional: a nil slot simply records "",
	// which the sweep resolves permissively.
	CurrentCredentialIdentity func(platform string) string
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
	Notifier      notifications.Sender
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
	var nm notifications.Sender
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

	// ONE registry, two holders. ExecuteTwitch registers into the
	// orchestrator's; cmd/moombox broadcasts through the worker's. Two
	// registries would compile, pass every registry unit test, and leave the
	// broadcast reaching an always-empty map.
	twitchChats := newTwitchChatRegistry()
	orchestrator := NewDownloadOrchestrator(db, queue, cfg.Paths.FfmpegPath, logger, cs, routedCs, pp, nm, conn)
	orchestrator.twitchChats = twitchChats

	return &DownloadWorker{
		db:           db,
		yt:           yt,
		tw:           tw,
		cfg:          cfg,
		queue:        queue,
		scheduler:    sched,
		orchestrator: orchestrator,
		twitchChats:  twitchChats,
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

// CookieResumeStatus is where a job parked in COOKIES? goes once its
// credentials are repaired — by the cookie-parked sweep and by the worker's
// own in-process refresh alike. The answer depends on queue_priority, and
// getting it wrong breaks the pacing in one direction or strands a job in
// the other:
//
//   - priority 1 (backlog) resumes to Queued, for the scheduler to re-admit
//     archive_slots at a time; the caller wakes it. Upcoming instead handed
//     a channel's whole parked backlog to the worker at once, bypassed
//     archive-slots entirely, and left CountBacklogInFlight over-counting
//     until they drained.
//   - priority 0 (live, upcoming, manually added) resumes to Upcoming. The
//     scheduler never admits a priority-0 row, so Queued would strand it.
//   - priority 1 with NO feed_items partner also resumes to Upcoming, because
//     Queued would strand it just as surely. CancelAndPrune (channel REMOVAL)
//     deletes the channel's never-started jobs and then its feed_items rows,
//     but deliberately leaves a RUNNING download alone; a backlog VOD that
//     was Downloading at that moment survives with no partner, and it is
//     exactly the row that parks in COOKIES? later. NextQueuedJobs
//     INNER-JOINs feed_items, so the scheduler would never return it, /retry
//     and /resume both refuse Queued, and ShouldProcess(Queued) is false —
//     the row would be lost permanently and silently.
//
// The partner check is GetFeedItem (nil, nil for no row). A read that errors
// returns that error with Upcoming: the cheap answer is the one that can
// still finish the download.
func CookieResumeStatus(db *database.Database, job *database.Job) (database.JobStatus, error) {
	if job.QueuePriority != 1 || job.ChannelID == nil {
		return database.StatusUpcoming, nil
	}
	it, err := db.GetFeedItem(*job.ChannelID, job.VideoID)
	if err != nil {
		return database.StatusUpcoming, err
	}
	if it != nil {
		return database.StatusQueued, nil
	}
	return database.StatusUpcoming, nil
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
// OnStreamRecover monitor callbacks so the next job to ask about that CHANNEL
// doesn't re-fetch what the monitor just successfully fetched.
func (w *DownloadWorker) StashTwitchStreamInfo(info *twitch.TwitchStreamInfo) {
	if w.streamProc != nil {
		w.streamProc.StashTwitchStreamInfo(info)
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

// CancelJob cancels a job and writes its status Cancelled. Returns true when an actively-processing run was
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

// muxOnRestart reports whether an interrupted Muxing row should be re-muxed
// from what is already staged instead of re-processed from the start (owner
// decision O-B). Three terms, each load-bearing: the row must actually be in
// Muxing; its staging must still hold recognised media or seg_N parts (else
// muxFromStaging has nothing to work with); and it must NOT be flagged
// incomplete_tail — that row's recording is known to be short, so it still
// needs the post-live VOD-refresh loop that only the download path runs.
func muxOnRestart(job *database.Job, stagingBase string) bool {
	if job == nil || job.Status != database.StatusMuxing || job.IncompleteTail {
		return false
	}
	return HasSegmentFiles(stagingBase, job.ID)
}

func (w *DownloadWorker) enqueueExistingJobs() {
	jobs, err := w.db.GetAllJobs()
	if err != nil {
		w.logger.Error("failed to get existing jobs", "err", err)
		return
	}

	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })

	for _, job := range jobs {
		if job.Status == database.StatusMuxing {
			if muxOnRestart(job, stagingBase) {
				// Owner decision O-B: mux what is staged. The previous reset
				// to Downloading re-probed the (now post-live) stream, routed
				// it to the manifest-free strategy whose dbResumeSeq seeds 0,
				// and truncated the complete recording (sweep-2 ENGINE-1).
				// The Mux action's path needs no network and no re-download.
				w.logger.Info("resuming interrupted mux from staged media", "jobID", job.ID)
				w.db.UpdateJobFields(job.ID, map[string]any{"error": ""})
				if err := w.MuxJob(job.ID); err != nil {
					// Only reachable if staging vanished between the check and
					// the call; fall back to the historical reset.
					w.logger.Warn("re-mux from staging refused; falling back to re-processing",
						"jobID", job.ID, "err", err)
				} else {
					continue
				}
			}
			// No staged media, or incomplete_tail: there is nothing to mux, or
			// the post-live VOD-refresh loop still has a tail to fetch. Reset
			// to Downloading and clear any stale error string so the UI does
			// not show a prior error alongside the fresh state (per audit
			// reports/worker.md Finding 24).
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
					// Logged like every sibling read: a failing DB otherwise
					// leaves this safety net dead with nothing in the log.
					w.logger.Warn("heartbeat: GetAllJobs failed", "err", err)
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

// twitchEndConfirmDelay is the gap between the two GetStreamInfo samples that
// must agree before a Twitch broadcast is declared over (owner decision O-C).
// A package var so tests can shrink it; production never reassigns it.
var twitchEndConfirmDelay = 5 * time.Second

// confirmTwitchLiveness answers "is this broadcast still live?" from TWO
// samples ~twitchEndConfirmDelay apart, and only when they agree that it is
// not. One sample was enough to finalize a live recording as Finished
// mid-broadcast (sweep-2 ENGINE-3): GetStreamInfo's `Stream == nil` covers a
// real offline AND the documented StreamMetadata flap that twitch_hint.go
// exists to dodge, and every consult site in the HLS loop reads this answer.
//
// The cost is paid only on the answer that ends a recording: a live first
// sample returns immediately, so a healthy stream pays nothing. An error from
// either sample is returned as-is, which the engine classifies as
// verdictUnknown and defers on — a failed check is not a verdict. A
// cancellation in the gap is likewise an error, never an "ended".
//
// sample is a function rather than the service so the rule is testable
// without a Twitch client.
func confirmTwitchLiveness(ctx context.Context, sample func() (*twitch.TwitchStreamInfo, error)) (*twitch.TwitchStreamInfo, error) {
	first, err := sample()
	if err != nil {
		return nil, err
	}
	if first != nil && first.IsLive {
		return first, nil
	}
	if err := utils.Sleep(ctx, twitchEndConfirmDelay); err != nil {
		return nil, err
	}
	return sample()
}

// confirmTwitchStreamInfo binds confirmTwitchLiveness to this worker's Twitch
// service for one channel login.
func (w *DownloadWorker) confirmTwitchStreamInfo(ctx context.Context, login string) (*twitch.TwitchStreamInfo, error) {
	return confirmTwitchLiveness(ctx, func() (*twitch.TwitchStreamInfo, error) {
		return w.tw.GetStreamInfo(ctx, login)
	})
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
			w.handleCancellation(job)
			return
		}
		w.setJobError(job, err)
		return
	}

	if !result.ShouldDownload {
		if errors.Is(result.ErrSentinel, ErrCancelled) {
			// A wait cancelled by ctx.Done() or the row's status (see
			// cancelledResult). Route through handleCancellation so shutdown
			// preserves state.
			w.handleCancellation(job)
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
		w.handleCancellation(job)
		return
	}

	// Download slot first — for VODs, blocks until one is available;
	// broadcasts pass through ungated (see acquireDownloadSlot). BEFORE the
	// lifecycle slot, not after it: a VOD that took its lifecycle slot and
	// then parked on the download pool held one of the 100 for nothing, so
	// enough admitted backlog (Σ archive_slots across channels) filled the
	// lifecycle pool with VODs that were merely queueing, and a live
	// broadcast — which never waits on the download pool — blocked behind
	// them and lost footage. A VOD now waits holding nothing that a broadcast
	// needs.
	if !w.acquireDownloadSlot(ctx, jobID, result.IsVod) {
		// Context cancelled while waiting for download slot
		w.handleCancellation(job)
		return
	}

	// Lifecycle slot (owner decision O-F): claimed HERE, once stream
	// processing has decided this job downloads, and never earlier. Dequeue
	// used to claim it, which meant every Upcoming job and every
	// manually-added offline Twitch channel held one of the 100 for its whole
	// wait — and a job stream processing then refused held one it never used.
	// It bounds the whole download half; the download slot above bounds the
	// VOD pool. Every exit path from here on runs through the deferred
	// queue.Complete above, which releases both.
	if !w.queue.AcquireLifecycleSlot(ctx, jobID) {
		// Only ctx cancellation ends that wait.
		w.handleCancellation(job)
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
				info, err := w.confirmTwitchStreamInfo(innerCtx, login)
				if err != nil {
					return false, err
				}
				return info != nil && info.IsLive, nil
			}
			variant.RecheckStreamFn = func(innerCtx context.Context) (*twitch.TwitchStreamInfo, error) {
				return w.confirmTwitchStreamInfo(innerCtx, login)
			}
			variant.FetchVariantsFn = func(innerCtx context.Context) ([]twitch.TwitchHLSVariant, error) {
				// The anonymous-playback verdict is discarded HERE and only
				// here. ONE VERDICT PER CAPTURE is the design (Arc 10 R6):
				// processTwitchLive takes the mark at capture start and this
				// closure does not take it again.
				//
				// Not because repeats are expensive — they are not. A repeat
				// NoteTwitchAuthLoss with the same reason computes
				// changed == false and shouldFireRecovery declines, so it is
				// an idempotent status write. The reason is that this closure
				// is not a detector worth building the story on: it backs
				// refreshBestVariant, so it runs on EVERY (re)start of a
				// downloader — post-outage resume, gap recovery, quality
				// change, init change — and the quality probe, which means it
				// re-mints the playback token many times on one capture and
				// may also never run at all on a clean stream.
				//
				// What that costs, stated plainly because nothing else says
				// it: a credential that dies MID-CAPTURE on a chat-off job is
				// not marked until the next capture start.
				variants, _, err := w.tw.GetHLSMasterPlaylist(innerCtx, login)
				return variants, err
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
		// ErrCancelled: the row was already Cancelled when ExecuteWithChat
		// started. That early return used to be nil, which this read as a
		// finished download — it deleted the staging of a job the operator
		// had just cancelled and might Mux or Resume, and skipped the
		// cancellation's own handling.
		if ctx.Err() != nil || errors.Is(dlErr, ErrCancelled) {
			w.handleCancellation(job)
			return
		}
		w.setJobError(job, dlErr)
		return
	}

	w.cleanupStagingAfterMux(job.ID, jobCtx.StagingDir)
}

// cleanupStagingAfterMux removes a job's staging directory now that its
// recording is muxed into the archive — UNLESS a part's captured media is
// still unmuxed (both the stream-end mux and the finalize backstop failed for
// it). Deleting it then would silently drop footage from a job now marked
// Finished; preserve it so the Mux action can recover it.
//
// Shared by the queue path (processJob, its only caller until now) and the
// off-queue restart/Mux path (MuxJob): that path skips processJob entirely, so
// a job that restart-muxed to Finished kept its full raw recording in staging
// forever, roughly doubling the disk cost of that archive with no warning and
// nothing to reclaim it (sweep-2 Task 2 review, finding 1). One function
// rather than two copies, so the carve-outs cannot drift apart.
func (w *DownloadWorker) cleanupStagingAfterMux(jobID, stagingDir string) {
	if stagingDir == "" {
		return
	}
	// Every line below is the LAST thing the operator reads about this job,
	// and by the time we get here the row is already Finished: the finalize
	// write untracks the job's per-job log routing synchronously, inside
	// UpdateJobFields (notifyJobUpdate calls OnJobChange subscribers inline,
	// and cmd/moombox's syncJobLogRoutingOnChange untracks on terminal —
	// CORE-12). Without this bracket the staging outcome reaches the global
	// log only, and the per-job log every UI shows ends mid-finalize.
	//
	// The same bracket RecoverAsides uses, and restoreLogRouting re-reads the
	// row, so a job resurrected while this ran is left tracked.
	w.db.TrackJobForLogs(jobID)
	defer w.restoreLogRouting(jobID)
	fresh, _ := w.db.GetJob(jobID)
	preserveForTail := fresh != nil && fresh.IncompleteTail
	// A chat capture that ended without completing leaves its resume
	// sidecar in staging; deleting the dir turns a recoverable truncation
	// into a permanent one (sweep-2 TWITCH-3, verifier merge M5). Same
	// shape as the incomplete_tail preservation above it, and the orphan
	// scanner mirrors it in jobNeedsStaging — but only the chat files are
	// kept, not the muxed-away media (see keepOnlyChatCapture).
	preserveForChat := fresh != nil && fresh.ChatStatus == chatStatusIncomplete
	if asides := stagedAsideRecordings(stagingDir); len(asides) > 0 {
		// A recording the engine could not resume was set aside rather than
		// truncated (engine.StagedRestartSuffix), and nothing in the mux
		// pipeline has consumed it: the fresh capture that replaced it is not
		// guaranteed to be as long (post-live segments are not always
		// re-servable), so deleting it here would destroy the longer copy on
		// the strength of a shorter one finishing cleanly. Named in recording
		// order so an operator muxing them by hand knows which came first.
		w.logger.Warn("preserving staging dir: a set-aside recording was never merged into the archive; these are in recording order, oldest first",
			"asides", strings.Join(asides, " | "), "path", stagingDir, "jobID", jobID)
	} else if w.hasUnmuxedParts(jobID, stagingDir) {
		w.logger.Warn("preserving staging dir: a captured part is still unmuxed after finalize; recover via the Mux action",
			"path", stagingDir, "jobID", jobID)
	} else if preserveForTail {
		w.logger.Warn("preserving staging dir: recording tail incomplete; Resume will append the missing segments from the sidecar",
			"path", stagingDir, "jobID", jobID)
	} else if preserveForChat {
		// Keep the chat capture, drop everything else: the media in here
		// is already muxed into the output file, so shielding the whole
		// dir for downloader.incomplete_staging_expiry_days (7 by
		// default) to protect one JSON sidecar would cost tens of GB per
		// long VOD.
		if err := keepOnlyChatCapture(stagingDir); err != nil {
			w.logger.Warn("failed to prune staging dir down to the chat capture",
				"path", stagingDir, "jobID", jobID, "err", err)
		}
		w.logger.Warn("preserving staging dir: chat capture incomplete; the chat resume sidecar is kept for a re-run",
			"path", stagingDir, "jobID", jobID)
	} else if err := os.RemoveAll(stagingDir); err != nil {
		w.logger.Warn("failed to remove staging directory", "path", stagingDir, "err", err)
	} else {
		w.logger.Debug("removed staging directory", "path", stagingDir)
	}
}

// keepOnlyChatCapture deletes everything under a preserved staging dir except
// the chat capture, and is what makes the chat-incomplete keep cheap. What a
// re-run needs is chat.json — a resumed pager APPENDS to it
// (utils.AppendChatMessages, via internal/twitch/vod_chat.go's flush), so
// deleting it would replace hours of captured comments with the tail — and
// the sidecar beside it, chat.json.resume.json, which carries the content
// offset and the recent-ID window loadResumeState continues from. Everything
// else in the dir is raw media that the mux has already written into the
// output file; keeping it would cost tens of GB for a week
// (downloader.incomplete_staging_expiry_days, default 7) to protect two small
// JSON files.
//
// The names are matched at ANY depth, because a quality- or gap-split job
// keeps each part's chat beside that part's media in seg_N/. Directories left
// empty are removed, but the staging dir itself always stays: its existence
// is what jobNeedsStaging and the orphan scanner reason about. Removal
// failures are collected, not fatal — the first is returned for the caller to
// log, and anything that could not be removed simply stays.
func keepOnlyChatCapture(dir string) error {
	if dir == "" {
		return nil
	}
	var firstErr error
	note := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	// prune reports whether anything survived under path, so a directory that
	// held only media can be removed on the way back up.
	var prune func(path string) bool
	prune = func(path string) bool {
		entries, err := os.ReadDir(path)
		if err != nil {
			// Unreadable: keep it rather than guess at what is inside.
			note(err)
			return true
		}
		survived := false
		for _, e := range entries {
			child := filepath.Join(path, e.Name())
			if e.IsDir() {
				if prune(child) {
					survived = true
					continue
				}
				if err := os.Remove(child); err != nil {
					note(err)
					survived = true
				}
				continue
			}
			if isChatCaptureFile(e.Name()) {
				survived = true
				continue
			}
			if err := os.Remove(child); err != nil {
				note(err)
				survived = true
			}
		}
		return survived
	}
	prune(dir)
	return firstErr
}

// isChatCaptureFile reports whether a staging file belongs to the chat
// capture: chat.json itself, or anything the chat writers put beside it under
// that name — chat.json.resume.json (the resume sidecar,
// internal/twitch/vod_chat.go and internal/chat/downloader.go) and
// chat.json.lostbatch.json (a batch spilled when a write failed,
// internal/twitch/chat_recording.go). Media resume sidecars are NOT matched:
// they are named after their media file (video.ts.resume.json), which this
// prune is deleting because it has already been muxed.
func isChatCaptureFile(name string) bool {
	return name == "chat.json" || strings.HasPrefix(name, "chat.json.")
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
	// A recording the no-truncate guard set aside (<file>.restart-<ts>) is
	// captured media that no mux has ever consumed — an unmuxed part in every
	// sense this predicate is consulted for. It has no seg_N dir and no
	// segment row of its own, so without this term the scan below reports
	// "nothing unmuxed here" and the staging cleanup deletes it along with
	// the dir.
	//
	// The orphan scanner deliberately does NOT go through this door: it
	// consults hasUnmuxedSegmentParts and applies its own aside shield
	// (internal/worker/orphans.go, jobNeedsStaging). That shield is
	// UNCONDITIONAL — no age rule, unlike the tail and chat shields beside it
	// — because finalize muxes every READABLE aside into its own sibling
	// file, so one still in staging is one FFmpeg could not read: the only
	// copy of footage that exists nowhere else, not something that goes stale
	// after a week. The split exists so the sweep can name the asides it
	// found (OrphanedEntry.Asides) while the two expiring shields keep their
	// own rule.
	if len(stagedAsideRecordings(stagingDir)) > 0 {
		return true
	}
	return hasUnmuxedSegmentParts(db, jobID, stagingDir)
}

// hasUnmuxedSegmentParts is hasUnmuxedPartsForJob without the aside term: a
// quality/gap-split part whose staging dir still holds recognized media that
// has no segment row. Split out so the orphan scanner can apply its own rule
// to asides without losing this one (sweep-2 Task 11, extra item b).
func hasUnmuxedSegmentParts(db *database.Database, jobID, stagingDir string) bool {
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
func (w *DownloadWorker) handleCancellation(job *database.Job) {
	// Consume the user-cancel flag BEFORE Complete — Complete clears any
	// leftover flag as part of slot cleanup. Reading the flag is lock-only
	// (no DB write), so the free-slot-before-DB-writes ordering below holds.
	userCancelled := w.queue.WasCancelled(job.ID)

	if userCancelled {
		// Written while the run is still registered: a Retry/Resume/
		// Reinitialize clicked while this run unwound waits for its Done
		// (afterJobExit), which only processJob's deferred Complete closes —
		// so this Cancelled (CancelJob already wrote one) cannot land on top
		// of the retry's fresh status.
		updates := map[string]any{"status": database.StatusCancelled}
		if fresh, err := w.db.GetJob(job.ID); err == nil && fresh != nil {
			if cs, ok := cancelledChatStatus(fresh.ChatStatus); ok {
				updates["chat_status"] = cs
			}
		}
		w.db.UpdateJobFields(job.ID, updates)
	}

	// Free the slots — before the notification, symmetric with setJobError.
	// The run itself ends at processJob's deferred Complete.
	w.queue.ReleaseSlots(job.ID)

	if userCancelled {
		w.logger.Info("job cancelled by user", "jobID", job.ID)
		if w.notifier != nil {
			w.notifier.Send(notifications.JobCancelled(NotifyFacts(job)))
		}
	} else {
		// Shutdown: the existing status stays so the job resumes on restart.
		// Or the row was deleted (processJob's OnJobDeleted listener cancels
		// the same ctx), and there is nothing left to write.
		w.logger.Info("job interrupted (shutdown or row deleted), leaving its state as is", "jobID", job.ID)
	}
}

// cancelledChatStatus settles the chat_status a user cancel leaves behind.
// Neither orchestrator's cancel arm records a chat verdict — it stops the
// downloader and returns, and on a shutdown that is right, since the row keeps
// its state and the capture resumes on the next start. A user cancel is
// terminal, though, and both UIs render the value verbatim, so a Cancelled job
// went on showing its chat as "downloading" (or "pending") indefinitely. A
// capture that was running stopped short — "incomplete", the same verdict a
// cut-off capture gets everywhere else; one that never started has nothing to
// report. A settled verdict is left as it is (ok false).
func cancelledChatStatus(current string) (string, bool) {
	switch current {
	case "downloading":
		return chatStatusIncomplete, true
	case "pending":
		return "", true
	}
	return "", false
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
		cfgMaxRes, cfgMaxTimeout, cfgSegWorkers  int
		cfgProgressMS                            int
		cfgPrefer60, cfgChat                     bool
		cfgInterruptionTimeout                   config.FlexDuration
	)
	w.readConfig(func(c *config.MoomboxConfig) {
		cfgOutputDir = c.Paths.OutputDirectory
		cfgStagingDir = c.Paths.EffectiveStagingDir()
		cfgTemplate = c.Downloader.OutputTemplate
		cfgMaxRes = c.Downloader.MaxVideoResolution
		cfgPrefer60 = c.Downloader.Prefer60fps
		cfgChat = c.Downloader.DownloadChat
		cfgMaxTimeout = c.Downloader.MaximumTimeout
		cfgSegWorkers = c.Downloader.SegmentWorkers
		cfgProgressMS = c.Downloader.ProgressIntervalMS
		cfgInterruptionTimeout = c.Downloader.InterruptionTimeout
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
			MaxVideoResolution:  cfgMaxRes,
			Prefer60fps:         cfgPrefer60,
			OutputDirectory:     outputDir,
			StagingDirectory:    stagingDir,
			FilenameTemplate:    template,
			DownloadChat:        cfgChat,
			MaximumTimeout:      cfgMaxTimeout,
			SegmentWorkers:      cfgSegWorkers,
			InterruptionTimeout: cfgInterruptionTimeout.AsDuration(time.Minute),
			ProgressInterval:    time.Duration(cfgProgressMS) * time.Millisecond,
		},
		YT:           w.yt,
		StagingDir:   stagingDir,
		OutputDir:    outputDir,
		Filename:     filename,
		Logger:       w.logger,
		Interruption: &interruptionSignal{},
	}
}

// cookiesStatusError reports whether err's chain warrants StatusCookies
// rather than StatusError — i.e. the user acting on cookie auth is the fix:
// worker.ErrCookiesRequired (player-API member/login flags),
// twitch.ErrTwitchAuthExpired (GQL 401/403 after token rotation), and
// twitch.ErrSubscriberOnly (usher entitlement restriction — logging into an
// account that has access is the fix). Audit reports/twitch.md #8.
// ErrNotAMember belongs here too: the session is alive but lacks the
// membership, and credentials for an account that has it are still the fix.
func cookiesStatusError(err error) bool {
	return errors.Is(err, ErrCookiesRequired) ||
		errors.Is(err, ErrNotAMember) ||
		errors.Is(err, twitch.ErrTwitchAuthExpired) ||
		errors.Is(err, twitch.ErrSubscriberOnly)
}

// cookieRefreshWorthAttempting reports whether firing the automatic cookie
// refresh could plausibly fix err. Everything that lands on StatusCookies
// qualifies EXCEPT ErrNotAMember: YouTube already answered that request as a
// signed-in session, so refreshing (rotating) that same session changes
// nothing and its failure message would send the operator after credentials
// that are not the problem.
func cookieRefreshWorthAttempting(err error) bool {
	return !errors.Is(err, ErrNotAMember)
}

// parkReasonForError classifies err into the database.ParkReason persisted
// alongside StatusCookies. This is the durable half of the same distinction
// cookieRefreshWorthAttempting makes in-process: the automatic refresh decides
// once, here and now, but the auth-recovery sweep in cmd/moombox decides
// again — minutes, days, or a restart later — and needs the answer written
// down rather than re-derived from the job's error prose.
//
// ErrNotAMember is the only membership case: YouTube answered a demonstrably
// signed-in request with "members only", so no amount of restoring or
// rotating THESE credentials can help.
//
// twitch.ErrSubscriberOnly looks similar but is not, for two reasons. Usher's
// 403 does not distinguish an anonymous session from an un-entitled one, so
// working credentials genuinely may be the fix. And the retry loop this
// classification exists to break is structurally absent on that side anyway:
// the auth-recovered sweep only fires on a not-auth → auth TRANSITION, and an
// un-entitled account with healthy Twitch auth produces no transitions, so
// nothing re-runs the job in the first place. It stays in the auth class.
//
// Returns ParkReasonNone for anything that does not park at StatusCookies, so
// callers can write the field unconditionally and never leave a stale
// classification behind on a job that failed for an unrelated reason.
func parkReasonForError(err error) database.ParkReason {
	if !cookiesStatusError(err) {
		return database.ParkReasonNone
	}
	if errors.Is(err, ErrNotAMember) {
		return database.ParkReasonMembership
	}
	return database.ParkReasonAuth
}

// errorStage answers "which button fixes this" from the only signal the error
// carries: the prefix the orchestrator writes.
//
// Four finalize shapes exist in orchestrator_mux.go and all four are the mux
// stage: muxAndFinalize's fmt.Errorf("mux: %w", err), verifyMuxedDuration's
// short-output refusal "mux produced …", "no media files to mux…" and
// "create output dir: %w". The "mux" prefix also keeps "mux segment N:" on the
// mux side. All of them reach setJobError unwrapped — ExecuteWithChat returns
// the finalize error straight through — so the prefix survives. Everything
// else is the download stage.
//
// No line numbers here on purpose: an earlier draft carried them and every one
// had already drifted by the end of the arc. The citation gate does not read
// Go comments, so nothing would have caught it; the error strings below are
// greppable and cannot drift.
//
// The prefixes are anchored deliberately: an ffmpeg stderr tail from a
// DOWNLOAD failure can mention muxing anywhere in its 500 characters, and a
// substring match would flip the answer for the case that matters most.
//
// Known limit: two finalize returns in the same file still read as "download"
// — "create segment output dir: …" and "no segment files found in staging
// directory". Naming them would mean teaching every producer a stage argument;
// the prefixes below are what exists today.
func errorStage(errMsg string) string {
	// "mux" alone covers "mux: …", "mux produced …" and "mux segment N: …".
	for _, p := range []string{"mux", "no media files to mux", "create output dir"} {
		if strings.HasPrefix(errMsg, p) {
			return "mux"
		}
	}
	return "download"
}

func (w *DownloadWorker) setJobError(job *database.Job, err error) {
	// Give the slots back before the tail below (notifications, and an
	// automatic cookie refresh that can take minutes), so the next download
	// does not wait on it. Only the slots: the run stays registered until
	// processJob's deferred Complete — see JobQueue.ReleaseSlots. Whatever
	// re-enqueues this job meanwhile (AutoReinitializeJob, the cookie
	// resume below) waits for that exit through afterJobExit instead of
	// starting a second run beside this one.
	w.queue.ReleaseSlots(job.ID)

	errMsg := err.Error()
	w.logger.Error("job error", "jobID", job.ID, "err", errMsg)

	status := database.StatusError
	if cookiesStatusError(err) {
		status = database.StatusCookies
	}

	// park_reason and park_identity are written on EVERY error transition,
	// including the cleared case, so a job that once parked as "membership"
	// and later failed for something else does not carry the old
	// classification — a stale one would suppress a legitimate auth-recovery
	// resume forever, and a stale identity would fake an account change.
	//
	// The identity is captured only for a membership park: it is the account
	// the platform refused this job under, and it is meaningless for any other
	// failure.
	reason := parkReasonForError(err)
	identity := ""
	if reason == database.ParkReasonMembership && w.CurrentCredentialIdentity != nil {
		identity = w.CurrentCredentialIdentity(job.Platform)
	}
	w.db.UpdateJobFields(job.ID, map[string]any{
		"status":        status,
		"error":         errMsg,
		"park_reason":   reason,
		"park_identity": identity,
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
			// Guarded, and the title escaped: the two shapes the Job Failed
			// send below keeps. An empty field value is a permanent Discord
			// 400 that drops the whole embed, and a raw title beside an
			// escaped one renders two ways in one embed.
			authFields := notifications.NewFieldBuilder().
				AddInlineIf(job.ChannelName != "", "Channel", job.ChannelName).
				AddInlineIf(job.VideoID != "", notifications.IDLabel(job.Platform), job.VideoID).
				Add("Reason", reason).
				Build()
			// The same row→facts mapper the Job Failed send below uses, for
			// the same three reasons and one more. This is a PER-JOB auth
			// alert — one dead cookie parks N jobs — and isBatchable
			// (internal/notifications/batch.go) admits `auth` into the 5 s
			// coalescing window only when it carries a JobID, so without
			// these the burst this arc exists to fold never folds. The footer
			// reads "Moombox" with no platform and no id for the one alert an
			// operator most needs to tell apart, and the deep link, which
			// keys on JobID and Author both, never applies.
			af := NotifyFacts(job)
			w.notifier.Send("Authentication Required",
				fmt.Sprintf("Cookies needed: %s", notifications.EscapeMarkdown(job.Title)),
				notifications.TypeWarning,
				authFields,
				notifications.SendOptions{
					URL:       af.URL,
					Thumbnail: af.ThumbnailURL,
					Event:     "auth",
					Author:    notifyAuthor(af),
					Platform:  af.Platform,
					JobID:     af.ID,
				},
			)
		} else {
			// The one row→facts mapper, so the only job send that is not a
			// builder call still carries the identity every builder sets: the
			// URL with its watch fallback, the author line, the platform and
			// the job id. Arc N3's terminal edit on "error" and Arc N2b's deep
			// link both key on Opts.JobID, and an embed with none can be
			// neither edited nor linked.
			f := NotifyFacts(job)
			var stagingBase string
			w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
			// "preserved" is the same predicate the resume route gates on
			// (HasStagingFiles), and Resume is YouTube-only in both UIs — a
			// Twitch job told "Resume available" gets a 400.
			staging := "removed"
			if HasStagingFiles(stagingBase, job.ID) {
				staging = "preserved"
				if job.Platform != "twitch" {
					staging = "preserved — Resume available"
				}
			}
			asides := len(ScanAsides(stagingBase, job.ID).Groups)

			fields := notifications.NewFieldBuilder().
				// Guarded for the same reason the builders guard their id
				// field: Field.Value carries no omitempty, clampEmbed never
				// drops an empty value, and Discord answers one with a 400 that
				// discord.go treats as permanent — the whole embed is dropped
				// after a single attempt.
				AddInlineIf(job.ChannelName != "", "Channel", notifications.EscapeMarkdown(job.ChannelName)).
				AddInlineIf(job.VideoID != "", notifications.IDLabel(job.Platform), job.VideoID).
				// Error is ALREADY wrapped by N1 — this Error field is one of
				// the four sites N1 escapes. Carry N1's line through unchanged;
				// a second wrap renders every \* as \\*. ChannelName is NOT one
				// of N1's four, so the wrap above is new and single. (No line
				// number: the one this comment used to carry was stale within
				// the arc, and nothing checks comments.)
				Add("Error", notifications.EscapeMarkdown(errMsg)).
				AddInline("Stage", errorStage(errMsg)).
				AddInline("Staging", staging).
				AddIf(job.AutoRetryCount > 0, "Automatic Retries",
					fmt.Sprintf("gave up after %d/%d", job.AutoRetryCount, MaxTwitchAutoRetries)).
				AddIf(asides > 0, "Set-aside recordings",
					fmt.Sprintf("%d — Recover to mux them", asides)).
				Build()
			w.notifier.Send("Job Failed",
				fmt.Sprintf("Job failed for: %s", notifications.EscapeMarkdown(job.Title)),
				notifications.TypeError,
				fields,
				notifications.SendOptions{
					URL:       f.URL,
					Thumbnail: f.ThumbnailURL,
					Event:     "error",
					Author:    notifyAuthor(f),
					Platform:  f.Platform,
					JobID:     f.ID,
				},
			)
		}
	}

	// Automatic cookie recovery. Deliberately OUTSIDE the notifier branch
	// above: an attempt to fix the session is not a notification, and gating
	// it on w.notifier != nil meant a deployment with no webhook configured
	// silently got neither the recovery nor any log line explaining why.
	// attemptCookieRefresh owns the "should we, and if not why not" decision
	// so that every reason for declining is stated in one place.
	if status == database.StatusCookies {
		w.attemptCookieRefresh(job, err)
	}
}

// attemptCookieRefresh runs (or deliberately declines to run) the automatic
// cookie refresh for a job that just parked at StatusCookies, and — when it
// cannot fix things — says what WILL, in terms the operator can act on.
//
// The advice is deliberately environment-neutral and leads with the cookie
// file. "Re-run setup from Settings" used to be the only thing printed here,
// and it is a dead end wherever the interactive browser login cannot run: it
// needs a headed browser and a person at it, and the setup endpoints are
// loopback-gated so a remote dashboard cannot reach them either. Naming the
// configured cookie file path instead makes the message concrete in every
// deployment — a Docker operator reads "/data/cookies.txt" and knows exactly
// which host file to replace — without probing for an environment we cannot
// reliably detect.
//
// The distinction that holds over time is between LOGGING IN (interactive,
// needs a browser and a human) and SUPPLYING COOKIES (a file Moombox reads).
// Only the first is bounded by the environment; phrase guidance against that
// line rather than against "container", which is a moving target.
func (w *DownloadWorker) attemptCookieRefresh(job *database.Job, err error) {
	// ErrNonActionable means "terminal, stop working this job", and the
	// recovery path does not merely log — on success it sets the job back to
	// Upcoming and re-enqueues it, restarting the probe budget from zero. The
	// two categories can co-occur: a multi-%w error (stream_processor_twitch's
	// probe give-up wraps ErrNonActionable alongside an underlying error that
	// may carry twitch.ErrTwitchAuthExpired) satisfies cookiesStatusError and
	// ErrNonActionable at the same time. That is unreachable today only
	// because classifyProbeErr's default routes "gql auth failure (401)" to
	// the network class — a string heuristic over the error's status-bearing
	// PREFIX (gqlRequest reports only a byte count, never the response body),
	// not an invariant worth relying on for a resurrection hazard.
	if errors.Is(err, ErrNonActionable) {
		w.logger.Warn("skipping automatic cookie refresh — this failure was already classified terminal, and recovery would re-queue the job and reset its retry budget",
			"jobID", job.ID,
			"videoID", job.VideoID)
		return
	}
	if !cookieRefreshWorthAttempting(err) {
		w.logger.Warn("skipping automatic cookie refresh — YouTube answered this request as a SIGNED-IN session, so the credentials are alive and the account simply lacks access",
			"jobID", job.ID,
			"videoID", job.VideoID,
			"fix", "supply cookies from the account that holds the channel membership")
		return
	}
	if w.OnCookieRefreshNeeded == nil {
		// Not wired (no auto-cookie service constructed). Debug rather than
		// Warn: this is a build/wiring fact, not something the operator did.
		w.logger.Debug("no automatic cookie refresh is wired; leaving the job parked",
			"jobID", job.ID, "videoID", job.VideoID)
		return
	}

	// job.Platform verbatim, with no defaulting applied here.
	//
	// Every production creator sets Platform explicitly, and this reads the
	// in-memory struct rather than a row, so the schema default never enters
	// into it. If one ever arrives empty, RefreshResult.Verdict("") is
	// RefreshUnknown — no health claim either way — and the job stays parked
	// for a human. Guessing "youtube" here would trade that safe outcome for
	// a second defaulting rule to keep in sync with the creators.
	w.logger.Info("attempting automatic cookie refresh...", "platform", job.Platform)
	if w.OnCookieRefreshNeeded(job.Platform) {
		w.logger.Info("cookie refresh succeeded, retrying job", "platform", job.Platform)
		// Upcoming, not Live, so StreamProcessor.Process re-probes and
		// classifies the stream afresh (per audit reports/worker.md
		// Finding 21) — or Queued for a backlog VOD, which re-enters through
		// the scheduler's pacing like the cookie-parked sweep's resumes.
		status, err := CookieResumeStatus(w.db, job)
		if err != nil {
			w.logger.Debug("could not read the feed_items partner; resuming to Upcoming",
				"jobID", job.ID, "err", err)
		}
		w.db.UpdateJobFields(job.ID, map[string]any{
			"status":        status,
			"error":         "",
			"park_reason":   database.ParkReasonNone,
			"park_identity": "",
		})
		// This runs inside the parked run's own tail, which is still
		// registered: an Enqueue now would be dropped as a duplicate (and the
		// job left for the heartbeat), so the hand-off waits for the run to
		// exit.
		w.afterJobExit(job.ID, "cookie-refresh resume", func() {
			if status == database.StatusQueued {
				if w.scheduler != nil {
					w.scheduler.Wake()
				}
			} else {
				w.queue.Enqueue(job.ID, database.StatusUpcoming)
			}
		})
		return
	}

	var cookieFile string
	w.readConfig(func(c *config.MoomboxConfig) { cookieFile = c.Cookies.CookieFile })
	if cookieFile == "" {
		w.logger.Warn("auto cookie refresh failed — no cookie file is configured",
			"fix", "set cookies.cookie_file to a Netscape cookies.txt exported from a browser signed in to the account")
		return
	}
	w.logger.Warn("auto cookie refresh failed — the cookie file has to be replaced by hand",
		"cookieFile", cookieFile,
		"fix", "export a fresh Netscape cookies.txt from a browser signed in to the account and overwrite that file",
		"why", "browsing on in the source browser profile rotates the session and invalidates an earlier export — export from a private window, then close it",
		"wizard", "the interactive browser login in Settings needs a headed browser and a person at it, so it runs only on the machine hosting Moombox")
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

// inFlightWait is how long Stop waits for in-flight jobs to finish before it
// cancels their muxes.
const inFlightWait = 10 * time.Second

// muxCancelGrace is how long Stop waits for FFmpeg to die after the mux root
// is cancelled, before giving up and exiting anyway.
const muxCancelGrace = 2 * time.Second

// StopBudget is the longest Stop can take: the in-flight wait plus the grace
// after mux cancellation. A caller's force-exit backstop must outlast it, or
// the process exits before Stop reaches CancelMuxes and FFmpeg is left
// writing into a staging dir the restarted child re-muxes with -y (owner
// decision O-E).
const StopBudget = inFlightWait + muxCancelGrace

// Stop signals the worker to stop processing new jobs and waits for in-flight
// jobs to finish (up to 10 seconds) so downloads aren't interrupted mid-write.
// Muxes still running after that wait are cancelled rather than orphaned
// (owner decision O-E) — their rows stay Muxing and the restarted child
// re-muxes them from the staging that is deliberately left in place.
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
	case <-time.After(inFlightWait):
		// Owner decision O-E: the jobs still running at this point are almost
		// always draining a mux, and exiting now would leave FFmpeg writing
		// into a staging dir the restarted child re-muxes with -y. Cancel the
		// mux root so those processes die with us; their partial part files
		// are re-muxed on restart.
		w.logger.Warn("download worker: timed out waiting for in-flight jobs; cancelling in-flight muxes")
		if w.orchestrator != nil {
			w.orchestrator.CancelMuxes()
		}
		select {
		case <-done:
			w.logger.Info("download worker: in-flight jobs finished after mux cancellation")
		case <-time.After(muxCancelGrace):
			w.logger.Warn("download worker: in-flight jobs still running after mux cancellation")
		}
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

// SetFfmpegPath forwards a paths.ffmpeg_path hot-reload to the orchestrator,
// whose muxer serves every download's mux, probe and part merge.
func (w *DownloadWorker) SetFfmpegPath(path string) {
	if w.orchestrator != nil {
		w.orchestrator.SetFfmpegPath(path)
	}
}

// FFprobePath reports the ffprobe path of the orchestrator's current muxer
// (observability for the hot-reload path; downloads themselves go through the
// orchestrator's own accessor). Empty when there is no orchestrator yet.
func (w *DownloadWorker) FFprobePath() string {
	if w.orchestrator == nil {
		return ""
	}
	return w.orchestrator.mux().FFprobePath()
}

// SetOnTwitchAuthLoss wires the Twitch platform-mark seam through to the
// stream processor, which is where the chat downgrade is observed.
//
// cmd/moombox holds both the refresh service and the worker; the worker holds
// the stream processor. This is the same one-hop forwarding SetConfigStore
// does, and it exists so cmd/moombox never has to know that the stream
// processor is where the callback lands.
func (w *DownloadWorker) SetOnTwitchAuthLoss(fn func(reason string)) {
	if w.streamProc != nil {
		w.streamProc.SetOnTwitchAuthLoss(fn)
	}
}

// ReauthenticateTwitchChats tells every live Twitch IRC chat downloader to
// re-read its credentials and reconnect, and returns how many were told.
//
// Called by cmd/moombox from RefreshService.OnCredentialsChanged("twitch") —
// the only signal a capture that is already running has that repaired cookies
// are on disk. Returns a COUNT and nothing else: no channel, no job, no
// account. "Told" is not "authenticated": a downloader with no live session
// only has its latches cleared.
//
// Nil-safe on both the receiver and the registry, so a partially constructed
// worker degrades to "nothing to tell" rather than panicking at the moment an
// operator fixes their credentials.
func (w *DownloadWorker) ReauthenticateTwitchChats() int {
	if w == nil {
		return 0
	}
	return w.twitchChats.reauthenticateAll()
}

// ResumeJob resumes a cancelled/errored YouTube job from its saved state.
// Preserves staging files, progress, and seq numbers. Resets auto_retry_count
// so any future error fires its notification — Resume is user-driven, so the
// "suppress retry-failure notifications" guard in setJobError must not apply.
func (w *DownloadWorker) ResumeJob(jobID string) {
	w.afterJobExit(jobID, "resume", func() {
		w.db.UpdateJobFields(jobID, map[string]any{
			"status":           database.StatusDownloading,
			"error":            "",
			"park_reason":      database.ParkReasonNone,
			"park_identity":    "",
			"auto_retry_count": 0,
		})
		w.EnqueueJob(jobID)
	})
}

// afterJobExitTimeout bounds how long a deferred Resume/Reinitialize waits for
// the job's previous run to unwind (a chat capture's shutdown grace is the
// long pole) before giving up rather than racing it.
const afterJobExitTimeout = 60 * time.Second

// afterJobExit runs fn now when jobID has no run in flight, or once the run
// that is still unwinding has exited. The cancel route writes Cancelled
// before the run has stopped, so Retry and Resume appear at once — and one
// clicked in that window used to race the run: Enqueue dropped the job as
// still processing, handleCancellation then wrote Cancelled over the fresh
// status, and Reinitialize deleted the staging the run was still writing its
// chat resume sidecar into. Callers stay synchronous (the TUI calls these on
// its update goroutine); the wait happens here, one per job.
func (w *DownloadWorker) afterJobExit(jobID, what string, fn func()) {
	if !w.queue.IsProcessing(jobID) {
		fn()
		return
	}
	if _, waiting := w.afterExitPending.LoadOrStore(jobID, struct{}{}); waiting {
		return // a reset is already waiting on this run
	}
	w.logger.Info("waiting for the job's previous run to stop before "+what, "jobID", jobID)
	w.wg.Go(func() {
		defer w.afterExitPending.Delete(jobID)
		defer func() {
			if r := recover(); r != nil {
				w.logger.Error("panic in deferred "+what, "jobID", jobID, "panic", fmt.Sprint(r))
			}
		}()
		if !w.WaitForJobExit(jobID, afterJobExitTimeout) {
			w.logger.Warn(what+" skipped: the job's previous run did not stop", "jobID", jobID,
				"waited", afterJobExitTimeout)
			return
		}
		fn()
	})
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
// Clears all progress fields and deletes the staging directory — after the
// job's previous run has stopped, when one is still unwinding (afterJobExit).
func (w *DownloadWorker) ReinitializeJob(jobID string) {
	w.afterJobExit(jobID, "reinitialize", func() { w.reinitializeNow(jobID) })
}

func (w *DownloadWorker) reinitializeNow(jobID string) {
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
		"park_reason":         database.ParkReasonNone,
		"park_identity":       "",
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
	// The monitor reacts to the Error row the moment setJobError writes it,
	// while that run is still finishing its tail: wait for it to exit, as a
	// user's Reinitialize does, rather than reset the row and staging under
	// it (and have the Enqueue dropped as a duplicate).
	w.afterJobExit(jobID, "auto-reinitialize", func() { w.autoReinitializeNow(jobID) })
}

func (w *DownloadWorker) autoReinitializeNow(jobID string) {
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
		"park_reason":         database.ParkReasonNone,
		"park_identity":       "",
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

// Asides reports the set-aside recordings in a job's staging directory, and
// whether that directory also still holds the chat capture keepOnlyChatCapture
// preserves. The read half of the recovery verb: RecoverAsides is what acts on
// it, and both UIs render it under the job.
//
// Errors only when the job row cannot be read — an empty report is a perfectly
// ordinary answer, and the caller must be able to tell it from "that job is
// gone".
func (w *DownloadWorker) Asides(jobID string) (AsideReport, error) {
	job, err := w.db.GetJob(jobID)
	if err != nil {
		return AsideReport{}, fmt.Errorf("look up job %s: %w", jobID, err)
	}
	if job == nil {
		return AsideReport{}, fmt.Errorf("job %s not found", jobID)
	}
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) {
		stagingBase = c.Paths.EffectiveStagingDir()
	})
	return ScanAsides(stagingBase, jobID), nil
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

	// The same per-job staging claim RecoverAsides takes. Both verbs mux out
	// of one directory into one output directory, and both reach
	// muxStagedAsides with their own asideOutputPath collision map — so two of
	// them together write the same sibling name over each other. Before the
	// status write, so a refusal leaves the row exactly as it found it.
	release, err := w.claimJobOperation(jobID, opMux)
	if err != nil {
		return err
	}

	w.db.UpdateJobFields(jobID, map[string]any{
		"status": database.StatusMuxing,
	})

	w.wg.Go(func() {
		defer release()
		defer func() {
			if r := recover(); r != nil {
				w.logger.Error("panic in MuxJob", "jobID", jobID, "panic", fmt.Sprint(r))
				w.db.UpdateJobFields(jobID, map[string]any{
					"status": database.StatusError,
					"error":  fmt.Sprintf("internal panic: %v", r),
				})
			}
		}()

		// Owner decision O-E: the orchestrator's mux root, never
		// context.Background() — a Stop reaches this FFmpeg instead of leaving
		// it writing into a staging dir the restarted child re-muxes with -y.
		root := w.orchestrator.muxRoot()

		// The operator's Cancel reaches this FFmpeg too. Both UIs offer Cancel
		// on a Muxing row and the route writes Cancelled, but queue.Cancel
		// only knows the jobs it dequeued — this one it never saw — so the
		// mux ran on, wrote Finished over the Cancelled row and sent a
		// "Download Finished" embed after the "Job Cancelled" one. Listened
		// for the way ExecuteWithChat listens, and a deleted row stops it the
		// way processJob's listener does. Subscribed BEFORE the row is read,
		// so a cancel that lands first is seen in the row instead.
		ctx, cancelMux := context.WithCancel(root)
		defer cancelMux()
		unsubscribe := w.db.OnJobUpdate(func(updated *database.Job) {
			if updated.ID == jobID && updated.Status == database.StatusCancelled {
				cancelMux()
			}
		})
		defer unsubscribe()
		unsubscribeDel := w.db.OnJobDeleted(func(deleted *database.JobDeleted) {
			if deleted.JobID == jobID {
				cancelMux()
			}
		})
		defer unsubscribeDel()

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
		if job.Status == database.StatusCancelled {
			cancelMux()
		}

		jobCtx := w.buildJobContext(job)

		// This mux takes the same download slot a queued job takes: a boot
		// that finds N interrupted Muxing rows would otherwise start N
		// FFmpegs at once, which num_parallel_downloads exists to prevent.
		// The wait ends on the mux root's cancellation, so a shutdown does not
		// sit here holding the process open.
		if !w.queue.AcquireDownloadSlot(ctx, jobID) {
			if root.Err() == nil {
				w.logger.Info("MuxJob: cancelled while waiting for a mux slot; staging is kept", "jobID", jobID)
				return
			}
			w.logger.Info("MuxJob: shutdown while waiting for a mux slot; the row stays Muxing for the next start", "jobID", jobID)
			return
		}
		defer w.queue.ReleaseDownloadSlot(jobID)

		if err := w.orchestrator.muxFromStaging(ctx, jobCtx); err != nil {
			if ctx.Err() != nil && root.Err() == nil {
				// Cancelled (or deleted) by the operator; staging stays for a
				// later Mux or Resume. The row says Cancelled unless the mux's
				// own Muxing write landed just after the route's — re-assert
				// it so the row cannot be left Muxing with nothing running.
				w.logger.Info("MuxJob: cancelled; staging is kept", "jobID", jobID)
				if fresh, _ := w.db.GetJob(jobID); fresh != nil && fresh.Status != database.StatusCancelled {
					w.db.UpdateJobFields(jobID, map[string]any{"status": database.StatusCancelled})
				}
				return
			}
			if ctx.Err() != nil {
				// Cancelled by Stop, not a failure: leave the row Muxing with
				// its staging intact so the restarted child re-muxes it
				// (muxOnRestart only routes a Muxing row — writing Error here
				// would strand the recording).
				w.logger.Info("MuxJob: cancelled by shutdown; the row stays Muxing for the next start", "jobID", jobID)
				return
			}
			w.logger.Error("MuxJob failed", "jobID", jobID, "err", err)
			w.db.UpdateJobFields(jobID, map[string]any{
				"status": database.StatusError,
				"error":  err.Error(),
			})
			return
		}

		// The recording is in the archive now: reclaim staging exactly the way
		// the queued path does, carve-outs included. Without this the
		// off-queue route kept every restart-muxed job's raw recording forever
		// (sweep-2 Task 2 review, finding 1).
		w.cleanupStagingAfterMux(jobID, jobCtx.StagingDir)
	})

	return nil
}

// claimJobOperation takes one job's staging claim for op and returns the
// release. It refuses with ErrStagingBusy, naming the operation in the way,
// when somebody already holds it.
//
// Taken SYNCHRONOUSLY by the caller, before it spawns anything, so a refusal
// is reported to the operator rather than discovered by two FFmpeg processes.
// The release is always deferred by the goroutine that got it, so a panicking
// run cannot leave a job stuck for the process's life.
func (w *DownloadWorker) claimJobOperation(jobID, op string) (func(), error) {
	w.stagingClaimMu.Lock()
	defer w.stagingClaimMu.Unlock()
	if held, busy := w.stagingClaims[jobID]; busy {
		return nil, fmt.Errorf("%w (%s in progress)", ErrStagingBusy, held)
	}
	if w.stagingClaims == nil {
		w.stagingClaims = map[string]string{}
	}
	w.stagingClaims[jobID] = op
	return func() {
		w.stagingClaimMu.Lock()
		defer w.stagingClaimMu.Unlock()
		delete(w.stagingClaims, jobID)
	}, nil
}

// restoreLogRouting puts a job's per-job log routing back where cmd/moombox's
// syncJobLogRouting would have it: tracked while non-terminal, untracked (with
// the buffer kept) once terminal.
//
// The counterpart to the TrackJobForLogs RecoverAsides does on the way in.
// That bracket exists because RouteLogToJobs scans only db.logRouted, and
// SyncJobLogTracking deletes every terminal job from it (CORE-12) — so a
// recovery, which runs ONLY on a terminal job and deliberately never writes a
// status, would emit every one of its log lines into nothing. MuxJob has no
// such problem: it flips the row to Muxing first.
//
// Re-reads the row rather than unconditionally untracking, so a job that was
// resurrected while the recovery ran (a /resume, a retry) is left tracked.
func (w *DownloadWorker) restoreLogRouting(jobID string) {
	fresh, err := w.db.GetJob(jobID)
	if err != nil || fresh == nil || fresh.IsTerminal() {
		w.db.UntrackJobForLogs(jobID)
		return
	}
	w.db.TrackJobForLogs(jobID)
}

// RecoverAsides muxes every recording the engine set aside for this job into
// its own file beside the job's archive, carries the kept chat capture beside
// the first of them, and then reclaims the staging directory — but only when
// nothing a mux still owes is left in it (see the guard at the end of the
// goroutine); the remaining carve-outs are cleanupStagingAfterMux's own.
//
// The off-queue twin of MuxJob, and deliberately NOT part of it: MuxJob means
// "mux the recording" and is gated on HasSegmentFiles, which does not know the
// .restart-<ts> suffix — an aside-only staging dir has no recording to mux
// (spec §5, "recovery is its own verb"). The job's STATUS is never written
// here either: the job finished (or failed, or was cancelled) long ago and
// this is not its lifecycle. Progress and failures reach the operator the way
// the aside mux always has, through the job's log lines.
//
// Returns one of the three typed refusals synchronously; a nil return means
// the recovery is running.
func (w *DownloadWorker) RecoverAsides(jobID string) error {
	job, err := w.db.GetJob(jobID)
	if err != nil {
		return fmt.Errorf("look up job %s: %w", jobID, err)
	}
	if job == nil {
		return fmt.Errorf("job %s not found", jobID)
	}
	if IsActiveJobStatus(job.Status) {
		return fmt.Errorf("%w (status %s)", ErrRecoveryJobActive, job.Status)
	}
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) {
		stagingBase = c.Paths.EffectiveStagingDir()
	})
	if len(ScanAsides(stagingBase, jobID).Groups) == 0 {
		return ErrNoAsides
	}
	release, err := w.claimJobOperation(jobID, opRecoverAsides)
	if err != nil {
		return err
	}
	// The job is terminal, so nothing is routing its log lines (CORE-12).
	// Both UIs point the operator at the job's log for this run's progress, so
	// route to it for the duration and hand it back at the end. Best-effort,
	// not a guarantee: SyncJobLogTracking drops every terminal ID from the
	// routed set and re-runs on each OnJobsChange fan-out, i.e. on every AddJob
	// and DeleteJob — so a stream discovered while a long aside is muxing
	// silently ends the routing and the rest of this run's lines go nowhere.
	w.db.TrackJobForLogs(jobID)

	w.wg.Go(func() {
		// Declared FIRST so it runs LAST: the recover handler's own line still
		// has to reach the job's log.
		defer w.restoreLogRouting(jobID)
		defer func() {
			if r := recover(); r != nil {
				// No status write: a panic in the recovery must not turn a
				// Finished job into an Error one. The asides are still in
				// staging, where the shield holds them for another attempt.
				w.logger.Error("panic in RecoverAsides", "jobID", jobID, "panic", fmt.Sprint(r))
			}
		}()
		defer release()

		jobCtx := w.buildJobContext(job)
		// The orchestrator's mux root, never context.Background(), for the
		// reason MuxJob gives: a Stop reaches this FFmpeg instead of leaving
		// it writing into a staging dir.
		ctx := w.orchestrator.muxRoot()

		// Same download slot a queued job takes — this starts an FFmpeg, and
		// num_parallel_downloads exists to bound exactly that.
		if !w.queue.AcquireDownloadSlot(ctx, jobID) {
			w.logger.Info("RecoverAsides: shutdown while waiting for a mux slot; the recordings stay in staging", "jobID", jobID)
			return
		}
		defer w.queue.ReleaseDownloadSlot(jobID)

		if err := w.orchestrator.recoverAsides(ctx, jobCtx); err != nil {
			if ctx.Err() != nil {
				w.logger.Info("RecoverAsides: cancelled by shutdown; the recordings stay in staging", "jobID", jobID)
				return
			}
			w.logger.Error("set-aside recovery did not finish; the recordings it could not mux stay in staging",
				"jobID", jobID, "err", err)
			return
		}

		// Every aside is out of staging now — but that is NOT enough to hand
		// the directory to cleanupStagingAfterMux. Its four shields are:
		// asides present (just consumed), hasUnmuxedParts (FALSE for a
		// single-file job, because hasUnmuxedSegmentParts returns false with
		// no seg_N dirs), IncompleteTail, and chat-incomplete. A Cancelled or
		// Error job whose staging holds both the fresh video.mp4 and an aside
		// — the commonest shape after a mid-stream restart, and exactly what
		// /mux and A M exist to rescue — falls through all four to
		// os.RemoveAll. Recovering the asides would delete the main recording.
		//
		// So: reclaim only when the directory holds no recognised media
		// (discoverStagingMedia, the same discovery muxFromStaging uses) and
		// no unmuxed part. The remaining jobNeedsStaging terms — the
		// incomplete tail and the chat capture — are cleanupStagingAfterMux's
		// own, and it still prunes rather than deletes for the chat one.
		//
		// …and the ROW has to be re-read first, because the decision above is
		// about a directory somebody else may now own. This verb writes no
		// status, so for its whole run the row still reads
		// Error/Cancelled/Finished — exactly what /retry, /resume and
		// /reinitialize accept — and a row parked at COOKIES? is promoted to
		// Upcoming by the credential sweep with no operator action at all. A
		// revival hands the SAME staging directory to a fresh download whose
		// first files are not yet a name discoverStagingMedia knows, so the
		// media guard would wave it through and the cleanup would os.RemoveAll
		// a live capture. MuxJob is immune only because it flips the row to
		// Muxing before it spawns; the claim covers the two off-queue verbs,
		// not the queue.
		fresh, _ := w.db.GetJob(jobID)
		if fresh == nil || IsActiveJobStatus(fresh.Status) {
			status := "row deleted"
			if fresh != nil {
				status = string(fresh.Status)
			}
			w.logger.Warn("set-aside recordings recovered; staging kept: the job is no longer the terminal row this recovery started from",
				"status", status, "path", jobCtx.StagingDir, "jobID", jobID)
			return
		}
		if discoverStagingMedia(jobCtx.StagingDir) == nil && !w.hasUnmuxedParts(jobID, jobCtx.StagingDir) {
			w.cleanupStagingAfterMux(jobID, jobCtx.StagingDir)
			return
		}
		w.logger.Warn("set-aside recordings recovered; staging kept: unmuxed recording present",
			"path", jobCtx.StagingDir, "jobID", jobID)
	})

	return nil
}
