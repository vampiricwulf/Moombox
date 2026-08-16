package youtube

import (
	"testing"
)

func TestSelectBestFormats(t *testing.T) {
	formats := []Format{
		{Itag: 137, MimeType: "video/mp4; codecs=\"avc1.640028\"", Bitrate: 4000000, Width: new(1920), Height: new(1080), Fps: new(30), URL: "https://example.com/v1"},
		{Itag: 298, MimeType: "video/mp4; codecs=\"avc1.640028\"", Bitrate: 4500000, Width: new(1920), Height: new(1080), Fps: new(60), URL: "https://example.com/v2"},
		{Itag: 248, MimeType: "video/webm; codecs=\"vp9\"", Bitrate: 3500000, Width: new(1920), Height: new(1080), Fps: new(30), URL: "https://example.com/v3"},
		{Itag: 136, MimeType: "video/mp4; codecs=\"avc1.4d401f\"", Bitrate: 2500000, Width: new(1280), Height: new(720), Fps: new(30), URL: "https://example.com/v4"},
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
		{Itag: 137, MimeType: "video/mp4; codecs=\"avc1\"", Bitrate: 4000000, Width: new(1920), Height: new(1080), Fps: new(30), URL: "https://example.com/v1"},
		{Itag: 136, MimeType: "video/mp4; codecs=\"avc1\"", Bitrate: 2500000, Width: new(1280), Height: new(720), Fps: new(30), URL: "https://example.com/v2"},
		{Itag: 135, MimeType: "video/mp4; codecs=\"avc1\"", Bitrate: 1500000, Width: new(854), Height: new(480), Fps: new(30), URL: "https://example.com/v3"},
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
		{Itag: 137, MimeType: "video/mp4; codecs=\"avc1\"", Bitrate: 4000000, Width: new(1920), Height: new(1080), URL: "https://example.com/v1"},
		{Itag: 136, MimeType: "video/mp4; codecs=\"avc1\"", Bitrate: 2500000, Width: new(1280), Height: new(720), URL: "https://example.com/v2"},
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
		{Itag: 137, MimeType: "video/mp4; codecs=\"avc1\"", Bitrate: 4000000, Width: new(1920), Height: new(1080), URL: "https://example.com/v1"},
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

func TestSelectBestFormats_AuthLevelTiebreaker(t *testing.T) {
	// Two identical formats at the same resolution/fps/codec/bitrate,
	// differing only in AuthLevel — the lower value wins, and since
	// 2026-08-15 android_vr carries the HIGHEST value (upstream ranks its
	// android tier below web; see types.go), so the WEB format wins.
	vrAuth := AuthLevelAndroidVR
	webAuth := AuthLevelWeb

	formats := []Format{
		{Itag: 137, MimeType: "video/mp4; codecs=\"avc1.640028\"", Bitrate: 4000000, Width: new(1920), Height: new(1080), Fps: new(30), URL: "https://example.com/v1", AuthLevel: &webAuth},
		{Itag: 138, MimeType: "video/mp4; codecs=\"avc1.640028\"", Bitrate: 4000000, Width: new(1920), Height: new(1080), Fps: new(30), URL: "https://example.com/v2", AuthLevel: &vrAuth},
		{Itag: 140, MimeType: "audio/mp4; codecs=\"mp4a.40.2\"", Bitrate: 128000, AudioQuality: "AUDIO_QUALITY_MEDIUM", URL: "https://example.com/a1", AuthLevel: &webAuth},
		{Itag: 141, MimeType: "audio/mp4; codecs=\"mp4a.40.2\"", Bitrate: 128000, AudioQuality: "AUDIO_QUALITY_MEDIUM", URL: "https://example.com/a2", AuthLevel: &vrAuth},
	}

	result := SelectBestFormats(formats, 1920, true)

	if result.Video == nil {
		t.Fatal("expected video format")
	}
	// The WEB format (itag 137) must win over the ANDROID_VR one (138).
	if result.Video.Itag != 137 {
		t.Errorf("expected itag 137 (WEB beats ANDROID_VR), got %d", result.Video.Itag)
	}

	if result.Audio == nil {
		t.Fatal("expected audio format")
	}
	// Same rule on the audio side: 140 is WEB, 141 is ANDROID_VR.
	if result.Audio.Itag != 140 {
		t.Errorf("expected itag 140 (WEB beats ANDROID_VR), got %d", result.Audio.Itag)
	}
}

func TestSelectBestFormats_EmptyFormats(t *testing.T) {
	result := SelectBestFormats([]Format{}, 1920, true)
	if result.Video != nil {
		t.Error("expected nil video for empty formats")
	}
	if result.Audio != nil {
		t.Error("expected nil audio for empty formats")
	}
}

func TestSelectBestFormats_NilFormats(t *testing.T) {
	result := SelectBestFormats(nil, 1920, true)
	if result.Video != nil || result.Audio != nil {
		t.Error("expected nil results for nil formats")
	}
}

func TestSelectBestFormats_Prefer30fps(t *testing.T) {
	formats := []Format{
		{Itag: 137, MimeType: "video/mp4; codecs=\"avc1.640028\"", Bitrate: 4000000, Width: new(1920), Height: new(1080), Fps: new(30), URL: "https://example.com/v1"},
		{Itag: 298, MimeType: "video/mp4; codecs=\"avc1.640028\"", Bitrate: 4500000, Width: new(1920), Height: new(1080), Fps: new(60), URL: "https://example.com/v2"},
	}

	// prefer60fps=false should pick 30fps
	result := SelectBestFormats(formats, 1920, false)
	if result.Video == nil {
		t.Fatal("expected video format")
	}
	if result.Video.Itag != 137 {
		t.Errorf("expected itag 137 (30fps), got %d", result.Video.Itag)
	}
}

func TestSelectBestFormats_NoURLSkipped(t *testing.T) {
	formats := []Format{
		{Itag: 137, MimeType: "video/mp4; codecs=\"avc1\"", Bitrate: 4000000, Width: new(1920), Height: new(1080), URL: ""},
		{Itag: 136, MimeType: "video/mp4; codecs=\"avc1\"", Bitrate: 2500000, Width: new(1280), Height: new(720), URL: "https://example.com/v2"},
		{Itag: 140, MimeType: "audio/mp4; codecs=\"mp4a.40.2\"", Bitrate: 128000, AudioQuality: "AUDIO_QUALITY_MEDIUM", URL: ""},
		{Itag: 251, MimeType: "audio/webm; codecs=\"opus\"", Bitrate: 160000, AudioQuality: "AUDIO_QUALITY_MEDIUM", URL: "https://example.com/a2"},
	}

	result := SelectBestFormats(formats, 1920, true)
	if result.Video == nil {
		t.Fatal("expected video format")
	}
	if result.Video.Itag != 136 {
		t.Errorf("formats without URL should be skipped; expected 136, got %d", result.Video.Itag)
	}
	if result.Audio == nil {
		t.Fatal("expected audio format")
	}
	if result.Audio.Itag != 251 {
		t.Errorf("audio without URL should be skipped; expected 251, got %d", result.Audio.Itag)
	}
}

func TestSelectBestFormats_CodecTiebreaker(t *testing.T) {
	// Same resolution, same fps — vp9 should beat avc1 (higher codec score)
	formats := []Format{
		{Itag: 137, MimeType: "video/mp4; codecs=\"avc1.640028\"", Bitrate: 4000000, Width: new(1920), Height: new(1080), Fps: new(30), URL: "https://example.com/v1"},
		{Itag: 248, MimeType: "video/webm; codecs=\"vp9\"", Bitrate: 3500000, Width: new(1920), Height: new(1080), Fps: new(30), URL: "https://example.com/v2"},
	}

	result := SelectBestFormats(formats, 1920, true)
	if result.Video == nil {
		t.Fatal("expected video format")
	}
	if result.Video.Itag != 248 {
		t.Errorf("expected itag 248 (vp9 higher codec score), got %d", result.Video.Itag)
	}
}

// TestSelectBestFormats_MatchesYtDlp is the regression guard for the
// "picked 303 (vp9) instead of 721 (AV1 Premium)" report. It pins the two
// yt-dlp-matching rules: AV1 beats vp9 at the same res/fps (codec order), and
// among AV1 the higher-bitrate Premium stream wins (quality tiebreak). yt-dlp's
// default selects exactly 721 for this shape.
func TestSelectBestFormats_MatchesYtDlp(t *testing.T) {
	formats := []Format{
		{Itag: 299, MimeType: `video/mp4; codecs="avc1.64002a"`, Bitrate: 5828877, Width: new(1920), Height: new(1080), Fps: new(60), URL: "https://x/avc"},
		{Itag: 303, MimeType: `video/webm; codecs="vp9"`, Bitrate: 5330212, Width: new(1920), Height: new(1080), Fps: new(60), URL: "https://x/vp9"},
		{Itag: 399, MimeType: `video/mp4; codecs="av01.0.09M.08"`, Bitrate: 4794946, Width: new(1920), Height: new(1080), Fps: new(60), URL: "https://x/av1"},
		{Itag: 721, MimeType: `video/mp4; codecs="av01.0.09M.08"`, Bitrate: 7609081, Width: new(1920), Height: new(1080), Fps: new(60), URL: "https://x/av1-premium"},
		{Itag: 251, MimeType: `audio/webm; codecs="opus"`, Bitrate: 165407, AudioQuality: "AUDIO_QUALITY_MEDIUM", URL: "https://x/opus"},
		{Itag: 140, MimeType: `audio/mp4; codecs="mp4a.40.2"`, Bitrate: 131072, AudioQuality: "AUDIO_QUALITY_MEDIUM", URL: "https://x/aac"},
	}

	result := SelectBestFormats(formats, 1920, true)
	if result.Video == nil || result.Video.Itag != 721 {
		t.Fatalf("expected itag 721 (AV1 Premium — highest codec + bitrate), got %v", result.Video)
	}
	if result.Audio == nil || result.Audio.Itag != 251 {
		t.Fatalf("expected itag 251 (opus), got %v", result.Audio)
	}
}

func TestScoreVideoCodec(t *testing.T) {
	tests := []struct {
		codec string
		want  int
	}{
		{"av01.0.09M.08", 6},
		{"vp9.2", 5},
		{"vp9", 4},
		{"vp09.00.10.08", 4},
		{"hev1.1.6.L93", 3},
		{"h265", 3},
		{"avc1.640028", 2},
		{"h264", 2},
		{"vp8", 1},
		{"unknown", 0},
		{"", 0},
	}

	for _, tt := range tests {
		got := scoreVideoCodec(tt.codec)
		if got != tt.want {
			t.Errorf("scoreVideoCodec(%q) = %d, want %d", tt.codec, got, tt.want)
		}
	}
}

func TestScoreAudioCodec(t *testing.T) {
	tests := []struct {
		codec string
		want  int
	}{
		{"opus", 4},
		{"mp4a.40.5", 3},
		{"mp4a.40.2", 2},
		{"mp4a.40.1", 1},
		{"unknown", 0},
		{"", 0},
	}

	for _, tt := range tests {
		got := scoreAudioCodec(tt.codec)
		if got != tt.want {
			t.Errorf("scoreAudioCodec(%q) = %d, want %d", tt.codec, got, tt.want)
		}
	}
}

func TestAuthLevelOf(t *testing.T) {
	auth := AuthLevelWeb
	f1 := &Format{AuthLevel: &auth}
	if authLevelOf(f1) != AuthLevelWeb {
		t.Errorf("expected AuthLevelWeb, got %d", authLevelOf(f1))
	}

	f2 := &Format{}
	if authLevelOf(f2) != 999 {
		t.Errorf("expected 999 for nil auth level, got %d", authLevelOf(f2))
	}
}
