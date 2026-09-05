package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/vampiricwulf/Moombox/internal/web/routes"
)

// YtdlpDialogModel is the R Y overlay: what the yt-dlp PO-token plugin looks
// like from here — installed or not, where, and whether the port it was
// written for is still the port this process serves on — plus the one key
// that fixes the last of those.
//
// It renders routes.YtdlpPluginInfo, the SAME value GET
// /api/ytdlp-plugin/status returns, rather than a terminal-shaped
// re-derivation: "installed" and "mismatched" are verdicts about a file on
// disk, and the dashboard and the TUI disagreeing about them would be a bug
// with no owner.
//
// It owns no textinput. App.Update's keypress arm calls routeComponentMsg AND
// handleKey for the same message, so a dialog that feeds a component from
// HandleKey feeds it twice (see CookieImportDialogModel's note); here
// UpdateComponents drives the spinner and HandleKey only decides.
type YtdlpDialogModel struct {
	visible       bool
	width, height int
	loading       bool
	installing    bool
	info          routes.YtdlpPluginInfo
	errorMsg      string
	spinner       spinner.Model
}

// NewYtdlpDialogModel creates the yt-dlp plugin overlay.
func NewYtdlpDialogModel() *YtdlpDialogModel {
	return &YtdlpDialogModel{spinner: newSpinner()}
}

// IsVisible returns true if the dialog is shown.
func (m *YtdlpDialogModel) IsVisible() bool { return m.visible }

// SetSize updates the dialog dimensions.
func (m *YtdlpDialogModel) SetSize(w, h int) {
	m.width, m.height = w, h
}

// Open shows the dialog in its loading state and returns the spinner's first
// tick. Also the refresh path: R re-opens rather than inventing a second
// "reloading" state.
func (m *YtdlpDialogModel) Open() tea.Cmd {
	m.visible = true
	m.loading = true
	m.installing = false
	m.errorMsg = ""
	m.spinner = newSpinner()
	return spinnerTickCmd(m.spinner)
}

// Close hides the dialog.
func (m *YtdlpDialogModel) Close() { m.visible = false }

// SetInstalling moves to the installing state. The App fires the command;
// this only says what the overlay shows while it runs.
func (m *YtdlpDialogModel) SetInstalling() {
	m.installing = true
	m.errorMsg = ""
}

// SetStatus renders a freshly read status, ending whichever wait was running.
func (m *YtdlpDialogModel) SetStatus(info routes.YtdlpPluginInfo) {
	m.loading = false
	m.installing = false
	m.errorMsg = ""
	m.info = info
}

// SetError renders a message in place of the status rows and ends the wait.
func (m *YtdlpDialogModel) SetError(msg string) {
	m.loading = false
	m.installing = false
	m.errorMsg = msg
}

// SpinnerInit returns the spinner's initial tick command.
func (m *YtdlpDialogModel) SpinnerInit() tea.Cmd { return spinnerTickCmd(m.spinner) }

// UpdateComponents ticks the spinner while either wait is in flight.
func (m *YtdlpDialogModel) UpdateComponents(msg tea.Msg) tea.Cmd {
	if !m.visible || (!m.loading && !m.installing) {
		return nil
	}
	var cmd tea.Cmd
	m.spinner, cmd = m.spinner.Update(msg)
	return cmd
}

// HandleKey processes one keypress and returns "close", "install", "refresh"
// or "". It NEVER feeds a component — see the type comment.
func (m *YtdlpDialogModel) HandleKey(msg tea.KeyPressMsg) string {
	switch msg.String() {
	case keyEsc, "q", "Q":
		m.Close()
		return "close"
	case "i", "I":
		if m.loading || m.installing {
			return ""
		}
		return "install"
	case "r", "R":
		if m.loading || m.installing {
			return ""
		}
		return "refresh"
	}
	return ""
}

// View renders the overlay.
func (m *YtdlpDialogModel) View() string {
	if !m.visible {
		return ""
	}

	boxW, _ := dialogBox(70, m.width)
	boxH := max(min(m.height-4, 20), 12)

	var b strings.Builder
	b.WriteString(TitleStyle.Render("yt-dlp Plugin") + "\n\n")

	switch {
	case m.installing:
		b.WriteString("  " + m.spinner.View() + " Installing...\n")
	case m.loading:
		b.WriteString("  " + m.spinner.View() + " Loading...\n")
	case m.errorMsg != "":
		b.WriteString(ErrorStyle.Render("  "+m.errorMsg) + "\n")
	default:
		b.WriteString(m.statusRows())
	}

	b.WriteString("\n" + DimStyle.Render("I: Install / reinstall   R: Refresh   Esc: Close"))

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorCyan).
		Width(boxW).
		Height(boxH).
		Render(b.String())

	return centerBox(box, m.width, m.height)
}

// statusRows is the loaded body: the four facts that are always true and the
// three that only exist in some states.
func (m *YtdlpDialogModel) statusRows() string {
	installed := "not installed"
	if m.info.Installed {
		installed = "yes"
	}
	https := "off"
	if m.info.HTTPSEnabled {
		https = "on"
	}

	var b strings.Builder
	b.WriteString(ytdlpRow("Installed:", installed))
	b.WriteString(ytdlpRow("Plugin dir:", ytdlpValueOrDash(m.info.PluginDir)))
	b.WriteString(ytdlpRow("Moombox port:", fmt.Sprintf("%d (https: %s)", m.info.CurrentPort, https)))
	// Gated on the PORT, not on Installed: InstalledPort is non-zero only
	// when a plugin file actually parsed, and it is the number the mismatch
	// row is about — a mismatch whose other half is not on screen is not an
	// explanation.
	if m.info.InstalledPort > 0 {
		b.WriteString(ytdlpRow("Plugin points:", fmt.Sprintf("%d", m.info.InstalledPort)))
	}
	if m.info.PortMismatch {
		b.WriteString(YellowStyle.Render(fmt.Sprintf("  %-15s %s", "Port mismatch:", "yes — press I to rewrite the plugin for the current port")) + "\n")
	}
	if m.info.ExtractedPath != "" {
		b.WriteString(ytdlpRow("Plugin file:", m.info.ExtractedPath))
	}
	return b.String()
}

func ytdlpRow(label, value string) string {
	return fmt.Sprintf("  %-15s %s\n", label, value)
}

// ytdlpValueOrDash keeps the dir row present when the platform has no
// standard yt-dlp plugin directory — an empty value there reads as a render
// bug rather than as the fact it is.
func ytdlpValueOrDash(v string) string {
	if v == "" {
		return "— (no standard yt-dlp plugin directory on this platform)"
	}
	return v
}
