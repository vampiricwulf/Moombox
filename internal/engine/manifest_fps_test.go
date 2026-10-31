package engine

import (
	"fmt"
	"strings"
	"testing"
)

// manifestFrameRates are the stated rates both manifest tests run, and the
// FPS each must parse to: yt-dlp drops a YouTube manifest format's fps of 1
// or less (`if f.get('fps') and f['fps'] <= 1: del f['fps']`,
// _video.py:3676-3677, 97afb093d), so 0 — not stated — is what those parse
// to. DASH's frameRate and HLS's FRAME-RATE share the rows; fractions and
// decimals are spelled the way each format writes them.
var manifestFrameRates = []struct {
	dash, hls string
	want      int
}{
	{"1", "1.000", 0},
	{"1/1", "1", 0},
	{"0", "0.000", 0},
	{"1/2", "0.500", 0},
	{"2", "2.000", 2},
	{"30000/1001", "29.970", 30},
	{"60", "60.000", 60},
}

// TestParseDashDropsAFrameRateOfOneOrLess pins knownFPS on ParseDash's
// frameRate.
//
// Mutants this kills:
//   - ParseDash's knownFPS call dropped → the "1", "1/1" and "1/2" rows keep
//     their 1
//   - the floor at `fps <= 2`            → the "2" row loses its 2
func TestParseDashDropsAFrameRateOfOneOrLess(t *testing.T) {
	var reps strings.Builder
	for i, tc := range manifestFrameRates {
		fmt.Fprintf(&reps, `<Representation id="%d" bandwidth="1000" width="1920" height="1080" frameRate="%s"><BaseURL>https://example.com/%d</BaseURL></Representation>`,
			100+i, tc.dash, i)
	}
	mpd := `<?xml version="1.0"?><MPD><Period><AdaptationSet mimeType="video/mp4">` + reps.String() + `</AdaptationSet></Period></MPD>`
	streams, err := ParseDash(mpd, "")
	if err != nil {
		t.Fatalf("ParseDash: %v", err)
	}
	if len(streams) != len(manifestFrameRates) {
		t.Fatalf("parsed %d representations, want %d", len(streams), len(manifestFrameRates))
	}
	for i, tc := range manifestFrameRates {
		if got := streams[i].FPS; got != tc.want {
			t.Errorf("frameRate=%q parsed to FPS %d, want %d", tc.dash, got, tc.want)
		}
	}
}

// TestParseHlsDropsAFrameRateOfOneOrLess pins knownFPS on a master playlist's
// FRAME-RATE, the YouTube HLS strategy's only source of a variant's rate.
//
// Mutants this kills:
//   - the master parse's knownFPS call dropped → the "1.000", "1" and "0.500"
//     rows keep their 1 (math.Round rounds 0.5 away from zero)
//   - the floor at `fps <= 2`                  → the "2.000" row loses its 2
func TestParseHlsDropsAFrameRateOfOneOrLess(t *testing.T) {
	var m3u8 strings.Builder
	m3u8.WriteString("#EXTM3U\n")
	for i, tc := range manifestFrameRates {
		fmt.Fprintf(&m3u8, "#EXT-X-STREAM-INF:BANDWIDTH=1000,RESOLUTION=1920x1080,FRAME-RATE=%s\nhttps://example.com/%d.m3u8\n", tc.hls, i)
	}
	result := ParseHls(m3u8.String(), "https://example.com/")
	if result == nil || len(result.Variants) != len(manifestFrameRates) {
		t.Fatalf("parsed %v, want %d variants", result, len(manifestFrameRates))
	}
	for i, tc := range manifestFrameRates {
		if got := result.Variants[i].FPS; got != tc.want {
			t.Errorf("FRAME-RATE=%s parsed to FPS %d, want %d", tc.hls, got, tc.want)
		}
	}
}
