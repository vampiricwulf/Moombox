package youtube

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestCaptureVisitorData pins the watch-page → service visitor-data hand-off
// shared by the authenticated AND public extraction paths. The public path
// firing it too is load-bearing for anonymous (cookie-less) users: after
// invalidate403Caches clears the service cache, Init() never re-runs
// (startup-only call site), so this callback is the ONLY refill source —
// without it one 403 refresh leaves every later probe visitor-less for the
// process lifetime.
func TestCaptureVisitorData(t *testing.T) {
	t.Run("forwards non-empty visitor data", func(t *testing.T) {
		var got string
		p := &PlayerAPI{OnVisitorData: func(vd string) { got = vd }}

		p.captureVisitorData(&YtcfgData{VisitorData: "vd-abc"})

		if got != "vd-abc" {
			t.Errorf("OnVisitorData got %q, want %q", got, "vd-abc")
		}
	})

	t.Run("empty visitor data does not fire the callback", func(t *testing.T) {
		fired := false
		p := &PlayerAPI{OnVisitorData: func(string) { fired = true }}

		p.captureVisitorData(&YtcfgData{})

		if fired {
			t.Error("OnVisitorData fired for empty visitor data")
		}
	})

	t.Run("nil ytcfg and nil callback are safe no-ops", func(t *testing.T) {
		p := &PlayerAPI{OnVisitorData: func(string) {}}
		p.captureVisitorData(nil) // must not panic

		p = &PlayerAPI{}                                   // nil callback
		p.captureVisitorData(&YtcfgData{VisitorData: "x"}) // must not panic
	})
}

// clientKeyedTransport answers /youtubei player requests with a canned body
// per X-YouTube-Client-Name header and records the call order.
type clientKeyedTransport struct {
	responses map[string]struct {
		status int
		body   string
	}
	calls []string
}

