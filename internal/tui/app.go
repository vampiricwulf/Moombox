// Package tui provides the terminal user interface for Moombox using BubbleTea.
package tui

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// FocusPanel identifies which panel is focused.
type FocusPanel int

const (
	PanelTasks FocusPanel = iota
	PanelDetails
	PanelLogs
)

// Message types for async updates.
type (
	// JobUpdateMsg carries a single-job update, including the list of
	// columns that were actually written. Subscribers gate expensive
	// re-renders on Change.Changes (DECISIONS #21 / audit tui.md F20).
	JobUpdateMsg struct{ Change *database.JobChange }
	// JobAddedMsg carries a single new job from AddJob. DECISIONS #21
	// lifecycle event — the TUI handler appends instead of clearing +
	// rebuilding the whole task list (the legacy OnJobsChange path).
	JobAddedMsg struct{ Added *database.JobAdded }
	// JobDeletedMsg carries the ID of a removed job. DECISIONS #21
	// lifecycle event — the TUI handler removes the row from local
	// state instead of re-loading a fresh full-list snapshot.
	JobDeletedMsg struct{ Deleted *database.JobDeleted }
	// TrimsChangedMsg carries a refreshed Job snapshot whose Trims
	// field reflects the post-AddTrim/DeleteTrim state. The forwarder
	// in tui_wiring re-fetches via db.GetJob so the handler can apply
	// the new trim list to the cached row + (if selected) the detail
	// panel. DECISIONS #21 lifecycle event.
	TrimsChangedMsg struct{ Job *database.Job }
	JobsUpdateMsg   struct{ Jobs []*database.Job }
	LogBatchMsg     struct{ Lines []string }
	CheckTimersMsg  struct {
		NextFeedCheck   time.Time
		NextDecapiCheck time.Time
		NextTwitchCheck time.Time
	}
	CookieStatusMsg struct {
		YT       CookieStatus
		TW       CookieStatus
		YTActive bool
		TWActive bool
	}
	DiskStatusMsg struct {
		Free    uint64
		UsedPct float64
		Warn    string // "ok", "warn", "critical"
	}
	// BackfillStatusMsg mirrors the backfill_status WebSocket payload (spec
	// §11 progress surfacing): one message per completed scan page (state
	// "scanning") and one per scan-state change ("done", "error", "idle" —
	// those carry Tab "" and Pages 0).
	BackfillStatusMsg struct {
		Channel string // channel ID
		Tab     string // "videos", "streams", "membership"
		Pages   int    // pages completed for Tab this scan session
		State   string // "scanning", "done", "error", "idle"
	}
	UpdateStatusMsg struct {
		Version      string
		TagName      string
		ReleaseNotes string
	}
	channelClosedMsg struct{ Name string }
	tickMsg          struct{}
	// progressTickMsg carries the generation of the schedule that produced
	// it: a cadence upshift (500ms→16ms) supersedes the in-flight schedule
	// with a fresh one, and the stale tick is dropped on arrival by its
	// old generation instead of waiting out its interval.
	progressTickMsg struct{ gen int }
	logFlushMsg     struct{} // trailing log-batch flush (see logFlushInterval)
	marqueeTickMsg  struct{} // 150ms marquee scroll tick

	// testNotificationResultMsg reports the outcome of an async
	// test-notification send dispatched from the settings overlay.
	testNotificationResultMsg struct{ Err string }

	// Async results for update check/apply
	updateCheckResultMsg struct {
		Info *UpdateStatusMsg // nil = up to date
		Err  string
	}
	updateApplyResultMsg struct {
		Err string // empty on success (process exits before this is seen)
	}
	signatureVerifyResultMsg struct {
		Err string // empty on success
	}
	// releaseNotesFetchedMsg is the async result of OnFetchReleaseNotes,
	// dispatched by the R N chord when no update is pending. Err empty
	// means success; Tag + Notes are populated on success.
	releaseNotesFetchedMsg struct {
		Tag   string
		Notes string
		Err   string
	}

	// Async results for AddVideo dialog
	fetchFormatsAutoAdvanceMsg struct{} // timer msg to auto-skip format on error
	addVideoResultMsg          struct {
		Feedback string
	}
	fetchFormatsResultMsg struct {
		Formats *FormatsData
		Err     string
	}
	importResultMsg struct {
		Title string
		Err   string
	}
	createTrimResultMsg struct {
		Filename string
		Err      string
	}
	deleteTrimResultMsg struct {
		TrimID   string
		Filename string
		Err      string
	}
	// deleteJobsResultMsg reports completion of an async job delete.
	// OnDeleteJob blocks in WaitForJobExit (up to 5s per job), so deletes
	// run off the update loop and report back here. Title is set for the
	// single-job path; batch deletes report Count only.
	deleteJobsResultMsg struct {
		Count int
		Title string
	}
	fetchOrphansResultMsg struct {
		Files []OrphanedFileEntry
		Err   string
	}
	deleteOrphanResultMsg struct {
		Path string
		Err  string
	}
	fetchOrphanedHistoryResultMsg struct {
		Entries []OrphanedHistoryEntry
		Err     string
	}
	deleteHistoryEntryResultMsg struct {
		VideoID string
		Err     string
	}

	// Async results for FFmpeg check overlay
	ffmpegCheckResultMsg struct {
		Valid   bool
		Version string
		Warning string
		Path    string // the path that was checked
	}
	ffmpegPrepareResultMsg struct {
		NeedsElevation bool
		Script         string
		Token          string
		Err            string
	}
	ffmpegConfirmResultMsg struct {
		Err string
	}
	// ffmpegMenuActionMsg carries a main/install menu action resolved when
	// the huh form completes on a non-key cycle (huh finishes a selection
	// via a follow-up message that only routeComponentMsg sees, never
	// HandleKey). App handles it exactly like the equivalent HandleKey
	// action string.
	ffmpegMenuActionMsg struct {
		Action string
	}

	// Async result for the R B feed-history re-scan: the forced sweep
	// returned — every scan it decided on is queued (progress then flows
	// through BackfillStatusMsg into the status bar).
	backfillRescanQueuedMsg struct{}

	// Async results for cookie refresh
	// cookieRecheckResultMsg carries VERDICTS, not booleans. R C is the key
	// an operator presses to ask "do my cookies work", and the flattened bool
	// could not tell "the site rejected them" from "the check never reached
	// the site" — so a DNS blip answered "YouTube not authenticated", which is
	// a claim the check did not make and sends the user off to re-export
	// perfectly good cookies.
	//
	// The two Reason strings are the WHY behind a verdict the verdict alone
	// cannot carry: "could not establish" is the same sentence for a rate
	// limit, a captive portal and an intercepting proxy, and only one of those
	// is worth waiting out. Empty whenever nothing was recorded, and empty
	// from any wiring that does not supply one — but NOT implied by a
	// conclusive verdict. A conclusive REFUSAL may carry one and two producers
	// do: the unsignable-jar sentinel (verdictFromCheck maps
	// ErrAuthCheckNotAttempted to RefreshFailed with the error recorded) and,
	// since Arc 10, the Twitch chat-downgrade mark. Only RefreshOK is empty by
	// construction — it requires a nil error, and the reason string is that
	// error.
	//
	// They are cookies.AuthStatus's YouTubeError / TwitchError, which had no
	// reader anywhere in the tree until Arc 8 Task 12a. Every string that can
	// reach them names a status code, a scheme+host, a header NAME or a static
	// sentence; none carries a response body. See CookieStatusPayload in
	// internal/web/routes/cookies.go, which projects the same two fields onto
	// the wire and states that rule in full.
	//
	// LastError is a DIFFERENT SERVICE'S fact, carried on the same message
	// because R C is where the TUI answers "what are my cookies doing" and an
	// operator asking that is owed both halves. The verdicts above come from
	// the in-process RefreshService's own check; this is
	// AutoCookieStatus.LastError — the last thing a cookie pass (browser
	// refresh or interactive setup) concluded that the operator has to act on,
	// and it can be non-empty while both verdicts are RefreshOK. That is not a
	// contradiction: cookies.txt can be alive on a session refresh while the
	// mechanism that RENEWS it is broken, and the field's write policy
	// (internal/cookies/autocookies.go) exists precisely so a recorded failure
	// is not retracted by a pass that never established it was over. Empty
	// whenever nothing is recorded, and empty from any wiring that does not
	// supply it.
	cookieRecheckResultMsg struct {
		YouTube       cookies.RefreshVerdict
		Twitch        cookies.RefreshVerdict
		YouTubeReason string
		TwitchReason  string
		LastError     string
	}
	cookieForceRefreshResultMsg struct {
		// Result is carried whole rather than pre-flattened to a bool pair.
		// The flattened form could not tell a pass that DECLINED to run from
		// one that ran and concluded the credentials are dead, so R F reported
		// a verification failure for healthy cookies whenever the 30-minute
		// tick or an interactive setup already held the single-flight slot.
		//
		// Three fields carry three independent facts and the feedback branch
		// needs all of them: Ran (did this pass do any work), Overall (what it
		// concluded, if anything), and Renewed (did THIS pass produce the
		// credentials it verified — a working cookies.txt outlives a browser
		// refresh that did nothing, so "the cookies work" and "the refresh
		// worked" are separate answers and the operator pressed a key asking
		// the second one).
		Result cookies.RefreshResult
		Err    error
	}

	// Async results for channel URL resolution
	channelResolvedMsg struct {
		ID       string
		Name     string
		Platform string
		Err      error
	}

	// Async results for client token management
	fetchClientTokensResultMsg struct {
		Tokens []*database.ClientToken
		Err    string
	}
	deleteClientTokenResultMsg struct {
		ID  string
		Err string
	}

	// Async results for setup wizard cookie extraction.
	//
	// Carries the whole SetupResult rather than the bool pair it used to. Two
	// of the three outcomes look identical through a bool: cookies verified,
	// and cookies saved that the check could not reach the site to confirm.
	// The wizard reported both as "configured", so a network blip during the
	// check was invisible — and its mirror image, an extraction whose cookies
	// cannot form an authenticated request at all, was reported as "no login
	// detected", which is a different problem with different advice.
	setupCookieFinishMsg struct {
		Platform string // "youtube" or "twitch"
		Result   cookies.SetupResult
		Err      string // error message from extraction (empty on success)
	}

	// Async result for setup wizard config save
	setupSaveResultMsg struct {
		Err string
	}

	// panicRecoveryMsg is sent when a tea.Cmd closure recovers from a panic.
	panicRecoveryMsg struct {
		Text string
	}
)

