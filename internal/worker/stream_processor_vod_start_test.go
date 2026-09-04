package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// A job created Upcoming holds the SCHEDULED start. When it is later processed
// as a VOD (live capture never ran), the replay chat offsets count from the
// ACTUAL start that YouTube now reports in ScheduledStartTime — the row must
// follow or the player biases the chat late by the late-start delta.
func TestVodStatusUpdatesRefreshesStaleStart(t *testing.T) {
	job := &database.Job{ID: "j", StreamStartTime: "2026-06-11T10:00:00Z"}
	info := &youtube.VideoInfo{ScheduledStartTime: "2026-06-11T10:12:00Z"}
	u := vodStatusUpdates(job, info)
	if u["status"] != database.StatusDownloading || u["is_vod"] != true {
		t.Errorf("status/is_vod = %v/%v", u["status"], u["is_vod"])
	}
	if u["stream_start_time"] != "2026-06-11T10:12:00Z" {
		t.Errorf("stream_start_time = %v, want the actual start", u["stream_start_time"])
	}
}

func TestVodStatusUpdatesLeavesMatchingOrUnknownStart(t *testing.T) {
	same := vodStatusUpdates(&database.Job{StreamStartTime: "2026-06-11T10:12:00Z"},
		&youtube.VideoInfo{ScheduledStartTime: "2026-06-11T10:12:00Z"})
	if _, ok := same["stream_start_time"]; ok {
		t.Error("equal times must not write stream_start_time")
	}
	none := vodStatusUpdates(&database.Job{StreamStartTime: "2026-06-11T10:12:00Z"}, &youtube.VideoInfo{})
	if _, ok := none["stream_start_time"]; ok {
		t.Error("an empty ScheduledStartTime must not clear the row")
	}
}
