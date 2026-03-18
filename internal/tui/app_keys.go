package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/vampiricwulf/Moombox/internal/database"
)

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
				onRestart := a.OnRestart
				return a, safeCmd(func() tea.Msg {
					onRestart()
					return tea.QuitMsg{}
				})
			}
		case "resolve_channel":
			return a, a.resolveChannelCmd(a.settings.GetChannelResolveInput())
		case "open_ffmpeg":
			a.settings.Close()
			a.ffmpegCheck.OnCheckPrereqs = a.OnCheckPrereqs
			a.ffmpegCheck.Open()
			a.ffmpegCheck.SetSize(a.width, a.height)
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
		// Allow Ctrl+C even during install/check (HandleKey blocks all input
		// in those states, so the normal Ctrl+C handler at line 220 is unreachable).
		if key == keyCtrlC {
			return a, tea.Quit
		}
		action := a.ffmpegCheck.HandleKey(key)
		switch {
		case action == "skip":
			a.ffmpegCheck.Close()
			return a, nil
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
		case strings.HasPrefix(action, "cancel:"):
			token := strings.TrimPrefix(action, "cancel:")
			if a.OnRejectInstall != nil {
				a.OnRejectInstall(token)
			}
			a.ffmpegCheck.ShowInstallOptions()
		case strings.HasPrefix(action, "check_custom:"):
			path := strings.TrimPrefix(action, "check_custom:")
			return a, tea.Batch(a.ffmpegCheckCmd(path), a.ffmpegCheck.spinner.Tick)
		case action == "dismiss":
			// Overlay already closed by HandleKey — nothing else to do.
		}
		return a, nil
	}

	// Setup wizard
	if a.setupWiz.IsVisible() {
		// Allow Ctrl+C to quit even during setup wizard
		if key == keyCtrlC {
			// Cancel any in-progress cookie setup before quitting
			if a.setupWiz.cookieActive && a.setupWiz.OnCancelAutoCookie != nil {
				a.setupWiz.OnCancelAutoCookie()
			}
			return a, tea.Quit
		}
		action := a.setupWiz.HandleKey(key)
		if action == "save" {
			// Dispatch async config save so the "Saving..." overlay is rendered
			cfg := a.setupWiz.pendingConfig
			installYtdlp := a.setupWiz.pendingYtdlp
			onComplete := a.setupWiz.OnComplete
			onInstall := a.setupWiz.OnInstallYtdlp
			return a, safeCmd(func() tea.Msg {
				if cfg == nil {
					return setupSaveResultMsg{Err: "no config to save"}
				}
				if onComplete != nil {
					if err := onComplete(cfg); err != nil {
						return setupSaveResultMsg{Err: err.Error()}
					}
				}
				// Create output/staging directories (matches web API behavior)
				if cfg.Paths.OutputDirectory != "" {
					os.MkdirAll(cfg.Paths.OutputDirectory, 0o755)
				}
				if cfg.Paths.StagingDirectory != "" {
					os.MkdirAll(cfg.Paths.StagingDirectory, 0o755)
				}
				// Post-save: install yt-dlp plugin if requested
				if installYtdlp && onInstall != nil {
					onInstall(cfg.Network.Port, cfg.Network.HTTPSEnabled)
				}
				return setupSaveResultMsg{}
			})
		}
		var cmds []tea.Cmd
		if action == "finish_cookie" {
			// Run cookie extraction async so TUI doesn't freeze
			platform := a.setupWiz.cookiePlatform
			finishFn := a.setupWiz.OnFinishAutoCookie
			cmds = append(cmds, safeCmd(func() tea.Msg {
				yt, tw := false, false
				var errStr string
				if finishFn != nil {
					var err error
					yt, tw, err = finishFn()
					if err != nil {
						errStr = err.Error()
					}
				}
				return setupCookieFinishMsg{Platform: platform, YTAuth: yt, TWAuth: tw, Err: errStr}
			}))
		}
		if a.setupWiz.cookieActive {
			cmds = append(cmds, a.setupWiz.spinner.Tick, cookieCountdownTick())
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
			chord, jobID, hasJob := strings.Cut(action, ":")
			var job *database.Job
			if hasJob {
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
		a.seenChordHint = true
		a.actionMenu.SetSize(a.width, a.height)
		a.actionMenu.Open(a.buildMenuItems())
		return a, nil
	case "?":
		a.seenChordHint = true
		return a.dispatchAction("?", nil)
	case "`":
		a.seenChordHint = true
		return a.dispatchAction("`", nil)
	case keyTab:
		a.cycleFocus()
		return a, nil
	}

	// Unrecognized single-character key — show invalid chord feedback
	if len(key) == 1 {
		a.setFeedbackWithDuration("Invalid Chord: "+strings.ToUpper(key), time.Second)
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

func (a *App) handleDetailKey(_ string) (tea.Model, tea.Cmd) {
	// Scroll handled by viewport in routeComponentMsg
	return a, nil
}

func (a *App) handleLogKey(key string) (tea.Model, tea.Cmd) {
	// Scroll handled by viewport in routeComponentMsg.
	// End key re-enables auto-scroll (not in helpViewportKeyMap, so handled here).
	if key == keyEnd {
		a.logs.ReEnableAutoScroll()
	}
	return a, nil
}

// handleFilter applies the F key action based on the focused panel.
func (a *App) handleFilter() {
	switch a.focusedPanel {
	case PanelTasks:
		a.taskList.CycleFilter()
		a.updateSelectedJob()
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
		// Wrong key — show invalid chord and consume
		typed := strings.ToUpper(a.chord.prefix) + " " + strings.ToUpper(a.chord.action) + " " + strings.ToUpper(key)
		a.setFeedbackWithDuration("Invalid Chord: "+typed, time.Second)
		a.chord = chordState{}
		return a, nil, true
	}

	// Active prefix, waiting for second key
	if a.chord.prefix != "" {
		m, cmd, handled := a.processSecondKey(a.chord.prefix, key)
		if handled {
			return m, cmd, true
		}
		// Not a valid second key — show invalid chord and consume
		typed := strings.ToUpper(a.chord.prefix) + " " + strings.ToUpper(key)
		a.setFeedbackWithDuration("Invalid Chord: "+typed, time.Second)
		a.chord = chordState{}
		return a, nil, true
	}

	// No active chord — check if key is a prefix
	switch key {
	case "a", "r", "o", "q":
		a.seenChordHint = true
		a.chord = chordState{prefix: key, prefixTime: now}
		a.setFeedback(a.chordFeedback(key))
		return a, nil, true
	}

	return a, nil, false
}

// processSecondKey handles the second key in a chord sequence.
// It looks up the chord in buildMenuItems() instead of hardcoding valid keys.
func (a *App) processSecondKey(prefix, key string) (tea.Model, tea.Cmd, bool) {
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
