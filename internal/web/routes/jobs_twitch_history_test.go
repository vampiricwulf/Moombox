package routes

import (
	"net/http"
	"slices"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// A Twitch VOD or live broadcast added by a single URL is in the processing
// history from the add (owner ruling RV-3), under the job ID the VOD backlog
// and the live monitor look up there, so deleting the job leaves an Orphaned
// entry that holds the video back until the operator removes it. Before, the
// add wrote no row and a Twitch delete writes none, so the next backlog
// listing or live check archived it again. An add that waits for an offline
// channel has an ID no monitor reads and leaves no entry.
//
// Mutants: the RecordManualTwitchAdd call dropped from the add route (no
// entry after the delete); the waiting add recorded (an entry for it).
func TestADeletedTwitchURLAddStaysInTheHistory(t *testing.T) {
	f := newJobsFixture(t)
	f.tw.vodMeta = &TwitchJobMetadata{ChannelName: "Streamer", Title: "old"}
	f.tw.streamMeta = &TwitchJobMetadata{StreamID: "77", ChannelName: "Streamer", Title: "hi", IsLive: true}
	add := func(body map[string]any) {
		t.Helper()
		if rec := doRequest(t, f.router, "POST", "/api/jobs", body); rec.Code != http.StatusCreated {
			t.Fatalf("add %v: want 201, got %d (%s)", body, rec.Code, rec.Body.String())
		}
	}
	add(map[string]any{"platform": "twitch", "videoId": "2412345678", "twitchType": "vod"})
	add(map[string]any{"platform": "twitch", "videoId": "streamer"})
	f.tw.streamMeta = &TwitchJobMetadata{ChannelName: "Sleeper"} // not live: the job waits
	add(map[string]any{"platform": "twitch", "videoId": "sleeper"})

	vodID, liveID := twitch.BuildJobID("2412345678", true), twitch.BuildJobID("77", false)
	if orphaned, err := f.db.ListOrphanedHistory(); err != nil || len(orphaned) != 0 {
		t.Fatalf("orphaned history while the jobs exist = %v (%v), want none", orphaned, err)
	}
	jobs, err := f.db.GetAllJobs()
	if err != nil || len(jobs) != 3 {
		t.Fatalf("jobs = %d (%v), want the three adds", len(jobs), err)
	}
	for _, j := range jobs {
		if rec := doRequest(t, f.router, "DELETE", "/api/jobs/"+j.ID, nil); rec.Code != http.StatusOK {
			t.Fatalf("delete %s: %d (%s)", j.ID, rec.Code, rec.Body.String())
		}
	}

	orphaned, err := f.db.ListOrphanedHistory()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range orphaned {
		got = append(got, e.VideoID)
	}
	slices.Sort(got)
	want := []string{liveID, vodID}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("orphaned history after the deletes = %v, want %v: the VOD and the live broadcast, not the waiting add", got, want)
	}
	for _, id := range want {
		if seen, _ := f.db.HasProcessed(id); !seen {
			t.Errorf("%s is not held back from its monitor", id)
		}
	}
}
