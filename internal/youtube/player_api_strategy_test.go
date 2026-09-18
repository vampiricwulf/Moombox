package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/cookies"
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
		res := newAPI().tryCookielessFallbacks(context.Background(), "test1234567", "vd", &pool, &mismatchTally{}, nil)
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
		res := newAPI().tryCookielessFallbacks(context.Background(), "test1234567", "vd", &pool, &mismatchTally{}, nil)
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
		res := newAPI().tryCookielessFallbacks(context.Background(), "test1234567", "vd", &pool, &mismatchTally{}, nil)
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
			tryCookielessFallbacks(context.Background(), "test1234567", "vd", &pool, &mismatchTally{}, nil)
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
			tryCookielessFallbacks(context.Background(), "test1234567", "vd", &pool, &mismatchTally{}, nil)
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
			tryCookielessFallbacks(context.Background(), "test1234567", "vd", &pool, &mismatchTally{}, nil)
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

// substituteBody is what a blocked IP gets: a well-formed 200 about ANOTHER
// video. The itags differ from adequateOKBody's so a leak into the pool is
// visible.
const substituteBody = `{
    "playabilityStatus": {"status": "OK"},
    "videoDetails": {"videoId": "OTHERvideo1", "title": "Substitute Video", "author": "Other Ch"},
    "streamingData": {"adaptiveFormats": [
        {"itag": 248, "url": "https://substitute/v", "mimeType": "video/webm; codecs=\"vp9\"", "width": 1920, "height": 1080},
        {"itag": 251, "url": "https://substitute/a", "mimeType": "audio/webm; codecs=\"opus\""}
    ]}
}`

// stubWatchPage points the cascades at an empty watch page so the tests
// exercise the CLIENT chain without a real HTTP round trip.
func stubWatchPage(t *testing.T) {
	t.Helper()
	orig := fetchWatchPage
	fetchWatchPage = func(context.Context, string, string) (*WatchPageResult, error) {
		return &WatchPageResult{Ytcfg: DefaultYtcfg()}, nil
	}
	t.Cleanup(func() { fetchWatchPage = orig })
}

// TestCascadeSkipsASubstitutingClient: TV is served the substitute, VISIONOS
// answers correctly, and the result must be VISIONOS's — with none of the
// substitute's formats in the pool.
//
// Mutants this kills:
//   - the mismatch check removed        → title is "Substitute Video"
//   - the mismatch not erroring the call → itags 248/251 appear in the pool
//   - the cascade stopping at the first mismatch → GetVideoInfoPublic errors
//   - ipBlockShape dropping its `survived == 0` half (`mismatched > 0` alone)
//     → the verdict fires although VISIONOS answered about the right video
func TestCascadeSkipsASubstitutingClient(t *testing.T) {
	stubWatchPage(t)
	tr := &clientKeyedTransport{responses: map[string]struct {
		status int
		body   string
	}{
		"7":   {http.StatusOK, substituteBody}, // TV_DOWNGRADED
		"101": {http.StatusOK, adequateOKBody}, // VISIONOS
		"28":  {http.StatusOK, adequateOKBody}, // ANDROID_VR
	}}
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })

	info, err := newRetryTestAPI().GetVideoInfoPublic(context.Background(), "test1234567")
	if err != nil {
		t.Fatalf("GetVideoInfoPublic: %v", err)
	}
	if info.Title == "Substitute Video" {
		t.Fatal("the cascade adopted the substitute video's metadata")
	}
	for _, f := range info.Formats {
		if f.Itag == 248 || f.Itag == 251 {
			t.Errorf("a substitute format reached the pool: itag %d from %s", f.Itag, f.Source)
		}
	}
}

// TestMismatchTallyIPBlockShape pins upstream's raise condition directly,
// `if skipped_clients: ... if not prs: raise` (_video.py:3181-3187), where the
// cascade tests can only reach it through whichever branch a given transport
// happens to take. Both halves are load-bearing and each has its own mutant.
//
// Mutants this kills:
//   - `attempts == mismatched` (the pre-round rule) → row "substitute plus
//     other-reason failures" goes false, and a real IP block reaches the
//     worker as the diagnosis-free "unhandled status: "
//   - `mismatched > 0` alone → row "substitute plus a survivor" goes true,
//     so a cascade that DID get a good answer is called an IP block
//   - `survived == 0` alone → row "no substitute, only failures" goes true,
//     raising the verdict on flakiness with no substitution signal at all
//   - note() counting an other-reason failure as a survivor → row one goes
//     false (that client contributes nothing to upstream's `prs` either)
func TestMismatchTallyIPBlockShape(t *testing.T) {
	sub := &VideoIDMismatchError{Requested: "test1234567", Got: "OTHERvideo1"}
	other := errors.New("Innertube API error: HTTP 404")

	for _, tc := range []struct {
		name    string
		results []error // one entry per client attempt; nil means "usable"
		want    bool
	}{
		{"nothing attempted", nil, false},
		{"substitute plus other-reason failures", []error{sub, other, other}, true},
		{"every client substituted", []error{sub, sub}, true},
		{"substitute plus a survivor", []error{sub, nil}, false},
		{"no substitute, only failures", []error{other, other}, false},
		{"everything fine", []error{nil, nil}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tally := &mismatchTally{}
			for _, err := range tc.results {
				tally.note(nil, err)
			}
			if got := tally.ipBlockShape(); got != tc.want {
				t.Errorf("ipBlockShape() = %v, want %v (attempts %d, mismatched %d, survived %d)",
					got, tc.want, tally.attempts, tally.mismatched, tally.survived)
			}
		})
	}

	t.Run("the first substitute id is carried for the verdict log", func(t *testing.T) {
		tally := &mismatchTally{}
		tally.note(nil, other)
		tally.note(nil, sub)
		tally.note(nil, &VideoIDMismatchError{Requested: "test1234567", Got: "LATERvideo1"})
		if tally.got != "OTHERvideo1" {
			t.Errorf("got = %q, want the FIRST substitute id", tally.got)
		}
	})
}

// warnCapturingLogger records every Warn call so a test can assert on the
// fields a log line carries. Debug/Info/Error are discarded — only the
// verdict's Warn is under test.
type warnCapturingLogger struct{ warns []string }

func (l *warnCapturingLogger) Debug(string, ...any) {}
func (l *warnCapturingLogger) Info(string, ...any)  {}
func (l *warnCapturingLogger) Error(string, ...any) {}
func (l *warnCapturingLogger) Warn(msg string, args ...any) {
	l.warns = append(l.warns, fmt.Sprintf("%s %v", msg, args))
}

