package jobstate

import "testing"

// TestMuxReadoutReadsWhatTheWriterWrites pins the mux readout's two lines
// (owner ruling R-D1 of 2026-11-10) and that MuxReadout reads exactly those: a download's
// counters and a wait line are not a readout, and neither line is a wait
// line, so the TUI row never tags a mux as waiting.
func TestMuxReadoutReadsWhatTheWriterWrites(t *testing.T) {
	if got := MuxProgressLine(42); got != "Muxing 42%" {
		t.Errorf("MuxProgressLine(42) = %q", got)
	}
	if MuxFinalizingLine != "Muxing: finalizing" {
		t.Errorf("MuxFinalizingLine = %q", MuxFinalizingLine)
	}
	for _, pct := range []int{0, 7, 42, 99} {
		got, fin, ok := MuxReadout(MuxProgressLine(pct))
		if !ok || fin || got != pct {
			t.Errorf("MuxReadout(%q) = %d, %v, %v", MuxProgressLine(pct), got, fin, ok)
		}
	}
	if _, fin, ok := MuxReadout(MuxFinalizingLine); !ok || !fin {
		t.Errorf("MuxReadout(finalizing) = finalizing %v, ok %v", fin, ok)
	}
	for _, line := range []string{"", "Muxing...", "Muxing %", "Muxing 4x%", "Muxing 42", "V:100% A:100% C: 12",
		"(V: 1234/1300 A: 1234/1300 C: 5678)", "V:95.3%", WaitLine("Reconnecting", "12s")} {
		if _, _, ok := MuxReadout(line); ok {
			t.Errorf("MuxReadout(%q) read a readout", line)
		}
	}
	for _, line := range []string{MuxProgressLine(42), MuxFinalizingLine} {
		if IsWaitLine(line) {
			t.Errorf("%q reads as a wait line", line)
		}
	}
}
