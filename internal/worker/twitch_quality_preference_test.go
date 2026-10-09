package worker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// TestTwitchSelectionReadsOnlyThePreference is D-T9's read half. The row is
// the shape a restart meets: an earlier run started on the source and wrote
// its name to twitch_quality, the job was created to record 720p, and
// quality_preference is empty — as it is on a row older than that column,
// which the startup backfill gives its channel's preference. Every selection,
// the capture start's and every re-selection's, must pick 720p.
//
// Mutants: selectTwitchVariant reading job.TwitchQuality (its "chunked"
// matches the source by name, so the capture restarts at 1080p); reading
// job.QualityPreference (empty selects "best", the source);
// newTwitchVariantInfo reading either of them (the re-selections pick the
// source, so the first probe reports a quality change).
func TestTwitchSelectionReadsOnlyThePreference(t *testing.T) {
	job := &database.Job{ID: "tw_1", Platform: "twitch",
		TwitchQuality: "chunked", TwitchQualityPreference: "720p", QualityPreference: ""}

	sp := &StreamProcessor{cfg: &config.MoomboxConfig{}}
	if got := sp.selectTwitchVariant(ladder(), job); got == nil || got.Name != "720p60" {
		t.Errorf("capture start selected %v, want 720p60 — the job's preference, not the variant an earlier run recorded", got)
	}
	info := newTwitchVariantInfo(job, &ladder()[1], &JobConfig{})
	if info.QualityPref != "720p" {
		t.Errorf("TwitchVariantInfo.QualityPref = %q, want the job's twitch_quality_preference", info.QualityPref)
	}
	if got := info.selectFrom(ladder()); got == nil || got.Name != "720p60" {
		t.Errorf("re-selection picked %v, want 720p60", got)
	}
}

// TestStartTwitchVariantRecordsThePick: the capture start writes the variant
// it picked to twitch_quality — the column both UIs show as "Quality" — in the
// same write as the caller's extra fields, live and VOD alike, and leaves the
// preference alone.
//
// Mutants: the twitch_quality entry dropped from startTwitchVariant's write
// (the VOD row keeps the empty value it was created with); the extra fields
// not merged (the live row is not flipped Live).
func TestStartTwitchVariantRecordsThePick(t *testing.T) {
	_, db := testWorkerSetup(t)
	for _, id := range []string{"tw_vod", "tw_live"} {
		if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, Platform: "twitch",
			Status: database.StatusUpcoming, TwitchQualityPreference: "720p"}); err != nil {
			t.Fatal(err)
		}
	}
	sp := &StreamProcessor{cfg: &config.MoomboxConfig{}, db: db}

	vod, _ := db.GetJob("tw_vod")
	if got := sp.startTwitchVariant(ladder(), vod, nil); got == nil || got.Name != "720p60" {
		t.Fatalf("VOD start picked %v, want 720p60", got)
	}
	live, _ := db.GetJob("tw_live")
	sp.startTwitchVariant(ladder(), live, map[string]any{"status": database.StatusLive})

	for id, wantStatus := range map[string]database.JobStatus{"tw_vod": database.StatusUpcoming, "tw_live": database.StatusLive} {
		j, _ := db.GetJob(id)
		if j.TwitchQuality != "720p60" {
			t.Errorf("%s twitch_quality = %q, want the picked variant 720p60", id, j.TwitchQuality)
		}
		if j.TwitchQualityPreference != "720p" {
			t.Errorf("%s twitch_quality_preference = %q, want it untouched", id, j.TwitchQualityPreference)
		}
		if j.Status != wantStatus {
			t.Errorf("%s status = %q, want %q", id, j.Status, wantStatus)
		}
	}

	// Nothing fits: nothing is written.
	if got := sp.startTwitchVariant(nil, vod, nil); got != nil {
		t.Errorf("an empty playlist picked %v", got)
	}
	if j, _ := db.GetJob("tw_vod"); j.TwitchQuality != "720p60" {
		t.Errorf("a failed pick rewrote twitch_quality to %q", j.TwitchQuality)
	}
}

// TestTwitchJobQualityPreferenceIsNeverEmpty: an empty column is the mark of a
// row that predates it — the backfill keys on it — so a new row may not
// write one.
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

