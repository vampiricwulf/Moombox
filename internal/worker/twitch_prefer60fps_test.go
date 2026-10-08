package worker

import (
	"context"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// sixtyBesideThirty is one size offered twice: the broadcaster's 60 fps
// source, listed first and outbidding, and a 30 fps transcode. Only
// prefer_60fps can choose between them (D-Y2; the ranking itself is pinned in
// internal/twitch/hls_framerate_test.go — these tests pin that every Twitch
// selection site is HANDED the setting).
func sixtyBesideThirty() []twitch.TwitchHLSVariant {
	return []twitch.TwitchHLSVariant{
		{Name: "chunked", URL: "https://example.com/chunked.m3u8", Bandwidth: 8000000,
			Width: 1920, Height: 1080, FPS: 60, VideoCodec: "avc1", IsSource: true},
		{Name: "1080p30", URL: "https://example.com/1080p30.m3u8", Bandwidth: 5000000,
			Width: 1920, Height: 1080, FPS: 30, VideoCodec: "avc1"},
	}
}

// TestTwitchCaptureStartHonoursPrefer60fps: the selection a live or VOD
// capture starts on (processTwitchLive / processTwitchVod →
// selectTwitchVariant) reads prefer_60fps from the config, as it reads
// max_video_resolution beside it.
//
// Mutants: selectTwitchVariant passing true instead of the config value (the
// off case returns the 60 fps source); passing false (the on case returns the
// transcode).
func TestTwitchCaptureStartHonoursPrefer60fps(t *testing.T) {
	for _, tc := range []struct {
		prefer60fps bool
		want        string
	}{
		{false, "1080p30"},
		{true, "chunked"},
	} {
		cfg := &config.MoomboxConfig{}
		cfg.Downloader.Prefer60fps = tc.prefer60fps
		sp := &StreamProcessor{cfg: cfg}
		got := sp.selectTwitchVariant(sixtyBesideThirty(), &database.Job{ID: "tw_1", TwitchQualityPreference: "best"})
		if got == nil || got.Name != tc.want {
			t.Errorf("prefer_60fps=%v: capture start selected %v, want %s", tc.prefer60fps, got, tc.want)
		}
	}
}

// TestTwitchReselectionCarriesPrefer60fps: every re-selection during a
// capture — the 30 s quality probe and refreshBestVariant, both through
// TwitchVariantInfo.selectFrom — uses the prefer_60fps the job context
// snapshotted, which processJob copies across in newTwitchVariantInfo. A
// re-selection that dropped it would flip a 30 fps capture to the 60 fps source
// at the first probe, as a "quality change" with a part split.
//
// Mutants: newTwitchVariantInfo leaving Prefer60fps unset (the probe returns
// 60); selectFrom passing true instead of v.Prefer60fps (the same);
// newTwitchVariantInfo leaving MaxResolution unset (the 720 cap is lost and the
// probe returns 1080).
func TestTwitchReselectionCarriesPrefer60fps(t *testing.T) {
	job := &database.Job{ID: "tw_1", TwitchQualityPreference: "best"}
	variant := newTwitchVariantInfo(job, &sixtyBesideThirty()[1], &JobConfig{Prefer60fps: false})
	variant.FetchVariantsFn = func(context.Context) ([]twitch.TwitchHLSVariant, error) {
		return sixtyBesideThirty(), nil
	}

	if got := variant.selectFrom(sixtyBesideThirty()); got == nil || got.Name != "1080p30" {
		t.Errorf("selectFrom with prefer_60fps off selected %v, want 1080p30", got)
	}
	o := &DownloadOrchestrator{logger: nopWorkerLogger{}}
	qi, err := o.buildTwitchProbeFn(variant)(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if qi.FPS != 30 {
		t.Errorf("the quality probe reports %s, want the 30 fps rendition the capture started on", qi.Label)
	}

	// The cap rides along the same way.
	capped := newTwitchVariantInfo(job, &sixtyBesideThirty()[1], &JobConfig{MaxVideoResolution: 720, Prefer60fps: true})
	withLower := append(sixtyBesideThirty(), twitch.TwitchHLSVariant{Name: "720p60",
		Bandwidth: 3000000, Width: 1280, Height: 720, FPS: 60, VideoCodec: "avc1"})
	if got := capped.selectFrom(withLower); got == nil || got.Name != "720p60" {
		t.Errorf("selectFrom under a 720 cap selected %v, want 720p60", got)
	}
}
