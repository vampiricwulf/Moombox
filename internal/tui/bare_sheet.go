package tui

import (
	"sort"
	"strings"

	"charm.land/bubbles/v2/textinput"
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
	// rows, then the header (the kept rows then move up a row, so the message
	// stays in one piece under them). A message too long even for those ends
	// in an ellipsis.
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
		if keep := min(max(s.keep, 0), bodyEnd-bodyTop); keep > 0 && slots[len(slots)-1] == 0 {
			// The message reaches the header: the kept rows move up over
			// it, so the message is one block under them and not a first
			// row above them with the rest below.
			copy(rows[:keep], rows[bodyTop:bodyTop+keep])
			slots[len(slots)-1] = keep
		}
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

// errorUnder puts an error or a notice on a sheet h rows tall that has a
// keys row, under the body rows the sheet already holds, which it may never
// cover: the inputs, the armed row. It is wrapped in the rows left above the
// keys where it fits there. One row short of that, the title gives way and
// the kept rows move up a row. Longer still, it is the sheet's message and
// takes the keys row and then the header before any of it is cut.
func (s *bareSheet) errorUnder(msg string, style lipgloss.Style, w, h int) {
	if strings.TrimSpace(msg) == "" {
		return
	}
	keep := len(s.body)
	rows := bareWrap(msg, w, len(msg)+1)
	switch room := h - 2 - keep; {
	case len(rows) <= room:
		s.body = append(s.body, bareStyled(rows, style)...)
		return
	case keep > 0 && len(rows) == room+1:
		// One row short: the title gives way and the kept rows move up.
		s.left, s.right = s.body[0], ""
		s.body = append(s.body[1:len(s.body):len(s.body)], bareStyled(rows, style)...)
		return
	}
	s.keep, s.msg, s.msgStyle = keep, msg, style
}

// bareInputWidth sets the width of a text input shared by the bare and the
// normal form of an overlay and lays its value out again for it, keeping the
// value and the cursor. The input only does that itself on an edit, and then
// only when the cursor has left the window it drew before, so SetWidth alone
// leaves the old width's window behind. An input draws width+1 cells, the
// last one the cursor's; 0 is no limit.
func bareInputWidth(ti *textinput.Model, width int) {
	value, pos := ti.Value(), ti.Position()
	ti.SetWidth(max(width, 0))
	ti.Reset() // forget the window the other width scrolled to
	ti.SetValue(value)
	ti.SetCursor(pos)
}

// bareInput draws a copy of a text input in width+1 cells, the last one the
// cursor's. The input itself is not touched, so the normal form finds its
// width and its window as it left them. The copy is laid out on every draw:
// the end of the value when the cursor is in its last half-width, otherwise
// a window with the cursor at its middle, so what is typed inside a long
// value is seen on both sides of the cursor.
func bareInput(ti textinput.Model, width int) string {
	width = max(width, 1)
	pos := ti.Position()
	bareInputWidth(&ti, width)
	if n := len([]rune(ti.Value())); pos < n-width/2 {
		ti.SetCursor(max(pos-width/2, 0)) // the window starts here
		ti.SetCursor(pos)
	}
	return ti.View()
}

// bareHeldInput remembers the value and the cursor of a text input the bare
// and the normal form of an overlay share, as they were when the terminal
// left the normal layout. The bare form draws the input from a copy
// (bareInput) and keys reach the real one at the width the normal form gave
// it, or at none when the overlay was opened bare. Back in the normal
// layout, an input that was written since is laid out for the normal width;
// one that was not is left exactly as the normal form had it, so a shrink
// and a regrow with nothing typed changes no cell.
type bareHeldInput struct {
	value string
	pos   int
}

// resized is called from SetSize with the layout before and after. It
// reports whether the normal form must lay the input out again.
func (h *bareHeldInput) resized(ti *textinput.Model, was, now layoutMode) bool {
	switch {
	case was == layoutNormal && now != layoutNormal:
		h.value, h.pos = ti.Value(), ti.Position()
	case was != layoutNormal && now == layoutNormal:
		return ti.Value() != h.value || ti.Position() != h.pos
	}
	return false
}

// bareFocus draws s as the focused row's label.
func bareFocus(s string) string {
	return lipgloss.NewStyle().Foreground(ColorCyan).Render(s)
}

// noteUnder puts a hint under the body rows the sheet already holds, where
// it fits whole in the rows left above the keys. A hint that does not fit is
// left out: it never takes a row from anything else.
func (s *bareSheet) noteUnder(msg string, style lipgloss.Style, w, h int) {
	if rows := bareWrap(msg, w, len(msg)+1); len(rows) <= h-2-len(s.body) {
		s.body = append(s.body, bareStyled(rows, style)...)
	}
}