// ConnectivityMsg is sent when internet connectivity state changes.
type ConnectivityMsg struct {
	Online bool
}

// chordState tracks the two-key chord system state machine.
type chordState struct {
	prefix     string    // "a", "r", "o", "q" or ""
	prefixTime time.Time // when prefix was pressed
	action     string    // second key (for confirm step), empty if waiting
	actionTime time.Time // when confirm prompt shown
}

// App is the root BubbleTea model.
type App struct {
	// Panels
	taskList        *TaskListModel
	details         *JobDetailsModel
	logs            *LogViewerModel
	statusBar       *StatusBarModel
	help            *HelpModel
	addVideo        *AddVideoModel
	importDlg       *ImportDialogModel
	trimDlg         *TrimDialogModel
	filesDlg        *FilesDialogModel
	clientTokensDlg *ClientTokensDialogModel
	setupWiz        *SetupWizardModel
	settings        *SettingsModel

	// Trim progress (async encoding)
	trimInProgress  bool
	trimStartedAt   time.Time
	trimProgressMu  sync.Mutex
	trimProgressPct float64

	// Progress
	progressStore *ProgressStore
	statusMap     map[string]database.JobStatus // track last-known status per job

	// Layout
	focusedPanel FocusPanel
	width        int
	height       int

	// Panel regions for mouse hit-testing
	taskRegion   PanelRegion
	detailRegion PanelRegion
	logRegion    PanelRegion

	// Chord state machine
	chord chordState

	// Action menu (command palette)
	actionMenu *ActionMenuModel

	// Feedback message (auto-clears after 3s)
	feedbackMsg string
	// feedbackSev is what the composer of feedbackMsg KNEW about its severity,
	// where it knew anything. severityUnstated — the zero value — means it did
	// not, and feedbackColor falls back to scanning the text.
	//
	// Only ever read while feedbackMsg != "" (see View), and every write of a
	// non-empty feedbackMsg goes through setFeedback, setFeedbackWithDuration
	// or setFeedbackWithSeverity, each of which writes this field in the same
	// statement pair. So a severity can never be read against a message other
	// than the one it was stated for; the sites that only CLEAR feedbackMsg
	// leave this behind harmlessly, and the next setter overwrites it.
	// TestStatedSeverityDoesNotLeakToTheNextMessage pins that.
	feedbackSev   feedbackSeverity
	feedbackTimer time.Time

	// Log batching buffer (250ms flush cycle like TypeScript)
	logBuffer []string

	// Demand-driven tick guards. Each self-perpetuating tick loop triggers a
	// full-screen View() rebuild per fire, so on a 24/7 dashboard we only run
	// a loop while it has something to do and stop it otherwise. The guard
	// bool ensures a restart can't stack a second overlapping ticker.
	//   marqueeTicking:    the 150ms marquee loop runs only while a visible
	//                      title actually overflows (NeedsScroll).
	//   logFlushScheduled: a trailing log flush is armed and pending. Log
	//                      flushing is a leading-edge throttle: the first
	//                      batch after a quiet period renders IMMEDIATELY
	//                      (real-time principle), and only follow-up batches
	//                      inside the logFlushInterval window wait for the
	//                      armed trailing flush. lastLogFlush is the stamp
	//                      the throttle compares against.
	//   progressTicking:   the progress-overlay loop runs only while a job is
	//                      live (or a trim runs) — its Duration / "Starts In" /
	//                      chat counts tick with wall-clock time; when every
	//                      job is terminal the overlay is static.
	marqueeTicking    bool
	logFlushScheduled bool
	lastLogFlush      time.Time
	progressTicking   bool
	// progressGen invalidates a superseded progress schedule on cadence
	// upshift; progressInterval records the class the running loop was last
	// scheduled at (so the upshift can detect 500ms→16ms transitions).
	progressGen      int
	progressInterval time.Duration
	// lastArchiveSweep throttles the 60s archive-boundary resweep run from
	// the 1s tick (TUI analog of the web UI's archive sweep).
	lastArchiveSweep time.Time
	// updateConfirmAt is the press-again confirmation window for applying
	// an update while downloads are active (applyUpdateAction).
	updateConfirmAt time.Time

	// Channels for async updates
	jobUpdateCh       <-chan *database.JobChange
	jobAddedCh        <-chan *database.JobAdded
	jobDeletedCh      <-chan *database.JobDeleted
	jobTrimsChangedCh <-chan *database.Job
	jobsUpdateCh      <-chan []*database.Job
	logCh             <-chan string
	checkTimersCh     <-chan CheckTimersMsg
	cookieStatusCh    <-chan CookieStatusMsg
	diskStatusCh      <-chan DiskStatusMsg
	backfillStatusCh  <-chan BackfillStatusMsg
	updateStatusCh    <-chan UpdateStatusMsg

	// Update status
	updateAvailable   *UpdateStatusMsg
	version           string
	releaseNotesPopup *releaseNotesOverlay

	// restartPending stays true once a settings save commits a
	// restart-required field, until the process actually exits. The
	// overlay-modal flow lets the user dismiss the prompt with Esc
	// without restarting; without this flag the dismissal would leave
	// the on-disk config drifting silently from the running process.
	// Audit reports/tui.md #26.
	restartPending bool

	// BubbleTea program reference (set by Run on the main goroutine;
	// Send/QuitTUI read it from other goroutines — atomic for race safety)
	program atomic.Pointer[tea.Program]

	// windowTitle holds the current terminal window title, set by updateTerminalTitle()
	// and applied via View()'s tea.View return value.
	windowTitle string

	// Config reference for settings panel. cfg is the direct
	// *MoomboxConfig pointer (used by applyValues big-block writes);
	// configStore exposes the same struct with synchronisation. New code
	// should prefer configStore.Read / configStore.Update.
	cfg         *config.MoomboxConfig
	configStore *config.Store

	// Internal token for CSRF bypass on local API calls
	internalToken string

	// Cached HTTP client for local API calls (avoids re-creating per request).
	// cachedClientHTTPS records which HTTPSEnabled value the cache was built
	// against so a toggle in settings forces a rebuild on the next call
	// (audit reports/tui.md Finding 3).
	cachedClient      *http.Client
	cachedClientHTTPS bool

	// Terminal background detection (updated from BackgroundColorMsg)
	isDark bool

	// Terminal color capability (updated from ColorProfileMsg on startup)

	// First-run flag: triggers setup wizard
	IsFirstRun bool

	// seenChordHint is set once the user first presses any chord key, dismissing
	// the newcomer hint in the status bar (session-only, not persisted).
	seenChordHint bool

	// Callbacks for actions
	OnAddVideo        func(url string)
	OnCancelJob       func(jobID string)
	OnDeleteJob       func(jobID string)
	OnResumeJob       func(jobID string)
	OnReinitializeJob func(jobID string)
	OnMuxJob          func(jobID string) error
	HasStagingFiles   func(jobID string) bool // checks if staging dir has files
	HasSegmentFiles   func(jobID string) bool // checks if staging dir has segment files
	OnCreateTrim      func(jobID string, startSec, endSec float64, onProgress func(float64)) (filename string, errMsg string)
	OnDeleteTrim      func(jobID, trimID string) error
	OnOpenFolder      func(jobID string)
	OnSaveConfig      func(cfg *config.MoomboxConfig)
	OnRestart         func()
	OnHashPassword    func(password string) string
	OnVerifyPassword  func(password, hash string) bool
	OnFetchFormats    func(videoID string) (*FormatsData, error)        // optional: fetch formats via service
	OnImportFile      func(path, title, channel string) (string, error) // optional: import zip, returns title
	OnListOrphans     func() ([]OrphanedFileEntry, error)               // list orphaned files
	OnDeleteOrphan    func(path string) error                           // delete orphaned file
	// Orphaned processing-history rows (no matching job) shown in the same overlay.
	OnListOrphanedHistory func() ([]OrphanedHistoryEntry, error)
	OnDeleteHistoryEntry  func(videoID string) error

	// Client token callbacks
	OnListClientTokens  func() ([]*database.ClientToken, error)
	OnDeleteClientToken func(id string) error

	// Update callbacks
	OnCheckUpdate     func() (*UpdateStatusMsg, error) // manual check — returns nil if up to date
	OnForceCheck      func()                           // force an immediate monitor poll of all sources
	OnBackfillRescan  func()                           // force a feed-history backfill re-scan of all channels (R B)
	OnApplyUpdate     func(version string) string      // returns error string (empty on success, process exits)
	OnVerifySignature func() error                     // verify current binary's signature
	// OnFetchReleaseNotes fetches release notes for a specific version from GitHub.
	// Used by R N chord when no update is available — shows the CURRENT version's
	// notes in the same overlay used for pending-update notes.
	OnFetchReleaseNotes func(version string) (tag, notes string, err error)

	// Cookie refresh callbacks
	// OnRecheckCookies runs the auth check and reports what it CONCLUDED per
	// platform, plus WHY for a platform that concluded nothing. See
	// cookieRecheckResultMsg for why the bool pair it used to return could not
	// be worded truthfully, and for what the two reason strings may contain.
	//
	// A reason is meaningful only alongside a RefreshUnknown verdict; wirings
	// return "" for the other two, and the renderer ignores a reason that
	// arrives with a conclusive verdict rather than trusting the caller.
	OnRecheckCookies func() (yt, tw cookies.RefreshVerdict, ytReason, twReason string)
	// OnAutoCookieLastError reports AutoCookieStatus.LastError, or "" when
	// nothing is recorded. nil when there is no auto-cookie service, and the
	// R C line then reads exactly as it does today.
	//
	// A CALLBACK OF ITS OWN rather than two more returns on OnRecheckCookies,
	// because it comes off a different service and a different call
	// (AutoCookieService.GetStatus, not RefreshService.GetStatus). Folding it
	// into that signature would make the two facts look like one measurement,
	// and would force every wiring that has an auth check but no auto-cookie
	// service to answer for a field it cannot see.
	OnAutoCookieLastError func() string
	// OnForceRefreshCookies runs the browser cookie refresh. nil if
	// auto-cookies are not configured. It returns the pass's whole result
	// rather than a bool: see cookieForceRefreshResultMsg for why the
	// flattened form could not be worded truthfully.
	OnForceRefreshCookies func() (cookies.RefreshResult, error)

	// FFmpeg check callbacks
	OnCheckFFmpeg    func(path string) (bool, string, string)                                   // check if ffmpeg path is valid → (valid, version, warning)
	OnCheckPrereqs   func() (bool, bool)                                                        // returns (chocoAvail, wingetAvail)
	OnPrepareInstall func(method string) (needsElevation bool, script, token string, err error) // elevation check + prepare
	OnConfirmInstall func(token string) error                                                   // execute reviewed elevated install
	OnRejectInstall  func(token string)                                                         // decline pending elevated install

	// FFmpeg check overlay
	ffmpegCheck *FFmpegCheckModel
	showFFmpeg  bool // flag to show FFmpeg check on startup
}

