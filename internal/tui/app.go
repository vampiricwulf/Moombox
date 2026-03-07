// Package tui provides the terminal user interface for Moombox using BubbleTea.
package tui

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/utils"
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
	tickMsg         struct{}
	progressTickMsg struct{}
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

	// Async results for AddVideo dialog
	fetchFormatsAutoAdvanceMsg struct{} // timer msg to auto-skip format on error
	addVideoResultMsg         struct {
		Feedback string
		IsError  bool
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
	cfg *config.MoomboxConfig

	// Internal token for CSRF bypass on local API calls
	internalToken string

	// Cached HTTP client for local API calls (avoids re-creating per request)
	cachedClient *http.Client

	// First-run flag: triggers setup wizard
	IsFirstRun bool

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
	OnCheckUpdate func() (*UpdateStatusMsg, error) // manual check — returns nil if up to date
	OnApplyUpdate func(version string) string      // returns error string (empty on success, process exits)

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

// SetSetupCallbacks wires callback functions for the TUI setup wizard.
func (a *App) SetSetupCallbacks(
	onComplete func(cfg *config.MoomboxConfig) error,
	onInstallYtdlp func(port int, httpsEnabled bool),
	onStartAutoCookie func(platform string) error,
	onFinishAutoCookie func() (bool, bool),
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
	return func() tea.Msg {
		select {
		case job, ok := <-a.jobUpdateCh:
			if ok {
				return JobUpdateMsg{Job: job}
			}
		case jobs, ok := <-a.jobsUpdateCh:
			if ok {
				return JobsUpdateMsg{Jobs: jobs}
			}
		case line, ok := <-a.logCh:
			if !ok {
				return nil
			}
			// Drain all pending log messages into a single batch to avoid
			// triggering a View() re-render per individual log line.
			batch := []string{line}
			for {
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
		case timers, ok := <-a.checkTimersCh:
			if ok {
				return timers
			}
		case cs, ok := <-a.cookieStatusCh:
			if ok {
				return cs
			}
		case ds, ok := <-a.diskStatusCh:
			if ok {
				return ds
			}
		case us, ok := <-a.updateStatusCh:
			if ok {
				return us
			}
		}
		return nil
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
		case database.StatusUpcoming, database.StatusDownloading, database.StatusLive, database.StatusMuxing:
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
	if a.cfg != nil && a.cfg.Network.Port > 0 {
		return a.cfg.Network.Port
	}
	return 774
}

// Update implements tea.Model.
func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width = msg.Width
		a.height = msg.Height
		a.recalcLayout()
		return a, nil

	case tickMsg:
		// Chord timeout
		if a.chord.prefix != "" {
			now := time.Now()
			if a.chord.action != "" && now.After(a.chord.actionTime.Add(3*time.Second)) {
				a.chord = chordState{}
			} else if a.chord.action == "" && now.After(a.chord.prefixTime.Add(3*time.Second)) {
				a.chord = chordState{}
			}
		}
		// Feedback message timeout
		if !a.feedbackTimer.IsZero() && time.Now().After(a.feedbackTimer) {
			a.feedbackMsg = ""
			a.feedbackTimer = time.Time{}
		}
		// Update terminal title
		return a, tea.Batch(a.updateTerminalTitle(), a.tick())

	case progressTickMsg:
		if a.hasActiveDownloads() {
			if sel := a.taskList.SelectedJob(); sel != nil {
				if p := a.progressStore.Get(sel.ID); p != nil {
					a.details.SetProgress(p)
				}
			}
		}
		// Update trim progress overlay if active
		if a.trimInProgress && a.trimDlg.IsVisible() {
			a.trimProgressMu.Lock()
			pct := a.trimProgressPct
			a.trimProgressMu.Unlock()
			elapsed := time.Since(a.trimStartedAt)
			a.trimDlg.SetProgress(pct, elapsed)
		}
		return a, a.progressTick()

	case logFlushMsg:
		// Flush buffered logs as batch (A2 - match TS concat batch)
		if len(a.logBuffer) > 0 {
			a.logs.AddLines(a.logBuffer)
			a.logBuffer = a.logBuffer[:0]
		}
		return a, a.logFlushTick()

	case marqueeTickMsg:
		a.taskList.marquee.Tick()
		a.details.marquee.Tick()
		return a, a.marqueeTick()

	case JobUpdateMsg:
		titleCmd := a.handleJobUpdate(msg.Job)
		return a, tea.Batch(titleCmd, a.listenForUpdates())

	case JobsUpdateMsg:
		// Structural change: clear and rebuild progress store + status map (match TS onJobsChange)
		a.progressStore.Clear()
		a.statusMap = make(map[string]database.JobStatus)
		for _, j := range msg.Jobs {
			a.statusMap[j.ID] = j.Status
			a.progressStore.Set(j.ID, &ProgressData{
				Progress:          j.Progress,
				Percent:           j.Percent,
				Speed:             j.Speed,
				ETA:               j.ETA,
				LastVideoSeq:      j.LastVideoSeq,
				LastAudioSeq:      j.LastAudioSeq,
				TotalVideoSeq:     j.TotalVideoSeq,
				TotalAudioSeq:     j.TotalAudioSeq,
				TotalChatMessages: j.TotalChatMessages,
				ChatStatus:        j.ChatStatus,
			})
		}
		a.taskList.SetJobs(msg.Jobs)
		a.statusBar.SetJobs(msg.Jobs)
		a.actionMenu.SetJobs(msg.Jobs)
		a.updateSelectedJob()
		// Refresh trim dialog's trim list if open (trims may have been added/deleted)
		if a.trimDlg.IsVisible() {
			for _, j := range msg.Jobs {
				if j.ID == a.trimDlg.JobID() {
					a.refreshTrimList(j)
					break
				}
			}
		}
		return a, tea.Batch(a.updateTerminalTitle(), a.listenForUpdates())

	case LogBatchMsg:
		// Batched log messages — single Update/View cycle for all pending logs
		a.logBuffer = append(a.logBuffer, msg.Lines...)
		return a, a.listenForUpdates()

	case CheckTimersMsg:
		if !msg.NextFeedCheck.IsZero() {
			a.taskList.NextFeedCheck = msg.NextFeedCheck
		}
		if !msg.NextDecapiCheck.IsZero() {
			a.taskList.NextDecapiCheck = msg.NextDecapiCheck
		}
		if !msg.NextTwitchCheck.IsZero() {
			a.taskList.NextTwitchCheck = msg.NextTwitchCheck
		}
		return a, a.listenForUpdates()

	case CookieStatusMsg:
		a.statusBar.SetCookieStatus(msg.YT, msg.TW)
		a.statusBar.SetActivePlatforms(msg.YTActive, msg.TWActive)
		return a, a.listenForUpdates()

	case DiskStatusMsg:
		a.statusBar.SetDiskStatus(msg.Free, msg.UsedPct, msg.Warn)
		return a, a.listenForUpdates()

	case UpdateStatusMsg:
		a.updateAvailable = &msg
		a.details.updateInfo = &msg
		return a, a.listenForUpdates()

	case updateCheckResultMsg:
		if msg.Err != "" {
			a.setFeedback("Update check failed: " + msg.Err)
		} else if msg.Info != nil {
			a.updateAvailable = msg.Info
			a.details.updateInfo = msg.Info
			a.setFeedback(fmt.Sprintf("Update available: %s — press R U to install", msg.Info.TagName))
		} else {
			a.setFeedback("Already up to date")
		}
		return a, nil

	case updateApplyResultMsg:
		if msg.Err != "" {
			a.setFeedback("Update failed: " + msg.Err)
		}
		// On success, the process is already exiting (QuitTUI was called)
		return a, nil

	case cookieRecheckResultMsg:
		var parts []string
		if a.statusBar.ytActive {
			if msg.YouTubeAuth {
				parts = append(parts, "YouTube OK")
			} else {
				parts = append(parts, "YouTube not authenticated")
			}
		}
		if a.statusBar.twActive {
			if msg.TwitchAuth {
				parts = append(parts, "Twitch OK")
			} else {
				parts = append(parts, "Twitch not authenticated")
			}
		}
		if len(parts) == 0 {
			a.setFeedback("Cookies: no platforms configured")
		} else {
			a.setFeedback("Cookies: " + strings.Join(parts, ", "))
		}
		return a, nil

	case cookieForceRefreshResultMsg:
		if msg.Err != nil {
			a.setFeedback("Browser cookie refresh failed: " + msg.Err.Error())
		} else if msg.Success {
			a.setFeedback("Browser cookie refresh successful")
		} else {
			a.setFeedback("Browser cookie refresh: no cookies acquired")
		}
		return a, nil

	case addVideoResultMsg:
		a.addVideo.Close()
		if msg.Feedback != "" {
			a.setFeedback(msg.Feedback)
		}
		return a, nil

	case fetchFormatsResultMsg:
		if msg.Err != "" {
			a.addVideo.SetError(msg.Err)
			// Auto-advance to confirmation after 2s on error (matching TS)
			return a, tea.Tick(2*time.Second, func(time.Time) tea.Msg {
				return fetchFormatsAutoAdvanceMsg{}
			})
		}
		a.addVideo.SetFormats(msg.Formats)
		return a, nil

	case fetchFormatsAutoAdvanceMsg:
		if a.addVideo.IsVisible() && a.addVideo.loading {
			// Skip to confirmation with auto settings
			a.addVideo.step = AddStepConfirm
			a.addVideo.advancedMode = false
			a.addVideo.loading = false
		}
		return a, nil

	case importResultMsg:
		if msg.Err != "" {
			a.importDlg.SetError(msg.Err)
			return a, nil
		}
		a.importDlg.Close()
		a.setFeedback("Imported: " + msg.Title)
		return a, nil

	case createTrimResultMsg:
		a.trimInProgress = false
		if a.trimDlg.IsVisible() {
			// Dialog still open — show result or error inline
			if msg.Err != "" {
				a.trimDlg.SetLoading(false)
				a.trimDlg.createStep = 1 // back to confirmation so user can retry or Esc
				a.trimDlg.SetError(msg.Err)
			} else {
				a.trimDlg.Close()
				a.setFeedback(fmt.Sprintf("Trim created: %s", msg.Filename))
			}
		} else {
			// Dialog was dismissed (background) — show feedback
			if msg.Err != "" {
				a.setFeedback(fmt.Sprintf("Trim failed: %s", msg.Err))
			} else {
				a.setFeedback(fmt.Sprintf("Trim created: %s", msg.Filename))
			}
		}
		return a, nil

	case deleteTrimResultMsg:
		a.trimDlg.SetLoading(false)
		if msg.Err != "" {
			a.trimDlg.SetError(msg.Err)
			return a, nil
		}
		// Remove the deleted trim from the dialog's list
		a.trimDlg.RemoveTrim(msg.TrimID)
		a.setFeedback(fmt.Sprintf("Trim deleted: %s", msg.Filename))
		return a, nil

	case fetchOrphansResultMsg:
		if msg.Err != "" {
			a.filesDlg.SetError(msg.Err)
			return a, nil
		}
		cmd := a.filesDlg.SetFiles(msg.Files)
		return a, cmd

	case deleteOrphanResultMsg:
		if msg.Err != "" {
			a.filesDlg.SetError(msg.Err)
		} else {
			a.filesDlg.RemoveFile(msg.Path)
			a.filesDlg.feedbackMsg = "Deleted"
		}
		return a, nil

	case fetchClientTokensResultMsg:
		if msg.Err != "" {
			a.clientTokensDlg.SetError(msg.Err)
			return a, nil
		}
		cmd := a.clientTokensDlg.SetTokens(msg.Tokens)
		return a, cmd

	case deleteClientTokenResultMsg:
		if msg.Err != "" {
			a.clientTokensDlg.SetError(msg.Err)
		} else {
			a.clientTokensDlg.RemoveToken(msg.ID)
			a.clientTokensDlg.feedbackMsg = "Revoked"
		}
		return a, nil

	case ffmpegPrepareResultMsg:
		if msg.Err != "" {
			a.ffmpegCheck.SetInstallResult(fmt.Sprintf("Install failed: %s", msg.Err), true)
		} else if msg.NeedsElevation {
			// Show script review mode
			a.ffmpegCheck.ShowReview(msg.Script, msg.Token)
		} else {
			// Ran directly (already elevated) — verify
			return a, a.ffmpegCheckCmd("")
		}
		return a, nil

	case ffmpegConfirmResultMsg:
		if msg.Err != "" {
			a.ffmpegCheck.SetInstallResult(fmt.Sprintf("Install failed: %s", msg.Err), true)
		} else {
			// Elevated install succeeded — verify FFmpeg is available
			return a, a.ffmpegCheckCmd("")
		}
		return a, nil

	case ffmpegCheckResultMsg:
		if msg.Valid {
			if a.ffmpegCheck.mode == ffmpegCustom {
				a.ffmpegCheck.SetCustomResult("Valid: "+msg.Version, true)
				// Persist the custom FFmpeg path to config
				if msg.Path != "" && a.cfg != nil {
					a.cfg.Paths.FfmpegPath = msg.Path
					if a.OnSaveConfig != nil {
						a.OnSaveConfig(a.cfg)
					}
				}
			} else if a.ffmpegCheck.mode == ffmpegManual {
				a.ffmpegCheck.SetManualResult("Valid: "+msg.Version, true)
				// Persist the custom FFmpeg path to config
				if msg.Path != "" && a.cfg != nil {
					a.cfg.Paths.FfmpegPath = msg.Path
					if a.OnSaveConfig != nil {
						a.OnSaveConfig(a.cfg)
					}
				}
			} else {
				a.ffmpegCheck.SetInstallResult("FFmpeg installed: "+msg.Version, false)
			}
			a.ffmpegCheck.warning = msg.Warning
			// Show success message — next keypress will dismiss the overlay
			a.ffmpegCheck.successDismiss = true
		} else {
			if a.ffmpegCheck.mode == ffmpegCustom {
				a.ffmpegCheck.SetCustomResult("Invalid: ffmpeg not found at this path", false)
			} else if a.ffmpegCheck.mode == ffmpegManual {
				a.ffmpegCheck.SetManualResult("Invalid: ffmpeg not found at this path", false)
			} else {
				a.ffmpegCheck.SetInstallResult("FFmpeg installed but not found on PATH. Restart may be needed.", true)
			}
		}
		return a, nil

	case channelResolvedMsg:
		a.settings.HandleChannelResolved(msg.ID, msg.Name, msg.Platform, msg.Err)
		return a, nil

	case setupCookieFinishMsg:
		if msg.Platform == "youtube" {
			a.setupWiz.cookieYTDone = msg.YTAuth
		} else {
			a.setupWiz.cookieTWDone = msg.TWAuth
		}
		a.setupWiz.cookieActive = false
		a.setupWiz.cookieFinishing = false
		a.setupWiz.cookiePlatform = ""
		return a, nil

	case tea.KeyMsg:
		var cmds []tea.Cmd
		if cmd := a.routeComponentMsg(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}
		_, cmd := a.handleKey(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		return a, tea.Batch(cmds...)

	case tea.MouseMsg:
		return a.handleMouse(msg)

	default:
		// Route spinner ticks and other component messages to active dialogs
		if cmd := a.routeComponentMsg(msg); cmd != nil {
			return a, cmd
		}
	}

	return a, nil
}

func (a *App) setFeedback(msg string) {
	a.feedbackMsg = msg
	a.feedbackTimer = time.Now().Add(3 * time.Second)
}

func (a *App) handleJobUpdate(job *database.Job) tea.Cmd {
	// Always update progress store (zero-cost)
	a.progressStore.Set(job.ID, &ProgressData{
		Progress:          job.Progress,
		Percent:           job.Percent,
		Speed:             job.Speed,
		ETA:               job.ETA,
		LastVideoSeq:      job.LastVideoSeq,
		LastAudioSeq:      job.LastAudioSeq,
		TotalVideoSeq:     job.TotalVideoSeq,
		TotalAudioSeq:     job.TotalAudioSeq,
		TotalChatMessages: job.TotalChatMessages,
		ChatStatus:        job.ChatStatus,
	})

	// Only re-sort/re-render if status changed
	prevStatus, exists := a.statusMap[job.ID]
	if exists && prevStatus == job.Status {
		return nil
	}

	a.statusMap[job.ID] = job.Status

	a.taskList.UpdateJob(job)
	if sel := a.taskList.SelectedJob(); sel != nil && sel.ID == job.ID {
		a.details.SetJob(job)
	}

	// Clean up progress for terminal/error statuses (match TS behavior)
	if isTerminalStatus(job.Status) || job.Status == database.StatusError {
		a.progressStore.Delete(job.ID)
	}

	// Update terminal title on status change (via BubbleTea render pipeline)
	return a.updateTerminalTitle()
}

func (a *App) updateSelectedJob() {
	a.details.SetJob(a.taskList.SelectedJob())
}

// routeComponentMsg forwards tea.Msg to the active dialog's embedded components
// (textinput, viewport). Called before handleKey so that text editing and
// viewport scrolling are processed before structural navigation.
func (a *App) routeComponentMsg(msg tea.Msg) tea.Cmd {
	if a.settings.IsVisible() {
		return a.settings.UpdateComponents(msg)
	}
	if a.setupWiz.IsVisible() {
		return a.setupWiz.UpdateComponents(msg)
	}
	if a.ffmpegCheck.IsVisible() {
		return a.ffmpegCheck.UpdateComponents(msg)
	}
	if a.importDlg.IsVisible() {
		return a.importDlg.UpdateComponents(msg)
	}
	if a.addVideo.IsVisible() {
		return a.addVideo.UpdateComponents(msg)
	}
	if a.trimDlg.IsVisible() {
		return a.trimDlg.UpdateComponents(msg)
	}
	if a.filesDlg.IsVisible() {
		return a.filesDlg.UpdateComponents(msg)
	}
	if a.clientTokensDlg.IsVisible() {
		return a.clientTokensDlg.UpdateComponents(msg)
	}
	// Help overlay viewport
	if a.help.IsVisible() {
		return a.help.UpdateViewport(msg)
	}
	// Panel viewports (when no dialog visible)
	switch a.focusedPanel {
	case PanelLogs:
		return a.logs.UpdateViewport(msg)
	case PanelDetails:
		return a.details.UpdateViewport(msg)
	}
	return nil
}

func (a *App) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// Settings panel intercepts all keys (before normalization to preserve case for text input)
	if a.settings.IsVisible() {
		action := a.settings.HandleKey(key)
		switch action {
		case "close":
			// Re-apply hide_finished_age_days in case it changed
			a.taskList.SetHideFinishedAgeDays(int(a.cfg.Monitors.HideFinishedAgeDays.Days()))
		case "restart":
			if a.OnRestart != nil {
				a.OnRestart()
			}
		case "resolve_channel":
			return a, a.resolveChannelCmd(a.settings.GetChannelResolveInput())
		}
		return a, nil
	}

	// Help overlay intercepts all keys (scroll handled by viewport in routeComponentMsg)
	if a.help.IsVisible() {
		switch key {
		case "?", keyEsc:
			a.help.Toggle()
		}
		return a, nil
	}

	// FFmpeg check overlay takes priority over all other dialogs
	if a.ffmpegCheck.IsVisible() {
		action := a.ffmpegCheck.HandleKey(key)
		switch {
		case action == "quit":
			return a, tea.Quit
		case strings.HasPrefix(action, "prepare:"):
			method := strings.TrimPrefix(action, "prepare:")
			return a, tea.Batch(a.ffmpegPrepareCmd(method), a.ffmpegCheck.spinner.Tick)
		case strings.HasPrefix(action, "confirm:"):
			token := strings.TrimPrefix(action, "confirm:")
			return a, tea.Batch(a.ffmpegConfirmCmd(token), a.ffmpegCheck.spinner.Tick)
		case strings.HasPrefix(action, "reject:"):
			token := strings.TrimPrefix(action, "reject:")
			if a.OnRejectInstall != nil {
				a.OnRejectInstall(token)
			}
			a.ffmpegCheck.ShowManual()
		case strings.HasPrefix(action, "check_custom:"):
			path := strings.TrimPrefix(action, "check_custom:")
			return a, a.ffmpegCheckCmd(path)
		}
		return a, nil
	}

	// Setup wizard
	if a.setupWiz.IsVisible() {
		action := a.setupWiz.HandleKey(key)
		if action == "complete" {
			a.setupWiz.Close()
		}
		var cmds []tea.Cmd
		if action == "finish_cookie" {
			// Run cookie extraction async so TUI doesn't freeze
			platform := a.setupWiz.cookiePlatform
			cmds = append(cmds, func() tea.Msg {
				yt, tw := false, false
				if a.setupWiz.OnFinishAutoCookie != nil {
					yt, tw = a.setupWiz.OnFinishAutoCookie()
				}
				return setupCookieFinishMsg{Platform: platform, YTAuth: yt, TWAuth: tw}
			})
		}
		if a.setupWiz.cookieActive {
			cmds = append(cmds, a.setupWiz.spinner.Tick)
		}
		// Deliver pending huh form init cmd immediately (cursor blink, focus)
		if a.setupWiz.advancedInitCmd != nil {
			cmds = append(cmds, a.setupWiz.advancedInitCmd)
			a.setupWiz.advancedInitCmd = nil
		}
		return a, tea.Batch(cmds...)
	}

	// Action menu intercepts
	if a.actionMenu.IsVisible() {
		action := a.actionMenu.HandleKey(key)
		if action == "close" {
			return a, nil
		}
		if action != "" {
			a.actionMenu.Close()
			// Parse "CHORD" or "CHORD:jobID"
			chord := action
			var job *database.Job
			if idx := strings.Index(action, ":"); idx >= 0 {
				chord = action[:idx]
				jobID := action[idx+1:]
				for _, j := range a.actionMenu.jobs {
					if j.ID == jobID {
						job = j
						break
					}
				}
			}
			return a.dispatchAction(chord, job)
		}
		return a, nil
	}

	// Dialog intercepts
	if a.importDlg.IsVisible() {
		action, data := a.importDlg.HandleKey(key)
		if action == "import" {
			return a, tea.Batch(a.importFileCmd(data), a.importDlg.SpinnerInit())
		}
		return a, nil
	}
	if a.addVideo.IsVisible() {
		action, data := a.addVideo.HandleKey(key)
		switch action {
		case "submit":
			return a, a.addVideoCmd(data)
		case "fetch_formats":
			return a, tea.Batch(a.fetchFormatsCmd(data), a.addVideo.SpinnerInit())
		}
		return a, nil
	}
	if a.trimDlg.IsVisible() {
		action := a.trimDlg.HandleKey(key)
		switch action {
		case "submit":
			if a.OnCreateTrim != nil {
				a.trimDlg.StartProgress()
				a.trimInProgress = true
				a.trimStartedAt = time.Now()
				a.trimProgressMu.Lock()
				a.trimProgressPct = 0
				a.trimProgressMu.Unlock()
				jobID := a.trimDlg.JobID()
				startSec := a.trimDlg.ParsedStartSeconds()
				endSec := a.trimDlg.ParsedEndSeconds()
				return a, tea.Batch(a.createTrimCmd(jobID, startSec, endSec), a.trimDlg.spinner.Tick)
			}
		case "background":
			a.setFeedback("Trim encoding in background...")
		case "delete":
			if a.OnDeleteTrim != nil {
				trimID := a.trimDlg.SelectedTrimID()
				if trimID != "" {
					a.trimDlg.SetLoading(true)
					jobID := a.trimDlg.JobID()
					return a, tea.Batch(a.deleteTrimCmd(jobID, trimID), a.trimDlg.spinner.Tick)
				}
			}
		}
		return a, nil
	}
	if a.filesDlg.IsVisible() {
		action, cmd := a.filesDlg.HandleKey(msg)
		switch action {
		case "refresh":
			return a, tea.Batch(a.fetchOrphansCmd(), a.filesDlg.SpinnerInit())
		case "delete":
			if sel := a.filesDlg.SelectedFile(); sel != nil {
				return a, a.deleteOrphanCmd(sel.Path)
			}
		}
		if cmd != nil {
			return a, cmd
		}
		return a, nil
	}
	if a.clientTokensDlg.IsVisible() {
		action, cmd := a.clientTokensDlg.HandleKey(msg)
		switch action {
		case "refresh":
			return a, tea.Batch(a.fetchClientTokensCmd(), a.clientTokensDlg.SpinnerInit())
		case "revoke":
			if sel := a.clientTokensDlg.SelectedToken(); sel != nil {
				return a, a.deleteClientTokenCmd(sel.ID)
			}
		}
		if cmd != nil {
			return a, cmd
		}
		return a, nil
	}

	// Normalize single-character keys to lowercase (match TS: accepts both d/D, c/C, etc.)
	// Done AFTER dialog intercepts so text inputs preserve case.
	if len(key) == 1 && key[0] >= 'A' && key[0] <= 'Z' {
		key = strings.ToLower(key)
	}

	// Ctrl+C: immediate quit (bypass chord)
	if key == keyCtrlC {
		return a, tea.Quit
	}

	// Chord system
	if model, cmd, handled := a.handleChord(key); handled {
		return model, cmd
	}

	// Single-press keys
	switch key {
	case "f":
		a.handleFilter()
		return a, nil
	case "m":
		a.actionMenu.SetSize(a.width, a.height)
		a.actionMenu.Open(a.buildMenuItems())
		return a, nil
	case "?":
		a.help.SetMenuItems(a.buildMenuItems())
		a.help.Toggle()
		return a, nil
	case "`":
		if a.cfg != nil {
			a.settings.SetSize(a.width, a.height)
			a.settings.OnSave = a.OnSaveConfig
			a.settings.OnRestart = a.OnRestart
			a.settings.OnHashPassword = a.OnHashPassword
			a.settings.OnVerifyPassword = a.OnVerifyPassword
			a.settings.Open(a.cfg)
		}
		return a, nil
	case keyTab:
		a.cycleFocus()
		return a, nil
	}

	// Panel navigation
	switch a.focusedPanel {
	case PanelTasks:
		return a.handleTaskKey(key)
	case PanelDetails:
		return a.handleDetailKey(key)
	case PanelLogs:
		return a.handleLogKey(key)
	}

	return a, nil
}

func (a *App) handleTaskKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case keyUp:
		a.taskList.MoveUp()
		a.updateSelectedJob()
	case keyDown:
		a.taskList.MoveDown()
		a.updateSelectedJob()
	case keyEnter:
		if a.taskList.SelectedIsDivider() {
			a.taskList.ToggleArchive()
		}
	}
	return a, nil
}

