package tui

import (
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
)

// releaseNotesOverlay is a modal that shows release notes for a pending
// update. Scrollable via arrow keys / pgup-pgdn (handled by the
// embedded viewport; letter-key bindings disabled to avoid conflict
// with app chords). Opened via R N chord; from within, U applies the
// update or Esc/Q closes.
type releaseNotesOverlay struct {
	open_    bool
	tag      string
	rawNotes string
	width    int
	height   int
	vp       viewport.Model
}

// newReleaseNotesOverlay returns a closed overlay ready for use.
func newReleaseNotesOverlay() *releaseNotesOverlay {
	vp := viewport.New(viewport.WithWidth(80), viewport.WithHeight(20))
	vp.KeyMap = helpViewportKeyMap() // reuse help.go's safe key map (no letter keys)
	return &releaseNotesOverlay{vp: vp}
}

// isOpen reports whether the overlay is currently visible.
func (o *releaseNotesOverlay) isOpen() bool { return o.open_ }

// open prepares and shows the overlay. width/height are the terminal
// dimensions; the overlay sizes itself to ~80% of those.
func (o *releaseNotesOverlay) open(tag, rawNotes string, width, height int) {
	o.open_ = true
	o.tag = tag
	o.rawNotes = rawNotes
	o.width = width
	o.height = height

	vpWidth := width * 8 / 10
	if vpWidth < 40 {
		vpWidth = 40
	}
	vpWidth -= 4 // account for borders + padding

	vpHeight := height * 8 / 10
	if vpHeight < 8 {
		vpHeight = 8
	}
	vpHeight -= 6 // account for borders + title + footer

	o.vp.SetWidth(vpWidth)
	o.vp.SetHeight(vpHeight)
	o.vp.SetContent(o.renderBody(vpWidth))
	o.vp.GotoTop()
}

// close hides the overlay and clears its state.
func (o *releaseNotesOverlay) close() {
	o.open_ = false
	o.tag = ""
	o.rawNotes = ""
}

// Update routes a tea.Msg to the embedded viewport for scroll handling.
// Returns the tea.Cmd from the viewport (typically nil).
func (o *releaseNotesOverlay) Update(msg tea.Msg) tea.Cmd {
	if !o.open_ {
		return nil
	}
	var cmd tea.Cmd
	o.vp, cmd = o.vp.Update(msg)
	return cmd
}

// View returns the rendered overlay frame. Empty string when closed.
func (o *releaseNotesOverlay) View() string {
	if !o.open_ {
		return ""
	}

	vpWidth := o.width * 8 / 10
	if vpWidth < 40 {
		vpWidth = 40
	}

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(ColorGreen).Padding(0, 1)
	footerStyle := lipgloss.NewStyle().Faint(true).Padding(0, 1)
	borderStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorGreen)

	title := titleStyle.Render("Release Notes — " + o.tag)
	footer := footerStyle.Render("U: Apply update  ↑/↓: Scroll  Esc/Q: Close")

	body := o.vp.View()
	inner := lipgloss.JoinVertical(lipgloss.Left, title, body, footer)
	box := borderStyle.Render(inner)

	return centerBox(box, o.width, o.height)
}

// renderBody runs glamour over the raw markdown to produce ANSI text
// sized to the given width. Returns a fallback message for empty notes.
// WithStandardStyle("dark") is used instead of WithAutoStyle() so that
// glamour always produces styled output — WithAutoStyle falls back to
// no-op ASCII mode when no TTY is detected (e.g. in tests or when
// TERM is unset), which would leave raw "## Heading" syntax visible.
// Moombox always runs in a terminal and defaults to a dark theme.
func (o *releaseNotesOverlay) renderBody(width int) string {
	if strings.TrimSpace(o.rawNotes) == "" {
		return "No release notes available for this update."
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return o.rawNotes
	}
	rendered, err := r.Render(o.rawNotes)
	if err != nil {
		return o.rawNotes
	}
	return rendered
}
