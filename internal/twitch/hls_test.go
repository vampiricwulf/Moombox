package twitch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestFetchHLSMasterPlaylistSubscriberOnly pins the usher 403 classification:
// a restriction body carrying error_code vod_manifest_restricted /
// unauthorized_entitlements (yt-dlp twitch.py parity) means subscriber-only
// content — the caller needs the ErrSubscriberOnly sentinel to route the job
// to COOKIES? with a "log into an account that has access" message instead
// of a generic Error with a raw JSON body. Anything else stays a plain error.
func TestFetchHLSMasterPlaylistSubscriberOnly(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool // errors.Is(err, ErrSubscriberOnly)
	}{
		{"unauthorized_entitlements", `[{"url":"","error":"Content is not available on this channel.","error_code":"unauthorized_entitlements","type":"error"}]`, true},
		{"vod_manifest_restricted", `[{"url":"","error":"restricted","error_code":"vod_manifest_restricted","type":"error"}]`, true},
		{"other error_code stays generic", `[{"url":"","error":"geoblocked","error_code":"geoblock","type":"error"}]`, false},
		{"non-JSON body stays generic", `Forbidden`, false},
		{"empty body stays generic", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			_, err := FetchHLSMasterPlaylist(context.Background(), srv.URL)
			if err == nil {
				t.Fatal("want an error for HTTP 403, got nil")
			}
			if got := errors.Is(err, ErrSubscriberOnly); got != tc.want {
				t.Errorf("errors.Is(err, ErrSubscriberOnly) = %v, want %v (err = %v)", got, tc.want, err)
			}
		})
	}
}

func TestParseHLSMasterPlaylistBasic(t *testing.T) {
	content := `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=6000000,RESOLUTION=1920x1080,FRAME-RATE=60.000,VIDEO="chunked"
https://example.com/chunked.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1280x720,FRAME-RATE=30.000,VIDEO="720p30"
https://example.com/720p30.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=160000,VIDEO="audio_only"
https://example.com/audio_only.m3u8`

	variants := ParseHLSMasterPlaylist(content)

	if len(variants) != 3 {
		t.Fatalf("expected 3 variants, got %d", len(variants))
	}

	// Source variant
	if variants[0].Bandwidth != 6000000 {
		t.Errorf("variant[0] bandwidth = %d, want 6000000", variants[0].Bandwidth)
	}
	if variants[0].Width != 1920 || variants[0].Height != 1080 {
		t.Errorf("variant[0] resolution = %dx%d, want 1920x1080", variants[0].Width, variants[0].Height)
	}
	if variants[0].FPS != 60.0 {
		t.Errorf("variant[0] FPS = %f, want 60.0", variants[0].FPS)
	}
	if !variants[0].IsSource {
		t.Error("variant[0] should be source")
	}
	if variants[0].Name != "chunked" {
		t.Errorf("variant[0] name = %q, want %q", variants[0].Name, "chunked")
	}

	// 720p variant
	if variants[1].Bandwidth != 3000000 {
		t.Errorf("variant[1] bandwidth = %d, want 3000000", variants[1].Bandwidth)
	}
	if variants[1].Height != 720 {
		t.Errorf("variant[1] height = %d, want 720", variants[1].Height)
	}
	if variants[1].IsSource {
		t.Error("variant[1] should not be source")
	}

	// Audio-only variant
	if variants[2].VideoGroup != "audio_only" {
		t.Errorf("variant[2] video group = %q, want %q", variants[2].VideoGroup, "audio_only")
	}
}

func TestParseHLSMasterPlaylistEmpty(t *testing.T) {
	variants := ParseHLSMasterPlaylist("")
	if len(variants) != 0 {
		t.Fatalf("expected 0 variants for empty content, got %d", len(variants))
	}
}

func TestParseHLSMasterPlaylistNoURL(t *testing.T) {
	// STREAM-INF without a following URL should be skipped
	content := `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=6000000,RESOLUTION=1920x1080,VIDEO="chunked"
`
	variants := ParseHLSMasterPlaylist(content)
	if len(variants) != 0 {
		t.Fatalf("expected 0 variants when URL is missing, got %d", len(variants))
	}
}

