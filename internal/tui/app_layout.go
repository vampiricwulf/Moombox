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
	if a.feedback.msg != "" {
		msgColor := feedbackColor(a.feedback.msg, a.feedback.sev)
		content = addOverlayMessage(content, a.width,
			lipgloss.NewStyle().Foreground(msgColor).Render(a.feedback.msg),
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

// feedbackSeverity is what the COMPOSER of a feedback line knew about it, as
// opposed to what a scan of the finished sentence can guess.
//
// THE SCAN IS DOWNSTREAM OF TWO LOSSY STEPS and that is why this type exists.
// feedbackColor reads a.feedback.msg, which is the line AFTER fitFeedback has
// clamped it to the terminal width — so at 40 columns
//
//	"Cookies: YouTube OK | Last cookie error: the browser profile held no…"
//
// arrives as "Cookies: YouTube OK | Last cookie err…", the marker the warning
// branch matches on is gone, and a line announcing a recorded failure renders
// in the SUCCESS colour. An operator in a split pane presses R C, is told their
// cookies are fine, and the browser refresh has been failing for days. The
// second step is subtler: precedence in the scan is decided by branch order
// over the whole string, so an appended clause can move the colour in either
// direction — the gray "deleted:" branch sits above the warning branch, so a
// recorded error whose words happened to contain it would render NEUTRAL.
//
// Both are the same defect: severity was being re-derived from prose by a
// reader that never saw the facts. The composer HAS the facts — the verdicts
// and whether anything was recorded — so it states the severity and the reader
// obeys it. The scan survives as the fallback for the forty-odd call sites that
// state nothing, where it is the only thing available and where the strings are
// composed from bounded vocabulary that it handles correctly.
type feedbackSeverity int

const (
	// severityUnstated is the ZERO VALUE, and that placement is load-bearing:
	// every setFeedback / setFeedbackWithDuration call site writes it without
	// naming it, so adding this type changed the colour of exactly nothing.
	severityUnstated feedbackSeverity = iota
	severitySuccess
	severityWarning
	severityError
)

// color maps a stated severity to its display colour, reporting false when
// nothing was stated. Deliberately has no gray: gray is a CATEGORY (a deletion
// happened), not a severity, and no composer that states a severity is
// reporting one.
func (s feedbackSeverity) color() (color.Color, bool) {
	switch s {
	case severitySuccess:
		return ColorGreen, true
	case severityWarning:
		return ColorYellow, true
	case severityError:
		return ColorRed, true
	default:
		return nil, false
	}
}

// feedbackColor returns the display color for a feedback message.
//
// A STATED severity wins outright. Everything below it is the fallback for
// messages that state nothing — see feedbackSeverity for what the fallback
// cannot see and why that mattered.
//
// Fallback order: chord (yellow) > error (red) > neutral (gray) > warning
// (yellow) > success (green).
func feedbackColor(msg string, stated feedbackSeverity) color.Color {
	if c, ok := stated.color(); ok {
		return c
	}

	lower := strings.ToLower(msg)

	// Chord feedback (yellow) — prefix match on known chord categories
	if strings.HasPrefix(msg, "Press ") || strings.HasPrefix(msg, "Action:") ||
		strings.HasPrefix(msg, "Request:") || strings.HasPrefix(msg, "Open:") ||
		strings.HasPrefix(msg, "Quit:") {
		return ColorYellow
	}

	// Errors (red) — cancelled jobs, invalid chords, or any failure
	if strings.HasPrefix(msg, "Cancelled:") || strings.HasPrefix(msg, "Invalid Chord:") ||
		strings.Contains(lower, "failed") ||
		// A CONCLUSIVE verdict, and it belongs on this side of the line.
		// cookies.RecheckReport words RefreshFailed as "<platform> not
		// authenticated" and RefreshUnknown as "<platform> — could not
		// establish", so this substring is reachable only from a check that
		// REACHED the site and was refused. R F's wording for the same state
		// ("...ran and auth verification failed") already landed here on the
		// "failed" substring above, so R C rendering yellow made one surface
		// answer one fact at two severities.
		//
		// Red is the actionable end: the remedy is to re-export credentials.
		// Yellow is reserved for "we could not check", which asks for nothing.
		// A MIXED line — one platform refused, the other unreachable — is red
		// too, and correctly: the conclusive half is the half to act on, which
		// is the same precedence the status-bar badge and the dashboard toast
		// already apply.
		//
		// R C NO LONGER DEPENDS ON THIS, and the reason is the same one that
		// moved the LastError clause below: at 30 columns the clamp leaves
		// "Cookies: YouTube not authen…" and this substring is gone, so the
		// conclusive refusal rendered GREEN. cookieRecheckFeedback states
		// severityError for RefreshFailed and the stated severity wins at the
		// top of this function. The entry stays as the fallback for factless
		// callers and is exercised as such by TestFeedbackColorErrorMessages;
		// the rendered R C line is pinned by
		// TestRecheckColourSurvivesTheClampUnchanged.
		strings.Contains(lower, "not authenticated") {
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
		// The two "we did not find out" cookie-refresh outcomes. They must
		// NOT reach the error branch above: a pass that declined to run, or
		// that ran without concluding, has established nothing about the
		// credentials, and a red line is an alarm the operator then has to
		// chase. Only a conclusive verdict says "failed" and earns red.
		strings.Contains(lower, "declined to run") ||
		strings.Contains(lower, "could not establish") ||
		strings.Contains(lower, "no platforms") ||
		// The AutoCookieStatus.LastError clause R C appends (see
		// cookieRecheckFeedback), kept as a FALLBACK and no longer as the
		// guard. cookieRecheckFeedback states severityWarning or higher
		// whenever it appends this clause, and a stated severity wins above —
		// which is what actually delivers the property, because this branch
		// cannot: the marker is at the END of the line and fitFeedback has
		// already truncated it away on any terminal narrower than about 42
		// columns, at which point the line falls through to green.
		//
		// It stays because it costs nothing and because a future composer that
		// writes this clause without stating a severity would otherwise get the
		// green default. It is exercised in that factless domain by
		// TestFeedbackColorWarningMessages; the domain that matters is pinned
		// by TestLastCookieErrorNeverLowersSeverity, which now renders through
		// the real clamp.
		strings.Contains(lower, "last cookie error") ||
		strings.Contains(lower, "already exists") ||
		strings.HasPrefix(msg, "Already up to date") {
		return ColorYellow
	}

	// Default: success (green)
	return ColorGreen
}
