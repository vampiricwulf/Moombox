package tui

import (
	"testing"

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