func (tr *clientKeyedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	client := req.Header.Get("X-YouTube-Client-Name")
	tr.calls = append(tr.calls, client)
	r, ok := tr.responses[client]
	if !ok {
		r.status, r.body = http.StatusNotFound, "{}"
	}
	return &http.Response{
		StatusCode: r.status,
		Body:       io.NopCloser(strings.NewReader(r.body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

const adequateOKBody = `{
	"playabilityStatus": {"status": "OK"},
	"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a"},
	"streamingData": {"adaptiveFormats": [
		{"itag": 299, "url": "https://example.com/v", "mimeType": "video/mp4; codecs=\"avc1.64002a\"", "width": 1920, "height": 1080},
		{"itag": 140, "url": "https://example.com/a", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\""}
	]}
}`

const audioOnlyOKBody = `{
	"playabilityStatus": {"status": "OK"},
	"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a"},
	"streamingData": {"adaptiveFormats": [
		{"itag": 140, "url": "https://example.com/a", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\""}
	]}
}`

// TestTryCookielessFallbacks pins the cookieless fallback chain introduced
// for yt-dlp 2026.08.19 parity: VISIONOS (client 101) is tried first, an
// adequate result short-circuits before ANDROID_VR (client 28), and when
// neither is adequate the pool still collects every fetched format at the
// last-resort tiers (VisionOS ahead of AndroidVR).
func TestTryCookielessFallbacks(t *testing.T) {
	swap := func(t *testing.T, tr *clientKeyedTransport) {
		t.Helper()
		orig := apiClient
		apiClient = &http.Client{Transport: tr}
		t.Cleanup(func() { apiClient = orig })
	}
	newAPI := func() *PlayerAPI { return NewPlayerAPI(nil, noopLogger{}) }

	t.Run("visionos adequate short-circuits android_vr", func(t *testing.T) {
		tr := &clientKeyedTransport{responses: map[string]struct {
			status int
			body   string
		}{
			"101": {http.StatusOK, adequateOKBody},
		}}
		swap(t, tr)

		var pool []Format
		res := newAPI().tryCookielessFallbacks(context.Background(), "test1234567", "vd", &pool)
		if res == nil {
			t.Fatal("expected a result from visionos")
		}
		if len(tr.calls) != 1 || tr.calls[0] != "101" {
			t.Errorf("calls = %v, want [101] only", tr.calls)
		}
		if len(pool) != 2 || pool[0].Source != "visionos" || *pool[0].AuthLevel != AuthLevelVisionOS {
			t.Errorf("pool = %+v, want 2 visionos formats at AuthLevelVisionOS", pool)
		}
	})

	t.Run("visionos failure falls through to android_vr", func(t *testing.T) {
		tr := &clientKeyedTransport{responses: map[string]struct {
			status int
			body   string
		}{
			"28": {http.StatusOK, adequateOKBody},
		}}
		swap(t, tr)

		var pool []Format
		res := newAPI().tryCookielessFallbacks(context.Background(), "test1234567", "vd", &pool)
		if res == nil {
			t.Fatal("expected a result from android_vr")
		}
		if len(tr.calls) != 2 || tr.calls[0] != "101" || tr.calls[1] != "28" {
			t.Errorf("calls = %v, want [101 28]", tr.calls)
		}
		if len(pool) != 2 || pool[0].Source != "android_vr" || *pool[0].AuthLevel != AuthLevelAndroidVR {
			t.Errorf("pool = %+v, want 2 android_vr formats at AuthLevelAndroidVR", pool)
		}
	})

	t.Run("neither adequate returns nil but pools formats", func(t *testing.T) {
		tr := &clientKeyedTransport{responses: map[string]struct {
			status int
			body   string
		}{
			"101": {http.StatusOK, audioOnlyOKBody},
			"28":  {http.StatusOK, audioOnlyOKBody},
		}}
		swap(t, tr)

		var pool []Format
		res := newAPI().tryCookielessFallbacks(context.Background(), "test1234567", "vd", &pool)
		if res != nil {
			t.Fatalf("expected nil result, got %+v", res)
		}
		if len(pool) != 2 {
			t.Fatalf("pool has %d formats, want 2 (one per client)", len(pool))
		}
		if *pool[0].AuthLevel != AuthLevelVisionOS || *pool[1].AuthLevel != AuthLevelAndroidVR {
			t.Errorf("pool auth levels = %d, %d; want %d then %d",
				*pool[0].AuthLevel, *pool[1].AuthLevel, AuthLevelVisionOS, AuthLevelAndroidVR)
		}
	})
}

// TestCookielessFallbackDashOnlyWhenNeeded pins the corrected live rule.
// VISIONOS returns no live dashManifestUrl, but its split video+audio
// adaptive formats already route to the manifest-free &sq=N path — the
// primary live path — so there is nothing to chase and the chain must stop.
// It consults the next client ONLY when the result is neither
// DASH-manifested nor split-adaptive. An earlier version always continued,
// costing a needless round trip on every anonymous live extraction.
func TestCookielessFallbackDashOnlyWhenNeeded(t *testing.T) {
	// Split adaptive: URLs present, no contentLength, one video + one audio.
	const liveSplitAdaptive = `{
		"playabilityStatus": {"status": "OK"},
		"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a", "isLive": true},
		"streamingData": {"hlsManifestUrl": "https://example.com/hls.m3u8", "adaptiveFormats": [
			{"itag": 137, "url": "https://example.com/v", "mimeType": "video/mp4; codecs=\"avc1.640028\"", "width": 1920, "height": 1080},
			{"itag": 140, "url": "https://example.com/a", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\""}
		]}
	}`
	// Muxed-only live: HLS-style single itag carrying both tracks, so NOT
	// segment-addressable on its own — this is the case that needs a manifest.
	const liveMuxedOnly = `{
		"playabilityStatus": {"status": "OK"},
		"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a", "isLive": true},
		"streamingData": {"hlsManifestUrl": "https://example.com/hls.m3u8", "formats": [
			{"itag": 18, "url": "https://example.com/muxed", "mimeType": "video/mp4; codecs=\"avc1.42001E, mp4a.40.2\"", "width": 640, "height": 360}
		]}
	}`
	const liveWithDash = `{
		"playabilityStatus": {"status": "OK"},
		"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a", "isLive": true},
		"streamingData": {"dashManifestUrl": "https://example.com/dash.mpd", "adaptiveFormats": [
			{"itag": 137, "url": "https://example.com/v2", "mimeType": "video/mp4; codecs=\"avc1.640028\"", "width": 1920, "height": 1080},
			{"itag": 140, "url": "https://example.com/a2", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\""}
		]}
	}`

	swap := func(t *testing.T, tr *clientKeyedTransport) {
		t.Helper()
		orig := apiClient
		apiClient = &http.Client{Transport: tr}
		t.Cleanup(func() { orig, apiClient = apiClient, orig })
	}

	t.Run("split adaptive live stops at visionos", func(t *testing.T) {
		tr := &clientKeyedTransport{responses: map[string]struct {
			status int
			body   string
		}{
			"101": {http.StatusOK, liveSplitAdaptive},
			"28":  {http.StatusOK, liveWithDash},
		}}
		swap(t, tr)

		var pool []Format
		res := NewPlayerAPI(nil, noopLogger{}).
			tryCookielessFallbacks(context.Background(), "test1234567", "vd", &pool)
		if res == nil {
			t.Fatal("expected a result")
		}
		if len(tr.calls) != 1 {
			t.Errorf("calls = %v, want only visionos — split adaptive formats need no manifest", tr.calls)
		}
		if !HasSplitAdaptiveFormats(pool) {
			t.Error("pool should be segment-addressable without a DASH manifest")
		}
	})

	t.Run("muxed-only live adopts the next client's manifest", func(t *testing.T) {
		tr := &clientKeyedTransport{responses: map[string]struct {
			status int
			body   string
		}{
			"101": {http.StatusOK, liveMuxedOnly},
			"28":  {http.StatusOK, liveWithDash},
		}}
		swap(t, tr)

		var pool []Format
		res := NewPlayerAPI(nil, noopLogger{}).
			tryCookielessFallbacks(context.Background(), "test1234567", "vd", &pool)
		if res == nil {
			t.Fatal("expected a result")
		}
		if len(tr.calls) != 2 {
			t.Errorf("calls = %v, want both — a muxed-only live pool needs a manifest", tr.calls)
		}
		if res.DashManifestURL != "https://example.com/dash.mpd" {
			t.Errorf("DashManifestURL = %q, want the android_vr manifest adopted", res.DashManifestURL)
		}
	})

	t.Run("visionos inadequate falls through to android_vr", func(t *testing.T) {
		tr := &clientKeyedTransport{responses: map[string]struct {
			status int
			body   string
		}{
			"101": {http.StatusOK, audioOnlyOKBody}, // OK but no video
			"28":  {http.StatusOK, adequateOKBody},
		}}
		swap(t, tr)

		var pool []Format
		res := NewPlayerAPI(nil, noopLogger{}).
			tryCookielessFallbacks(context.Background(), "test1234567", "vd", &pool)
		if res == nil {
			t.Fatal("expected the android_vr result")
		}
		if len(tr.calls) != 2 || tr.calls[1] != "28" {
			t.Errorf("calls = %v, want fallthrough to android_vr", tr.calls)
		}
		if *pool[0].AuthLevel != AuthLevelVisionOS || *pool[1].AuthLevel != AuthLevelAndroidVR {
			t.Error("both clients' formats should be pooled at their own tiers")
		}
	})
}
