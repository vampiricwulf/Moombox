package routes

import (
	"net/http"
	"testing"
)

// TestTwitchAddRecordsThePreferenceOnly is the Web add's half of D-T9: the row
// records the dialog's quality_preference as its twitch_quality_preference
// ("best" when the dialog sent none) and leaves twitch_quality — the variant
// being recorded — empty until a capture starts. It used to write the
// preference into twitch_quality, where the stream start overwrote it.
//
// Mutants: twitch_quality set to the preference again; the preference written
// raw (an empty one stores "", the mark of a row that predates the column);
// twitch_quality_preference left unset.
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
	if job.TwitchQualityPreference != "720p" {
		t.Errorf("twitch_quality_preference = %q, want 720p", job.TwitchQualityPreference)
	}
	if job.TwitchQuality != "" {
		t.Errorf("twitch_quality = %q at creation, want empty — nothing is recording yet", job.TwitchQuality)
	}

	f.tw.streamMeta = &TwitchJobMetadata{StreamID: "78", ChannelName: "Other", IsLive: true}
	if rec := doRequest(t, f.router, "POST", "/api/jobs", map[string]any{"platform": "twitch", "videoId": "other"}); rec.Code != http.StatusCreated {
		t.Fatalf("add without a preference: want 201, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if job, _ := f.db.GetJob("tw_78"); job == nil || job.TwitchQualityPreference != "best" {
		t.Errorf("a Twitch add with no preference recorded %+v, want twitch_quality_preference best", job)
	}
}
