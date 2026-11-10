package tui

import (
	"strings"

	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The release notes' bare form, drawn below the normal layout
// (overlay_bare.go): the tag and the keys share the first row, the notice a
// withdrawn release leaves takes a row of its own under it, and the notes
// fill the rest at the full width with no border and no margin.
//
// While U and S act, the header spells them out ("U update", "S skip") at
// every width: S skips the release on one press. The tag is what gives way.

// Release-notes header keys, longest first. The short pending form is 19
// cells, so it fits beside a cut tag at the 30-column floor.
const (
	bareNotesKeysPending      = "U update · S skip · Esc close"
	bareNotesKeysPendingShort = "U update·S skip·Esc"
	bareNotesKeysViewer       = "↑/↓ scroll · Esc close"
	bareNotesKeysViewerShort  = "Esc close"
)

// bare reports whether the overlay draws its bare form: its own size is
// below the normal layout. Derived on every call, never stored.
func (o *releaseNotesOverlay) bare() bool {
	return layoutModeFor(o.width, o.height) == layoutCompact
}

// bareNoticeRows is the withdrawn notice wrapped for the bare form: up to
// two rows (three from 10 rows up), so the instruction after "No longer
// pending" is read on a narrow screen. It gives rows back, down to one,
// while the notes would have fewer than three.
func (o *releaseNotesOverlay) bareNoticeRows() []string {
	if o.notice == "" {
		return nil
	}
	limit := 2
	if o.height >= 10 {
		limit = 3
	}
	limit = max(min(limit, o.height-1-3), 1)
	return bareWrap(o.notice, max(o.width, 1), limit)
}

// bareBodyRows is how many rows the notes get in the bare form: every row
// but the header and the notice's.
func (o *releaseNotesOverlay) bareBodyRows() int {
	return max(o.height-1-len(o.bareNoticeRows()), 1)
}

// applyBareSize sizes the viewport for the bare form and renders the notes
// at the full width.
func (o *releaseNotesOverlay) applyBareSize() {
	o.vp.SetWidth(o.width)
	o.vp.SetHeight(o.bareBodyRows())
	o.vp.SetContent(o.renderBareBody(o.width))
}

// renderBareBody is renderBody for the bare form: the same glamour render
// and the same wrap, without glamour's document margin and without the blank
// line it opens every document with, so the first row of a small screen is
// text.
func (o *releaseNotesOverlay) renderBareBody(width int) string {
	if strings.TrimSpace(o.rawNotes) == "" {
		return "No release notes available for this update."
	}
	style := releaseNotesStyle(o.isDark)
	// A new pointer on the copy: the margin is shared with glamour's own
	// package-level style and must not be written through.
	var noMargin uint
	style.Document.Margin = &noMargin
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithWordWrap(0), // off: wrapReleaseNotes wraps, below
	)
	if err != nil {
		return o.rawNotes
	}
	rendered, err := r.Render(o.rawNotes)
	if err != nil {
		return o.rawNotes
	}
	return wrapReleaseNotes(strings.TrimPrefix(rendered, "\n"), max(width, 1))
}

// bareView draws the bare form: exactly width x height cells.
func (o *releaseNotesOverlay) bareView() string {
	// withdraw sets the notice after the viewport was sized, so the height
	// is settled here; the content does not depend on it.
	o.vp.SetHeight(o.bareBodyRows())

	tag := lipgloss.NewStyle().Bold(true).Foreground(ColorGreen).Render(o.tag)
	long, short := bareNotesKeysViewer, bareNotesKeysViewerShort
	if o.pending {
		long, short = bareNotesKeysPending, bareNotesKeysPendingShort
	}
	keys := short
	if ansi.StringWidth(o.tag)+2+ansi.StringWidth(long) <= o.width {
		keys = long
	}

	// Wrapped, where the normal form's wider box shows it on one row:
	// what to do next is in the notice's second half.
	body := bareStyled(o.bareNoticeRows(), lipgloss.NewStyle().Foreground(ColorWarning))
	body = append(body, strings.Split(o.vp.View(), "\n")...)
	return bareSheet{
		left:  tag,
		right: DimStyle.Render(keys),
		body:  body,
	}.render(o.width, o.height)
}
