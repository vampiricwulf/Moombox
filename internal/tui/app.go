// Package tui provides the terminal user interface for Moombox using BubbleTea.
package tui

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
	LogMsg         struct{ Line string }
	LogBatchMsg    struct{ Lines []string }
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
	tickMsg         struct{}
	progressTickMsg struct{}
	logFlushMsg     struct{} // 250ms log batching flush
	marqueeTickMsg  struct{} // 150ms marquee scroll tick

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
	ffmpegInstallResultMsg struct {
		Err string // empty on success
	}
	ffmpegCheckResultMsg struct {
		Valid   bool
		Version string
		Path    string // the path that was checked
	}
)

// App is the root BubbleTea model.
type App struct {
	// Panels
	taskList  *TaskListModel
	details   *JobDetailsModel
	logs      *LogViewerModel
	statusBar *StatusBarModel
	help      *HelpModel
	addVideo  *AddVideoModel
	trimDlg   *TrimDialogModel
	filesDlg  *FilesDialogModel
	setupWiz  *SetupWizardModel
	settings  *SettingsModel

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

	// Confirmation state
	deleteConfirmID string
	cancelConfirmID string
	confirmTimer    time.Time

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

	// BubbleTea program reference (set by Run, used by QuitTUI)
	program *tea.Program

	// Config reference for settings panel
	cfg *config.MoomboxConfig

	// First-run flag: triggers setup wizard
	IsFirstRun bool

	// Callbacks for actions
	OnAddVideo   func(url string)
	OnCancelJob  func(jobID string)
	OnDeleteJob  func(jobID string)
	OnRetryJob   func(jobID string)
	OnCreateTrim func(jobID string, startSec, endSec float64)
	OnDeleteTrim func(jobID, trimID string)
	OnOpenFolder func(jobID string)
	OnSaveConfig     func(cfg *config.MoomboxConfig)
	OnRestart        func()
	OnHashPassword   func(password string) string
	OnVerifyPassword func(password, hash string) bool
	OnFetchFormats   func(videoID string) (*FormatsData, error)          // optional: fetch formats via service
	OnImportFile     func(path, title, channel string) (string, error)  // optional: import zip, returns title
	OnListOrphans    func() ([]OrphanedFileEntry, error)                // list orphaned files
	OnDeleteOrphan   func(path string) error                            // delete orphaned file

	// FFmpeg check callbacks
	OnCheckFFmpeg  func(path string) (bool, string) // check if ffmpeg path is valid
	OnInstallFFmpeg func(method string) error        // install ffmpeg via choco/winget
	OnCheckPrereqs  func() (bool, bool)              // returns (chocoAvail, wingetAvail)

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
		trimDlg:       NewTrimDialogModel(),
		filesDlg:      NewFilesDialogModel(),
		setupWiz:      NewSetupWizardModel(),
		settings:      NewSettingsModel(),
		ffmpegCheck:   NewFFmpegCheckModel(),
		progressStore: ps,
		statusMap:     make(map[string]database.JobStatus),
	}
}

// ShowFFmpegCheck marks the FFmpeg check overlay to show after init.
func (a *App) ShowFFmpegCheck() {
	a.showFFmpeg = true
}

// SetConfig provides the config reference for the settings panel.
func (a *App) SetConfig(cfg *config.MoomboxConfig) {
	a.cfg = cfg
	a.taskList.SetHideFinishedAgeDays(int(cfg.Monitors.HideFinishedAgeDays.Days()))
}

