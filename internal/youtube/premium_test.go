package youtube

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// premiumTopbar is a ytInitialData topbar whose logo renderer is logo — the
// shape yt-dlp's _is_premium_subscriber reads (_video.py:3899-3908):
// topbar.desktopTopbarRenderer.logo.topbarLogoRenderer.
func premiumTopbar(logo string) string {
	return `"topbar":{"desktopTopbarRenderer":{"logo":{"topbarLogoRenderer":` + logo + `}}}`
}

// premiumPage is a watch page whose ytInitialData carries topbar (a
// premiumTopbar, or "" for none) after a filler renderer, and whose ytcfg
// states loggedIn.
func premiumPage(topbar string, loggedIn bool) string {
	login := "false"
	if loggedIn {
		login = "true"
	}
	data := `{"contents":{"twoColumnWatchNextResults":{}}`
	if topbar != "" {
		data += "," + topbar
	}
	data += "}"
	return `<!DOCTYPE html><html><head><script>ytcfg.set({"LOGGED_IN":` + login + `,"VISITOR_DATA":"vd"});</script>` +
		`<script nonce="q">var ytInitialData = ` + data + `;</script></head><body></body></html>`
}

// The topbar logo renderers a Premium subscriber's page and an ordinary one
// carry. The Premium one is the shape yt-dlp tests for (iconType
// YOUTUBE_PREMIUM_LOGO); the tooltip one is upstream's second test, for a
// renderer whose icon type is something else.
const (
	premiumLogo        = `{"iconImage":{"iconType":"YOUTUBE_PREMIUM_LOGO"},"tooltipText":{"runs":[{"text":"YouTube Home"}]}}`
	premiumTooltipLogo = `{"iconImage":{"iconType":"YOUTUBE_LOGO"},"tooltipText":{"runs":[{"text":"YouTube "},{"text":"Premium"},{"text":" Home"}]}}`
	premiumSimpleLogo  = `{"iconImage":{"iconType":"YOUTUBE_LOGO"},"tooltipText":{"simpleText":"YouTube PREMIUM home"}}`
	ordinaryLogo       = `{"iconImage":{"iconType":"YOUTUBE_LOGO"},"tooltipText":{"runs":[{"text":"YouTube Home"}]}}`
)

// TestExtractPremiumSubscriber pins yt-dlp's _is_premium_subscriber
// (_video.py:3899-3908) on the page bytes: the topbar logo renderer's
// iconType YOUTUBE_PREMIUM_LOGO, or "premium" in its lower-cased tooltip
// text (runs or simpleText), is a Premium subscriber; an ordinary logo, a
// page with no topbar and a page with no ytInitialData are not. A malformed
// first candidate (an assignment that is no JSON) does not deny the real blob
// behind it, and a Premium logo outside the topbar path is no verdict.
//
// Mutants (each run): dropping the iconType test fails the icon row; dropping
// the tooltip test fails the runs and simpleText rows; matching the tooltip
// case-sensitively fails the simpleText row; reading "premium" anywhere in
// the page fails the elsewhere row; accepting the first candidate whatever
// it decodes to fails the malformed row.
func TestExtractPremiumSubscriber(t *testing.T) {
	for _, tc := range []struct {
		name string
		page string
		want bool
	}{
		{"premium icon", premiumPage(premiumTopbar(premiumLogo), true), true},
		{"premium in the tooltip runs", premiumPage(premiumTopbar(premiumTooltipLogo), true), true},
		{"premium in the tooltip simpleText", premiumPage(premiumTopbar(premiumSimpleLogo), true), true},
		{"ordinary logo", premiumPage(premiumTopbar(ordinaryLogo), true), false},
		{"no topbar", premiumPage("", true), false},
		{"no ytInitialData", `<html><script>ytcfg.set({"LOGGED_IN":true});</script></html>`, false},
		{"the Premium logo elsewhere", premiumPage(`"promo":{"iconImage":{"iconType":"YOUTUBE_PREMIUM_LOGO"}}`, true), false},
		{
			"a malformed candidate ahead of the real blob",
			`<script>var ytInitialData = {"x":tru};</script>` + premiumPage(premiumTopbar(premiumLogo), true),
			true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractPremiumSubscriber([]byte(tc.page)); got != tc.want {
				t.Errorf("extractPremiumSubscriber = %v, want %v", got, tc.want)
			}
		})
	}
}

