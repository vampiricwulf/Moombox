package youtube

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

// manifestSourceTransport answers /youtubei player requests with a canned
// body per client. It is clientKeyedTransport with one more key: WEB_SAFARI
// and WEB share X-YouTube-Client-Name "1" and differ only by User-Agent, so
// "1s" answers web_safari and "1w" answers the plain web fallback. A client
// with no entry gets a 404, which the cascade treats as that client failing.
type manifestSourceTransport struct {
	responses map[string]string
}

func (tr *manifestSourceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	key := req.Header.Get("X-YouTube-Client-Name")
	if key == "1" {
		if req.Header.Get("User-Agent") == constants.UserAgents.WebSafari {
			key = "1s"
		} else {
			key = "1w"
		}
	}
	status, body := http.StatusOK, tr.responses[key]
	if body == "" {
		status, body = http.StatusNotFound, "{}"
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// srcBody builds a player response. formats is "adequate" (split video +
// audio), "whole" (the same split, but complete files with a contentLength),
// "audio" (audio only) or "none". dash and hls are the manifest URLs it
// carries ("" for none).
func srcBody(live bool, formats, dash, hls string) string {
	var list string
	switch formats {
	case "adequate":
		list = `{"itag": 299, "url": "https://example.com/v", "mimeType": "video/mp4; codecs=\"avc1.64002a\"", "width": 1920, "height": 1080},
			{"itag": 140, "url": "https://example.com/a", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\""}`
	case "whole":
		list = `{"itag": 299, "url": "https://example.com/v", "contentLength": "1000", "mimeType": "video/mp4; codecs=\"avc1.64002a\"", "width": 1920, "height": 1080},
			{"itag": 140, "url": "https://example.com/a", "contentLength": "1000", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\""}`
	case "audio":
		list = `{"itag": 140, "url": "https://example.com/a", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\""}`
	}
	sd := []string{fmt.Sprintf(`"adaptiveFormats": [%s]`, list)}
	if dash != "" {
		sd = append(sd, fmt.Sprintf(`"dashManifestUrl": %q`, dash))
	}
	if hls != "" {
		sd = append(sd, fmt.Sprintf(`"hlsManifestUrl": %q`, hls))
	}
	return fmt.Sprintf(`{
		"playabilityStatus": {"status": "OK"},
		"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a", "isLive": %t, "isLiveContent": %t},
		"streamingData": {%s}
	}`, live, live, strings.Join(sd, ", "))
}

const ageRestrictedBody = `{
	"playabilityStatus": {"status": "AGE_VERIFICATION_REQUIRED", "reason": "Sign in to confirm your age"},
	"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a"}
}`

// assertManifestSources checks the invariant every extraction must keep —
// a recorded manifest URL always carries the client it came from — and the
// exact client the scenario expects for each.
func assertManifestSources(t *testing.T, info *VideoInfo, wantDash, wantDashSrc, wantHls, wantHlsSrc string) {
	t.Helper()
	if info == nil {
		t.Fatal("extraction returned nil info")
	}
	if (info.DashManifestURL != "") != (info.DashManifestSource != "") {
		t.Errorf("DASH manifest %q recorded with source %q — a manifest URL must always carry its client",
			info.DashManifestURL, info.DashManifestSource)
	}
	if (info.HlsManifestURL != "") != (info.HlsManifestSource != "") {
		t.Errorf("HLS manifest %q recorded with source %q — a manifest URL must always carry its client",
			info.HlsManifestURL, info.HlsManifestSource)
	}
	if info.DashManifestURL != wantDash || info.DashManifestSource != wantDashSrc {
		t.Errorf("DASH = (%q, source %q), want (%q, source %q)",
			info.DashManifestURL, info.DashManifestSource, wantDash, wantDashSrc)
	}
	if info.HlsManifestURL != wantHls || info.HlsManifestSource != wantHlsSrc {
		t.Errorf("HLS = (%q, source %q), want (%q, source %q)",
			info.HlsManifestURL, info.HlsManifestSource, wantHls, wantHlsSrc)
	}
}

func useManifestSourceTransport(t *testing.T, responses map[string]string) {
	t.Helper()
	orig := apiClient
	apiClient = &http.Client{Transport: &manifestSourceTransport{responses: responses}}
	t.Cleanup(func() { apiClient = orig })
}

// stubWatchPageWithPlayerResponse is stubWatchPage with the page's own
// embedded player response, so the watch_page parse contributes manifests.
func stubWatchPageWithPlayerResponse(t *testing.T, body string) {
	t.Helper()
	var pr map[string]any
	if err := json.Unmarshal([]byte(body), &pr); err != nil {
		t.Fatalf("watch-page player response: %v", err)
	}
	orig := fetchWatchPage
	fetchWatchPage = func(context.Context, string, string) (*WatchPageResult, error) {
		return &WatchPageResult{Ytcfg: DefaultYtcfg(), PlayerResponse: pr}, nil
	}
	t.Cleanup(func() { fetchWatchPage = orig })
}

// TestExtractionStampsEveryManifestWithItsClient pins the half of the
// 2026-09-29 live-path fix that lives in the extraction cascade: every site
// that puts a DASH or HLS manifest URL on the returned VideoInfo records
// WHICH client served it, because the live strategies decide from that label
// whether the manifest takes a WebPO GVS token (visionos / android_vr /
// android_vr_dash_fallback ride bare, as upstream does). An unrecorded
// source reads as non-WebPO downstream — safe, but it would silently strip
// the token from a TV/WEB manifest — so each scenario below drives one
// assignment site and asserts both the label and the URL⇔source invariant.
//
// Mutant: stamping any one site with "" (or the wrong label) fails the
// scenario named after it.
func TestExtractionStampsEveryManifestWithItsClient(t *testing.T) {
	const (
		dTV  = "https://m.example/tv.mpd"
		hTV  = "https://m.example/tv.m3u8"
		dWS  = "https://m.example/web_safari.mpd"
		dW   = "https://m.example/web.mpd"
		dEmb = "https://m.example/emb.mpd"
		hEmb = "https://m.example/emb.m3u8"
		dVR  = "https://m.example/vr.mpd"
		dWC  = "https://m.example/wc.mpd"
		hWC  = "https://m.example/wc.m3u8"
		hVis = "https://m.example/vis.m3u8"
		dWP  = "https://m.example/wp.mpd"
		hWP  = "https://m.example/wp.m3u8"
	)

	auth := []struct {
		name      string
		responses map[string]string
		watchPage string
		dash      string
		dashSrc   string
		hls       string
		hlsSrc    string
	}{
		{
			name:      "tv_auth serves both manifests",
			responses: map[string]string{"7": srcBody(true, "adequate", dTV, hTV)},
			dash:      dTV, dashSrc: "tv_auth", hls: hTV, hlsSrc: "tv_auth",
		},
		{
			name: "web_safari DASH adopted into the TV result",
			responses: map[string]string{
				"7":  srcBody(true, "adequate", "", hTV),
				"1s": srcBody(true, "adequate", dWS, ""),
			},
			dash: dWS, dashSrc: "web_safari", hls: hTV, hlsSrc: "tv_auth",
		},
		{
			name: "plain web DASH adopted after web_safari fails",
			responses: map[string]string{
				"7":  srcBody(true, "adequate", "", hTV),
				"1w": srcBody(true, "adequate", dW, ""),
			},
			dash: dW, dashSrc: "web", hls: hTV, hlsSrc: "tv_auth",
		},
		{
			name: "web_embedded DASH adopted when TV and web have none",
			responses: map[string]string{
				"56": srcBody(true, "adequate", dEmb, ""),
				"7":  srcBody(true, "adequate", "", hTV),
				"1s": srcBody(true, "adequate", "", ""),
			},
			dash: dEmb, dashSrc: "web_embedded", hls: hTV, hlsSrc: "tv_auth",
		},
		{
			name: "android_vr DASH fallback",
			responses: map[string]string{
				"7":  srcBody(true, "adequate", "", hTV),
				"28": srcBody(true, "adequate", dVR, ""),
			},
			dash: dVR, dashSrc: "android_vr_dash_fallback", hls: hTV, hlsSrc: "tv_auth",
		},
		{
			name: "age-restricted returns the web_embedded result",
			responses: map[string]string{
				"56": srcBody(false, "adequate", dEmb, hEmb),
				"7":  ageRestrictedBody,
			},
			dash: dEmb, dashSrc: "web_embedded", hls: hEmb, hlsSrc: "web_embedded",
		},
		{
			name: "web_creator result returned when TV has no formats",
			responses: map[string]string{
				"7":  srcBody(false, "none", "", ""),
				"62": srcBody(false, "adequate", dWC, hWC),
			},
			dash: dWC, dashSrc: "web_creator", hls: hWC, hlsSrc: "web_creator",
		},
		{
			name:      "visionos result from the cookieless chain",
			responses: map[string]string{"101": srcBody(false, "adequate", "", hVis)},
			hls:       hVis, hlsSrc: "visionos",
		},
		{
			name: "cookieless chain adopts the android_vr DASH manifest",
			responses: map[string]string{
				"101": srcBody(true, "whole", "", hVis),
				"28":  srcBody(true, "whole", dVR, ""),
			},
			dash: dVR, dashSrc: "android_vr", hls: hVis, hlsSrc: "visionos",
		},
		{
			name:      "watch-page manifests merged into the TV result",
			responses: map[string]string{"7": srcBody(false, "adequate", "", "")},
			watchPage: srcBody(false, "adequate", dWP, hWP),
			dash:      dWP, dashSrc: "watch_page", hls: hWP, hlsSrc: "watch_page",
		},
	}
	for _, tc := range auth {
		t.Run("authenticated/"+tc.name, func(t *testing.T) {
			if tc.watchPage != "" {
				stubWatchPageWithPlayerResponse(t, tc.watchPage)
			} else {
				stubWatchPage(t)
			}
			useManifestSourceTransport(t, tc.responses)
			info, err := newRetryTestAPI().GetVideoInfoAuthenticated(context.Background(), "test1234567")
			if err != nil {
				t.Fatalf("GetVideoInfoAuthenticated: %v", err)
			}
			assertManifestSources(t, info, tc.dash, tc.dashSrc, tc.hls, tc.hlsSrc)
		})
	}

	public := []struct {
		name      string
		responses map[string]string
		watchPage string
		dash      string
		dashSrc   string
		hls       string
		hlsSrc    string
	}{
		{
			name:      "tv_public serves both manifests",
			responses: map[string]string{"7": srcBody(true, "adequate", dTV, hTV)},
			dash:      dTV, dashSrc: "tv_public", hls: hTV, hlsSrc: "tv_public",
		},
		{
			name: "android_vr DASH enrichment fetched fresh",
			responses: map[string]string{
				"7":  srcBody(true, "adequate", "", hTV),
				"28": srcBody(true, "adequate", dVR, ""),
			},
			dash: dVR, dashSrc: "android_vr_dash_fallback", hls: hTV, hlsSrc: "tv_public",
		},
		{
			name: "android_vr DASH enrichment reused from the chain",
			responses: map[string]string{
				"7":   srcBody(true, "audio", "", ""),
				"101": srcBody(true, "audio", "", ""),
				"28":  srcBody(true, "audio", dVR, ""),
			},
			dash: dVR, dashSrc: "android_vr_dash_fallback",
		},
		{
			name:      "visionos result from the cookieless chain",
			responses: map[string]string{"101": srcBody(false, "adequate", "", hVis)},
			hls:       hVis, hlsSrc: "visionos",
		},
		{
			name:      "watch-page manifests merged into the TV result",
			responses: map[string]string{"7": srcBody(false, "adequate", "", "")},
			watchPage: srcBody(false, "adequate", dWP, hWP),
			dash:      dWP, dashSrc: "watch_page", hls: hWP, hlsSrc: "watch_page",
		},
	}
	for _, tc := range public {
		t.Run("public/"+tc.name, func(t *testing.T) {
			if tc.watchPage != "" {
				stubWatchPageWithPlayerResponse(t, tc.watchPage)
			} else {
				stubWatchPage(t)
			}
			useManifestSourceTransport(t, tc.responses)
			info, err := newRetryTestAPI().GetVideoInfoPublic(context.Background(), "test1234567")
			if err != nil {
				t.Fatalf("GetVideoInfoPublic: %v", err)
			}
			assertManifestSources(t, info, tc.dash, tc.dashSrc, tc.hls, tc.hlsSrc)
		})
	}

	t.Run("probe/ProbeVideoStatus is android_vr", func(t *testing.T) {
		useManifestSourceTransport(t, map[string]string{"28": srcBody(true, "adequate", dVR, hVis)})
		info, err := newRetryTestAPI().ProbeVideoStatus(context.Background(), "test1234567", "vd")
		if err != nil {
			t.Fatalf("ProbeVideoStatus: %v", err)
		}
		assertManifestSources(t, info, dVR, "android_vr", hVis, "android_vr")
	})

	t.Run("probe/ProbeVideoStatusAuthenticated is tv_auth", func(t *testing.T) {
		useManifestSourceTransport(t, map[string]string{"7": srcBody(true, "adequate", dTV, hTV)})
		info, err := newRetryTestAPI().ProbeVideoStatusAuthenticated(context.Background(), "test1234567", "vd")
		if err != nil {
			t.Fatalf("ProbeVideoStatusAuthenticated: %v", err)
		}
		assertManifestSources(t, info, dTV, "tv_auth", hTV, "tv_auth")
	})
}

// TestManifestSourceJSONRoundTrip: the two new fields ride VideoInfo's JSON
// under their own keys and vanish when empty, so a VideoInfo without them
// marshals exactly as it did before they existed.
func TestManifestSourceJSONRoundTrip(t *testing.T) {
	in := VideoInfo{DashManifestURL: "d", DashManifestSource: "tv_auth", HlsManifestURL: "h", HlsManifestSource: "visionos"}
	blob, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), `"dashManifestSource":"tv_auth"`) || !strings.Contains(string(blob), `"hlsManifestSource":"visionos"`) {
		t.Errorf("marshal = %s, want dashManifestSource/hlsManifestSource keys", blob)
	}
	var out VideoInfo
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatal(err)
	}
	if out.DashManifestSource != "tv_auth" || out.HlsManifestSource != "visionos" {
		t.Errorf("round trip = (%q, %q), want (tv_auth, visionos)", out.DashManifestSource, out.HlsManifestSource)
	}
	bare, _ := json.Marshal(VideoInfo{})
	if strings.Contains(string(bare), "ManifestSource") {
		t.Errorf("empty sources must be omitted: %s", bare)
	}
}