// NewApp creates a new TUI application.
func NewApp() *App {
	ps := NewProgressStore()
	tl := NewTaskListModel()
	tl.progressStore = ps

	return &App{
		taskList:          tl,
		details:           NewJobDetailsModel(),
		logs:              NewLogViewerModel(),
		statusBar:         NewStatusBarModel(),
		help:              NewHelpModel(),
		addVideo:          NewAddVideoModel(),
		importDlg:         NewImportDialogModel(),
		trimDlg:           NewTrimDialogModel(),
		filesDlg:          NewFilesDialogModel(),
		clientTokensDlg:   NewClientTokensDialogModel(),
		setupWiz:          NewSetupWizardModel(),
		settings:          NewSettingsModel(),
		ffmpegCheck:       NewFFmpegCheckModel(),
		actionMenu:        NewActionMenuModel(),
		releaseNotesPopup: newReleaseNotesOverlay(),
		progressStore:     ps,
		statusMap:         make(map[string]database.JobStatus),
		isDark:            true, // default to dark; updated by BackgroundColorMsg
	}
}

// BackfillLogs seeds the log viewer with historical lines (e.g. from the logger's ring buffer).
// Must be called before Run().
func (a *App) BackfillLogs(lines []string) {
	a.logs.AddLines(lines)
}