// stubPageGet answers every page fetch with body, as the watch page's own
// GET (pageGet) would, and counts the fetches.
func stubPageGet(t *testing.T, body string) {
	t.Helper()
	orig := pageGet
	pageGet = func(ctx context.Context, pageURL string, _ time.Duration, _ map[string]string) (*http.Response, context.CancelFunc, error) {
		u, _ := url.Parse(pageURL)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
			Request:    &http.Request{URL: u},
		}, func() {}, nil
	}
	t.Cleanup(func() { pageGet = orig })
}

// TestFetchWatchPageReadsPremiumOnlySignedIn pins where the verdict is read:
// FetchWatchPage claims Premium only off a page YouTube answered as signed in,
// as upstream reads it only `if self.is_authenticated`. A signed-out page
// that shows the Premium logo — it never should, but a forged or cached page
// could — is no Premium session, and keeps the GVS token mandatory.
//
// Mutants (each run): reading the topbar whatever the session fails the
// signed-out row; never reading it fails the signed-in row.
func TestFetchWatchPageReadsPremiumOnlySignedIn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		loggedIn bool
		want     bool
	}{
		{"signed in", true, true},
		{"signed out", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubPageGet(t, premiumPage(premiumTopbar(premiumLogo), tc.loggedIn))
			wp, err := FetchWatchPage(context.Background(), "test1234567", "")
			if err != nil {
				t.Fatalf("FetchWatchPage: %v", err)
			}
			if wp.IsPremium != tc.want {
				t.Errorf("IsPremium = %v (session %q), want %v", wp.IsPremium, wp.SessionAuth, tc.want)
			}
		})
	}
}

// TestWithAttestationStampsPremiumAndGuessedBinding pins the hand-off from the
// page to the strategies: withAttestation carries the page's Premium verdict
// onto the VideoInfo, and marks the GVS binding a guess exactly when the
// session is signed in and the page stated no datasync ID, the video-ID
// experiment on or off (gvsContentBinding) — the binding D12 counts as a
// missing token.
//
// Mutants (each run): not stamping IsPremium fails the Premium row; stamping
// the guess from the binding kind alone (visitor data) fails the signed-out
// row; never stamping it fails the guessed row.
func TestWithAttestationStampsPremiumAndGuessedBinding(t *testing.T) {
	for _, tc := range []struct {
		name        string
		wp          *WatchPageResult
		wantPremium bool
		wantGuess   bool
		wantKind    string
	}{
		{
			name:        "a Premium session with its datasync ID",
			wp:          &WatchPageResult{Ytcfg: &YtcfgData{DataSyncID: "ds", VisitorData: "vd"}, SessionAuth: SessionAuthLoggedIn, IsPremium: true},
			wantPremium: true, wantKind: BindingDataSyncID,
		},
		{
			name:      "signed in, no datasync ID: a guessed visitor-data binding",
			wp:        &WatchPageResult{Ytcfg: &YtcfgData{VisitorData: "vd"}, SessionAuth: SessionAuthLoggedIn},
			wantGuess: true, wantKind: BindingVisitorData,
		},
		{
			// Upstream refuses this mint (fetch_po_token, _video.py:2815-2819:
			// no experiment exemption), so it is a guess (wave FC decision 6).
			name:      "signed in under the video-ID experiment, no datasync ID: a guess",
			wp:        &WatchPageResult{Ytcfg: &YtcfgData{GvsBindToVideoID: true, VisitorData: "vd"}, SessionAuth: SessionAuthLoggedIn},
			wantGuess: true, wantKind: BindingVideoID,
		},
		{
			name:     "signed in under the video-ID experiment with its datasync ID: no guess",
			wp:       &WatchPageResult{Ytcfg: &YtcfgData{GvsBindToVideoID: true, DataSyncID: "ds", VisitorData: "vd"}, SessionAuth: SessionAuthLoggedIn},
			wantKind: BindingVideoID,
		},
		{
			name:     "signed out: visitor data is the binding, no guess",
			wp:       &WatchPageResult{Ytcfg: &YtcfgData{VisitorData: "vd"}, SessionAuth: SessionAuthLoggedOut},
			wantKind: BindingVisitorData,
		},
		{
			name:     "no watch page: no verdict either way",
			wantKind: BindingVideoID,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := withAttestation(&VideoInfo{}, tc.wp, "vid123")
			if info.IsPremium != tc.wantPremium {
				t.Errorf("IsPremium = %v, want %v", info.IsPremium, tc.wantPremium)
			}
			if info.GvsBindingGuessed != tc.wantGuess || info.GvsBindingKind != tc.wantKind {
				t.Errorf("binding (%q, guessed %v), want (%q, guessed %v)", info.GvsBindingKind, info.GvsBindingGuessed, tc.wantKind, tc.wantGuess)
			}
		})
	}
}

