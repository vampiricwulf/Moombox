package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/vampiricwulf/Moombox/internal/cookies"
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

	result := deduplicateFormats(context.Background(), pool)

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

	got, _ := pa.parseFormats(context.Background(), streamingData)

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

	got, _ := pa.parseFormats(context.Background(), streamingData)
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

	result := deduplicateFormats(context.Background(), pool)
	if len(result) != 1 {
		t.Fatalf("expected 1 deduplicated format, got %d", len(result))
	}
	if result[0].Source != "adaptive" {
		t.Errorf("expected first-inserted (adaptive) to win same-auth tiebreak, got %q", result[0].Source)
	}
}

func TestDeduplicateFormats_EmptyPool(t *testing.T) {
	result := deduplicateFormats(context.Background(), nil)
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

// TestVideoIDMismatchErrorBoundsTheSubstituteID is close-review Finding 4.
// Got comes straight off the wire (videoDetails.videoId) and was rendered
// unbounded: a 14,000-byte id produced a 22,129-byte Error(), which reaches
// the job's `error` column through `full fetch failed: %w` on the confirmatory
// fetch and the IP-block verdict's `substitute` log field. A real substitute
// id is 11 characters.
//
// The FIELD stays intact — a caller comparing or logging it deliberately gets
// the whole thing; only the rendered text is bounded, and on a RUNE boundary
// so the %q output stays valid UTF-8.
//
// Mutants this kill:
//   - the cap removed from Error()            → the length check fails
//   - the cap applied as a byte slice         → the UTF-8 check fails
//   - the cap applied to the Got field itself → the "field intact" check fails
func TestVideoIDMismatchErrorBoundsTheSubstituteID(t *testing.T) {
	huge := strings.Repeat("あ", 14000) // 42,000 bytes of three-byte runes
	mm := &VideoIDMismatchError{Requested: "test1234567", Got: huge}

	got := mm.Error()
	if len(got) > 300 {
		t.Errorf("Error() is %d bytes for a %d-byte substitute id, want a bounded string", len(got), len(huge))
	}
	// Asserted on the CAPPED ID, not on Error()'s output: %q escapes an
	// invalid byte as \xe3, which is itself valid UTF-8, so the rendered
	// string can never catch a byte-sliced cut.
	if !utf8.ValidString(capSubstituteID(huge)) {
		t.Errorf("capSubstituteID = %q is not valid UTF-8 — the cut must fall on a rune boundary", capSubstituteID(huge))
	}
	if strings.Contains(got, `\x`) {
		t.Errorf("Error() = %q carries an escaped invalid byte — the cut split a rune", got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("Error() = %q, want the truncation marked", got)
	}
	if !strings.Contains(got, "test1234567") {
		t.Errorf("Error() = %q, want the REQUESTED id named in full", got)
	}
	if mm.Got != huge {
		t.Errorf("Got was truncated in place (%d bytes) — the field is the caller's, only the text is bounded", len(mm.Got))
	}

	// An ordinary 11-character id is untouched.
	short := (&VideoIDMismatchError{Requested: "test1234567", Got: "OTHERvideo1"}).Error()
	if !strings.Contains(short, `"OTHERvideo1"`) || strings.Contains(short, "…") {
		t.Errorf("Error() = %q, want a real substitute id rendered whole", short)
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
// skipped (_video.py:3418-3428, the tv-client DRM experiment, issue #12563)
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

	formats, _ := p.parseFormats(context.Background(), sd)

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
// `self.report_warning(msg, video_id, only_once=True)` (_video.py:3420-3428,
// the call itself on 3428),
// and a silently DRM-stripped pool is the difference between "this video has
// no 1080p" and "this ACCOUNT gets no 1080p" — the operator has to see it at
// the default level, once, with the count.
//
// This is the single-response half: one parse, one line, carrying that
// response's count. TestDRMWarnIsOncePerExtractionNotPerResponse pins the
// other half — several responses in one cascade still get one line.
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

	kept, _ := p.parseFormats(context.Background(), sd)
	if got := len(kept); got != 1 {
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
// (_video.py:3396-3397) through the collapsed output. The 3-part key is what
// keeps every rendition ALIVE long enough to be ranked: the auth-level
// tie-break only applies WITHIN one stream identity, so a dubbed or DRC
// rendition from a lower-auth client must not evict the original that only a
// higher-auth client returned. Here the es.3 dub and the DRC twin come from
// tv (auth 1) and the English original from web (auth 5) — the arrangement
// that makes each part of the key observable after the collapse.
//
// Mutants this kills:
//   - keying on itag alone        → the tv es.3 dub wins the auth tie-break
//     before anything ranks the tracks, and the original never reaches the
//     collapse
//   - dropping audioTrackID       → the two CLEAN renditions collide (es.3 from
//     tv, en.4 from web), tv wins that key on auth, and the collapse then ranks
//     the surviving es.3 dub against tv's en.4 DRC twin — which the track score
//     prefers, so itag 140 resolves to https://x/en-drc. Measured; the doc used
//     to say the dub won (close-review Finding 13d)
//   - dropping isDrc from the key → tv's DRC twin evicts the clean original
func TestDeduplicateFormatsKeysOnUpstreamsStreamIdentity(t *testing.T) {
	tv, web := AuthLevelTVAuth, AuthLevelWeb
	mk := func(track, name string, def, drc bool, lvl *int, source, url string) Format {
		return Format{Itag: 140, URL: url, MimeType: "audio/mp4; codecs=\"mp4a.40.2\"",
			AudioTrackID: track, AudioTrackName: name, AudioIsDefault: def, IsDrc: drc,
			Source: source, AuthLevel: lvl}
	}
	pool := []Format{
		mk("es.3", "Spanish", true, false, &tv, "tv_auth", "https://x/es"),
		mk("en.4", "English original", false, true, &tv, "tv_auth", "https://x/en-drc"),
		mk("en.4", "English original", false, false, &web, "web", "https://x/en"),
	}

	got := deduplicateFormats(context.Background(), pool)
	if len(got) != 1 {
		t.Fatalf("itag 140 must reach the consumers once, got %d rows: %+v", len(got), got)
	}
	if got[0].URL != "https://x/en" || got[0].IsDrc {
		t.Errorf("itag 140 resolved to %q (track %q, drc=%v), want the clean en.4 original https://x/en",
			got[0].URL, got[0].AudioTrackID, got[0].IsDrc)
	}
}

// dubbedPool is the real-shaped auto-dubbed response the collapse tests read:
// two video itags plus itag 140 in four renditions (a dubbed default, a
// descriptive track, the English original and the original's DRC twin) and
// itag 251 in two, all from ONE client at ONE auth level — which is how
// YouTube actually serves an auto-dubbed video. The dub carries the higher
// bitrate on purpose: it is what wins every itag-keyed lookup downstream when
// nothing ranks the tracks.
func dubbedPool() []Format {
	lvl := AuthLevelTVAuth
	mk := func(itag int, mime, track, name string, def, drc bool, bitrate int, url string) Format {
		return Format{Itag: itag, URL: url, MimeType: mime, Bitrate: bitrate,
			AudioTrackID: track, AudioTrackName: name, AudioIsDefault: def, IsDrc: drc,
			Source: "tv_auth", AuthLevel: &lvl}
	}
	const aac = "audio/mp4; codecs=\"mp4a.40.2\""
	const opus = "audio/webm; codecs=\"opus\""
	w, h := 1920, 1080
	return []Format{
		{Itag: 137, URL: "https://r1/v137", MimeType: "video/mp4; codecs=\"avc1.640028\"",
			Bitrate: 4000000, Width: &w, Height: &h, Source: "tv_auth", AuthLevel: &lvl},
		mk(140, aac, "es.3", "Spanish", true, false, 131000, "https://r1/a140-es"),
		mk(140, aac, "en.10", "English descriptive", false, false, 130000, "https://r1/a140-en-desc"),
		mk(140, aac, "en.4", "English original", false, false, 129000, "https://r1/a140-en"),
		mk(140, aac, "en.4", "English original", false, true, 130500, "https://r1/a140-en-drc"),
		mk(251, opus, "en.4", "English original", false, false, 141000, "https://r1/a251-en"),
		mk(251, opus, "es.3", "Spanish", true, false, 144000, "https://r1/a251-es"),
	}
}

// TestDeduplicateFormatsCollapsesEachItagToThePreferredRendition pins the
// invariant every itag-keyed consumer depends on: after dedup, ONE rendition
// per itag reaches VideoInfo.Formats, and it is the one the audio-track
// preference would choose. internal/worker looks formats up by itag alone —
// SelectBestDashStream picks by bandwidth among same-itag entries, and
// resolveFormatURLByItag returns the FIRST entry of that itag at setup and on
// every 403 credential refresh — so a pool holding several renditions of one
// itag lets the live path archive the dub, or splice a second language into a
// file already half written. Upstream keeps all of them and carries the
// identity into its format ids; Moombox collapses instead, because its
// consumers key on the itag.
//
// Mutants this kills:
//   - no collapse at all               → 4 itag-140 rows survive
//   - collapse keeps the first-listed  → the en.10 descriptive rendition wins
//     itag 140 (the 3-part sort orders the renditions by track id, so
//     "first" is en.10, not the dub)
//   - collapse ranks by bandwidth, as SelectBestDashStream does → the es.3
//     dub wins itag 140 and itag 251, which is the downstream bug itself
//
// The DRC rung IS observable at this layer (close-review Finding 13a): the
// collapse ranks by audioTrackScore, which scores a clean rendition 20 against
// its DRC twin's 19, so there is no tie and the penalty decides. The sentence
// that used to stand here claimed the opposite on both halves. The dedicated
// pin lives in TestSelectBestAudioPrefersTheOriginalNonDRCTrack, where the
// penalty is the only thing under test.
func TestDeduplicateFormatsCollapsesEachItagToThePreferredRendition(t *testing.T) {
	got := deduplicateFormats(context.Background(), dubbedPool())

	perItag := map[int][]Format{}
	var order []int
	for _, f := range got {
		if _, seen := perItag[f.Itag]; !seen {
			order = append(order, f.Itag)
		}
		perItag[f.Itag] = append(perItag[f.Itag], f)
	}
	for _, itag := range order {
		if n := len(perItag[itag]); n != 1 {
			t.Errorf("itag %d reaches the consumers as %d renditions, want exactly 1: %+v", itag, n, perItag[itag])
		}
	}
	for itag, wantURL := range map[int]string{140: "https://r1/a140-en", 251: "https://r1/a251-en"} {
		if len(perItag[itag]) == 0 {
			t.Fatalf("itag %d vanished: %+v", itag, got)
		}
		// The FIRST entry of the itag is what resolveFormatURLByItag returns,
		// so the preferred rendition has to be that one.
		if first := perItag[itag][0]; first.URL != wantURL {
			t.Errorf("itag %d resolves to %q (track %q, drc=%v), want the clean original %q",
				itag, first.URL, first.AudioTrackName, first.IsDrc, wantURL)
		}
	}
	if len(perItag[137]) != 1 || perItag[137][0].URL != "https://r1/v137" {
		t.Errorf("the video itag must pass through untouched: %+v", perItag[137])
	}
}

// TestDeduplicateFormatsLeavesAnOrdinaryPoolUnchanged is the differential pin
// for the collapse: a response with no audioTrack and no isDrc anywhere — every
// ordinary video — must come out of dedup exactly as it did before the collapse
// existed. One row per itag, the lowest auth level winning each itag, sorted by
// itag ascending. The collapse can only ever fire where the 3-part key already
// produced two rows for one itag, which needs a track id or a DRC flag.
//
// Mutants this kills:
//   - the collapsed rows emitted in map order → the itag order breaks
//   - the collapse running before the auth tie-break → the WEB row wins itag 140
//   - the collapse dropping a distinct itag    → fewer than 3 rows
func TestDeduplicateFormatsLeavesAnOrdinaryPoolUnchanged(t *testing.T) {
	tv, web, vr := AuthLevelTVAuth, AuthLevelWeb, AuthLevelAndroidVR
	pool := []Format{
		{Itag: 248, URL: "https://web/v248", MimeType: "video/webm; codecs=\"vp9\"", Source: "web", AuthLevel: &web},
		{Itag: 140, URL: "https://web/a140", MimeType: "audio/mp4; codecs=\"mp4a.40.2\"", Source: "web", AuthLevel: &web},
		{Itag: 140, URL: "https://tv/a140", MimeType: "audio/mp4; codecs=\"mp4a.40.2\"", Source: "tv_auth", AuthLevel: &tv},
		{Itag: 137, URL: "https://vr/v137", MimeType: "video/mp4; codecs=\"avc1.640028\"", Source: "android_vr", AuthLevel: &vr},
		{Itag: 137, URL: "https://web/v137", MimeType: "video/mp4; codecs=\"avc1.640028\"", Source: "web", AuthLevel: &web},
	}

	got := deduplicateFormats(context.Background(), pool)

	want := []struct {
		itag int
		url  string
	}{{137, "https://web/v137"}, {140, "https://tv/a140"}, {248, "https://web/v248"}}
	if len(got) != len(want) {
		t.Fatalf("dedup returned %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Itag != w.itag || got[i].URL != w.url {
			t.Errorf("row %d = itag %d %q, want itag %d %q (the pre-collapse output, row for row)",
				i, got[i].Itag, got[i].URL, w.itag, w.url)
		}
	}
}

// TestParsePlayerResponseKeepsPostLiveWhenEveryFormatIsDRM pins the
// classification input. Dropping DRM formats at parse changed what
// classifyStream is told: a finished broadcast whose formats are ALL DRM now
// reports zero formats, and `!hasFormats && lbd != nil` returns upcoming. The
// authenticated cascade absorbs that (an empty pool falls through to the next
// client), but ProbeVideoStatusAuthenticated is ONE tv fetch with no fallback
// and it is what the waiting-room poller reads — so the account the yt-dlp
// #12563 experiment hits would wait forever on a stream that already ended.
// The response HAD formats; classification must be told so.
//
// Mutants this kills:
//   - classifying on the post-filter count (len(formats) > 0) → upcoming
func TestParsePlayerResponseKeepsPostLiveWhenEveryFormatIsDRM(t *testing.T) {
	p := NewPlayerAPI(nil, noopLogger{})
	data := decodePlayerJSON(t, `{
		"playabilityStatus": {"status": "OK"},
		"videoDetails": {"videoId": "DRMonlyVid1", "title": "Ended Broadcast", "author": "Ch", "isLiveContent": true},
		"microformat": {"playerMicroformatRenderer": {"liveBroadcastDetails": {
			"isLiveNow": false, "startTimestamp": "2026-09-16T10:00:00+00:00", "endTimestamp": "2026-09-16T12:30:00+00:00"}}},
		"streamingData": {"adaptiveFormats": [
			{"itag": 137, "url": "https://tv/v", "mimeType": "video/mp4; codecs=\"avc1.640028\"", "drmFamilies": ["WIDEVINE"]},
			{"itag": 140, "url": "https://tv/a", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\"", "drmFamilies": ["WIDEVINE"]}
		]}
	}`)

	info, err := p.parsePlayerResponse(context.Background(), data, "", nil, "DRMonlyVid1")
	if err != nil {
		t.Fatalf("parsePlayerResponse: %v", err)
	}
	if len(info.Formats) != 0 {
		t.Fatalf("the DRM formats must still be dropped, got %+v", info.Formats)
	}
	if info.StreamStatus != StreamPostLive || !info.IsPostLiveDVR || info.IsUpcoming {
		t.Errorf("status = %q (postLiveDVR=%v upcoming=%v), want post_live — a DRM-only response still HAD formats",
			info.StreamStatus, info.IsPostLiveDVR, info.IsUpcoming)
	}
}

// TestParsePlayerResponseKeepsPostLiveWhenEveryFormatIsSABR is the URL-less
// half of the rule the DRM test above pins. A client YouTube has forced onto
// SABR returns formats with neither `url` nor `signatureCipher` — the response
// still PROVES the broadcast has media, but every entry is dropped, so
// classifying on the post-filter count turns a finished stream into
// `upcoming`. That is the same stall for the same reason: the single-client
// ProbeVideoStatusAuthenticated path has no fallback, and the waiting-room
// poller would wait forever on a stream that already ended.
//
// Mutants this kills:
//   - classifying on len(formats) alone, i.e. URLlessFormats not folded into
//     the hasFormats predicate → upcoming
func TestParsePlayerResponseKeepsPostLiveWhenEveryFormatIsSABR(t *testing.T) {
	p := NewPlayerAPI(nil, noopLogger{})
	data := decodePlayerJSON(t, `{
		"playabilityStatus": {"status": "OK"},
		"videoDetails": {"videoId": "SABRonlyVid", "title": "Ended Broadcast", "author": "Ch", "isLiveContent": true},
		"microformat": {"playerMicroformatRenderer": {"liveBroadcastDetails": {
			"isLiveNow": false, "startTimestamp": "2026-09-16T10:00:00+00:00", "endTimestamp": "2026-09-16T12:30:00+00:00"}}},
		"streamingData": {
			"serverAbrStreamingUrl": "https://rr1---sn-x.googlevideo.com/videoplayback?...",
			"adaptiveFormats": [
				{"itag": 137, "mimeType": "video/mp4; codecs=\"avc1.640028\"", "width": 1920, "height": 1080},
				{"itag": 140, "mimeType": "audio/mp4; codecs=\"mp4a.40.2\""}
			]
		}
	}`)

	info, err := p.parsePlayerResponse(context.Background(), data, "", nil, "SABRonlyVid")
	if err != nil {
		t.Fatalf("parsePlayerResponse: %v", err)
	}
	if len(info.Formats) != 0 {
		t.Fatalf("the URL-less formats must still be dropped, got %+v", info.Formats)
	}
	if info.StreamStatus != StreamPostLive || !info.IsPostLiveDVR || info.IsUpcoming {
		t.Errorf("status = %q (postLiveDVR=%v upcoming=%v), want post_live — a SABR-forced response still HAD formats",
			info.StreamStatus, info.IsPostLiveDVR, info.IsUpcoming)
	}
}

// drmOKBody is an otherwise-adequate player response whose pool also carries a
// DRM-protected rendition — the shape an account in YouTube's tv-client DRM
// experiment gets back from MOST clients in a cascade (yt-dlp issue #12563).
const drmOKBody = `{
	"playabilityStatus": {"status": "OK"},
	"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a"},
	"streamingData": {"adaptiveFormats": [
		{"itag": 299, "url": "https://example.com/v", "mimeType": "video/mp4; codecs=\"avc1.64002a\"", "width": 1920, "height": 1080},
		{"itag": 140, "url": "https://example.com/a", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\""},
		{"itag": 137, "url": "https://example.com/drm", "mimeType": "video/mp4; codecs=\"avc1.640028\"", "drmFamilies": ["WIDEVINE"]}
	]}
}`

// TestDRMWarnIsOncePerExtractionNotPerResponse pins the DRM-skip report to
// upstream's cardinality: `report_warning(..., only_once=True)`
// (_video.py:3420-3428) fires ONCE per run, not once per player response. This
// cascade parses several responses per extraction, and for an account in the
// tv-client DRM experiment nearly all of them carry DRM formats — a per-
// response Warn turns one diagnosis into a repeating wall at the default log
// level.
//
// Both responses here really are parsed: the watch page's own player response
// and the TV client's, which is why the transport call assertion is part of the
// test — without it a cascade that gave up after the watch page would pass
// vacuously.
//
// Mutants this kills:
//   - the Warn left per response → two DRM lines from one cascade
//   - the dedupe flag hoisted to a package-level var or onto PlayerAPI, i.e.
//     made per-process instead of per-extraction → the second cascade stays
//     silent
func TestDRMWarnIsOncePerExtractionNotPerResponse(t *testing.T) {
	origFetch := fetchWatchPage
	fetchWatchPage = func(context.Context, string, string) (*WatchPageResult, error) {
		return &WatchPageResult{
			Ytcfg:          DefaultYtcfg(),
			PlayerResponse: decodePlayerJSON(t, drmOKBody),
		}, nil
	}
	t.Cleanup(func() { fetchWatchPage = origFetch })

	tr := &clientKeyedTransport{responses: map[string]struct {
		status int
		body   string
	}{
		"7": {http.StatusOK, drmOKBody}, // TV_DOWNGRADED
	}}
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })

	lg := &warnCapturingLogger{}
	p := NewPlayerAPI(NewAuth(cookies.NewCookieJar(), noopLogger{}), lg)

	drmWarns := func() []string {
		var out []string
		for _, w := range lg.warns {
			if strings.Contains(w, "DRM") {
				out = append(out, w)
			}
		}
		return out
	}

	if _, err := p.GetVideoInfoPublic(context.Background(), "test1234567"); err != nil {
		t.Fatalf("cascade failed before the cardinality could be judged: %v", err)
	}
	if !slices.Contains(tr.calls, "7") {
		t.Fatalf("the TV client was never asked (calls = %v), so only one response was parsed", tr.calls)
	}
	if got := drmWarns(); len(got) != 1 {
		t.Fatalf("one extraction logged %d DRM lines, want exactly 1: %q", len(got), got)
	}

	// A second extraction is a second run: it must report again, or an
	// operator only ever learns about the DRM experiment once per process.
	if _, err := p.GetVideoInfoPublic(context.Background(), "test1234567"); err != nil {
		t.Fatalf("second cascade failed: %v", err)
	}
	if got := drmWarns(); len(got) != 2 {
		t.Fatalf("two extractions logged %d DRM lines, want exactly 2: %q", len(got), got)
	}
}

// TestParsePlayabilityStatusRecognisesEveryAgeGateShape ports yt-dlp's
// _is_agegated (_video.py:2894-2904). The bypass gate downstream
// (player_api_strategy.go:382 and :557) matches PlayabilityAgeRestricted
// LITERALLY, so an "unknown" verdict never reaches the web_embedded age path
// at all.
//
// The reason substrings are the load-bearing half: upstream's status entries
// are lower-case and substring-matched against the raw upper-case `status`, so
// upstream in practice recognises these responses through their REASON.
//
// Mutants this kills:
//   - the AGE_CHECK_REQUIRED arm dropped        → the reason-less row reports "unknown"
//   - the reason substrings dropped             → the inappropriate/confirm rows report "unknown"
//   - desktopLegacyAgeGateReason not consulted  → that row reports "unknown"
//   - the `statusCode != "OK"` guard dropped, letting the age block preempt
//     the OK arm                                → the two OK rows report "age_restricted",
//     i.e. a playable response aborts the job
//   - hasDesktopLegacyAgeGate's default arm calling an empty container truthy
//     → the empty-map/empty-list rows report "age_restricted"
//   - the age match hoisted above the upcoming check → the synthetic ordering
//     row reports "age_restricted"
//
// The brief's other ordering half — "the age match placed above the
// members-only arm" — describes the SHIPPED code and so can never be a mutant:
// the members-only tests live inside `case "LOGIN_REQUIRED"` / `case
// "UNPLAYABLE"`, already below the age block. The four regression rows at the
// end are guards rather than mutant-killers for that half: none of the three
// substrings occurs in YouTube's membership ("Join this channel to get access
// to members-only content") or waiting-room text, and that disjointness is
// what makes the placement safe. They are what would fail if a future edit
// widened the substring list far enough to overlap either message (a list
// carrying "content", for one) or YouTube reworded them.
func TestParsePlayabilityStatusRecognisesEveryAgeGateShape(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		want   PlayabilityError
	}{
		{"AGE_CHECK_REQUIRED", `{"status": "AGE_CHECK_REQUIRED", "reason": "Sign in to confirm your age"}`, PlayabilityAgeRestricted},
		// Added to the brief's table: with a reason present, the substring half
		// already catches AGE_CHECK_REQUIRED, so the status arm's own mutant
		// ("the AGE_CHECK_REQUIRED arm dropped") survives every other row. This
		// reason-less shape is the one that reaches the status switch, and
		// YouTube does serve the code with no reason text on some clients.
		{"AGE_CHECK_REQUIRED without a reason", `{"status": "AGE_CHECK_REQUIRED"}`, PlayabilityAgeRestricted},
		{"AGE_VERIFICATION_REQUIRED", `{"status": "AGE_VERIFICATION_REQUIRED", "reason": "This video may be inappropriate for some users."}`, PlayabilityAgeRestricted},
		{"UNPLAYABLE inappropriate", `{"status": "UNPLAYABLE", "reason": "This video may be inappropriate for some users."}`, PlayabilityAgeRestricted},
		{"UNPLAYABLE age-restricted", `{"status": "UNPLAYABLE", "reason": "This video is age-restricted and can only be watched on YouTube."}`, PlayabilityAgeRestricted},
		{"LOGIN_REQUIRED confirm your age", `{"status": "LOGIN_REQUIRED", "reason": "Sign in to confirm your age"}`, PlayabilityAgeRestricted},
		{"desktopLegacyAgeGateReason", `{"status": "UNPLAYABLE", "reason": "", "desktopLegacyAgeGateReason": 1}`, PlayabilityAgeRestricted},

		// A response YouTube itself marks PLAYABLE is not an error, whatever
		// age markers ride along with it. checkPlayability
		// (internal/worker/stream_processor.go) aborts the job for every
		// non-ok verdict, and the age_restricted arm suppresses the
		// notification — so reclassifying an OK response ends a downloadable
		// stream in silence. Upstream never creates the conflict: _is_agegated
		// only ever APPENDS clients (_video.py:3157-3175), it does not
		// override a playability verdict.
		{"OK with a legacy age gate stays ok", `{"status": "OK", "desktopLegacyAgeGateReason": 1}`, PlayabilityOK},
		{"OK with an age-flavoured reason stays ok", `{"status": "OK", "reason": "This video may be inappropriate for some users."}`, PlayabilityOK},

		// An empty container is FALSY in Python, so upstream's truthiness test
		// does not see an age gate here either.
		{"empty-map legacy gate is not a gate", `{"status": "UNPLAYABLE", "reason": "Playback on other websites has been disabled", "desktopLegacyAgeGateReason": {}}`, PlayabilityUnknown},
		{"empty-list legacy gate is not a gate", `{"status": "UNPLAYABLE", "reason": "Playback on other websites has been disabled", "desktopLegacyAgeGateReason": []}`, PlayabilityUnknown},

		// Synthetic — NOT a YouTube-observed reason. It pins the evaluation
		// ORDER contract rather than a wire shape: a waiting room is never an
		// error, even if its reason text ever carried an age substring.
		{"upcoming wins over an age substring (synthetic)", `{"status": "LIVE_STREAM_OFFLINE", "reason": "This live event may be inappropriate for some users"}`, PlayabilityOK},

		// Regressions the new match must NOT cause.
		{"members only stays members only", `{"status": "LOGIN_REQUIRED", "reason": "Join this channel to get access to members-only content"}`, PlayabilityMembersOnly},
		{"upcoming stays ok", `{"status": "LIVE_STREAM_OFFLINE", "reason": "Premieres in 3 hours"}`, PlayabilityOK},
		{"private stays private", `{"status": "UNPLAYABLE", "reason": "This video is private."}`, PlayabilityPrivate},
		{"plain unplayable stays unknown", `{"status": "UNPLAYABLE", "reason": "Playback on other websites has been disabled"}`, PlayabilityUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := parsePlayabilityStatus(decodePlayerJSON(t, tc.status))
			if got != tc.want {
				t.Errorf("parsePlayabilityStatus(%s) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

// TestDeduplicateFormatsKeepsATokenFreeShadowOfAWebFamilyWinner pins the
// shadow copy (owner ruling 2026-09-29). yt-dlp decides the GVS token BEFORE
// it deduplicates, so a web_creator format skipped for a missing token never
// shadows the next client's copy; Moombox dedups first and learns about the
// missing token at download time. So dedup keeps the AuthLevel winner exactly
// as before AND, when that winner's client requires a GVS token, the
// lowest-AuthLevel token-free copy of the same stream on TokenFreeAlternate,
// which the VOD missing_pot degrade swaps to.
//
// Mutants this kills:
//   - keep the higher-level token-free loser → the web_embedded row gets visionos
//   - keep an alternate on a token-free winner → the tv_public and token-free-winner rows
//   - no alternate at all → the web_creator + visionos row
func TestDeduplicateFormatsKeepsATokenFreeShadowOfAWebFamilyWinner(t *testing.T) {
	lvl := map[string]int{
		"web_creator":  AuthLevelWebCreator,
		"web":          AuthLevelWeb,
		"web_embedded": AuthLevelWebEmbedded,
		"visionos":     AuthLevelVisionOS,
		"tv_public":    AuthLevelTVPublic,
		"android_vr":   AuthLevelAndroidVR,
	}
	mk := func(source string) Format {
		l := lvl[source]
		return Format{Itag: 302, URL: "https://" + source + "/v302", MimeType: `video/webm; codecs="vp9"`, Source: source, AuthLevel: &l}
	}
	for _, tc := range []struct {
		name       string
		sources    []string
		wantWinner string
		wantAlt    string // "" = no alternate
	}{
		{"web_creator winner, visionos loser", []string{"web_creator", "visionos"}, "web_creator", "visionos"},
		{"visionos listed first still loses and shadows", []string{"visionos", "web_creator"}, "web_creator", "visionos"},
		{"tv_public wins outright and carries no shadow", []string{"web_creator", "tv_public", "visionos"}, "tv_public", ""},
		// web_embedded (6) outranks web_creator (7) outright, so the
		// shadow-slot ranking between token-free losers is only reachable
		// under a lower-level WEB-family winner: web (5).
		{"web_embedded beats visionos for the shadow slot", []string{"web", "visionos", "web_embedded"}, "web", "web_embedded"},
		{"web_embedded first, then visionos", []string{"web", "web_embedded", "visionos"}, "web", "web_embedded"},
		{"displaced token-free winner is then outranked", []string{"visionos", "web", "web_embedded"}, "web", "web_embedded"},
		{"web_creator loses to web_embedded outright", []string{"web_creator", "visionos", "web_embedded"}, "web_embedded", ""},
		{"visionos beats android_vr for the shadow slot", []string{"android_vr", "web_creator", "visionos"}, "web_creator", "visionos"},
		{"a lone web_creator has no shadow", []string{"web_creator"}, "web_creator", ""},
		{"a token-free winner keeps no shadow of a web_creator loser", []string{"web_creator", "web_embedded"}, "web_embedded", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := make([]Format, 0, len(tc.sources))
			for _, s := range tc.sources {
				pool = append(pool, mk(s))
			}
			got := deduplicateFormats(context.Background(), pool)
			if len(got) != 1 {
				t.Fatalf("dedup returned %d rows, want 1: %+v", len(got), got)
			}
			w := got[0]
			if w.Source != tc.wantWinner || w.URL != "https://"+tc.wantWinner+"/v302" {
				t.Errorf("winner = %s %q, want %s — the shadow must never change which copy wins", w.Source, w.URL, tc.wantWinner)
			}
			if tc.wantAlt == "" {
				if w.TokenFreeAlternate != nil {
					t.Errorf("TokenFreeAlternate = %+v, want nil", *w.TokenFreeAlternate)
				}
				return
			}
			alt := w.TokenFreeAlternate
			if alt == nil {
				t.Fatalf("TokenFreeAlternate = nil, want the %s copy", tc.wantAlt)
			}
			if alt.Source != tc.wantAlt || alt.URL != "https://"+tc.wantAlt+"/v302" || alt.Itag != 302 {
				t.Errorf("TokenFreeAlternate = %s %q itag %d, want %s https://%s/v302", alt.Source, alt.URL, alt.Itag, tc.wantAlt, tc.wantAlt)
			}
			if alt.TokenFreeAlternate != nil {
				t.Errorf("the shadow carries a shadow of its own: %+v", *alt.TokenFreeAlternate)
			}
		})
	}
}

// TestDeduplicateFormatsShadowRidesTheCollapsedRendition pins the collapse
// half: the rendition collapseToPreferredRendition keeps carries its own
// shadow, and the collapse count is the same as without shadows (a shadow is
// not a rendition), and the shadow is keyed by the full rendition identity,
// not the itag.
func TestDeduplicateFormatsShadowRidesTheCollapsedRendition(t *testing.T) {
	wc, vo := AuthLevelWebCreator, AuthLevelVisionOS
	const aac = "audio/mp4; codecs=\"mp4a.40.2\""
	mk := func(track, name string, def bool, source string, lvl *int) Format {
		return Format{Itag: 140, URL: "https://" + source + "/a140-" + track, MimeType: aac,
			AudioTrackID: track, AudioTrackName: name, AudioIsDefault: def, Source: source, AuthLevel: lvl}
	}
	pool := []Format{
		mk("es.3", "Spanish", true, "web_creator", &wc),
		mk("en.4", "English original", false, "web_creator", &wc),
		// visionos es.3 listed FIRST: a shadow keyed by itag alone would hand
		// the kept en.4 rendition this es.3 copy.
		mk("es.3", "Spanish", true, "visionos", &vo),
		mk("en.4", "English original", false, "visionos", &vo),
	}
	ctx := withExtractionState(context.Background())
	got := deduplicateFormats(ctx, pool)
	if len(got) != 1 {
		t.Fatalf("dedup returned %d rows, want 1: %+v", len(got), got)
	}
	if got[0].URL != "https://web_creator/a140-en.4" {
		t.Errorf("itag 140 = %q, want the web_creator English original", got[0].URL)
	}
	if alt := got[0].TokenFreeAlternate; alt == nil || alt.URL != "https://visionos/a140-en.4" || alt.AudioTrackID != "en.4" {
		t.Errorf("TokenFreeAlternate = %+v, want the visionos English original (same rendition key, not merely the same itag)", alt)
	}
	if _, collapsed := extractionStateFrom(ctx).poolCounts(); collapsed != 1 {
		t.Errorf("collapsed renditions = %d, want 1 — a shadow is not a rendition", collapsed)
	}
}

// TestFormatJSONOmitsTheTokenFreeAlternate: the shadow is never serialised —
// a resume re-extracts, and a persisted URL would be stale anyway.
func TestFormatJSONOmitsTheTokenFreeAlternate(t *testing.T) {
	alt := Format{Itag: 302, URL: "https://visionos/v302", Source: "visionos"}
	f := Format{Itag: 302, URL: "https://web_creator/v302", Source: "web_creator", TokenFreeAlternate: &alt}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if s := string(b); strings.Contains(s, "visionos") || strings.Contains(strings.ToLower(s), "alternate") {
		t.Errorf("marshalled Format carries the shadow: %s", s)
	}
}

// TestDeduplicateFormatsDropsAStaleShadowOnItsInput: a pool entry can already
// carry a shadow (collectFormats re-labels a result's formats, and a result may
// have been deduplicated once). Dedup recomputes every shadow from the pool it
// is handed: a stale one on a token-free winner is cleared, and a shadow never
// carries a shadow of its own.
//
// Mutants this kills:
//   - not clearing the winner's incoming shadow → the tv_public row keeps it
//   - not clearing the alternate's incoming shadow → the chain row
func TestDeduplicateFormatsDropsAStaleShadowOnItsInput(t *testing.T) {
	wc, vo, tv := AuthLevelWebCreator, AuthLevelVisionOS, AuthLevelTVPublic
	stale := &Format{Itag: 302, URL: "https://stale/v302", Source: "visionos", AuthLevel: &vo}

	t.Run("tv_public winner", func(t *testing.T) {
		pool := []Format{{Itag: 302, URL: "https://tv_public/v302", Source: "tv_public", AuthLevel: &tv, TokenFreeAlternate: stale}}
		got := deduplicateFormats(context.Background(), pool)
		if len(got) != 1 || got[0].TokenFreeAlternate != nil {
			t.Errorf("token-free winner kept a stale shadow: %+v", got)
		}
	})

	t.Run("chain", func(t *testing.T) {
		pool := []Format{
			{Itag: 302, URL: "https://web_creator/v302", Source: "web_creator", AuthLevel: &wc},
			{Itag: 302, URL: "https://visionos/v302", Source: "visionos", AuthLevel: &vo, TokenFreeAlternate: stale},
		}
		got := deduplicateFormats(context.Background(), pool)
		if len(got) != 1 || got[0].TokenFreeAlternate == nil {
			t.Fatalf("want the web_creator winner with a visionos shadow, got %+v", got)
		}
		if alt := got[0].TokenFreeAlternate; alt.URL != "https://visionos/v302" || alt.TokenFreeAlternate != nil {
			t.Errorf("shadow = %q carrying %+v, want https://visionos/v302 with no shadow of its own", alt.URL, alt.TokenFreeAlternate)
		}
		if pool[1].TokenFreeAlternate != stale {
			t.Errorf("dedup rewrote its input's shadow pointer")
		}
	})
}
