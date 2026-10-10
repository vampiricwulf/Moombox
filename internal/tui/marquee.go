package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// marqueeWaitTicks is the number of 150ms ticks to pause at each end (~2 seconds).
const marqueeWaitTicks = 13

// Marquee provides auto-scrolling text that bounces left/right when it exceeds maxWidth.
//
// Offsets are in terminal cells and every width is measured with x/ansi, the
// grapheme-aware measure the renderer itself uses. It used to walk runes with
// go-runewidth, which counts an emoji with a variation selector ("❤️", "1️⃣")
// one cell narrower than the terminal draws it, so the padded window came out
// a cell too wide and the row wrapped.
type Marquee struct {
	text        string // full text
	maxWidth    int    // display width
	offset      int    // current cell offset into text
	direction   int    // +1 (forward) or -1 (backward)
	waitTicks   int    // countdown ticks for pause at each end
	needsScroll bool   // true if text exceeds maxWidth
	maxOffset   int    // largest offset, in cells, whose window still ends at the text's end
}

// Reset sets new text and maxWidth, restarting the animation.
func (m *Marquee) Reset(text string, maxWidth int) {
	m.text = text
	m.maxWidth = maxWidth
	m.offset = 0
	m.direction = 1
	m.waitTicks = marqueeWaitTicks // initial pause before scrolling starts

	totalW := ansi.StringWidth(text)
	if totalW <= maxWidth {
		m.needsScroll = false
		m.maxOffset = 0
		return
	}
	m.needsScroll = true
	m.maxOffset = totalW - maxWidth
}

// Tick advances the marquee animation by one step (called every 150ms).
// Returns whether the visible offset actually moved — false during the
// ~2s end pauses (waitTicks) and when nothing scrolls — so callers can
// skip re-render work for frames that would be identical.
func (m *Marquee) Tick() bool {
	if !m.needsScroll {
		return false
	}

	// Pausing at an end
	if m.waitTicks > 0 {
		m.waitTicks--
		return false
	}

	// Advance
	m.offset += m.direction

	// Hit the right end — pause and reverse
	if m.offset >= m.maxOffset {
		m.offset = m.maxOffset
		m.direction = -1
		m.waitTicks = marqueeWaitTicks
	}

	// Hit the left end — pause and reverse
	if m.offset <= 0 {
		m.offset = 0
		m.direction = 1
		m.waitTicks = marqueeWaitTicks
	}
	return true
}

// View returns the visible portion of text at the current offset, padded to
// maxWidth. A wide character the window's left edge falls inside is dropped
// whole rather than split, and the padding makes up the cell.
func (m *Marquee) View() string {
	if !m.needsScroll {
		return m.text
	}
	s := cutWidth(m.text, m.offset, m.offset+m.maxWidth)
	if sw := ansi.StringWidth(s); sw < m.maxWidth {
		s += strings.Repeat(" ", m.maxWidth-sw)
	}
	return s
}

// NeedsScroll returns whether the text exceeds the display width.
func (m *Marquee) NeedsScroll() bool {
	return m.needsScroll
}
