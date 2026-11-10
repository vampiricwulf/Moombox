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
