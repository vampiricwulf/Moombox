package routes

import (
	"net/http"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestAddRoutesSendOneJobAddedEmbed is the route half of audit C3: the two add
// routes sent two different titles for the same action.
//
// Mutants this kill:
//   - leaving "Twitch Video Added" in place on the Twitch route.
//   - dropping the advanced-option fields from the YouTube route.
func TestAddRoutesSendOneJobAddedEmbed(t *testing.T) {
	t.Run("youtube", func(t *testing.T) {
		f := newJobsFixture(t)
		f.yt.meta = &YouTubeJobMetadata{Title: "A Title", ChannelName: "A Channel", ThumbnailURL: "https://i.ytimg.example/t.jpg"}

		rec := f.post(t, "/api/jobs", `{"videoId":"dQw4w9WgXcQ","selectedVideoItag":299}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST /api/jobs = %d, want 201", rec.Code)
		}
		calls := f.notify.ByEvent("added")
		if len(calls) != 1 {
			t.Fatalf("recorded %d added calls, want 1", len(calls))
		}
		if calls[0].Title != "Job Added" {
			t.Errorf("title = %q, want %q", calls[0].Title, "Job Added")
		}
		if calls[0].Opts.Author == nil || calls[0].Opts.Author.Name != "A Channel" {
			t.Error("the add embed carries no author line")
		}
		if _, ok := calls[0].Field("Video Format"); !ok {
			t.Error("the selected video itag is missing from the embed")
		}
	})

	t.Run("twitch", func(t *testing.T) {
		f := newJobsFixture(t)
		// IsLive is load-bearing: internal/web/routes/jobs.go:712 gates the
		// live-add branch on it, and without it the route falls to the
		// tw_manual_<login>_<unixnano> fallback at :728 — every assertion
		// below would still pass while testing a path this subtest does not
		// name.
		f.tw.streamMeta = &TwitchJobMetadata{
			Title: "Live", ChannelName: "Streamer", StreamID: "12345",
			IsLive: true, AvatarURL: "https://static.example/p.png",
		}

		rec := f.post(t, "/api/jobs", `{"url":"https://www.twitch.tv/streamer"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST /api/jobs = %d, want 201", rec.Code)
		}
		calls := f.notify.ByEvent("added")
		if len(calls) != 1 {
			t.Fatalf("recorded %d added calls, want 1", len(calls))
		}
		if calls[0].Title != "Job Added" {
			t.Errorf("title = %q — the Twitch route still names the platform in its title", calls[0].Title)
		}
		if v, ok := calls[0].Field("Stream ID"); !ok || v != "tw_12345" {
			t.Errorf("Stream ID = %q (present=%v), want the job id the old send carried", v, ok)
		}
		if calls[0].Opts.Author == nil || calls[0].Opts.Author.IconURL == "" {
			t.Error("the Twitch add embed lost the avatar the row carries")
		}
	})
}

// TestCancelRouteSendsTheOneCancelEmbed is C5's route half. The route only
// sends when the worker did not (jobs.go dedupes on CancelJob's bool), so this
// fixture's nil worker is exactly the shape that reaches the send.
func TestCancelRouteSendsTheOneCancelEmbed(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "vid9", func(j *database.Job) {
		j.Status = database.StatusUpcoming
		j.Title = "Cancel Me"
		j.URL = ""
	})

	rec := f.post(t, "/api/jobs/vid9/cancel", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST cancel = %d, want 200", rec.Code)
	}
	calls := f.notify.ByEvent("cancelled")
	if len(calls) != 1 {
		t.Fatalf("recorded %d cancelled calls, want 1", len(calls))
	}
	if calls[0].Title != "Job Cancelled" {
		t.Errorf("title = %q", calls[0].Title)
	}
	if calls[0].Opts.URL != "https://www.youtube.com/watch?v=vid9" {
		t.Errorf("opts.URL = %q, want the watch-URL fallback for a row with no url", calls[0].Opts.URL)
	}
}