// ShowFFmpegCheck marks the FFmpeg check overlay to show after init.
func (a *App) ShowFFmpegCheck() {
	a.showFFmpeg = true
}

// SetVersion sets the current application version for display.
func (a *App) SetVersion(v string) {
	a.version = v
	a.details.version = v
}

// SetInternalToken sets the secret token for CSRF bypass on local API calls.
func (a *App) SetInternalToken(token string) {
	a.internalToken = token
}

// SetConfig provides the config reference for the settings panel.
func (a *App) SetConfig(cfg *config.MoomboxConfig) {
	a.cfg = cfg
	a.taskList.SetHideFinishedAgeDays(int(cfg.Monitors.HideFinishedAgeDays.Days()))
}

// SetConfigStore wires the unified config Store into the App and its
// sub-models (DECISIONS #8). Also sets a.cfg + a.settings.cfg as
// stable pointers for the applyValues big-block writes that mutate cfg
// directly under the Store's lock.
func (a *App) SetConfigStore(s *config.Store) {
	a.configStore = s
	a.cfg = s.Config()
	a.settings.configStore = s
	a.settings.cfg = s.Config()
}

// SetSetupCallbacks wires callback functions for the TUI setup wizard.
func (a *App) SetSetupCallbacks(
	onComplete func(cfg *config.MoomboxConfig) error,
	onInstallYtdlp func(port int, httpsEnabled bool),
	onStartAutoCookie func(platform string) error,
	onFinishAutoCookie func() (cookies.SetupResult, error),
	onCancelAutoCookie func(),
	onRestart func(),
) {
	a.setupWiz.OnComplete = onComplete
	a.setupWiz.OnInstallYtdlp = onInstallYtdlp
	a.setupWiz.OnStartAutoCookie = onStartAutoCookie
	a.setupWiz.OnFinishAutoCookie = onFinishAutoCookie
	a.setupWiz.OnCancelAutoCookie = onCancelAutoCookie
	a.setupWiz.OnRestart = onRestart
}

