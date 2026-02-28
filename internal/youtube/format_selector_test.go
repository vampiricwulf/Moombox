package youtube

import (
	"testing"
)

func intPtr(n int) *int { return &n }

func TestSelectBestFormats(t *testing.T) {
	formats := []Format{
		{Itag: 137, MimeType: "video/mp4; codecs=\"avc1.640028\"", Bitrate: 4000000, Width: intPtr(1920), Height: intPtr(1080), Fps: intPtr(30), URL: "https://example.com/v1"},
		{Itag: 298, MimeType: "video/mp4; codecs=\"avc1.640028\"", Bitrate: 4500000, Width: intPtr(1920), Height: intPtr(1080), Fps: intPtr(60), URL: "https://example.com/v2"},
		{Itag: 248, MimeType: "video/webm; codecs=\"vp9\"", Bitrate: 3500000, Width: intPtr(1920), Height: intPtr(1080), Fps: intPtr(30), URL: "https://example.com/v3"},
		{Itag: 136, MimeType: "video/mp4; codecs=\"avc1.4d401f\"", Bitrate: 2500000, Width: intPtr(1280), Height: intPtr(720), Fps: intPtr(30), URL: "https://example.com/v4"},
		{Itag: 140, MimeType: "audio/mp4; codecs=\"mp4a.40.2\"", Bitrate: 128000, AudioQuality: "AUDIO_QUALITY_MEDIUM", URL: "https://example.com/a1"},
		{Itag: 251, MimeType: "audio/webm; codecs=\"opus\"", Bitrate: 160000, AudioQuality: "AUDIO_QUALITY_MEDIUM", URL: "https://example.com/a2"},
	}

	// max_video_resolution=1920 allows up to 1080p (max dimension is 1920)
	result := SelectBestFormats(formats, 1920, true)

	if result.Video == nil {
		t.Fatal("expected video format")
	}
	// Should pick 60fps 1080p
	if result.Video.Itag != 298 {
		t.Errorf("expected itag 298 (1080p60), got %d", result.Video.Itag)
	}

	if result.Audio == nil {
		t.Fatal("expected audio format")
	}
	// Should pick opus (higher codec score)
	if result.Audio.Itag != 251 {
		t.Errorf("expected itag 251 (opus), got %d", result.Audio.Itag)
	}
}

func TestSelectBestFormatsResolutionLimit(t *testing.T) {
	formats := []Format{
		{Itag: 137, MimeType: "video/mp4; codecs=\"avc1\"", Bitrate: 4000000, Width: intPtr(1920), Height: intPtr(1080), Fps: intPtr(30), URL: "https://example.com/v1"},
		{Itag: 136, MimeType: "video/mp4; codecs=\"avc1\"", Bitrate: 2500000, Width: intPtr(1280), Height: intPtr(720), Fps: intPtr(30), URL: "https://example.com/v2"},
		{Itag: 135, MimeType: "video/mp4; codecs=\"avc1\"", Bitrate: 1500000, Width: intPtr(854), Height: intPtr(480), Fps: intPtr(30), URL: "https://example.com/v3"},
	}

	// max dimension 1280 allows up to 720p
	result := SelectBestFormats(formats, 1280, true)
	if result.Video == nil {
		t.Fatal("expected video format")
	}
	if result.Video.Itag != 136 {
		t.Errorf("expected itag 136 (720p), got %d", result.Video.Itag)
	}
}

func TestSelectWithManualItag(t *testing.T) {
	formats := []Format{
		{Itag: 137, MimeType: "video/mp4; codecs=\"avc1\"", Bitrate: 4000000, Width: intPtr(1920), Height: intPtr(1080), URL: "https://example.com/v1"},
		{Itag: 136, MimeType: "video/mp4; codecs=\"avc1\"", Bitrate: 2500000, Width: intPtr(1280), Height: intPtr(720), URL: "https://example.com/v2"},
		{Itag: 140, MimeType: "audio/mp4; codecs=\"mp4a.40.2\"", Bitrate: 128000, AudioQuality: "AUDIO_QUALITY_MEDIUM", URL: "https://example.com/a1"},
	}

	videoItag := 136
	result := SelectWithOptions(formats, 1080, true, &videoItag, nil)
	if result.Video == nil || result.Video.Itag != 136 {
		t.Error("expected manual video itag 136")
	}
}

func TestSelectSkipStream(t *testing.T) {
	formats := []Format{
		{Itag: 137, MimeType: "video/mp4; codecs=\"avc1\"", Bitrate: 4000000, Width: intPtr(1920), Height: intPtr(1080), URL: "https://example.com/v1"},
		{Itag: 140, MimeType: "audio/mp4; codecs=\"mp4a.40.2\"", Bitrate: 128000, AudioQuality: "AUDIO_QUALITY_MEDIUM", URL: "https://example.com/a1"},
	}

	skipVideo := -1
	result := SelectWithOptions(formats, 1080, true, &skipVideo, nil)
	if result.Video != nil {
		t.Error("expected nil video when itag=-1")
	}
	if result.Audio == nil {
		t.Error("expected audio to still be selected")
	}
}

func TestExtractCodec(t *testing.T) {
	tests := []struct {
		mimeType string
		expected string
	}{
		{`video/mp4; codecs="avc1.640028"`, "avc1.640028"},
		{`video/webm; codecs="vp9"`, "vp9"},
		{`audio/webm; codecs="opus"`, "opus"},
		{`audio/mp4; codecs="mp4a.40.2"`, "mp4a.40.2"},
		{"video/mp4", ""},
	}

	for _, tt := range tests {
		result := extractCodec(tt.mimeType)
		if result != tt.expected {
			t.Errorf("extractCodec(%q) = %q, want %q", tt.mimeType, result, tt.expected)
		}
	}
}