func TestParseHLSMasterPlaylistMissingAttributes(t *testing.T) {
	// No resolution, no frame rate, no video group — only bandwidth and URL
	content := `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=160000
https://example.com/audio.m3u8`

	variants := ParseHLSMasterPlaylist(content)
	if len(variants) != 1 {
		t.Fatalf("expected 1 variant, got %d", len(variants))
	}
	if variants[0].Bandwidth != 160000 {
		t.Errorf("bandwidth = %d, want 160000", variants[0].Bandwidth)
	}
	if variants[0].Width != 0 || variants[0].Height != 0 {
		t.Error("expected 0x0 resolution for audio-only-like variant")
	}
	if variants[0].Name != "unknown" {
		t.Errorf("name = %q, want %q", variants[0].Name, "unknown")
	}
}

func TestParseHLSMasterPlaylistNameFallbacks(t *testing.T) {
	// No video group but has resolution — name should be derived from height+fps
	content := `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1280x720,FRAME-RATE=30.000
https://example.com/720p30.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=6000000,RESOLUTION=1920x1080,FRAME-RATE=60.000
https://example.com/1080p60.m3u8`

	variants := ParseHLSMasterPlaylist(content)
	if len(variants) != 2 {
		t.Fatalf("expected 2 variants, got %d", len(variants))
	}
	if variants[0].Name != "720p30" {
		t.Errorf("variant[0] name = %q, want %q", variants[0].Name, "720p30")
	}
	if variants[1].Name != "1080p60" {
		t.Errorf("variant[1] name = %q, want %q", variants[1].Name, "1080p60")
	}
}

func TestParseHLSMasterPlaylistBlankLinesBetween(t *testing.T) {
	// Test that blank lines between STREAM-INF and URL are handled
	content := `#EXTM3U

#EXT-X-STREAM-INF:BANDWIDTH=6000000,RESOLUTION=1920x1080,VIDEO="chunked"

https://example.com/chunked.m3u8`

	variants := ParseHLSMasterPlaylist(content)
	if len(variants) != 1 {
		t.Fatalf("expected 1 variant, got %d", len(variants))
	}
	if variants[0].URL != "https://example.com/chunked.m3u8" {
		t.Errorf("URL = %q", variants[0].URL)
	}
}

func TestSelectBestVariantEmpty(t *testing.T) {
	result := SelectBestVariant(nil, "best", 0)
	if result != nil {
		t.Error("expected nil for empty variants")
	}
}

func TestSelectBestVariantSource(t *testing.T) {
	variants := []TwitchHLSVariant{
		{Name: "720p30", Bandwidth: 3000000, Height: 720, FPS: 30},
		{Name: "chunked", Bandwidth: 6000000, Height: 1080, FPS: 60, IsSource: true},
		{Name: "audio_only", Bandwidth: 160000},
	}

	result := SelectBestVariant(variants, "best", 0)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Name != "chunked" {
		t.Errorf("expected source variant 'chunked', got %q", result.Name)
	}
}

func TestSelectBestVariantAudioOnly(t *testing.T) {
	variants := []TwitchHLSVariant{
		{Name: "chunked", Bandwidth: 6000000, Height: 1080, IsSource: true},
		{Name: "audio_only", Bandwidth: 160000},
	}

	result := SelectBestVariant(variants, "audio_only", 0)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Name != "audio_only" {
		t.Errorf("expected audio_only, got %q", result.Name)
	}
}

func TestSelectBestVariantHeightPref(t *testing.T) {
	variants := []TwitchHLSVariant{
		{Name: "chunked", Bandwidth: 6000000, Width: 1920, Height: 1080, FPS: 60, IsSource: true},
		{Name: "720p60", Bandwidth: 3000000, Width: 1280, Height: 720, FPS: 60},
		{Name: "720p30", Bandwidth: 2500000, Width: 1280, Height: 720, FPS: 30},
		{Name: "480p30", Bandwidth: 1500000, Width: 854, Height: 480, FPS: 30},
	}

	result := SelectBestVariant(variants, "720p60", 0)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Name != "720p60" {
		t.Errorf("expected 720p60, got %q", result.Name)
	}
}