// SetupWizFFmpegCheck sets the FFmpeg check callback for the setup wizard.
func (a *App) SetupWizFFmpegCheck(fn func() (bool, string)) {
	a.setupWiz.OnCheckFFmpeg = fn
}

// SetupWizHashPassword sets the password hashing callback for the setup wizard.
func (a *App) SetupWizHashPassword(fn func(string) (string, error)) {
	a.setupWiz.OnHashPassword = fn
}

// SetUpdateChannels configures the async update channels.
func (a *App) SetUpdateChannels(
	jobUpdate <-chan *database.JobChange,
	jobAdded <-chan *database.JobAdded,
	jobDeleted <-chan *database.JobDeleted,
	jobTrimsChanged <-chan *database.Job,
	jobsUpdate <-chan []*database.Job,
	logCh <-chan string,
	checkTimers <-chan CheckTimersMsg,
	cookieStatus <-chan CookieStatusMsg,
	diskStatus <-chan DiskStatusMsg,
	backfillStatus <-chan BackfillStatusMsg,
	updateStatus <-chan UpdateStatusMsg,
) {
	a.jobUpdateCh = jobUpdate
	a.jobAddedCh = jobAdded
	a.jobDeletedCh = jobDeleted
	a.jobTrimsChangedCh = jobTrimsChanged
	a.jobsUpdateCh = jobsUpdate
	a.logCh = logCh
	a.checkTimersCh = checkTimers
	a.cookieStatusCh = cookieStatus
	a.diskStatusCh = diskStatus
	a.backfillStatusCh = backfillStatus
	a.updateStatusCh = updateStatus
}

