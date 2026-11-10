package tui

import tea "charm.land/bubbletea/v2"

// mouseActionMenu routes the mouse to the action menu while it is on top.
func (a *App) mouseActionMenu(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	a.actionMenu.HandleMouse(msg)
	return a, nil
}

// mouseSettings routes the mouse to settings while it is on top.
func (a *App) mouseSettings(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	a.settings.HandleMouse(msg)
	// A click on [ Save & Return ] or [ Return ] closes the overlay
	// without passing through handleKey's "close" action, and a save
	// may have changed the archive threshold the task list applies.
	if !a.settings.IsVisible() {
		a.afterSettingsClose()
	}
	// [ Save & Return ] can be the save that removes a channel with "delete
	// its pending jobs".
	return a, a.channelPruneCmd()
}

// mouseJobLog: the O L overlay scrolls under the wheel as the log panel
// does, three rows a notch; a click does nothing there.
func (a *App) mouseJobLog(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if isScrollUp(msg) {
		a.jobLog.Scroll(-3)
	} else if isScrollDown(msg) {
		a.jobLog.Scroll(3)
	}
	return a, nil
}

// wheelDir is the direction of a wheel notch: -1 up, 1 down, 0 for any
// other mouse event (a click, a release, motion, a sideways notch).
func wheelDir(msg tea.MouseMsg) int {
	switch {
	case isScrollUp(msg):
		return -1
	case isScrollDown(msg):
		return 1
	}
	return 0
}

// overlayWheelRows is how far a wheel notch scrolls a text overlay, the same
// three rows it scrolls the log and details panels.
const overlayWheelRows = 3

// The text overlays scroll under the wheel, three rows a notch; a click does
// nothing there (owner ruling R-C2).

func (a *App) mouseHelp(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	a.help.Scroll(wheelDir(msg) * overlayWheelRows)
	return a, nil
}

func (a *App) mouseReleaseNotes(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	a.releaseNotesPopup.scroll(wheelDir(msg) * overlayWheelRows)
	return a, nil
}

// mouseFFmpegCheck: only the install-script review is text to scroll. The
// overlay's other screens are a menu and a path field, where the wheel does
// nothing.
func (a *App) mouseFFmpegCheck(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	a.ffmpegCheck.ScrollReview(wheelDir(msg) * overlayWheelRows)
	return a, nil
}

// wheelAsArrow is the wheel in a list or table overlay (owner ruling R-C2):
// a notch moves the cursor one row, by being the Up or Down key the overlay
// already handles. The key takes the route App.Update gives a real one, the
// overlay's components and then its key handler, so the cursor, whatever
// the overlay does when it moves (a retracted delete confirm, a skipped
// section header) and the scroll that follows it are the arrow key's own.
// ok is the overlay's answer to "is a list on screen for the wheel to move
// through"; a click, and a notch while ok is false, do nothing.
func (a *App) wheelAsArrow(msg tea.MouseMsg, ok func(dir int) bool) (tea.Model, tea.Cmd) {
	dir := wheelDir(msg)
	o := a.activeOverlay()
	if dir == 0 || o == nil || !ok(dir) {
		return a, nil
	}
	key := tea.KeyPressMsg{Code: tea.KeyDown}
	if dir < 0 {
		key.Code = tea.KeyUp
	}
	var cmds []tea.Cmd
	if o.components != nil {
		cmds = append(cmds, o.components(a, key))
	}
	_, cmd := o.key(a, key, key.String())
	return a, tea.Batch(append(cmds, cmd)...)
}

// mouseSetupWizard: the wheel moves through the channel list. The wizard's
// other screens are forms and three-row menus.
func (a *App) mouseSetupWizard(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	return a.wheelAsArrow(msg, a.setupWiz.wheelMovesChannel)
}

// mouseImport: the wheel moves through the file picker, not the title and
// channel fields that follow it.
func (a *App) mouseImport(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	return a.wheelAsArrow(msg, func(int) bool { return a.importDlg.step == importStepPick })
}

// mouseChapterImport: with the pointer below the paste box, the wheel
// scrolls a preview that has more chapters than fit, one row a notch.
// Otherwise it moves the paste box's cursor a line, which scrolls a paste
// longer than the box.
func (a *App) mouseChapterImport(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if dir := wheelDir(msg); dir != 0 && a.chapterDlg.WheelPreview(msg.Mouse().Y, dir) {
		return a, nil
	}
	return a.wheelAsArrow(msg, func(int) bool { return a.chapterDlg.wheelMovesPaste() })
}

// mouseAddVideo: the wheel moves through the format table of the step on
// screen. Like the arrow keys, the first notch on a table still on Auto
// shows its cursor.
func (a *App) mouseAddVideo(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	return a.wheelAsArrow(msg, func(int) bool { return a.addVideo.wheelMovesFormat() })
}

// mouseTrim: the wheel moves through the delete list and stops at its ends.
func (a *App) mouseTrim(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	return a.wheelAsArrow(msg, a.trimDlg.wheelMovesTrim)
}

// mouseFiles and mouseClientTokens: the wheel moves through the list. Each
// dialog's key handler ignores navigation while it is loading or empty.
func (a *App) mouseFiles(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	return a.wheelAsArrow(msg, func(int) bool { return true })
}

func (a *App) mouseClientTokens(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	return a.wheelAsArrow(msg, func(int) bool { return true })
}

func (a *App) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	// A notice has replaced the screen (app_compact.go): there is nothing
	// under the pointer to click or scroll.
	if a.screenHeld() {
		return a, nil
	}

	// The overlay on top takes the mouse — the same one a key would reach
	// (the registry order: overlays.go). One with no mouse handler
	// swallows it; the handlers above scroll or move a cursor on the wheel
	// and leave a click alone. Ahead of the compact layout's own mouse: an
	// overlay drawn bare covers the task list, and a wheel notch over it
	// must not move the list underneath.
	if o := a.activeOverlay(); o != nil {
		if o.mouse == nil {
			return a, nil
		}
		return o.mouse(a, msg)
	}
	if a.layoutMode() == layoutCompact {
		return a.handleCompactMouse(msg)
	}

	m := msg.Mouse()
	x, y := m.X, m.Y

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

		// Scroll the panel under the cursor, not the focused panel
		var targetPanel FocusPanel
		switch {
		case a.taskRegion.Contains(x, y):
			targetPanel = PanelTasks
		case a.detailRegion.Contains(x, y):
			targetPanel = PanelDetails
		case a.logRegion.Contains(x, y):
			targetPanel = PanelLogs
		default:
			return a, nil
		}

		switch targetPanel {
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

// cycleFocus moves focus to the next panel (Tab) or, with delta -1, the
// previous one (Shift-Tab).
func (a *App) cycleFocus(delta int) {
	a.setFocus(FocusPanel((int(a.focusedPanel) + delta + 3) % 3))
}

func (a *App) setFocus(panel FocusPanel) {
	prevPanel := a.focusedPanel
	a.focusedPanel = panel
	a.taskList.SetFocused(panel == PanelTasks)
	a.details.SetFocused(panel == PanelDetails)
	a.logs.SetFocused(panel == PanelLogs)
	// Re-enable log auto-scroll when focus leaves the logs (match TS) — by
	// Tab, Shift-Tab or a click alike.
	if prevPanel == PanelLogs && panel != PanelLogs {
		a.logs.ReEnableAutoScroll()
	}
	a.recalcLayout()
}
