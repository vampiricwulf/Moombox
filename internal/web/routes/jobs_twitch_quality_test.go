package routes

import (
	"net/http"
	"testing"
)

// TestTwitchAddRecordsThePreferenceOnly is the Web add's half: a Twitch row,
// live or VOD, records the dialog's quality_preference ("best" when the dialog
// sent none) — the one preference every Twitch selection reads — and leaves
// twitch_quality, the variant being recorded, empty until a capture starts.
// It used to write the preference into twitch_quality too, where the stream
// start overwrote it.
//
// Mutants: twitch_quality set to the preference again; the preference written
// raw (an add with none stores "").
func TestTwitchAddRecordsThePreferenceOnly(t *testing.T) {
	f := newJobsFixture(t)
	f.tw.streamMeta = &TwitchJobMetadata{StreamID: "77", ChannelName: "Streamer", Title: "hi", IsLive: true}

	rec := doRequest(t, f.router, "POST", "/api/jobs",
		map[string]any{"platform": "twitch", "videoId": "streamer", "quality_preference": "720p"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("add: want 201, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	job, err := f.db.GetJob("tw_77")
	if err != nil || job == nil {
		t.Fatalf("GetJob tw_77: %v", err)
	}
	if job.QualityPreference != "720p" {
		t.Errorf("quality_preference = %q, want 720p", job.QualityPreference)
	}
	if job.TwitchQuality != "" {
		t.Errorf("twitch_quality = %q at creation, want empty — nothing is recording yet", job.TwitchQuality)
	}

	f.tw.streamMeta = &TwitchJobMetadata{StreamID: "78", ChannelName: "Other", IsLive: true}
	if rec := doRequest(t, f.router, "POST", "/api/jobs", map[string]any{"platform": "twitch", "videoId": "other"}); rec.Code != http.StatusCreated {
		t.Fatalf("add without a preference: want 201, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if job, _ := f.db.GetJob("tw_78"); job == nil {
		t.Error("the add without a preference stored no tw_78 row")
	} else if job.QualityPreference != "best" {
		t.Errorf("a Twitch add with no preference recorded quality_preference %q, want best", job.QualityPreference)
	}

	f.tw.vodMeta = &TwitchJobMetadata{ChannelName: "Streamer", Title: "old"}
	if rec := doRequest(t, f.router, "POST", "/api/jobs",
		map[string]any{"platform": "twitch", "videoId": "123", "twitchType": "vod"}); rec.Code != http.StatusCreated {
		t.Fatalf("VOD add: want 201, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if job, _ := f.db.GetJob("tw_v123"); job == nil {
		t.Error("the VOD add stored no tw_v123 row")
	} else if job.QualityPreference != "best" || job.TwitchQuality != "" {
		t.Errorf("a Twitch VOD add recorded quality_preference %q, twitch_quality %q; want best and empty",
			job.QualityPreference, job.TwitchQuality)
	}
}
