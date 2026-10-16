package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestVodStatusUpdates pins the row update a job processed as a VOD gets:
// stream_start_time follows the start YouTube reports, and nothing else moves.
//
// Every row also pins that the update writes NO status: the VOD's
// download-slot wait is still ahead, and Downloading here showed every queued
// VOD as downloading (mutant: put "status": StatusDownloading back).
func TestVodStatusUpdates(t *testing.T) {
	for _, tc := range []struct {
		name      string
		jobStart  string
		infoStart string
		wantStart string // "" = stream_start_time not written
	}{
		// A job created Upcoming holds the SCHEDULED start. When it is later
		// processed as a VOD (live capture never ran), the replay chat offsets
		// count from the ACTUAL start that YouTube now reports in
		// ScheduledStartTime — the row must follow or the player biases the
		// chat late by the late-start delta.
		{"refreshes stale start", "2026-06-11T10:00:00Z", "2026-06-11T10:12:00Z", "2026-06-11T10:12:00Z"},
		// An empty job.StreamStartTime (both epochs unknown —
		// stream_processor.go:266-268) must still be FILLED from
		// info.ScheduledStartTime, not skipped as "already matching" — a
		// condition that additionally required job.StreamStartTime != ""
		// would silently drop this fill (M-D).
		{"fills empty start", "", "2026-06-11T10:12:00Z", "2026-06-11T10:12:00Z"},
		// Equal times must not write stream_start_time.
		{"leaves matching start", "2026-06-11T10:12:00Z", "2026-06-11T10:12:00Z", ""},
		// An empty ScheduledStartTime must not clear the row.
		{"leaves unknown start", "2026-06-11T10:12:00Z", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := vodStatusUpdates(&database.Job{ID: "j", StreamStartTime: tc.jobStart}, &youtube.VideoInfo{ScheduledStartTime: tc.infoStart})
			if _, ok := u["status"]; ok || u["is_vod"] != true {
				t.Errorf("status/is_vod = %v/%v, want no status (the slot wait is still ahead) and is_vod", u["status"], u["is_vod"])
			}
			got, ok := u["stream_start_time"]
			if tc.wantStart == "" {
				if ok {
					t.Errorf("stream_start_time = %v, want no write", got)
				}
			} else if got != tc.wantStart {
				t.Errorf("stream_start_time = %v, want %s", got, tc.wantStart)
			}
		})
	}
}
