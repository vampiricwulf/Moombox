package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/engine"
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
