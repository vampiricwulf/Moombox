package tui

import (
	"strings"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The panel frame is the four panels joined into one screen: the task list
// beside the details, then the log panel and the status bar under them, with
// the persistent banners on top. Every panel memoises its own View(), but the
// join used to run on every View() call, and bubbletea calls View() after
// every message. lipgloss.JoinHorizontal and JoinVertical measure the display
// width of every line they are given (an ANSI parse plus grapheme
// segmentation), so a frame in which nothing had changed still cost ~0.5 ms
// and ~146 KB at 200x60, about 120 times a second with one download active.
//
// frameComposer removes that in two steps, both byte-identical to the
// lipgloss joins (frame_compose_test.go compares them):
//
//  1. The whole composed frame is memoised under frameKey. An unchanged
//     frame is one struct comparison; the panel strings in the key are the
//     panels' own cached values, so comparing them is a pointer check.
//  2. A frame that did change re-measures only the blocks that changed. Each
//     block's lines and widths are kept beside the string they were measured
//     from (measuredBlock), and the join pads from those.

// frameKey is everything the composed frame is built from. A new input to
// the panel path of App.View must be a field here, or the frame goes stale.
type frameKey struct {
	task, details, logs, status string
	restart                     bool
	security                    string
	width, height               int
	// The feedback line, as View draws it: text, stated severity (its
	// colour), and whether it wraps. Its expiry time is not an input: the
	// line is cleared by emptying msg.
	fbMsg  string
	fbSev  feedbackSeverity
	fbWrap bool
}

// frameComposer holds the memoised frame and the per-block measurements.
type frameComposer struct {
	valid   bool
	key     frameKey
	content string

	task, details, logs, status measuredBlock
	restart, security           measuredBlock
}

// measuredBlock is one block of the frame split into lines, each with its
// display width. It is what the unexported getLines in lipgloss computes,
// kept so an unchanged block is not measured again.
type measuredBlock struct {
	valid  bool
	src    string
	lines  []string
	widths []int
	widest int
	// additive: every line can be concatenated with another such line, or
	// with spaces, and the width of the result is the sum of the widths
	// (lineIsAdditive). The side-by-side row needs it to know its own width
	// without measuring the joined line.
	additive bool
}

// measure splits s exactly as lipgloss does before a join (tabs become four
// spaces, CRLF becomes LF) and records the width of each line. A block equal
// to the one already measured is left alone.
func (b *measuredBlock) measure(s string) {
	if b.valid && b.src == s {
		return
	}
	b.valid, b.src = true, s
	if strings.IndexByte(s, '\t') >= 0 {
		s = strings.ReplaceAll(s, "\t", "    ")
	}
	if strings.IndexByte(s, '\r') >= 0 {
		s = strings.ReplaceAll(s, "\r\n", "\n")
	}
	b.lines = strings.Split(s, "\n")
	b.widths = b.widths[:0]
	b.widest = 0
	b.additive = true
	for _, l := range b.lines {
		w := ansi.StringWidth(l)
		b.widths = append(b.widths, w)
		b.widest = max(b.widest, w)
		if b.additive && !lineIsAdditive(l) {
			b.additive = false
		}
	}
}

// isCellRune reports whether r is a rune that never joins a neighbour into
// one grapheme cluster: printable ASCII, or a box-drawing or block element
// (the panel borders).
func isCellRune(r rune) bool {
	return (r >= 0x20 && r <= 0x7e) || (r >= 0x2500 && r <= 0x259f)
}

// lineIsAdditive reports whether ansi.StringWidth(a+l+b) is certain to equal
// the sum of the three widths for any a and b that also pass. That holds
// when nothing can reach across a junction:
//
//   - the line is valid UTF-8, so no stray byte is read as a C1 control;
//   - its only control bytes are ESC, each starting a complete CSI sequence,
//     so the parser is back in its ground state at the end of the line (and
//     no CR is left to pair with a newline);
//   - its first rune is ESC or a cell rune and its last rune is a cell rune
//     (the final byte of a CSI sequence is printable ASCII), so no grapheme
//     cluster spans the junction.
//
// Every bordered panel passes. A line that does not is not wrong, only
// unproven, and the composer falls back to the lipgloss joins for it.
func lineIsAdditive(l string) bool {
	if l == "" {
		return true
	}
	if !utf8.ValidString(l) {
		return false
	}
	if first, _ := utf8.DecodeRuneInString(l); first != 0x1b && !isCellRune(first) {
		return false
	}
	if last, _ := utf8.DecodeLastRuneInString(l); !isCellRune(last) {
		return false
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		if c >= 0x20 && c != 0x7f {
			continue
		}
		if c != 0x1b {
			return false // a C0 control or DEL
		}
		// ESC [ params(0x30-0x3f)* intermediates(0x20-0x2f)* final(0x40-0x7e)
		i++
		if i >= len(l) || l[i] != '[' {
			return false
		}
		i++
		for i < len(l) && l[i] >= 0x30 && l[i] <= 0x3f {
			i++
		}
		for i < len(l) && l[i] >= 0x20 && l[i] <= 0x2f {
			i++
		}
		if i >= len(l) || l[i] < 0x40 || l[i] > 0x7e {
			return false
		}
	}
	return true
}

// writeSpaces writes n spaces; n <= 0 writes nothing. It is
// sb.WriteString(strings.Repeat(" ", n)) without the allocation.
func writeSpaces(sb *strings.Builder, n int) {
	const pad = "                                                                "
	for n > 0 {
		k := min(n, len(pad))
		sb.WriteString(pad[:k])
		n -= k
	}
}

// joinPanelsWithLipgloss is the composition as it was written before the
// composer: the reference compose must equal, and its fallback.
func joinPanelsWithLipgloss(restartBanner, securityBanner, task, details, logs, status string) string {
	parts := make([]string, 0, 5)
	if restartBanner != "" {
		parts = append(parts, restartBanner)
	}
	if securityBanner != "" {
		parts = append(parts, securityBanner)
	}
	parts = append(parts, lipgloss.JoinHorizontal(lipgloss.Top, task, details), logs, status)
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// compose joins the blocks into the panel frame: the banners that are not
// empty, then task beside details, then logs, then status. The result is,
// byte for byte, joinPanelsWithLipgloss of the same arguments.
func (f *frameComposer) compose(restartBanner, securityBanner, task, details, logs, status string) string {
	f.task.measure(task)
	f.details.measure(details)
	if !f.task.additive || !f.details.additive {
		// The row's width cannot be had by adding: let lipgloss measure it.
		return joinPanelsWithLipgloss(restartBanner, securityBanner, task, details, logs, status)
	}
	f.logs.measure(logs)
	f.status.measure(status)

	// JoinHorizontal pads every line of a block to the block's widest line,
	// and a short block with empty lines, so every row of the pair is
	// exactly rowW wide. The lines being additive, that is also the width
	// JoinVertical would measure for the row.
	rowW := f.task.widest + f.details.widest
	rows := max(len(f.task.lines), len(f.details.lines))

	widest := max(rowW, f.logs.widest, f.status.widest)
	size := rows*(rowW+1) + len(task) + len(details) + len(logs) + len(status)
	if restartBanner != "" {
		f.restart.measure(restartBanner)
		widest = max(widest, f.restart.widest)
		size += len(restartBanner)
	}
	if securityBanner != "" {
		f.security.measure(securityBanner)
		widest = max(widest, f.security.widest)
		size += len(securityBanner)
	}

	var sb strings.Builder
	sb.Grow(size + 64)
	first := true
	newline := func() {
		if !first {
			sb.WriteByte('\n')
		}
		first = false
	}
	block := func(b *measuredBlock) {
		for i, l := range b.lines {
			newline()
			sb.WriteString(l)
			writeSpaces(&sb, widest-b.widths[i])
		}
	}
	side := func(b *measuredBlock, i int) {
		if i < len(b.lines) {
			sb.WriteString(b.lines[i])
			writeSpaces(&sb, b.widest-b.widths[i])
			return
		}
		writeSpaces(&sb, b.widest)
	}

	if restartBanner != "" {
		block(&f.restart)
	}
	if securityBanner != "" {
		block(&f.security)
	}
	for i := range rows {
		newline()
		side(&f.task, i)
		side(&f.details, i)
		writeSpaces(&sb, widest-rowW)
	}
	block(&f.logs)
	block(&f.status)
	return sb.String()
}
