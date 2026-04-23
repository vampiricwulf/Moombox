package youtube

import (
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
		{Itag: 137, URL: "https://example.com/v2", AuthLevel: &vrAuth}, // lower auth level
		{Itag: 140, URL: "https://example.com/a1", AuthLevel: &webAuth},
		{Itag: 999, URL: "", AuthLevel: &webAuth}, // no URL, should be filtered
	}

	result := deduplicateFormats(pool)

	if len(result) != 2 {
		t.Fatalf("expected 2 deduplicated formats, got %d", len(result))
	}

	// itag 137 should prefer lower auth level (VR=0 < Web=5)
	for _, f := range result {
		if f.Itag == 137 {
			if *f.AuthLevel != AuthLevelAndroidVR {
				t.Errorf("expected itag 137 to prefer lower auth level (AndroidVR), got %d", *f.AuthLevel)
			}
		}
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
