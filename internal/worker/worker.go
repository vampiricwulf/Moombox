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
	"sync/atomic"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/cipher"
	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/disk"
	"github.com/vampiricwulf/Moombox/internal/jobstate"
	"github.com/vampiricwulf/Moombox/internal/jobverbs"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/redact"
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

// ErrRateLimited marks a playability refusal of the session rather than of
// the video (youtube.PlayabilityRateLimited: "try again later", or a captcha
// challenge, on an extraction with no formats), from Process or from a stale
// VOD's re-extraction. processJob routes it — and an Innertube 429 on the
// whole cascade, the same limit's HTTP form (isRateLimited) — to
// outlastRateLimit (rate_limit.go): a backlog VOD back to Queued for an
// hour, any other job waiting in place, instead of setJobError, which would
// end in Error a job that only had to wait.
var ErrRateLimited = errors.New("rate-limited by YouTube")

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
	// collision is symmetric. MuxJob also answers it while the job's last
	// run is still registered (muxJob).
	ErrStagingBusy = errors.New("another operation is already working this job's staging directory")
	// ErrNoAsides: nothing in staging was ever set aside. Recovery is not a
	// second Mux button, so this is a refusal rather than a no-op success.
	ErrNoAsides = errors.New("no set-aside recordings in staging")
)

// jobOp names an operation holding a job's staging claim (claimJobOperation):
// the per-job lease every verb that writes or deletes a job's staging outside
// a queue run takes, so a refusal or a wait can say what is in the way. The
// values are the words the operator reads in a 409 or a TUI feedback line.
type jobOp string

// The operations that can hold a job's staging claim. Named so a refusal can
// say what is in the way. opChapterImport and opBootCleanup live beside their
// operations (chapter_import.go, boot_leftovers.go).
const (
	opMux           jobOp = "mux"
	opRecoverAsides jobOp = "set-aside recovery"
)

// The reset verbs hold the claim under their afterJobExit names while their
// reset runs (hold), each with its afterExitPending entry beside it.
const (
	opResume              jobOp = "resume"
	opReinitialize        jobOp = "reinitialize"
	opAutoReinitialize    jobOp = "auto-reinitialize"
	opCookieRefreshResume jobOp = "cookie-refresh resume"
)

// IsResetVerb reports whether op is a reset verb running its reset. Such a
// holder is also the job's pending verb, so the REST layer lets the request
// through to the worker — a repeat of the same verb is then the quiet no-op
// afterJobExit makes it, a different one ErrResetPending — instead of
// answering a double-click with "staging is busy".
func (op jobOp) IsResetVerb() bool {
	switch op {
	case opResume, opReinitialize, opAutoReinitialize, opCookieRefreshResume:
		return true
	}
	return false
}

// heartbeatInterval is the safety-net poll interval for catching missed jobs.
// Normal job discovery is signal-driven via NotifyNewJob. The backlog
// scheduler reuses it as its sweep heartbeat (spec §10) — its normal path is
// also signal-driven, via Scheduler.Wake.
const heartbeatInterval = 60 * time.Second

