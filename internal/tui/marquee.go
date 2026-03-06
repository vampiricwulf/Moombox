package tui

import (
	"strings"

	"github.com/mattn/go-runewidth"
)

// marqueeWaitTicks is the number of 150ms ticks to pause at each end (~2 seconds).
const marqueeWaitTicks = 13

// Marquee provides auto-scrolling text that bounces left/right when it exceeds maxWidth.
type Marquee struct {
	text        string // full text
	maxWidth    int    // display width
	offset      int    // current rune offset into text
	direction   int    // +1 (forward) or -1 (backward)
	waitTicks   int    // countdown ticks for pause at each end
	needsScroll bool   // true if text exceeds maxWidth
	maxOffset   int    // precomputed max offset (in runes)
	runes       []rune // text as runes for efficient slicing
}

// Reset sets new text and maxWidth, restarting the animation.
func (m *Marquee) Reset(text string, maxWidth int) {
	m.text = text
	m.maxWidth = maxWidth
	m.offset = 0
	m.direction = 1
	m.waitTicks = marqueeWaitTicks // initial pause before scrolling starts
	m.runes = []rune(text)

	if runewidth.StringWidth(text) <= maxWidth {
		m.needsScroll = false
		m.maxOffset = 0
		return
	}

	m.needsScroll = true

	// Compute max offset: find the smallest rune offset where the remaining
	// text fits within maxWidth.
	m.maxOffset = 0
	for i := 1; i <= len(m.runes); i++ {
		remaining := string(m.runes[i:])
		if runewidth.StringWidth(remaining) <= maxWidth {
			m.maxOffset = i
			break
		}
	}
	if m.maxOffset == 0 {
		m.maxOffset = len(m.runes)
	}
}

// Tick advances the marquee animation by one step (called every 150ms).
func (m *Marquee) Tick() {
	if !m.needsScroll {
		return
	}

	// Pausing at an end
	if m.waitTicks > 0 {
		m.waitTicks--
		return
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
}

// View returns the visible portion of text at the current offset, padded to maxWidth.
func (m *Marquee) View() string {
	if !m.needsScroll {
		return m.text
	}

	// Slice from offset, take as many runes as fit within maxWidth
	start := m.offset
	if start > len(m.runes) {
		start = len(m.runes)
	}

	var result []rune
	w := 0
	for _, r := range m.runes[start:] {
		rw := runewidth.RuneWidth(r)
		if w+rw > m.maxWidth {
			break
		}
		result = append(result, r)
		w += rw
	}

	s := string(result)
	// Pad to maxWidth
	sw := runewidth.StringWidth(s)
	if sw < m.maxWidth {
		s += strings.Repeat(" ", m.maxWidth-sw)
	}
	return s
}

// NeedsScroll returns whether the text exceeds the display width.
func (m *Marquee) NeedsScroll() bool {
	return m.needsScroll
}
