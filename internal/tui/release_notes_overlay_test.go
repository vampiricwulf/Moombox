package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

const sampleNotes = "## Features\n\n- Something reasonably long that will need to wrap somewhere\n\n## Fixes\n\n- Another line\n"

func widestLine(s string) int {
	w := 0
	for _, ln := range strings.Split(s, "\n") {
		w = max(w, lipgloss.Width(ln))
	}
	return w
}

// TestReleaseNotesOverlayFitsTerminal pins the overflow fix. The box used
// to be sized by its widest line, and the footer ("U: Apply update  ↑/↓:
// Scroll  Esc/Q: Close") is a fixed 44 columns — so a 30-column terminal
// rendered a 46-column box. centerBox pads but never clips, so the excess
// spilled off-screen instead of being cut.
func TestReleaseNotesOverlayFitsTerminal(t *testing.T) {
	for w := 10; w <= 140; w++ {
		o := newReleaseNotesOverlay()
		o.open("v2.8.1", sampleNotes, w, 24)
		if got := widestLine(o.View()); got > w {
			t.Fatalf("terminal width %d: overlay rendered %d columns", w, got)
		}
	}
}

// TestReleaseNotesOverlayKeepsEscHint: however narrow the box gets, the
// reader must still be told how to close it — Esc is the one binding they
// cannot be stranded without.
func TestReleaseNotesOverlayKeepsEscHint(t *testing.T) {
	for w := 12; w <= 140; w++ {
		o := newReleaseNotesOverlay()
		o.open("v2.8.1", sampleNotes, w, 24)
		if !strings.Contains(stripANSI(o.View()), "Esc") {
			t.Errorf("width %d: no Esc hint in overlay", w)
		}
	}
}

// TestReleaseNotesOverlayReflowsOnResize: the overlay bakes glamour's word
// wrap into its content at open() time, so a resize while it is open has to
// re-render — otherwise the text stays wrapped for the old width. Scroll
// position must survive the reflow.
func TestReleaseNotesOverlayReflowsOnResize(t *testing.T) {
	o := newReleaseNotesOverlay()
	o.open("v2.8.1", sampleNotes, 120, 24)
	o.vp.SetYOffset(1)
	before := o.vp.YOffset()

	o.setSize(50, 20)
	if got := widestLine(o.View()); got > 50 {
		t.Errorf("after resize to 50: overlay rendered %d columns", got)
	}
	if o.vp.YOffset() != before {
		t.Errorf("scroll position lost across resize: %d -> %d", before, o.vp.YOffset())
	}
}

// TestReleaseNotesOverlaySetSizeWhenClosed: a resize while the overlay is
// closed must record the dimensions without resurrecting it (recalcLayout
// calls setSize unconditionally).
func TestReleaseNotesOverlaySetSizeWhenClosed(t *testing.T) {
	o := newReleaseNotesOverlay()
	o.setSize(80, 24)
	if o.isOpen() {
		t.Error("setSize opened a closed overlay")
	}
	if o.View() != "" {
		t.Error("closed overlay rendered content")
	}
}
