package tui

import (
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// bareSheet is the frame-less container every bare overlay view is drawn
// with (overlay_bare.go): a one-row header, the body, and a last row of
// keys. No border and no margin; render returns exactly h rows of exactly w
// cells, so App.View has nothing to cut.
type bareSheet struct {
	// left and right are the header row. right (the keys or the page) is
	// kept whole; left (the title) is cut with an ellipsis to make room.
	left, right string
	// body is the rows under the header, already cut to the width by the
	// caller. Rows past what fits are dropped, missing ones are blank.
	body []string
	// msg is an error, a confirm or a notice, in plain text. It is wrapped
	// at spaces and takes rows from the bottom up: the keys row, then body
	// rows, then the header. A message too long even for those ends in an
	// ellipsis.
	msg string
	// msgStyle colours each row of msg.
	msgStyle lipgloss.Style
	// keys is the last row. Empty: the body runs to the last row.
	keys string
	// keep is how many body rows, counted from the first, msg may never
	// cover: the focused input, the armed row.
	keep int
}

// render draws the sheet as exactly h rows of exactly w cells.
func (s bareSheet) render(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	rows := make([]string, h)
	rows[0] = bareHeader(s.left, s.right, w)
	bodyTop, bodyEnd := 1, h
	if s.keys != "" && h >= 2 {
		bodyEnd = h - 1
		rows[h-1] = s.keys
	}
	for i := bodyTop; i < bodyEnd && i-bodyTop < len(s.body); i++ {
		rows[i] = s.body[i-bodyTop]
	}

	if strings.TrimSpace(s.msg) != "" {
		// The rows the message may take, in the order it takes them.
		var slots []int
		if bodyEnd < h {
			slots = append(slots, h-1)
		}
		for i := bodyEnd - 1; i >= bodyTop+max(s.keep, 0); i-- {
			slots = append(slots, i)
		}
		slots = append(slots, 0)

		lines := wrapFeedback(s.msg, w+feedbackIndent, len(s.msg)+1)
		if len(lines) > len(slots) {
			rest := strings.Join(lines[len(slots)-1:], " ")
			lines = append(lines[:len(slots)-1], truncateWidth(rest, w, "…"))
		}
		slots = slots[:len(lines)]
		sort.Ints(slots)
		for i, at := range slots {
			rows[at] = s.msgStyle.Render(truncateWidth(lines[i], w, "…"))
		}
	}
	return fitBlock(strings.Join(rows, "\n"), w, h)
}

// bareHeader is a bare overlay's one-row header: left, a gap, and right
// against the right edge. right is kept whole and left gives way to it.
func bareHeader(left, right string, w int) string {
	rw := ansi.StringWidth(right)
	if rw >= w {
		return truncateWidth(right, w, "")
	}
	room := w - rw
	if rw > 0 {
		room-- // the gap
	}
	left = truncateWidth(left, max(room, 0), "…")
	var sb strings.Builder
	sb.WriteString(left)
	writeSpaces(&sb, w-ansi.StringWidth(left)-rw)
	sb.WriteString(right)
	return sb.String()
}

// bareWrap wraps plain text for a bare view: rows of at most w cells, broken
// at spaces (a word wider than a row is broken where the row ends), at most
// maxRows of them. Text that does not fit ends in an ellipsis.
func bareWrap(msg string, w, maxRows int) []string {
	return wrapFeedback(msg, w+feedbackIndent, maxRows)
}

// bareStyled renders each row in style.
func bareStyled(rows []string, style lipgloss.Style) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = style.Render(row)
	}
	return out
}

// bareRows is rows as exactly n rows: the ones past n dropped, blank ones
// added.
func bareRows(rows []string, n int) []string {
	out := make([]string, max(n, 0))
	copy(out, rows)
	return out
}

// bareHeadCut cuts s to w cells from its head, keeping its end: a path's
// file name, an address's last group.
func bareHeadCut(s string, w int) string {
	sw := ansi.StringWidth(s)
	if sw <= w {
		return s
	}
	if w <= 1 {
		return truncateWidth("…", w, "")
	}
	return "…" + cutWidth(s, sw-(w-1), sw)
}

// bareMiddleCut cuts s to w cells from its middle, keeping both ends.
func bareMiddleCut(s string, w int) string {
	sw := ansi.StringWidth(s)
	if sw <= w {
		return s
	}
	if w <= 2 {
		return truncateWidth(s, w, "")
	}
	tail := (w - 1) / 2
	return truncateWidth(s, w-1-tail, "") + "…" + cutWidth(s, sw-tail, sw)
}

// setError puts an error on a sheet h rows tall that has a keys row: wrapped
// in the body where it fits there, so the title and the keys stay, and as
// the sheet's message where it does not, so that it takes the keys row and
// the header before any of it is cut.
func (s *bareSheet) setError(msg string, w, h int) {
	if rows := bareWrap(msg, w, h); len(rows) <= h-2 {
		s.body = bareStyled(rows, ErrorStyle)
		return
	}
	s.body = nil
	s.msg, s.msgStyle = msg, ErrorStyle
}
