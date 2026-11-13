package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobstate"
)

// The compact row's figure for a Downloading, Muxing, Queued, Error or COOKIES? row is
// the normal row's progress cell, reformatted and nothing else: one rule
// (progressCell), two drawings.
func TestCompactFigureIsTheProgressCell(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	m := NewTaskListModel()
	m.setCompact(true)
	m.SetSize(40, 4)
	m.SetAdmissionHolds(jobstate.AdmissionHolds{Offline: true,
		CookieRetries: map[string]jobstate.CookieRetry{"parked": {Count: 2, Of: 3, Last: now.Add(-time.Hour)}}})

	jobs := []*database.Job{
		{ID: "parked", Platform: "youtube", Status: database.StatusCookies, ParkReason: database.ParkReasonAuth},
		{ID: "wait", Status: database.StatusDownloading, Percent: 50, Progress: jobstate.WaitLine("verifying the stream ended", "")},
		{ID: "pct", Status: database.StatusDownloading, Percent: 7},
		{ID: "full", Status: database.StatusMuxing, Percent: 100},
		{ID: "held", Status: database.StatusQueued, Platform: "youtube"},
		{ID: "mux", Platform: "twitch", Status: database.StatusError, Error: "twitch HLS: 500",
			ParkReason: database.ParkReasonTwitchEndUnconfirmed},
		{ID: "retry", Platform: "twitch", Status: database.StatusError, Error: jobstate.TwitchOfflineErrMsg,
			ParkReason: database.ParkReasonTwitchOffline},
		{ID: "plain", Platform: "twitch", Status: database.StatusError, Error: "boom"},
	}
	for _, job := range jobs {
		cell, cellDim := m.progressCell(job, now)
		if job.ID != "plain" && cell == "" {
			t.Fatalf("%s: premise lost: the normal row has no progress cell", job.ID)
		}
		got, dim := m.compactFigure(job, now)
		if strings.TrimSpace(got) != strings.TrimSpace(cell) || (cell != "" && dim != cellDim) {
			t.Errorf("%s: compact figure %q (dim %v), the progress cell is %q (dim %v)", job.ID, got, dim, cell, cellDim)
		}
		if got != strings.TrimRight(got, " ") {
			t.Errorf("%s: the figure %q keeps the cell's trailing space", job.ID, got)
		}
	}
	// A percent is four cells at every value.
	for _, id := range []string{"pct", "full"} {
		for _, job := range jobs {
			if got, _ := m.compactFigure(job, now); job.ID == id && ansi.StringWidth(got) != 4 {
				t.Errorf("%s: the percent %q is not four cells", id, got)
			}
		}
	}
}

// A status style that underlines (Error, COOKIES?, an incomplete Finished)
// underlines the tag's letters only, never the cells that pad it to four.
func TestCompactTagPaddingIsNotUnderlined(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	m := NewTaskListModel()
	m.setCompact(true)
	m.SetSize(40, 4)
	underlined := 0
	for _, status := range []database.JobStatus{database.StatusError, database.StatusMuxing, database.StatusQueued} {
		job := &database.Job{ID: "j", Title: "A title", Status: status}
		tag := compactStatusTag(status)
		if len(tag) >= compactTagWidth {
			t.Fatalf("premise lost: %s has no padding", tag)
		}
		row := m.renderCompactJob(job, false, false, 40, now)
		styled := statusMarkStyle(job, false).Render(tag)
		if !strings.HasPrefix(row, styled+strings.Repeat(" ", compactTagWidth-len(tag)+compactTagGap)) {
			t.Errorf("%s: the row does not open with the styled tag and plain padding: %q", status, row)
		}
		if strings.Contains(styled, ";4m") || strings.Contains(styled, "[4m") || strings.Contains(styled, "4:3") {
			underlined++
		}
		if got := ansi.Strip(row); !strings.HasPrefix(got, tag+strings.Repeat(" ", compactTagWidth-len(tag))+" A title") || ansi.StringWidth(got) != 40 {
			t.Errorf("%s: the plain row changed: %q", status, got)
		}
	}
	if underlined == 0 {
		t.Error("premise lost: no status style here underlines")
	}
}

// A tab or another control character in a title is drawn as a space, so the
// title is measured as it is drawn: the ellipsis and the figure stay.
func TestCompactTitleControlCharacters(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	m := NewTaskListModel()
	m.setCompact(true)
	m.SetSize(30, 4)
	job := &database.Job{ID: "j", Title: "A broken\tstream\twith a\ttab and\x07more text\nhere", Status: database.StatusDownloading, Percent: 42}
	for _, selected := range []bool{false, true} {
		row := m.renderCompactJob(job, selected, false, 30, now)
		got := ansi.Strip(row)
		if want := "DOWN A broken stream wit…  42%"; got != want || ansi.StringWidth(got) != 30 {
			t.Errorf("selected=%v:\n got %q\nwant %q", selected, got, want)
		}
		if strings.ContainsAny(row, "\t\n\x07") {
			t.Errorf("selected=%v: a control character reached the row: %q", selected, row)
		}
	}
}