func (a *App) handleDetailKey(key string) (tea.Model, tea.Cmd) {
	// Scroll handled by viewport in routeComponentMsg
	return a, nil
}

func (a *App) handleLogKey(key string) (tea.Model, tea.Cmd) {
	// Scroll handled by viewport in routeComponentMsg
	return a, nil
}

// handleFilter applies the F key action based on the focused panel.
func (a *App) handleFilter() {
	switch a.focusedPanel {
	case PanelTasks:
		a.taskList.CycleFilter()
	case PanelDetails:
		a.details.ToggleDescription()
	case PanelLogs:
		a.logs.CycleLevel()
	}
}

// handleChord processes the chord state machine. Returns (model, cmd, handled).
func (a *App) handleChord(key string) (tea.Model, tea.Cmd, bool) {
	now := time.Now()

	// Check timeout on active chord
	if a.chord.prefix != "" {
		if a.chord.action != "" && now.After(a.chord.actionTime.Add(3*time.Second)) {
			a.chord = chordState{}
		} else if a.chord.action == "" && now.After(a.chord.prefixTime.Add(3*time.Second)) {
			a.chord = chordState{}
		}
	}

	// Confirm step: waiting for third key press
	if a.chord.prefix != "" && a.chord.action != "" {
		if key == a.chord.action {
			// Confirmed — build chord, resolve job if needed, execute
			chord := strings.ToUpper(a.chord.prefix) + " " + strings.ToUpper(a.chord.action)
			var job *database.Job
			// Look up the item to check if it needs a job, re-validate filter
			for _, item := range a.buildMenuItems() {
				if item.Chord == chord && item.NeedsJob {
					job = a.taskList.SelectedJob()
					if job != nil && item.JobFilter != nil && !item.JobFilter(job) {
						job = nil // job status changed during confirm window
					}
					break
				}
			}
			a.chord = chordState{}
			m, cmd := a.dispatchAction(chord, job)
			return m, cmd, true
		}
		// Wrong key — reset and fall through to check if key starts a new chord
		a.chord = chordState{}
	}

	// Active prefix, waiting for second key
	if a.chord.prefix != "" {
		m, cmd, handled := a.processSecondKey(a.chord.prefix, key)
		if handled {
			return m, cmd, true
		}
		// Not a valid second key — reset and fall through to check if key starts a new chord
		a.chord = chordState{}
	}

	// No active chord — check if key is a prefix
	switch key {
	case "a", "r", "o", "q":
		a.chord = chordState{prefix: key, prefixTime: now}
		a.setFeedback(a.chordFeedback(key))
		return a, nil, true
	}

	return a, nil, false
}

