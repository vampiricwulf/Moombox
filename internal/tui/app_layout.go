package tui

import (
	"image/color"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/vampiricwulf/Moombox/internal/config"
)

func (a *App) recalcLayout() {
	// Status bar is 1 row at the bottom
	contentH := a.height - 1

	// Persistent banners sit above the panels — subtract their rendered
	// height (they can wrap on narrow terminals) and shift the mouse
	// regions down by the same amount. Must stay in step with the banner
	// block in View(); a banner that isn't accounted for here renders the
	// frame taller than the terminal.
	bannerH := 0
	if a.width > 0 {
		if a.restartPending {
			bannerH += lipgloss.Height(restartBanner(a.width))
		}
		if warn := a.securityBannerText(); warn != "" {
			bannerH += lipgloss.Height(securityBanner(a.width, warn))
		}
		contentH -= bannerH
	}

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
	switch a.focusedPanel {
	case PanelTasks:
		taskW = a.width * 45 / 100 // TS: 45% for tasks when focused
	case PanelDetails:
		taskW = a.width * 35 / 100
	default:
		taskW = a.width / 2
	}
	detailW = a.width - taskW

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
	if a.releaseNotesPopup != nil {
		a.releaseNotesPopup.setSize(a.width, a.height)
	}

	// Store regions for mouse (offset by the restart banner when shown)
	a.taskRegion = PanelRegion{X: 0, Y: bannerH, Width: taskW, Height: topH}
	a.detailRegion = PanelRegion{X: taskW, Y: bannerH, Width: detailW, Height: topH}
	a.logRegion = PanelRegion{X: 0, Y: bannerH + topH, Width: a.width, Height: logH}
}

// viewWithMode wraps content in a tea.View with standard terminal mode settings.
func (a *App) viewWithMode(content string) tea.View {
	v := tea.NewView(content)
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = a.windowTitle
	return v
}

// View implements tea.Model.
func (a *App) View() tea.View {
	if a.width == 0 || a.height == 0 {
		return a.viewWithMode("Initializing...")
	}

	// Render overlays on top (priority matches handleKey order)
	if a.settings.IsVisible() {
		return a.viewWithMode(a.settings.View())
	}
	if a.help.IsVisible() {
		return a.viewWithMode(a.help.View())
	}
	if a.releaseNotesPopup != nil && a.releaseNotesPopup.isOpen() {
		return a.viewWithMode(a.releaseNotesPopup.View())
	}
	if a.ffmpegCheck.IsVisible() {
		return a.viewWithMode(a.ffmpegCheck.View())
	}
	if a.setupWiz.IsVisible() {
		return a.viewWithMode(a.setupWiz.View())
	}
	if a.actionMenu.IsVisible() {
		return a.viewWithMode(a.actionMenu.View())
	}
	if a.importDlg.IsVisible() {
		return a.viewWithMode(a.importDlg.View())
	}
	if a.addVideo.IsVisible() {
		return a.viewWithMode(a.addVideo.View())
	}
	if a.trimDlg.IsVisible() {
		return a.viewWithMode(a.trimDlg.View())
	}
	if a.filesDlg.IsVisible() {
		return a.viewWithMode(a.filesDlg.View())
	}
	if a.clientTokensDlg.IsVisible() {
		return a.viewWithMode(a.clientTokensDlg.View())
	}

	// Sync status bar state before rendering
	a.statusBar.ShowChordHint = !a.seenChordHint
	a.statusBar.SelectedCount = a.taskList.SelectedCount()

	// Top row: task list + details
	topRow := lipgloss.JoinHorizontal(lipgloss.Top,
		a.taskList.View(),
		a.details.View(),
	)

	// Main content
	mainParts := []string{}
	if a.restartPending {
		mainParts = append(mainParts, restartBanner(a.width))
	}
	if warn := a.securityBannerText(); warn != "" {
		mainParts = append(mainParts, securityBanner(a.width, warn))
	}
	mainParts = append(mainParts, topRow, a.logs.View(), a.statusBar.View())
	content := lipgloss.JoinVertical(lipgloss.Left, mainParts...)

	// Feedback / confirmation messages
	if a.feedbackMsg != "" {
		msgColor := feedbackColor(a.feedbackMsg)
		content = addOverlayMessage(content, a.width,
			lipgloss.NewStyle().Foreground(msgColor).Render(a.feedbackMsg),
		)
	}

	return a.viewWithMode(content)
}

// restartBanner renders the persistent "restart required" banner shown above
// the main TUI content. Stays visible until the process actually restarts —
// the audit's specific concern is that dismissing the settings modal with
// Esc previously left a config/runtime mismatch silently. Audit
// reports/tui.md #26.
func restartBanner(width int) string {
	if width <= 0 {
		return ""
	}
	style := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#000000")).
		Background(ColorYellow).
		Bold(true).
		Padding(0, 1).
		Width(width)
	return style.Render("⚠ Restart required — saved config differs from running process. Press ` then Save & Restart, or restart Moombox.")
}

// securityBannerText returns the persistent security warning shown above the
// main content, or "" when the running config is not in a warned state.
// Single condition today: external/public network access with no dashboard
// password. Every interactive surface refuses to SET that combination
// (settings.go, config API, setup wizard), so it can only come from a
// hand-edited config file — policy is block-set / warn-boot, and this banner
// plus the twin startup log warning in web.Server.Start is the warn-boot
// half. Reads the store per render; a single uncontended RLock is noise next
// to the render cost.
func (a *App) securityBannerText() string {
	if a.configStore == nil {
		return ""
	}
	var access, hash string
	a.configStore.Read(func(c *config.MoomboxConfig) {
		access = c.Network.NetworkAccess
		hash = c.Network.PasswordHash
	})
	if (access == "external" || access == "public") && hash == "" {
		return "⚠ SECURITY: network_access is \"" + access + "\" with no dashboard password — every reachable IP has full control. Set a password (` → Network) or lower network_access."
	}
	return ""
}

// securityBanner renders the passwordless-external warning with the same
// persistent-banner treatment as restartBanner, in red.
func securityBanner(width int, msg string) string {
	if width <= 0 {
		return ""
	}
	style := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#FFFFFF")).
		Background(ColorRed).
		Bold(true).
		Padding(0, 1).
		Width(width)
	return style.Render(msg)
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
func feedbackColor(msg string) color.Color {
	lower := strings.ToLower(msg)

	// Chord feedback (yellow) — prefix match on known chord categories
	if strings.HasPrefix(msg, "Press ") || strings.HasPrefix(msg, "Action:") ||
		strings.HasPrefix(msg, "Request:") || strings.HasPrefix(msg, "Open:") ||
		strings.HasPrefix(msg, "Quit:") {
		return ColorYellow
	}

	// Errors (red) — cancelled jobs, invalid chords, or any failure
	if strings.HasPrefix(msg, "Cancelled:") || strings.HasPrefix(msg, "Invalid Chord:") ||
		strings.Contains(lower, "failed") {
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
		return ColorYellow
	}

	// Default: success (green)
	return ColorGreen
}