// MaxTwitchAutoRetries caps how many times the Twitch monitor's auto-recovery
// will re-enqueue an errored Twitch live job before giving up
// (jobstate.MaxTwitchAutoRetries, which jobstate.TwitchAutoRetryPending
// reads). User-driven Reinit always resets this counter (see ReinitializeJob).
const MaxTwitchAutoRetries = jobstate.MaxTwitchAutoRetries

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
	ChatStatus database.ChatStatus
	// CapturedMux marks the operator's Mux of a job that was not already
	// muxing — an Error, Cancelled or parked row, set by MuxJob. What is
	// staged may be all there is, so the muxing embed does not claim the
	// download completed. A boot re-mux of an interrupted Muxing row leaves
	// it false: that download did complete.
	CapturedMux bool
	// FinishReclaimed is set by a finalize whose finishing mark re-claimed
	// the row from a Cancel that landed after its mux had completed (owner
	// ruling R-H2, beginFinishing). That Cancel ended the run's context, and
	// the job finished all the same: what its run still does after the
	// finalize — the post-download trim — runs on the mux root instead.
	FinishReclaimed bool
	// Refinalize marks a finalize of the recording the job's archive already
	// holds: MuxJob's, which re-muxes the staging the archive was (or was to
	// be) made from — the Mux action over an Error or Cancelled row, the
	// automatic mux of an ended Twitch broadcast, the boot re-mux of an
	// interrupted Muxing row (owner decision CJ-2) — and a download run that
	// continues it (owner decision CK-1: a Resume, the monitor's auto-resume,
	// a parked or requeued row's retry — runJobContext). It keeps a chapter
	// import's list beside the archive (keptImportFor) where every other
	// finalize drops it. A Reinitialize is a new recording and drops the
	// import when it resets the row (reinitializeNow), so no finalize after
	// one can find it.
	Refinalize bool
	// RecordedFirstPart is the first recorded part's file (segment rows, in
	// GetSegments' order) as the row stood when the context was built, ""
	// for a row with no parts. It is where the import route saved a split
	// row's chapter import (routes.splitSidecarPath: beside the first part,
	// whatever the row's output_file names), so a split re-finalize reads a
	// kept import there (writeSplitArchiveChapters) — read at the run's
	// start, so parts the run itself records under a name the template now
	// resolves afresh never count.
	RecordedFirstPart string
	// Run is the run's record of what holds across its strategies'
	// repeated setups (runRecord): a damaged pin's one Warn, a live capture's
	// path switches and the no-JS path it records on. Shared by pointer like
	// Interruption; nil-safe.
	Run *runRecord
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
	// PreferHDR is downloader.prefer_hdr, snapshotted at job start: the
	// dynamic-range rung of every YouTube selection this job makes — the
	// VOD selector, the live and DASH-manifest selector, the HLS variant
	// selector and the quality probe alike (youtube's rangeScore).
	PreferHDR bool
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
	// runChats is each running job's chat downloaders (runChatRegistry), the
	// same registry the orchestrator and the stream processor register into.
	runChats   *runChatRegistry
	streamProc *StreamProcessor
	notifier   notifications.Sender
	logger     logger
	// conn is the connectivity monitor (nil: always online), the one the
	// scheduler and the orchestrator hold. processJob waits an outage out
	// on it rather than failing a job whose fetch met one (outage_wait.go).
	conn      Connectivity
	wg        sync.WaitGroup // tracks in-flight processJob goroutines
	notifyJob chan struct{}  // signal to re-check for new jobs (non-blocking send)

	// stagingClaimMu guards stagingClaims, the per-job claim BOTH off-queue
	// verbs take before they start: MuxJob and RecoverAsides. Each writes into
	// one staging directory and one output directory, and each calls
	// muxStagedAsides with its OWN asideOutputPath `used` map — so two of them
	// running together both pick the plain <stem>.restart-<ts>.mp4 and write
	// over each other. The value is the operation holding it, so the refusal
	// can say which. The chapter import and the boot cleanup take it too, and
	// the reset verbs (Resume, Reinitialize, auto-reinitialize) hold it while
	// they reset and queue the job (afterJobExit), so it is the job's one
	// lease on its staging outside a queue run. Lazily allocated, so a
	// zero-value worker (the tests build several) needs no constructor change.
	// finishing marks the jobs whose finalize has produced its archive and is
	// recording it (beginFinishing): a Cancel then answers
	// ErrAlreadyFinishing rather than land between the archive and its
	// Finished write (owner ruling R-C1). finishMu serialises the mark with
	// CancelJob's status write, so either the Cancel lands first and the
	// mark finds it (and re-claims the row when the mux had completed, owner
	// ruling R-H2), or the mark does and the Cancel is refused. Lazily
	// allocated, like stagingClaims.
	finishMu  sync.Mutex
	finishing map[string]bool
	// muxRuns is the off-queue mux runs in flight (muxJob's goroutine: the
	// Mux action, a boot re-mux, the automatic Twitch mux), and for each
	// whether a Cancel that applied during it left its "Job Cancelled" to the
	// run (noteMuxRunCancel, endMuxRun). The queue knows none of these runs,
	// so it flags nothing for them. Lazily allocated, like finishing.
	muxRunMu sync.Mutex
	muxRuns  map[string]bool

	stagingClaimMu sync.Mutex
	stagingClaims  map[string]stagingClaim
	// stagingClaimGen numbers the claims taken, so a release frees only the
	// claim it was handed (stagingClaimRelease). Under stagingClaimMu.
	stagingClaimGen uint64
	// stagingClaimFreed holds, per job, a channel closed when that job's
	// claim is released — what awaitStagingClaim waits on. Created by the
	// first waiter, under stagingClaimMu; lazily allocated like the claims.
	stagingClaimFreed map[string]chan struct{}

	// afterExitPending holds, per job, the name of the verb an afterJobExit
	// is running or waiting to run, so the same verb again is a no-op and a
	// different one is refused with ErrResetPending rather than dropped.
	afterExitPending sync.Map
	// runCtx is Start's context, which ends at shutdown: what a deferred verb
	// stops waiting on (awaitJobExit). Nil until Start, and in tests that
	// never call it, where such a wait ends only with the run.
	runCtx atomic.Pointer[context.Context]

	// autoMuxPending holds the jobs AutoMuxEndedBroadcast is confirming, so a
	// monitor poll that lands while one is in flight does not start another;
	// autoMuxMu makes autoMuxNow's re-check and claim one step. See
	// twitch_end_unconfirmed.go.
	autoMuxPending sync.Map
	autoMuxMu      sync.Mutex
	// twitchLiveness overrides confirmTwitchStreamInfo for
	// AutoMuxEndedBroadcast's confirmation. nil in production; tests set it.
	twitchLiveness func(ctx context.Context, login string) (*twitch.TwitchStreamInfo, error)

	// OnCookieRefreshNeeded is called when auth fails and auto-refresh should
	// be attempted. Returns CookieRefreshRestored if THE NAMED PLATFORM ended
	// up authenticated, CookieRefreshSkipped if the refresh was skipped for a
	// reason the callback has logged, CookieRefreshUnconfirmed if it ran and
	// could not establish whether the platform's cookies work, CookieRefreshOff if
	// automatic refresh is turned off, and CookieRefreshNotRestored otherwise.
	//
	// The platform argument is not decoration. Without it the callback could
	// only answer "did any platform end up authenticated", so a healthy Twitch
	// told a YouTube job to retry — spending a probe attempt and a slot on a
	// request that had just conclusively failed, on every cycle.
	OnCookieRefreshNeeded func(platform string) CookieRefreshOutcome

	// CurrentCredentialIdentity returns an opaque fingerprint of the account
	// the platform's cookies currently belong to (cookies.CookieJar's
	// YouTubeIdentity), or "" when it cannot be determined. Recorded on a
	// membership park so the credential sweep can tell later whether the
	// account has actually changed. Optional: a nil slot simply records "",
	// which the sweep resolves permissively.
	CurrentCredentialIdentity func(platform string) string

	// backlogRetries counts each backlog job's consecutive runs that went
	// back to Queued (requeueBacklog). In memory, like the scheduler's holds:
	// a restart grants a fresh budget, which still ends a permanently broken
	// video in Error, and so do an operator's Cancel, Retry and Resume
	// (endBacklogStreak). Lazily allocated.
	backlogRetryMu sync.Mutex
	backlogRetries map[string]int
	// rateLimitSpells is when each job's spell of rate-limit refusals began,
	// which rateLimitCap is measured from; captcha is the session's captcha
	// spell, shared with streamProc (rate_limit.go).
	rateLimitSpells rateLimitSpells
	captcha         *captchaAlert

	// processStreamFn and refreshVodInfoFn replace streamProc.Process and
	// streamProc.RefreshVodInfo when set — a test seam, since youtube.Service
	// has none of its own. nil in production (processStream,
	// refreshVodInfo).
	processStreamFn  func(ctx context.Context, job *database.Job) (*StreamProcessResult, error)
	refreshVodInfoFn func(ctx context.Context, job *database.Job) (*youtube.VideoInfo, error)

	// diskSpace replaces disk.GetDiskSpace in readOutputDisk when set — a
	// test seam standing in a volume of any fullness. nil in production.
	diskSpace func(path string) (*disk.DiskSpace, error)

	// CookieFileInUse returns the cookie file the running services read and
	// write (the jar's path). cookies.cookie_file is restart-required, so after
	// a save without the restart the setting names a file nothing touches, and
	// advice to replace it pointed the operator at the wrong one. Optional: nil
	// or "" falls back to the setting.
	CookieFileInUse func() string
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

	// TrimService is the trim service both UIs use; the post-download trim
	// runs through it, so its one-trim-per-job slot, RunningTrims and Stop
	// cover that trim too. Without one, a post-download trim sends Trim
	// Failed instead of running.
	TrimService *TrimService
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
	var trims *TrimService
	if deps != nil {
		cs = deps.CipherSolver
		routedCs = deps.RoutedCipherSolver
		pp = deps.PotProvider
		tw = deps.TwitchService
		nm = deps.Notifier
		conn = deps.Conn
		trims = deps.TrimService
	}

	sp := NewStreamProcessor(yt, tw, cfg, db, logger)
	if nm != nil {
		sp.SetNotifier(nm)
	}
	if conn != nil {
		sp.SetIsOnline(conn.IsOnline)
	}

	sched := newScheduler(db, queue, logger)
	sched.conn = conn
	// Slot-release flips (spec §10) free an archive slot mid-flight — a
	// backlog job going Live or entering the upcoming wait stops counting in
	// M. The wake lets the scheduler admit the channel's next backlog VOD
	// promptly instead of on the heartbeat.
	sp.SetWakeScheduler(sched.Wake)
	// The waiting loop waits a rate limit out in place, so its refusals hold
	// the backlog from there (loopRateLimit).
	sp.SetOnRateLimited(func() { sched.holdForRateLimit(time.Now(), rateLimitBackoff) })
	// One captcha spell for the session, whichever job meets it: the
	// waiting loop's fetches and processJob's (captchaAlert).
	captcha := newCaptchaAlert(nm, logger)
	sp.captcha = captcha

	// ONE registry, two holders. ExecuteTwitch registers into the
	// orchestrator's; cmd/moombox broadcasts through the worker's. Two
	// registries would compile, pass every registry unit test, and leave the
	// broadcast reaching an always-empty map.
	twitchChats := newTwitchChatRegistry()
	orchestrator := NewDownloadOrchestrator(db, queue, cfg.Paths.FfmpegPath, logger, cs, routedCs, pp, nm, conn)
	orchestrator.twitchChats = twitchChats
	// One registry again, three holders: the stream processor and the
	// orchestrator register the chat downloaders a run builds, and the worker
	// stops them where a run ends without their own teardown (stopRunChats).
	runChats := newRunChatRegistry()
	orchestrator.runChats = runChats
	sp.runChats = runChats
	orchestrator.trims = trims

	w := &DownloadWorker{
		db:           db,
		yt:           yt,
		tw:           tw,
		cfg:          cfg,
		queue:        queue,
		scheduler:    sched,
		orchestrator: orchestrator,
		twitchChats:  twitchChats,
		runChats:     runChats,
		streamProc:   sp,
		notifier:     nm,
		logger:       logger,
		conn:         conn,
		notifyJob:    make(chan struct{}, 1),
		captcha:      captcha,
	}
	// The scheduler reads the disk through the worker, which holds the
	// config: the output directory and the critical threshold both follow a
	// Settings save without a restart.
	sched.readDisk = w.readOutputDisk
	// And skips a backlog row whose staging an off-queue operation holds (a
	// set-aside recovery accepts a Queued row): admitting it starts a run
	// that writes the staging FFmpeg is reading. The claim's release wakes
	// the scheduler (stagingClaimRelease).
	sched.stagingBusy = w.stagingClaimed
	sched.stagingClaimedCount = w.stagingClaimCount
	// A trim asks the worker whether a chapter import holds the job's
	// archive (ClaimChapterImport has the other half).
	if trims != nil {
		trims.setArchiveRewriting(w.chapterImportRunning)
	}
	// And a finalize waits for one (owner decision PR-M5): it writes the
	// archive the import's re-mux is about to rename its copy over.
	if orchestrator != nil {
		orchestrator.waitArchiveRewrite = w.waitChapterImport
		orchestrator.beginFinishing = w.beginFinishing
	}
	// A deleted row takes its backlog retry streak with it. The id is the
	// video id, so a backlog rescan can create the row again, and it would
	// have inherited the old count and hold. For the worker's lifetime,
	// which is the process's.
	//
	// Two subscriptions for the two ways a row goes: DeleteJob fires
	// OnJobDeleted, while a channel removal's "delete its pending jobs"
	// (DeleteJobsAndHistoryForChannel) deletes in bulk and fires only one
	// OnJobsChange, with the list it left behind (endStreaksGoneFrom). That
	// list is delivered off the writer's goroutine, so a third makes the
	// rule exact however the timing falls: a row AddJob inserts has had no
	// run to earn a streak, so whatever one its id still has is stale.
	if db != nil {
		db.OnJobDeleted(func(ev *database.JobDeleted) { w.endBacklogStreak(ev.JobID) })
		db.OnJobsChange(w.endStreaksGoneFrom)
		db.OnJobAdded(func(ev *database.JobAdded) { w.endBacklogStreak(ev.Job.ID) })
	}
	return w
}

