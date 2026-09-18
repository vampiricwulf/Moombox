package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestParsePlayabilityStatus_NilStatus(t *testing.T) {
	errType, reason := parsePlayabilityStatus(nil)
	if errType != PlayabilityUnknown {
		t.Errorf("expected PlayabilityUnknown, got %q", errType)
	}
	if reason != "" {
		t.Errorf("expected empty reason, got %q", reason)
	}
}

func TestParsePlayabilityStatus_OK(t *testing.T) {
	status := map[string]any{"status": "OK"}
	errType, _ := parsePlayabilityStatus(status)
	if errType != PlayabilityOK {
		t.Errorf("expected PlayabilityOK, got %q", errType)
	}
}

func TestParsePlayabilityStatus_LoginRequired(t *testing.T) {
	status := map[string]any{
		"status": "LOGIN_REQUIRED",
		"reason": "Sign in to confirm your age",
	}
	errType, reason := parsePlayabilityStatus(status)
	if errType != PlayabilityAgeRestricted {
		t.Errorf("expected PlayabilityAgeRestricted, got %q", errType)
	}
	if reason == "" {
		t.Error("expected non-empty reason")
	}
}

func TestParsePlayabilityStatus_LoginRequiredGeneric(t *testing.T) {
	status := map[string]any{
		"status": "LOGIN_REQUIRED",
		"reason": "Please sign in to continue",
	}
	errType, _ := parsePlayabilityStatus(status)
	if errType != PlayabilityLoginRequired {
		t.Errorf("expected PlayabilityLoginRequired, got %q", errType)
	}
}

func TestParsePlayabilityStatus_MembersOnly(t *testing.T) {
	tests := []struct {
		name   string
		status map[string]any
	}{
		{
			"login_required_members",
			map[string]any{
				"status": "LOGIN_REQUIRED",
				"reason": "Join this channel to get access to members-only content",
			},
		},
		{
			"unplayable_members",
			map[string]any{
				"status": "UNPLAYABLE",
				"reason": "This video is available to members only",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errType, _ := parsePlayabilityStatus(tt.status)
			if errType != PlayabilityMembersOnly {
				t.Errorf("expected PlayabilityMembersOnly, got %q", errType)
			}
		})
	}
}

func TestParsePlayabilityStatus_Upcoming(t *testing.T) {
	tests := []struct {
		name   string
		status map[string]any
	}{
		{
			"live_stream_offline",
			map[string]any{"status": "LIVE_STREAM_OFFLINE"},
		},
		{
			"unplayable_live_event",
			map[string]any{
				"status": "UNPLAYABLE",
				"reason": "This live event will begin in a few moments",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errType, _ := parsePlayabilityStatus(tt.status)
			if errType != PlayabilityOK {
				t.Errorf("expected PlayabilityOK (upcoming is not an error), got %q", errType)
			}
		})
	}
}

func TestParsePlayabilityStatus_Private(t *testing.T) {
	status := map[string]any{
		"status": "UNPLAYABLE",
		"reason": "This video is private",
	}
	errType, _ := parsePlayabilityStatus(status)
	if errType != PlayabilityPrivate {
		t.Errorf("expected PlayabilityPrivate, got %q", errType)
	}
}

func TestParsePlayabilityStatus_RegionBlocked(t *testing.T) {
	status := map[string]any{
		"status": "UNPLAYABLE",
		"reason": "This video is not available in your country",
	}
	errType, _ := parsePlayabilityStatus(status)
	if errType != PlayabilityRegionBlocked {
		t.Errorf("expected PlayabilityRegionBlocked, got %q", errType)
	}
}

func TestParsePlayabilityStatus_AgeRestricted(t *testing.T) {
	status := map[string]any{
		"status": "AGE_VERIFICATION_REQUIRED",
		"reason": "Age-restricted",
	}
	errType, _ := parsePlayabilityStatus(status)
	if errType != PlayabilityAgeRestricted {
		t.Errorf("expected PlayabilityAgeRestricted, got %q", errType)
	}
}

func TestParsePlayabilityStatus_FallbackMessages(t *testing.T) {
	// When reason is empty, should fall back to messages array
	status := map[string]any{
		"status":   "UNPLAYABLE",
		"messages": []any{"Video is unavailable"},
	}
	errType, reason := parsePlayabilityStatus(status)
	if errType != PlayabilityUnavailable {
		t.Errorf("expected PlayabilityUnavailable, got %q", errType)
	}
	if reason != "Video is unavailable" {
		t.Errorf("expected reason from messages, got %q", reason)
	}
}

func TestClassifyStream_NotAStream(t *testing.T) {
	vd := map[string]any{"isLiveContent": false}
	status, isLive, isUpcoming, isPostLiveDVR := classifyStream(vd, nil, nil, true)
	if status != StreamNotAStream {
		t.Errorf("expected StreamNotAStream, got %q", status)
	}
	if isLive || isUpcoming || isPostLiveDVR {
		t.Error("expected all flags false for not-a-stream")
	}
}

func TestClassifyStream_Live(t *testing.T) {
	vd := map[string]any{
		"isLiveContent": true,
		"isLive":        true,
	}
	status, isLive, isUpcoming, _ := classifyStream(vd, nil, nil, true)
	if status != StreamLive {
		t.Errorf("expected StreamLive, got %q", status)
	}
	if !isLive {
		t.Error("expected isLive=true")
	}
	if isUpcoming {
		t.Error("expected isUpcoming=false")
	}
}