// processSecondKey handles the second key in a chord sequence.
// It looks up the chord in buildMenuItems() instead of hardcoding valid keys.
func (a *App) processSecondKey(prefix, key string) (tea.Model, tea.Cmd, bool) {
	// Special case: Q Q (quit) is not in buildMenuItems the same way
	if prefix == "q" && key == "q" {
		return a, tea.Quit, true
	}

	// Form the full chord string: prefix "a" + key "k" → "A K"
	chord := strings.ToUpper(prefix) + " " + strings.ToUpper(key)
	items := a.buildMenuItems()

	// Find matching item
	var item *ActionMenuItem
	for i := range items {
		if items[i].Chord == chord {
			item = &items[i]
			break
		}
	}
	if item == nil {
		return a, nil, false
	}

	// NeedsConfirm + NeedsJob: check job first, then enter confirm step
	if item.NeedsConfirm && item.NeedsJob {
		job := a.taskList.SelectedJob()
		if job == nil || (item.JobFilter != nil && !item.JobFilter(job)) {
			return a, nil, true // no valid job — consume key but do nothing
		}
		a.chord.action = key
		a.chord.actionTime = time.Now()
		a.setFeedback(fmt.Sprintf("Press %s to confirm %s \"%s\" (3s)",
			strings.ToUpper(key), strings.ToLower(item.HintLabel), job.Title))
		return a, nil, true
	}

	// NeedsConfirm (no job): enter confirm step
	if item.NeedsConfirm {
		a.chord.action = key
		a.chord.actionTime = time.Now()
		a.setFeedback(fmt.Sprintf("Press %s to confirm %s (3s)",
			strings.ToUpper(key), strings.ToLower(item.HintLabel)))
		return a, nil, true
	}

	// NeedsJob (no confirm): use selected job, check filter
	if item.NeedsJob {
		job := a.taskList.SelectedJob()
		if job == nil || (item.JobFilter != nil && !item.JobFilter(job)) {
			return a, nil, true // no valid job — consume key but do nothing
		}
		a.chord = chordState{}
		m, cmd := a.dispatchAction(chord, job)
		return m, cmd, true
	}

	// Direct action (no job, no confirm)
	a.chord = chordState{}
	m, cmd := a.dispatchAction(chord, nil)
	return m, cmd, true
}