// Start begins the worker loop, processing jobs from the queue.
func (w *DownloadWorker) Start(ctx context.Context) {
	w.runCtx.Store(&ctx)
	w.logger.Info("download worker started")

	// Incomplete-tail flags from before the end-of-stream margin
	// (clearToleratedTails), then the staging leftovers a previous run left
	// behind that are provably redundant (reclaimBootLeftovers). Off the queue's path: a slow delete
	// must not hold up a live capture waiting to start.
	w.wg.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				w.logger.Error("panic in the boot staging cleanup", "panic", fmt.Sprint(r))
			}
		}()
		// Before the leftovers sweep: a row this clears no longer shields
		// its staging.
		w.clearToleratedTails()
		w.reclaimBootLeftovers()
	})

	// Enqueue existing pending jobs
	w.enqueueExistingJobs()

	// Poll for new jobs periodically
	go w.pollForJobs(ctx)

	// Backlog admission (spec §10). The scheduler owns the only path out of
	// Queued — pollForJobs never touches those rows (ShouldProcess is false
	// for Queued by design) — so Run carries its own restart-on-panic wrapper.
	go w.scheduler.Run(ctx)

	// Twitch archive-VOD gap-fill (owner decision D17): the plans finished
	// live captures left in twitch_gap_fills, looked at on their backoff.
	go w.runTwitchGapFills(ctx, w.newTwitchGapFiller())

	// Process jobs from queue
	for {
		jobID, jobCtx, ok := w.queue.Dequeue(ctx)
		if !ok {
			return // Context cancelled
		}

		w.wg.Go(func() {
			defer func() {
				// processJob records its own panic (recordRunPanic), ahead
				// of the Complete that ends the run; this one is what
				// recording it panics into, and writes the guarded Error
				// alone — the run's flag and settled mark went with that
				// Complete.
				if r := recover(); r != nil {
					w.logger.Error("panic in processJob", "jobID", jobID, "panic", fmt.Sprint(r))
					w.writePanicError(jobID, r)
				}
			}()
			w.processJob(jobCtx, jobID)
		})
	}
}

// EnqueueJob adds a job to the processing queue.
// Looks up the job from DB to determine its priority.
//
// Not while the job's staging claim is held (a set-aside recovery, a Mux, a
// chapter import, a reset verb mid-reset): a Reinitialize writes its reset
// Upcoming before it deletes the staging (reinitializeNow), and a monitor
// that queued that row meanwhile started a run writing the directory being
// deleted. The claim's release wakes the heartbeat, which queues the job
// then (enqueueRunnable). A verb that holds the claim queues its own job
// through enqueueHeld.
func (w *DownloadWorker) EnqueueJob(jobID string) {
	if w.stagingClaimed(jobID) {
		w.logger.Debug("enqueue deferred: the job's staging is claimed; its release wakes the heartbeat", "jobID", jobID)
		return
	}
	w.enqueueHeld(jobID)
}

