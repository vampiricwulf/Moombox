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

// stubWatchPageOnVariant serves a watch page whose ytcfg states sts (0: none),
// which named its player on a `tcl` path or not, and whose own player
// response is otherVariantPageBody.
func stubWatchPageOnVariant(t *testing.T, sts int, tclPath bool) {
	t.Helper()
	var pr map[string]any
	if err := json.Unmarshal([]byte(otherVariantPageBody), &pr); err != nil {
		t.Fatalf("watch-page player response: %v", err)
	}
	orig := fetchWatchPage
	fetchWatchPage = func(context.Context, string, string) (*WatchPageResult, error) {
		ytcfg := DefaultYtcfg()
		ytcfg.STS = sts
		ytcfg.PlayerOnTCL = tclPath
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
// A page on a regular variant keeps both, so the drop is the other variant's
// alone.
//
// A `tcl` page is also known by the path it named its player on, whatever
// its STS: five digits, or none in ytcfg at all. Read by its STS alone such a
// page kept media only its own player could solve. These rows are built, as
// the long-STS one is: no capture of a `tcl` watch page exists.
//
// The drop is logged at Info, once per extraction, and not at all for a
// regular page.
//
// Mutants: extractSTS returning ytcfg.STS as stated; either cascade's
// dropOtherVariantMedia call removed; the drop applied to every page;
// pageRunsOtherVariant reading the STS alone; the line back at Debug.
func TestPageOnAnotherVariantSendsTheMainSTSAndLosesItsMedia(t *testing.T) {
	for _, path := range []string{"public", "authenticated"} {
		for _, tc := range []struct {
			name      string
			pageSTS   int
			tclPath   bool
			pageMedia bool
		}{
			{name: "tcl page", pageSTS: 20779001, pageMedia: false},
			{name: "regular page", pageSTS: 20779, pageMedia: true},
			{name: "tcl path with a five-digit STS", pageSTS: 20779, tclPath: true, pageMedia: false},
			{name: "tcl path with no STS", pageSTS: 0, tclPath: true, pageMedia: false},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				stubWatchPageOnVariant(t, tc.pageSTS, tc.tclPath)
				tr := serveBodies(t, nil)
				p := newRetryTestAPI()
				logs := &infoRecorder{}
				p.logger = logs
				get := p.GetVideoInfoPublic
				if path == "authenticated" {
					get = p.GetVideoInfoAuthenticated
				}
				info, err := get(context.Background(), "test1234567")
				if err != nil {
					t.Fatalf("extraction: %v", err)
				}
				tv := tr.of("7")
				if tc.pageSTS > 0 && (len(tv) == 0 || !strings.Contains(tv[0], `"signatureTimestamp":20779,`) && !strings.Contains(tv[0], `"signatureTimestamp":20779}`)) {
					t.Errorf("TV request bodies %q, want signatureTimestamp 20779", tv)
				}
				dropLines := 0
				for _, msg := range logs.infos {
					if strings.Contains(msg, "another player variant") {
						dropLines++
					}
				}
				if want := map[bool]int{true: 0, false: 1}[tc.pageMedia]; dropLines != want {
					t.Errorf("Info lines about the dropped media = %d (%q), want %d", dropLines, logs.infos, want)
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

// infoRecorder keeps the Info lines an extraction logs.
type infoRecorder struct {
	noopLogger
	infos []string
}

func (r *infoRecorder) Info(msg string, _ ...any) { r.infos = append(r.infos, msg) }

// TestWatchPageTCLPlayerPath: the page's player URL is rewritten to the main
// variant, so the variant it named is read off the URL first, as the page
// gave it. Only the `tcl` family counts: tce and tcc are regular variants,
// whose STS and answers are main's.
//
// Mutants: the flag read off the rewritten URL (never set); the pattern
// loosened to any `_tc` variant (tce and tcc flagged).
func TestWatchPageTCLPlayerPath(t *testing.T) {
	const mainURL = "https://www.youtube.com/s/player/b485e332/player_ias.vflset/en_US/base.js"
	for _, tc := range []struct {
		raw string
		tcl bool
	}{
		{`\/s\/player\/b485e332\/player_ias_tcl.vflset\/en_US\/base.js`, true},
		{`/s/player/b485e332/player_es6_tcl.vflset/en_US/base.js`, true},
		{`\/s\/player\/b485e332\/tv-player-ias-tcl.vflset\/tv-player-ias-tcl.js`, true},
		{`\/s\/player\/b485e332\/player_ias.vflset\/en_US\/base.js`, false},
		{`\/s\/player\/b485e332\/player_es6.vflset\/en_US\/base.js`, false},
		{`\/s\/player\/b485e332\/player_ias_tce.vflset\/en_US\/base.js`, false},
		{`\/s\/player\/b485e332\/player_es6_tcc.vflset\/en_US\/base.js`, false},
		{`\/s\/player\/b485e332\/tv-player-ias.vflset\/tv-player-ias.js`, false},
	} {
		page := `<script>ytcfg.set({"PLAYER_JS_URL":"` + tc.raw + `"});</script>`
		ytcfg, _ := extractYtcfgAndPlayerResponse([]byte(page))
		if ytcfg.PlayerURL != mainURL || ytcfg.PlayerOnTCL != tc.tcl {
			t.Errorf("page naming %s: PlayerURL %q, PlayerOnTCL %v; want the main variant's URL and %v", tc.raw, ytcfg.PlayerURL, ytcfg.PlayerOnTCL, tc.tcl)
		}
		if got := pageRunsOtherVariant(ytcfg); got != tc.tcl {
			t.Errorf("page naming %s with no STS: pageRunsOtherVariant = %v, want %v", tc.raw, got, tc.tcl)
		}
	}
}
