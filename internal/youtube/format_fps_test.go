package youtube

import (
	"context"
	"fmt"
	"testing"
)

// TestParseFormatsDropsAFrameRateOfOneOrLess pins yt-dlp's
// `'fps': fps if fps > 1 else None` — "For some formats, fps is wrongly
// returned as 1" (_video.py:3463, 97afb093d). A rate of 1 or less is stored
// as unstated.
//
// Mutants this kills:
//   - the floor reverted to `fps > 0` → the fps-1 row keeps its 1
//   - the floor at `fps > 2`          → the fps-2 row loses its 2
func TestParseFormatsDropsAFrameRateOfOneOrLess(t *testing.T) {
	for _, tc := range []struct {
		fps  string // the JSON value, "" for none
		want int    // 0 for an unstated rate
	}{
		{"", 0}, {"0", 0}, {"1", 0}, {"2", 2}, {"25", 25}, {"30", 30}, {"60", 60},
	} {
		t.Run("fps="+tc.fps, func(t *testing.T) {
			f := `{"itag": 137, "url": "https://x/137", "mimeType": "video/mp4; codecs=\"avc1.640028\"", "width": 1920, "height": 1080`
			if tc.fps != "" {
				f += `, "fps": ` + tc.fps
			}
			sd := decodePlayerJSON(t, `{"adaptiveFormats": [`+f+`}]}`)
			got, _ := NewPlayerAPI(nil, noopLogger{}).parseFormats(context.Background(), sd)
			if len(got) != 1 {
				t.Fatalf("parsed %d formats, want 1", len(got))
			}
			switch {
			case tc.want == 0 && got[0].Fps != nil:
				t.Errorf("fps %s stored as %d, want it unstated", tc.fps, *got[0].Fps)
			case tc.want != 0 && (got[0].Fps == nil || *got[0].Fps != tc.want):
				t.Errorf("fps %s stored as %v, want %d", tc.fps, got[0].Fps, tc.want)
			}
		})
	}
}

// TestABogusFrameRateDoesNotWinTheFPSRung is the case the floor exists for,
// from the wire: with prefer_60fps off the lower rate wins the FPS rung ahead
// of the codec, so an avc1 misreported at 1 fps beat the av01 beside it at
// 30. Unstated, it reads as 30, ties, and the codec rung picks the av01.
func TestABogusFrameRateDoesNotWinTheFPSRung(t *testing.T) {
	video := func(itag int, codecs string, fps int) string {
		return fmt.Sprintf(`{"itag": %d, "url": "https://x/%d", "mimeType": "video/mp4; codecs=\"%s\"", "bitrate": 4000000, "width": 1920, "height": 1080, "fps": %d}`,
			itag, itag, codecs, fps)
	}
	sd := decodePlayerJSON(t, `{"adaptiveFormats": [`+video(137, "avc1.640028", 1)+`,`+video(399, "av01.0.08M.08", 30)+`]}`)
	formats, _ := NewPlayerAPI(nil, noopLogger{}).parseFormats(context.Background(), sd)
	got := SelectBestFormats(formats, 1080, false)
	if got.Video == nil || got.Video.Itag != 399 {
		t.Errorf("video = %v, want the av01 399 — the avc1's 1 fps is not a frame rate", itagOf(got.Video))
	}
}
