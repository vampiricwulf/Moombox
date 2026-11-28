package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The latency backfill's switch and its status line in Settings (owner
// ruling RV-20). The line's wording is worker.LatencyBackfillLine, worded in
// cmd/moombox; these tests are about Settings drawing the line it is handed
// under the latency_backfill toggle, in the normal and the bare form.

const latencyTestLine = "paused: YouTube rate limit until 14:05 (212 of 540 recordings done)"

func init() {
	// In the size and shrink-and-back tables with every other Settings state.
	bareSettingsStates["monitors, the latency backfill line"] = func(t *testing.T, r *compactRig) {
		r.app.Update(LatencyBackfillLineMsg{Line: latencyTestLine})
		toSection(t, r.app.settings, "Monitors")
		toField(t, r.app.settings, "latency_backfill")
	}
}

// Mutants: the key that opens Settings not reading the line (OnLatencyBackfillLine
// never asked); the LatencyBackfillLineMsg not stored; the line drawn under
// another field, or not at all.
func TestSettingsShowsTheLatencyBackfillLine(t *testing.T) {
	r := newCompactRig(t, 120, 40)
	asked := 0
	r.app.OnLatencyBackfillLine = func() string { asked++; return "caught up (540 recordings)" }
	_, cmd := r.app.handleKey(namedKey("`"))
	if !r.app.settings.IsVisible() {
		t.Fatal("` did not open Settings")
	}
	if cmd == nil {
		t.Fatal("opening Settings asked nothing for the line")
	}
	r.app.Update(cmd())
	if asked != 1 {
		t.Fatalf("OnLatencyBackfillLine asked %d times, want 1", asked)
	}
	toSection(t, r.app.settings, "Monitors")
	rows := strings.Split(ansi.Strip(r.app.settings.View()), "\n")
	at := -1
	for i, row := range rows {
		if strings.Contains(row, "Latency backfill") {
			at = i
		}
	}
	if at < 0 || at+1 >= len(rows) || !strings.Contains(rows[at+1], "caught up (540 recordings)") {
		t.Fatalf("the line is not the row under the toggle:\n%s", strings.Join(rows, "\n"))
	}

	// A pushed change replaces it.
	r.app.Update(LatencyBackfillLineMsg{Line: latencyTestLine})
	if !strings.Contains(ansi.Strip(r.app.settings.View()), latencyTestLine) {
		t.Error("a pushed line was not drawn")
	}
	if strings.Contains(ansi.Strip(r.app.settings.View()), "caught up") {
		t.Error("the old line stayed")
	}
}

// The bare form draws the line under the focused toggle, as it draws a
// preview. (The toggle's write is TestEverySettingsFieldIsWritable's.)
//
// Mutant: the bare form not asking fieldSecondRow (no line at 40x10).
func TestBareSettingsShowsTheLatencyBackfillLine(t *testing.T) {
	r := bareSettingsRig(t, 40, 10)
	r.app.Update(LatencyBackfillLineMsg{Line: "212 of 540 recordings done"})
	toSection(t, r.app.settings, "Monitors")
	toField(t, r.app.settings, "latency_backfill")
	if view := ansi.Strip(r.app.settings.View()); !strings.Contains(view, "212 of 540") {
		t.Fatalf("the bare form has no line under the focused toggle:\n%s", view)
	}
}