// TestBackfillTwitchQualityPreferencesRule pins the rule the startup backfill
// applies to a row that predates the column, as owner decision D-T9 states
// it: its channel's current preference while the channel is configured, else
// "best" — and a VOD is never matched to a channel, since its URL's first path
// segment is "videos", not a login. The row's own quality_preference, the
// channel's setting when the row was created, is never consulted: tw_recorded
// was created at 720p on a channel now set to 480p, and tw_away recorded
// 1080p60 on a channel the config no longer holds.
//
// Mutants: the row's own quality_preference consulted first (the shipped rule
// before this test: tw_recorded keeps 720p, tw_away keeps 1080p60); the
// channel lookup dropped (tw_channel and tw_recorded fall to "best"); the VOD
// guard dropped (tw_vvod is matched to the "videos" channel's 160p).
func TestBackfillTwitchQualityPreferencesRule(t *testing.T) {
	_, db := testWorkerSetup(t)
	for _, j := range []*database.Job{
		{ID: "tw_recorded", URL: "https://twitch.tv/streamer", QualityPreference: "720p"},
		{ID: "tw_channel", URL: "https://twitch.tv/Streamer"},
		{ID: "tw_away", URL: "https://twitch.tv/gone", QualityPreference: "1080p60"},
		{ID: "tw_stranger", URL: "https://twitch.tv/someoneelse"},
		{ID: "tw_vvod", URL: "https://www.twitch.tv/videos/123"},
	} {
		j.VideoID, j.Platform, j.Status = j.ID, "twitch", database.StatusFinished
		if _, err := db.AddJob(j); err != nil {
			t.Fatal(err)
		}
	}
	channels := []config.ChannelConfig{
		{ID: "streamer", Platform: "twitch", QualityPreference: "480p"},
		{ID: "videos", Platform: "twitch", QualityPreference: "160p"},
	}
	n, err := BackfillTwitchQualityPreferences(db, channels)
	if err != nil || n != 5 {
		t.Fatalf("BackfillTwitchQualityPreferences = %d, %v; want 5, nil", n, err)
	}
	for id, want := range map[string]string{
		"tw_recorded": "480p",
		"tw_channel":  "480p",
		"tw_away":     "best",
		"tw_stranger": "best",
		"tw_vvod":     "best",
	} {
		j, _ := db.GetJob(id)
		if j.TwitchQualityPreference != want {
			t.Errorf("%s backfilled to %q, want %q", id, j.TwitchQualityPreference, want)
		}
	}
}

// TestTwitchQualitySplitRecordsTheNewVariant drives the real ExecuteTwitch
// through a quality split and checks twitch_quality follows the capture: the
// 720p variant's playlist dies, the refresh offers only 1080p, and the row
// must name the variant now being recorded. It was written once, at the
// stream start, so a split left it naming the variant the job had left.
//
// The split comes early, inside minSegmentDuration, so the closed part is the
// short-segment discard — the variant changes all the same, and the test
// stays short.
//
// Mutant: the recordVariant call removed from the quality-split branch (the
// row still names 720p30).
func TestTwitchQualitySplitRecordsTheNewVariant(t *testing.T) {
	srv, low, high, switched := twoVariantServer(t)
	h := newEndVerdictHarness(t, "tw_qsplit_quality")
	h.variant.URL, h.variant.Name, h.variant.Width, h.variant.Height, h.variant.FPS = low.URL, low.Name, low.Width, low.Height, low.FPS
	h.variant.CheckStreamFn = func(context.Context) (bool, error) { return !srv.ended(), nil }
	h.variant.FetchVariantsFn = func(context.Context) ([]twitch.TwitchHLSVariant, error) {
		if switched() {
			return []twitch.TwitchHLSVariant{high}, nil
		}
		return []twitch.TwitchHLSVariant{low}, nil
	}
	h.db.UpdateJobFields(h.job.ID, map[string]any{"twitch_quality": low.Name})
	h.jobCtx.OutputDir = t.TempDir()
	h.jobCtx.Filename = "qsplit"

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := h.o.ExecuteTwitch(ctx, h.jobCtx, h.variant, false, nil); err != nil {
		t.Logf("ExecuteTwitch: %v", err)
	}

	j, err := h.db.GetJob(h.job.ID)
	if err != nil || j == nil {
		t.Fatalf("GetJob: %v", err)
	}
	if j.TwitchQuality != high.Name {
		t.Errorf("after the quality split twitch_quality = %q, want %q — the variant being recorded", j.TwitchQuality, high.Name)
	}
}

// twoVariantStream is a live broadcast offered at 720p30 and then, from
// switchAt, at 1080p60 only: the 720p playlist 404s from then on, as a
// rendition Twitch withdrew does. Both variants serve one sliding window of
// one-second segments, and the playlist ends ENDLIST at endAt.
type twoVariantStream struct {
	start           time.Time
	switchAt, endAt time.Duration
}

func (s *twoVariantStream) ended() bool { return time.Since(s.start) > s.endAt }

func twoVariantServer(t *testing.T) (*twoVariantStream, twitch.TwitchHLSVariant, twitch.TwitchHLSVariant, func() bool) {
	t.Helper()
	ts := oneSecondTS(t)
	st := &twoVariantStream{start: time.Now(), switchAt: 2 * time.Second, endAt: 5 * time.Second}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if !strings.HasSuffix(r.URL.Path, ".m3u8") {
			w.Header().Set("Content-Type", "video/mp2t")
			_, _ = w.Write(ts)
			return
		}
		name = strings.TrimSuffix(name, ".m3u8")
		el := time.Since(st.start)
		if name == "720" && el > st.switchAt {
			http.NotFound(w, r)
			return
		}
		head := int(el.Seconds())
		lo := max(0, head-5)
		var b strings.Builder
		fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n", lo)
		for seq := lo; seq <= head; seq++ {
			fmt.Fprintf(&b, "#EXTINF:1.000,\n/%s/%d.ts\n", name, seq)
		}
		if el > st.endAt {
			b.WriteString("#EXT-X-ENDLIST\n")
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte(b.String()))
	}))
	t.Cleanup(srv.Close)
	low := twitch.TwitchHLSVariant{URL: srv.URL + "/720.m3u8", Name: "720p30", Width: 1280, Height: 720, FPS: 30}
	high := twitch.TwitchHLSVariant{URL: srv.URL + "/1080.m3u8", Name: "1080p60", Width: 1920, Height: 1080, FPS: 60}
	return st, low, high, func() bool { return time.Since(st.start) > st.switchAt }
}