// SetSetupCallbacks wires callback functions for the TUI setup wizard.
func (a *App) SetSetupCallbacks(
	onComplete func(cfg *config.MoomboxConfig) error,
	onInstallYtdlp func(port int),
	onStartAutoCookie func(platform string),
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
) {
	a.jobUpdateCh = jobUpdate
	a.jobsUpdateCh = jobsUpdate
	a.logCh = logCh
	a.checkTimersCh = checkTimers
	a.cookieStatusCh = cookieStatus
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
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg {
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
		}
		return nil
	}
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
		// Confirmation timeout (A7)
		if !a.confirmTimer.IsZero() && time.Now().After(a.confirmTimer) {
			if a.deleteConfirmID != "" {
				a.setFeedback("Delete cancelled")
			} else if a.cancelConfirmID != "" {
				a.setFeedback("Cancel dismissed")
			}
			a.deleteConfirmID = ""
			a.cancelConfirmID = ""
			a.confirmTimer = time.Time{}
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
		a.updateSelectedJob()
		return a, tea.Batch(a.updateTerminalTitle(), a.listenForUpdates())

	case LogMsg:
		// Buffer logs instead of adding directly (A2)
		a.logBuffer = append(a.logBuffer, msg.Line)
		return a, a.listenForUpdates()

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
			a.addVideo.SetError(msg.Err)
			a.addVideo.importStep = 1 // Back to metadata step (match TS)
			return a, nil
		}
		a.addVideo.Close()
		a.setFeedback("Imported: " + msg.Title)
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
		} else {
			a.filesDlg.SetFiles(msg.Files)
		}
		return a, nil

	case deleteOrphanResultMsg:
		if msg.Err != "" {
			a.filesDlg.SetError(msg.Err)
		} else {
			a.filesDlg.RemoveFile(msg.Path)
			a.filesDlg.feedbackMsg = "Deleted"
		}
		return a, nil

	case ffmpegInstallResultMsg:
		if msg.Err != "" {
			a.ffmpegCheck.SetInstallResult(fmt.Sprintf("Install failed: %s", msg.Err), true)
		} else {
			// Install succeeded — verify FFmpeg is available
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
			} else {
				a.ffmpegCheck.SetInstallResult("FFmpeg installed: "+msg.Version, false)
			}
			// Show success message — next keypress will dismiss the overlay
			a.ffmpegCheck.successDismiss = true
		} else {
			if a.ffmpegCheck.mode == ffmpegCustom {
				a.ffmpegCheck.SetCustomResult("Invalid: ffmpeg not found at this path", false)
			} else {
				a.ffmpegCheck.SetInstallResult("FFmpeg installed but not found on PATH. Restart may be needed.", true)
			}
		}
		return a, nil

	case tea.KeyMsg:
		return a.handleKey(msg)

	case tea.MouseMsg:
		return a.handleMouse(msg)
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

	for i, j := range a.taskList.jobs {
		if j.ID == job.ID {
			a.taskList.jobs[i] = job
			a.taskList.rebuildVirtualList()
			break
		}
	}
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
		}
		return a, nil
	}

	// Help overlay intercepts all keys
	if a.help.IsVisible() {
		switch key {
		case "?", keyEsc:
			a.help.Toggle()
		case keyUp:
			a.help.ScrollUp()
		case keyDown:
			a.help.ScrollDown()
		}
		return a, nil
	}

	// FFmpeg check overlay takes priority over all other dialogs
	if a.ffmpegCheck.IsVisible() {
		action := a.ffmpegCheck.HandleKey(key)
		switch {
		case action == "quit":
			return a, tea.Quit
		case strings.HasPrefix(action, "install:"):
			method := strings.TrimPrefix(action, "install:")
			return a, a.ffmpegInstallCmd(method)
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
		return a, nil
	}

	// Dialog intercepts
	if a.addVideo.IsVisible() {
		action, data := a.addVideo.HandleKey(key)
		switch action {
		case "submit":
			return a, a.addVideoCmd(data)
		case "fetch_formats":
			return a, a.fetchFormatsCmd(data)
		case "import":
			return a, a.importFileCmd(data)
		}
		return a, nil
	}
	if a.trimDlg.IsVisible() {
		action := a.trimDlg.HandleKey(key)
		switch action {
		case "submit":
			if a.OnCreateTrim != nil {
				a.OnCreateTrim(a.trimDlg.JobID(), a.trimDlg.ParsedStartSeconds(), a.trimDlg.ParsedEndSeconds())
				a.trimDlg.Close()
			}
		case "delete":
			if a.OnDeleteTrim != nil {
				trimID := a.trimDlg.SelectedTrimID()
				if trimID != "" {
					a.trimDlg.SetLoading(true)
					jobID := a.trimDlg.JobID()
					return a, a.deleteTrimCmd(jobID, trimID)
				}
			}
		}
		return a, nil
	}
	if a.filesDlg.IsVisible() {
		action := a.filesDlg.HandleKey(key)
		switch action {
		case "refresh":
			return a, a.fetchOrphansCmd()
		case "delete":
			if sel := a.filesDlg.SelectedFile(); sel != nil {
				return a, a.deleteOrphanCmd(sel.Path)
			}
		}
		return a, nil
	}

	// Normalize single-character keys to lowercase (match TS: accepts both d/D, c/C, etc.)
	// Done AFTER dialog intercepts so text inputs preserve case.
	if len(key) == 1 && key[0] >= 'A' && key[0] <= 'Z' {
		key = strings.ToLower(key)
	}

	// Clear confirmations on any non-matching key (A9)
	if a.deleteConfirmID != "" && key != "d" {
		a.deleteConfirmID = ""
		a.confirmTimer = time.Time{}
	}
	if a.cancelConfirmID != "" && key != "c" {
		a.cancelConfirmID = ""
		a.confirmTimer = time.Time{}
	}

	// Global keys
	switch key {
	case keyQ, keyCtrlC:
		return a, tea.Quit
	case "?":
		a.help.SetActivePanel(a.focusedPanel)
		a.help.Toggle()
		// Clear confirmations when opening help (match TS)
		a.deleteConfirmID = ""
		a.cancelConfirmID = ""
		a.confirmTimer = time.Time{}
		return a, nil
	case keyTab:
		a.cycleFocus()
		return a, nil
	case "w":
		scheme := "http"
		if a.cfg != nil && a.cfg.Network.HTTPSEnabled {
			scheme = "https"
		}
		url := fmt.Sprintf("%s://localhost:%d", scheme, a.getPort())
		a.setFeedback(fmt.Sprintf("Opening: %s", url))
		openBrowser(url)
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
	case "~":
		a.filesDlg.SetSize(a.width, a.height)
		a.filesDlg.Open()
		return a, a.fetchOrphansCmd()
	}

	// Panel-specific keys
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
	case "a":
		a.feedbackMsg = "" // match TS: clear feedback when opening add dialog
		a.addVideo.SetSize(a.width, a.height)
		a.addVideo.Open()
	case "f":
		a.taskList.CycleFilter()
	case keyEnter:
		if a.taskList.SelectedIsDivider() {
			a.taskList.ToggleArchive()
		}
	case "d":
		a.handleDelete()
	case "c":
		a.handleCancel()
	case "r":
		if job := a.taskList.SelectedJob(); job != nil && a.OnRetryJob != nil {
			if job.Status == database.StatusError || job.Status == database.StatusCancelled || job.Status == database.StatusCookies {
				a.OnRetryJob(job.ID)
			}
		}
	case "o":
		if job := a.taskList.SelectedJob(); job != nil && a.OnOpenFolder != nil {
			switch job.Status {
			case database.StatusFinished:
				if job.OutputFile != "" {
					a.OnOpenFolder(job.ID)
					a.setFeedback(fmt.Sprintf("Opening folder for: %s", job.Title))
				}
			case database.StatusUpcoming, database.StatusLive,
				database.StatusDownloading, database.StatusMuxing:
				a.OnOpenFolder(job.ID)
				a.setFeedback(fmt.Sprintf("Opening staging folder for: %s", job.Title))
			}
		}
	case "t":
		if job := a.taskList.SelectedJob(); job != nil {
			if job.Status == database.StatusFinished && job.OutputFile != "" {
				a.trimDlg.SetSize(a.width, a.height)
				a.trimDlg.Open(job.ID, job.Title)
				// Provide job metadata for estimated size calculation
				var lenSec float64
				var fSize int64
				if job.LengthSeconds != nil {
					lenSec = float64(*job.LengthSeconds)
				}
				if job.FileSize != nil {
					fSize = *job.FileSize
				}
				a.trimDlg.SetJobMetadata(lenSec, fSize)
				// Provide existing trims for delete mode
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
	}
	return a, nil
}

func (a *App) handleDetailKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case keyUp:
		a.details.ScrollUp()
	case keyDown:
		a.details.ScrollDown()
	}
	return a, nil
}