// enqueueHeld is EnqueueJob for the holder of the job's staging claim: the
// reset verbs queue the job they reset while they still hold it.
func (w *DownloadWorker) enqueueHeld(jobID string) {
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
//     scheduler never admits a priority-0 row, so Queued would strand it; so
//     would a priority-1 row with no channel, since the scheduler only sweeps
//     channels (QueuedChannels skips a NULL channel_id).
//
// A backlog row of a REMOVED channel resumes to Queued like any other. Its
// feed_items partner is gone — the departure prune deletes the channel's feed
// history, and the row was kept, either by the removal's default or as a
// download already running — and it used to resume to Upcoming for that,
// because NextQueuedJobs INNER-JOINed feed_items and would never have
// returned it. The join is a LEFT JOIN now (W25-09), and the scheduler admits
// such a row under the global archive_slots.
func CookieResumeStatus(job *database.Job) database.JobStatus {
	if job.QueuePriority != 1 || job.ChannelID == nil {
		return database.StatusUpcoming
	}
	return database.StatusQueued
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

// CancelJob is the operator's Cancel: it writes the job's status Cancelled and
// stops its run, if one is in flight. cancelled reports whether it did. When
// it did, flagged reports whether an actively-processing run was flagged —
// that run's handleCancellation emits the "cancelled" notification, so
// callers that notify should skip their own emission. A run that has already
// settled its outcome (a failure recorded, a backlog requeue, a run the write
// alone ended — JobQueue.settle) is not flagged: it no longer reads the flag,
// and the caller sends the notification itself.
//
// A Cancel is decided on a status read earlier — the row a UI showed, the
// cancel route's own read — and the write is never over a terminal one
// (UpdateJobFieldsIfIn the cancellable statuses). Written unconditionally, it turned a job
// that finished or failed in between into a Cancelled one: a Finished
// archive or an Error the operator had yet to read, gone. A Cancel that finds
// the job ended does nothing at all — no flag, no stopped run, no streak
// ended — and answers cancelled false, so its caller says so rather than
// claiming a cancel.
//
// The write comes first and the flag after, so it is the write that decides.
// The write alone ends a run (its listeners cancel the download); one that
// gets to WasCancelled before the flag is settled by it, and is the caller's
// to report.
//
// The write applies only while the job is cancellable
// (jobverbs.CancellableStatuses — every status short of an outcome). And not
// once its finalize has produced the archive (owner ruling R-C1): the job
// then ends Finished, and the Cancel answers ErrAlreadyFinishing, with
// nothing written or flagged. A Cancel that lands before that mark while
// the mux still runs stops it and stands; one that lands in the instants
// between the mux's return and the mark is applied and answered, and the
// mark then re-claims the row for the archive that is already written
// (beginFinishing, owner ruling R-H2).
//
// A Cancel applied while an off-queue mux of the job runs (muxJob) is
// flagged too, and announced by that run at its exit — only if the row still
// reads Cancelled then (endMuxRun). The queue does not know such a run, so
// the caller used to announce the Cancel at once, and a finish that
// re-claimed the row sent "Download Finished" after it. Noted under finishMu
// with the write, so the mark that re-claims the row always finds it noted.
func (w *DownloadWorker) CancelJob(jobID string) (cancelled, flagged bool, err error) {
	w.finishMu.Lock()
	if w.finishing[jobID] {
		w.finishMu.Unlock()
		return false, false, ErrAlreadyFinishing
	}
	// announce_found goes with it: a job cancelled before its first fetch is
	// owed no "Stream Found", and a later Retry must not announce a discovery
	// long past.
	applied := w.db.UpdateJobFieldsIfIn(jobID, jobverbs.CancellableStatuses(), map[string]any{
		"status":         database.StatusCancelled,
		"announce_found": false,
	})
	muxRunWillNotify := applied && w.noteMuxRunCancel(jobID)
	w.finishMu.Unlock()
	if !applied {
		return false, false, nil
	}
	flagged = w.queue.Cancel(jobID)
	w.endBacklogStreak(jobID)
	return true, flagged || muxRunWillNotify, nil
}

// beginMuxRun registers an off-queue mux run of the job, before its goroutine
// starts: a Cancel applied from here until endMuxRun is the run's to announce.
func (w *DownloadWorker) beginMuxRun(jobID string) {
	w.muxRunMu.Lock()
	defer w.muxRunMu.Unlock()
	if w.muxRuns == nil {
		w.muxRuns = map[string]bool{}
	}
	w.muxRuns[jobID] = false
}

// noteMuxRunCancel records that a Cancel applied while an off-queue mux of
// the job runs, and reports whether one does: its announcement is then that
// run's (endMuxRun), not the Cancel's caller's.
func (w *DownloadWorker) noteMuxRunCancel(jobID string) bool {
	w.muxRunMu.Lock()
	defer w.muxRunMu.Unlock()
	if _, running := w.muxRuns[jobID]; !running {
		return false
	}
	w.muxRuns[jobID] = true
	return true
}

// endMuxRun ends an off-queue mux run's registration, at its goroutine's
// exit, and sends the "Job Cancelled" a Cancel left to it — when the row
// still reads Cancelled. A Cancel the finishing mark superseded (owner ruling
// R-H2) reads Finished here, and its one notification is the finalize's
// "Download Finished". A Cancel that applies after this is its caller's to
// announce again.
func (w *DownloadWorker) endMuxRun(jobID string, prior *database.Job, finished bool) {
	w.muxRunMu.Lock()
	owed := w.muxRuns[jobID]
	delete(w.muxRuns, jobID)
	w.muxRunMu.Unlock()
	if !owed {
		return
	}
	job, err := w.db.GetJob(jobID)
	if err != nil {
		// CancelJob has told the Cancel's caller this run announces it, so
		// nobody else will, and a row that cannot be read is no reason to
		// send nothing. The run goes by what it knows: a mux that finished
		// recorded its archive and sent "Download Finished" (the Cancel was
		// superseded, or stopped nothing); one that did not left the Cancel
		// standing, announced from prior — the row the Mux read as it
		// started, nil when that read failed too.
		if finished || prior == nil {
			return
		}
		w.logger.Warn("could not read the row at the end of a cancelled mux; announcing the cancel from the row read at its start", "jobID", jobID, "err", err)
		job = prior
	} else if job == nil || job.Status != database.StatusCancelled {
		return
	}
	w.logger.Info("job cancelled by user", "jobID", jobID)
	if w.notifier != nil {
		w.notifier.Send(notifications.JobCancelled(NotifyFacts(job)))
	}
}

// beginFinishing marks a job's finalize as past the point of no return —
// its archive is written — and returns the release, or ok false when the
// finalize must stop: with ErrCancelled, its staging kept, and the row left
// as it is. Under finishMu with the read, so no Cancel can land between the
// check and the mark; a Cancel after it is refused (CancelJob).
//
// The row decides, read here:
//   - Muxing, the status every finalize writes before it muxes (or Live or
//     Downloading, which one whose Muxing write failed on the database still
//     reads): marked.
//   - Cancelled, with the mux complete (muxed): an operator's Cancel landed
//     between FFmpeg's return and this mark, and was acknowledged. Finished
//     wins (owner ruling R-H2): the archive is whole, and a row left
//     Cancelled beside it named no file while the file sat at the output
//     path on no row. The row is re-claimed for the finalize — Cancelled to
//     Muxing, conditional on its still reading Cancelled — and marked;
//     reclaimed is true, and the finalize runs its tail on a context the
//     Cancel did not end. The Cancel had answered success, so the flip is
//     the job update both UIs get, and the one Info line below names it.
//   - Cancelled, the mux not complete: the Cancel stopped the mux (a split
//     finalize's merge or set-aside recovery, whose failures are swallowed),
//     and stands.
//   - anything else — another outcome, a reset verb's Upcoming, a requeue,
//     a park, the row gone: the finalize stops and writes nothing over it.
//
// A row that cannot be read is not an outcome (as writeUnlessOutcome reads
// one): the finalize goes on.
func (w *DownloadWorker) beginFinishing(jobID string, muxed bool) (release func(), reclaimed, ok bool) {
	w.finishMu.Lock()
	defer w.finishMu.Unlock()
	if job, err := w.db.GetJob(jobID); err == nil {
		switch {
		case job == nil:
			return nil, false, false
		case job.Status == database.StatusMuxing, job.Status == database.StatusDownloading, job.Status == database.StatusLive:
		case job.Status == database.StatusCancelled && muxed:
			// Conditional, though finishMu keeps a Cancel out: a reset verb
			// takes no such lock, and one that claimed the Cancelled row
			// since the read keeps it.
			if !w.db.UpdateJobFieldsIf(jobID, database.StatusCancelled, map[string]any{
				"status": database.StatusMuxing,
			}) {
				return nil, false, false
			}
			reclaimed = true
			w.logger.Info("a Cancel landed after the mux completed and before it was recorded; the archive is written, so the job finishes (the Cancel is superseded)",
				"jobID", jobID)
		default:
			return nil, false, false
		}
	}
	if w.finishing == nil {
		w.finishing = map[string]bool{}
	}
	w.finishing[jobID] = true
	return func() {
		w.finishMu.Lock()
		delete(w.finishing, jobID)
		w.finishMu.Unlock()
	}, reclaimed, true
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

// bootEnqueueOrder is the order the boot pass offers its rows to the queue:
// captures that were running when the process stopped first, waiters last.
// The queue's backlog cap is checked before priority, so offered in row
// order a Live or Downloading row behind a hundred fresher Upcoming ones was
// dropped until the first heartbeat; in this order the cap can only ever
// delay an Upcoming waiter.
var bootEnqueueOrder = [...]database.JobStatus{
	database.StatusLive,
	database.StatusDownloading,
	database.StatusUpcoming,
}

// stillMuxingAfter applies fields to a row the boot pass read as Muxing, only
// while it still is (the read is one GetAllJobs for every row, and a re-mux
// of an earlier row can take a while; an operator's Cancel or Delete in
// between stands). It reports whether the row is still the boot pass's to
// handle. A write that did not apply is not by itself a no: the conditional
// writers also answer false when the statement fails, and nothing but the
// next boot picks a Muxing row up again, so the row is read once more and
// given up only when it is gone or no longer Muxing.
func (w *DownloadWorker) stillMuxingAfter(jobID string, fields map[string]any) bool {
	if w.db.UpdateJobFieldsIf(jobID, database.StatusMuxing, fields) {
		return true
	}
	cur, err := w.db.GetJob(jobID)
	if err != nil {
		return true
	}
	if cur == nil || cur.Status != database.StatusMuxing {
		status := database.JobStatus("")
		if cur != nil {
			status = cur.Status
		}
		w.logger.Debug("interrupted mux job changed since boot read it; leaving it", "jobID", jobID, "status", status)
		return false
	}
	return true
}

func (w *DownloadWorker) enqueueExistingJobs() {
	jobs, err := w.db.GetAllJobs()
	if err != nil {
		w.logger.Error("failed to get existing jobs", "err", err)
		return
	}

	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })

	var runnable []*database.Job
	for _, job := range jobs {
		if job.Status == database.StatusMuxing {
			if muxOnRestart(job, stagingBase) {
				// Owner decision O-B: mux what is staged. The previous reset
				// to Downloading re-probed the (now post-live) stream, routed
				// it to the manifest-free strategy whose dbResumeSeq seeds 0,
				// and truncated the complete recording (sweep-2 ENGINE-1).
				// The Mux action's path needs no network and no re-download.
				// A row cancelled or deleted since the read is left alone: the
				// re-mux accepts a Cancelled row as the Mux action does, and
				// would archive what the operator just cancelled.
				if !w.stillMuxingAfter(job.ID, map[string]any{"error": ""}) {
					continue
				}
				w.logger.Info("resuming interrupted mux from staged media", "jobID", job.ID)
				if err := w.muxJob(job.ID, muxActionFailure, true); err != nil {
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
			// reports/worker.md Finding 24). Only while the row is still
			// Muxing, like every write decided on an earlier status read.
			if !w.stillMuxingAfter(job.ID, map[string]any{
				"status": database.StatusDownloading,
				"error":  "",
			}) {
				continue
			}
			w.logger.Info("resetting interrupted mux job", "jobID", job.ID)
			job.Status = database.StatusDownloading
		}
		if ShouldProcess(job) {
			// A wait line left by a process that died mid-wait: its writer's
			// deferred clear never ran, and YouTube's waitForLive writes no
			// progress of its own, so an Upcoming row would show the frozen
			// wait (both UIs show an Upcoming row's wait line, owner ruling
			// R-D1) for the whole re-run wait. No run of this process has
			// started yet, so any wait line here is stale. Upcoming only: it is
			// the one re-enqueued status whose run writes no progress before
			// the wait ends; a Live or Downloading run overwrites its line
			// itself, and until then the line is the last thing it did.
			// Conditional on the status read, like every write decided on it.
			if job.Status == database.StatusUpcoming && jobstate.IsWaitLine(job.Progress) {
				w.db.UpdateJobFieldsIf(job.ID, job.Status, map[string]any{"progress": ""})
			}
			runnable = append(runnable, job)
		}
	}

	// Offered only now, after every interrupted mux above has been started
	// or reset, and still before Start's heartbeat and Dequeue loop begin.
	for _, status := range bootEnqueueOrder {
		for _, job := range runnable {
			if job.Status == status {
				w.queue.Enqueue(job.ID, job.Status)
			}
		}
	}
}

// pollForJobs is signal-driven: wakes on NotifyNewJob signals or a 60s safety heartbeat.
// Most job discovery happens via explicit EnqueueJob calls; this is a catch-all.
// Wraps the ticker loop in a restart-on-panic pattern so a single panic doesn't
// permanently kill the heartbeat poller.
// enqueueRunnable is the heartbeat's catch-all: every row ShouldProcess
// accepts and no run holds is queued. Not one whose staging claim is held: a
// set-aside recovery accepts a COOKIES? row, and the credential sweep's
// resume writes it Upcoming without the claim — queued here, it started a
// run writing the staging FFmpeg was muxing out of. A reset verb holding the
// claim queues the job itself. The claim's release wakes this loop
// (stagingClaimRelease), so a skipped row waits for the claim, not a tick.
func (w *DownloadWorker) enqueueRunnable(jobs []*database.Job) {
	for _, job := range jobs {
		if ShouldProcess(job) && !w.queue.IsProcessing(job.ID) && !w.stagingClaimed(job.ID) {
			w.queue.Enqueue(job.ID, job.Status)
		}
	}
}

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
				w.enqueueRunnable(jobs)
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
//
// A VOD that has to queue says so in its progress line for the length of the
// wait (vodSlotWaitLine, rewritten every vodSlotWaitRefresh with the time
// waited), and the line is cleared however the wait ends — left behind, a
// cancelled row would go on claiming it was queueing. The refresher has
// stopped before the clear is written, so no late rewrite outlives it. One
// that finds a slot free writes nothing.
func (w *DownloadWorker) acquireDownloadSlot(ctx context.Context, jobID string, isVod bool) bool {
	if !isVod {
		return true
	}
	if w.queue.TryAcquireDownloadSlot(jobID) {
		return true
	}
	if w.db == nil { // nil only in a zero-value test worker
		return w.queue.AcquireDownloadSlot(ctx, jobID)
	}
	since := time.Now()
	w.db.UpdateJobFields(jobID, map[string]any{"progress": vodSlotWaitLine(0)})
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		defer func() {
			if r := recover(); r != nil {
				w.logger.Error("download-slot wait refresher panicked", "jobID", jobID, "panic", fmt.Sprint(r))
			}
		}()
		t := time.NewTicker(vodSlotWaitRefresh)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				w.db.UpdateJobFields(jobID, map[string]any{"progress": vodSlotWaitLine(time.Since(since))})
			}
		}
	}()
	defer func() {
		close(stop)
		<-stopped
		w.db.UpdateJobFields(jobID, map[string]any{"progress": ""})
	}()
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
		// The run's chat entry ends with the run (runChatRegistry.release).
		w.runChats.release(jobID)
		w.queue.Complete(jobID)
		// Every job exit funnels through here — Finished, Error, Cancelled,
		// COOKIES? — and all of those drop out of the scheduler's M
		// allow-list, so a freed archive slot may exist. Wake the scheduler
		// to admit the channel's next backlog VOD now rather than on its
		// heartbeat. Coalesced + non-blocking; harmless when nothing freed.
		w.scheduler.Wake()
	}()
	// Deferred after the Complete above, so it runs before it: Complete
	// drops the run's user-cancel flag, and Start's recover — which used to
	// record a panic alone — found nothing left to say a Cancel had flagged
	// the run, so the Job Cancelled the cancel route left to it was never
	// sent. Its own defer, not a recover inside the one above: a panic in
	// the record goes on to Start's recover and still passes through the
	// Complete.
	var job *database.Job
	defer func() {
		if r := recover(); r != nil {
			// The chat first: the panic unwound past the orchestrator's own
			// teardown, and a downloader left running would write
			// "downloading" over the settle the record makes.
			w.stopRunChats(jobID)
			w.recordRunPanic(jobID, job, r)
		}
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

	// Stale check: the row must still be one the queue processes. Terminal
	// rows were the only ones skipped, but a heartbeat that read the row
	// before a run parked it in COOKIES? (or a sweep that sent it back to
	// Queued) can enqueue it once that run exits — and this run would then
	// probe, notify and refresh cookies for a parked row all over again, or
	// download a backlog VOD the scheduler never admitted.
	if !ShouldProcess(job) {
		if job.Status == database.StatusCancelled && w.queue.WasCancelled(jobID) {
			// Cancelled between Dequeue and here: queue.Cancel flagged this
			// run, so the cancel route left the notification to it.
			w.logger.Info("job cancelled by user", "jobID", jobID)
			if w.notifier != nil {
				w.notifier.Send(notifications.JobCancelled(NotifyFacts(job)))
			}
			return
		}
		w.logger.Debug("skipping job no longer in a processable state", "jobID", jobID, "status", job.Status)
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

	// One pass per attempt at the job. A pass after the first is a download
	// whose setup fetch met a connectivity outage, run again from stream
	// processing once the network is back (waitOutSetupOutage), or a job
	// YouTube rate-limited, run again once it has waited (outlastRateLimit).
	var setupOutageSince, rateLimitSince time.Time
	for {
		// What an earlier pass registered is not this pass's: stopped by its
		// own teardown already, and let go of here (runChatRegistry.nextPass).
		w.runChats.nextPass(jobID)
		// Process stream (probe, wait for live, etc.) — waiting a connectivity
		// outage out rather than failing on it (outage_wait.go).
		result, err := w.processStreamThroughOutages(ctx, job)
		// When the format URLs were extracted, near enough: for a VOD, Process
		// returns straight after its fetch. refreshStaleVodInfo measures the slot
		// waits below from here.
		extractedAt := time.Now()
		if err != nil {
			if ctx.Err() != nil {
				w.handleCancellation(job)
				return
			}
			if isRateLimited(err) {
				// An Innertube 429 on the whole cascade: the HTTP form of the
				// same limit, waited out as its verdict is (below).
				if job = w.outlastRateLimit(ctx, job, err, &rateLimitSince); job == nil {
					return
				}
				continue
			}
			requeued, err := w.requeueBacklogAfterTransientFailure(job, err)
			if !requeued {
				w.setJobError(job, err)
			}
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
			if errors.Is(result.ErrSentinel, ErrRateLimited) {
				// The session's refusal, not the video's: wait it out rather
				// than record it.
				if job = w.outlastRateLimit(ctx, job, result.AsError(), &rateLimitSince); job == nil {
					return
				}
				continue
			}
			if result.Error == "" {
				// Every producer of ShouldDownload:false names a reason today.
				// One that did not would leave the row in whatever non-terminal
				// state it had, with nothing driving it and nothing said; make
				// that visible instead.
				result.Error = "stream processing declined the job without a reason"
			}
			// AsError preserves any ErrSentinel attached by the producer (e.g.
			// ErrCookiesRequired from checkPlayability) so setJobError's
			// errors.Is checks fire correctly. Without this wrap, the prior
			// code's errors.New stripped the sentinel and forced setJobError
			// back to string-matching.
			w.setJobError(job, result.AsError())
			return
		}
		// An answer ends the spell of rate-limit refusals: one after it — the
		// stale re-extraction's — is a fresh limit (outlastRateLimit), whose
		// day is counted from it (noteAnswered).
		rateLimitSince = time.Time{}
		w.noteAnswered(job)

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

		// Both waits are over. A VOD's format URLs were extracted before them,
		// and a googlevideo URL lives ~6 h: one that queued longer than that
		// failed its first request with a 403 that nothing retried. Re-extract a
		// stale one now that the download is really starting.
		if err := w.refreshStaleVodInfoThroughOutages(ctx, job, result, extractedAt); err != nil {
			if ctx.Err() != nil || errors.Is(err, ErrCancelled) {
				w.handleCancellation(job)
				return
			}
			if isRateLimited(err) {
				// The re-extraction was rate-limited — its verdict, or a 429 —
				// and the URLs it would have replaced have expired: as from
				// Process.
				if job = w.outlastRateLimit(ctx, job, err, &rateLimitSince); job == nil {
					return
				}
				continue
			}
			requeued, err := w.requeueBacklogAfterTransientFailure(job, err)
			if !requeued {
				w.setJobError(job, err)
			}
			return
		}

		// Build job context
		jobCtx := w.runJobContext(job)

		// Route to platform-specific orchestrator
		var dlErr error
		if job.Platform == "twitch" && result.TwitchClip != nil {
			// A clip: one whole MP4, no variant, no chat (owner decision D26).
			dlErr = w.orchestrator.ExecuteTwitchClip(ctx, jobCtx, result.TwitchClip)
		} else if job.Platform == "twitch" && result.TwitchVariant != nil {
			variant := newTwitchVariantInfo(job, result.TwitchVariant, jobCtx.Config)
			// Stable broadcast identity for engine resume validation: the live
			// stream ID when known, else the job's video/VOD ID.
			if result.TwitchStreamInfo != nil && result.TwitchStreamInfo.StreamID != "" {
				variant.StreamID = result.TwitchStreamInfo.StreamID
			} else {
				variant.StreamID = job.VideoID
			}
			// For live streams, provide a stream-end check function and quality probe
			variant.VodChapters = result.TwitchVodChapters
			if !result.IsVod && result.TwitchStreamInfo != nil && w.tw != nil {
				login := result.TwitchStreamInfo.ChannelLogin
				startedAt := result.TwitchStreamInfo.StartedAt
				variant.CheckStreamFn = func(innerCtx context.Context) (bool, error) {
					info, err := w.confirmTwitchStreamInfo(innerCtx, login)
					if err != nil {
						return false, err
					}
					return info != nil && info.IsLive, nil
				}
				variant.CategoryFn = func(innerCtx context.Context) (string, error) {
					info, err := w.tw.GetStreamInfo(innerCtx, login)
					if err != nil || info == nil || !info.IsLive {
						return "", err
					}
					return info.GameCategory, nil
				}
				variant.ChannelLogin = login
				w.readConfig(func(c *config.MoomboxConfig) {
					variant.GapFillAdBreaks = twitchChannelGapFillAdBreaks(c.Channels, login)
				})
				variant.ArchiveChaptersFn = func(innerCtx context.Context) []utils.Chapter {
					return w.twitchArchiveChapters(innerCtx, login, startedAt)
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
			if result.IsVod && w.tw != nil {
				wireTwitchVodInfo(variant, job, result, w.tw.GetVodInfo)
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
			// ErrCancelled: the orchestrator's first status write found an
			// outcome on the row — an operator's Cancel that landed before
			// it, or the row gone — in ExecuteWithChat or ExecuteTwitch alike.
			// That early return used to be nil, which this read as a finished
			// download — it deleted the staging of a job the operator had just
			// cancelled and might Mux or Resume, and skipped the
			// cancellation's own handling.
			if ctx.Err() != nil || errors.Is(dlErr, ErrCancelled) {
				w.handleCancellation(job)
				return
			}
			// A download whose setup fetch met a connectivity outage waits for
			// the network and runs the job again, the row keeping its status
			// (waitOutSetupOutage), rather than ending in Error over a fetch
			// the outage failed.
			if fresh, wait := w.waitOutSetupOutage(ctx, job, result, dlErr, &setupOutageSince); wait {
				if fresh == nil {
					w.handleCancellation(job)
					return
				}
				job = fresh
				continue
			}
			// A backlog VOD that ran out of disk waits in Queued for the space
			// instead of ending in Error (requeueBacklogAfterDiskFull).
			requeued, err := w.requeueBacklogAfterDiskFull(job, dlErr)
			if !requeued {
				w.setJobError(job, err)
			}
			return
		}

		w.forgetBacklogRetries(job.ID)
		w.cleanupStagingAfterMux(job.ID, jobCtx.StagingDir)
		return
	}
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
	// The job's copies are done: a repaired copy one of them could not
	// remove is tried again here, before staging a mux-gap record keeps is
	// kept with it.
	w.removeStaleRepairedCopies(jobID, stagingDir)
	fresh, _ := w.db.GetJob(jobID)
	v := decideStagingCleanup(w.db, fresh, jobID, stagingDir)
	switch v.keep {
	case keepStagingForAsides:
		// A recording the engine could not resume was set aside rather than
		// truncated (engine.StagedRestartSuffix), and nothing in the mux
		// pipeline has consumed it: the fresh capture that replaced it is not
		// guaranteed to be as long (post-live segments are not always
		// re-servable), so deleting it here would destroy the longer copy on
		// the strength of a shorter one finishing cleanly. Named in recording
		// order so an operator muxing them by hand knows which came first.
		w.logger.Warn("preserving staging dir: a set-aside recording was never merged into the archive; these are in recording order, oldest first",
			"asides", strings.Join(v.asides, " | "), "path", stagingDir, "jobID", jobID)
	case keepStagingForUnmuxedPart:
		w.logger.Warn("preserving staging dir: a captured part is still unmuxed after finalize; recover via the Mux action",
			"path", stagingDir, "jobID", jobID)
	case keepStagingForMuxGaps:
		// A staged recording was muxed from a repaired copy that left its
		// damaged bytes out, or could not be checked against its own
		// fragments (copyStaged, mux_repair.go): what this dir holds beyond
		// the file it was muxed into is in no output at all. That file is
		// the archive, a part, or a sibling beside them — a set-aside
		// recording's, a superseded part's — and the record's reason names
		// it. One line per recording.
		for _, g := range v.gaps {
			w.logger.Warn("preserving staging dir: a mux could not carry a staged recording whole, and staging keeps it",
				"recording", g.path, "why", g.rec.reason(), "path", stagingDir, "jobID", jobID)
		}
	case keepStagingForUnusedRoot:
		// The staging root holds a recording the finalize did not use: the
		// from-the-start download beside a job that finalized as parts (one
		// that did not complete, or one from before the root was claimed for
		// it), or a second recording beside the one a single-file finalize
		// muxed. Either can be the longer copy; deleting it here is how a
		// complete VOD download used to vanish (vod_supersede.go).
		w.logger.Warn("preserving staging dir: the staging root holds a recording the finalize did not use",
			"recording", v.unusedRoot, "path", stagingDir, "jobID", jobID)
	case keepStagingForTail:
		w.logger.Warn("preserving staging dir: recording tail incomplete; Resume will append the missing segments from the sidecar",
			"path", stagingDir, "jobID", jobID)
	case keepStagingChatOnly:
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
	default:
		if err := os.RemoveAll(stagingDir); err != nil {
			w.logger.Warn("failed to remove staging directory", "path", stagingDir, "err", err)
		} else {
			w.logger.Debug("removed staging directory", "path", stagingDir)
		}
	}
}

// stagingKeep is what decideStagingCleanup concluded about a finalized job's
// staging directory: delete it (removeStaging), keep all of it for one of five
// reasons, or prune it down to the chat capture.
type stagingKeep int

const (
	removeStaging stagingKeep = iota
	keepStagingForAsides
	keepStagingForUnmuxedPart
	keepStagingForUnusedRoot
	keepStagingForMuxGaps
	keepStagingForTail
	keepStagingChatOnly
)

// expires reports whether the orphan sweep's age rule
// (incompleteStagingExpired) lifts this keep: only the tail and chat keeps
// expire; the four media keeps hold captured media that exists nowhere else
// and never do (jobNeedsStaging).
func (k stagingKeep) expires() bool {
	switch k {
	case keepStagingForTail, keepStagingChatOnly:
		return true
	case removeStaging, keepStagingForAsides, keepStagingForUnmuxedPart,
		keepStagingForUnusedRoot, keepStagingForMuxGaps:
		return false
	}
	return false
}

// stagingVerdict is decideStagingCleanup's answer, with what the Warn that
// keeps the directory names: the set-aside recordings, the root recording the
// finalize did not use, or the recordings a mux could not carry whole.
type stagingVerdict struct {
	keep       stagingKeep
	asides     []string
	unusedRoot string
	gaps       []muxGapNote
}

// decideStagingCleanup is cleanupStagingAfterMux's decision, without the
// acting on it — split out so the boot sweep (reclaimBootLeftovers) deletes a
// finalized job's staging on exactly the rules this function would have, and
// never on a looser copy of them. The shields are checked in the order the
// cleanup has always checked them, so the Warn an operator reads names the
// first that holds. fresh may be nil (the row read failed or the row is
// gone): the tail and chat shields then do not apply, as they never did.
// The orphan sweep derives its shield from this verdict (jobNeedsStaging),
// lifting the expiring keeps on its age rule — so those two must stay LAST
// here: a keep added after them could be hidden behind an expired one.
func decideStagingCleanup(db *database.Database, fresh *database.Job, jobID, stagingDir string) stagingVerdict {
	if asides := stagedAsideRecordings(stagingDir); len(asides) > 0 {
		return stagingVerdict{keep: keepStagingForAsides, asides: asides}
	}
	if hasUnmuxedPartsForJob(db, jobID, stagingDir) {
		return stagingVerdict{keep: keepStagingForUnmuxedPart}
	}
	if unused := unusedRootRecording(db, jobID, stagingDir); unused != "" {
		return stagingVerdict{keep: keepStagingForUnusedRoot, unusedRoot: unused}
	}
	// A recording a mux could not carry whole (stagedMuxGaps): its damaged
	// bytes were left out of a repaired copy, or nothing could check the copy
	// against its fragments. Ahead of the tail and chat keeps, whose prune
	// would take it, and on no row field at all — the record beside the
	// recording is the whole of it, so the boot sweep and the orphan sweep
	// read the same answer (W30-A11).
	if gaps := stagedMuxGaps(stagingDir); len(gaps) > 0 {
		return stagingVerdict{keep: keepStagingForMuxGaps, gaps: gaps}
	}
	if fresh != nil && fresh.IncompleteTail {
		return stagingVerdict{keep: keepStagingForTail}
	}
	// A chat capture that ended without completing leaves its resume
	// sidecar in staging; deleting the dir turns a recoverable truncation
	// into a permanent one (sweep-2 TWITCH-3, verifier merge M5). Same
	// shape as the incomplete_tail preservation above it, and the orphan
	// scanner mirrors it in jobNeedsStaging — but only the chat files are
	// kept, not the muxed-away media (see keepOnlyChatCapture).
	if fresh != nil && fresh.ChatStatus == database.ChatIncomplete {
		return stagingVerdict{keep: keepStagingChatOnly}
	}
	return stagingVerdict{keep: removeStaging}
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
// internal/twitch/chat_recording.go) — and the YouTube chat's raw sidecar,
// chat.raw.jsonl (internal/chat/markers.go), every action that did not
// become a message: pruned, the chat's re-run started it over with only its
// own lines and copyAssets copied that over the archive's, losing every
// marker and action the first run kept. Media resume sidecars are NOT
// matched: they are named after their media file (video.ts.resume.json),
// which this prune is deleting because it has already been muxed.
func isChatCaptureFile(name string) bool {
	return name == "chat.json" || strings.HasPrefix(name, "chat.json.") ||
		name == "chat.raw.jsonl" || strings.HasPrefix(name, "chat.raw.jsonl.")
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
	// The orphan sweep reaches this function too, through
	// decideStagingCleanup's verdict (internal/worker/orphans.go,
	// jobNeedsStaging) — after that verdict's own aside check, which answers
	// first. The sweep lifts only the expiring keeps (stagingKeep.expires:
	// tail and chat); the aside and unmuxed-part keeps never expire, because
	// finalize muxes every READABLE aside into its own sibling file, so one
	// still in staging is one FFmpeg could not read: the only copy of footage
	// that exists nowhere else, not something that goes stale after a week.
	if len(stagedAsideRecordings(stagingDir)) > 0 {
		return true
	}
	return hasUnmuxedSegmentParts(db, jobID, stagingDir)
}

// hasUnmuxedSegmentParts is hasUnmuxedPartsForJob without the aside term: a
// quality/gap-split part whose staging dir still holds recognized media that
// has no segment row. Split out for the orphan scanner (sweep-2 Task 11,
// extra item b), which now reads decideStagingCleanup's verdict instead (K17);
// its callers are hasUnmuxedPartsForJob and HasUnmuxedParts (staging.go),
// the predicate that offers the Mux verb, which leaves asides to their own
// verb.
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
	if rootIsPartZero(stagingDir) && !recorded[0] && discoverStagingMedia(stagingDir) != nil {
		return true // root is part 0 and it was never recorded
	}
	for _, sd := range segDirs {
		if !recorded[sd.idx] && discoverStagingMedia(sd.dir) != nil {
			return true
		}
	}
	return false
}

// recordRunPanic records a processJob run that panicked, from processJob's own
// recover — ahead of its deferred Complete, which drops the run's user-cancel
// flag. job is the row the run read, nil when it panicked before reading one.
//
// Settled the way setJobError settles a failure (JobQueue.settle). A Cancel
// that flagged the run first left its Job Cancelled to the run, so the run
// ends as a cancelled one and sends it (handleCancellation) — unless the
// run's own outcome landed over the Cancel: a Finished archive whose tail
// panicked stands, as below. Otherwise the run is settled now, and a Cancel
// from here on is its caller's to report.
//
// A flagged run whose row is gone ends cancelled too, as setJobError ends it:
// a Cancelled row is deleted at once (the delete route waits on no run for
// one), and a run unwinding from that Cancel that then panicked sent nothing
// — its Job Cancelled was still the run's to send, and an edit-mode message
// stayed at its last state. Only a run that panicked before it read its row,
// which is gone, has nothing to send it for, as processJob ends a row that
// vanished before its run.
//
// The panic's Error is not written over an outcome: the run may have finished
// or failed before it panicked, or the operator cancelled it while it ran —
// written unconditionally, that Cancelled came back as "internal panic".
// processJob runs no row that is already terminal, so whichever one it holds
// landed during this run and stands. Nor over an outcome the run recorded and
// settled before it panicked, terminal or not (JobQueue.settleRun): a COOKIES?
// park, which a credential repair resumes; a backlog VOD requeued to Queued;
// the Upcoming a successful automatic cookie refresh resumed it to; the row a
// shutdown leaves to resume on restart. Guarded by the terminal statuses
// alone, a panic in what was left of the run — setJobError's notification,
// the cookie refresh that can hold it for minutes — turned each of them into
// "internal panic", which only a manual Retry undid.
func (w *DownloadWorker) recordRunPanic(jobID string, job *database.Job, r any) {
	w.logger.Error("panic in processJob", "jobID", jobID, "panic", fmt.Sprint(r))
	flagged, already := w.queue.settleRun(jobID)
	if already {
		w.logger.Info("panic after the run recorded its outcome; leaving it as is", "jobID", jobID)
		return
	}
	if flagged {
		row, err := w.db.GetJob(jobID)
		if err == nil && row != nil && row.Status == database.StatusMuxing {
			// A flagged run's row reads Muxing only when its finish
			// re-claimed it from the Cancel that flagged the run
			// (beginFinishing, owner ruling R-H2): CancelJob wrote Cancelled
			// ahead of the flag, and a Mux is refused while this run is
			// registered. The job is not cancelled, so this is no cancel's
			// end — handleCancellation's Cancelled write was refused there,
			// "Job Cancelled" was sent, and the row stayed Muxing with
			// nothing running. The finalize panicked between its mark and
			// its Finished write: recorded as any other panicked finalize,
			// an Error that Mux recovers.
			w.logger.Info("panic in a finish that had re-claimed its row from a Cancel; recording the panic, not the cancel", "jobID", jobID)
			w.writePanicError(jobID, r)
			return
		}
		if err == nil && (row == nil || row.Status == database.StatusCancelled || !row.IsTerminal()) {
			if job == nil {
				job = row
			}
			if job != nil {
				w.handleCancellation(job)
				return
			}
		}
	}
	w.writePanicError(jobID, r)
}

// writePanicError writes a panicked run's Error, guarded by the terminal
// statuses, and settles the chat it leaves in flight as setJobError's Error
// does (owner rulings R-E1, R-E1a, R-H1): recordRunPanic and Start's recover
// both write through it, so a run that panics mid-capture does not leave
// "downloading" on its Error row. The settle
// stops no chat downloader itself: processJob's recover stops the run's
// (stopRunChats) before the record that comes here, since the orchestrator's
// cleanup is an explicit call a panic unwinds past, and a downloader left
// running wrote its own value (OnStart's "downloading") over the settle.
// Start's recover reaches this with no stop: it catches only a panic in
// processJob's own deferred handlers, and by then processJob has released
// the run's entry (runChatRegistry.release), which closes the "downloading"
// gate — nothing can write over the settle, though a downloader may be left
// running in that double fault.
func (w *DownloadWorker) writePanicError(jobID string, r any) {
	if !w.db.UpdateJobFieldsUnlessTerminal(jobID, map[string]any{
		"status": database.StatusError,
		"error":  fmt.Sprintf("internal panic: %v", r),
	}) {
		return
	}
	w.settleErrorChat(jobID)
}

// stopRunChats stops the chat downloaders the job's current run built
// (runChatRegistry.stop) and closes their "downloading" write, for the two
// exits that end a run without an orchestrator's chat teardown having
// necessarily run: a panic (processJob's recover — the teardown is explicit
// calls a panic unwinds past) and a failure (setJobError). Ahead of the
// Error write and settleErrorChat, so the settle is the last word. Guarded:
// it runs inside a recover, and a downloader's Stop that panicked there
// would lose the panic's own record.
func (w *DownloadWorker) stopRunChats(jobID string) {
	defer func() {
		if r := recover(); r != nil {
			w.logger.Error("panic stopping a run's chat downloaders", "jobID", jobID, "panic", fmt.Sprint(r))
		}
	}()
	if n := w.runChats.stop(jobID); n > 0 {
		w.logger.Debug("stopped the run's chat downloaders", "jobID", jobID, "count", n)
	}
}

// handleCancellation handles a cancelled/shutdown job.
// User-initiated cancels update status to Cancelled.
// Shutdown cancels preserve original status so jobs resume on restart (matches TS).
func (w *DownloadWorker) handleCancellation(job *database.Job) {
	// Consume the user-cancel flag BEFORE Complete — Complete clears any
	// leftover flag as part of slot cleanup. Reading the flag is lock-only
	// (no DB write), so the free-slot-before-DB-writes ordering below holds.
	userCancelled := w.queue.WasCancelled(job.ID)

	// Written while the run is still registered: a Retry/Resume/Reinitialize
	// clicked while this run unwound waits for its Done (afterJobExit), which
	// only processJob's deferred Complete closes, and a Mux is refused until
	// then (muxJob). And on a row that still reads Cancelled only: CancelJob
	// wrote that status before it flagged this run, and every write the run
	// makes after it is conditional, so an unconditional one here re-asserted
	// nothing and could only land on another actor's write — a Finished
	// archive a Mux of the Cancelled row made while this run unwound.
	//
	// The chat is settled on the row's Cancel whether or not the flag is
	// here. CancelJob's row write runs its subscribers ahead of queue.Cancel,
	// and a run can stop on that write alone — its conditional go-live or
	// Downloading write refused — and reach this point with no flag; the
	// Cancel's caller reports it then (settle), and the row kept the
	// "pending" or "downloading" both UIs render verbatim. Without the flag
	// the write is made only when there is a chat to settle: a shutdown's
	// row keeps its state, and a deleted row has none.
	var updates map[string]any
	if userCancelled {
		updates = map[string]any{"status": database.StatusCancelled}
	}
	if fresh, err := w.db.GetJob(job.ID); err == nil && fresh != nil && (userCancelled || fresh.Status == database.StatusCancelled) {
		if cs, ok := stoppedChatStatus(fresh.ChatStatus); ok {
			if updates == nil {
				updates = map[string]any{}
			}
			updates["chat_status"] = cs
		}
	}
	if updates != nil {
		w.db.UpdateJobFieldsIf(job.ID, database.StatusCancelled, updates)
	}

	// Free the slots — before the notification, symmetric with setJobError.
	// The run itself ends at processJob's deferred Complete.
	w.queue.ReleaseSlots(job.ID)
	w.forgetBacklogRetries(job.ID) // nor does this run end back in Queued

	if userCancelled {
		w.logger.Info("job cancelled by user", "jobID", job.ID)
		if w.notifier != nil {
			w.notifier.Send(notifications.JobCancelled(NotifyFacts(job)))
		}
	} else {
		// Shutdown: the existing status stays so the job resumes on restart.
		// Or the row was deleted (processJob's OnJobDeleted listener cancels
		// the same ctx), and there is nothing left to write. Or the row's
		// Cancel stopped the run ahead of its flag, and only its chat was
		// settled above.
		w.logger.Info("job interrupted (shutdown, row deleted, or a Cancel not yet flagged), leaving its state as is", "jobID", job.ID)
	}
}

// stoppedChatStatus is the one rule for settling a chat capture its run
// stopped without recording a verdict, written once for both settles: a user
// cancel (handleCancellation) and an Error on either platform
// (settleErrorChat, owner rulings R-E1a, which made the Error settle match
// this split, and R-H1, which extended it to Twitch). Neither
// orchestrator's cancel or error arm records a chat verdict — it stops the
// downloader and returns, and on a shutdown that is right, since the row keeps
// its state and the capture resumes on the next start. A user cancel and an
// Error are where the run ends, though, and both UIs render the value
// verbatim, so the row went on showing its chat as "downloading" (or
// "pending") indefinitely. A capture that was running stopped short —
// "incomplete", the same verdict a cut-off capture gets everywhere else; one
// that never started has nothing to report (""). ok is true exactly for an
// in-flight value (database.ChatStatus.InFlight); a settled verdict is left
// as it is (ok false).
func stoppedChatStatus(current database.ChatStatus) (database.ChatStatus, bool) {
	switch current {
	case database.ChatDownloading:
		return database.ChatIncomplete, true
	case database.ChatPending:
		return database.ChatNone, true
	}
	return database.ChatNone, false
}

// runJobContext is the JobContext processJob runs a job's download with:
// buildJobContext's, marked a re-finalize (JobContext.Refinalize) when the
// run continues the recording the row already holds (continuesRecording),
// so its finalize keeps a chapter import as MuxJob's does (owner decision
// CK-1).
func (w *DownloadWorker) runJobContext(job *database.Job) *JobContext {
	jobCtx := w.buildJobContext(job)
	jobCtx.Refinalize = w.continuesRecording(job)
	return jobCtx
}

// continuesRecording reports whether a download run of job, as read at the
// run's start, continues the recording the row already holds (owner decision
// CK-1): the row names an archive (output_file, which only a finalize writes)
// or has recorded parts (segment rows). That is every run that is not a new
// recording — a Resume, which keeps both, and every automatic re-run that
// leaves the row as it was: the monitor's auto-resume (ResumeJob), a parked
// COOKIES? row re-admitted once its cookies are repaired, a backlog VOD
// requeued after a transient failure, a boot resume of an interrupted row.
// A first download of a fresh row has neither, and a Reinitialize clears
// both (reinitializeNow; clearJobParts), so neither is ever a re-finalize.
// An auto-reinitialize keeps the parts of the same still-live broadcast and
// continues at the next one (AutoReinitializeJob), and continues the
// recording too.
//
// Keyed on the row rather than on the verb that started the run, so a
// Resume that a restart interrupted, and resumed from its Downloading row
// at boot, still continues the recording. Which import a re-finalize may
// keep is keptImportFor's callers' to say — only one beside the archive the
// row names, never one at a name a new run resolved to. A segment read that
// fails answers false: the finalize writes its own chapters, as before CK-1.
func (w *DownloadWorker) continuesRecording(job *database.Job) bool {
	if job == nil {
		return false
	}
	if job.OutputFile != "" {
		return true
	}
	segs, err := w.db.GetSegments(job.ID)
	return err == nil && len(segs) > 0
}

func (w *DownloadWorker) buildJobContext(job *database.Job) *JobContext {
	// Snapshot all config fields under lock
	var (
		cfgOutputDir, cfgStagingDir, cfgTemplate string
		cfgMaxRes, cfgMaxTimeout, cfgSegWorkers  int
		cfgProgressMS                            int
		cfgPrefer60, cfgChat, cfgPreferHDR       bool
		cfgInterruptionTimeout                   config.FlexDuration
		channelOutputDir                         string
	)
	w.readConfig(func(c *config.MoomboxConfig) {
		channelOutputDir = channelOutputOverride(c.Channels, job)
		cfgOutputDir = c.Paths.OutputDirectory
		cfgStagingDir = c.Paths.EffectiveStagingDir()
		cfgTemplate = c.Downloader.OutputTemplate
		cfgMaxRes = c.Downloader.MaxVideoResolution
		cfgPrefer60 = c.Downloader.Prefer60fps
		cfgPreferHDR = c.Downloader.PreferHDR
		cfgChat = c.Downloader.DownloadChat
		cfgMaxTimeout = c.Downloader.MaximumTimeout
		cfgSegWorkers = c.Downloader.SegmentWorkers
		cfgProgressMS = c.Downloader.ProgressIntervalMS
		cfgInterruptionTimeout = c.Downloader.InterruptionTimeout
	})

	var firstPart string
	hasParts := false
	if w.db != nil {
		if segs, err := w.db.GetSegments(job.ID); err != nil {
			hasParts = true // unknown: the row's directory stays as it is
		} else if len(segs) > 0 {
			firstPart = segs[0].FilePath
			hasParts = true
		}
	}

	// A monitor-detected row that has not started takes the directory the
	// settings name now, and keeps it from here on (owner ruling R2-X1,
	// output_dir.go).
	w.settleOutputDirectory(job, hasParts, cfgOutputDir, channelOutputDir)

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
			PreferHDR:           cfgPreferHDR,
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
		Run:          &runRecord{},

		RecordedFirstPart: firstPart,
	}
}

// fetchURL is a helper to download a URL's body.
//
// Its errors go through redact.MediaError. The live DASH and HLS strategies
// fetch their manifest here — a signed googlevideo URL carrying the client's
// public IP and the URL's signature, with the GVS PO token appended as a
// /pot/<token> path segment — and a transport failure, or a URL net/url
// refuses, is a *url.Error quoting the whole URL: returned as it was, all of
// it reached the "job error" log line, the job's stored error and the Job
// Failed embed.
func fetchURL(ctx context.Context, url string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, redact.MediaError(err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := workerHTTPClient.Do(req)
	if err != nil {
		return nil, 0, redact.MediaError(err)
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
// the process exits before Stop reaches CancelMuxes (owner decision O-E).
// FFmpeg no longer outlives that exit to write into a staging dir the
// restarted child re-muxes — it dies with the process (internal/childproc)
// and only ever wrote its own temporary file (engine.Muxer.writeOutput) —
// but a mux ended that way is killed mid-write instead of cancelled.
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
		// always draining a mux. Exiting now would end them by killing FFmpeg
		// with the process (internal/childproc); cancel the mux root so they
		// end here instead, each removing its temporary file. Their rows stay
		// Muxing and are re-muxed on restart.
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
