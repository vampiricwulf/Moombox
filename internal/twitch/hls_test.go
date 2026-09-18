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
	// maxResolution checks max(width, height), so set cap to 1280 to allow 720p (1280x720)
	variants := []TwitchHLSVariant{
		{Name: "chunked", Bandwidth: 6000000, Width: 1920, Height: 1080, IsSource: true},
		{Name: "720p30", Bandwidth: 3000000, Width: 1280, Height: 720},
		{Name: "480p30", Bandwidth: 1500000, Width: 854, Height: 480},
	}

	result := SelectBestVariant(variants, "best", 1280)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Width > 1280 {
		t.Errorf("expected variant within 1280 cap, got %dx%d", result.Width, result.Height)
	}
	if result.Name != "720p30" {
		t.Errorf("expected 720p30, got %q", result.Name)
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
// default does to the opt-in. The resolution cap (SelectBestVariant step 3)
// runs BEFORE the codec-aware source step and compares the LONG edge, so under
// downloader.max_video_resolution's default of 2160 a 2560x1440 enhanced
// source is dropped from the candidate list before selectSourceVariant ever
// sees it and the H.264 1080p source is archived. An operator must raise the
// key to >= 2560 (1440p) or >= 3840 (4K) to receive an enhanced rendition.
//
// Mutants: applying the cap AFTER the source step (2160 then returns the 1440p
// HEVC source); comparing v.Height instead of max(v.Height, v.Width) (1440 <=
// 2160, so the HEVC source survives the cap and wins at the default).
func TestSelectBestVariantEnhancedSourceUnderTheDefaultCap(t *testing.T) {
	variants := ParseHLSMasterPlaylist(enhancedMasterPlaylist)

	got := SelectBestVariant(variants, "best", 2160)
	if got == nil {
		t.Fatal("SelectBestVariant returned nil at the default cap")
	}
	if got.VideoCodec != "avc1" || got.Width != 1920 || got.Height != 1080 {
		t.Errorf("at max_video_resolution=2160 selected %dx%d (%s), want the 1920x1080 avc1 "+
			"source — the enhanced source's 2560 LONG EDGE is over the default cap, and the "+
			"cap runs before the codec step", got.Width, got.Height, got.VideoCodec)
	}

	got = SelectBestVariant(variants, "best", 2560)
	if got == nil {
		t.Fatal("SelectBestVariant returned nil at a 2560 cap")
	}
	if got.VideoCodec != "hevc" || got.Width != 2560 || got.Height != 1440 {
		t.Errorf("at max_video_resolution=2560 selected %dx%d (%s), want the 2560x1440 hevc "+
			"source — raising the cap to the long edge is what admits an enhanced rendition",
			got.Width, got.Height, got.VideoCodec)
	}
}

// TestSelectBestVariantIsUnchangedWithoutCodecs is the byte-identity pin: a
// pre-enhanced playlist lists only H.264 renditions, so every source ties at
// `avc1` and the `> codecRank("avc1")` guard keeps playlist order — the
// selection is exactly what it was before this task. The CODECS-less fixture
// below is the degenerate end of the same rule (every source ties at the
// absent family); the second one is the shape Twitch actually serves.
//
// Mutant: ranking an absent codec family above avc1, or letting pixel area
// reorder equal-rank sources — either one changes the answer here.
func TestSelectBestVariantIsUnchangedWithoutCodecs(t *testing.T) {
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
		t.Errorf("selected %q, want the FIRST source variant in playlist order — with no "+
			"CODECS attribute nothing may reorder the candidates", got.URL)
	}

	// Same rule, a shape the fixture above cannot see: here the LATER source
	// carries both the larger frame and the higher bandwidth. Above, the
	// incumbent is also the largest frame, so a mutant that ranks the absent
	// family above avc1 reaches the pixel-area tie-break and still picks the
	// incumbent — it survives. This shape kills it, and kills dropping the
	// `cand > codecRank("avc1")` guard with it.
	const legacyLargerLater = `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=6000000,RESOLUTION=1280x720,FRAME-RATE=60.000,VIDEO="chunked"
https://example.com/chunked-first.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=9000000,RESOLUTION=1920x1080,FRAME-RATE=60.000,VIDEO="chunked"
https://example.com/chunked-larger.m3u8`
	got = SelectBestVariant(ParseHLSMasterPlaylist(legacyLargerLater), "best", 0)
	if got == nil {
		t.Fatal("SelectBestVariant returned nil for the larger-later playlist")
	}
	if got.URL != "https://example.com/chunked-first.m3u8" {
		t.Errorf("selected %q, want the FIRST source variant in playlist order — with no "+
			"CODECS attribute neither a larger frame nor a higher bandwidth may displace it",
			got.URL)
	}

	// The REAL pre-enhanced shape: Twitch usher playlists carry CODECS today
	// (internal/engine/manifest_test.go's Twitch fixture, and the live gate's
	// avc1 x5), so byte identity rests on the avc1 TIE, not on an absent
	// family. Here both sources are avc1 and the LATER one carries both the
	// larger frame and the higher bandwidth; only the `cand > codecRank("avc1")`
	// guard keeps the incumbent.
	//
	// Mutant: dropping that guard from selectSourceVariant's tie-break — two
	// avc1 sources then reorder by pixel area and the larger, later one wins.
	const legacyWithCodecs = `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=6000000,RESOLUTION=1280x720,CODECS="avc1.4D401F,mp4a.40.2",FRAME-RATE=60.000,VIDEO="chunked"
https://example.com/avc1-first.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=9000000,RESOLUTION=1920x1080,CODECS="avc1.64002A,mp4a.40.2",FRAME-RATE=60.000,VIDEO="chunked"
https://example.com/avc1-larger.m3u8`
	got = SelectBestVariant(ParseHLSMasterPlaylist(legacyWithCodecs), "best", 0)
	if got == nil {
		t.Fatal("SelectBestVariant returned nil for the two-avc1 playlist")
	}
	if got.URL != "https://example.com/avc1-first.m3u8" {
		t.Errorf("selected %q, want the FIRST source variant in playlist order — two H.264 "+
			"sources tie at avc1, and neither a larger frame nor a higher bandwidth may "+
			"displace the incumbent at that rank", got.URL)
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