// TestGvsTokenMandatory pins the per-stream half of the GVS policy:
// upstream's require_po_token, `stream_id[0] not in ['18'] and
// gvs_pot_required(policy, is_premium_subscriber, ...)` (_video.py:3267-3271,
// 3516-3518). A web-family format needs the token unless it is itag 18 or the
// session is Premium; a token-free client's never does, nor does no format.
// GvsTokenRequired — whether a minted token is attached — answers true for
// every web-family row: the waivers make the token optional, not unwanted.
//
// Mutants (each run): dropping the itag 18 test fails the web 18 row;
// dropping the Premium waiver fails both Premium rows; asking the client
// alone (GvsTokenRequired) fails every waived row.
func TestGvsTokenMandatory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		f       *Format
		premium bool
		want    bool
	}{
		{"web_creator 137", &Format{Itag: 137, Source: "web_creator"}, false, true},
		{"watch_page 251", &Format{Itag: 251, Source: "watch_page"}, false, true},
		{"web_safari 18", &Format{Itag: 18, Source: "web_safari"}, false, false},
		{"web 137, Premium", &Format{Itag: 137, Source: "web"}, true, false},
		{"web_creator 616, Premium", &Format{Itag: 616, Source: "web_creator"}, true, false},
		{"tv_auth 137", &Format{Itag: 137, Source: "tv_auth"}, false, false},
		{"visionos 18", &Format{Itag: 18, Source: "visionos"}, false, false},
		{"no format", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatGvsTokenMandatory(tc.f, tc.premium); got != tc.want {
				t.Errorf("FormatGvsTokenMandatory = %v, want %v", got, tc.want)
			}
			if tc.f != nil {
				if got, want := GvsTokenMandatory(tc.f.Source, tc.premium), GvsTokenRequired(tc.f.Source) && !tc.premium; got != want {
					t.Errorf("GvsTokenMandatory(%q, %v) = %v, want %v", tc.f.Source, tc.premium, got, want)
				}
			}
		})
	}
}

// clientCountingTransport is manifestSourceTransport counting the player
// calls each client key receives. A key in refuse is answered with that HTTP
// status instead — a 400, say, which the player call does not retry.
type clientCountingTransport struct {
	inner  manifestSourceTransport
	refuse map[string]int
	mu     sync.Mutex
	calls  map[string]int
}

func (tr *clientCountingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	key := req.Header.Get("X-YouTube-Client-Name")
	tr.mu.Lock()
	tr.calls[key]++
	tr.mu.Unlock()
	if status, ok := tr.refuse[key]; ok {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header), Request: req}, nil
	}
	return tr.inner.RoundTrip(req)
}

func (tr *clientCountingTransport) count(key string) int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.calls[key]
}

// premiumBody is a VOD player response carrying the Premium-only itag 616
// beside the ordinary 299 + 140 split, as web_creator answers a Premium
// subscriber.
const premiumBody = `{
	"playabilityStatus": {"status": "OK"},
	"videoDetails": {"videoId": "test1234567", "title": "t", "author": "a", "isLive": false, "isLiveContent": false},
	"streamingData": {"adaptiveFormats": [
		{"itag": 616, "url": "https://example.com/premium", "contentLength": "1000", "mimeType": "video/mp4; codecs=\"vp09.00.40.08\"", "width": 1920, "height": 1080},
		{"itag": 299, "url": "https://example.com/v", "contentLength": "1000", "mimeType": "video/mp4; codecs=\"avc1.64002a\"", "width": 1920, "height": 1080},
		{"itag": 140, "url": "https://example.com/a", "contentLength": "1000", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\""}
	]}
}`

