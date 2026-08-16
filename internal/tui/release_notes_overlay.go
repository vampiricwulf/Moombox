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
	o.applySize(width, height)
	o.vp.GotoTop()
}

// applySize sizes the viewport for the given terminal dimensions and
// re-renders the body at that width (glamour bakes its word wrap into the
// rendered text, so the content must be regenerated whenever the width
// changes — resizing the viewport alone would leave the old wrap points).
//
// The min() against the terminal is what keeps the box on screen. The ~80%
// target has a floor (40 columns wide, 8 rows tall) so the notes stay
// readable on a normal terminal, but that floor used to be applied without
// any upper bound, so a 30-column window produced a 46-column box — and
// centerBox can only pad, never clip, so it spilled past the right edge.
func (o *releaseNotesOverlay) applySize(width, height int) {
	o.width = width
	o.height = height

	// -4 borders + padding, -6 borders + title + footer.
	vpWidth := max(min(max(width*8/10, 40), width)-4, 1)
	vpHeight := max(min(max(height*8/10, 8), height)-6, 1)

	o.vp.SetWidth(vpWidth)
	o.vp.SetHeight(vpHeight)
	o.vp.SetContent(o.renderBody(vpWidth))
}

// setSize reflows an open overlay for a new terminal size, preserving the
// reader's scroll position. recalcLayout calls this on every resize: the
// overlay used to size itself only in open(), so resizing the terminal
// while the notes were up left the text wrapped for the old width.
func (o *releaseNotesOverlay) setSize(width, height int) {
	if !o.open_ {
		o.width, o.height = width, height
		return
	}
	off := o.vp.YOffset()
	o.applySize(width, height)
	o.vp.SetYOffset(off)
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

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(ColorGreen).Padding(0, 1)
	footerStyle := lipgloss.NewStyle().Faint(true).Padding(0, 1)
	borderStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorGreen)

	// The title and footer are laid out beside a viewport that was already
	// sized to the terminal, so THEY decide the box width whenever they are
	// the widest line — and the full footer is 44 columns, which is what
	// made a 30-column terminal render a 46-column box. Both now shrink to
	// the viewport's width: the footer steps down through shorter spellings
	// (keeping Esc, the one binding a reader must not be stranded without)
	// and the title truncates.
	inner := o.vp.Width()
	footerText := "U: Apply update  ↑/↓: Scroll  Esc/Q: Close"
	for _, cand := range []string{
		footerText,
		"U update · ↑/↓ scroll · Esc close",
		"U · ↑/↓ · Esc",
		"Esc",
	} {
		footerText = cand
		if lipgloss.Width(cand)+2 <= inner { // +2 for the style's horizontal padding
			break
		}
	}
	title := titleStyle.Render(truncateString("Release Notes — "+o.tag, max(inner-2, 1)))
	footer := footerStyle.Render(footerText)

	body := o.vp.View()
	joined := lipgloss.JoinVertical(lipgloss.Left, title, body, footer)
	box := borderStyle.Render(joined)

	// Backstop: centerBox pads but never clips, so anything still too wide
	// would spill off-screen rather than being cut.
	return centerBox(lipgloss.NewStyle().MaxWidth(o.width).Render(box), o.width, o.height)
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
