package youtube

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestMainVariantSTS: a page's ytcfg STS is cut to its first five digits,
// the signature timestamp of the main variant Moombox solves with — yt-dlp's
// `int(str(sts)[:5])` (b2e6bb0dd, #18126). A regular variant's is untouched.
//
// Mutants: the cut dropped (20779001 stays), or taking the last five digits.
func TestMainVariantSTS(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{0, 0},
		{20381, 20381},
		{99999, 99999},
		{20779001, 20779},
		{207790, 20779},
		{2077900123, 20779},
	} {
		if got := mainVariantSTS(tc.in); got != tc.want {
			t.Errorf("mainVariantSTS(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// otherVariantPageBody is a watch page's own player response: formats and
// both manifests, each of which only the page's own player could solve.
const otherVariantPageBody = `{
	"playabilityStatus": {"status": "OK"},
	"videoDetails": {"videoId": "test1234567", "title": "the page's title", "author": "a"},
	"streamingData": {
		"dashManifestUrl": "https://example.com/page.mpd",
		"hlsManifestUrl": "https://example.com/page.m3u8",
		"adaptiveFormats": [
			{"itag": 313, "url": "https://example.com/page-v", "mimeType": "video/webm; codecs=\"vp9\"", "width": 3840, "height": 2160},
			{"itag": 251, "url": "https://example.com/page-a", "mimeType": "audio/webm; codecs=\"opus\""}
		]
	}
}`

// stubWatchPageOnVariant serves a watch page whose ytcfg states sts and whose
// own player response is otherVariantPageBody.
func stubWatchPageOnVariant(t *testing.T, sts int) {
	t.Helper()
	var pr map[string]any
	if err := json.Unmarshal([]byte(otherVariantPageBody), &pr); err != nil {
		t.Fatalf("watch-page player response: %v", err)
	}
	orig := fetchWatchPage
	fetchWatchPage = func(context.Context, string, string) (*WatchPageResult, error) {
		ytcfg := DefaultYtcfg()
		ytcfg.STS = sts
		ytcfg.PlayerURL = iframePlayerURL
		return &WatchPageResult{Ytcfg: ytcfg, PlayerResponse: pr}, nil
	}
	t.Cleanup(func() { fetchWatchPage = orig })
}

// TestPageOnAnotherVariantSendsTheMainSTSAndLosesItsMedia: a watch page
// served a `tcl` player states that player's eight-digit STS. Both cascades
// send the main variant's five digits on every player call (yt-dlp
// b2e6bb0dd), and nothing of the page's own player response's media — its
// formats, its DASH and HLS manifests — reaches the result, since YouTube
// built those for the page's player (yt-dlp f31e4c27a skips that response).
// A page on a regular variant keeps both, so the drop is the long STS's
// alone.
//
// Mutants: extractSTS returning ytcfg.STS as stated; either cascade's
// dropOtherVariantMedia call removed; the drop applied to every page.
func TestPageOnAnotherVariantSendsTheMainSTSAndLosesItsMedia(t *testing.T) {
	for _, path := range []string{"public", "authenticated"} {
		for _, tc := range []struct {
			name      string
			pageSTS   int
			pageMedia bool
		}{
			{name: "tcl page", pageSTS: 20779001, pageMedia: false},
			{name: "regular page", pageSTS: 20779, pageMedia: true},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				stubWatchPageOnVariant(t, tc.pageSTS)
				tr := serveBodies(t, nil)
				p := newRetryTestAPI()
				get := p.GetVideoInfoPublic
				if path == "authenticated" {
					get = p.GetVideoInfoAuthenticated
				}
				info, err := get(context.Background(), "test1234567")
				if err != nil {
					t.Fatalf("extraction: %v", err)
				}
				tv := tr.of("7")
				if len(tv) == 0 || !strings.Contains(tv[0], `"signatureTimestamp":20779,`) && !strings.Contains(tv[0], `"signatureTimestamp":20779}`) {
					t.Errorf("TV request bodies %q, want signatureTimestamp 20779", tv)
				}
				pageFormat := false
				for _, f := range info.Formats {
					if strings.HasPrefix(f.URL, "https://example.com/page-") || f.Source == "watch_page" {
						pageFormat = true
					}
				}
				pageManifest := info.DashManifestSource == "watch_page" || info.HlsManifestSource == "watch_page" ||
					strings.Contains(info.DashManifestURL, "page.mpd") || strings.Contains(info.HlsManifestURL, "page.m3u8")
				if got := pageFormat || pageManifest; got != tc.pageMedia {
					t.Errorf("page media in the result = %v (formats %v, manifests %v), want %v; DASH %q HLS %q",
						got, pageFormat, pageManifest, tc.pageMedia, info.DashManifestURL, info.HlsManifestURL)
				}
				if info.Title == "" {
					t.Errorf("the result has no title")
				}
			})
		}
	}
}
