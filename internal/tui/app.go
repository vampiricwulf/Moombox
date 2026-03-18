// Package tui provides the terminal user interface for Moombox using BubbleTea.
package tui

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/vampiricwulf/Moombox/internal/config"
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
	JobUpdateMsg   struct{ Job *database.Job }
	JobsUpdateMsg  struct{ Jobs []*database.Job }
	LogBatchMsg struct{ Lines []string }
	CheckTimersMsg struct {
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
	UpdateStatusMsg struct {
		Version      string
		TagName      string
		ReleaseNotes string
	}
	channelClosedMsg struct{ Name string }
	tickMsg          struct{}
	progressTickMsg  struct{}
	logFlushMsg     struct{} // 250ms log batching flush
	marqueeTickMsg  struct{} // 150ms marquee scroll tick

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

	// Async results for AddVideo dialog
	fetchFormatsAutoAdvanceMsg struct{} // timer msg to auto-skip format on error
	addVideoResultMsg         struct {
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
	fetchOrphansResultMsg struct {
		Files []OrphanedFileEntry
		Err   string
	}
	deleteOrphanResultMsg struct {
		Path string
		Err  string
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

	// Async results for cookie refresh
	cookieRecheckResultMsg struct {
		YouTubeAuth bool
		TwitchAuth  bool
	}
	cookieForceRefreshResultMsg struct {
		Success bool
		Err     error
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

	// Async results for setup wizard cookie extraction
	setupCookieFinishMsg struct {
		Platform string // "youtube" or "twitch"
		YTAuth   bool
		TWAuth   bool
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
	taskList  *TaskListModel
	details   *JobDetailsModel
	logs      *LogViewerModel
	statusBar *StatusBarModel
	help      *HelpModel
	addVideo  *AddVideoModel
	importDlg *ImportDialogModel
	trimDlg   *TrimDialogModel
	filesDlg       *FilesDialogModel
	clientTokensDlg *ClientTokensDialogModel
	setupWiz       *SetupWizardModel
	settings  *SettingsModel

	// Trim progress (async encoding)
	trimInProgress  bool
	trimStartedAt  time.Time
	trimProgressMu sync.Mutex
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
	feedbackMsg   string
	feedbackTimer time.Time

	// Log batching buffer (250ms flush cycle like TypeScript)
	logBuffer []string

	// Channels for async updates
	jobUpdateCh      <-chan *database.Job
	jobsUpdateCh     <-chan []*database.Job
	logCh            <-chan string
	checkTimersCh    <-chan CheckTimersMsg
	cookieStatusCh   <-chan CookieStatusMsg
	diskStatusCh     <-chan DiskStatusMsg
	updateStatusCh   <-chan UpdateStatusMsg

	// Update status
	updateAvailable *UpdateStatusMsg
	version         string

	// BubbleTea program reference (set by Run, used by QuitTUI)
	program *tea.Program

	// Config reference for settings panel
	cfg   *config.MoomboxConfig
	cfgMu *sync.RWMutex // shared config mutex (set via SetCfgMu)

	// Internal token for CSRF bypass on local API calls
	internalToken string

	// Cached HTTP client for local API calls (avoids re-creating per request)
	cachedClient *http.Client

	// First-run flag: triggers setup wizard
	IsFirstRun bool

	// Transient flag: set when setup wizard completes, shown once in empty state
	justCompletedSetup bool

	// Callbacks for actions
	OnAddVideo   func(url string)
	OnCancelJob  func(jobID string)
	OnDeleteJob  func(jobID string)
	OnRetryJob   func(jobID string)
	OnCreateTrim func(jobID string, startSec, endSec float64, onProgress func(float64)) (filename string, errMsg string)
	OnDeleteTrim func(jobID, trimID string) error
	OnOpenFolder func(jobID string)
	OnSaveConfig     func(cfg *config.MoomboxConfig)
	OnRestart        func()
	OnHashPassword   func(password string) string
	OnVerifyPassword func(password, hash string) bool
	OnFetchFormats   func(videoID string) (*FormatsData, error)          // optional: fetch formats via service
	OnImportFile     func(path, title, channel string) (string, error)  // optional: import zip, returns title
	OnListOrphans    func() ([]OrphanedFileEntry, error)                // list orphaned files
	OnDeleteOrphan   func(path string) error                            // delete orphaned file

	// Client token callbacks
	OnListClientTokens  func() ([]*database.ClientToken, error)
	OnDeleteClientToken func(id string) error

	// Update callbacks
	OnCheckUpdate       func() (*UpdateStatusMsg, error) // manual check — returns nil if up to date
	OnApplyUpdate       func(version string) string      // returns error string (empty on success, process exits)
	OnVerifySignature   func() error                     // verify current binary's signature

	// Cookie refresh callbacks
	OnRecheckCookies      func() (ytAuth bool, twAuth bool)
	OnForceRefreshCookies func() (ok bool, err error) // nil if auto-cookies not configured

	// FFmpeg check callbacks
	OnCheckFFmpeg    func(path string) (bool, string, string)                                    // check if ffmpeg path is valid → (valid, version, warning)
	OnCheckPrereqs   func() (bool, bool)                                                         // returns (chocoAvail, wingetAvail)
	OnPrepareInstall func(method string) (needsElevation bool, script, token string, err error)  // elevation check + prepare
	OnConfirmInstall func(token string) error                                                    // execute reviewed elevated install
	OnRejectInstall  func(token string)                                                          // decline pending elevated install

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
		taskList:      tl,
		details:       NewJobDetailsModel(),
		logs:          NewLogViewerModel(),
		statusBar:     NewStatusBarModel(),
		help:          NewHelpModel(),
		addVideo:      NewAddVideoModel(),
		importDlg:     NewImportDialogModel(),
		trimDlg:       NewTrimDialogModel(),
		filesDlg:        NewFilesDialogModel(),
		clientTokensDlg: NewClientTokensDialogModel(),
		setupWiz:        NewSetupWizardModel(),
		settings:      NewSettingsModel(),
		ffmpegCheck:   NewFFmpegCheckModel(),
		actionMenu:    NewActionMenuModel(),
		progressStore: ps,
		statusMap:     make(map[string]database.JobStatus),
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

// SetCfgMu sets the shared config mutex for synchronized config access.
// Propagates to sub-models that write config (settings).
func (a *App) SetCfgMu(mu *sync.RWMutex) {
	a.cfgMu = mu
	a.settings.cfgMu = mu
}

// SetSetupCallbacks wires callback functions for the TUI setup wizard.
func (a *App) SetSetupCallbacks(
	onComplete func(cfg *config.MoomboxConfig) error,
	onInstallYtdlp func(port int, httpsEnabled bool),
	onStartAutoCookie func(platform string) error,
	onFinishAutoCookie func() (bool, bool, error),
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
	jobUpdate <-chan *database.Job,
	jobsUpdate <-chan []*database.Job,
	logCh <-chan string,
	checkTimers <-chan CheckTimersMsg,
	cookieStatus <-chan CookieStatusMsg,
	diskStatus <-chan DiskStatusMsg,
	updateStatus <-chan UpdateStatusMsg,
) {
	a.jobUpdateCh = jobUpdate
	a.jobsUpdateCh = jobsUpdate
	a.logCh = logCh
	a.checkTimersCh = checkTimers
	a.cookieStatusCh = cookieStatus
	a.diskStatusCh = diskStatus
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

	return tea.Batch(a.tick(), a.progressTick(), a.logFlushTick(), a.marqueeTick(), a.listenForUpdates())
}

func (a *App) tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg{}
	})
}

func (a *App) progressTick() tea.Cmd {
	interval := 500 * time.Millisecond
	if a.hasActiveDownloads() || (a.trimInProgress && a.trimDlg.IsVisible()) {
		interval = 16 * time.Millisecond // ~60fps during active downloads
	}
	return tea.Tick(interval, func(t time.Time) tea.Msg {
		return progressTickMsg{}
	})
}

// logFlushTick returns a command that fires every 250ms for log batching (A2).
func (a *App) logFlushTick() tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(t time.Time) tea.Msg {
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
	if a.jobUpdateCh == nil && a.jobsUpdateCh == nil && a.logCh == nil &&
		a.checkTimersCh == nil && a.cookieStatusCh == nil && a.diskStatusCh == nil &&
		a.updateStatusCh == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case job, ok := <-a.jobUpdateCh:
			if !ok {
				return channelClosedMsg{Name: "jobUpdate"}
			}
			return JobUpdateMsg{Job: job}
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
	for _, s := range a.statusMap {
		switch s {
		case database.StatusDownloading, database.StatusLive, database.StatusMuxing:
			return true
		}
	}
	return false
}

// updateTerminalTitle returns a tea.Cmd that sets the terminal title with
// active/upcoming counts via BubbleTea's render pipeline (not direct stdout).
func (a *App) updateTerminalTitle() tea.Cmd {
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

	return tea.SetWindowTitle(title)
}

// getPort returns the configured port or default 774.
func (a *App) getPort() int {
	if a.cfg != nil {
		if a.cfgMu != nil {
			a.cfgMu.RLock()
			defer a.cfgMu.RUnlock()
		}
		if a.cfg.Network.Port > 0 {
			return a.cfg.Network.Port
		}
	}
	return 774
}
