package tui

import (
	"fmt"
	"strings"
	"testing"
)

// TestFFmpegManualOverlayFitsTheFloorTerminal: Manual mode is ten body lines
// plus the header and footer, and a path check's result adds two more — 22
// rows at 80x20, and at 60x20 the "usually at /usr/bin/ffmpeg" line also
// wrapped. bubbletea drops a too-tall frame's overflow from the top, so the
// "FFmpeg Not Found" title went first.
//
// Mutant: dropping dropSpacersToFit — the frame overflows.
func TestFFmpegManualOverlayFitsTheFloorTerminal(t *testing.T) {
	for _, size := range [][2]int{{60, 20}, {80, 20}, {100, 20}} {
		m := NewFFmpegCheckModel()
		m.SetSize(size[0], size[1])
		m.Open()
		m.ShowManual()
		m.manualResult = "✗ not an ffmpeg binary: /opt/bin/ffprobe"
		m.manualValid = false
		v := stripANSI(m.View())
		where := fmt.Sprintf("%dx%d", size[0], size[1])
		if n := strings.Count(v, "\n") + 1; n > size[1] {
			t.Errorf("%s: frame is %d rows:\n%s", where, n, v)
		}
		for _, want := range []string{"FFmpeg Not Found", "Enter FFmpeg path:", "not an ffmpeg binary", "Esc: Back"} {
			if !strings.Contains(v, want) {
				t.Errorf("%s: %q is not on screen:\n%s", where, want, v)
			}
		}
	}
}
