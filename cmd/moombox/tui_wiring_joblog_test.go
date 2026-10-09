package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestJobLogWiringReadsTheDatabaseBuffer pins the O L chord's one source, in
// the idiom of TestRecoverAsidesWiringForwardsTheWorker: by reading
// tui_wiring.go as text, because the closures it installs are only ever
// reached through a live tea.Program over a real database.
//
// The overlay must show the per-job buffer the dashboard's job dialog reads
// (db.GetJobLogs, behind GET /api/jobs/{id}/logs) and nothing else — not a
// second buffer filled by the TUI's own log subscription, which would drift
// from the dashboard's the moment either side dropped a line.
//
// THE MUTANTS: OnGetJobLogs left unwired — the chord vanishes from the TUI;
// wired to anything but s.db.GetJobLogs (a closure returning nil, a copy of
// the routing over the TUI's log channel) — the overlay stops showing what
// the dashboard shows.
func TestJobLogWiringReadsTheDatabaseBuffer(t *testing.T) {
	src, err := os.ReadFile("tui_wiring.go")
	if err != nil {
		t.Fatalf("read tui_wiring.go: %v", err)
	}
	text := strings.ReplaceAll(string(src), "\r\n", "\n")
	if !regexp.MustCompile(`(?m)^\s*app\.OnGetJobLogs\s*=\s*s\.db\.GetJobLogs\s*$`).MatchString(text) {
		t.Error("OnGetJobLogs is not wired to s.db.GetJobLogs — the O L overlay must read the same " +
			"per-job buffer GET /api/jobs/{id}/logs serves the dashboard")
	}
}
