package tui

import tea "github.com/charmbracelet/bubbletea"

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