// TestEveryClientSubstitutedIsReportedAsAnIPBlock mirrors upstream's
// "All player responses are invalid. Your IP is likely being blocked by
// Youtube" (_video.py:3181-3187). Reporting "no formats" instead would send
// the operator hunting for a format problem that does not exist — and the
// worker renders that empty result as the diagnosis-free "unhandled status: ".
//
// Mutants this kills:
//   - finishExtraction not raising                     → err is nil
//   - ipBlockShape reverted to `attempts == mismatched` → the mixed subtest
//     below gets a nil error and an empty VideoInfo instead of the verdict
//   - ipBlockShape dropping `mismatched > 0`            → the no-substitute
//     subtest raises the verdict on flakiness alone
//   - the verdict's Warn losing the "substitute" field  → the log subtest
//   - raising even though the watch page produced a usable response
//     → covered by the "a valid watch page survives" subtest
func TestEveryClientSubstitutedIsReportedAsAnIPBlock(t *testing.T) {
	all := map[string]struct {
		status int
		body   string
	}{
		"7": {http.StatusOK, substituteBody}, "101": {http.StatusOK, substituteBody},
		"28": {http.StatusOK, substituteBody}, "56": {http.StatusOK, substituteBody},
	}
	swapTransport := func(t *testing.T, tr *clientKeyedTransport) {
		t.Helper()
		orig := apiClient
		apiClient = &http.Client{Transport: tr}
		t.Cleanup(func() { apiClient = orig })
	}

	t.Run("no watch page survivor", func(t *testing.T) {
		stubWatchPage(t)
		swapTransport(t, &clientKeyedTransport{responses: all})

		_, err := newRetryTestAPI().GetVideoInfoPublic(context.Background(), "test1234567")
		if !errors.Is(err, ErrAllClientsMismatched) {
			t.Fatalf("err = %v, want ErrAllClientsMismatched", err)
		}
	})

	// One substitute plus transport/status failures on the rest is upstream's
	// `if skipped_clients: ... if not prs: raise` exactly: a failing client
	// hits `continue` (_video.py:3119-3121) and adds nothing to `prs`, so it
	// cannot veto the verdict. The pre-round rule (attempts == mismatched) let
	// it veto, and the operator got StreamProcessResult.Error = "unhandled
	// status: " — a dead end naming no cause at all.
	t.Run("one substitute, every other client failing", func(t *testing.T) {
		stubWatchPage(t)
		// Only TV answers; 101/28 fall to the transport's 404 default, which
		// doRetryRequest turns into a non-mismatch "API error: HTTP 404".
		swapTransport(t, &clientKeyedTransport{responses: map[string]struct {
			status int
			body   string
		}{
			"7": {http.StatusOK, substituteBody},
		}})

		info, err := newRetryTestAPI().GetVideoInfoPublic(context.Background(), "test1234567")
		if !errors.Is(err, ErrAllClientsMismatched) {
			t.Fatalf("err = %v (info %+v), want ErrAllClientsMismatched — one positive substitution signal and no survivor is an IP block", err, info)
		}
	})

	// The mirror image: no substitute anywhere, every client simply failed.
	// The IP-block verdict must NOT fire on absence of evidence — upstream's
	// OTHER raise covers this shape (`elif not prs: raise ExtractorError(
	// 'Failed to extract any player response')`, _video.py:3188-3189), and
	// that is what the cascade returns here.
	//
	// What this subtest can no longer assert is the TV client's own HTTP error
	// coming back BARE: row #58 (YOUTUBE-12) made the public path mirror the
	// authenticated one, which has always logged a TV failure and carried on,
	// so the cascade asks VISIONOS and ANDROID_VR first and reports the
	// exhaustion, with the last client error wrapped inside it.
	t.Run("no substitute at all, every client failing", func(t *testing.T) {
		stubWatchPage(t)
		tr := &clientKeyedTransport{responses: map[string]struct {
			status int
			body   string
		}{}}
		swapTransport(t, tr)

		info, err := newRetryTestAPI().GetVideoInfoPublic(context.Background(), "test1234567")
		if errors.Is(err, ErrAllClientsMismatched) {
			t.Fatalf("flakiness alone raised the IP-block verdict: %v", err)
		}
		if err == nil || !strings.Contains(err.Error(), "failed to extract any player response") {
			t.Fatalf("err = %v (info %+v), want the exhaustion verdict", err, info)
		}
		if info != nil {
			t.Errorf("info = %+v, want nil alongside that error", info)
		}
		for _, want := range []string{"101", "28"} {
			if !slices.Contains(tr.calls, want) {
				t.Errorf("client %s was never tried (calls = %v) — row #58 exists so a TV failure no longer ends the cascade", want, tr.calls)
			}
		}
	})

	// F4: at the default log level every per-client mismatch is Debug, so if
	// the verdict's own Warn does not name the substitute the operator has no
	// way to learn which video YouTube served. Upstream's warning names it
	// (_video.py:3182-3184).
	t.Run("the verdict names the substitute", func(t *testing.T) {
		stubWatchPage(t)
		swapTransport(t, &clientKeyedTransport{responses: all})

		lg := &warnCapturingLogger{}
		p := NewPlayerAPI(NewAuth(cookies.NewCookieJar(), noopLogger{}), lg)
		if _, err := p.GetVideoInfoPublic(context.Background(), "test1234567"); !errors.Is(err, ErrAllClientsMismatched) {
			t.Fatalf("err = %v, want ErrAllClientsMismatched", err)
		}
		var verdict string
		for _, w := range lg.warns {
			if strings.Contains(w, "none survived") {
				verdict = w
			}
		}
		if verdict == "" {
			t.Fatalf("no verdict Warn logged; warns = %q", lg.warns)
		}
		if !strings.Contains(verdict, "OTHERvideo1") {
			t.Errorf("verdict Warn %q does not name the substitute id", verdict)
		}
		if !strings.Contains(verdict, "test1234567") {
			t.Errorf("verdict Warn %q does not name the requested video", verdict)
		}
	})

	t.Run("a valid watch page survives", func(t *testing.T) {
		origFetch := fetchWatchPage
		fetchWatchPage = func(context.Context, string, string) (*WatchPageResult, error) {
			return &WatchPageResult{
				Ytcfg:          DefaultYtcfg(),
				PlayerResponse: decodePlayerJSON(t, adequateOKBody),
			}, nil
		}
		t.Cleanup(func() { fetchWatchPage = origFetch })

		tr := &clientKeyedTransport{responses: all}
		orig := apiClient
		apiClient = &http.Client{Transport: tr}
		t.Cleanup(func() { apiClient = orig })

		info, err := newRetryTestAPI().GetVideoInfoPublic(context.Background(), "test1234567")
		if err != nil {
			t.Fatalf("a usable watch page must survive a full client sweep: %v", err)
		}
		if info == nil || len(info.Formats) == 0 {
			t.Fatalf("info = %+v, want the watch page's formats", info)
		}
	})
}

// upcomingTVBody is what TV answers during a waiting room: a healthy
// playability verdict, no formats, isUpcoming set.
const upcomingTVBody = `{
	"playabilityStatus": {"status": "OK"},
	"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a", "isUpcoming": true, "isLiveContent": true},
	"streamingData": {}
}`