// dispatchAction executes a chord action. For job-specific actions, job comes from
// the selected task (keyboard chords) or from the menu's job picker (menu flow).
func (a *App) dispatchAction(chord string, job *database.Job) (tea.Model, tea.Cmd) {
	switch chord {
	case "A A":
		a.feedbackMsg = ""
		a.addVideo.SetSize(a.width, a.height)
		a.addVideo.Open()
	case "A I":
		a.feedbackMsg = ""
		a.importDlg.SetSize(a.width, a.height)
		startDir := "."
		if a.cfg != nil && a.cfg.Paths.OutputDirectory != "" {
			startDir = a.cfg.Paths.OutputDirectory
		}
		return a, a.importDlg.Open(startDir)
	case "A R":
		if job != nil && a.OnRetryJob != nil {
			a.OnRetryJob(job.ID)
			a.setFeedback(fmt.Sprintf("Retrying: %s", job.Title))
		}
	case "A C":
		if job != nil && a.OnCancelJob != nil {
			a.OnCancelJob(job.ID)
			a.setFeedback(fmt.Sprintf("Cancelled: %s", job.Title))
		}
	case "A D":
		if job != nil && a.OnDeleteJob != nil {
			a.OnDeleteJob(job.ID)
			a.setFeedback(fmt.Sprintf("Deleted: %s", job.Title))
			a.taskList.MoveUp()
		}
	case "A T":
		if a.trimInProgress {
			a.setFeedback("A trim is already in progress")
			return a, nil
		}
		if job != nil {
			a.openTrimForJob(job)
		}
	case "A O":
		a.filesDlg.SetSize(a.width, a.height)
		a.filesDlg.Open()
		return a, tea.Batch(a.fetchOrphansCmd(), a.filesDlg.SpinnerInit())
	case "A K":
		if a.OnListClientTokens != nil {
			a.clientTokensDlg.SetSize(a.width, a.height)
			a.clientTokensDlg.Open()
			return a, tea.Batch(a.fetchClientTokensCmd(), a.clientTokensDlg.SpinnerInit())
		}
	case "R C":
		if a.OnRecheckCookies != nil {
			a.setFeedback("Rechecking cookies...")
			recheckFn := a.OnRecheckCookies
			return a, func() tea.Msg {
				ytAuth, twAuth := recheckFn()
				return cookieRecheckResultMsg{YouTubeAuth: ytAuth, TwitchAuth: twAuth}
			}
		}
	case "R F":
		if a.OnForceRefreshCookies != nil {
			a.setFeedback("Running browser cookie refresh...")
			refreshFn := a.OnForceRefreshCookies
			return a, func() tea.Msg {
				ok, err := refreshFn()
				return cookieForceRefreshResultMsg{Success: ok, Err: err}
			}
		}
	case "R V":
		if a.OnCheckUpdate != nil {
			a.setFeedback("Checking for updates...")
			return a, func() tea.Msg {
				info, err := a.OnCheckUpdate()
				if err != nil {
					return updateCheckResultMsg{Err: err.Error()}
				}
				return updateCheckResultMsg{Info: info}
			}
		}
	case "R U":
		if a.updateAvailable != nil && a.OnApplyUpdate != nil {
			a.setFeedback(fmt.Sprintf("Updating to %s...", a.updateAvailable.TagName))
			ver := a.updateAvailable.Version
			return a, func() tea.Msg {
				errStr := a.OnApplyUpdate(ver)
				return updateApplyResultMsg{Err: errStr}
			}
		}
		a.setFeedback("No update available — use R V to check")
	case "R P":
		if a.OnRestart != nil {
			a.OnRestart()
		}
	case "O F":
		if job != nil && a.OnOpenFolder != nil {
			a.OnOpenFolder(job.ID)
			a.setFeedback(fmt.Sprintf("Opening folder for: %s", job.Title))
		}
	case "O S":
		if job != nil {
			if url := streamURL(job); url != "" {
				openBrowser(url)
				a.setFeedback("Opening: " + url)
			}
		}
	case "O W":
		scheme := "http"
		if a.cfg != nil && a.cfg.Network.HTTPSEnabled {
			scheme = "https"
		}
		url := fmt.Sprintf("%s://localhost:%d", scheme, a.getPort())
		a.setFeedback(fmt.Sprintf("Opening: %s", url))
		openBrowser(url)
	case "F":
		a.handleFilter()
	case "`":
		if a.cfg != nil {
			a.settings.SetSize(a.width, a.height)
			a.settings.OnSave = a.OnSaveConfig
			a.settings.OnRestart = a.OnRestart
			a.settings.OnHashPassword = a.OnHashPassword
			a.settings.OnVerifyPassword = a.OnVerifyPassword
			a.settings.Open(a.cfg)
		}
	case "?":
		a.help.SetMenuItems(a.buildMenuItems())
		a.help.Toggle()
	case "Q Q":
		return a, tea.Quit
	}
	return a, nil
}

