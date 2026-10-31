package youtube

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

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
	} else if secs, ok := clientPreRoll[key]; ok {
		body = withPreRoll(body, secs)
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

// clientPreRoll is the pre-roll ad, in seconds, every response a client
// answers carries (by manifestSourceTransport's key), and watchPagePreRoll
// the watch page's own: a different length per client, so an instant that
// moved with the wrong URL — or with none — reads as the wrong number.
// sourcePreRoll is the same table by the Format.Source label each client's
// formats and manifests are recorded under.
var (
	clientPreRoll = map[string]int{"7": 7, "1s": 11, "1w": 13, "56": 17, "28": 19, "62": 23, "101": 29}
	sourcePreRoll = map[string]int{
		"tv_auth": 7, "tv_public": 7, "web_safari": 11, "web": 13, "web_embedded": 17,
		"android_vr": 19, "android_vr_dash_fallback": 19, "web_creator": 23, "visionos": 29, "watch_page": watchPagePreRoll,
	}
)

const watchPagePreRoll = 31

// withPreRoll returns the player response body with one unskippable
// AD_PLACEMENT_KIND_START ad of secs seconds added (or body itself when it is
// not a JSON object).
func withPreRoll(body string, secs int) string {
	var pr map[string]any
	if json.Unmarshal([]byte(body), &pr) != nil {
		return body
	}
	pr["adPlacements"] = []any{startPlacement("AD_PLACEMENT_KIND_START", adRenderer(secs, -1))}
	blob, err := json.Marshal(pr)
	if err != nil {
		return body
	}
	return string(blob)
}

// preRollTestAPI is newRetryTestAPI on preRollNow, so each response's
// pre-roll instant is preRollBase plus its client's ad.
func preRollTestAPI() *PlayerAPI {
	p := newRetryTestAPI()
	p.playbackClock = func() time.Time { return preRollNow }
	return p
}

// wantAvailableAt is the instant a format or manifest recorded under source
// must carry.
func wantAvailableAt(t *testing.T, source string) time.Time {
	t.Helper()
	secs, ok := sourcePreRoll[source]
	if !ok {
		t.Fatalf("no pre-roll ad known for source %q", source)
	}
	return preRollBase.Add(time.Duration(secs) * time.Second)
}

// assertAvailabilityMovesWithItsSource checks that every manifest and every
// pooled format (its token-free shadow too) carries the pre-roll instant of
// the response it came from — the client its source names — however the
// cascade handed it on. A format with no source (a probe's, never pooled) is
// not checked; its manifests are.
func assertAvailabilityMovesWithItsSource(t *testing.T, info *VideoInfo) {
	t.Helper()
	check := func(what, url, source string, got time.Time) {
		t.Helper()
		if url == "" {
			if !got.IsZero() {
				t.Errorf("%s: no URL, yet available at %v", what, got)
			}
			return
		}
		if want := wantAvailableAt(t, source); !got.Equal(want) {
			t.Errorf("%s from %s available at %v, want %v — that client's own response's pre-roll", what, source, got, want)
		}
	}
	check("DASH manifest", info.DashManifestURL, info.DashManifestSource, info.DashManifestAvailableAt)
	check("HLS manifest", info.HlsManifestURL, info.HlsManifestSource, info.HlsManifestAvailableAt)
	for _, f := range info.Formats {
		if f.Source != "" {
			check(fmt.Sprintf("itag %d", f.Itag), f.URL, f.Source, f.AvailableAt)
		}
		if alt := f.TokenFreeAlternate; alt != nil {
			check(fmt.Sprintf("itag %d shadow", f.Itag), alt.URL, alt.Source, alt.AvailableAt)
		}
	}
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
	assertAvailabilityMovesWithItsSource(t, info)
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
	if err := json.Unmarshal([]byte(withPreRoll(body, watchPagePreRoll)), &pr); err != nil {
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
// whether the manifest takes a GVS PO token (GvsTokenRequired: tv,
// web_embedded, visionos and android_vr* ride bare, as upstream does). An
// unrecorded source reads as bare downstream — safe, but it would silently
// strip the token from a WEB-family manifest — so each scenario below
// drives one assignment site and asserts both the label and the URL⇔source
// invariant.
//
// Mutant: stamping any one site with "" (or the wrong label) fails the
// scenario named after it.
//
// Every response here also carries a pre-roll ad of its client's own length
// (clientPreRoll), and each manifest and pooled format must come out with its
// own response's instant (assertAvailabilityMovesWithItsSource): the worker
// waits on whatever it ends up fetching, so an instant left behind at an
// adoption is a URL requested before its ad is over. Mutants (each run):
// dropping the instant from the web_safari, web_embedded, android_vr (authed
// or public) or cookieless adoption, or from either half of the watch-page
// merge, fails the scenario named after that site.
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
			info, err := preRollTestAPI().GetVideoInfoAuthenticated(context.Background(), "test1234567")
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
			info, err := preRollTestAPI().GetVideoInfoPublic(context.Background(), "test1234567")
			if err != nil {
				t.Fatalf("GetVideoInfoPublic: %v", err)
			}
			assertManifestSources(t, info, tc.dash, tc.dashSrc, tc.hls, tc.hlsSrc)
		})
	}

	t.Run("probe/ProbeVideoStatus is android_vr", func(t *testing.T) {
		useManifestSourceTransport(t, map[string]string{"28": srcBody(true, "adequate", dVR, hVis)})
		info, err := preRollTestAPI().ProbeVideoStatus(context.Background(), "test1234567", "vd")
		if err != nil {
			t.Fatalf("ProbeVideoStatus: %v", err)
		}
		assertManifestSources(t, info, dVR, "android_vr", hVis, "android_vr")
	})

	t.Run("probe/ProbeVideoStatusAuthenticated is tv_auth", func(t *testing.T) {
		useManifestSourceTransport(t, map[string]string{"7": srcBody(true, "adequate", dTV, hTV)})
		info, err := preRollTestAPI().ProbeVideoStatusAuthenticated(context.Background(), "test1234567", "vd")
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