// TestUpcomingPollSkipsTheFormatHuntingFallbacks is owner decision O-I. An
// upcoming stream has no formats BY DEFINITION, so the three clients the
// cascade consults to go find some cannot succeed — yet on every 30 s
// waiting-room poll it fetched WEB_CREATOR, VISIONOS and ANDROID_VR, the last
// of them TWICE.
//
// android_vr stays in the roster everywhere else; this rule is about one
// verdict, StreamUpcoming with PlayabilityOK from the TV authority.
//
// Mutants this kills:
//   - the short-circuit dropped        → 62/101/28 appear in the call list
//   - the short-circuit not requiring
//     PlayabilityOK                    → covered by the members-only subtest
func TestUpcomingPollSkipsTheFormatHuntingFallbacks(t *testing.T) {
	stubWatchPage(t)
	tr := &clientKeyedTransport{responses: map[string]struct {
		status int
		body   string
	}{
		"7":   {http.StatusOK, upcomingTVBody},
		"56":  {http.StatusOK, upcomingTVBody},
		"62":  {http.StatusOK, adequateOKBody},
		"101": {http.StatusOK, adequateOKBody},
		"28":  {http.StatusOK, adequateOKBody},
	}}
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })

	info, err := newRetryTestAPI().GetVideoInfoPublic(context.Background(), "test1234567")
	if err != nil {
		t.Fatalf("GetVideoInfoPublic: %v", err)
	}
	if info.StreamStatus != StreamUpcoming {
		t.Fatalf("StreamStatus = %q, want upcoming", info.StreamStatus)
	}
	for _, c := range tr.calls {
		if c == "62" || c == "101" || c == "28" {
			t.Errorf("format-hunting client %s was called on an upcoming TV verdict: calls=%v", c, tr.calls)
		}
	}
}

// TestUpcomingPollSkipsTheFormatHuntingFallbacksAuthenticated is the same rule
// on the cascade that actually pays for it. With every client answering the
// way a real waiting room does — OK playability, isUpcoming, no formats — the
// old order was WEB_EMBEDDED, TV, WEB, ANDROID_VR (DASH enrichment),
// WEB_CREATOR, VISIONOS, ANDROID_VR AGAIN: 7 player calls every 30 s for the
// whole window, because VISIONOS fails hasAdequateFormats on an upcoming
// stream so the cookieless chain never broke before its second client.
//
// Mutants this kills:
//   - the short-circuit dropped on either gate → 28/62 reappear in the call
//     list (the ANDROID_VR gate and the WEB_CREATOR gate each own part of the
//     tail, so dropping only one still fails the exact-sequence compare)
//   - tvSaysWaitingRoom reading a client other than the TV authority → the
//     sequence changes shape
func TestUpcomingPollSkipsTheFormatHuntingFallbacksAuthenticated(t *testing.T) {
	stubWatchPage(t)
	tr := &clientKeyedTransport{responses: map[string]struct {
		status int
		body   string
	}{
		"56":  {http.StatusOK, upcomingTVBody}, // WEB_EMBEDDED
		"7":   {http.StatusOK, upcomingTVBody}, // TV_DOWNGRADED — the authority
		"1":   {http.StatusOK, upcomingTVBody}, // WEB_SAFARI
		"62":  {http.StatusOK, upcomingTVBody}, // WEB_CREATOR
		"101": {http.StatusOK, upcomingTVBody}, // VISIONOS
		"28":  {http.StatusOK, upcomingTVBody}, // ANDROID_VR
	}}
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })

	info, err := newRetryTestAPI().GetVideoInfoAuthenticated(context.Background(), "test1234567")
	if err != nil {
		t.Fatalf("GetVideoInfoAuthenticated: %v", err)
	}
	if info.StreamStatus != StreamUpcoming {
		t.Fatalf("StreamStatus = %q, want upcoming", info.StreamStatus)
	}
	if got, want := strings.Join(tr.calls, ","), "56,7,1"; got != want {
		t.Errorf("calls = [%s], want [%s] — a waiting-room poll must not go format hunting", got, want)
	}
}

// TestUpcomingWithAWalledVerdictStillRunsTheFallbacks: the short-circuit is
// gated on PlayabilityOK, because an upcoming MEMBERS-ONLY stream is exactly
// the case the fallback chain exists for.
//
// Mutant this kills: short-circuiting on StreamUpcoming alone → no fallback
// client is called and a members-only waiting room loses its chain.
func TestUpcomingWithAWalledVerdictStillRunsTheFallbacks(t *testing.T) {
	stubWatchPage(t)
	const walled = `{
		"playabilityStatus": {"status": "LOGIN_REQUIRED", "reason": "Sign in to confirm you are not a bot"},
		"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a", "isUpcoming": true, "isLiveContent": true},
		"streamingData": {}
	}`
	tr := &clientKeyedTransport{responses: map[string]struct {
		status int
		body   string
	}{
		"7":   {http.StatusOK, walled},
		"101": {http.StatusOK, adequateOKBody},
	}}
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })

	if _, err := newRetryTestAPI().GetVideoInfoPublic(context.Background(), "test1234567"); err != nil {
		t.Fatalf("GetVideoInfoPublic: %v", err)
	}
	var sawVisionOS bool
	for _, c := range tr.calls {
		if c == "101" {
			sawVisionOS = true
		}
	}
	if !sawVisionOS {
		t.Errorf("a login-required upcoming stream skipped the cookieless chain: calls=%v", tr.calls)
	}
}

// TestCookielessFallbacksReuseAPrefetchedClient is O-I's no-behaviour-change
// half: the caller that already fetched ANDROID_VR for its DASH manifest hands
// the result in rather than paying for it twice. Here the prefetch FAILED, and
// a failed prefetch is reused too — the same cascade must not retry it.
//
// Mutants this kills:
//   - the prefetch ignored          → calls == [101 28], two round trips
func TestCookielessFallbacksReuseAPrefetchedClient(t *testing.T) {
	tr := &clientKeyedTransport{responses: map[string]struct {
		status int
		body   string
	}{
		"101": {http.StatusOK, audioOnlyOKBody}, // inadequate, so the chain goes on
	}}
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })

	p := NewPlayerAPI(nil, noopLogger{})
	var pool []Format
	prefetchedVR, prefetchedErr := p.fetchWithCookielessClient(context.Background(), "test1234567", "vd", constants.AndroidVRClient)
	// Simulate the :263 caller: it pooled what it fetched.
	vrFormats := 0
	if prefetchedVR != nil {
		collectFormats(&pool, prefetchedVR.Formats, "android_vr_dash_fallback", AuthLevelAndroidVR)
		vrFormats = len(prefetchedVR.Formats)
	}
	before := len(tr.calls)

	tally := &mismatchTally{}
	p.tryCookielessFallbacks(context.Background(), "test1234567", "vd", &pool, tally,
		&cookielessPrefetch{clientName: constants.AndroidVRClient.ClientName, result: prefetchedVR, err: prefetchedErr, pooled: true})

	for _, c := range tr.calls[before:] {
		if c == "28" {
			t.Errorf("ANDROID_VR was fetched again although a result was handed in: calls=%v", tr.calls)
		}
	}
	vrInPool := 0
	for _, f := range pool {
		if f.Source == "android_vr" || f.Source == "android_vr_dash_fallback" {
			vrInPool++
		}
	}
	if vrInPool != vrFormats {
		t.Errorf("android_vr formats in the pool = %d, want %d — a pooled prefetch must not be collected twice", vrInPool, vrFormats)
	}
}

