package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

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
	a.importDlg.SetSize(a.width, a.height)
	a.trimDlg.SetSize(a.width, a.height)
	a.filesDlg.SetSize(a.width, a.height)
	a.clientTokensDlg.SetSize(a.width, a.height)
	a.setupWiz.SetSize(a.width, a.height)
	a.ffmpegCheck.SetSize(a.width, a.height)
	a.actionMenu.SetSize(a.width, a.height)
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
		msgColor := feedbackColor(a.feedbackMsg)
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

// feedbackColor returns the display color for a feedback message.
// Order: chord (yellow) > error (red) > neutral (gray) > warning (yellow) > success (green).
func feedbackColor(msg string) lipgloss.Color {
	lower := strings.ToLower(msg)

	// Chord feedback (yellow) — prefix match on known chord categories
	if strings.HasPrefix(msg, "Press ") || strings.HasPrefix(msg, "Action:") ||
		strings.HasPrefix(msg, "Request:") || strings.HasPrefix(msg, "Open:") ||
		strings.HasPrefix(msg, "Quit:") {
		return lipgloss.Color("#f1c40f")
	}

	// Errors (red) — cancelled jobs or any failure
	if strings.HasPrefix(msg, "Cancelled:") || strings.Contains(lower, "failed") {
		return ColorRed
	}

	// Deletions (gray)
	if strings.Contains(lower, "deleted:") {
		return ColorGray
	}

	// Warnings (yellow) — inability, conditions, advisory messages
	if strings.HasPrefix(msg, "Can only") || strings.HasPrefix(msg, "Trim only") ||
		strings.HasPrefix(msg, "No update") || strings.HasPrefix(msg, "No stream") ||
		strings.HasPrefix(msg, "A trim is already") ||
		strings.Contains(lower, "no cookies acquired") ||
		strings.Contains(lower, "not authenticated") ||
		strings.Contains(lower, "no platforms") ||
		strings.Contains(lower, "already exists") ||
		strings.HasPrefix(msg, "Already up to date") {
		return lipgloss.Color("#f1c40f")
	}

	// Default: success (green)
	return ColorGreen
}