// chordFeedback builds contextual feedback for a chord prefix.
// It derives available options from buildMenuItems() using HintLabel.
func (a *App) chordFeedback(prefix string) string {
	if prefix == "q" {
		return "Quit: Q Confirm (3s)"
	}

	upperPrefix := strings.ToUpper(prefix)
	items := a.buildMenuItems()
	job := a.taskList.SelectedJob()

	var parts []string
	for i := range items {
		item := &items[i]
		// Match items whose chord starts with this prefix (e.g. "A " for prefix "a")
		if !strings.HasPrefix(item.Chord, upperPrefix+" ") {
			continue
		}
		// For NeedsJob items, check if selected job passes the filter
		if item.NeedsJob && (job == nil || (item.JobFilter != nil && !item.JobFilter(job))) {
			continue
		}
		// Extract the second key character from the chord (e.g. "A K" → "K")
		secondKey := item.Chord[len(upperPrefix)+1:]
		parts = append(parts, secondKey+" "+item.HintLabel)
	}

	// Category label from prefix
	var label string
	switch prefix {
	case "a":
		label = "Action"
	case "r":
		label = "Request"
	case "o":
		label = "Open"
	default:
		label = strings.ToUpper(prefix)
	}

	if len(parts) == 0 {
		return label + ": (none available) (3s)"
	}
	return label + ": " + strings.Join(parts, " | ") + " (3s)"
}

// canOpenFolder returns true if the job's folder can be opened.
func canOpenFolder(j *database.Job) bool {
	switch j.Status {
	case database.StatusFinished:
		return j.OutputFile != ""
	case database.StatusUpcoming, database.StatusLive, database.StatusDownloading, database.StatusMuxing:
		return true
	}
	return false
}

// canOpenStream returns true if the job has a stream URL to open.
func canOpenStream(j *database.Job) bool {
	return j.URL != "" || j.VideoID != ""
}

// streamURL returns the stream page URL for a job, or "" if unavailable.
func streamURL(j *database.Job) string {
	if j.URL != "" {
		return j.URL
	}
	if j.VideoID == "" {
		return ""
	}
	if j.Platform == "twitch" {
		if j.IsVod {
			return "https://www.twitch.tv/videos/" + j.VideoID
		}
		return "https://www.twitch.tv/" + j.ChannelName
	}
	return "https://www.youtube.com/watch?v=" + j.VideoID
}

// openTrimForJob opens the trim dialog for a specific job.
func (a *App) openTrimForJob(job *database.Job) {
	if job.Status == database.StatusFinished && job.OutputFile != "" {
		a.trimDlg.SetSize(a.width, a.height)
		a.trimDlg.Open(job.ID, job.Title)
		var lenSec float64
		var fSize int64
		if job.LengthSeconds != nil {
			lenSec = float64(*job.LengthSeconds)
		}
		if job.FileSize != nil {
			fSize = *job.FileSize
		}
		a.trimDlg.SetJobMetadata(lenSec, fSize)
		var trimInfos []TrimInfo
		for _, tr := range job.Trims {
			var fs int64
			if tr.FileSize != nil {
				fs = *tr.FileSize
			}
			trimInfos = append(trimInfos, TrimInfo{
				ID:        tr.ID,
				StartTime: tr.StartTime,
				EndTime:   tr.EndTime,
				Duration:  tr.Duration,
				FileSize:  fs,
				Filename:  tr.Filename,
			})
		}
		a.trimDlg.SetTrims(trimInfos)
	} else {
		a.setFeedback("Trim only available for finished jobs with files")
	}
}

// refreshTrimList updates the trim dialog's trim list without resetting dialog state.
func (a *App) refreshTrimList(job *database.Job) {
	var trimInfos []TrimInfo
	for _, tr := range job.Trims {
		var fs int64
		if tr.FileSize != nil {
			fs = *tr.FileSize
		}
		trimInfos = append(trimInfos, TrimInfo{
			ID:        tr.ID,
			StartTime: tr.StartTime,
			EndTime:   tr.EndTime,
			Duration:  tr.Duration,
			FileSize:  fs,
			Filename:  tr.Filename,
		})
	}
	a.trimDlg.SetTrims(trimInfos)
}