func TestSelectBestVariantMaxResolution(t *testing.T) {
	// The cap compares the SHORT edge (R1), so a 1280 cap admits the
	// 1920x1080 source: its short edge is 1080. Before this arc the same
	// playlist returned 720p30, because max(1920, 1080) exceeded 1280.
	//
	// Mutant: long edge restored — 720p30 comes back.
	variants := []TwitchHLSVariant{
		{Name: "chunked", Bandwidth: 6000000, Width: 1920, Height: 1080, IsSource: true},
		{Name: "720p30", Bandwidth: 3000000, Width: 1280, Height: 720},
		{Name: "480p30", Bandwidth: 1500000, Width: 854, Height: 480},
	}

	result := SelectBestVariant(variants, "best", 1280)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Name != "chunked" {
		t.Errorf("expected chunked (1920x1080, short edge 1080 <= 1280), got %q", result.Name)
	}

	// And a cap the source really is over: 720 admits only the 720p and 480p
	// transcodes, and the largest at or below wins.
	result = SelectBestVariant(variants, "best", 720)
	if result == nil {
		t.Fatal("expected non-nil result at a 720 cap")
	}
	if result.Name != "720p30" {
		t.Errorf("expected 720p30 at a 720 cap, got %q", result.Name)
	}
}

// TestSelectBestVariantBelowEveryRendition is the fallthrough R1 deletes. The
// pre-arc code kept the UNFILTERED list when the cap matched nothing, so the
// answer depended on the source step rather than on the cap — here it happened
// to be the 1080p source. R1 makes it the CLOSEST rendition above instead.
//
// Mutant: the unfiltered fallthrough restored — chunked (1080p) comes back.
// Mutant: closest-above replaced by largest — chunked comes back too.
func TestSelectBestVariantBelowEveryRendition(t *testing.T) {
	variants := []TwitchHLSVariant{
		{Name: "chunked", Bandwidth: 6000000, Width: 1920, Height: 1080, IsSource: true},
		{Name: "720p30", Bandwidth: 3000000, Width: 1280, Height: 720},
	}
	got := SelectBestVariant(variants, "best", 360)
	if got == nil {
		t.Fatal("expected non-nil result below every rendition")
	}
	if got.Name != "720p30" {
		t.Errorf("selected %q at a 360 cap, want 720p30 — the CLOSEST rendition above the cap", got.Name)
	}
}

// TestSelectBestVariantFourKUnderTheDefaultCap is spec §3's per-site matrix at
// the Twitch site: a 3840x2160 source is 2160 and the shipped default admits
// it. Before this arc its 3840 LONG edge was over the cap and it was dropped —
// the transcode was archived instead, and only the over-cap fallthrough (which
// this arc also deletes) ever handed back anything larger.
//
// Mutants: long edge restored (720p60 comes back — 3840 > 2160 drops the
// source while the 1280-long-edge transcode survives); closest-above replaced
// by largest (unchanged here, so pair with TestSelectBestVariantBelowEveryRendition).
func TestSelectBestVariantFourKUnderTheDefaultCap(t *testing.T) {
	variants := []TwitchHLSVariant{
		{Name: "chunked", Bandwidth: 25000000, Width: 3840, Height: 2160, VideoCodec: "av01", IsSource: true},
		{Name: "720p60", Bandwidth: 3000000, Width: 1280, Height: 720, VideoCodec: "avc1"},
	}
	got := SelectBestVariant(variants, "best", 2160)
	if got == nil {
		t.Fatal("expected non-nil result for a 3840x2160 source at cap 2160")
	}
	if got.Name != "chunked" || got.Height != 2160 {
		t.Errorf("selected %q (%dx%d) at max_video_resolution=2160, want the 3840x2160 source — "+
			"its SHORT edge is 2160", got.Name, got.Width, got.Height)
	}
}

// TestSelectBestVariantPortraitUnderTheDefaultCap: the same frame the other way
// round reads the same — a 2160x3840 portrait source is 2160, not 3840.
//
// Mutant: long edge restored — 3840 > 2160 drops the portrait source while the
// 720x1280 transcode (long edge 1280) survives, and 720p60 comes back. The
// single-variant shape this test first had was GREEN before the change (the old
// over-cap fallthrough handed the unfiltered list straight back), which is why
// the transcode is here: without it the test pins nothing.
func TestSelectBestVariantPortraitUnderTheDefaultCap(t *testing.T) {
	got := SelectBestVariant([]TwitchHLSVariant{
		{Name: "chunked", Bandwidth: 18000000, Width: 2160, Height: 3840, VideoCodec: "avc1", IsSource: true},
		{Name: "720p60", Bandwidth: 3000000, Width: 720, Height: 1280, VideoCodec: "avc1"},
	}, "best", 2160)
	if got == nil {
		t.Fatal("expected non-nil result for a 2160x3840 portrait source at cap 2160")
	}
	if got.Name != "chunked" {
		t.Errorf("selected %q, want chunked — a 2160x3840 portrait source is 2160 on its short edge", got.Name)
	}
}

