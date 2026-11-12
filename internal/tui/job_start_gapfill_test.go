package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// fieldRow returns the details panel's field row labelled label, or nil.
func fieldRow(m *JobDetailsModel, label string) *detailRow {
	for i := range m.rows {
		if m.rows[i].kind == rowField && m.rows[i].label == label {
			return &m.rows[i]
		}
	}
	return nil
}

const edgeReason = "the stream had been live longer than YouTube keeps its segments (~120 h)"

// TestLiveEdgeLine pins the TUI's wording of a late start (owner decision
// PR-R2), the dashboard's liveEdgeText: the reason, and the join point once
// the finalize wrote it; nothing for a capture with no reason, or one whose
// archive records from 0 after all.
//
// Mutants: the join left out; a join of 0 still drawn.
func TestLiveEdgeLine(t *testing.T) {
	join := func(v float64) *float64 { return &v }
	for _, tc := range []struct {
		job  *database.Job
		want string
	}{
		{&database.Job{}, ""},
		{&database.Job{LiveEdgeReason: edgeReason}, "Recorded from the live edge: " + edgeReason},
		{&database.Job{LiveEdgeReason: edgeReason, JoinOffsetSeconds: join(5025)}, "Recorded from the live edge: " + edgeReason + ", joined 1:23:45 in"},
		{&database.Job{LiveEdgeReason: edgeReason, JoinOffsetSeconds: join(0)}, ""},
	} {
		if got := liveEdgeLine(tc.job); got != tc.want {
			t.Errorf("liveEdgeLine(%+v) = %q, want %q", tc.job, got, tc.want)
		}
	}
}

// TestPriorArchiveLine pins the details line for the archive a Reinitialize
// kept (owner ruling R2-W1), the dashboard's priorArchiveText on the same
// paths: the file's name, whichever separator the row's path uses.
//
// Mutants: the whole path shown; filepath.Base used (on Linux a Windows row
// shows its whole path).
func TestPriorArchiveLine(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"", ""},
		{`D:\out\Chan\Show.restart-1700000000.mp4`, "Show.restart-1700000000.mp4 (kept, not replaced)"},
		{"/out/Chan/Show.mp4", "Show.mp4 (kept, not replaced)"},
	} {
		if got := priorArchiveLine(&database.Job{PriorOutputFile: tc.path}); got != tc.want {
			t.Errorf("priorArchiveLine(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestGapFillLine pins the TUI's wording of a gap-fill (owner decision
// PR-R5), the dashboard's gapFillText.
//
// Mutants: the pending line without its spans or deadline; the kept line
// without its reason; an unknown state drawn.
func TestGapFillLine(t *testing.T) {
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	until := now.Add(3 * time.Hour).Format(time.RFC3339)
	pending := gapFillLine(&GapFillView{State: "pending", Spans: 2, Until: until, Reason: "the VOD is not published yet"}, now)
	if !strings.HasPrefix(pending, "Gap-fill pending: 2 spans, until ") || !strings.Contains(pending, "(in 3h") ||
		!strings.HasSuffix(pending, " (the VOD is not published yet)") {
		t.Errorf("pending = %q", pending)
	}
	for _, tc := range []struct {
		g    *GapFillView
		want string
	}{
		{nil, ""},
		{&GapFillView{State: "filled", Spans: 1}, "Gap-fill: filled 1 span"},
		{&GapFillView{State: "filled", Spans: 3, Reason: "one span is past the VOD's end"}, "Gap-fill: filled 3 spans — one span is past the VOD's end"},
		{&GapFillView{State: "kept", Reason: "the archive VOD was deleted"}, "Gap-fill: kept as recorded: the archive VOD was deleted"},
		{&GapFillView{State: "other"}, ""},
	} {
		if got := gapFillLine(tc.g, now); got != tc.want {
			t.Errorf("gapFillLine(%+v) = %q, want %q", tc.g, got, tc.want)
		}
	}
}

// TestDetailsPanelShowsLateStartAndGapFill: the details panel draws both
// lines as the dashboard's dialog does, the gap-fill from the App's
// JobGapFill — asked only for a Finished live Twitch job — and a job switch
// drops the previous job's gap-fill.
//
// Mutants: the Start row left out of buildRows; the Gap-fill row left out;
// updateSelectedJob not pushing the gap-fill; gapFillFor asking for any job
// (the YouTube row asks); its IsVod guard dropped (the Twitch VOD row asks).
func TestDetailsPanelShowsLateStartAndGapFill(t *testing.T) {
	app := NewApp()
	var asked []string
	app.JobGapFill = func(id string) *GapFillView {
		asked = append(asked, id)
		return &GapFillView{State: "kept", Reason: "no archive VOD"}
	}
	app.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	app.taskList.SetJobs([]*database.Job{
		{ID: "tw", Title: "Twitch", Platform: "twitch", Status: database.StatusFinished, CreatedAt: "2026-10-04T10:00:00Z"},
		{ID: "yt", Title: "YouTube", Platform: "youtube", Status: database.StatusFinished, LiveEdgeReason: edgeReason, CreatedAt: "2026-10-04T09:00:00Z"},
		{ID: "twv", Title: "Twitch VOD", Platform: "twitch", IsVod: true, Status: database.StatusFinished, CreatedAt: "2026-10-04T08:00:00Z"},
	})
	app.updateSelectedJob()
	if sel := app.taskList.SelectedJob(); sel == nil || sel.ID != "tw" {
		t.Fatalf("premise lost: selected %v", sel)
	}
	if r := fieldRow(app.details, "Gap-fill"); r == nil || r.value != "Gap-fill: kept as recorded: no archive VOD" {
		t.Errorf("Gap-fill row = %+v", r)
	}
	if strings.Join(asked, ",") != "tw" {
		t.Errorf("JobGapFill asked for %v, want the Twitch job alone", asked)
	}

	app.taskList.MoveDown()
	app.updateSelectedJob()
	if r := fieldRow(app.details, "Gap-fill"); r != nil {
		t.Errorf("the YouTube job shows the Twitch job's gap-fill: %+v", r)
	}
	if strings.Join(asked, ",") != "tw" {
		t.Errorf("JobGapFill asked for %v; a YouTube job has no gap-fill to ask about", asked)
	}
	if r := fieldRow(app.details, "Start"); r == nil || r.value != "Recorded from the live edge: "+edgeReason {
		t.Errorf("Start row = %+v", r)
	}

	// A Twitch VOD is downloaded whole: it has no gap-fill to ask about.
	app.taskList.MoveDown()
	app.updateSelectedJob()
	if sel := app.taskList.SelectedJob(); sel == nil || sel.ID != "twv" {
		t.Fatalf("premise lost: selected %v", sel)
	}
	if strings.Join(asked, ",") != "tw" {
		t.Errorf("JobGapFill asked for %v; a Twitch VOD has no gap-fill to ask about", asked)
	}
	if r := fieldRow(app.details, "Gap-fill"); r != nil {
		t.Errorf("the Twitch VOD shows a gap-fill: %+v", r)
	}
}