// buildMenuItems builds context-sensitive action menu items.
// This is the single source of truth for all chords, menu entries, feedback hints, and help text.
func (a *App) buildMenuItems() []ActionMenuItem {
	items := []ActionMenuItem{
		{Chord: "A A", Label: "Add Video", HintLabel: "Add", Category: "Action"},
		{Chord: "A I", Label: "Import Archive", HintLabel: "Import", Category: "Action"},
		{Chord: "A R", Label: "Retry Job", HintLabel: "Retry", Category: "Action", NeedsJob: true,
			JobFilter: func(j *database.Job) bool {
				return j.Status == database.StatusError || j.Status == database.StatusCancelled || j.Status == database.StatusCookies
			}},
		{Chord: "A C", Label: "Cancel Job", HintLabel: "Cancel", Category: "Action", NeedsJob: true, NeedsConfirm: true,
			JobFilter: func(j *database.Job) bool {
				return j.Status != database.StatusFinished && j.Status != database.StatusCancelled && j.Status != database.StatusError
			}},
		{Chord: "A D", Label: "Delete Job", HintLabel: "Delete", Category: "Action", NeedsJob: true, NeedsConfirm: true},
		{Chord: "A T", Label: "Trim Video", HintLabel: "Trim", Category: "Action", NeedsJob: true,
			JobFilter: func(j *database.Job) bool {
				return j.Status == database.StatusFinished && j.OutputFile != ""
			}},
		{Chord: "A O", Label: "Browse Orphaned Files", HintLabel: "Orphans", Category: "Action"},
	}

	if a.OnListClientTokens != nil {
		items = append(items, ActionMenuItem{Chord: "A K", Label: "Manage Client Tokens", HintLabel: "Tokens", Category: "Action"})
	}

	// Request — conditional on callbacks being set
	if a.OnRecheckCookies != nil {
		items = append(items, ActionMenuItem{Chord: "R C", Label: "Recheck Cookies", HintLabel: "Cookies", Category: "Request"})
	}
	if a.OnForceRefreshCookies != nil {
		items = append(items, ActionMenuItem{Chord: "R F", Label: "Force Cookie Refresh", HintLabel: "Force Refresh", Category: "Request"})
	}
	if a.OnCheckUpdate != nil {
		items = append(items, ActionMenuItem{Chord: "R V", Label: "Check for Updates", HintLabel: "Version", Category: "Request"})
	}
	if a.updateAvailable != nil && a.OnApplyUpdate != nil {
		items = append(items, ActionMenuItem{Chord: "R U", Label: "Apply Update " + a.updateAvailable.TagName, HintLabel: "Update", Category: "Request"})
	}
	if a.OnRestart != nil {
		items = append(items, ActionMenuItem{Chord: "R P", Label: "Restart Program", HintLabel: "Restart", Category: "Request", NeedsConfirm: true})
	}

	// Open
	items = append(items,
		ActionMenuItem{Chord: "O F", Label: "Open Folder", HintLabel: "Folder", Category: "Open", NeedsJob: true,
			JobFilter: func(j *database.Job) bool { return canOpenFolder(j) }},
		ActionMenuItem{Chord: "O S", Label: "Open Stream Page", HintLabel: "Stream", Category: "Open", NeedsJob: true,
			JobFilter: func(j *database.Job) bool { return canOpenStream(j) }},
		ActionMenuItem{Chord: "O W", Label: "Open Web UI", HintLabel: "Web", Category: "Open"},
	)

	// Filter + Other
	items = append(items,
		ActionMenuItem{Chord: "F", Label: "Cycle Filter", HintLabel: "Filter", Category: "Filter"},
		ActionMenuItem{Chord: "`", Label: "Settings", HintLabel: "Settings", Category: "Other"},
		ActionMenuItem{Chord: "?", Label: "Help", HintLabel: "Help", Category: "Other"},
		ActionMenuItem{Chord: "Q Q", Label: "Quit", HintLabel: "Quit", Category: "Other"},
	)
	return items
}

func (a *App) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	// Route mouse to action menu when visible
	if a.actionMenu.IsVisible() {
		a.actionMenu.HandleMouse(msg)
		return a, nil
	}

	if a.hasActiveOverlay() {
		return a, nil
	}

	x, y := msg.X, msg.Y

	if isLeftClick(msg) {
		prevPanel := a.focusedPanel
		switch {
		case a.taskRegion.Contains(x, y):
			a.setFocus(PanelTasks)
			// Re-enable log auto-scroll when clicking away from logs (A8)
			if prevPanel == PanelLogs {
				a.logs.ReEnableAutoScroll()
			}
			contentY := a.taskRegion.ContentY(y) - 1 // additional -1 for header row (TS: y - 2)
			if contentY >= 0 && a.taskList.SelectAtOffset(contentY) {
				if a.taskList.SelectedIsDivider() {
					a.taskList.ToggleArchive()
				} else {
					a.updateSelectedJob()
				}
			}
		case a.detailRegion.Contains(x, y):
			a.setFocus(PanelDetails)
			// Re-enable log auto-scroll when clicking away from logs (A8)
			if prevPanel == PanelLogs {
				a.logs.ReEnableAutoScroll()
			}
		case a.logRegion.Contains(x, y):
			a.setFocus(PanelLogs)
		}
		return a, nil
	}

	if isScrollUp(msg) || isScrollDown(msg) {
		dir := 1
		if isScrollUp(msg) {
			dir = -1
		}

		switch a.focusedPanel {
		case PanelTasks:
			for range 3 {
				if dir < 0 {
					a.taskList.MoveUp()
				} else {
					a.taskList.MoveDown()
				}
			}
			a.updateSelectedJob()
		case PanelDetails:
			for range 3 {
				if dir < 0 {
					a.details.ScrollUp()
				} else {
					a.details.ScrollDown()
				}
			}
		case PanelLogs:
			for range 3 {
				if dir < 0 {
					a.logs.ScrollUp()
				} else {
					a.logs.ScrollDown()
				}
			}
		}
		return a, nil
	}

	return a, nil
}

func (a *App) cycleFocus() {
	prevPanel := a.focusedPanel
	a.focusedPanel = (a.focusedPanel + 1) % 3
	a.taskList.SetFocused(a.focusedPanel == PanelTasks)
	a.details.SetFocused(a.focusedPanel == PanelDetails)
	a.logs.SetFocused(a.focusedPanel == PanelLogs)
	// Re-enable log auto-scroll when tabbing away from logs (match TS)
	if prevPanel == PanelLogs && a.focusedPanel != PanelLogs {
		a.logs.ReEnableAutoScroll()
	}
	a.recalcLayout()
}

func (a *App) setFocus(panel FocusPanel) {
	a.focusedPanel = panel
	a.taskList.SetFocused(panel == PanelTasks)
	a.details.SetFocused(panel == PanelDetails)
	a.logs.SetFocused(panel == PanelLogs)
	a.recalcLayout()
}

func (a *App) recalcLayout() {
	// Status bar is 1 row at the bottom
	contentH := a.height - 1

	// Top panels: 70% focused, 25% unfocused (A4 - match TypeScript)
	var topH, logH int
	if a.focusedPanel == PanelLogs {
		topH = contentH * 25 / 100
		logH = contentH - topH
	} else {
		topH = contentH * 70 / 100
		logH = contentH - topH
	}

	// Task list vs details width split (A5 - match TypeScript)
	var taskW, detailW int
	if a.focusedPanel == PanelTasks {
		taskW = a.width * 45 / 100 // TS: 45% for tasks when focused
		detailW = a.width - taskW
	} else if a.focusedPanel == PanelDetails {
		taskW = a.width * 35 / 100
		detailW = a.width - taskW
	} else {
		taskW = a.width / 2
		detailW = a.width - taskW
	}

	a.taskList.SetSize(taskW, topH)
	a.details.SetSize(detailW, topH)
	a.logs.SetSize(a.width, logH)
	a.statusBar.SetWidth(a.width)
	a.help.SetSize(a.width, a.height)
	a.addVideo.SetSize(a.width, a.height)
	a.trimDlg.SetSize(a.width, a.height)
	a.filesDlg.SetSize(a.width, a.height)
	a.clientTokensDlg.SetSize(a.width, a.height)
	a.setupWiz.SetSize(a.width, a.height)
	a.ffmpegCheck.SetSize(a.width, a.height)
	a.settings.SetSize(a.width, a.height)

	// Store regions for mouse
	a.taskRegion = PanelRegion{X: 0, Y: 0, Width: taskW, Height: topH}
	a.detailRegion = PanelRegion{X: taskW, Y: 0, Width: detailW, Height: topH}
	a.logRegion = PanelRegion{X: 0, Y: topH, Width: a.width, Height: logH}
}