func (a *App) handleLogKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case keyUp:
		a.logs.ScrollUp()
	case keyDown:
		a.logs.ScrollDown()
	case keyPgUp:
		a.logs.PageUp()
	case keyPgDown:
		a.logs.PageDown()
	case "l":
		a.logs.CycleLevel()
	}
	return a, nil
}

func (a *App) handleDelete() {
	job := a.taskList.SelectedJob()
	if job == nil || a.OnDeleteJob == nil {
		return
	}
	if a.deleteConfirmID == job.ID && time.Now().Before(a.confirmTimer) {
		a.OnDeleteJob(job.ID)
		a.deleteConfirmID = ""
		a.confirmTimer = time.Time{}
		a.setFeedback(fmt.Sprintf("Deleted: %s", job.Title))
		// Move selection up by 1 after deletion (match TS)
		if a.taskList.selectedIndex > 0 {
			a.taskList.selectedIndex--
		}
	} else {
		a.deleteConfirmID = job.ID
		a.cancelConfirmID = ""
		a.confirmTimer = time.Now().Add(3 * time.Second)
		a.setFeedback(fmt.Sprintf("Press D again to delete \"%s\"", job.Title))
	}
}

func (a *App) handleCancel() {
	job := a.taskList.SelectedJob()
	if job == nil || a.OnCancelJob == nil {
		return
	}
	// Status guard: only allow cancel on non-terminal, non-error jobs (match TS)
	if job.Status == database.StatusFinished || job.Status == database.StatusCancelled || job.Status == database.StatusError {
		return
	}
	if a.cancelConfirmID == job.ID && time.Now().Before(a.confirmTimer) {
		a.OnCancelJob(job.ID)
		a.cancelConfirmID = ""
		a.confirmTimer = time.Time{}
		a.setFeedback(fmt.Sprintf("Cancelled: %s", job.Title))
	} else {
		a.cancelConfirmID = job.ID
		a.deleteConfirmID = ""
		a.confirmTimer = time.Now().Add(3 * time.Second)
		a.setFeedback(fmt.Sprintf("Press C again to cancel \"%s\"", job.Title))
	}
}