// TestAuthenticatedCascadeFetchesWebCreatorForPremium pins owner decision D11
// in the cascade: a Premium session's extraction asks web_creator — the lead
// of upstream's _DEFAULT_PREMIUM_CLIENTS (_video.py:147, 2973-2974) — and
// pools its formats as a contributor, not the lead: the stream tv returned
// keeps tv's copy, and web_creator adds what tv lacks (the Premium 616). A
// session without Premium keeps asking web_creator only as the fallback, so
// a pool tv filled never sees it; and the fallback reuses the Premium answer
// rather than asking twice — a refusal included. A waiting room asks no
// web_creator at all, Premium or not: an upcoming stream has no formats to
// contribute, and every waiting-room poll would pay one more player call
// (owner decision O-I's short-circuit; see
// TestUpcomingPollSkipsTheFormatHuntingFallbacksAuthenticated).
//
// Mutants (each run): skipping the Premium fetch fails the Premium row's call
// count and its 616; pooling web_creator ahead of tv (a lower AuthLevel)
// fails the 299 row's source; fetching web_creator again in the fallback
// fails the no-formats row's single call; asking it again after the Premium
// fetch failed fails the refused row's single call; asking it in a waiting
// room fails the upcoming row; asking it without Premium fails the ordinary
// row.
func TestAuthenticatedCascadeFetchesWebCreatorForPremium(t *testing.T) {
	for _, tc := range []struct {
		name       string
		premium    bool
		tv         string
		refuseWC   bool // web_creator answers 400
		wantCalls  int
		want616    bool
		wantSource string // the 299 winner's client
	}{
		{name: "Premium: web_creator contributes", premium: true, tv: srcBody(false, "whole", "", ""), wantCalls: 1, want616: true, wantSource: "tv_auth"},
		{name: "Premium, tv has no formats: asked once", premium: true, tv: srcBody(false, "none", "", ""), wantCalls: 1, want616: true, wantSource: "web_creator"},
		{name: "Premium, web_creator refused: not asked again", premium: true, tv: srcBody(false, "none", "", ""), refuseWC: true, wantCalls: 1},
		{name: "Premium, waiting room: not asked", premium: true, tv: upcomingTVBody, wantCalls: 0},
		{name: "no Premium: tv's pool is enough", tv: srcBody(false, "whole", "", ""), wantCalls: 0, wantSource: "tv_auth"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := fetchWatchPage
			fetchWatchPage = func(context.Context, string, string) (*WatchPageResult, error) {
				return &WatchPageResult{Ytcfg: DefaultYtcfg(), SessionAuth: SessionAuthLoggedIn, IsPremium: tc.premium}, nil
			}
			t.Cleanup(func() { fetchWatchPage = orig })
			tr := &clientCountingTransport{inner: manifestSourceTransport{responses: map[string]string{"7": tc.tv, "62": premiumBody}}, calls: map[string]int{}}
			if tc.refuseWC {
				tr.refuse = map[string]int{"62": http.StatusBadRequest}
			}
			origClient := apiClient
			apiClient = &http.Client{Transport: tr}
			t.Cleanup(func() { apiClient = origClient })

			info, err := newRetryTestAPI().GetVideoInfoAuthenticated(context.Background(), "test1234567")
			if err != nil {
				t.Fatalf("GetVideoInfoAuthenticated: %v", err)
			}
			if n := tr.count("62"); n != tc.wantCalls {
				t.Errorf("web_creator asked %d times, want %d", n, tc.wantCalls)
			}
			if info.IsPremium != tc.premium {
				t.Errorf("IsPremium = %v, want %v", info.IsPremium, tc.premium)
			}
			var got616 bool
			source299 := ""
			for _, f := range info.Formats {
				switch f.Itag {
				case 616:
					got616 = f.Source == "web_creator"
				case 299:
					source299 = f.Source
				}
			}
			if got616 != tc.want616 {
				t.Errorf("web_creator's Premium 616 pooled = %v, want %v (formats %v)", got616, tc.want616, info.Formats)
			}
			if source299 != tc.wantSource {
				t.Errorf("itag 299 served from %q, want %q — web_creator contributes, it does not lead", source299, tc.wantSource)
			}
		})
	}
}