// View implements tea.Model.
func (a *App) View() string {
	if a.width == 0 || a.height == 0 {
		return "Initializing..."
	}

	// Render overlays on top (priority matches handleKey order)
	if a.settings.IsVisible() {
		return a.settings.View()
	}
	if a.help.IsVisible() {
		return a.help.View()
	}
	if a.ffmpegCheck.IsVisible() {
		return a.ffmpegCheck.View()
	}
	if a.setupWiz.IsVisible() {
		return a.setupWiz.View()
	}
	if a.actionMenu.IsVisible() {
		return a.actionMenu.View()
	}
	if a.importDlg.IsVisible() {
		return a.importDlg.View()
	}
	if a.addVideo.IsVisible() {
		return a.addVideo.View()
	}
	if a.trimDlg.IsVisible() {
		return a.trimDlg.View()
	}
	if a.filesDlg.IsVisible() {
		return a.filesDlg.View()
	}
	if a.clientTokensDlg.IsVisible() {
		return a.clientTokensDlg.View()
	}

	// Top row: task list + details
	topRow := lipgloss.JoinHorizontal(lipgloss.Top,
		a.taskList.View(),
		a.details.View(),
	)

	// Main content
	content := lipgloss.JoinVertical(lipgloss.Left,
		topRow,
		a.logs.View(),
		a.statusBar.View(),
	)

	// Feedback / confirmation messages
	if a.feedbackMsg != "" {
		msgColor := ColorGreen
		if strings.HasPrefix(a.feedbackMsg, "Press ") || strings.HasPrefix(a.feedbackMsg, "Action:") ||
			strings.HasPrefix(a.feedbackMsg, "Request:") || strings.HasPrefix(a.feedbackMsg, "Open:") ||
			strings.HasPrefix(a.feedbackMsg, "Quit:") {
			msgColor = lipgloss.Color("#f1c40f") // yellow for chord feedback
		} else if strings.HasPrefix(a.feedbackMsg, "Can only") || strings.HasPrefix(a.feedbackMsg, "Trim only") {
			msgColor = lipgloss.Color("#f1c40f") // yellow for warnings
		} else if strings.HasPrefix(a.feedbackMsg, "No update") {
			msgColor = lipgloss.Color("#f1c40f")
		} else if strings.HasPrefix(a.feedbackMsg, "Cancelled:") {
			msgColor = ColorRed
		} else if strings.Contains(a.feedbackMsg, "Deleted:") {
			msgColor = ColorGray
		}
		content = addOverlayMessage(content, a.width,
			lipgloss.NewStyle().Foreground(msgColor).Render(a.feedbackMsg),
		)
	}

	return content
}

func addOverlayMessage(content string, width int, msg string) string {
	lines := strings.Split(content, "\n")
	if len(lines) > 2 {
		idx := len(lines) - 2
		padded := "  " + msg
		paddedW := lipgloss.Width(padded)
		lines[idx] = padded + strings.Repeat(" ", max(0, width-paddedW))
	}
	return strings.Join(lines, "\n")
}

// apiPort returns the port for local API calls.
func (a *App) apiPort() int {
	if a.cfg != nil && a.cfg.Network.Port > 0 {
		return a.cfg.Network.Port
	}
	return 774
}

// apiBaseURL returns the correct scheme + host for local API calls.
func (a *App) apiBaseURL() string {
	scheme := "http"
	if a.cfg != nil && a.cfg.Network.HTTPSEnabled {
		scheme = "https"
	}
	return fmt.Sprintf("%s://127.0.0.1:%d", scheme, a.apiPort())
}

// internalTokenTransport injects the X-Internal-Token header on every request
// so that local API calls bypass the CSRF middleware.
type internalTokenTransport struct {
	base  http.RoundTripper
	token string
}

func (t *internalTokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.token != "" {
		req.Header.Set("X-Internal-Token", t.token)
	}
	return t.base.RoundTrip(req)
}

// apiClient returns an HTTP client suitable for local API calls.
// Injects the internal CSRF bypass token. When HTTPS is enabled, TLS
// verification is skipped since the server uses a self-signed certificate.
// The client is cached on first call and reused for all subsequent requests.
func (a *App) apiClient() *http.Client {
	if a.cachedClient != nil {
		return a.cachedClient
	}
	base := http.DefaultTransport
	if a.cfg != nil && a.cfg.Network.HTTPSEnabled {
		base = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	a.cachedClient = &http.Client{
		Transport: &internalTokenTransport{base: base, token: a.internalToken},
	}
	return a.cachedClient
}

// addVideoCmd creates a job by POSTing to the local API (matches TS TUI behavior).
func (a *App) addVideoCmd(input string) tea.Cmd {
	platform := a.addVideo.GetPlatform()
	videoItag := a.addVideo.GetSelectedVideoItag()
	audioItag := a.addVideo.GetSelectedAudioItag()
	startTime := a.addVideo.GetStartTime()
	endTime := a.addVideo.GetEndTime()
	baseURL := a.apiBaseURL()
	client := a.apiClient()

	// Fire-and-forget OnAddVideo callback for logging
	if a.OnAddVideo != nil {
		a.OnAddVideo(input)
	}

	return func() tea.Msg {
		body := map[string]any{
			"videoId": input,
		}
		if platform == "twitch" {
			body["platform"] = "twitch"
			// Detect VOD vs channel
			if strings.HasPrefix(input, "tw_v") {
				body["twitchType"] = "vod"
				body["videoId"] = strings.TrimPrefix(input, "tw_v")
			} else {
				body["twitchType"] = "channel"
			}
		}
		if videoItag != nil {
			body["selectedVideoItag"] = *videoItag
		}
		if audioItag != nil {
			body["selectedAudioItag"] = *audioItag
		}
		if startTime != "" {
			body["startTime"] = startTime
		}
		if endTime != "" {
			body["endTime"] = endTime
		}

		jsonBody, _ := json.Marshal(body)
		url := fmt.Sprintf("%s/api/jobs", baseURL)
		resp, err := client.Post(url, "application/json", bytes.NewReader(jsonBody))
		if err != nil {
			return addVideoResultMsg{Feedback: "Failed to connect to server", IsError: true}
		}
		defer resp.Body.Close()

		if resp.StatusCode == 409 {
			return addVideoResultMsg{Feedback: "Job already exists"}
		}
		if resp.StatusCode >= 400 {
			var errResp struct{ Error string `json:"error"` }
			json.NewDecoder(resp.Body).Decode(&errResp)
			msg := errResp.Error
			if msg == "" {
				msg = fmt.Sprintf("Failed to add job (HTTP %d)", resp.StatusCode)
			}
			return addVideoResultMsg{Feedback: msg, IsError: true}
		}

		label := "Added to queue"
		if platform == "twitch" {
			if strings.HasPrefix(input, "tw_v") {
				label = "Added Twitch VOD to queue"
			} else {
				label = "Added Twitch channel to queue"
			}
		}
		return addVideoResultMsg{Feedback: label}
	}
}

// fetchFormatsCmd fetches format options from the local API for advanced mode.
func (a *App) fetchFormatsCmd(videoID string) tea.Cmd {
	baseURL := a.apiBaseURL()
	client := a.apiClient()

	// If a callback is provided, use it directly (avoids HTTP round-trip)
	if a.OnFetchFormats != nil {
		cb := a.OnFetchFormats
		return func() tea.Msg {
			data, err := cb(videoID)
			if err != nil {
				return fetchFormatsResultMsg{Err: "Failed to fetch formats. Proceeding with auto selection."}
			}
			return fetchFormatsResultMsg{Formats: data}
		}
	}

	return func() tea.Msg {
		url := fmt.Sprintf("%s/api/formats/%s", baseURL, videoID)
		resp, err := client.Get(url)
		if err != nil {
			return fetchFormatsResultMsg{Err: "Failed to fetch formats. Proceeding with auto selection."}
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			return fetchFormatsResultMsg{Err: "Failed to fetch formats. Proceeding with auto selection."}
		}

		var data FormatsData
		if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
			return fetchFormatsResultMsg{Err: "Failed to parse format data. Proceeding with auto selection."}
		}
		return fetchFormatsResultMsg{Formats: &data}
	}
}

