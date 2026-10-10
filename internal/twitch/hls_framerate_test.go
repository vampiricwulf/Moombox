package twitch

import "testing"

// sixtyBesideThirtyPlaylist is the common Twitch shape at one size: the
// broadcaster's 1080p60 source beside a 1080p30 transcode, both H.264, the
// source listed first and carrying the higher bandwidth — so neither playlist
// order nor bandwidth nor the source flag can pick the 30 fps rendition.
const sixtyBesideThirtyPlaylist = `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=8000000,RESOLUTION=1920x1080,CODECS="avc1.64002A,mp4a.40.2",FRAME-RATE=60.000,VIDEO="chunked"
https://example.com/chunked.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080,CODECS="avc1.64002A,mp4a.40.2",FRAME-RATE=30.000,VIDEO="1080p30"
https://example.com/1080p30.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1280x720,CODECS="avc1.4D401F,mp4a.40.2",FRAME-RATE=30.000,VIDEO="720p30"
https://example.com/720p30.m3u8`

// TestSelectBestVariantFrameRateFollowsPrefer60fps is D-Y2: at the chosen size
// the order is codec, then the frame rate prefer_60fps asks for, then the
// source flag, then bandwidth. Twitch ignored the setting outright, so with it
// off the 60 fps source still won every time.
//
// Block 1 (prefer60fps off) pins the rung and its place ABOVE the source flag
// and bandwidth: only the frame rate can reach the 1080p30 transcode.
// Mutants: the frame-rate rung deleted from rankAtChosenSize (the source
// comes back); the rung moved below the source flag (the source comes back);
// SelectBestVariant passing true to rankAtChosenSize instead of prefer60fps
// (the source comes back).
//
// Block 2 (prefer60fps on) pins the direction: the 60 fps rendition wins even
// when it is NOT the source and is OUTBID by a 30 fps source.
// Mutants: preferredFrameRate's on-arm inverted to fps < 50 (the 30 fps source
// comes back); the rung moved below the source flag (the same).
//
// Block 3 pins the rung BELOW the codec: an HEVC 30 fps rendition still beats
// an H.264 60 fps one with prefer60fps on.
// Mutant: the frame-rate rung moved above the codec rung.
//
// Block 4 pins the NTSC rate and the unknown rate: 29.97 counts as 30-ish, and
// a rendition with no FRAME-RATE is preferred under neither setting.
// Mutant: the off-arm's fps > 0 guard dropped (the unknown-rate source,
// listed first and outbidding, comes back).
func TestSelectBestVariantFrameRateFollowsPrefer60fps(t *testing.T) {
	variants := ParseHLSMasterPlaylist(sixtyBesideThirtyPlaylist)

	got := SelectBestVariant(variants, "best", 0, false)
	if got == nil || got.URL != "https://example.com/1080p30.m3u8" {
		t.Errorf("prefer_60fps off selected %v, want the 1080p30 transcode — the frame rate "+
			"outranks the source flag and the bandwidth at the chosen size", got)
	}
	got = SelectBestVariant(variants, "best", 0, true)
	if got == nil || got.URL != "https://example.com/chunked.m3u8" {
		t.Errorf("prefer_60fps on selected %v, want the 1080p60 source", got)
	}

	got = SelectBestVariant([]TwitchHLSVariant{
		{Name: "chunked", Bandwidth: 9000000, Width: 1920, Height: 1080, FPS: 30, VideoCodec: "avc1", IsSource: true},
		{Name: "1080p60", Bandwidth: 6000000, Width: 1920, Height: 1080, FPS: 59.94, VideoCodec: "avc1"},
	}, "best", 0, true)
	if got == nil || got.Name != "1080p60" {
		t.Errorf("prefer_60fps on selected %v, want 1080p60 — the 59.94 fps transcode outranks a "+
			"30 fps source however it is bid", got)
	}

	got = SelectBestVariant([]TwitchHLSVariant{
		{Name: "chunked", Bandwidth: 9000000, Width: 1920, Height: 1080, FPS: 60, VideoCodec: "avc1", IsSource: true},
		{Name: "1080p30-hevc", Bandwidth: 4000000, Width: 1920, Height: 1080, FPS: 30, VideoCodec: "hevc"},
	}, "best", 0, true)
	if got == nil || got.Name != "1080p30-hevc" {
		t.Errorf("selected %v, want 1080p30-hevc — the codec still outranks the frame rate", got)
	}

	got = SelectBestVariant([]TwitchHLSVariant{
		{Name: "chunked", Bandwidth: 9000000, Width: 1920, Height: 1080, VideoCodec: "avc1", IsSource: true},
		{Name: "1080p30", Bandwidth: 5000000, Width: 1920, Height: 1080, FPS: 29.97, VideoCodec: "avc1"},
	}, "best", 0, false)
	if got == nil || got.Name != "1080p30" {
		t.Errorf("prefer_60fps off selected %v, want 1080p30 — 29.97 fps is the 30 fps the "+
			"setting asks for, and an unknown rate is not", got)
	}
}

// TestPreferredFrameRate pins the thresholds against the YouTube selectors'
// fpsPreference (internal/worker/format_utils.go), which they mirror.
//
// Mutants: the on-arm threshold moved to >= 60 (59.94 fails); the off-arm's
// fps > 0 guard dropped (an unknown rate reads as 30-ish).
func TestPreferredFrameRate(t *testing.T) {
	for _, tc := range []struct {
		fps         float64
		prefer60fps bool
		want        bool
	}{
		{60, true, true},
		{59.94, true, true},
		{50, true, true},
		{30, true, false},
		{0, true, false},
		{30, false, true},
		{29.97, false, true},
		{31, false, true},
		{60, false, false},
		{0, false, false},
	} {
		if got := preferredFrameRate(tc.fps, tc.prefer60fps); got != tc.want {
			t.Errorf("preferredFrameRate(%g, %v) = %v, want %v", tc.fps, tc.prefer60fps, got, tc.want)
		}
	}
}
