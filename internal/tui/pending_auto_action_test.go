package tui

import (
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobstate"
)

// TestPendingAutoActionShowsInTheDetailsAndTheRow pins owner ruling R-D2 in
// the TUI: an Error row the Twitch monitor will mux or retry by itself says
// so — an "Automatic" section in the details pane with jobstate's sentence,
// and a dim "⟳ mux" / "⟳ retry" tag in its task-list row — and an Error row
// with nothing pending shows neither.
//
// Mutants: the details section not added; progressCellText not tagging the
// Error row (the "" early return kept for Error).
func TestPendingAutoActionShowsInTheDetailsAndTheRow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		job     *database.Job
		tag     string
		section string
	}{
		{"auto mux", &database.Job{ID: "a", Platform: "twitch", Status: database.StatusError, Error: "twitch HLS: 500",
			ParkReason: database.ParkReasonTwitchEndUnconfirmed}, "⟳ mux ", jobstate.AutoActionMux.Text()},
		{"auto retry", &database.Job{ID: "b", Platform: "twitch", Status: database.StatusError, Error: jobstate.TwitchOfflineErrMsg,
			ParkReason: database.ParkReasonTwitchOffline},
			"⟳ retry ", jobstate.AutoActionRetry.Text()},
		{"nothing pending", &database.Job{ID: "c", Platform: "twitch", Status: database.StatusError, Error: "boom"}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list := NewTaskListModel()
			text, wait := list.progressCellText(tc.job)
			if text != tc.tag || (tc.tag != "" && !wait) {
				t.Errorf("row tag = %q (dim %v), want %q dim", text, wait, tc.tag)
			}
			m := NewJobDetailsModel()
			m.SetSize(200, 40)
			m.SetJob(tc.job)
			var header bool
			var lines []string
			for _, r := range m.rows {
				if r.kind == rowHeader && r.label == "Automatic" {
					header = true
				}
				if header && r.kind == rowField && r.label == "" {
					lines = append(lines, r.value)
				}
			}
			got := strings.Join(lines, " ")
			if tc.section == "" {
				if header {
					t.Errorf("an Error row with nothing pending shows an Automatic section: %q", got)
				}
				return
			}
			if !header || !strings.Contains(got, tc.section) {
				t.Errorf("details Automatic section = %v %q, want %q", header, got, tc.section)
			}
		})
	}
}