func TestClassifyStream_Upcoming(t *testing.T) {
	vd := map[string]any{"isLiveContent": true}
	ps := map[string]any{"status": "LIVE_STREAM_OFFLINE"}
	status, _, isUpcoming, _ := classifyStream(vd, ps, nil, false)
	if status != StreamUpcoming {
		t.Errorf("expected StreamUpcoming, got %q", status)
	}
	if !isUpcoming {
		t.Error("expected isUpcoming=true")
	}
}

func TestClassifyStream_UpcomingVD_WaitingRoom(t *testing.T) {
	// When YouTube starts the waiting room at scheduled time, isLive becomes true
	// but isUpcoming is also true. Without microformat, this should still classify
	// as upcoming (not live) since the creator hasn't actually started streaming.
	vd := map[string]any{
		"isLiveContent": true,
		"isLive":        true,
		"isUpcoming":    true,
	}
	status, isLive, isUpcoming, _ := classifyStream(vd, nil, nil, false)
	if status != StreamUpcoming {
		t.Errorf("expected StreamUpcoming for waiting room, got %q", status)
	}
	if isLive {
		t.Error("expected isLive=false for waiting room")
	}
	if !isUpcoming {
		t.Error("expected isUpcoming=true for waiting room")
	}
}

func TestClassifyStream_UpcomingVD_WithFormats(t *testing.T) {
	// If isUpcoming is true but formats are present, the stream is actually
	// playable — should NOT be forced to upcoming.
	vd := map[string]any{
		"isLiveContent": true,
		"isLive":        true,
		"isUpcoming":    true,
	}
	status, isLive, _, _ := classifyStream(vd, nil, nil, true)
	if status == StreamUpcoming {
		t.Error("should not classify as upcoming when formats are present")
	}
	if !isLive {
		t.Error("expected isLive=true when formats are present")
	}
}

func TestClassifyStream_UpcomingVD_Only(t *testing.T) {
	// When only isUpcoming=true is set (no isLive, no playability, no microformat),
	// should classify as upcoming via the standalone isUpcomingVD check.
	vd := map[string]any{
		"isLiveContent": true,
		"isUpcoming":    true,
	}
	status, _, isUpcoming, _ := classifyStream(vd, nil, nil, false)
	if status != StreamUpcoming {
		t.Errorf("expected StreamUpcoming, got %q", status)
	}
	if !isUpcoming {
		t.Error("expected isUpcoming=true")
	}
}

func TestClassifyStream_VOD(t *testing.T) {
	vd := map[string]any{"isLiveContent": true}
	mf := map[string]any{
		"liveBroadcastDetails": map[string]any{
			"startTimestamp": "2025-01-01T00:00:00Z",
		},
	}
	status, _, _, _ := classifyStream(vd, nil, mf, true)
	if status != StreamVOD {
		t.Errorf("expected StreamVOD, got %q", status)
	}
}

func TestClassifyStream_LiveStreamabilityFallback(t *testing.T) {
	// ANDROID_VR probes of an unpublished scheduled premiere sometimes
	// return only playabilityStatus.liveStreamability — no microformat,
	// no videoDetails.isUpcoming, no LIVE_STREAM_OFFLINE status. Those
	// should still classify as upcoming rather than not_a_stream.
	vd := map[string]any{}
	ps := map[string]any{
		"status": "OK",
		"liveStreamability": map[string]any{
			"liveStreamabilityRenderer": map[string]any{
				"offlineSlate": map[string]any{
					"liveStreamOfflineSlateRenderer": map[string]any{
						"scheduledStartTime": "1999999999",
					},
				},
			},
		},
	}

	status, _, isUpcoming, _ := classifyStream(vd, ps, nil, false)
	if status != StreamUpcoming {
		t.Errorf("expected StreamUpcoming from liveStreamability, got %q", status)
	}
	if !isUpcoming {
		t.Error("expected isUpcoming=true from liveStreamability")
	}
}

func TestClassifyStream_LiveStreamabilityDoesNotOverrideFormats(t *testing.T) {
	// A stream that has liveStreamability but also has formats is already
	// live — the hasFormats guard should keep classification out of the
	// upcoming branch.
	vd := map[string]any{"isLiveContent": true, "isLive": true}
	ps := map[string]any{
		"status": "OK",
		"liveStreamability": map[string]any{
			"liveStreamabilityRenderer": map[string]any{},
		},
	}

	status, isLive, _, _ := classifyStream(vd, ps, nil, true)
	if status == StreamUpcoming {
		t.Errorf("should not classify as upcoming when formats are present, got %q", status)
	}
	if !isLive {
		t.Error("expected isLive=true when formats are present")
	}
}

func TestClassifyStream_PostLiveDVR(t *testing.T) {
	vd := map[string]any{"isLiveContent": true}
	mf := map[string]any{
		"liveBroadcastDetails": map[string]any{
			"startTimestamp": "2025-01-01T00:00:00Z",
			"endTimestamp":   "2025-01-01T02:00:00Z",
		},
	}
	status, _, _, isPostLiveDVR := classifyStream(vd, nil, mf, true)
	if status != StreamPostLive {
		t.Errorf("expected StreamPostLive, got %q", status)
	}
	if !isPostLiveDVR {
		t.Error("expected isPostLiveDVR=true")
	}
}

