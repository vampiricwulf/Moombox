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
