package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestDetailsPanelSaysTheArchiveHasNoAudio: a Finished job the finalize's
// missing-audio guard flagged shows an Audio line in its Media section, as
// the dashboard shows its "No audio" badge; an active job with a stale flag,
// and an unflagged Finished job, show none.
//
// Mutant: the Audio line not added (the flagged job shows nothing).
func TestDetailsPanelSaysTheArchiveHasNoAudio(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status database.JobStatus
		flag   bool
		want   bool
	}{
		{"flagged finished", database.StatusFinished, true, true},
		{"unflagged finished", database.StatusFinished, false, false},
		{"stale flag while downloading", database.StatusDownloading, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewJobDetailsModel()
			m.SetSize(100, 40)
			m.SetJob(&database.Job{ID: "j1", Title: "t", VideoID: "v", Status: tc.status, Platform: "youtube", MissingAudio: tc.flag})
			found := false
			for _, r := range m.rows {
				if r.label == "Audio" && r.value == MissingAudioText {
					found = true
				}
			}
			if found != tc.want {
				t.Errorf("Audio line shown %v, want %v", found, tc.want)
			}
		})
	}
}

// TestTaskRowSaysTheArchiveHasNoAudio: a flagged Finished row carries a dim
// "no audio" tag in its progress slot, in the normal and the compact
// drawing, as the dashboard's row carries its No audio mark, so a silent
// archive is seen in the list without opening its details. The tag is
// counted in the title budget, so the row never wraps; an unflagged row and
// an active row with a stale flag carry none.
//
// Mutants: the Finished case left out of progressCell (no tag in either
// drawing); the Finished gate dropped (the stale flag tags a downloading
// row); compactFigure not asking progressCell (the compact row lacks it).
func TestTaskRowSaysTheArchiveHasNoAudio(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	long := strings.Repeat("Title ", 20)
	for _, tc := range []struct {
		name   string
		status database.JobStatus
		flag   bool
		want   bool
	}{
		{"flagged finished", database.StatusFinished, true, true},
		{"unflagged finished", database.StatusFinished, false, false},
		{"stale flag while downloading", database.StatusDownloading, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := &database.Job{ID: "j1", Title: long, Status: tc.status, Platform: "youtube", MissingAudio: tc.flag, Percent: 42}
			m := NewTaskListModel()
			m.SetSize(60, 10)
			row := stripANSI(m.renderJob(job, false, false))
			if got := strings.Contains(row, strings.TrimSpace(missingAudioTag)); got != tc.want {
				t.Errorf("normal row tagged %v, want %v: %q", got, tc.want, row)
			}
			for line := range strings.SplitSeq(row, "\n") {
				if w := runewidth.StringWidth(line); w > 58 {
					t.Errorf("row wraps: width %d > 58: %q", w, line)
				}
			}
			c := NewTaskListModel()
			c.setCompact(true)
			c.SetSize(40, 4)
			compact := stripANSI(c.renderCompactJob(job, false, false, 40, now))
			if got := strings.Contains(compact, strings.TrimSpace(missingAudioTag)); got != tc.want {
				t.Errorf("compact row tagged %v, want %v: %q", got, tc.want, compact)
			}
		})
	}
}