func (a *App) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if a.settings.IsVisible() || a.help.IsVisible() || a.addVideo.IsVisible() || a.trimDlg.IsVisible() || a.filesDlg.IsVisible() || a.setupWiz.IsVisible() || a.ffmpegCheck.IsVisible() {
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
			if contentY >= 0 {
				idx := a.taskList.scrollOffset + contentY
				if idx < len(a.taskList.virtualItems) {
					a.taskList.selectedIndex = idx
					if a.taskList.SelectedIsDivider() {
						a.taskList.ToggleArchive()
					} else {
						a.updateSelectedJob()
					}
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
	a.statusBar.SetFocused(a.focusedPanel)
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
	a.statusBar.SetFocused(panel)
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
	if a.addVideo.IsVisible() {
		return a.addVideo.View()
	}
	if a.trimDlg.IsVisible() {
		return a.trimDlg.View()
	}
	if a.filesDlg.IsVisible() {
		return a.filesDlg.View()
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
		if strings.HasPrefix(a.feedbackMsg, "Press D") || strings.HasPrefix(a.feedbackMsg, "Press C") {
			msgColor = lipgloss.Color("#f1c40f") // yellow for confirmations
		} else if strings.HasPrefix(a.feedbackMsg, "Can only") || strings.HasPrefix(a.feedbackMsg, "Trim only") {
			msgColor = lipgloss.Color("#f1c40f") // yellow for warnings
		} else if strings.HasPrefix(a.feedbackMsg, "Cancelled:") {
			msgColor = ColorRed
		} else if strings.Contains(a.feedbackMsg, "Deleted:") || a.feedbackMsg == "Delete cancelled" || a.feedbackMsg == "Cancel dismissed" {
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

// apiClient returns an HTTP client suitable for local API calls.
// When HTTPS is enabled, TLS verification is skipped since the server
// typically uses a self-signed certificate on localhost.
func (a *App) apiClient() *http.Client {
	if a.cfg != nil && a.cfg.Network.HTTPSEnabled {
		return &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		}
	}
	return http.DefaultClient
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
		url := fmt.Sprintf("%s/api/v1/jobs", baseURL)
		resp, err := client.Post(url, "application/json", strings.NewReader(string(jsonBody)))
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
		url := fmt.Sprintf("%s/api/v1/formats/%s", baseURL, videoID)
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
	title := a.addVideo.GetImportTitle()
	channel := a.addVideo.GetImportChannel()
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
		fileData, err := os.ReadFile(path)
		if err != nil {
			return importResultMsg{Err: fmt.Sprintf("Import failed: %s", err)}
		}

		url := fmt.Sprintf("%s/api/import", baseURL)
		req, err := http.NewRequest("POST", url, strings.NewReader(string(fileData)))
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
		deleteFn(jobID, trimID)
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

// ffmpegInstallCmd runs FFmpeg installation asynchronously via tea.Cmd.
func (a *App) ffmpegInstallCmd(method string) tea.Cmd {
	installFn := a.OnInstallFFmpeg
	return func() tea.Msg {
		if installFn == nil {
			return ffmpegInstallResultMsg{Err: "install not available"}
		}
		if err := installFn(method); err != nil {
			return ffmpegInstallResultMsg{Err: err.Error()}
		}
		return ffmpegInstallResultMsg{}
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
		valid, ver := checkFn(path)
		return ffmpegCheckResultMsg{Valid: valid, Version: ver, Path: path}
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
