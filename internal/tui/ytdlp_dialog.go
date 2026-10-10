package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/vampiricwulf/Moombox/internal/ytdlpplugin"
)

// YtdlpDialogModel is the E Y overlay: what the yt-dlp PO-token plugin looks
// like from here — installed or not, where, and whether the port it was
// written for is still the port this process serves on — plus the one key
// that fixes the last of those.
//
// It renders ytdlpplugin.Info, the SAME value GET /api/ytdlp-plugin/status
// returns, rather than a terminal-shaped re-derivation: "installed" and "mismatched" are verdicts about a file on
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
	info          ytdlpplugin.Info
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
func (m *YtdlpDialogModel) SetStatus(info ytdlpplugin.Info) {
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
	b.WriteString(TitleStyle.Render("yt-dlp Plugin"))
	b.WriteString("\n\n")

	switch {
	case m.installing:
		b.WriteString("  ")
		b.WriteString(m.spinner.View())
		b.WriteString(" Installing...\n")
	case m.loading:
		b.WriteString("  ")
		b.WriteString(m.spinner.View())
		b.WriteString(" Loading...\n")
	case m.errorMsg != "":
		b.WriteString(ErrorStyle.Render("  " + m.errorMsg))
		b.WriteString("\n")
	default:
		b.WriteString(m.statusRows(boxW - 2))
	}

	b.WriteString("\n")
	b.WriteString(DimStyle.Render("I: Install / reinstall   R: Refresh   Esc/Q: Close"))

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorCyan).
		Width(boxW).
		Height(boxH).
		Render(b.String())

	return centerBox(box, m.width, m.height)
}

// statusRows is the loaded body: the three facts that are always on screen
// (Installed, Plugin dir, Moombox port) and the four that only exist in some
// states (Plugin points, Plugin state, Port mismatch, Plugin path).
func (m *YtdlpDialogModel) statusRows(contentW int) string {
	installed := "not installed"
	if m.info.Installed {
		installed = "yes"
	}
	https := "off"
	if m.info.HTTPSEnabled {
		https = "on"
	}

	var b strings.Builder
	b.WriteString(ytdlpRow("Installed:", installed, contentW))
	b.WriteString(ytdlpRow("Plugin dir:", ytdlpValueOrDash(m.info.PluginDir), contentW))
	b.WriteString(ytdlpRow("Moombox port:", fmt.Sprintf("%d (https: %s)", m.info.CurrentPort, https), contentW))
	// Gated on the PORT, not on Installed: InstalledPort is non-nil only when
	// a plugin file actually parsed, and it is the number the mismatch row is
	// about — a mismatch whose other half is not on screen is not an
	// explanation. The nil case is also the wire's "installedPort": null.
	if m.info.InstalledPort != nil {
		// With the scheme: a mismatch in the scheme alone (https toggled, the
		// port unchanged) otherwise showed two equal port numbers.
		points := fmt.Sprintf("%d", *m.info.InstalledPort)
		if m.info.InstalledScheme != "" {
			pluginHTTPS := "off"
			if m.info.InstalledScheme == "https" {
				pluginHTTPS = "on"
			}
			points = fmt.Sprintf("%d (https: %s)", *m.info.InstalledPort, pluginHTTPS)
		}
		b.WriteString(ytdlpRow("Plugin points:", points, contentW))
	}
	if m.info.Unparseable {
		// Same shape as the mismatch row below — YellowStyle, label column, one
		// short value that fits the 66-column content box — because it is the
		// same kind of fact: something about the file on disk is wrong and I is
		// what fixes it.
		//
		// Its own label, NOT the path row's: Status fills ExtractedPath
		// whenever the plugin dir is known, and Installed can only become true
		// inside that same block, so the path row is on screen in every real
		// occurrence of this state. Two rows reading "Plugin file:" would be a
		// render bug to anyone reading them.
		b.WriteString(YellowStyle.Render(fmt.Sprintf("  %-15s %s", "Plugin state:", "not recognized — I reinstalls it")))
		b.WriteString("\n")
	}
	if m.info.PortMismatch {
		// Short on purpose: the label column plus this value has to fit the
		// 66-column content box, or the sentence wraps with a dangling second
		// line at every width up to ~88.
		// Not "Port mismatch": PortMismatch also covers the scheme, and the
		// two rows above now show both halves of each.
		b.WriteString(YellowStyle.Render(fmt.Sprintf("  %-15s %s", "Mismatch:", "yes — I rewrites it to match")))
		b.WriteString("\n")
	}
	if m.info.ExtractedPath != "" {
		// "path", not "file": ExtractedPath is the plugin DIRECTORY the manual
		// --plugin-dirs invocation takes, not the .py the rows above are about.
		b.WriteString(ytdlpRow("Plugin path:", m.info.ExtractedPath, contentW))
	}
	return b.String()
}

// ytdlpRowIndent is where a row's value starts: the two-space margin, the
// 15-column label and its separating space.
const ytdlpRowIndent = 2 + 15 + 1

// ytdlpRow is one "label  value" line. The value wraps at the content width —
// at a path separator where it can — with every continuation line indented
// under the value column: the plugin paths are long, and at the 60-column
// floor the box is 54 wide, where they used to wrap back to column 0 under
// the labels.
//
// contentW <= 0 is a box that has not been sized yet: nothing to wrap to.
func ytdlpRow(label, value string, contentW int) string {
	lines := []string{value}
	if contentW > 0 {
		valueW := max(contentW-ytdlpRowIndent, 10)
		lines = strings.Split(ansi.Wrap(value, valueW, "/\\"), "\n")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  %-15s %s\n", label, lines[0])
	for _, l := range lines[1:] {
		b.WriteString(strings.Repeat(" ", ytdlpRowIndent) + l + "\n")
	}
	return b.String()
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