// TestCookielessFallbacksDoNotRecollectAPooledPrefetch is the SUCCESSFUL-
// prefetch half of the same rule, which the failed-prefetch test above cannot
// reach: the handed-in result carries formats the caller already pooled, so
// the chain must adopt it as its own answer without collecting those formats
// a second time.
//
// Mutants this kills:
//   - the prefetch ignored     → "28" appears after the hand-in
//   - `pooled` ignored (the
//     alreadyPooled guard
//     dropped)                 → the pool carries 4 android_vr formats, not 2
func TestCookielessFallbacksDoNotRecollectAPooledPrefetch(t *testing.T) {
	tr := &clientKeyedTransport{responses: map[string]struct {
		status int
		body   string
	}{
		"101": {http.StatusOK, audioOnlyOKBody}, // inadequate, so the chain goes on
		"28":  {http.StatusOK, adequateOKBody},
	}}
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })

	p := NewPlayerAPI(nil, noopLogger{})
	var pool []Format
	prefetchedVR, prefetchedErr := p.fetchWithCookielessClient(context.Background(), "test1234567", "vd", constants.AndroidVRClient)
	if prefetchedErr != nil {
		t.Fatalf("prefetch: %v", prefetchedErr)
	}
	collectFormats(&pool, prefetchedVR.Formats, "android_vr_dash_fallback", AuthLevelAndroidVR)
	vrFormats := len(prefetchedVR.Formats)
	before := len(tr.calls)

	chosen := p.tryCookielessFallbacks(context.Background(), "test1234567", "vd", &pool, &mismatchTally{},
		&cookielessPrefetch{clientName: constants.AndroidVRClient.ClientName, result: prefetchedVR, err: nil, pooled: true})

	if chosen != prefetchedVR {
		t.Errorf("chosen = %+v, want the handed-in ANDROID_VR result", chosen)
	}
	for _, c := range tr.calls[before:] {
		if c == "28" {
			t.Errorf("ANDROID_VR was fetched again although a result was handed in: calls=%v", tr.calls)
		}
	}
	vrInPool := 0
	for _, f := range pool {
		if f.Source == "android_vr" || f.Source == "android_vr_dash_fallback" {
			vrInPool++
		}
	}
	if vrInPool != vrFormats {
		t.Errorf("android_vr formats in the pool = %d, want %d — a pooled prefetch must not be collected twice", vrInPool, vrFormats)
	}
}

// TestCookielessFallbacksFetchAndRecordAnEmptyPrefetch is the OTHER direction
// of the prefetch slot, and closes close-review Findings 6 and 7 together.
//
// Finding 7: a prefetch whose result AND error are both nil means "the caller
// wants this client's answer but has not made the call". The chain used to
// read that as a veto and SKIP the client outright, silently dropping the
// last-resort client from the chain.
//
// Finding 6: the public path had no result to hand in (its ANDROID_VR DASH
// enrichment runs AFTER the chain, not before it), so on the degraded shape it
// asked android_vr the same question twice in one extraction — the exact
// "never fetch it twice" O-I promised. An empty slot handed in is now FILLED by
// the chain, so the enrichment reads the answer instead of paying for it.
//
// Mutants this kill:
//   - the `result == nil && err == nil` arm still `continue`s → chosen is nil
//     and "28" never appears
//   - the write-back removed                                  → pf.result stays nil
//   - `pooled` not recorded on the write-back                 → pf.pooled false,
//     so the enrichment would collect the same formats a second time
func TestCookielessFallbacksFetchAndRecordAnEmptyPrefetch(t *testing.T) {
	tr := &clientKeyedTransport{responses: map[string]struct {
		status int
		body   string
	}{
		"101": {http.StatusOK, audioOnlyOKBody}, // inadequate, so the chain goes on
		"28":  {http.StatusOK, adequateOKBody},
	}}
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })

	p := NewPlayerAPI(nil, noopLogger{})
	var pool []Format
	// The empty slot: the caller names the client it will want back and makes
	// no call of its own.
	pf := &cookielessPrefetch{clientName: constants.AndroidVRClient.ClientName}

	chosen := p.tryCookielessFallbacks(context.Background(), "test1234567", "vd", &pool, &mismatchTally{}, pf)

	if chosen == nil {
		t.Fatalf("the chain skipped ANDROID_VR on an empty prefetch: calls=%v", tr.calls)
	}
	vrCalls := 0
	for _, c := range tr.calls {
		if c == "28" {
			vrCalls++
		}
	}
	if vrCalls != 1 {
		t.Errorf("ANDROID_VR fetched %d times, want exactly 1: calls=%v", vrCalls, tr.calls)
	}
	if pf.result != chosen || pf.err != nil {
		t.Errorf("prefetch = (%p, %v), want the chain's own ANDROID_VR answer written back", pf.result, pf.err)
	}
	if !pf.pooled {
		t.Error("the write-back did not record that the chain pooled these formats — the caller would collect them twice")
	}
}

// TestPublicPathFetchesAndroidVROnlyOnce is Finding 6 end to end, on the
// degraded public shape the Task 7 review measured as calls [7 101 28 28]: the
// TV client answers OK but inadequate (so the cookieless chain runs), neither
// cookieless client is adequate either (so the chain returns nil), and the
// watch page carried no player response (so there is no wpParsed to return) —
// which drops through to the ANDROID_VR DASH enrichment that used to re-ask the
// client the chain had just asked.
//
// Mutant this kills: the enrichment fetching unconditionally instead of
// reading the filled prefetch → "28" appears twice.
func TestPublicPathFetchesAndroidVROnlyOnce(t *testing.T) {
	// Live, OK, audio-only — adequate enough to keep the extraction alive and
	// inadequate enough that nothing short-circuits.
	const liveAudioOnly = `{
		"playabilityStatus": {"status": "OK"},
		"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a", "isLive": true, "isLiveContent": true},
		"streamingData": {"adaptiveFormats": [
			{"itag": 140, "url": "https://example.com/a", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\""}
		]}
	}`
	// The same, plus the DASH manifest the enrichment exists to source.
	const liveAudioOnlyWithDash = `{
		"playabilityStatus": {"status": "OK"},
		"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a", "isLive": true, "isLiveContent": true},
		"streamingData": {"dashManifestUrl": "https://example.com/manifest.mpd", "adaptiveFormats": [
			{"itag": 140, "url": "https://example.com/a", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\""}
		]}
	}`

	stubWatchPage(t)
	tr := &clientKeyedTransport{responses: map[string]struct {
		status int
		body   string
	}{
		"7":   {http.StatusOK, liveAudioOnly},
		"101": {http.StatusOK, liveAudioOnly},
		"28":  {http.StatusOK, liveAudioOnlyWithDash},
	}}
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })

	info, err := newRetryTestAPI().GetVideoInfoPublic(context.Background(), "test1234567")
	if err != nil {
		t.Fatalf("GetVideoInfoPublic: %v", err)
	}
	vrCalls := 0
	for _, c := range tr.calls {
		if c == "28" {
			vrCalls++
		}
	}
	if vrCalls != 1 {
		t.Errorf("ANDROID_VR fetched %d times in one extraction, want 1: calls=%v", vrCalls, tr.calls)
	}
	if info.DashManifestURL != "https://example.com/manifest.mpd" {
		t.Errorf("DashManifestURL = %q, want the manifest the reused ANDROID_VR answer carried", info.DashManifestURL)
	}
}