// importFileCmd reads a ZIP file and uploads it to the import API.
func (a *App) importFileCmd(path string) tea.Cmd {
	title := a.importDlg.GetImportTitle()
	channel := a.importDlg.GetImportChannel()
	baseURL := a.apiBaseURL()
	client := a.apiClient()

	// If a callback is provided, use it directly
	if a.OnImportFile != nil {
		cb := a.OnImportFile
		return func() tea.Msg {
			importedTitle, err := cb(path, title, channel)
			if err != nil {
				return importResultMsg{Err: fmt.Sprintf("Import failed: %s", err)}
			}
			return importResultMsg{Title: importedTitle}
		}
	}

	return func() tea.Msg {
		f, err := os.Open(path)
		if err != nil {
			return importResultMsg{Err: fmt.Sprintf("Import failed: %s", err)}
		}
		defer f.Close()

		url := fmt.Sprintf("%s/api/import", baseURL)
		req, err := http.NewRequest("POST", url, f)
		if err != nil {
			return importResultMsg{Err: fmt.Sprintf("Import failed: %s", err)}
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		if title != "" {
			req.Header.Set("X-Import-Title", strings.TrimSpace(title))
		}
		if channel != "" {
			req.Header.Set("X-Import-Channel", strings.TrimSpace(channel))
		}

		resp, err := client.Do(req)
		if err != nil {
			return importResultMsg{Err: fmt.Sprintf("Import failed: %s", err)}
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(resp.Body)
			var errResp struct{ Error string `json:"error"` }
			json.Unmarshal(body, &errResp)
			msg := errResp.Error
			if msg == "" {
				msg = fmt.Sprintf("Import failed (HTTP %d)", resp.StatusCode)
			}
			return importResultMsg{Err: msg}
		}

		var result struct{ Title string `json:"title"` }
		json.NewDecoder(resp.Body).Decode(&result)
		importedTitle := result.Title
		if importedTitle == "" {
			importedTitle = strings.TrimSpace(title)
		}
		if importedTitle == "" {
			importedTitle = "archive"
		}
		return importResultMsg{Title: importedTitle}
	}
}

func (a *App) createTrimCmd(jobID string, startSec, endSec float64) tea.Cmd {
	createFn := a.OnCreateTrim
	progressMu := &a.trimProgressMu
	progressPct := &a.trimProgressPct
	return func() tea.Msg {
		if createFn == nil {
			return createTrimResultMsg{Err: "Create trim not available"}
		}
		onProgress := func(pct float64) {
			progressMu.Lock()
			*progressPct = pct
			progressMu.Unlock()
		}
		filename, errMsg := createFn(jobID, startSec, endSec, onProgress)
		if errMsg != "" {
			return createTrimResultMsg{Err: errMsg}
		}
		return createTrimResultMsg{Filename: filename}
	}
}

func (a *App) deleteTrimCmd(jobID, trimID string) tea.Cmd {
	// Capture state on the main goroutine to avoid data races in the closure.
	deleteFn := a.OnDeleteTrim
	var filename string
	for _, t := range a.trimDlg.trims {
		if t.ID == trimID {
			filename = t.Filename
			break
		}
	}
	return func() tea.Msg {
		if deleteFn == nil {
			return deleteTrimResultMsg{Err: "Delete trim not available"}
		}
		if err := deleteFn(jobID, trimID); err != nil {
			return deleteTrimResultMsg{Err: err.Error()}
		}
		return deleteTrimResultMsg{TrimID: trimID, Filename: filename}
	}
}

func (a *App) fetchOrphansCmd() tea.Cmd {
	listFn := a.OnListOrphans
	return func() tea.Msg {
		if listFn == nil {
			return fetchOrphansResultMsg{Err: "Not available"}
		}
		files, err := listFn()
		if err != nil {
			return fetchOrphansResultMsg{Err: err.Error()}
		}
		return fetchOrphansResultMsg{Files: files}
	}
}

func (a *App) fetchClientTokensCmd() tea.Cmd {
	listFn := a.OnListClientTokens
	return func() tea.Msg {
		if listFn == nil {
			return fetchClientTokensResultMsg{Err: "Not available"}
		}
		tokens, err := listFn()
		if err != nil {
			return fetchClientTokensResultMsg{Err: err.Error()}
		}
		return fetchClientTokensResultMsg{Tokens: tokens}
	}
}

func (a *App) deleteClientTokenCmd(id string) tea.Cmd {
	deleteFn := a.OnDeleteClientToken
	return func() tea.Msg {
		if deleteFn == nil {
			return deleteClientTokenResultMsg{ID: id, Err: "Not available"}
		}
		if err := deleteFn(id); err != nil {
			return deleteClientTokenResultMsg{ID: id, Err: err.Error()}
		}
		return deleteClientTokenResultMsg{ID: id}
	}
}

func (a *App) deleteOrphanCmd(path string) tea.Cmd {
	deleteFn := a.OnDeleteOrphan
	return func() tea.Msg {
		if deleteFn == nil {
			return deleteOrphanResultMsg{Path: path, Err: "Not available"}
		}
		if err := deleteFn(path); err != nil {
			return deleteOrphanResultMsg{Path: path, Err: err.Error()}
		}
		return deleteOrphanResultMsg{Path: path}
	}
}

// ffmpegCheckCmd runs FFmpeg path validation asynchronously via tea.Cmd.
func (a *App) ffmpegCheckCmd(path string) tea.Cmd {
	checkFn := a.OnCheckFFmpeg
	return func() tea.Msg {
		if checkFn == nil {
			return ffmpegCheckResultMsg{Valid: false, Path: path}
		}
		if path == "" {
			path = "ffmpeg"
		}
		valid, ver, warn := checkFn(path)
		return ffmpegCheckResultMsg{Valid: valid, Version: ver, Warning: warn, Path: path}
	}
}

// ffmpegPrepareCmd checks elevation and either installs directly or returns
// a script for review.
func (a *App) ffmpegPrepareCmd(method string) tea.Cmd {
	prepareFn := a.OnPrepareInstall
	return func() tea.Msg {
		if prepareFn == nil {
			return ffmpegPrepareResultMsg{Err: "install not available"}
		}
		needsElev, script, token, err := prepareFn(method)
		if err != nil {
			return ffmpegPrepareResultMsg{Err: err.Error()}
		}
		return ffmpegPrepareResultMsg{
			NeedsElevation: needsElev,
			Script:         script,
			Token:          token,
		}
	}
}

// ffmpegConfirmCmd executes a reviewed elevated install.
func (a *App) ffmpegConfirmCmd(token string) tea.Cmd {
	confirmFn := a.OnConfirmInstall
	return func() tea.Msg {
		if confirmFn == nil {
			return ffmpegConfirmResultMsg{Err: "confirm not available"}
		}
		if err := confirmFn(token); err != nil {
			return ffmpegConfirmResultMsg{Err: err.Error()}
		}
		return ffmpegConfirmResultMsg{}
	}
}

// resolveChannelCmd resolves a channel URL asynchronously via tea.Cmd.
func (a *App) resolveChannelCmd(input string) tea.Cmd {
	return func() tea.Msg {
		resolved, err := utils.ResolveChannelInput(context.Background(), input)
		if err != nil {
			return channelResolvedMsg{Err: err}
		}
		if resolved == nil {
			// Not a recognized URL — return input as-is
			return channelResolvedMsg{ID: input}
		}
		return channelResolvedMsg{
			ID:       resolved.ID,
			Name:     resolved.Name,
			Platform: resolved.Platform,
		}
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// Run starts the TUI program.
func Run(app *App) error {
	p := tea.NewProgram(app,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	app.program = p
	_, err := p.Run()
	return err
}

// QuitTUI programmatically exits the TUI (used by restart to unblock Run).
func (a *App) QuitTUI() {
	if a.program != nil {
		a.program.Quit()
	}
}