func TestSelectBestVariantFallbackToLower(t *testing.T) {
	variants := []TwitchHLSVariant{
		{Name: "chunked", Bandwidth: 6000000, Width: 1920, Height: 1080, IsSource: true},
		{Name: "480p30", Bandwidth: 1500000, Width: 854, Height: 480},
	}

	// Request 720p but only 1080 and 480 available — should fall back to 480
	result := SelectBestVariant(variants, "720p", 0)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Height != 480 {
		t.Errorf("expected fallback to 480p, got %dp", result.Height)
	}
}

func TestParseQualityPref(t *testing.T) {
	tests := []struct {
		input      string
		wantHeight int
		wantFPS    int
	}{
		{"1080p60", 1080, 60},
		{"720p", 720, 0},
		{"480p30", 480, 30},
		{"best", 0, 0},
		{"", 0, 0},
	}
	for _, tt := range tests {
		h, f := parseQualityPref(tt.input)
		if h != tt.wantHeight || f != tt.wantFPS {
			t.Errorf("parseQualityPref(%q) = (%d, %d), want (%d, %d)", tt.input, h, f, tt.wantHeight, tt.wantFPS)
		}
	}
}

// enhancedMasterPlaylist is the shape Twitch serves once the usher request
// carries platform=web and supported_codecs=av1,h265,h264: the source group
// gains an HEVC rendition above the H.264 one, and the transcodes stay H.264.
const enhancedMasterPlaylist = `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=16000000,RESOLUTION=2560x1440,CODECS="hvc1.2.4.L150.90,mp4a.40.2",FRAME-RATE=60.000,VIDEO="chunked"
https://example.com/chunked-hevc.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=8000000,RESOLUTION=1920x1080,CODECS="avc1.64002A,mp4a.40.2",FRAME-RATE=60.000,VIDEO="chunked"
https://example.com/chunked-h264.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1280x720,CODECS="avc1.4D401F,mp4a.40.2",FRAME-RATE=30.000,VIDEO="720p30"
https://example.com/720p30.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=160000,CODECS="mp4a.40.2",VIDEO="audio_only"
https://example.com/audio_only.m3u8`

// TestParseHLSMasterPlaylistReadsCodecs pins the new attribute. The parser
// keeps the raw CODECS list AND a normalized video family, because the raw list
// is an ordered comma string whose video entry is not always first.
//
// Mutants: not parsing CODECS at all (Codecs is empty); deriving the family
// from the FIRST entry unconditionally (the audio-only variant then reports a
// video codec, and an "mp4a,hvc1" ordering would report none).
func TestParseHLSMasterPlaylistReadsCodecs(t *testing.T) {
	variants := ParseHLSMasterPlaylist(enhancedMasterPlaylist)
	if len(variants) != 4 {
		t.Fatalf("parsed %d variants, want 4", len(variants))
	}
	for i, want := range []struct{ codecs, family string }{
		{"hvc1.2.4.L150.90,mp4a.40.2", "hevc"},
		{"avc1.64002A,mp4a.40.2", "avc1"},
		{"avc1.4D401F,mp4a.40.2", "avc1"},
		{"mp4a.40.2", ""},
	} {
		if variants[i].Codecs != want.codecs {
			t.Errorf("variant[%d].Codecs = %q, want %q", i, variants[i].Codecs, want.codecs)
		}
		if variants[i].VideoCodec != want.family {
			t.Errorf("variant[%d].VideoCodec = %q, want %q", i, variants[i].VideoCodec, want.family)
		}
	}
}