// Init implements tea.Model.
func (a *App) Init() tea.Cmd {
	a.focusedPanel = PanelTasks
	a.taskList.SetFocused(true)

	// Auto-trigger setup wizard on first run (A3)
	if a.IsFirstRun {
		a.setupWiz.Open()
	}

	// Show FFmpeg check overlay if flagged
	if a.showFFmpeg && !a.IsFirstRun {
		a.ffmpegCheck.OnCheckPrereqs = a.OnCheckPrereqs
		a.ffmpegCheck.Open()
	}

	// Set the initial terminal title once — thereafter it's event-driven
	// (job-lifecycle handlers refresh it on status change), no longer polled
	// on the 1s tick.
	a.updateTerminalTitle()

	// marqueeTick, logFlushTick and progressTick are NOT started here — they're
	// demand-driven (ensureMarqueeTicking / scheduleLogFlush /
	// ensureProgressTicking). At startup no title is selected, no logs are
	// buffered, and statusMap is empty, so none of them has work to do; the
	// first JobsUpdateMsg (initial snapshot) starts the progress loop if any
	// job is live.
	return tea.Batch(a.tick(), a.listenForUpdates(), tea.RequestBackgroundColor)
}

// ensureMarqueeTicking starts the marquee animation loop when a visible title
// overflows its column and the loop isn't already running. Returns nil when
// nothing needs to scroll or a loop is already active, so callers can batch it
// unconditionally. The marquee only ever animates the SELECTED job's title, so
// every event that can newly require it hooks this directly (key nav, mouse,
// resize, job events, async dialog closes), with the 1s tick as a backstop
// bounding a missed hook's start latency to <=1s. Note the marquee's ~2s
// initial pause is TICK-counted, not wall-clock — backstop delay is additive
// to the pause, not absorbed by it — which is why the direct hooks matter.
func (a *App) ensureMarqueeTicking() tea.Cmd {
	if a.marqueeTicking {
		return nil
	}
	// A full-screen overlay (settings, setup wizard, help, action menu, …)
	// replaces the whole view, hiding the task list and detail titles — no
	// point animating a marquee nobody can see. It restarts when the overlay
	// closes (the closing keypress hook, or the 1s backstop within <=1s).
	if a.hasActiveOverlay() {
		return nil
	}
	if a.taskList.marquee.NeedsScroll() || a.details.marquee.NeedsScroll() {
		a.marqueeTicking = true
		return a.marqueeTick()
	}
	return nil
}

// scheduleLogFlush arms a single trailing flush when logs have arrived and no
// flush is already pending. The flush handler disarms the guard once the
// buffer drains, so during a log burst exactly one flush runs per
// logFlushInterval window and idle periods run none. Callers flush the
// leading edge inline (see the LogBatchMsg handler) — this only covers the
// follow-up batches inside an open window.
func (a *App) scheduleLogFlush() tea.Cmd {
	if a.logFlushScheduled {
		return nil
	}
	a.logFlushScheduled = true
	return a.logFlushTick()
}

// flushLogBuffer drains the buffered log lines into the log viewer and stamps
// lastLogFlush for the leading-edge/trailing throttle.
func (a *App) flushLogBuffer() {
	if len(a.logBuffer) > 0 {
		a.logs.AddLines(a.logBuffer)
		a.logBuffer = a.logBuffer[:0]
	}
	a.lastLogFlush = time.Now()
}

func (a *App) tick() tea.Cmd {
	// Phase-locked to the wall-clock second: fire ~20ms after each second
	// boundary instead of a drifting 1Hz phase. The task-list header's
	// next-check countdowns are seconds-granularity values computed at View
	// time — with a drifting phase they lag the true boundary by up to 1s
	// and occasionally skip a displayed second; phase-locked, they flip
	// right after every boundary at the same one-render-per-second cost.
	next := time.Until(time.Now().Truncate(time.Second).Add(time.Second + 20*time.Millisecond))
	return tea.Tick(next, func(t time.Time) tea.Msg {
		return tickMsg{}
	})
}