func TestCollectFormats(t *testing.T) {
	pool := []Format{}
	formats := []Format{
		{Itag: 137, MimeType: "video/mp4", URL: "https://example.com/v"},
		{Itag: 140, MimeType: "audio/mp4", URL: "https://example.com/a"},
	}
	collectFormats(&pool, formats, "test_source", AuthLevelWeb)

	if len(pool) != 2 {
		t.Fatalf("expected 2 formats in pool, got %d", len(pool))
	}
	for _, f := range pool {
		if f.Source != "test_source" {
			t.Errorf("expected source 'test_source', got %q", f.Source)
		}
		if f.AuthLevel == nil || *f.AuthLevel != AuthLevelWeb {
			t.Errorf("expected AuthLevelWeb, got %v", f.AuthLevel)
		}
	}
}

func TestCollectFormats_DoesNotMutateInput(t *testing.T) {
	formats := []Format{
		{Itag: 137, MimeType: "video/mp4", URL: "https://example.com/v"},
		{Itag: 140, MimeType: "audio/mp4", URL: "https://example.com/a"},
	}

	pool := []Format{}
	collectFormats(&pool, formats, "first", AuthLevelWeb)

	// The caller's slice must retain its original empty Source/AuthLevel so
	// that a subsequent collectFormats call with a different source/level
	// does not get a stale value from the previous call.
	for i, f := range formats {
		if f.Source != "" {
			t.Errorf("formats[%d].Source was mutated to %q, expected empty", i, f.Source)
		}
		if f.AuthLevel != nil {
			t.Errorf("formats[%d].AuthLevel was mutated to %v, expected nil", i, f.AuthLevel)
		}
	}

	// A second collect with a different source/level must not be leaked into
	// previously-added pool entries.
	pool2 := []Format{}
	collectFormats(&pool2, formats, "second", AuthLevelAndroidVR)
	for _, f := range pool {
		if f.Source != "first" {
			t.Errorf("pool entry source changed to %q after second collect", f.Source)
		}
		if f.AuthLevel == nil || *f.AuthLevel != AuthLevelWeb {
			t.Errorf("pool entry auth level changed to %v after second collect", f.AuthLevel)
		}
	}
}

func TestDeduplicateFormats(t *testing.T) {
	webAuth := AuthLevelWeb
	vrAuth := AuthLevelAndroidVR

	pool := []Format{
		{Itag: 137, URL: "https://example.com/v1", AuthLevel: &webAuth},
		{Itag: 137, URL: "https://example.com/v2", AuthLevel: &vrAuth}, // android tier: must LOSE
		{Itag: 140, URL: "https://example.com/a1", AuthLevel: &webAuth},
		{Itag: 999, URL: "", AuthLevel: &webAuth}, // no URL, should be filtered
	}

	result := deduplicateFormats(pool)

	if len(result) != 2 {
		t.Fatalf("expected 2 deduplicated formats, got %d", len(result))
	}

	// itag 137 must resolve to the WEB entry, not the ANDROID_VR one. Ranking
	// android_vr last mirrors yt-dlp's client priority (android tier, below
	// web); ranking it first is what put live segment downloads on ANDROID_VR
	// URLs while carrying a WebPO token that does not apply to that client —
	// a 403 every ~20s in the field (2026-08-15). See the AuthLevel block in
	// types.go.
	for _, f := range result {
		if f.Itag == 137 {
			if *f.AuthLevel != AuthLevelWeb {
				t.Errorf("itag 137 auth level = %d, want AuthLevelWeb (%d): a WEB format must beat an ANDROID_VR one",
					*f.AuthLevel, AuthLevelWeb)
			}
			if f.URL != "https://example.com/v1" {
				t.Errorf("itag 137 URL = %q, want the WEB url", f.URL)
			}
		}
	}
}

