package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// afterSettingsClose re-applies what a save may have changed and nothing
// else re-reads: hide_finished_age_days, and the status bar's platform
// indicators (cookies.active_platforms), which otherwise followed only an
// auth transition — a platform switched off kept its indicator indefinitely.
// Read under the store lock — HTTP handlers mutate config via
// configStore.Update concurrently (matches getPort/apiBaseURL).
func (a *App) afterSettingsClose() {
	if a.configStore == nil {
		return
	}
	var days float64
	var ytActive, twActive bool
	a.configStore.Read(func(c *config.MoomboxConfig) {
		days = c.Monitors.HideFinishedAgeDays.Days()
		ytActive, twActive = config.GetActivePlatforms(c)
	})
	a.taskList.SetHideFinishedAgeDays(days)
	a.statusBar.SetActivePlatforms(ytActive, twActive)
	a.updateSelectedJob() // the rebuild can move the cursor
}

func (a *App) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// Ctrl+C ALWAYS quits — checked before ANY overlay intercept (O-M).
	// bubbletea v2 delivers Ctrl+C as a plain key (tea.InterruptMsg comes
	// only from a real SIGINT), and every overlay in the registry
	// (overlays.go) consumes every key before the rest of this function;
	// twelve of the fourteen there once swallowed the "Ctrl+C  Quit
	// immediately" help.go promises. No text input in the TUI binds Ctrl+C — both search boxes
	// already hand it back — so hoisting it costs nothing (CORE-5).
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

	// The size guard, after the Ctrl+C hoist so the wizard's cookie-slot
	// release above still runs (owner ruling R-C6). Below the compact floor
	// the screen is a notice and every other key is ignored: a chord typed
	// blind once cancelled a recording. Below the normal layout an overlay
	// that was already open is kept under a notice, and only Esc reaches it,
	// where the notice says it does (heldEscFor): never an Esc that would
	// quit, or raise or answer a prompt that is not on screen.
	switch a.layoutMode() {
	case layoutTooSmall:
		return a, nil
	case layoutCompact:
		if o := a.activeOverlay(); o != nil {
			if key != keyEsc || a.heldEscFor(o) == heldEscOff {
				return a, nil
			}
			return o.key(a, msg, key)
		}
	}

	if o := a.activeOverlay(); o != nil {
		return o.key(a, msg, key)
	}

	// Log search intercept — must be before key normalization to preserve
	// case for N (shift+n) and before chord system to capture / and n/N.
	if a.focusedPanel == PanelLogs {
		if cmd, consumed := a.logs.HandleSearchKey(msg); consumed {
			// Esc clearing an applied log search returns here, ahead of the
			// Esc branch below; every Esc disarms, or A D, Esc, D deletes.
			if key == keyEsc {
				a.cancelChord()
			}
			return a, cmd
		}
		// "/" starts search (not in chord system, not during active search input)
		// The search box swallows keys until Enter/Esc, so an armed chord
		// would outlive it and fire on the first key after; disarm it.
		if key == "/" && !a.logs.IsSearching() {
			a.cancelChord()
			return a, a.logs.StartSearch()
		}
	}

	// Task-list search intercept — same shape as the log panel: capture keys
	// while the box is open (before normalization/chords), and "/" opens it.
	// All overlays/dialogs intercepted and returned above, so reaching here
	// with the Tasks panel focused means the main view owns the keyboard.
	if a.focusedPanel == PanelTasks {
		if cmd, consumed := a.taskList.HandleSearchKey(msg); consumed {
			// Enter/Esc re-filter and put the cursor on the first match; the
			// details panel follows it, as it does for every other move.
			a.updateSelectedJob()
			return a, cmd
		}
		if key == "/" && !a.taskList.IsSearching() {
			a.cancelChord() // as on the log panel
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
			a.disarmChord()
			a.clearFeedback()
			return a, nil // consume the Esc, don't propagate
		}
		// Clear an active task-list search (box closed, query applied). The
		// box-open case is already consumed by HandleSearchKey above. Every
		// Esc disarms: returning here with a confirm armed let a lone D
		// within the window delete the cursor job.
		if a.focusedPanel == PanelTasks && a.taskList.ClearSearch() {
			a.cancelChord()
			a.updateSelectedJob()
			return a, nil
		}
		// Also clear any active chord prefix on Esc
		a.cancelChord()
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
		// The action menu is an overlay itself.
		if a.refuseOverlay() {
			return a, nil
		}
		a.actionMenu.SetSize(a.width, a.height)
		a.actionMenu.Open(a.buildMenuItems())
		return a, nil
	case "?":
		a.seenChordHint = true
		return a.dispatchAction("?", nil)
	case "`":
		a.seenChordHint = true
		return a.dispatchAction("`", nil)
	case keyTab, keyShiftTab:
		// The compact layout has the one panel: no focus to move.
		if a.layoutMode() == layoutNormal {
			delta := 1
			if key == keyShiftTab {
				delta = -1
			}
			a.cycleFocus(delta)
		}
		return a, nil
	case keySpace:
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

// disarmChord returns the chord machine to Idle. It is the one reset every
// path uses (timeout, confirm, invalid key, Esc, "/"); it leaves the feedback
// line alone, since most callers write their own next.
func (a *App) disarmChord() { a.chord = chordState{} }

// cancelChord disarms an armed chord and clears the prompt it left on the
// feedback line. A no-op while idle, so it never wipes unrelated feedback.
func (a *App) cancelChord() {
	if a.chord.prefix == "" {
		return
	}
	a.disarmChord()
	a.clearFeedback()
}

// setPrefixHint writes the hint of a prefix that has just been armed. Where
// everything under the prefix opens an overlay the layout cannot draw, the
// hint is the size refusal, in the refusal's colour.
func (a *App) setPrefixHint(prefix string) {
	hint, refused := a.chordHint(prefix)
	if refused {
		a.setFeedbackWithSeverity(hint, severityWarning)
		return
	}
	a.setFeedback(hint)
}

// handleChord processes the chord state machine. Returns (model, cmd, handled).
func (a *App) handleChord(key string) (tea.Model, tea.Cmd, bool) {
	now := time.Now()

	// Check timeout on active chord
	if a.chord.expired(now) {
		a.disarmChord()
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
					// The job the prompt named, not the cursor's (chordState.jobID).
					job = a.taskList.GetJobByID(a.chord.jobID)
					if job != nil && item.JobFilter != nil && !item.JobFilter(job) {
						job = nil // job status changed during confirm window
					}
					break
				}
			}
			a.disarmChord()
			m, cmd := a.dispatchAction(chord, job)
			return m, cmd, true
		}
		// Wrong key — show invalid chord and consume
		typed := strings.ToUpper(a.chord.prefix) + " " + strings.ToUpper(a.chord.action) + " " + strings.ToUpper(key)
		a.setFeedbackWithDuration("Invalid Chord: "+typed, time.Second)
		a.disarmChord()
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
		a.disarmChord()
		return a, nil, true
	}

	// No active chord — check if key is a prefix
	switch key {
	case "a", "r", "o", "e", "q":
		a.seenChordHint = true
		a.chord = chordState{prefix: key, prefixTime: now}
		a.setPrefixHint(key)
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

	// Refused before any job check or confirm: below the normal layout the
	// answer to a chord that opens an overlay is the same whatever is
	// selected (dispatchAction holds the same guard for every other route).
	if item.Opens != "" && a.refuseOverlay() {
		return a, nil, true
	}

	// NeedsConfirm + NeedsJob: check job first, then enter confirm step
	if item.NeedsConfirm && item.NeedsJob {
		// Batch mode: if jobs are selected, confirm with count instead of
		// single job — only for chords dispatchAction implements batch for;
		// the rest fall through to the single-selected-job path.
		if item.SupportsBatch && a.taskList.SelectedCount() > 0 {
			// Count what the batch will act on, as the Web's batch bar
			// does, not the whole selection: the prompt used to offer
			// "delete 1 jobs" for a selection of one Downloading job, whose
			// confirm then found nothing deletable.
			eligible := 0
			var eligibleIDs []string
			for _, id := range a.taskList.SelectedIDs() {
				if j := a.taskList.GetJobByID(id); j != nil && (item.JobFilter == nil || item.JobFilter(j)) {
					eligible++
					eligibleIDs = append(eligibleIDs, id)
				}
			}
			if eligible == 0 {
				a.rejectChordForJob(item)
				return a, nil, true
			}
			a.chord.action = key
			a.chord.actionTime = time.Now()
			a.setFeedback(fmt.Sprintf("Press %s to confirm %s %s%s (3s)",
				strings.ToUpper(key), strings.ToLower(item.HintLabel), jobCount(eligible), confirmDetail(item, eligibleIDs)))
			return a, nil, true
		}
		job := a.taskList.SelectedJob()
		if job == nil || (item.JobFilter != nil && !item.JobFilter(job)) {
			a.rejectChordForJob(item)
			return a, nil, true
		}
		a.chord.action = key
		a.chord.actionTime = time.Now()
		a.chord.jobID = job.ID
		// The clause leads the title: the line is one row cut to the
		// terminal's width, and a long title must not push what the action
		// deletes or cancels (owner ruling R-D2) off its end.
		if d := confirmDetail(item, []string{job.ID}); d != "" {
			a.setFeedback(fmt.Sprintf("Press %s to confirm %s%s — \"%s\" (3s)",
				strings.ToUpper(key), strings.ToLower(item.HintLabel), d, job.Title))
		} else {
			a.setFeedback(fmt.Sprintf("Press %s to confirm %s \"%s\" (3s)",
				strings.ToUpper(key), strings.ToLower(item.HintLabel), job.Title))
		}
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
			a.disarmChord()
			m, cmd := a.dispatchAction(chord, nil)
			return m, cmd, true
		}
		job := a.taskList.SelectedJob()
		if job == nil || (item.JobFilter != nil && !item.JobFilter(job)) {
			a.rejectChordForJob(item)
			return a, nil, true
		}
		a.disarmChord()
		m, cmd := a.dispatchAction(chord, job)
		return m, cmd, true
	}

	// Direct action (no job, no confirm)
	a.disarmChord()
	m, cmd := a.dispatchAction(chord, nil)
	return m, cmd, true
}

// confirmDetail is item's ConfirmDetail for ids as the confirm line's
// clause (": deletes …"), "" when the item has none or it says nothing.
func confirmDetail(item *ActionMenuItem, ids []string) string {
	if item.ConfirmDetail == nil {
		return ""
	}
	if d := item.ConfirmDetail(ids); d != "" {
		return ": " + d
	}
	return ""
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
	a.disarmChord()
	reason := item.DisabledReason
	if reason == "" {
		reason = "no eligible jobs"
	}
	a.setFeedbackWithSeverity(item.Label+": "+reason+" in selection", severityWarning)
}