const (
	progressFastInterval = 16 * time.Millisecond  // ~60fps during active downloads
	progressIdleInterval = 500 * time.Millisecond // Upcoming countdown / chat-count cadence
)

// wantsFastProgress reports whether the progress loop should run at the
// 60fps class: an actively-delivering download, or a visible in-progress trim.
func (a *App) wantsFastProgress() bool {
	return a.hasActiveDownloads() || (a.trimInProgress && a.trimDlg.IsVisible())
}

func (a *App) progressTick() tea.Cmd {
	interval := progressIdleInterval
	if a.wantsFastProgress() {
		interval = progressFastInterval
	}
	a.progressInterval = interval
	gen := a.progressGen
	return tea.Tick(interval, func(t time.Time) tea.Msg {
		return progressTickMsg{gen: gen}
	})
}

// logFlushInterval is the log-batching window (A2): the first batch after a
// quiet period flushes immediately; follow-ups within this window coalesce
// into one trailing flush.
const logFlushInterval = 250 * time.Millisecond

// logFlushTick returns the one-shot trailing flush command.
func (a *App) logFlushTick() tea.Cmd {
	return tea.Tick(logFlushInterval, func(t time.Time) tea.Msg {
		return logFlushMsg{}
	})
}

// marqueeTick returns a command that fires every 150ms for marquee scrolling.
func (a *App) marqueeTick() tea.Cmd {
	return tea.Tick(150*time.Millisecond, func(t time.Time) tea.Msg {
		return marqueeTickMsg{}
	})
}

func (a *App) listenForUpdates() tea.Cmd {
	// If all channels are nil, don't spawn a blocking goroutine
	if a.jobUpdateCh == nil && a.jobAddedCh == nil && a.jobDeletedCh == nil &&
		a.jobTrimsChangedCh == nil &&
		a.jobsUpdateCh == nil && a.logCh == nil &&
		a.checkTimersCh == nil && a.cookieStatusCh == nil && a.diskStatusCh == nil &&
		a.backfillStatusCh == nil && a.updateStatusCh == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case ev, ok := <-a.jobUpdateCh:
			if !ok {
				return channelClosedMsg{Name: "jobUpdate"}
			}
			return JobUpdateMsg{Change: ev}
		case ev, ok := <-a.jobAddedCh:
			if !ok {
				return channelClosedMsg{Name: "jobAdded"}
			}
			return JobAddedMsg{Added: ev}
		case ev, ok := <-a.jobDeletedCh:
			if !ok {
				return channelClosedMsg{Name: "jobDeleted"}
			}
			return JobDeletedMsg{Deleted: ev}
		case job, ok := <-a.jobTrimsChangedCh:
			if !ok {
				return channelClosedMsg{Name: "jobTrimsChanged"}
			}
			return TrimsChangedMsg{Job: job}
		case jobs, ok := <-a.jobsUpdateCh:
			if !ok {
				return channelClosedMsg{Name: "jobsUpdate"}
			}
			return JobsUpdateMsg{Jobs: jobs}
		case line, ok := <-a.logCh:
			if !ok {
				return channelClosedMsg{Name: "log"}
			}
			// Drain all pending log messages into a single batch to avoid
			// triggering a View() re-render per individual log line.
			batch := []string{line}
			for len(batch) < 200 {
				select {
				case more, ok := <-a.logCh:
					if !ok {
						return LogBatchMsg{Lines: batch}
					}
					batch = append(batch, more)
				default:
					return LogBatchMsg{Lines: batch}
				}
			}
			return LogBatchMsg{Lines: batch}
		case timers, ok := <-a.checkTimersCh:
			if !ok {
				return channelClosedMsg{Name: "checkTimers"}
			}
			return timers
		case cs, ok := <-a.cookieStatusCh:
			if !ok {
				return channelClosedMsg{Name: "cookieStatus"}
			}
			return cs
		case ds, ok := <-a.diskStatusCh:
			if !ok {
				return channelClosedMsg{Name: "diskStatus"}
			}
			return ds
		case bs, ok := <-a.backfillStatusCh:
			if !ok {
				return channelClosedMsg{Name: "backfillStatus"}
			}
			return bs
		case us, ok := <-a.updateStatusCh:
			if !ok {
				return channelClosedMsg{Name: "updateStatus"}
			}
			return us
		}
	}
}

// hasActiveOverlay returns true if any overlay dialog is currently visible.
func (a *App) hasActiveOverlay() bool {
	return a.settings.IsVisible() ||
		a.help.IsVisible() ||
		(a.releaseNotesPopup != nil && a.releaseNotesPopup.isOpen()) ||
		a.importDlg.IsVisible() ||
		a.addVideo.IsVisible() ||
		a.trimDlg.IsVisible() ||
		a.filesDlg.IsVisible() ||
		a.clientTokensDlg.IsVisible() ||
		a.setupWiz.IsVisible() ||
		a.ffmpegCheck.IsVisible() ||
		a.actionMenu.IsVisible()
}