// TestChatSourceCarriesThePagesFetchInstant is close-review Finding 10.
// ChatSource.FetchedAt is what ChatSource.Usable measures the two-minute
// window from, and it was stamped at the END of the cascade — which on an
// authenticated extraction can be tens of seconds after the page arrived. The
// window was therefore measured from too late, i.e. a continuation was trusted
// for LONGER than its real age, which is the one direction that costs a chat
// start (a stale token, a failed first poll).
//
// Mutants this kill:
//   - withAttestation stamping time.Now() again → the first row's timestamp
//     check fails and the past-the-window row reports Usable
//   - FetchWatchPage not stamping the field     → a real page's source reads
//     as freshly fetched no matter how old it is (covered by the same rows,
//     since the zero-value fallback is what they would hit)
func TestChatSourceCarriesThePagesFetchInstant(t *testing.T) {
	pageAt := time.Now().Add(-90 * time.Second)
	info := withAttestation(&VideoInfo{}, &WatchPageResult{
		Ytcfg:            DefaultYtcfg(),
		FetchedAt:        pageAt,
		ChatContinuation: "tok",
	}, "vid123")
	if !info.Chat.FetchedAt.Equal(pageAt) {
		t.Errorf("Chat.FetchedAt = %v, want the PAGE's fetch instant %v", info.Chat.FetchedAt, pageAt)
	}
	if !info.Chat.Usable() {
		t.Error("a 90-second-old page is inside the two-minute window and must still be usable")
	}

	// A page older than the window is not made fresh by a cascade that only
	// just finished.
	stale := withAttestation(&VideoInfo{}, &WatchPageResult{
		Ytcfg:            DefaultYtcfg(),
		FetchedAt:        time.Now().Add(-3 * time.Minute),
		ChatContinuation: "tok",
	}, "vid123")
	if stale.Chat.Usable() {
		t.Error("a three-minute-old page reported Usable — the window is measured from the extraction, not the page")
	}

	// A synthesized result (the fetch failed) carries no stamp; the fallback
	// keeps Usable answerable rather than leaving a zero time behind.
	synth := withAttestation(&VideoInfo{}, &WatchPageResult{Ytcfg: DefaultYtcfg(), ChatContinuation: "tok"}, "vid123")
	if synth.Chat.FetchedAt.IsZero() {
		t.Error("an unstamped watch page left ChatSource.FetchedAt zero")
	}
}

// TestInnertubeErrorCarriesYouTubesOwnMessage: YouTube explains a 400/401/403
// in the body ("Precondition check failed", "Request is missing required
// authentication credential"), and the operator only ever saw "HTTP 403".
// The "HTTP <code>" substring must SURVIVE, because worker/probe_classify.go
// keys on it to decide whether a probe error is transient.
//
// Mutants this kills:
//   - the detail not appended             → the message check fails
//   - the "HTTP %d" prefix replaced       → the prefix check fails
//   - a non-JSON body crashing or leaking → the second subtest fails
//   - the detail put BEFORE the prefix    → the differential subtest fails
//   - the " — " separator reworded        → the separator subtest fails
func TestInnertubeErrorCarriesYouTubesOwnMessage(t *testing.T) {
	t.Run("json error body", func(t *testing.T) {
		got := innertubeErrorDetail([]byte(`{"error":{"code":403,"message":"Precondition check failed.","status":"FAILED_PRECONDITION"}}`))
		if !strings.Contains(got, "FAILED_PRECONDITION") || !strings.Contains(got, "Precondition check failed.") {
			t.Errorf("innertubeErrorDetail = %q, want YouTube's status and message", got)
		}
	})

	t.Run("non-json body yields nothing", func(t *testing.T) {
		if got := innertubeErrorDetail([]byte("<html>502 Bad Gateway</html>")); got != "" {
			t.Errorf("innertubeErrorDetail on HTML = %q, want \"\"", got)
		}
	})

	// The differential: every error string this package produces for a non-200
	// still STARTS with the exact bytes it produced before the detail existed.
	// probe_classify.go's fallback lowercases and substring-matches "http 4" /
	// "http 5", and a detail placed ahead of the prefix (or a reworded prefix)
	// would silently reclassify a terminal 404 as transient.
	t.Run("the classifier's prefix survives byte-for-byte", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			body string
		}{
			{"with a detail", `{"error":{"message":"Precondition check failed.","status":"FAILED_PRECONDITION"}}`},
			{"with an empty error object", `{"error":{}}`},
			{"with an HTML body", "<html>404</html>"},
			{"with an empty body", ""},
		} {
			for _, code := range []int{400, 401, 403, 404, 429, 500} {
				want := fmt.Sprintf("WEB API error: HTTP %d", code)
				got := innertubeHTTPError("WEB", code, []byte(tc.body)).Error()
				if !strings.HasPrefix(got, want) {
					t.Errorf("%s: innertubeHTTPError(%d) = %q, want the prefix %q — probe_classify.go keys on it",
						tc.name, code, got, want)
				}
			}
		}
	})

	// The delimiter itself, pinned on the YOUTUBE side. probe_classify.go's
	// string fallback (internal/worker/probe_classify.go, the
	// `strings.Index(msg, " — ")` window cut) throws away everything after
	// this exact sequence before it reads the status code, because YouTube's
	// own message can contain "backend timeout" and would beat the code on the
	// transient arm. Nothing in internal/youtube pinned the separator, so both
	// packages stayed green while the flip came back.
	//
	// Mutant this kills: the separator changed to ": " or to an EN dash "–"
	// (U+2013) in innertubeHTTPError's format string. probe_classify.go's
	// window then never cuts, a terminal 404 whose message reads "Backend
	// timeout …" classifies transient, the give-up counter never advances, and
	// the probe casts a false "network down" vote into the connectivity
	// oracle.
	t.Run("the separator is exactly an em dash between spaces", func(t *testing.T) {
		const sep = " — " // space, EM DASH (U+2014), space
		body := `{"error":{"message":"Precondition check failed.","status":"FAILED_PRECONDITION"}}`
		got := innertubeHTTPError("WEB", 404, []byte(body)).Error()
		detail := innertubeErrorDetail([]byte(body))
		want := "WEB API error: HTTP 404" + sep + detail
		if got != want {
			t.Errorf("innertubeHTTPError = %q, want %q — probe_classify.go cuts on %q before reading the status code",
				got, want, sep)
		}
		if head, _, ok := strings.Cut(got, sep); !ok || head != "WEB API error: HTTP 404" {
			t.Errorf("cutting %q on %q gave (%q, %v), want (%q, true)",
				got, sep, head, ok, "WEB API error: HTTP 404")
		}
	})

	// YouTube localises error.message, so the 512-byte bound can land in the
	// middle of a multi-byte rune — and a string slice is a BYTE operation.
	// Invalid UTF-8 then goes into a log line and, through the error, into the
	// job's error column.
	//
	// Mutant this kills: the bound applied as a plain byte slice.
	t.Run("the bound cuts on a rune boundary", func(t *testing.T) {
		// Three-byte runes, so the 512-byte bound falls inside one.
		body := `{"error":{"status":"FAILED_PRECONDITION","message":"` + strings.Repeat("あ", 400) + `"}}`
		got := innertubeErrorDetail([]byte(body))
		if len(got) > innertubeErrorDetailMax {
			t.Errorf("detail is %d bytes, want at most %d", len(got), innertubeErrorDetailMax)
		}
		if !utf8.ValidString(got) {
			t.Errorf("detail is not valid UTF-8 after the bound: %q", got)
		}
		if !strings.HasPrefix(got, "FAILED_PRECONDITION: あ") {
			t.Errorf("detail = %q, want YouTube's status and the start of its message", got)
		}
	})

	// error.message is attacker-adjacent text that lands in a single-line slog
	// record and in the job's error column. A newline in it splits the log
	// line; a NUL or an escape sequence is worse.
	//
	// C0 was never the whole set (close-review Finding 3). NEL (U+0085, a C1
	// control) and LINE/PARAGRAPH SEPARATOR (U+2028/U+2029) break a record in
	// several viewers exactly as "\n" does, and the bidi overrides
	// (U+202A-202E, U+2066-2069) reverse the VISUAL order of everything after
	// them in a terminal or the dashboard's job row — so a substitute video id
	// or a status code can be made to read as something else entirely.
	//
	// NOT all of Cf: U+200D (ZERO WIDTH JOINER) is what holds a multi-person
	// emoji together, and mangling it would corrupt ordinary text.
	//
	// Mutants this kill:
	//   - the control-character strip removed         → the C0 rows fail
	//   - the strip left at `r < 0x20 || r == 0x7f`   → NEL, U+2028/9 and RLO
	//     survive into the log line
	//   - the strip widened to all of unicode.Cf      → the ZWJ row fails
	t.Run("control characters are replaced", func(t *testing.T) {
		// A ZERO WIDTH JOINER emoji sequence, built from runes so nothing in
		// the toolchain can normalise it away: it is the control case for
		// "the strip is the control set, not all of unicode.Cf".
		zwjFamily := string(rune(0x1F468)) + string(rune(0x200D)) +
			string(rune(0x1F469)) + string(rune(0x200D)) + string(rune(0x1F467))
		// Built rather than written out, so the fixture carries the real
		// bytes: a newline, a carriage return, a tab, a NUL, and an ESC — the
		// lead byte of an ANSI sequence a terminal would obey — plus the C1,
		// separator and bidi runes that a C0-only strip let through.
		msg := "line one\nline two\r\tand" + string(rune(0)) + "a nul" + string(rune(0x1b)) + "[31m" +
			string(rune(0x85)) + "nel" + string(rune(0x2028)) + "ls" + string(rune(0x2029)) +
			"ps" + string(rune(0x202e)) + "rlo" + string(rune(0x2066)) + "fsi" +
			" keep " + zwjFamily + " together"
		body, mErr := json.Marshal(map[string]any{"error": map[string]any{"status": "NOT_FOUND", "message": msg}})
		if mErr != nil {
			t.Fatalf("marshal fixture: %v", mErr)
		}

		got := innertubeErrorDetail(body)
		for _, bad := range []string{
			"\n", "\r", "\t", string(rune(0)), string(rune(0x1b)),
			string(rune(0x85)), string(rune(0x2028)), string(rune(0x2029)),
			string(rune(0x202e)), string(rune(0x2066)),
		} {
			if strings.Contains(got, bad) {
				t.Errorf("detail %q still carries %q", got, bad)
			}
		}
		if !strings.Contains(got, "line one line two") {
			t.Errorf("detail = %q, want the text itself preserved with the control characters spaced out", got)
		}
		if !strings.Contains(got, zwjFamily) {
			t.Errorf("detail = %q, want the ZWJ emoji sequence intact — the strip is the control set, not all of Cf", got)
		}
	})

	t.Run("the prefix survives on the wire too", func(t *testing.T) {
		tr := &clientKeyedTransport{responses: map[string]struct {
			status int
			body   string
		}{
			"28": {http.StatusForbidden, `{"error":{"message":"Precondition check failed.","status":"FAILED_PRECONDITION"}}`},
		}}
		orig := apiClient
		apiClient = &http.Client{Transport: tr}
		t.Cleanup(func() { apiClient = orig })

		_, err := NewPlayerAPI(nil, noopLogger{}).ProbeVideoStatus(context.Background(), "test1234567", "vd")
		if err == nil {
			t.Fatal("a 403 was not reported as an error")
		}
		if !strings.Contains(err.Error(), "HTTP 403") {
			t.Errorf("err = %q — probe_classify.go keys on the \"HTTP <code>\" substring", err)
		}
		if !strings.Contains(err.Error(), "Precondition check failed.") {
			t.Errorf("err = %q, want YouTube's own message appended", err)
		}
	})
}