// TestParseFormats_DefersCipherDecryption verifies the Stage 3 contract:
// parseFormats captures the raw URL + EncryptedSig/SigKey from a
// signatureCipher entry without invoking any cipher solver. The actual
// resolution happens in worker strategies via cipher.ResolveFormatURL.
func TestParseFormats_DefersCipherDecryption(t *testing.T) {
	logger := noopLogger{}
	pa := NewPlayerAPI(nil, logger)
	// No cipher solver wired — parseFormats must NOT call one.

	streamingData := map[string]any{
		"adaptiveFormats": []any{
			map[string]any{
				"itag":              float64(137),
				"mimeType":          `video/mp4; codecs="avc1.640028"`,
				"bitrate":           float64(2500000),
				"width":             float64(1920),
				"height":            float64(1080),
				"targetDurationSec": float64(5),
				"signatureCipher":   "url=https%3A%2F%2Fr1---sn-test.googlevideo.com%2Fvideoplayback%3Fexpire%3D123%26n%3DENC_N%26itag%3D137&s=ENC_SIG&sp=signature",
			},
			map[string]any{
				"itag":     float64(140),
				"mimeType": `audio/mp4; codecs="mp4a.40.2"`,
				"bitrate":  float64(128000),
				"url":      "https://r1---sn-test.googlevideo.com/videoplayback?expire=123&n=ENC&itag=140",
			},
		},
	}

	got := pa.parseFormats(streamingData)

	if len(got) != 2 {
		t.Fatalf("expected 2 formats, got %d", len(got))
	}

	// First format: sigCipher entry. URL should be the bare URL (sig
	// not yet appended); EncryptedSig + SigKey populated.
	video := got[0]
	if video.Itag != 137 {
		t.Errorf("video itag: want 137 got %d", video.Itag)
	}
	if video.URL == "" {
		t.Errorf("video URL should be set (raw URL part), got empty")
	}
	if video.EncryptedSig != "ENC_SIG" {
		t.Errorf("video EncryptedSig: want ENC_SIG got %q", video.EncryptedSig)
	}
	if video.SigKey != "signature" {
		t.Errorf("video SigKey: want signature got %q", video.SigKey)
	}
	if video.TargetDurationSec != 5 {
		t.Errorf("video TargetDurationSec: want 5 got %d", video.TargetDurationSec)
	}
	// URL must not yet contain the decrypted sig — the strategy resolves
	// it later via cipher.ResolveFormatURL.
	if got, want := video.URL, "https://r1---sn-test.googlevideo.com/videoplayback?expire=123&n=ENC_N&itag=137"; got != want {
		t.Errorf("video URL: want %q got %q", want, got)
	}

	// Second format: direct URL. EncryptedSig should be empty.
	audio := got[1]
	if audio.Itag != 140 {
		t.Errorf("audio itag: want 140 got %d", audio.Itag)
	}
	if audio.EncryptedSig != "" {
		t.Errorf("direct URL audio should have empty EncryptedSig, got %q", audio.EncryptedSig)
	}
	if audio.SigKey != "" {
		t.Errorf("direct URL audio should have empty SigKey, got %q", audio.SigKey)
	}
}

// TestParseFormats_DefaultsSigKey — when sp is missing from the
// signatureCipher (older players), default to "signature".
func TestParseFormats_DefaultsSigKey(t *testing.T) {
	pa := NewPlayerAPI(nil, noopLogger{})

	streamingData := map[string]any{
		"adaptiveFormats": []any{
			map[string]any{
				"itag":            float64(137),
				"mimeType":        `video/mp4`,
				"signatureCipher": "url=https%3A%2F%2Fexample.com%2F&s=ENC",
				// no sp
			},
		},
	}

	got := pa.parseFormats(streamingData)
	if len(got) != 1 {
		t.Fatalf("expected 1 format, got %d", len(got))
	}
	if got[0].SigKey != "signature" {
		t.Errorf("default SigKey: want signature got %q", got[0].SigKey)
	}
}

func TestDeduplicateFormats_SameAuthPrefersFirstInsertion(t *testing.T) {
	// When two formats share the same itag AND auth level, the first one
	// inserted into the pool wins. This is the guarantee
	// parseFormatsWithCipher relies on to prefer adaptiveFormats over the
	// legacy muxed formats[] array (adaptiveFormats is iterated first).
	webAuth := AuthLevelWeb
	pool := []Format{
		{Itag: 137, URL: "https://example.com/adaptive", AuthLevel: &webAuth, Source: "adaptive"},
		{Itag: 137, URL: "https://example.com/muxed", AuthLevel: &webAuth, Source: "muxed"},
	}

	result := deduplicateFormats(pool)
	if len(result) != 1 {
		t.Fatalf("expected 1 deduplicated format, got %d", len(result))
	}
	if result[0].Source != "adaptive" {
		t.Errorf("expected first-inserted (adaptive) to win same-auth tiebreak, got %q", result[0].Source)
	}
}

func TestDeduplicateFormats_EmptyPool(t *testing.T) {
	result := deduplicateFormats(nil)
	if len(result) != 0 {
		t.Errorf("expected 0 formats, got %d", len(result))
	}
}

func TestHasAdequateFormats(t *testing.T) {
	// No formats
	info := &VideoInfo{Formats: []Format{}}
	if hasAdequateFormats(info) {
		t.Error("expected false for empty formats")
	}

	// Only video
	info = &VideoInfo{Formats: []Format{
		{Itag: 137, MimeType: "video/mp4", Width: new(1920), Height: new(1080), URL: "https://example.com"},
	}}
	if hasAdequateFormats(info) {
		t.Error("expected false for video-only formats")
	}

	// Video + audio
	info = &VideoInfo{Formats: []Format{
		{Itag: 137, MimeType: "video/mp4", Width: new(1920), Height: new(1080), URL: "https://example.com"},
		{Itag: 140, MimeType: "audio/mp4", AudioQuality: "AUDIO_QUALITY_MEDIUM", URL: "https://example.com"},
	}}
	if !hasAdequateFormats(info) {
		t.Error("expected true for video+audio formats")
	}

	// Audio detection should work even when AudioQuality is empty but mime
	// type says audio (seen with some Innertube adaptiveFormats payloads).
	info = &VideoInfo{Formats: []Format{
		{Itag: 137, MimeType: "video/mp4", Width: new(1920), Height: new(1080), URL: "https://example.com"},
		{Itag: 140, MimeType: "audio/mp4; codecs=\"mp4a.40.2\"", URL: "https://example.com"},
	}}
	if !hasAdequateFormats(info) {
		t.Error("expected true when audio-only format has empty AudioQuality but audio mime type")
	}
}

