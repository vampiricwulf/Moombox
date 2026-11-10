package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobstate"
)

// TestMuxingRowShowsTheMuxReadout is the TUI half of owner ruling R-D1 of 2026-11-10: a
// Muxing row whose progress line is the mux readout shows the copy's percent
// in its progress slot, not the download's 100%, then a dim "⋯ final"; the
// compact row shows the same; and the details pane spells the line out. The
// line arrives as a progress tick, so it is read from the progress store. A
// Muxing row with no readout (an incomplete tail, a part mux) keeps the
// percent it had.
//
// Mutants: progressCell not reading the readout (the row says 100% through
// the mux); reading it for a row that is not Muxing.
func TestMuxingRowShowsTheMuxReadout(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, line string
		status     database.JobStatus
		want       string
		dim        bool
	}{
		{"copying", jobstate.MuxProgressLine(42), database.StatusMuxing, "42% ", false},
		{"just started", jobstate.MuxProgressLine(0), database.StatusMuxing, "0% ", false},
		{"finalizing", jobstate.MuxFinalizingLine, database.StatusMuxing, "⋯ final ", true},
		{"no readout", "V:100% A:100% C: 7", database.StatusMuxing, "100% ", false},
		{"a download is never read as a mux", jobstate.MuxProgressLine(42), database.StatusDownloading, "100% ", false},
	} {
		m := NewTaskListModel()
		job := &database.Job{ID: "j", Status: tc.status, Percent: 100, Progress: "V:100% A:100% C: 7"}
		m.progressStore.Set("j", &ProgressData{Progress: tc.line, Percent: 100})
		text, dim := m.progressCell(job, now)
		if text != tc.want || dim != tc.dim {
			t.Errorf("%s: progress slot = %q (dim %v), want %q (dim %v)", tc.name, text, dim, tc.want, tc.dim)
		}
		m.setCompact(true)
		if got, gotDim := m.compactFigure(job, now); strings.TrimSpace(got) != strings.TrimSpace(tc.want) || gotDim != tc.dim {
			t.Errorf("%s: compact figure = %q (dim %v), want %q (dim %v)", tc.name, got, gotDim, strings.TrimSpace(tc.want), tc.dim)
		}
	}

	d := NewJobDetailsModel()
	d.SetSize(80, 24)
	d.SetJob(&database.Job{ID: "j", Title: "T", Platform: "youtube", Status: database.StatusMuxing, Percent: 100, Progress: "V:100% A:100% C: 7"})
	d.SetProgress(&ProgressData{Progress: jobstate.MuxProgressLine(42), Percent: 100})
	if got := progressRowValue(d); got != "Muxing 42%" {
		t.Errorf("details Progress row = %q, want the readout", got)
	}
	d.SetProgress(&ProgressData{Progress: jobstate.MuxFinalizingLine, Percent: 100})
	if got := progressRowValue(d); got != "Muxing: finalizing" {
		t.Errorf("details Progress row = %q, want the finalizing line", got)
	}
}