// hasActiveDownloads returns true if any job has live progress to display.
func (a *App) hasActiveDownloads() bool {
	return a.activeDownloadCount() > 0
}

// activeDownloadCount counts jobs currently downloading/live/muxing —
// used by hasActiveDownloads and the update-apply confirmation.
func (a *App) activeDownloadCount() int {
	n := 0
	for _, s := range a.statusMap {
		switch s {
		case database.StatusDownloading, database.StatusLive, database.StatusMuxing:
			n++
		}
	}
	return n
}

// applyUpdateAction runs the update-apply flow shared by the R U chord and
// the release-notes overlay's U key. When downloads are active it requires
// a SECOND invocation within 5s (mirroring the confirm-chord pattern):
// updating restarts the process, interrupting recordings — live segments
// broadcast during the restart gap may be lost (Twitch expires fastest) —
// so that must be a deliberate choice. Returns nil when only feedback was
// shown (no update, or confirmation pending).
func (a *App) applyUpdateAction() tea.Cmd {
	if a.updateAvailable == nil || a.OnApplyUpdate == nil {
		a.setFeedback("No update available — use R V to check")
		return nil
	}
	if n := a.activeDownloadCount(); n > 0 && time.Since(a.updateConfirmAt) > 5*time.Second {
		a.updateConfirmAt = time.Now()
		plural := "download is"
		if n != 1 {
			plural = "downloads are"
		}
		a.setFeedback(fmt.Sprintf("%d %s active — the update restart interrupts them; press R U again within 5s to confirm", n, plural))
		return nil
	}
	a.updateConfirmAt = time.Time{}
	a.setFeedback(fmt.Sprintf("Updating to %s...", a.updateAvailable.TagName))
	ver := a.updateAvailable.Version
	applyFn := a.OnApplyUpdate
	return safeCmd(func() tea.Msg {
		return updateApplyResultMsg{Err: applyFn(ver)}
	})
}

// hasLiveContent reports whether the progress-overlay refresh loop has any work
// to do: a non-terminal job (whose Duration / "Starts In" countdown / chat
// counts advance with wall-clock time and so need periodic re-render) or an
// in-progress trim. When every job is terminal (Finished/Error/Cancelled/
// Cookies) and no trim runs, the overlay is static — the loop stops rather than
// waking every 500ms. Broader than hasActiveDownloads on purpose: Upcoming jobs
// have a live countdown even though nothing is downloading yet.
func (a *App) hasLiveContent() bool {
	if a.trimInProgress && a.trimDlg.IsVisible() {
		return true
	}
	for _, s := range a.statusMap {
		switch s {
		case database.StatusUpcoming, database.StatusLive,
			database.StatusDownloading, database.StatusMuxing:
			return true
		}
	}
	return false
}

// ensureProgressTicking starts the progress-overlay refresh loop when there's
// live content and the loop isn't already running. Returns nil otherwise, so
// callers batch it unconditionally. The guard flag stops a restart from
// stacking a second ticker; it's cheap to call on every job event because it
// short-circuits on the flag while a download is already ticking.
func (a *App) ensureProgressTicking() tea.Cmd {
	if a.progressTicking {
		// Cadence upshift without waiting out the pending tick: the loop is
		// running at the idle 500ms class and a download/trim just went
		// live. Supersede the in-flight schedule with a fresh 16ms one NOW —
		// the old tick is dropped on arrival by its stale generation —
		// instead of letting up to one 500ms beat delay 60fps progress
		// (real-time principle; the lag predated the demand-driven loops).
		if a.progressInterval != progressFastInterval && a.wantsFastProgress() {
			a.progressGen++
			return a.progressTick()
		}
		return nil
	}
	if a.hasLiveContent() {
		a.progressTicking = true
		return a.progressTick()
	}
	return nil
}

// updateTerminalTitle updates a.windowTitle with the current active/upcoming counts.
// The title is applied to the terminal via the View() return value.
func (a *App) updateTerminalTitle() {
	var activeCount, upcomingCount int
	for _, s := range a.statusMap {
		switch s {
		case database.StatusDownloading, database.StatusLive, database.StatusMuxing:
			activeCount++
		case database.StatusUpcoming:
			upcomingCount++
		}
	}

	title := "Moombox"
	if activeCount > 0 {
		title += fmt.Sprintf(" — %d active", activeCount)
	}
	if upcomingCount > 0 {
		title += fmt.Sprintf(" — %d upcoming", upcomingCount)
	}

	// Skip the reassignment when unchanged — the title only moves on status
	// transitions, so most calls (rapid progress-driven job updates) produce
	// an identical string.
	if title != a.windowTitle {
		a.windowTitle = title
	}
}

// getPort returns the configured port or default 774.
func (a *App) getPort() int {
	if a.configStore != nil {
		var port int
		a.configStore.Read(func(c *config.MoomboxConfig) {
			port = c.Network.Port
		})
		if port > 0 {
			return port
		}
	}
	if a.cfg != nil {
		if a.cfg.Network.Port > 0 {
			return a.cfg.Network.Port
		}
	}
	return 774
}