func TestFormatIsAudio(t *testing.T) {
	tests := []struct {
		name string
		f    Format
		want bool
	}{
		{
			"audio mime, no AudioQuality",
			Format{MimeType: "audio/mp4; codecs=\"mp4a.40.2\""},
			true,
		},
		{
			"audio mime with AudioQuality",
			Format{MimeType: "audio/webm; codecs=\"opus\"", AudioQuality: "AUDIO_QUALITY_MEDIUM"},
			true,
		},
		{
			"video mime",
			Format{MimeType: "video/mp4", Width: new(1920), Height: new(1080)},
			false,
		},
		{
			"audio mime but has Width set (combined/muxed — unusual)",
			Format{MimeType: "audio/mp4", Width: new(1920)},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.f.IsAudio(); got != tt.want {
				t.Errorf("IsAudio() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMergeWatchPageMetadata(t *testing.T) {
	target := &VideoInfo{
		Title:       "Unknown Title",
		ChannelName: "Unknown Channel",
	}
	source := &VideoInfo{
		Title:              "Real Title",
		ChannelName:        "Real Channel",
		ChannelID:          "UC123",
		Description:        "A description",
		ScheduledStartTime: "2025-01-01T00:00:00Z",
		DashManifestURL:    "https://example.com/dash",
	}

	mergeWatchPageMetadata(target, source)

	if target.Title != "Real Title" {
		t.Errorf("expected merged title, got %q", target.Title)
	}
	if target.ChannelName != "Real Channel" {
		t.Errorf("expected merged channel name, got %q", target.ChannelName)
	}
	if target.ChannelID != "UC123" {
		t.Errorf("expected merged channel ID, got %q", target.ChannelID)
	}
	if target.ScheduledStartTime != "2025-01-01T00:00:00Z" {
		t.Errorf("expected merged start time, got %q", target.ScheduledStartTime)
	}
}

func TestMergeWatchPageMetadata_NilSource(t *testing.T) {
	target := &VideoInfo{Title: "Keep"}
	mergeWatchPageMetadata(target, nil)
	if target.Title != "Keep" {
		t.Error("nil source should not modify target")
	}
}

func TestMergeWatchPageMetadata_DoNotOverwrite(t *testing.T) {
	target := &VideoInfo{
		Title:       "My Title",
		ChannelName: "My Channel",
		ChannelID:   "UC111",
	}
	source := &VideoInfo{
		Title:       "Other Title",
		ChannelName: "Other Channel",
		ChannelID:   "UC222",
	}

	mergeWatchPageMetadata(target, source)

	// Title and ChannelName already set (not "Unknown"), so should not overwrite
	if target.Title != "My Title" {
		t.Errorf("should not overwrite existing title, got %q", target.Title)
	}
	if target.ChannelName != "My Channel" {
		t.Errorf("should not overwrite existing channel name, got %q", target.ChannelName)
	}
	// ChannelID already set, should not overwrite
	if target.ChannelID != "UC111" {
		t.Errorf("should not overwrite existing channel ID, got %q", target.ChannelID)
	}
}

func TestMergeWatchPageMetadata_ScheduledStartTimeOverwrite(t *testing.T) {
	// Watch page's liveBroadcastDetails.startTimestamp is authoritative —
	// it should overwrite a stale liveStreamability.scheduledStartTime from TV client.
	target := &VideoInfo{
		ScheduledStartTime: "2025-01-01T14:00:00Z", // old time from TV client
	}
	source := &VideoInfo{
		ScheduledStartTime: "2025-01-01T16:00:00Z", // rescheduled time from watch page
	}

	mergeWatchPageMetadata(target, source)

	if target.ScheduledStartTime != "2025-01-01T16:00:00Z" {
		t.Errorf("expected watch page's rescheduled time, got %q", target.ScheduledStartTime)
	}
}

// --- JSON helper tests ---

func TestGetStr(t *testing.T) {
	m := map[string]any{"key": "value", "num": 42}
	if getStr(m, "key") != "value" {
		t.Error("expected 'value'")
	}
	if getStr(m, "num") != "" {
		t.Error("expected empty for non-string")
	}
	if getStr(m, "missing") != "" {
		t.Error("expected empty for missing key")
	}
	if getStr(nil, "key") != "" {
		t.Error("expected empty for nil map")
	}
}

func TestGetInt(t *testing.T) {
	m := map[string]any{"f64": float64(42), "i": 7, "s": "text"}
	if getInt(m, "f64") != 42 {
		t.Error("expected 42 from float64")
	}
	if getInt(m, "i") != 7 {
		t.Error("expected 7 from int")
	}
	if getInt(m, "s") != 0 {
		t.Error("expected 0 for non-numeric")
	}
	if getInt(nil, "key") != 0 {
		t.Error("expected 0 for nil map")
	}
}

func TestGetNestedMap(t *testing.T) {
	m := map[string]any{
		"a": map[string]any{
			"b": map[string]any{
				"c": "deep",
			},
		},
	}
	result, ok := getNestedMap(m, "a", "b")
	if !ok || getStr(result, "c") != "deep" {
		t.Error("expected nested map traversal to work")
	}

	_, ok = getNestedMap(m, "a", "missing")
	if ok {
		t.Error("expected false for missing key")
	}

	_, ok = getNestedMap(nil, "a")
	if ok {
		t.Error("expected false for nil map")
	}
}

func TestGetDeepStr(t *testing.T) {
	m := map[string]any{
		"a": map[string]any{
			"b": map[string]any{
				"c": "found",
			},
		},
	}
	if getDeepStr(m, "a", "b", "c") != "found" {
		t.Error("expected deep string traversal to work")
	}
	if getDeepStr(m, "a", "missing", "c") != "" {
		t.Error("expected empty for missing path")
	}
	if getDeepStr(m) != "" {
		t.Error("expected empty for no keys")
	}
}

func TestExtractScheduledStartTime(t *testing.T) {
	// From liveBroadcastDetails
	mf := map[string]any{
		"liveBroadcastDetails": map[string]any{
			"startTimestamp": "2025-01-01T00:00:00Z",
		},
	}
	result := extractScheduledStartTime(mf, nil)
	if result != "2025-01-01T00:00:00Z" {
		t.Errorf("expected ISO timestamp, got %q", result)
	}

	// From uploadDate fallback
	mf2 := map[string]any{
		"uploadDate": "2025-06-15",
	}
	result2 := extractScheduledStartTime(mf2, nil)
	if result2 != "2025-06-15" {
		t.Errorf("expected uploadDate, got %q", result2)
	}

	// Nil inputs
	result3 := extractScheduledStartTime(nil, nil)
	if result3 != "" {
		t.Errorf("expected empty for nil inputs, got %q", result3)
	}
}

func TestExtractPublishedAt(t *testing.T) {
	cases := []struct {
		name, status, wantTS, wantPrec string
		microformat                    map[string]any
	}{
		// YouTube emits startTimestamp offset-bearing ("+00:00"); it MUST come
		// back Z-normalized. Consumers compare PublishedAt LEXICOGRAPHICALLY
		// against Z-format cutoffs (the archive window re-check, the walk's
		// exhaustion math, DECAPI's window check) and store it into
		// feed_items.published, whose contract is "RFC3339 UTC — lexicographic
		// order IS chronological order". An offset-bearing string mis-compares
		// at every window boundary, and once stored at 'started' rank it can
		// never be corrected (rank ties don't overwrite).
		{"post_live takes startTimestamp as started, Z-normalized", "post_live", "2026-07-14T20:00:00Z", "started",
			playerWith(map[string]any{"startTimestamp": "2026-07-14T20:00:00+00:00", "endTimestamp": "2026-07-14T22:00:00+00:00"}, "2026-07-01")},
		{"vod without lbd falls back to uploadDate as day, normalized to end of day", "vod", "2026-07-01T23:59:59Z", "day",
			playerWith(nil, "2026-07-01")},
		{"not_a_stream takes uploadDate as day, normalized to end of day", "not_a_stream", "2026-07-01T23:59:59Z", "day",
			playerWith(nil, "2026-07-01")},
		// Microformat dates with a time component carry local offsets
		// ("-07:00"). 13:00-07:00 IS 20:00Z, but the raw string sorts as 13:00
		// against a Z cutoff — hours of error at the same lexicographic seam
		// as above, unhealable once stored at 'day' rank. Convert to UTC.
		{"day value with a time component is Z-normalized", "vod", "2026-07-01T20:00:00Z", "day",
			playerWith(nil, "2026-07-01T13:00:00-07:00")},
		// Unparseable values must become NO-date, not pass-through garbage: a
		// non-chronological string stored at started/day rank would sort
		// nonsensically forever. A bad startTimestamp falls back to the
		// microformat date; a bad microformat date yields nothing.
		{"unparseable startTimestamp falls back to the microformat day date", "post_live", "2026-07-01T23:59:59Z", "day",
			playerWith(map[string]any{"startTimestamp": "garbage"}, "2026-07-01")},
		{"unparseable microformat date with a time component yields nothing", "vod", "", "",
			playerWith(nil, "2026-07-01T99:99:99")},
		{"upcoming stores nothing — startTimestamp here is the FUTURE", "upcoming", "", "",
			playerWith(map[string]any{"startTimestamp": "2027-01-01T00:00:00+00:00"}, "2026-07-01")},
		{"live stores nothing", "live", "", "", playerWith(nil, "2026-07-01")},
		{"vod with neither yields nothing (caller enforces the terminal invariant)", "vod", "", "",
			playerWith(nil, "")},
	}
	for _, c := range cases {
		ts, prec := extractPublishedAt(c.status, c.microformat)
		if ts != c.wantTS || prec != c.wantPrec {
			t.Errorf("%s: got (%q,%q) want (%q,%q)", c.name, ts, prec, c.wantTS, c.wantPrec)
		}
	}
}

// playerWith builds the minimal already-unwrapped microformat map —
// the same shape classifyStream and extractScheduledStartTime consume in
// this file (see TestClassifyStream_PostLiveDVR and
// TestExtractScheduledStartTime above): liveBroadcastDetails sits directly
// at the top level, not nested under microformat.playerMicroformatRenderer.
func playerWith(lbd map[string]any, uploadDate string) map[string]any {
	m := map[string]any{}
	if lbd != nil {
		m["liveBroadcastDetails"] = lbd
	}
	if uploadDate != "" {
		m["uploadDate"] = uploadDate
	}
	return m
}

// decodePlayerJSON is a small helper for the parse tests: the parser takes the
// already-decoded map YouTube's body unmarshals into.
func decodePlayerJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	return m
}

// TestParsePlayerResponseRejectsASubstituteVideo pins yt-dlp's
// _invalid_player_response (_video.py:3022-3025). A blocked source IP is
// served a DIFFERENT video's player response; accepting it hands this job the
// substitute's status and formats, and the waiting-room probe then archives
// the wrong video under this job's ID.
//
// Mutants this kills:
//   - the comparison dropped entirely      → err is nil
//   - the mismatch only logged, not raised → err is nil
//   - the error not naming both IDs        → the two Contains checks fail
func TestParsePlayerResponseRejectsASubstituteVideo(t *testing.T) {
	p := NewPlayerAPI(nil, noopLogger{})
	data := decodePlayerJSON(t, `{
        "playabilityStatus": {"status": "OK"},
        "videoDetails": {"videoId": "OTHERvideo1", "title": "Substitute Video", "author": "Other Ch"},
        "streamingData": {"adaptiveFormats": [
            {"itag": 137, "url": "https://example.com/v", "mimeType": "video/mp4; codecs=\"avc1.640028\"", "width": 1920, "height": 1080},
            {"itag": 140, "url": "https://example.com/a", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\""}
        ]}
    }`)

	info, err := p.parsePlayerResponse(context.Background(), data, "", nil, "REQUESTEDvid")
	if err == nil {
		t.Fatalf("parsePlayerResponse accepted a substitute video: %+v", info)
	}
	var mm *VideoIDMismatchError
	if !errors.As(err, &mm) {
		t.Fatalf("err = %T (%v), want *VideoIDMismatchError", err, err)
	}
	if mm.Requested != "REQUESTEDvid" || mm.Got != "OTHERvideo1" {
		t.Errorf("mismatch = %+v, want Requested REQUESTEDvid / Got OTHERvideo1", mm)
	}
	if !strings.Contains(err.Error(), "REQUESTEDvid") || !strings.Contains(err.Error(), "OTHERvideo1") {
		t.Errorf("error text %q names neither both IDs", err.Error())
	}
	if info != nil {
		t.Errorf("a rejected response still returned info: %+v", info)
	}
}

// TestParsePlayerResponseKeepsAResponseWithNoVideoID pins upstream's EFFECTIVE
// rule, which Moombox matches. _invalid_player_response returns the id rather
// than a bool (_video.py:3022-3026) and both call sites test that return for
// truthiness (:3038, :3122), so an absent or empty videoId keeps the response
// upstream too. Go has no truthiness, so the guard spells it out.
//
// It is load-bearing: TV is this cascade's playability AUTHORITY and some of
// its refusals arrive as a playabilityStatus with no videoDetails at all, so
// the members-only verdict every downstream error string is built from travels
// on exactly the shape this test pins.
//
// Mutants this kills:
//   - "absent-is-mismatch" — the literal `got != requestedVideoID` a future
//     porter might write believing it re-aligns with upstream → err non-nil
//     here, and the members-only verdict path breaks with it
//   - skipping the check when the response carries a DIFFERENT non-empty id
//     → caught by TestParsePlayerResponseRejectsASubstituteVideo above
func TestParsePlayerResponseKeepsAResponseWithNoVideoID(t *testing.T) {
	p := NewPlayerAPI(nil, noopLogger{})

	noDetails := decodePlayerJSON(t, `{"playabilityStatus": {"status": "LOGIN_REQUIRED", "reason": "Join this channel to get access"}}`)
	info, err := p.parsePlayerResponse(context.Background(), noDetails, "", nil, "REQUESTEDvid")
	if err != nil {
		t.Fatalf("a members-only verdict with no videoDetails was rejected: %v", err)
	}
	if info.PlayabilityError != PlayabilityMembersOnly {
		t.Errorf("PlayabilityError = %q, want members_only — the verdict must survive", info.PlayabilityError)
	}

	emptyID := decodePlayerJSON(t, `{"playabilityStatus": {"status": "OK"}, "videoDetails": {"videoId": "", "title": "t"}}`)
	if _, err := p.parsePlayerResponse(context.Background(), emptyID, "", nil, "REQUESTEDvid"); err != nil {
		t.Fatalf("an empty videoId was treated as a mismatch: %v", err)
	}

	matching := decodePlayerJSON(t, `{"playabilityStatus": {"status": "OK"}, "videoDetails": {"videoId": "REQUESTEDvid", "title": "t"}}`)
	if _, err := p.parsePlayerResponse(context.Background(), matching, "", nil, "REQUESTEDvid"); err != nil {
		t.Fatalf("a matching videoId was rejected: %v", err)
	}
}

// TestParseFormatsSkipsDRMAndKeepsTrackIdentity pins both halves of upstream's
// format identity. DRM formats are dropped at parse — yt-dlp reports them as
// skipped (_video.py:3418-3426, the tv-client DRM experiment, issue #12563)
// and YoutubeDL.py:2930 filters them out — because muxing encrypted samples
// produces an unplayable archive. The three track fields are kept because
// they are two thirds of upstream's stream identity.
//
// Mutants this kills:
//   - drmFamilies ignored        → the DRM itag 137 survives
//   - audioTrack not parsed      → AudioTrackID is ""
//   - isDrc not parsed           → IsDrc is false for the DRC entry
func TestParseFormatsSkipsDRMAndKeepsTrackIdentity(t *testing.T) {
	p := NewPlayerAPI(nil, noopLogger{})
	sd := decodePlayerJSON(t, `{"adaptiveFormats": [
		{"itag": 137, "url": "https://tv/v-drm", "mimeType": "video/mp4; codecs=\"avc1.640028\"", "width": 1920, "height": 1080, "drmFamilies": ["WIDEVINE"]},
		{"itag": 136, "url": "https://tv/v-clean", "mimeType": "video/mp4; codecs=\"avc1.4d401f\"", "width": 1280, "height": 720},
		{"itag": 140, "url": "https://tv/a-orig", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\"", "audioTrack": {"id": "en.4", "displayName": "English original", "audioIsDefault": true}},
		{"itag": 140, "url": "https://tv/a-drc", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\"", "isDrc": true, "audioTrack": {"id": "en.4", "displayName": "English original", "audioIsDefault": true}}
	]}`)

	formats := p.parseFormats(sd)

	for _, f := range formats {
		if f.Itag == 137 {
			t.Errorf("a DRM-protected format survived parse: %+v", f)
		}
	}
	var orig, drc *Format
	for i := range formats {
		switch {
		case formats[i].Itag == 140 && formats[i].IsDrc:
			drc = &formats[i]
		case formats[i].Itag == 140:
			orig = &formats[i]
		}
	}
	if orig == nil || drc == nil {
		t.Fatalf("want both itag-140 renditions, got %+v", formats)
	}
	if orig.AudioTrackID != "en.4" || orig.AudioTrackName != "English original" || !orig.AudioIsDefault {
		t.Errorf("track fields = %+v, want id en.4 / name \"English original\" / default true", orig)
	}
	if drc.IsDrc != true {
		t.Errorf("the isDrc rendition parsed with IsDrc=false: %+v", drc)
	}
}

// TestParseFormatsWarnsOnceForSkippedDRM pins the LEVEL and the CARDINALITY of
// the DRM-skip report. Upstream reports it with
// `self.report_warning(msg, video_id, only_once=True)` (_video.py:3419-3427),
// and a silently DRM-stripped pool is the difference between "this video has
// no 1080p" and "this ACCOUNT gets no 1080p" — the operator has to see it at
// the default level, once, with the count.
//
// Mutants this kills:
//   - the line demoted back to Debug  → no Warn captured
//   - one line per dropped format     → two Warns instead of one
//   - the count dropped from the line → the count assertion fails
func TestParseFormatsWarnsOnceForSkippedDRM(t *testing.T) {
	lg := &warnCapturingLogger{}
	p := NewPlayerAPI(nil, lg)
	sd := decodePlayerJSON(t, `{"adaptiveFormats": [
		{"itag": 137, "url": "https://tv/v-drm", "mimeType": "video/mp4; codecs=\"avc1.640028\"", "drmFamilies": ["WIDEVINE"]},
		{"itag": 248, "url": "https://tv/v-drm2", "mimeType": "video/webm; codecs=\"vp9\"", "drmFamilies": ["PLAYREADY"]},
		{"itag": 136, "url": "https://tv/v-clean", "mimeType": "video/mp4; codecs=\"avc1.4d401f\""}
	]}`)

	if got := len(p.parseFormats(sd)); got != 1 {
		t.Fatalf("parseFormats kept %d formats, want only the clean one", got)
	}
	if len(lg.warns) != 1 {
		t.Fatalf("warns = %q, want exactly one DRM line for the whole response", lg.warns)
	}
	if !strings.Contains(lg.warns[0], "DRM") {
		t.Errorf("the Warn %q does not say what was skipped", lg.warns[0])
	}
	if !strings.Contains(lg.warns[0], "2") {
		t.Errorf("the Warn %q does not name the count of skipped formats", lg.warns[0])
	}
}

// TestDeduplicateFormatsKeysOnUpstreamsStreamIdentity pins get_stream_id
// (_video.py:3396-3397). Keying on itag alone silently discards every dubbed
// track but the first-listed one — the more reachable half of the row,
// because both entries come from the SAME client at the SAME auth level.
//
// Mutants this kills:
//   - keying on itag alone       → only one itag-140 survives
//   - dropping isDrc from the key → the DRC rendition evicts the clean one
//   - dropping audioTrackID      → the ja dub evicts the en original
func TestDeduplicateFormatsKeysOnUpstreamsStreamIdentity(t *testing.T) {
	lvl := AuthLevelTVAuth
	mk := func(track string, drc bool, url string) Format {
		return Format{Itag: 140, URL: url, MimeType: "audio/mp4; codecs=\"mp4a.40.2\"",
			AudioTrackID: track, IsDrc: drc, Source: "tv_auth", AuthLevel: &lvl}
	}
	pool := []Format{
		mk("en.4", false, "https://x/en"),
		mk("ja.3", false, "https://x/ja"),
		mk("en.4", true, "https://x/en-drc"),
	}

	got := deduplicateFormats(pool)
	if len(got) != 3 {
		t.Fatalf("dedup kept %d of 3 distinct streams: %+v", len(got), got)
	}
	seen := map[string]bool{}
	for _, f := range got {
		seen[f.AudioTrackID+"/"+fmt.Sprint(f.IsDrc)] = true
	}
	for _, want := range []string{"en.4/false", "ja.3/false", "en.4/true"} {
		if !seen[want] {
			t.Errorf("dedup dropped the %s rendition: %+v", want, got)
		}
	}
}
