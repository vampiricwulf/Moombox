package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// ladder is a source above two transcodes, all named the way Twitch names
// them: the source by its group ("chunked"), the transcodes by their size.
func ladder() []twitch.TwitchHLSVariant {
	return []twitch.TwitchHLSVariant{
		{Name: "chunked", Bandwidth: 8000000, Width: 1920, Height: 1080, FPS: 60, VideoCodec: "avc1", IsSource: true},
		{Name: "720p60", Bandwidth: 3000000, Width: 1280, Height: 720, FPS: 60, VideoCodec: "avc1"},
		{Name: "480p30", Bandwidth: 1500000, Width: 854, Height: 480, FPS: 30, VideoCodec: "avc1"},
	}
}

// TestTwitchSelectionReadsOnlyThePreference: every Twitch selection — the
// capture start's and every re-selection's — reads the job's
// quality_preference and nothing else. The rows are the shapes a restart
// meets: an earlier run started on the source and wrote its name to
// twitch_quality, and the job was created to record 720p; and a row from
// before Twitch rows always named a preference, its quality_preference empty,
// which an earlier run recorded at 720p60 — empty selects as "best".
//
// Mutants: selectTwitchVariant reading job.TwitchQuality (the first row's
// "chunked" matches the source by name, so the capture restarts at 1080p, and
// the second row's "720p60" pins 720p); newTwitchVariantInfo reading it (the
// re-selections follow the same names, so the first probe reports a quality
// change).
func TestTwitchSelectionReadsOnlyThePreference(t *testing.T) {
	sp := &StreamProcessor{cfg: &config.MoomboxConfig{}}
	for _, tc := range []struct {
		job  *database.Job
		want string
	}{
		{&database.Job{ID: "tw_1", Platform: "twitch", TwitchQuality: "chunked", QualityPreference: "720p"}, "720p60"},
		{&database.Job{ID: "tw_2", Platform: "twitch", TwitchQuality: "720p60", QualityPreference: ""}, "chunked"},
	} {
		if got := sp.selectTwitchVariant(ladder(), tc.job); got == nil || got.Name != tc.want {
			t.Errorf("%s: capture start selected %v, want %s — the job's preference, not the variant an earlier run recorded", tc.job.ID, got, tc.want)
		}
		info := newTwitchVariantInfo(tc.job, &ladder()[1], &JobConfig{})
		if info.QualityPref != tc.job.QualityPreference {
			t.Errorf("%s: TwitchVariantInfo.QualityPref = %q, want the job's quality_preference %q", tc.job.ID, info.QualityPref, tc.job.QualityPreference)
		}
		if got := info.selectFrom(ladder()); got == nil || got.Name != tc.want {
			t.Errorf("%s: re-selection picked %v, want %s", tc.job.ID, got, tc.want)
		}
	}
}

// TestStartTwitchVariantRecordsThePick: the capture start writes the variant
// it picked to twitch_quality — the column both UIs show as "Quality" — in the
// same write as the caller's extra fields, live and VOD alike, and leaves the
// preference, quality_preference, alone.
//
// Mutants: the twitch_quality entry dropped from startTwitchVariant's write
// (the VOD row keeps the empty value it was created with); the extra fields
// not merged (the live row is not flipped Live).
func TestStartTwitchVariantRecordsThePick(t *testing.T) {
	_, db := testWorkerSetup(t)
	for _, id := range []string{"tw_vod", "tw_live"} {
		if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, Platform: "twitch",
			Status: database.StatusUpcoming, QualityPreference: "720p"}); err != nil {
			t.Fatal(err)
		}
	}
	sp := &StreamProcessor{cfg: &config.MoomboxConfig{}, db: db}

	vod, _ := db.GetJob("tw_vod")
	if got, outcome := sp.startTwitchVariant(ladder(), vod, nil); got == nil || got.Name != "720p60" || outcome {
		t.Fatalf("VOD start picked %v (outcome %v), want 720p60 and no outcome", got, outcome)
	}
	live, _ := db.GetJob("tw_live")
	if _, outcome := sp.startTwitchVariant(ladder(), live, map[string]any{"status": database.StatusLive}); outcome {
		t.Errorf("the live start's write on an Upcoming row reported an outcome")
	}

	for id, wantStatus := range map[string]database.JobStatus{"tw_vod": database.StatusUpcoming, "tw_live": database.StatusLive} {
		j, _ := db.GetJob(id)
		if j.TwitchQuality != "720p60" {
			t.Errorf("%s twitch_quality = %q, want the picked variant 720p60", id, j.TwitchQuality)
		}
		if j.QualityPreference != "720p" {
			t.Errorf("%s quality_preference = %q, want it untouched", id, j.QualityPreference)
		}
		if j.Status != wantStatus {
			t.Errorf("%s status = %q, want %q", id, j.Status, wantStatus)
		}
	}

	// Nothing fits: nothing is written.
	if got, _ := sp.startTwitchVariant(nil, vod, nil); got != nil {
		t.Errorf("an empty playlist picked %v", got)
	}
	if j, _ := db.GetJob("tw_vod"); j.TwitchQuality != "720p60" {
		t.Errorf("a failed pick rewrote twitch_quality to %q", j.TwitchQuality)
	}
}

// TestTwitchJobQualityPreferenceIsNeverEmpty: every Twitch row created now
// names its preference, "best" when nothing was named.
//
// Mutant: TwitchJobQualityPreference returning the preference unchanged.
func TestTwitchJobQualityPreferenceIsNeverEmpty(t *testing.T) {
	if got := TwitchJobQualityPreference(""); got != "best" {
		t.Errorf("TwitchJobQualityPreference(\"\") = %q, want best", got)
	}
	if got := TwitchJobQualityPreference("720p"); got != "720p" {
		t.Errorf("TwitchJobQualityPreference(720p) = %q", got)
	}
}

// TestTwitchChannelQualityPreference: the configured Twitch channel's
// preference, matched without regard to case; a YouTube channel with the same
// ID is not it; an unconfigured login, or a channel with no preference, is
// "best".
//
// Mutants: the platform check dropped (the YouTube channel's 360p is
// returned); exact-case matching (the mixed-case login falls to "best").
func TestTwitchChannelQualityPreference(t *testing.T) {
	channels := []config.ChannelConfig{
		{ID: "samename", Platform: "youtube", QualityPreference: "360p"},
		{ID: "samename", Platform: "twitch", QualityPreference: "720p60"},
		{ID: "noprefs", Platform: "twitch"},
	}
	for login, want := range map[string]string{
		"SameName": "720p60",
		"noprefs":  "best",
		"stranger": "best",
		"":         "best",
	} {
		if got := TwitchChannelQualityPreference(channels, login); got != want {
			t.Errorf("TwitchChannelQualityPreference(%q) = %q, want %q", login, got, want)
		}
	}
}