// TestFormatDiagCountsTheSABRSignal: a client forced onto SABR returns formats
// with neither url nor signatureCipher plus a serverAbrStreamingUrl. Today
// that logs as "formats 0", indistinguishable from an empty streamingData —
// and serverAbrStreamingUrl appears nowhere in the codebase at all.
//
// Mutants this kills:
//   - URL-less formats not counted   → URLlessFormats == 0
//   - serverAbrStreamingUrl not read → SabrForced == false
func TestFormatDiagCountsTheSABRSignal(t *testing.T) {
	p := NewPlayerAPI(nil, noopLogger{})
	data := decodePlayerJSON(t, `{
		"playabilityStatus": {"status": "OK"},
		"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a"},
		"streamingData": {
			"serverAbrStreamingUrl": "https://rr1---sn-x.googlevideo.com/videoplayback?...",
			"adaptiveFormats": [
				{"itag": 137, "mimeType": "video/mp4; codecs=\"avc1.640028\"", "width": 1920, "height": 1080},
				{"itag": 140, "mimeType": "audio/mp4; codecs=\"mp4a.40.2\""}
			]
		}
	}`)

	info, err := p.parsePlayerResponse(context.Background(), data, "", nil, "test1234567")
	if err != nil {
		t.Fatalf("parsePlayerResponse: %v", err)
	}
	if len(info.Formats) != 0 {
		t.Fatalf("Formats = %+v, want none (they carry no URL)", info.Formats)
	}
	if info.FormatDiag.URLlessFormats != 2 {
		t.Errorf("URLlessFormats = %d, want 2", info.FormatDiag.URLlessFormats)
	}
	if !info.FormatDiag.SabrForced {
		t.Error("SabrForced = false although streamingData carried serverAbrStreamingUrl")
	}
}

// countingPotProvider counts PLAYER PO-token mints. PotTokenProvider
// (player_api.go) has exactly one method, so this is the whole fake.
type countingPotProvider struct {
	token string
	calls int
}

func (c *countingPotProvider) GeneratePoTokenString(context.Context, string, bool) (string, error) {
	c.calls++
	return c.token, nil
}

