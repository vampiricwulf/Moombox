package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// hlsLadder is the shape a YouTube live master playlist has: one variant per
// rendition, largest first.
func hlsLadder() []engine.HlsVariant {
	return []engine.HlsVariant{
		{Bandwidth: 18000000, Width: 3840, Height: 2160, FPS: 30, URL: "https://example.com/2160.m3u8"},
		{Bandwidth: 9000000, Width: 2560, Height: 1440, FPS: 30, URL: "https://example.com/1440.m3u8"},
		{Bandwidth: 4000000, Width: 1920, Height: 1080, FPS: 60, URL: "https://example.com/1080.m3u8"},
		{Bandwidth: 2500000, Width: 1280, Height: 720, FPS: 30, URL: "https://example.com/720.m3u8"},
	}
}

// TestSelectHlsVariantCap is ruling R1 at the YouTube live-HLS site. This site
// had the harshest pre-arc behaviour of the four: a cap with nothing under it
// returned the hard error "no HLS variants found within resolution limit (%d)"
// and the whole download failed.
//
// Mutants:
//   - long edge restored: "4K under the default cap" selects the 1080 variant.
//   - the hard error restored (nil here): "nothing at or below the cap" fatals.
//   - closest-above replaced by largest: the same row returns the 2160 variant.
//   - the 9999 sentinel restored for maxRes <= 0: harmless on this ladder, so
//     "cap 0 is unbounded" is the companion row that keeps the intent stated.
func TestSelectHlsVariantCap(t *testing.T) {
	for _, tc := range []struct {
		name     string
		variants []engine.HlsVariant
		pref     string
		maxRes   int
		wantURL  string
	}{
		{"4K under the default cap", hlsLadder(), "best", 2160, "https://example.com/2160.m3u8"},
		{"cap 1080", hlsLadder(), "best", 1080, "https://example.com/1080.m3u8"},
		{"cap 0 is unbounded", hlsLadder(), "best", 0, "https://example.com/2160.m3u8"},
		{
			name: "a portrait source is admitted by a 2160 cap",
			variants: []engine.HlsVariant{
				{Bandwidth: 18000000, Width: 2160, Height: 3840, FPS: 30, URL: "https://example.com/portrait.m3u8"},
			},
			pref:    "best",
			maxRes:  2160,
			wantURL: "https://example.com/portrait.m3u8",
		},
		{
			name:     "nothing at or below the cap picks the closest above",
			variants: hlsLadder()[:2],
			pref:     "best",
			maxRes:   1080,
			wantURL:  "https://example.com/1440.m3u8",
		},
		{
			name:     "a lower quality pref still works under a high cap",
			variants: hlsLadder(),
			pref:     "720p",
			maxRes:   2160,
			wantURL:  "https://example.com/720.m3u8",
		},
		{
			name:     "audio_only takes the lowest bandwidth under the cap",
			variants: hlsLadder(),
			pref:     "audio_only",
			maxRes:   2160,
			wantURL:  "https://example.com/720.m3u8",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := selectHlsVariant(tc.variants, tc.pref, tc.maxRes)
			if got == nil {
				t.Fatalf("selectHlsVariant returned nil for %q at cap %d — the cap is a preference, never a failure", tc.pref, tc.maxRes)
			}
			if got.URL != tc.wantURL {
				t.Errorf("selected %q, want %q", got.URL, tc.wantURL)
			}
		})
	}
}

// TestSelectHlsVariantEmpty: no variants at all is the one case that still has
// no answer. DownloadHls rejects an empty master playlist before this point;
// the nil is the contract, not a resolution decision.
func TestSelectHlsVariantEmpty(t *testing.T) {
	if got := selectHlsVariant(nil, "best", 2160); got != nil {
		t.Errorf("selectHlsVariant(nil, …) = %+v, want nil", got)
	}
}

const potTestMaster = `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=2500000,RESOLUTION=1280x720,FRAME-RATE=30
https://manifest.googlevideo.com/api/manifest/hls_playlist/id/abc/itag/95/index.m3u8
`

// TestDownloadHlsAttachesTokenOnlyWhereRequired is the HLS half of the
// live-path policy, in the same shape as the DASH test: the master
// playlist's recorded client decides, by youtube.GvsTokenRequired, whether a
// GVS token is minted and carried on the master path, the variant path and
// the downloader. VISIONOS serves live as HLS only, so a visionos master is
// the common bare case; tv and web_embedded masters ride bare too, as
// upstream fetches no GVS token for them.
//
// Mutant: dropping the GvsTokenRequired gate fails every bare row (a mint, a
// /pot/ master path, a token, no skip line); gating on the old WebPO-client
// list fails the tv_auth, tv_public and web_embedded rows.
func TestDownloadHlsAttachesTokenOnlyWhereRequired(t *testing.T) {
	for _, tc := range []struct {
		source  string
		wantPot bool
	}{
		{source: "web", wantPot: true},
		{source: "watch_page", wantPot: true},
		{source: "tv_auth"},
		{source: "tv_public"},
		{source: "web_embedded"},
		{source: "visionos"},
		{source: "android_vr"},
		{source: ""},
	} {
		t.Run("source="+tc.source, func(t *testing.T) {
			calls := fakeVodMint(t, "tok123", nil)
			srv := newManifestServer(t, potTestMaster)
			job, logs := vodPotJob(t)
			info := &youtube.VideoInfo{
				StreamStatus:      youtube.StreamLive,
				HlsManifestURL:    srv.URL + "/api/manifest/hls_variant/id/abc/file/index.m3u8",
				HlsManifestSource: tc.source,
			}

			res, err := DownloadHls(context.Background(), job, info, nil, nil, &bgutils.PotProvider{}, nil)
			if err != nil {
				t.Fatalf("DownloadHls: %v", err)
			}
			path := srv.onlyPath(t)
			variantLines := potLines(logs, "[POT] added PO token to HLS variant URL")
			if tc.wantPot {
				if n := calls.Load(); n != 1 {
					t.Errorf("mint ran %d times, want 1", n)
				}
				if !strings.HasSuffix(path, "/pot/tok123") {
					t.Errorf("master path = %q, want the /pot/tok123 suffix", path)
				}
				if len(variantLines) != 1 {
					t.Errorf("variant URL token lines = %v, want one", variantLines)
				}
				if got := res.VideoDownloader.PoToken(); got != "tok123" {
					t.Errorf("downloader token = %q, want tok123", got)
				}
				lines := potLines(logs, "[POT] GVS mint")
				if len(lines) != 1 || lines[0]["source"] != tc.source {
					t.Errorf("[POT] GVS mint lines = %v, want one naming source=%s", lines, tc.source)
				}
				assertSkipLines(t, logs)
				return
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("mint ran %d times, want 0 — a %q master takes no GVS token", n, tc.source)
			}
			if strings.Contains(path, "/pot/") {
				t.Errorf("master path = %q, want no /pot/ segment", path)
			}
			if len(variantLines) != 0 {
				t.Errorf("variant URL was tokenised: %v", variantLines)
			}
			if got := res.VideoDownloader.PoToken(); got != "" {
				t.Errorf("downloader token = %q, want none", got)
			}
			assertSkipLines(t, logs, skipLine(job, tc.source))
		})
	}
}
