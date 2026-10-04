package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
)

func (a *App) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// Ctrl+C ALWAYS quits — checked before ANY overlay intercept (O-M).
	// bubbletea v2 delivers Ctrl+C as a plain key (tea.InterruptMsg comes
	// only from a real SIGINT), and fourteen overlays consume every key
	// before it reaches the rest of this function, so twelve of them
	// swallowed the "Ctrl+C  Quit immediately" help.go promises. No text
	// input in the TUI binds Ctrl+C — both search boxes already hand it
	// back — so hoisting it costs nothing (CORE-5).
	//
	// The wizard's cookie step is the one thing that needs doing on the way
	// out: AutoCookieService holds the acquisition slot until someone
	// cancels, so quitting with a headed browser open would orphan the
	// window and meet the next login with ErrSetupInProgress for the whole
	// grace window. This is the same release closeCookieLogin performs; it
	// lives here because the hoist returns before the wizard's own handler
	// is ever reached.
	if key == keyCtrlC {
		if a.setupWiz.IsVisible() && a.setupWiz.cookieActive && a.setupWiz.OnCancelAutoCookie != nil {
			a.setupWiz.OnCancelAutoCookie()
		}
		return a, tea.Quit
	}

	// Settings panel intercepts all keys (before normalization to preserve case for text input)
	if a.settings.IsVisible() {
		action := a.settings.HandleKey(key)
		switch action {
		case "close":
			// Re-apply hide_finished_age_days in case it changed. Read
			// under the store lock — HTTP handlers mutate config via
			// configStore.Update concurrently (matches getPort/apiBaseURL).
			if a.configStore != nil {
				var days float64
				a.configStore.Read(func(c *config.MoomboxConfig) {
					days = c.Monitors.HideFinishedAgeDays.Days()
				})
				a.taskList.SetHideFinishedAgeDays(days)
			}
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
		case "test_notification":
			return a, a.testNotificationCmd(a.settings.SelectedNotificationURL())
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

	// Release-notes overlay intercepts keys when open. Scroll is handled by
	// routeComponentMsg forwarding to the embedded viewport; here we only
	// handle the close/apply bindings.
	if a.releaseNotesPopup != nil && a.releaseNotesPopup.isOpen() {
		switch key {
		case keyEsc, "q", "Q":
			a.releaseNotesPopup.close()
			return a, nil
		case "u", "U":
			// Shared apply flow (active-downloads confirm included). Close
			// the overlay first so the confirmation/`Updating...` feedback is
			// visible on the main screen; a pending confirm re-arms via the
			// R U chord within the 5s window.
			cmd := a.applyUpdateAction()
			a.releaseNotesPopup.close()
			return a, cmd
		case "s", "S":
			// S skips exactly the version whose notes are on screen. The footer
			// offers it only while pending is set, and this keys off the same
			// flag: an UpdateStatusMsg landing while an arbitrary version's
			// notes are open would otherwise let S permanently skip a release
			// the operator has never seen, with no key on screen saying so.
			if !a.releaseNotesPopup.pending || a.updateAvailable == nil || a.OnDismissUpdate == nil {
				return a, nil
			}
			if a.updateAvailable.TagName != a.releaseNotesPopup.tag {
				return a, nil
			}
			tag := a.updateAvailable.TagName
			fn := a.OnDismissUpdate
			a.setFeedback("Skipping " + tag + "...")
			return a, safeCmd(func() tea.Msg { return dismissUpdateResultMsg{Tag: tag, Err: fn(tag)} })
		}
		// All other keys (arrows, pgup/pgdn) are forwarded to the viewport via
		// routeComponentMsg — nothing to do here.
		return a, nil
	}

	// FFmpeg check overlay takes priority over all other dialogs
	if a.ffmpegCheck.IsVisible() {
		action := a.ffmpegCheck.HandleKey(key)
		switch {
		case action == "skip":
			a.ffmpegCheck.Close()
			return a, nil
		case action == "quit":
			return a, tea.Quit
		case strings.HasPrefix(action, "prepare:"):
			method := strings.TrimPrefix(action, "prepare:")
			return a, tea.Batch(a.ffmpegPrepareCmd(method), spinnerTickCmd(a.ffmpegCheck.spinner))
		case strings.HasPrefix(action, "confirm:"):
			token := strings.TrimPrefix(action, "confirm:")
			return a, tea.Batch(a.ffmpegConfirmCmd(token), spinnerTickCmd(a.ffmpegCheck.spinner))
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
			return a, tea.Batch(a.ffmpegCheckCmd(path), spinnerTickCmd(a.ffmpegCheck.spinner))
		case action == "dismiss":
			// Overlay already closed by HandleKey — nothing else to do.
		}
		return a, nil
	}

	// Setup wizard
	if a.setupWiz.IsVisible() {
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
				var result cookies.SetupResult
				var errStr string
				if finishFn != nil {
					var err error
					result, err = finishFn()
					if err != nil {
						errStr = err.Error()
					}
				}
				return setupCookieFinishMsg{Platform: platform, Result: result, Err: errStr}
			}))
		}
		if a.setupWiz.cookieActive {
			cmds = append(cmds, spinnerTickCmd(a.setupWiz.spinner))
			// Arm the countdown only when no chain is already ticking —
			// appending one per keypress stacks chains and the countdown
			// would drain N+1 per second.
			if tick := a.setupWiz.armCookieTick(); tick != nil {
				cmds = append(cmds, tick)
			}
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
	if a.cookieImportDlg.IsVisible() {
		action, path := a.cookieImportDlg.HandleKey(key)
		if action == "import" {
			a.cookieImportDlg.SetImporting()
			return a, tea.Batch(a.importCookieFileCmd(path), a.cookieImportDlg.SpinnerInit())
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
				// A trim's progress overlay ticks at the fast class — start the loop now
				// (it's demand-driven and may be stopped if all jobs are terminal).
				return a, tea.Batch(a.createTrimCmd(jobID, startSec, endSec), spinnerTickCmd(a.trimDlg.spinner), a.ensureProgressTicking())
			}
		case "background":
			a.setFeedback("Trim encoding in background...")
		case "delete":
			if a.OnDeleteTrim != nil {
				trimID := a.trimDlg.SelectedTrimID()
				if trimID != "" {
					a.trimDlg.SetLoading(true)
					jobID := a.trimDlg.JobID()
					return a, tea.Batch(a.deleteTrimCmd(jobID, trimID), spinnerTickCmd(a.trimDlg.spinner))
				}
			}
		}
		return a, nil
	}
	if a.filesDlg.IsVisible() {
		action, data := a.filesDlg.HandleKey(msg)
		switch action {
		case "refresh":
			return a, tea.Batch(a.fetchOrphansCmd(), a.fetchOrphanedHistoryCmd(), a.filesDlg.SpinnerInit())
		case "delete":
			if sel := a.filesDlg.SelectedFile(); sel != nil {
				return a, a.deleteOrphanCmd(sel.Path)
			}
		case "delete-history":
			if sel := a.filesDlg.SelectedHistory(); sel != nil {
				return a, a.deleteHistoryEntryCmd(sel.VideoID)
			}
		case "delete-all-files":
			return a, tea.Batch(a.deleteAllOrphansCmd(data.([]string)), a.filesDlg.SpinnerInit())
		case "delete-all-history":
			return a, a.deleteAllHistoryCmd(data.([]string))
		}
		if cmd, ok := data.(tea.Cmd); ok && cmd != nil {
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
	// The whole KeyPressMsg for symmetry with the dialogs above, but this one
	// owns no component: HandleKey only decides. See YtdlpDialogModel.
	if a.ytdlpDlg.IsVisible() {
		switch a.ytdlpDlg.HandleKey(msg) {
		case "install":
			// The chord is gated on the STATUS callback, so the overlay can be
			// open with no way to install. Say so rather than swallowing I.
			if a.OnInstallYtdlpPlugin == nil {
				a.ytdlpDlg.SetError("Install is unavailable in this process")
				return a, nil
			}
			a.ytdlpDlg.SetInstalling()
			return a, tea.Batch(a.ytdlpInstallCmd(), a.ytdlpDlg.SpinnerInit())
		case "refresh":
			return a, tea.Batch(a.ytdlpStatusCmd(), a.ytdlpDlg.Open())
		}
		return a, nil
	}

	if a.statsDlg.IsVisible() {
		switch a.statsDlg.HandleKey(key) {
		case "refresh":
			// Re-fetch on the open session's epoch — the chain E T started
			// keeps ticking, and starting a second one here is what made
			// every r press double the polling.
			return a, tea.Batch(a.statsDlg.Open(), a.fetchStatsCmd(a.statsEpoch))
		case "close":
			// HandleKey already hid the overlay (esc/q). Retiring the epoch
			// orphans the session's pending tick and any in-flight fetch.
			a.statsEpoch++
		}
		return a, nil
	}

	// Log search intercept — must be before key normalization to preserve
	// case for N (shift+n) and before chord system to capture / and n/N.
	if a.focusedPanel == PanelLogs {
		if cmd, consumed := a.logs.HandleSearchKey(msg); consumed {
			return a, cmd
		}
		// "/" starts search (not in chord system, not during active search input)
		if key == "/" && !a.logs.IsSearching() {
			return a, a.logs.StartSearch()
		}
	}

	// Task-list search intercept — same shape as the log panel: capture keys
	// while the box is open (before normalization/chords), and "/" opens it.
	// All overlays/dialogs intercepted and returned above, so reaching here
	// with the Tasks panel focused means the main view owns the keyboard.
	if a.focusedPanel == PanelTasks {
		if cmd, consumed := a.taskList.HandleSearchKey(msg); consumed {
			return a, cmd
		}
		if key == "/" && !a.taskList.IsSearching() {
			return a, a.taskList.StartSearch()
		}
	}

	// Normalize single-character keys to lowercase (match TS: accepts both d/D, c/C, etc.)
	// Done AFTER dialog intercepts so text inputs preserve case.
	if len(key) == 1 && key[0] >= 'A' && key[0] <= 'Z' {
		key = strings.ToLower(key)
	}

	// Esc: clear batch selection if any (takes priority over chord reset)
	if key == keyEsc {
		if a.taskList.SelectedCount() > 0 {
			a.taskList.ClearSelection()
			// Also disarm any pending chord — leaving a confirm chord armed
			// would let the next keypress fire the destructive action
			// against the cursor job without its own confirmation.
			a.chord = chordState{}
			a.clearFeedback()
			return a, nil // consume the Esc, don't propagate
		}
		// Clear an active task-list search (box closed, query applied). The
		// box-open case is already consumed by HandleSearchKey above.
		if a.focusedPanel == PanelTasks && a.taskList.ClearSearch() {
			return a, nil
		}
		// Also clear any active chord prefix on Esc
		if a.chord.prefix != "" {
			a.chord = chordState{}
			a.clearFeedback()
			return a, nil
		}
		return a, nil
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
	case " ":
		// Space: toggle batch selection on focused task (only when task panel focused)
		if a.focusedPanel == PanelTasks {
			if job := a.taskList.SelectedJob(); job != nil {
				a.taskList.ToggleSelection(job.ID)
			}
		}
		return a, nil
	}

	// "c" clears the log view (history, filtered lines, and any active
	// search — the level filter stays) when the log panel is focused. The
	// search intercept above already consumes "c" while typing a query;
	// IsSearching() here is defense-in-depth, not the primary guard.
	if key == "c" && a.focusedPanel == PanelLogs && !a.logs.IsSearching() {
		a.logs.Clear()
		a.setFeedback("Log view cleared")
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
	// The four paging keys the list's KeyMap has always been configured for
	// and never received (CORE-16, O-X). Each refreshes the details panel
	// exactly as the arrow keys do, so the selected row and the panel beside
	// it cannot disagree.
	case keyPgUp:
		a.taskList.PrevPage()
		a.updateSelectedJob()
	case keyPgDown:
		a.taskList.NextPage()
		a.updateSelectedJob()
	case keyHome:
		a.taskList.GoToStart()
		a.updateSelectedJob()
	case keyEnd:
		a.taskList.GoToEnd()
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
					if item.SupportsBatch && a.taskList.SelectedCount() > 0 {
						break // batch mode — pass nil job so dispatchAction takes batch path
					}
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
	case "a", "r", "o", "e", "q":
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
		// Batch mode: if jobs are selected, confirm with count instead of
		// single job — only for chords dispatchAction implements batch for;
		// the rest fall through to the single-selected-job path.
		if item.SupportsBatch && a.taskList.SelectedCount() > 0 {
			a.chord.action = key
			a.chord.actionTime = time.Now()
			a.setFeedback(fmt.Sprintf("Press %s to confirm %s %d jobs (3s)",
				strings.ToUpper(key), strings.ToLower(item.HintLabel), a.taskList.SelectedCount()))
			return a, nil, true
		}
		job := a.taskList.SelectedJob()
		if job == nil || (item.JobFilter != nil && !item.JobFilter(job)) {
			a.rejectChordForJob(item)
			return a, nil, true
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
	// Batch mode: if jobs are selected, dispatch directly (batch handled in
	// dispatchAction) — only for chords that implement a batch path.
	if item.NeedsJob {
		if item.SupportsBatch && a.taskList.SelectedCount() > 0 {
			a.chord = chordState{}
			m, cmd := a.dispatchAction(chord, nil)
			return m, cmd, true
		}
		job := a.taskList.SelectedJob()
		if job == nil || (item.JobFilter != nil && !item.JobFilter(job)) {
			a.rejectChordForJob(item)
			return a, nil, true
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

// rejectChordForJob answers a registered NeedsJob chord whose selected job
// fails the item's JobFilter, or that was pressed with no job selected. The
// key is consumed, the chord resets to idle exactly as the invalid-chord path
// does, and the line says why — with the item's DisabledReason, the same
// words the action menu shows beside a greyed entry. Before this the key was
// swallowed in silence and the prefix stayed armed, so the operator's next
// press was read as a second key of a chord they thought had ended.
//
// The severity is stated: "no finished jobs in selection" has no word the
// fallback scan reads as a warning, and it would otherwise render in the
// success green.
func (a *App) rejectChordForJob(item *ActionMenuItem) {
	a.chord = chordState{}
	reason := item.DisabledReason
	if reason == "" {
		reason = "no eligible jobs"
	}
	a.setFeedbackWithSeverity(item.Label+": "+reason+" in selection", severityWarning)
}