// TestProbeVideoDateMintsNoPlayerToken is owner decision O-R. yt-dlp's WEB
// PLAYER_PO_TOKEN_POLICY is required=False / recommended=False (_base.py:90),
// so upstream mints none; Moombox minted one per PROBED video ID and parked a
// 6 h session-cache entry that every later mint sweeps.
//
// Mutants this kills:
//   - the probe still routed through fetchWithClient → minted == 1
//   - the skip applied to real player calls too      → the second half fails
func TestProbeVideoDateMintsNoPlayerToken(t *testing.T) {
	tr := &clientKeyedTransport{responses: map[string]struct {
		status int
		body   string
	}{
		"1": {http.StatusOK, adequateOKBody},
	}}
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })

	prov := &countingPotProvider{token: "POT"}
	p := newRetryTestAPI()
	p.SetPotProvider(prov)

	if _, _, err := p.ProbeVideoDate(context.Background(), "test1234567", "vd"); err != nil {
		t.Fatalf("ProbeVideoDate: %v", err)
	}
	if prov.calls != 0 {
		t.Errorf("ProbeVideoDate minted %d PLAYER PO tokens, want 0", prov.calls)
	}

	ytcfg := DefaultYtcfg()
	ytcfg.VisitorData = "vd"
	if _, err := p.fetchWithClient(context.Background(), "test1234567", constants.WebSafariClient, ytcfg, 0); err != nil {
		t.Fatalf("fetchWithClient: %v", err)
	}
	if prov.calls != 1 {
		t.Errorf("a real WEB player call minted %d tokens, want 1 — the skip is for probes only", prov.calls)
	}
}

// TestPublicPathContinuesAfterATVFailure is row #58. The authenticated path
// logs and carries on into WEB_CREATOR / the cookieless chain; the public path
// returned the TV error outright whenever the watch page had no player
// response, so an anonymous extraction gave up with clients left untried.
//
// Mutant this kills: the early `return nil, err` restored → err is non-nil
// and VISIONOS is never called.
func TestPublicPathContinuesAfterATVFailure(t *testing.T) {
	stubWatchPage(t)
	tr := &clientKeyedTransport{responses: map[string]struct {
		status int
		body   string
	}{
		"7":   {http.StatusNotFound, `{"error":{"message":"not found","status":"NOT_FOUND"}}`},
		"101": {http.StatusOK, adequateOKBody},
	}}
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })

	info, err := newRetryTestAPI().GetVideoInfoPublic(context.Background(), "test1234567")
	if err != nil {
		t.Fatalf("GetVideoInfoPublic gave up after the TV failure: %v", err)
	}
	if info == nil || len(info.Formats) == 0 {
		t.Fatalf("info = %+v, want VISIONOS's formats", info)
	}
	if !slices.Contains(tr.calls, "101") {
		t.Errorf("calls = %v, want VISIONOS (101) among them", tr.calls)
	}
}

// drmDubbedAudioOnlyBody is what the TV client returns for an account in the
// DRM experiment on a dubbed video: two itag-140 renditions of one stream, a
// DRM-protected video format, and so no usable video at all.
const drmDubbedAudioOnlyBody = `{
	"playabilityStatus": {"status": "OK"},
	"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a"},
	"streamingData": {"adaptiveFormats": [
		{"itag": 140, "url": "https://tv/a-en", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\"", "audioTrack": {"id": "en.4", "displayName": "English original", "audioIsDefault": true}},
		{"itag": 140, "url": "https://tv/a-ja", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\"", "audioTrack": {"id": "ja.3", "displayName": "Japanese"}},
		{"itag": 137, "url": "https://tv/v", "mimeType": "video/mp4; codecs=\"avc1.640028\"", "width": 1920, "height": 1080, "drmFamilies": ["WIDEVINE"]}
	]}
}`

// drmDubbedAdequateBody is the same video from a cookieless client: the SAME
// two renditions again, a clean video format, and one more DRM entry.
const drmDubbedAdequateBody = `{
	"playabilityStatus": {"status": "OK"},
	"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a"},
	"streamingData": {"adaptiveFormats": [
		{"itag": 140, "url": "https://vis/a-en", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\"", "audioTrack": {"id": "en.4", "displayName": "English original", "audioIsDefault": true}},
		{"itag": 140, "url": "https://vis/a-ja", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\"", "audioTrack": {"id": "ja.3", "displayName": "Japanese"}},
		{"itag": 299, "url": "https://vis/v", "mimeType": "video/mp4; codecs=\"avc1.64002a\"", "width": 1920, "height": 1080},
		{"itag": 136, "url": "https://vis/drm", "mimeType": "video/mp4; codecs=\"avc1.4d401f\"", "width": 1280, "height": 720, "drmFamilies": ["PLAYREADY"]}
	]}
}`

// drmDubbedCascade points the public cascade at the two bodies above: TV
// answers with audio only (so the cookieless chain runs) and VISIONOS answers
// adequately. Both responses carry DRM and both carry the same two renditions
// of itag 140, which is what makes the summed/collapsed distinction visible.
func drmDubbedCascade(t *testing.T) *clientKeyedTransport {
	t.Helper()
	stubWatchPage(t)
	tr := &clientKeyedTransport{responses: map[string]struct {
		status int
		body   string
	}{
		"7":   {http.StatusOK, drmDubbedAudioOnlyBody},
		"101": {http.StatusOK, drmDubbedAdequateBody},
	}}
	orig := apiClient
	apiClient = &http.Client{Transport: tr}
	t.Cleanup(func() { apiClient = orig })
	return tr
}

// TestFormatDiagCarriesThePoolLevelCounts pins the two cascade-wide figures
// FormatDiag hands the worker, and the DIFFERENT arithmetic each one needs.
//
// DRM entries never reach a pool — they are dropped at parse — so there is no
// pool identity to collapse them against and the count is SUMMED over the
// responses (1 from TV + 1 from VISIONOS = 2). Alternate renditions do reach
// the pool, where dedup merges the copies several clients each returned, so
// summing the per-response counts would report the same rendition twice; the
// figure is taken where the collapse actually happens, at the pool (1).
//
// Mutants this kills:
//   - the renditions summed per response instead of counted at the pool → 2
//   - the DRM counts not accumulated across the cascade                 → 1
//   - the counts left on the per-client VideoInfo, unstamped at the exit → 0
func TestFormatDiagCarriesThePoolLevelCounts(t *testing.T) {
	tr := drmDubbedCascade(t)

	info, err := newRetryTestAPI().GetVideoInfoPublic(context.Background(), "test1234567")
	if err != nil {
		t.Fatalf("GetVideoInfoPublic: %v", err)
	}
	if !slices.Contains(tr.calls, "7") || !slices.Contains(tr.calls, "101") {
		t.Fatalf("calls = %v, want both clients — only one response was parsed", tr.calls)
	}
	if info.FormatDiag.DRMSkipped != 2 {
		t.Errorf("DRMSkipped = %d, want 2 — one entry from each of the two responses, summed",
			info.FormatDiag.DRMSkipped)
	}
	if info.FormatDiag.CollapsedRenditions != 1 {
		t.Errorf("CollapsedRenditions = %d, want 1 — both clients returned the same ja.3 rendition, and the pool collapses it once",
			info.FormatDiag.CollapsedRenditions)
	}
}

