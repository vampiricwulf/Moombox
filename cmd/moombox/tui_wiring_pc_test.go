package main

import (
	"os"
	"strings"
	"testing"
)

// TestTUIWiringReadsThePreviewAndTheGapFill pins the two delegates wave PC
// hangs off the TUI, in the idiom of TestRecoverAsidesWiringForwardsTheWorker
// and for its reason: the closures are installed on a tui.App that runTUI
// hands to a live tea.Program, with no seam to call them through.
//
// Mutants: ReinitializeSummary not summing worker.PreviewReinitialize (A I's
// confirm names nothing, owner decision PR-M1); JobGapFill not reading
// worker.TwitchGapFillStateOf (the details panel never shows a gap-fill,
// owner decision PR-R5).
func TestTUIWiringReadsThePreviewAndTheGapFill(t *testing.T) {
	src, err := os.ReadFile("tui_wiring.go")
	if err != nil {
		t.Fatalf("read tui_wiring.go: %v", err)
	}
	text := string(src)
	for _, want := range []string{
		"app.ReinitializeSummary = func(jobIDs []string) string {",
		"total.Add(worker.PreviewReinitialize(s.db, base, job))",
		"return total.Summary()",
		"app.JobGapFill = func(jobID string) *tui.GapFillView {",
		"g := worker.TwitchGapFillStateOf(s.db, job)",
		"return &tui.GapFillView{State: g.State, Spans: g.Spans, Until: g.Until, Reason: g.Reason}",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("tui_wiring.go lacks %q", want)
		}
	}
}
