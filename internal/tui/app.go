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
	"github.com/vampiricwulf/Moombox/internal/stats"
	"github.com/vampiricwulf/Moombox/internal/ytdlpplugin"
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
	// it: a cadence upshift (500ms→8ms) supersedes the in-flight schedule
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
	// dismissUpdateResultMsg is the async result of OnDismissUpdate, dispatched
	// by the S key in the release-notes overlay.
	dismissUpdateResultMsg struct {
		Tag string
		Err error
	}
	signatureVerifyResultMsg struct {
		Err string // empty on success
		// Manifest: the running release's signed manifest was checked too.
		// False on success means the release publishes none.
		Manifest bool
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
	// Each carries the video ID it was issued for, so a result that lands
	// after the operator moved on — a different ID, a closed or reopened
	// dialog — is told apart from the one the dialog is waiting on.
	fetchFormatsAutoAdvanceMsg struct { // timer msg to auto-skip format on error
		VideoID string
	}
	addVideoResultMsg struct {
		VideoID  string
		Feedback string
	}
	fetchFormatsResultMsg struct {
		VideoID string
		Formats *FormatsData
		Err     string
	}
	// importResultMsg is the A Z upload's answer. Note is the server's own
	// line for a name it found taken in imports/ (re-adopted or renamed), and
	// Renamed says the archive took a " (n)" name beside a different file.
	importResultMsg struct {
		Title   string
		Err     string
		Note    string
		Renamed bool
	}
	// cookieImportResultMsg is the async result of OnImportCookieFile (E I).
	// The whole cookies.ImportResult, not a bool: the overlay words each
	// platform off the outcome AND the verdict, and neither survives being
	// flattened. Err is the error VALUE rather than a string because the
	// import's refusals are sentinels the renderer prints verbatim; nothing
	// here ever carries cookie content.
	cookieImportResultMsg struct {
		Result cookies.ImportResult
		Err    error
	}
	createTrimResultMsg struct {
		Filename string
		Err      string
	}
	deleteTrimResultMsg struct {
		JobID    string // the job whose trim dialog asked
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
	// setWatchedResultMsg reports completion of an async A W toggle.
	setWatchedResultMsg struct {
		Count   int
		Watched bool
		Err     error
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
	// bulkOrphanResultMsg reports the outcome of an A-key delete-all sweep
	// over one section (orphaned files or orphaned history) — the per-item
	// callback ran once per entry, so partial failure is normal, not fatal.
	bulkOrphanResultMsg struct {
		Deleted  int
		Failures []string
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

	// Async results for the E Y yt-dlp plugin overlay. Info is the same
	// ytdlpplugin.Info GET /api/ytdlp-plugin/status returns — read straight
	// from the package the route's own body uses, so the terminal never has to
	// import the HTTP layer for one struct; Err is the error VALUE rather than
	// a string because nothing here reformats it.
	ytdlpStatusMsg struct {
		Info ytdlpplugin.Info
		Err  error
	}
	ytdlpInstallResultMsg struct {
		Err error
	}

	// statsSnapshotMsg is the async result of OnGetStats — the E T overlay's
	// open, its r key, and the 60 s refresh tick all go through it. Epoch is
	// the App.statsEpoch the fetch started under; a result that outlives its
	// open is dropped rather than painted over the new one.
	statsSnapshotMsg struct {
		Epoch int
		Snap  stats.Snapshot
		Err   error
	}
	// statsRefreshTickMsg fires every statsRefreshInterval while the E T
	// overlay is open (the Web Stats tab's own poll cadence); ignored once
	// the overlay is closed, OnGetStats is nil, or Epoch names an earlier
	// open (see App.statsEpoch).
	statsRefreshTickMsg struct{ Epoch int }

	// jobLogLinesMsg is the async result of OnGetJobLogs — the O L overlay's
	// open and every refresh tick go through it. Epoch is the App.jobLogEpoch
	// the read started under, so a read that outlives its open is dropped
	// rather than painted into another job's overlay.
	jobLogLinesMsg struct {
		Epoch int
		Lines []string
	}
	// jobLogRefreshTickMsg fires every jobLogRefreshInterval while the O L
	// overlay is open; ignored once the overlay is closed, OnGetJobLogs is
	// nil, or Epoch names an earlier open (see App.jobLogEpoch).
	jobLogRefreshTickMsg struct{ Epoch int }

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

	// clipboardResultMsg carries the outcome of the OS clipboard helper the
	// O C chord dispatches. The helper is a child process (clip.exe, on a
	// local Windows console) and must not run on the update goroutine — a
	// wedged child would freeze rendering and input for the whole of its
	// bound — so the chord returns a Cmd and the wording is finalised here.
	// Copied is true only when something reported that it really took the
	// text; the OSC 52 write the chord always sends can never report that.
	clipboardResultMsg struct {
		URL    string
		Copied bool
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

// SidecarStatusMsg is sent when the BotGuard sidecar's liveness changes.
// A plain bool by design: internal/tui must not import internal/bgutils, so
// cmd/moombox's wiring projects sidecar.Health onto this and nothing else
// crosses the boundary.
type SidecarStatusMsg struct {
	Healthy bool
}

// chordState tracks the two-key chord system state machine.
type chordState struct {
	prefix     string    // "a", "r", "o", "e", "q" or ""
	prefixTime time.Time // when prefix was pressed
	action     string    // second key (for confirm step), empty if waiting
	actionTime time.Time // when confirm prompt shown
	// jobID is the job the confirm prompt named, for a single-job confirm
	// chord. The confirm step acts on THIS job, not on whatever the cursor
	// is on at the third key: a mouse click or wheel tick, or a deletion
	// from the dashboard handing the cursor to a neighbour, can move it
	// inside the window, and the prompt named the original.
	jobID string
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
	cookieImportDlg *CookieImportDialogModel
	trimDlg         *TrimDialogModel
	filesDlg        *FilesDialogModel
	clientTokensDlg *ClientTokensDialogModel
	ytdlpDlg        *YtdlpDialogModel
	statsDlg        *StatsDialogModel
	jobLog          *JobLogModel
	setupWiz        *SetupWizardModel
	settings        *SettingsModel

	// statsEpoch names the current E T session. It is bumped on every open
	// and every close, and both stats messages carry the epoch they were
	// created under, so exactly one 60 s refresh chain is alive at a time:
	// ticks and fetch results from an earlier open are dropped instead of
	// re-arming a second chain (the Web's single setInterval).
	statsEpoch int
	// jobLogEpoch is statsEpoch's twin for the O L overlay: bumped on every
	// open and close, so exactly one refresh chain follows exactly one job,
	// and a read from an earlier open never lands in a later one.
	jobLogEpoch int

	// Trim progress (async encoding)
	trimInProgress  bool
	trimStartedAt   time.Time
	trimProgressMu  sync.Mutex
	trimProgressPct float64

	// Progress
	progressStore *ProgressStore
	statusMap     map[string]database.JobStatus // track last-known status per job
	// jobVersions is, per job, the newest database.Job.Version applied to the
	// held row and to the progress store. Writers notify after releasing the
	// database lock, so two of a job's updates can arrive in the opposite
	// order to their writes; the older is dropped (see staleJobUpdate) rather
	// than put back over the newer.
	jobVersions map[string]appliedJobVersions

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

	// feedback is the transient status line (auto-clears after 3 s) together
	// with everything known about it.
	//
	// ONE struct because the invariant is "a message never outlives the
	// severity it was stated for", which three fields could only maintain by
	// convention: every setter wrote all three in the same statement pair and
	// every clear-only site left the severity behind, harmlessly but on trust.
	// Written as a whole value there is nowhere to put a message without a
	// severity, and clearing is `appFeedback{}`.
	//
	// Behaviour is unchanged by the fold: the severity is still only read
	// while msg != "" (viewMain), and feedbackColor still falls back to
	// scanning the text when sev is severityUnstated — the zero value a
	// composer that knew nothing about its own line leaves in place.
	// TestStatedSeverityDoesNotLeakToTheNextMessage pins the invariant this
	// makes structural.
	feedback appFeedback

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
	// scheduled at (so the upshift can detect 500ms→8ms transitions).
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
	// webPort reports the port the web server bound (SetWebPort); nil in
	// tests, which fall back to the configured port.
	webPort func() int
	// webHTTPS reports whether the bound web server serves HTTPS
	// (SetWebHTTPS); nil in tests, where the setting is read instead.
	webHTTPS func() bool

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

	// asidesJobID / asidesCache memoise ONE job's JobAsides answer — the
	// selected one. updateSelectedJob runs on every cursor move and on every
	// JobsUpdateMsg, so an unmemoised probe would read the disk once per
	// database write. Keyed on the row's updated_at too (see asidesFor), and
	// invalidated by invalidateAsides when a recovery is dispatched for that
	// job or while it is active.
	asidesJobID     string
	asidesUpdatedAt string // the row version the memo was probed for
	asidesCache     AsideSummary

	// Callbacks for actions
	OnAddVideo func(url string)
	// OnCancelJob cancels a job and reports whether it did: a job that
	// ended since the list last showed it is left as it ended.
	OnCancelJob func(jobID string) bool
	OnDeleteJob func(jobID string)
	// OnSetWatched marks jobs watched/unwatched (the A W chord); the Web's
	// /watched routes are the twin.
	OnSetWatched      func(ids []string, watched bool) error
	OnResumeJob       func(jobID string)
	OnReinitializeJob func(jobID string)
	OnMuxJob          func(jobID string) error
	// OnRecoverAsides muxes the recordings the engine set aside for a job into
	// their own files beside the archive (the A S chord). Deliberately NOT
	// OnMuxJob: /mux and A M mean "mux the recording", and an aside overlaps
	// the recording from sequence 0 — it can only ever be a sibling.
	OnRecoverAsides func(jobID string) error
	HasStagingFiles func(jobID string) bool // checks if staging dir has files
	HasSegmentFiles func(jobID string) bool // checks if staging dir has segment files
	// HasUnmuxedParts reports whether a Finished job's staging still holds a
	// split part its finalize could not mux (worker.HasUnmuxedParts) — what
	// makes A M offer itself on a Finished row. A disk probe like the two
	// above, run on selection only.
	HasUnmuxedParts func(jobID string) bool
	// JobAsides reports a job's set-aside recordings and whether its staging
	// dir still holds a chat capture. A DISK probe like HasStagingFiles and
	// HasSegmentFiles beside it, so the same rule applies: it runs on
	// SELECTION, never when the action menu opens (CORE-9).
	JobAsides    func(jobID string) AsideSummary
	OnCreateTrim func(jobID string, startSec, endSec float64, onProgress func(float64)) (filename string, errMsg string)
	OnDeleteTrim func(jobID, trimID string) error
	OnOpenFolder func(jobID string)
	// OnSaveConfig persists the settings model's config. It returns the save
	// error so the overlay can report a failure instead of showing "Saved"
	// over a write that never landed (CORE-4).
	OnSaveConfig func(cfg *config.MoomboxConfig) error
	// OnFfmpegPathChange re-applies paths.ffmpeg_path to the services that
	// captured it when their muxers were built. Separate from OnSaveConfig
	// because the FFmpeg overlay deliberately keeps a validated path live
	// even when the disk write is refused, and OnSaveConfig skips its own
	// hot-reload block on that error (CORE-4).
	OnFfmpegPathChange func(path string)
	OnRestart          func()
	OnHashPassword     func(password string) string
	OnVerifyPassword   func(password, hash string) bool
	OnFetchFormats     func(videoID string) (*FormatsData, error)        // optional: fetch formats via service
	OnImportFile       func(path, title, channel string) (string, error) // optional: import zip, returns title
	OnListOrphans      func() ([]OrphanedFileEntry, error)               // list orphaned files
	OnDeleteOrphan     func(path string) error                           // delete orphaned file
	// Orphaned processing-history rows (no matching job) shown in the same overlay.
	OnListOrphanedHistory func() ([]OrphanedHistoryEntry, error)
	OnDeleteHistoryEntry  func(videoID string) error

	// Client token callbacks
	OnListClientTokens  func() ([]*database.ClientToken, error)
	OnDeleteClientToken func(id string) error

	// Update callbacks
	OnCheckUpdate     func() (*UpdateStatusMsg, error)  // manual check — returns nil if up to date
	OnForceCheck      func()                            // force an immediate monitor poll of all sources
	OnBackfillRescan  func()                            // force a feed-history backfill re-scan of all channels (R B)
	OnApplyUpdate     func(version string) string       // returns error string (empty on success, process exits)
	OnVerifySignature func() (manifest bool, err error) // verify current binary's signature, and its release's manifest when it has one
	// OnDismissUpdate skips a pending version (the S key in the
	// release-notes overlay); nil hides the key.
	OnDismissUpdate func(tag string) error
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
	// OnImportCookieFile imports a Netscape cookies.txt from disk through
	// AutoCookieService.ImportCookies — the E I chord, and the TUI's half of
	// the Web dashboard's import panel. nil when there is no auto-cookie
	// service, and nil DELETES the chord rather than making it inert: like
	// OnForceRefreshCookies, dispatchAction, buildMenuItems and the help
	// overlay each test the field.
	//
	// It takes a PATH and returns a result, never bytes in either direction.
	// Reading the file belongs to the wiring, so nothing in this package can
	// put a credential on the screen or in a log line; see
	// CookieImportDialogModel.
	OnImportCookieFile func(path string) (cookies.ImportResult, error)

	// OnYtdlpPluginStatus reports the yt-dlp PO-token plugin's state for the
	// port and scheme this process is actually serving on — the E Y overlay's
	// body, and the same ytdlpplugin.Status the dashboard's Integrations card
	// reads through routes.YtdlpPluginStatus. nil DELETES the chord rather than making it inert, like
	// OnImportCookieFile: an overlay whose only content can never load is
	// worse than a chord that is not offered.
	OnYtdlpPluginStatus func() (ytdlpplugin.Info, error)
	// OnInstallYtdlpPlugin (re)writes the yt-dlp plugin for the live port —
	// the E Y overlay's I key. Distinct from the setup wizard's
	// OnInstallYtdlp, which reports nothing back.
	//
	// It does NOT gate the chord: with a status callback and no install one,
	// the overlay is still worth reading and I says so instead of no-opping.
	OnInstallYtdlpPlugin func() error

	// OnGetStats returns the numbers the Web Stats tab shows (the E T
	// chord); nil deletes the chord.
	OnGetStats func() (stats.Snapshot, error)

	// OnGetJobLogs returns a copy of one job's own log buffer — db.GetJobLogs,
	// the buffer the dashboard's job dialog reads through GET
	// /api/jobs/{id}/logs — for the O L overlay; nil deletes the chord.
	OnGetJobLogs func(jobID string) []string

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

// appFeedback is the App's transient status line: what it says, what its
// composer knew about how alarming it is, and when it stops being shown.
//
// The dialogs' own feedbackMsg fields (FilesDialogModel, ClientTokensDialogModel)
// are unrelated — a different line with a different lifecycle, keyed to a
// confirm chord rather than a timer.
type appFeedback struct {
	msg string
	// sev is what the COMPOSER knew, where it knew anything. severityUnstated
	// — the zero value — means it did not, and feedbackColor falls back to
	// scanning the text. See feedbackSeverity for why the inference is not
	// good enough on the one line that carries a stated fact.
	sev feedbackSeverity
	// wrap lets the line take more than one row: View word-wraps it onto the
	// rows above the status bar (wrapFeedback) instead of cutting it to one.
	// Set only by setWrappedFeedback, for the lines that carry a sentence
	// written elsewhere whose tail is the part to act on — the held profile's
	// lock path, an import's outcome note. False, the zero value, is the one
	// ellipsized row every other line has always had.
	wrap bool
	// until is when the line stops being shown. The zero value means "nothing
	// scheduled", which is what an empty struct reads as.
	until time.Time
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
		cookieImportDlg:   NewCookieImportDialogModel(),
		trimDlg:           NewTrimDialogModel(),
		filesDlg:          NewFilesDialogModel(),
		clientTokensDlg:   NewClientTokensDialogModel(),
		ytdlpDlg:          NewYtdlpDialogModel(),
		statsDlg:          NewStatsDialogModel(),
		jobLog:            NewJobLogModel(),
		setupWiz:          NewSetupWizardModel(),
		settings:          NewSettingsModel(),
		ffmpegCheck:       NewFFmpegCheckModel(),
		actionMenu:        NewActionMenuModel(),
		releaseNotesPopup: newReleaseNotesOverlay(),
		progressStore:     ps,
		statusMap:         make(map[string]database.JobStatus),
		jobVersions:       make(map[string]appliedJobVersions),
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

// SetSidecarDown seeds the status bar's BotGuard sidecar alert before Run.
//
// The subscription in cmd/moombox carries every later transition through
// SidecarStatusMsg, but its immediate first snapshot lands before tui.Run has
// stored the program, and Send is a no-op until then. The state that already
// exists at TUI start therefore has to come in through the model, like every
// other pre-run Set* on this type.
func (a *App) SetSidecarDown(down bool) {
	a.statusBar.sidecarDown = down
}

// SetConfig provides the config reference for the settings panel.
func (a *App) SetConfig(cfg *config.MoomboxConfig) {
	a.cfg = cfg
	a.taskList.SetHideFinishedAgeDays(cfg.Monitors.HideFinishedAgeDays.Days())
}

// syncHideFinishedAge re-reads hide_finished_age_days from the config store
// and pushes it to the list when it moved.
//
// The TUI's own settings save applies it on overlay close (app_keys.go), but
// a change made from the DASHBOARD produces no TUI-side event at all — so
// before this the two UIs disagreed about which Finished jobs are archived
// until the operator happened to open and close the TUI settings overlay
// (CORE-11). Called from the same 60 s archive sweep that already re-buckets
// aged rows, so it costs one store read a minute.
//
// Read under the store lock, like getPort/apiBaseURL: HTTP handlers mutate
// the config through configStore.Update concurrently. The equality gate is
// what keeps this off the render path — SetHideFinishedAgeDays ends in a
// rebuild (and so moves the render cache key), which an unchanged threshold
// must not do once a minute for nothing.
func (a *App) syncHideFinishedAge() {
	if a.configStore == nil {
		return
	}
	var days float64
	a.configStore.Read(func(c *config.MoomboxConfig) {
		days = c.Monitors.HideFinishedAgeDays.Days()
	})
	if days == a.taskList.HideFinishedAgeDays() {
		return
	}
	a.taskList.SetHideFinishedAgeDays(days)
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
	// tuiTargetFPS is the renderer's frame-rate ceiling, handed to
	// tea.WithFPS at the package's one tea.NewProgram site (Run, in
	// app_commands.go). Bubbletea defaults to 60 and caps at 120
	// (defaultFPS / maxFPS, charm.land/bubbletea/v2), so 120 is the most
	// there is to ask for; the owner asked for it (ruling F1, 2026-09-25).
	// It costs nothing while nothing moves — a frame is drawn only when the
	// view actually changed, which is what the FrameCost pins measure.
	tuiTargetFPS = 120

	// progressFastInterval is the progress-tick cadence while a download is
	// delivering: one tick per frame at tuiTargetFPS (a frame is 8.33ms at
	// 120 fps), so a progress change reaches the model before the next frame
	// rather than sitting through one. A no-change tick costs no rebuild
	// (JobDetailsModel.SetProgress skips an unchanged pointer inside one
	// wall-clock second) and no repaint (the renderer's viewEquals) — only
	// the memoised View() bubbletea calls after every message, the 30-alloc
	// frame TestFrameCostAtLogCap pins (~0.25-0.6 ms at 20-1,000 jobs), so
	// doubling the tick count doubles that and nothing else.
	progressFastInterval = 8 * time.Millisecond   // ~120fps during active downloads
	progressIdleInterval = 500 * time.Millisecond // Upcoming countdown / chat-count cadence
)

// wantsFastProgress reports whether the progress loop should run at the fast
// class — one tick per frame at tuiTargetFPS: an actively-delivering
// download, or a visible in-progress trim.
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
		a.cookieImportDlg.IsVisible() ||
		a.addVideo.IsVisible() ||
		a.trimDlg.IsVisible() ||
		a.filesDlg.IsVisible() ||
		a.clientTokensDlg.IsVisible() ||
		a.ytdlpDlg.IsVisible() ||
		a.statsDlg.IsVisible() ||
		a.jobLog.IsVisible() ||
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
		// live. Supersede the in-flight schedule with a fresh 8ms one NOW —
		// the old tick is dropped on arrival by its stale generation —
		// instead of letting up to one 500ms beat delay fast-class progress
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

// SetWebHTTPS supplies whether the bound web server serves HTTPS. The scheme,
// like the port, is fixed when the server starts: https_enabled saved without
// the restart it asks for used to switch the TUI's own API client, O W and
// the plugin status to https:// against a listener still serving http, so
// every TUI action failed until the restart.
func (a *App) SetWebHTTPS(fn func() bool) {
	a.webHTTPS = fn
}

// httpsActive reports which scheme the TUI's local calls must use: the bound
// server's when SetWebHTTPS supplied it, the saved setting otherwise.
func (a *App) httpsActive() bool {
	if a.webHTTPS != nil {
		return a.webHTTPS()
	}
	on := false
	if a.configStore != nil {
		a.configStore.Read(func(c *config.MoomboxConfig) { on = c.Network.HTTPSEnabled })
	}
	return on
}

// SetWebPort supplies the port the web server actually bound, which can differ
// from the configured one when that was in use at boot.
func (a *App) SetWebPort(fn func() int) {
	a.webPort = fn
}

// getPort returns the port the web server is serving on, else the configured
// port, else the default 774.
func (a *App) getPort() int {
	if a.webPort != nil {
		if port := a.webPort(); port > 0 {
			return port
		}
	}
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
