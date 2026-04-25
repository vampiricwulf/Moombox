package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// Update implements tea.Model.
func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width = msg.Width
		a.height = msg.Height
		a.recalcLayout()
		return a, nil

	case tea.BackgroundColorMsg:
		a.isDark = msg.IsDark()
		a.setupWiz.isDark = a.isDark
		a.ffmpegCheck.isDark = a.isDark
		return a, nil

	case tea.ColorProfileMsg:
		a.colorProfile = msg.Profile
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
		a.updateTerminalTitle()
		return a, a.tick()

	case progressTickMsg:
		// Refresh progress overlay for the selected job. Active downloads get
		// 16ms ticks; all other jobs still get 500ms ticks which is enough for
		// chat count updates on Upcoming jobs with early chat running.
		if sel := a.taskList.SelectedJob(); sel != nil {
			if p := a.progressStore.Get(sel.ID); p != nil {
				a.details.SetProgress(p)
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

	case channelClosedMsg:
		switch msg.Name {
		case "jobUpdate":
			a.jobUpdateCh = nil
		case "jobsUpdate":
			a.jobsUpdateCh = nil
		case "log":
			a.logCh = nil
		case "checkTimers":
			a.checkTimersCh = nil
		case "cookieStatus":
			a.cookieStatusCh = nil
		case "diskStatus":
			a.diskStatusCh = nil
		case "updateStatus":
			a.updateStatusCh = nil
		}
		return a, a.listenForUpdates()

	case JobUpdateMsg:
		a.handleJobUpdate(msg.Change)
		return a, a.listenForUpdates()

	case JobsUpdateMsg:
		// If jobs already exist, the user has used the app before — dismiss the newcomer hint.
		if len(msg.Jobs) > 0 {
			a.seenChordHint = true
		}
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
		a.updateTerminalTitle()
		return a, a.listenForUpdates()

	case LogBatchMsg:
		// Batched log messages — single Update/View cycle for all pending logs
		a.logBuffer = append(a.logBuffer, msg.Lines...)
		if len(a.logBuffer) > 1000 {
			// slices.Clone avoids aliasing the old backing array; without it
			// the re-slice retains the full original capacity, leaking memory
			// over the 24/7 runtime target.
			a.logBuffer = slices.Clone(a.logBuffer[len(a.logBuffer)-1000:])
		}
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

	case ConnectivityMsg:
		a.statusBar.offline = !msg.Online
		return a, nil

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

	case signatureVerifyResultMsg:
		if msg.Err != "" {
			a.setFeedback("Signature verification failed: " + msg.Err)
		} else {
			a.setFeedback("Signature verified — binary is authentic")
		}
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
		if a.addVideo.IsVisible() && a.addVideo.errorMsg != "" {
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
			a.ffmpegCheck.installing = false
			a.ffmpegCheck.installResult = "Verifying installation..."
			return a, a.ffmpegCheckCmd("")
		}
		return a, nil

	case ffmpegConfirmResultMsg:
		if msg.Err != "" {
			a.ffmpegCheck.SetInstallResult(fmt.Sprintf("Install failed: %s", msg.Err), true)
		} else {
			// Elevated install succeeded — verify FFmpeg is available
			a.ffmpegCheck.installing = false
			a.ffmpegCheck.installResult = "Verifying installation..."
			return a, a.ffmpegCheckCmd("")
		}
		return a, nil

	case ffmpegCheckResultMsg:
		if msg.Valid {
			switch a.ffmpegCheck.mode {
			case ffmpegCustom:
				a.ffmpegCheck.SetCustomResult("Valid: "+msg.Version, true)
			case ffmpegManual:
				a.ffmpegCheck.SetManualResult("Valid: "+msg.Version, true)
			default:
				a.ffmpegCheck.SetInstallResult("FFmpeg installed: "+msg.Version, false)
			}
			// Persist custom/manual FFmpeg path to config. Do the disk write
			// OUTSIDE the configStore lock so TUI renders and apiClient
			// construction aren't stalled behind disk IO. Matches
			// settings_security.go:121-131. Audit reports/tui.md Finding 2.
			if (a.ffmpegCheck.mode == ffmpegCustom || a.ffmpegCheck.mode == ffmpegManual) && msg.Path != "" && a.cfg != nil {
				var saveCb func(*config.MoomboxConfig)
				var cfgSnapshot *config.MoomboxConfig
				mu := a.configStore.RWMutex()
				mu.Lock()
				a.cfg.Paths.FfmpegPath = msg.Path
				if a.OnSaveConfig != nil {
					saveCb = a.OnSaveConfig
					cfgSnapshot = a.cfg
				}
				mu.Unlock()
				if saveCb != nil {
					saveCb(cfgSnapshot)
				}
			}
			a.ffmpegCheck.warning = msg.Warning
			a.ffmpegCheck.successDismiss = true
		} else {
			switch a.ffmpegCheck.mode {
			case ffmpegCustom:
				a.ffmpegCheck.SetCustomResult("Invalid: ffmpeg not found at this path", false)
			case ffmpegManual:
				a.ffmpegCheck.SetManualResult("Invalid: ffmpeg not found at this path", false)
			default:
				a.ffmpegCheck.SetInstallResult("FFmpeg installed but not found on PATH. Restart may be needed.", true)
			}
		}
		return a, nil

	case channelResolvedMsg:
		a.settings.HandleChannelResolved(msg.ID, msg.Name, msg.Platform, msg.Err)
		return a, nil

	case setupSaveResultMsg:
		if msg.Err != "" {
			a.setupWiz.saving = false
			a.setupWiz.pendingConfig = nil
			a.setupWiz.errorMsg = fmt.Sprintf("Failed to save: %v", msg.Err)
			return a, nil
		}
		// Save succeeded — mark setup complete for onboarding nudge, then trigger restart
		a.taskList.JustCompletedSetup = true
		a.setupWiz.Close()
		if a.setupWiz.OnRestart != nil {
			onRestart := a.setupWiz.OnRestart
			return a, safeCmd(func() tea.Msg {
				onRestart()
				return tea.QuitMsg{}
			})
		}
		return a, nil

	case setupCookieFinishMsg:
		// Update both platform flags — extraction may detect cookies for either
		// platform regardless of which was targeted (matches Web UI behavior).
		if msg.YTAuth {
			a.setupWiz.cookieYTDone = true
		}
		if msg.TWAuth {
			a.setupWiz.cookieTWDone = true
		}
		a.setupWiz.cookieActive = false
		a.setupWiz.cookieFinishing = false
		a.setupWiz.cookiePlatform = ""
		a.setupWiz.cookieCountdown = 0
		// Show per-platform success feedback, or error/no-login feedback
		if msg.Err != "" {
			a.setupWiz.errorMsg = msg.Err
		} else if !msg.YTAuth && !msg.TWAuth {
			a.setupWiz.errorMsg = "No login detected — try signing in again"
		} else {
			if msg.YTAuth {
				a.setFeedback("YouTube cookies configured")
			}
			if msg.TWAuth {
				a.setFeedback("Twitch cookies configured")
			}
		}
		return a, nil

	case panicRecoveryMsg:
		// Clear setup wizard async state if a panic occurred during save or cookie extraction
		if a.setupWiz.IsVisible() && (a.setupWiz.saving || a.setupWiz.cookieFinishing) {
			a.setupWiz.saving = false
			a.setupWiz.pendingConfig = nil
			a.setupWiz.cookieActive = false
			a.setupWiz.cookieFinishing = false
			a.setupWiz.cookiePlatform = ""
			a.setupWiz.errorMsg = msg.Text
		} else {
			a.setFeedback(msg.Text)
		}
		return a, nil

	case tea.KeyPressMsg:
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

func (a *App) setFeedbackWithDuration(msg string, d time.Duration) {
	a.feedbackMsg = msg
	a.feedbackTimer = time.Now().Add(d)
}

// displayColumns is the set of database column names whose changes require
// rebuilding the task-list row and (if selected) the detail panel.
// Mirrors the previous 12-field compare in handleJobUpdate but driven by
// JobChange.Changes from UpdateJobFields — the database tells us exactly
// which columns were written, so we no longer need to fetch the previous
// snapshot and compare field-by-field. Audit reports/tui.md F20.
var displayColumns = map[string]struct{}{
	"status":            {},
	"title":             {},
	"channel_name":      {},
	"thumbnail_url":     {},
	"description":       {},
	"stream_start_time": {},
	"stream_end_time":   {},
	"error":             {},
	"output_file":       {},
	"filename":          {},
	"is_vod":            {},
	"chat_status":       {},
}

// hasDisplayChange reports whether any column in changes warrants a
// task-list / detail-panel rebuild.
func hasDisplayChange(changes []string) bool {
	for _, col := range changes {
		if _, ok := displayColumns[col]; ok {
			return true
		}
	}
	return false
}

func (a *App) handleJobUpdate(ev *database.JobChange) {
	job := ev.Job

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

	// Rebuild task-list row + detail panel only when a display-relevant
	// column was actually written. Progress-only updates (~10/sec during
	// downloads) flow through progressStore and don't need list rebuilds.
	if hasDisplayChange(ev.Changes) {
		a.taskList.UpdateJob(job)
		if sel := a.taskList.SelectedJob(); sel != nil && sel.ID == job.ID {
			a.details.SetJob(job)
		}
	}

	// Only run status-transition logic when status actually changed
	prevStatus, exists := a.statusMap[job.ID]
	if !exists || prevStatus != job.Status {
		a.statusMap[job.ID] = job.Status

		// Clean up progress for terminal/error statuses (match TS behavior)
		if isCompletedStatus(job.Status) || job.Status == database.StatusError || job.Status == database.StatusCookies {
			a.progressStore.Delete(job.ID)
		}

		// Update terminal title on status change
		a.updateTerminalTitle()
	}
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
	// Help overlay viewport (priority matches handleKey order)
	if a.help.IsVisible() {
		return a.help.UpdateViewport(msg)
	}
	if a.ffmpegCheck.IsVisible() {
		return a.ffmpegCheck.UpdateComponents(msg)
	}
	if a.setupWiz.IsVisible() {
		return a.setupWiz.UpdateComponents(msg)
	}
	// actionMenu has no component routing
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
	// Panel viewports (when no dialog visible)
	switch a.focusedPanel {
	case PanelLogs:
		if a.logs.IsSearching() {
			return a.logs.UpdateSearchInput(msg)
		}
		return a.logs.UpdateViewport(msg)
	case PanelDetails:
		return a.details.UpdateViewport(msg)
	}
	return nil
}