// TestVideoCodecFamily covers the families Twitch can offer plus the shapes
// that must NOT be read as video.
//
// Mutant: matching "av1"/"h265" (the usher REQUEST spelling) instead of the
// RFC 6381 codec ids "av01"/"hev1"/"hvc1" that appear in a playlist.
func TestVideoCodecFamily(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"av01.0.08M.10,mp4a.40.2", "av01"},
		{"hvc1.2.4.L150.90", "hevc"},
		{"hev1.2.4.L150.90", "hevc"},
		{"avc1.64002A", "avc1"},
		{"avc3.64002A", "avc1"},
		{"mp4a.40.2", ""},
		{"", ""},
		{"  AV01.0.08M.10 , mp4a.40.2 ", "av01"},
	} {
		if got := videoCodecFamily(tc.in); got != tc.want {
			t.Errorf("videoCodecFamily(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSelectBestVariantPrefersTheEnhancedSource is the owner's "codec-aware
// variant selection preferring the enhanced source". Both source renditions
// are VIDEO="chunked"; the old rule took the FIRST one in playlist order, which
// is fine today (there is only ever one) but would be a coin flip once Twitch
// offers two.
//
// Mutants: keeping the first-IsSource loop (the 1080p H.264 source is
// returned); ranking by bandwidth instead of codec (also the H.264 source on a
// playlist where the transcode outbids the AV1 source).
func TestSelectBestVariantPrefersTheEnhancedSource(t *testing.T) {
	variants := ParseHLSMasterPlaylist(enhancedMasterPlaylist)
	got := SelectBestVariant(variants, "best", 0)
	if got == nil {
		t.Fatal("SelectBestVariant returned nil")
	}
	if got.VideoCodec != "hevc" || got.Height != 1440 {
		t.Errorf("selected %s %dx%d (%s), want the 2560x1440 hevc source",
			got.Name, got.Width, got.Height, got.VideoCodec)
	}

	// The same choice with the enhanced source listed LAST and OUTBID. The
	// fixture above cannot see either named mutant on its own: its HEVC
	// source is already first in playlist order AND the highest bandwidth, so
	// both the pre-task first-IsSource loop and a bandwidth ranking pick it by
	// accident. Here only the codec rank reaches it.
	const enhancedSourceListedLast = `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=9000000,RESOLUTION=1920x1080,CODECS="avc1.64002A,mp4a.40.2",FRAME-RATE=60.000,VIDEO="chunked"
https://example.com/chunked-h264.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=7000000,RESOLUTION=2560x1440,CODECS="hvc1.2.4.L150.90,mp4a.40.2",FRAME-RATE=60.000,VIDEO="chunked"
https://example.com/chunked-hevc.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1280x720,CODECS="avc1.4D401F,mp4a.40.2",FRAME-RATE=30.000,VIDEO="720p30"
https://example.com/720p30.m3u8`
	got = SelectBestVariant(ParseHLSMasterPlaylist(enhancedSourceListedLast), "best", 0)
	if got == nil {
		t.Fatal("SelectBestVariant returned nil for the source-listed-last playlist")
	}
	if got.URL != "https://example.com/chunked-hevc.m3u8" {
		t.Errorf("selected %q, want the hevc source — neither playlist order nor bandwidth "+
			"may keep the H.264 source once a better family is offered", got.URL)
	}
}

// TestSelectBestVariantEnhancedSourceUnderTheDefaultCap is what the SHIPPED
// default does to the enhanced-broadcast opt-in. It used to defeat it: the cap
// compared the LONG edge, so a 2560x1440 enhanced source (long edge 2560) was
// dropped before the codec step and the H.264 transcode was archived, and an
// operator had to raise max_video_resolution to 2560 to get 1440p at all.
//
// Under ruling R1 the cap compares the SHORT edge, so 1440 <= 2160 and the
// enhanced source is admitted at the shipped default. Nobody has to raise the
// key for an enhanced rendition any more.
//
// Mutants: long edge restored (the 1920x1080 avc1 source comes back at 2160);
// the codec rung dropped from rankAtChosenSize (nothing at 1440 but the HEVC
// source, so pair this with TestSelectBestVariantPrefersTheEnhancedSource).
func TestSelectBestVariantEnhancedSourceUnderTheDefaultCap(t *testing.T) {
	variants := ParseHLSMasterPlaylist(enhancedMasterPlaylist)

	got := SelectBestVariant(variants, "best", 2160)
	if got == nil {
		t.Fatal("SelectBestVariant returned nil at the default cap")
	}
	if got.VideoCodec != "hevc" || got.Width != 2560 || got.Height != 1440 {
		t.Errorf("at max_video_resolution=2160 selected %dx%d (%s), want the 2560x1440 hevc "+
			"source — its SHORT edge is 1440, well under the default cap",
			got.Width, got.Height, got.VideoCodec)
	}

	// A cap that really is below the enhanced source: 1080 admits the H.264
	// source and the 720p transcode, and the largest at or below wins.
	got = SelectBestVariant(variants, "best", 1080)
	if got == nil {
		t.Fatal("SelectBestVariant returned nil at a 1080 cap")
	}
	if got.VideoCodec != "avc1" || got.Width != 1920 || got.Height != 1080 {
		t.Errorf("at max_video_resolution=1080 selected %dx%d (%s), want the 1920x1080 avc1 source",
			got.Width, got.Height, got.VideoCodec)
	}
}

// TestSelectBestVariantWithoutCodecsRanksBySizeThenBandwidth: with no CODECS
// attribute every rendition ties at the absent family, so the codec rung is
// inert and the SIZE rung decides — which it did not before this arc, when the
// first source in playlist order won regardless of frame size.
//
// Mutants: the size rung dropped (the first source in playlist order comes back
// in cases 2 and 3); the final bandwidth rung made >= instead of > (case 4
// returns the second of two identical renditions).
func TestSelectBestVariantWithoutCodecsRanksBySizeThenBandwidth(t *testing.T) {
	// 1. The largest short edge wins; the 1280x720 source and the 720p30
	//    transcode are both below it.
	const legacy = `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=6000000,RESOLUTION=1920x1080,FRAME-RATE=60.000,VIDEO="chunked"
https://example.com/chunked.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=9000000,RESOLUTION=1280x720,FRAME-RATE=30.000,VIDEO="chunked"
https://example.com/chunked-second.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1280x720,FRAME-RATE=30.000,VIDEO="720p30"
https://example.com/720p30.m3u8`
	got := SelectBestVariant(ParseHLSMasterPlaylist(legacy), "best", 0)
	if got == nil {
		t.Fatal("SelectBestVariant returned nil")
	}
	if got.URL != "https://example.com/chunked.m3u8" {
		t.Errorf("selected %q, want the 1920x1080 source — the largest short edge wins", got.URL)
	}

	// 2. The larger rendition is listed LAST and it still wins: playlist order
	//    no longer outranks frame size.
	const legacyLargerLater = `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=6000000,RESOLUTION=1280x720,FRAME-RATE=60.000,VIDEO="chunked"
https://example.com/chunked-first.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=9000000,RESOLUTION=1920x1080,FRAME-RATE=60.000,VIDEO="chunked"
https://example.com/chunked-larger.m3u8`
	got = SelectBestVariant(ParseHLSMasterPlaylist(legacyLargerLater), "best", 0)
	if got == nil {
		t.Fatal("SelectBestVariant returned nil for the larger-later playlist")
	}
	if got.URL != "https://example.com/chunked-larger.m3u8" {
		t.Errorf("selected %q, want the 1920x1080 source — with no cap the largest short edge "+
			"wins, and playlist order is now the LAST rung, not the first", got.URL)
	}

	// 3. The real pre-enhanced shape: Twitch usher playlists carry CODECS, and
	//    two H.264 sources tie at avc1, so again the size rung decides.
	const legacyWithCodecs = `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=6000000,RESOLUTION=1280x720,CODECS="avc1.4D401F,mp4a.40.2",FRAME-RATE=60.000,VIDEO="chunked"
https://example.com/avc1-first.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=9000000,RESOLUTION=1920x1080,CODECS="avc1.64002A,mp4a.40.2",FRAME-RATE=60.000,VIDEO="chunked"
https://example.com/avc1-larger.m3u8`
	got = SelectBestVariant(ParseHLSMasterPlaylist(legacyWithCodecs), "best", 0)
	if got == nil {
		t.Fatal("SelectBestVariant returned nil for the two-avc1 playlist")
	}
	if got.URL != "https://example.com/avc1-larger.m3u8" {
		t.Errorf("selected %q, want the 1920x1080 avc1 source — the two sources tie at avc1, "+
			"so the size rung decides", got.URL)
	}

	// 4. Everything ties: same size, same (absent) codec, same source flag,
	//    same bandwidth. The incumbent — playlist order — is what is left.
	const identical = `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=6000000,RESOLUTION=1920x1080,FRAME-RATE=60.000,VIDEO="chunked"
https://example.com/first.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=6000000,RESOLUTION=1920x1080,FRAME-RATE=60.000,VIDEO="chunked"
https://example.com/second.m3u8`
	got = SelectBestVariant(ParseHLSMasterPlaylist(identical), "best", 0)
	if got == nil {
		t.Fatal("SelectBestVariant returned nil for the identical-renditions playlist")
	}
	if got.URL != "https://example.com/first.m3u8" {
		t.Errorf("selected %q, want the FIRST — two indistinguishable renditions keep playlist order", got.URL)
	}
}

// TestSelectBestVariantCodecRankAtTheChosenSize is ruling R2: at the chosen
// size the order is AV1 > HEVC > H.264, ahead of the source flag and ahead of
// bandwidth. The AV1 rendition below is NOT flagged source and carries the
// LOWEST bandwidth, so only the codec rung can reach it.
//
// Mutants: codec order reversed (the avc1 source comes back); the source flag
// ranked above the codec (the avc1 source comes back); bandwidth ranked above
// the codec (the hevc source comes back).
func TestSelectBestVariantCodecRankAtTheChosenSize(t *testing.T) {
	variants := []TwitchHLSVariant{
		{Name: "chunked", Bandwidth: 9000000, Width: 1920, Height: 1080, VideoCodec: "avc1", IsSource: true},
		{Name: "1080p60-hevc", Bandwidth: 8000000, Width: 1920, Height: 1080, VideoCodec: "hevc"},
		{Name: "1080p60-av1", Bandwidth: 6000000, Width: 1920, Height: 1080, VideoCodec: "av01"},
		{Name: "720p60", Bandwidth: 3000000, Width: 1280, Height: 720, VideoCodec: "av01"},
	}
	got := SelectBestVariant(variants, "best", 0)
	if got == nil {
		t.Fatal("SelectBestVariant returned nil")
	}
	if got.Name != "1080p60-av1" {
		t.Errorf("selected %q, want 1080p60-av1 — AV1 outranks HEVC and H.264 at the chosen size, "+
			"ahead of the source flag and of bandwidth", got.Name)
	}

	// Drop AV1 and HEVC wins; drop HEVC too and the H.264 SOURCE wins over the
	// H.264 transcode at the same size.
	got = SelectBestVariant(variants[:2], "best", 0)
	if got == nil || got.Name != "1080p60-hevc" {
		t.Errorf("selected %v, want 1080p60-hevc once AV1 is gone", got)
	}
	got = SelectBestVariant([]TwitchHLSVariant{
		{Name: "1080p60", Bandwidth: 9000000, Width: 1920, Height: 1080, VideoCodec: "avc1"},
		{Name: "chunked", Bandwidth: 6000000, Width: 1920, Height: 1080, VideoCodec: "avc1", IsSource: true},
	}, "best", 0)
	if got == nil || got.Name != "chunked" {
		t.Errorf("selected %v, want chunked — at equal codec the SOURCE outranks the transcode "+
			"and outranks bandwidth", got)
	}
}

// TestSelectBestVariantHonoursAnExplicitHeightOverTheEnhancedSource: an
// operator who asked for 1080p60 gets 1080p60, enhanced source or not. The
// codec preference is the SOURCE step's tie-break, not an override of the
// quality preference.
//
// Mutant: applying the codec rank inside selectVariantByHeight — the 1440p
// HEVC source is returned for a 1080p60 request.
func TestSelectBestVariantHonoursAnExplicitHeightOverTheEnhancedSource(t *testing.T) {
	got := SelectBestVariant(ParseHLSMasterPlaylist(enhancedMasterPlaylist), "1080p60", 0)
	if got == nil {
		t.Fatal("SelectBestVariant returned nil")
	}
	if got.Height != 1080 {
		t.Errorf("selected %dx%d for quality pref 1080p60, want a 1080-high variant",
			got.Width, got.Height)
	}
}