// TestAllClientsFailedIsReportedAsAnError is upstream's second raise:
// `elif not prs: raise ExtractorError('Failed to extract any player response')`
// (_video.py:3188-3189), the sibling of the IP-block verdict Task 3 ported.
//
// Row #58 made the public path carry on past a TV failure like the
// authenticated one — which meant a cascade where EVERY client fails and the
// watch page carried no player response returned (&VideoInfo{}, nil), and the
// worker rendered that as its diagnosis-free "unhandled status: ". Both paths
// had that hole; the single exit closes both. The last client error is wrapped
// rather than replaced, so worker/probe_classify.go still finds the
// "HTTP <code>" it keys on.
//
// Mutants this kills:
//   - the guard removed                    → the first two subtests get (&VideoInfo{}, nil)
//   - the guard placed BEFORE the IP-block
//     branch                               → "a substitution still wins" gets the wrong error
//   - the error not wrapping tally.lastErr → the "HTTP 404" assertion fails
//   - the guard ignoring wpParsed          → "a watch-page response is a survivor" fails
//   - the guard firing when a client
//     survived                             → "one clean client is unaffected" fails
func TestAllClientsFailedIsReportedAsAnError(t *testing.T) {
	noClientAnswers := func(t *testing.T) {
		t.Helper()
		orig := apiClient
		apiClient = &http.Client{Transport: &clientKeyedTransport{responses: map[string]struct {
			status int
			body   string
		}{}}}
		t.Cleanup(func() { apiClient = orig })
	}

	t.Run("public", func(t *testing.T) {
		stubWatchPage(t)
		noClientAnswers(t)

		info, err := newRetryTestAPI().GetVideoInfoPublic(context.Background(), "test1234567")
		if err == nil {
			t.Fatalf("info = %+v, err = nil — the worker renders that as \"unhandled status: \"", info)
		}
		if info != nil {
			t.Errorf("info = %+v, want nil alongside the error", info)
		}
		if !strings.HasPrefix(err.Error(), "failed to extract any player response: ") {
			t.Errorf("err = %q, want upstream's wording first", err)
		}
		if !strings.Contains(err.Error(), "HTTP 404") {
			t.Errorf("err = %q — the last client error must be WRAPPED, so probe_classify.go still sees the status", err)
		}
	})

	t.Run("authenticated", func(t *testing.T) {
		stubWatchPage(t)
		noClientAnswers(t)

		info, err := newRetryTestAPI().GetVideoInfoAuthenticated(context.Background(), "test1234567")
		if err == nil {
			t.Fatalf("info = %+v, err = nil — the authenticated path had the same hole", info)
		}
		if !strings.HasPrefix(err.Error(), "failed to extract any player response: ") {
			t.Errorf("err = %q, want upstream's wording first", err)
		}
		if !strings.Contains(err.Error(), "HTTP 404") {
			t.Errorf("err = %q, want the last client error wrapped", err)
		}
	})

	t.Run("one clean client is unaffected", func(t *testing.T) {
		stubWatchPage(t)
		orig := apiClient
		apiClient = &http.Client{Transport: &clientKeyedTransport{responses: map[string]struct {
			status int
			body   string
		}{
			"101": {http.StatusOK, adequateOKBody}, // VISIONOS survives
		}}}
		t.Cleanup(func() { apiClient = orig })

		info, err := newRetryTestAPI().GetVideoInfoPublic(context.Background(), "test1234567")
		if err != nil {
			t.Fatalf("err = %v, want none — one client survived", err)
		}
		if info == nil || len(info.Formats) == 0 {
			t.Fatalf("info = %+v, want VISIONOS's formats", info)
		}
	})

	t.Run("a watch-page response is a survivor", func(t *testing.T) {
		origFetch := fetchWatchPage
		fetchWatchPage = func(context.Context, string, string) (*WatchPageResult, error) {
			return &WatchPageResult{
				Ytcfg:          DefaultYtcfg(),
				PlayerResponse: decodePlayerJSON(t, adequateOKBody),
			}, nil
		}
		t.Cleanup(func() { fetchWatchPage = origFetch })
		noClientAnswers(t)

		info, err := newRetryTestAPI().GetVideoInfoPublic(context.Background(), "test1234567")
		if err != nil {
			t.Fatalf("err = %v, want none — the watch page's own player response is in `prs`", err)
		}
		if info == nil || len(info.Formats) == 0 {
			t.Fatalf("info = %+v, want the watch page's formats", info)
		}
	})

	t.Run("a substitution still wins the verdict", func(t *testing.T) {
		stubWatchPage(t)
		orig := apiClient
		apiClient = &http.Client{Transport: &clientKeyedTransport{responses: map[string]struct {
			status int
			body   string
		}{
			"7": {http.StatusOK, substituteBody}, // the rest 404
		}}}
		t.Cleanup(func() { apiClient = orig })

		_, err := newRetryTestAPI().GetVideoInfoPublic(context.Background(), "test1234567")
		if !errors.Is(err, ErrAllClientsMismatched) {
			t.Fatalf("err = %v, want ErrAllClientsMismatched — a positive substitution signal outranks plain exhaustion", err)
		}
	})
}

// TestDRMSkipIsSilentOnProbesAndNamesTheClientOnce covers the two halves of
// the DRM report's audience rule. A probe is not an extraction: the quality
// monitor and the members-only waiting-room poll call
// ProbeVideoStatusAuthenticated every 30 s, and an account in the tv-client
// DRM experiment gets DRM formats in nearly every one of those responses — a
// Warn there is a permanent wall at the default log level, so the probe path
// reports at Debug. A real extraction keeps upstream's once-per-run Warn, and
// names the client the way upstream's message embeds client_name.
//
// Mutants this kills:
//   - the probe path warning                → the first subtest counts 1
//   - the client name dropped from the Warn → the second subtest
//   - the Warn moved to Debug everywhere    → the second subtest counts 0
func TestDRMSkipIsSilentOnProbesAndNamesTheClientOnce(t *testing.T) {
	drmWarns := func(lg *warnCapturingLogger) []string {
		var out []string
		for _, w := range lg.warns {
			if strings.Contains(w, "DRM") {
				out = append(out, w)
			}
		}
		return out
	}

	t.Run("a probe-only call never warns", func(t *testing.T) {
		tr := &clientKeyedTransport{responses: map[string]struct {
			status int
			body   string
		}{
			"7": {http.StatusOK, drmDubbedAudioOnlyBody},
		}}
		orig := apiClient
		apiClient = &http.Client{Transport: tr}
		t.Cleanup(func() { apiClient = orig })

		lg := &warnCapturingLogger{}
		p := NewPlayerAPI(NewAuth(cookies.NewCookieJar(), noopLogger{}), lg)

		if _, err := p.ProbeVideoStatusAuthenticated(context.Background(), "test1234567", "vd"); err != nil {
			t.Fatalf("ProbeVideoStatusAuthenticated: %v", err)
		}
		if got := drmWarns(lg); len(got) != 0 {
			t.Errorf("a 30 s probe logged %d DRM warnings, want 0 (Debug only): %q", len(got), got)
		}
	})

	t.Run("an extraction warns once, naming the first client", func(t *testing.T) {
		drmDubbedCascade(t)

		lg := &warnCapturingLogger{}
		p := NewPlayerAPI(NewAuth(cookies.NewCookieJar(), noopLogger{}), lg)
		if _, err := p.GetVideoInfoPublic(context.Background(), "test1234567"); err != nil {
			t.Fatalf("GetVideoInfoPublic: %v", err)
		}

		got := drmWarns(lg)
		if len(got) != 1 {
			t.Fatalf("one extraction logged %d DRM warnings, want exactly 1: %q", len(got), got)
		}
		if !strings.Contains(got[0], constants.TVDowngradedClient.ClientName) {
			t.Errorf("DRM warning = %q, want it to name %q — upstream's message embeds client_name",
				got[0], constants.TVDowngradedClient.ClientName)
		}
	})
}
