package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// vs16Titles carry emoji with a variation selector or a keycap sequence —
// graphemes the renderer (x/ansi) draws two cells wide and go-runewidth
// counted as one, so every row padded with runewidth came out a cell too wide.
var vs16Titles = []string{"1️⃣ first stream of the year", "❤️ karaoke night ❤️ with friends", "▶️ ✔️ ©️ #️⃣ mixed bag"}

// TestTaskRowsWithVS16TitlesFitThePanel: the title is cut with x/ansi and was
// padded with go-runewidth, so a keycap title rendered 26 cells into a
// 25-cell column — at 60x20 the whole frame went to 61 columns and the
// renderer clipped the Details panel's right border.
//
// Mutant: measuring the title pad with runewidth again — the rows overflow.
func TestTaskRowsWithVS16TitlesFitThePanel(t *testing.T) {
	for _, w := range []int{28, 40} {
		m := NewTaskListModel()
		m.SetSize(w, 10)
		var jobs []*database.Job
		for i, title := range vs16Titles {
			jobs = append(jobs, &database.Job{ID: string(rune('a' + i)), Title: title, Status: database.StatusFinished, Platform: "youtube"})
		}
		m.SetJobs(jobs)
		for _, line := range strings.Split(m.View(), "\n") {
			if got := ansi.StringWidth(line); got > w {
				t.Errorf("width %d: a row is %d cells: %q", w, got, line)
			}
		}
	}
}

// TestMarqueeWindowIsExactlyItsWidth scrolls a VS16 title through every
// offset: each window must be exactly maxWidth cells as the renderer counts
// them, not as go-runewidth did.
//
// Mutant: padding the window by rune count — a VS16 window is a cell wide.
func TestMarqueeWindowIsExactlyItsWidth(t *testing.T) {
	for _, title := range vs16Titles {
		var mq Marquee
		mq.Reset(title, 9)
		if !mq.NeedsScroll() {
			t.Fatalf("%q: fixture must need scrolling", title)
		}
		for range 400 {
			if got := ansi.StringWidth(mq.View()); got != 9 {
				t.Fatalf("%q at offset %d: window is %d cells, want 9", title, mq.offset, got)
			}
			mq.Tick()
		}
	}
}

// TestWrapTextHardBreaksByGrapheme: a word wider than the line is split in
// whole graphemes, each piece no wider than the line as the renderer counts.
func TestWrapTextHardBreaksByGrapheme(t *testing.T) {
	for _, maxW := range []int{1, 3, 5} {
		for _, line := range wrapText("❤️❤️❤️1️⃣1️⃣abcdef", maxW) {
			if got := ansi.StringWidth(line); got > max(maxW, 2) {
				t.Errorf("maxW %d: piece %q is %d cells", maxW, line, got)
			}
		}
	}
	if got := strings.Join(wrapText("❤️❤️❤️1️⃣1️⃣abcdef", 3), ""); got != "❤️❤️❤️1️⃣1️⃣abcdef" {
		t.Errorf("hard break lost or split a grapheme: %q", got)
	}
}

// TestTruncateStringHoldsToTheRenderersWidth: x/ansi's Truncate fits a keycap
// sequence in one cell where its own StringWidth — and the renderer — count
// two, so every cut through a keycap title came out wider than asked.
//
// Mutant: truncateString back to a bare ansi.Truncate — the keycap cuts are
// a cell too wide.
func TestTruncateStringHoldsToTheRenderersWidth(t *testing.T) {
	for _, s := range append([]string{"1️⃣1️⃣1️⃣1️⃣1️⃣", "#️⃣ tag #️⃣ tag", "plain ascii text", "日本語のタイトル"}, vs16Titles...) {
		for w := 1; w <= ansi.StringWidth(s)+1; w++ {
			if got := ansi.StringWidth(truncateString(s, w)); got > w {
				t.Errorf("truncateString(%q, %d) is %d cells", s, w, got)
			}
		}
	}
}
