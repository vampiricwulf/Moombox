package engine

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// TestHlsVodRidesOutAOneOffRefusal pins G33: an HLS VOD segment, or the fMP4
// #EXT-X-MAP init every segment needs, that an edge refuses once with a 403
// or 410 is asked again and archived — where it used to be permanent at once,
// because an HLS fetch never knows a head (only GVS sends X-Head-Seqnum): the
// segment became a gap row and a hole in the VOD, and the init ended the
// whole VOD (runHlsVodParallel returns ensureHlsInit's error). yt-dlp retries
// every fragment HTTPError fragment_retries times before it skips the
// fragment (downloader/fragment.py); Moombox asks headlessRefusalAttempts
// times (fetchSegmentWithRetry).
//
// It runs in a synctest bubble with production delays over
// serveEngineInMemory, so the retry's 500 ms backoff is the bubble's clock.
//
// Mutant: dropping the `d.headSeq.Load() < 0` branch from
// fetchSegmentWithRetry — the segment rows record a gap and the init rows end
// Start with "fetch HLS init segment". Mutant: headlessRefusalAttempts = 1 —
// the same.
func TestHlsVodRidesOutAOneOffRefusal(t *testing.T) {
	const segs = 6
	for _, tc := range []struct {
		name    string
		fmp4    bool
		refused string // the path answered `status` once
		status  int
	}{
		{"a TS segment refused 403 once", false, "seg3.ts", http.StatusForbidden},
		{"a TS segment refused 410 once", false, "seg0.ts", http.StatusGone},
		{"the fMP4 init refused 403 once", true, "init0.mp4", http.StatusForbidden},
		{"the fMP4 init refused 410 once", true, "init0.mp4", http.StatusGone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ext := ".ts"
			if tc.fmp4 {
				ext = ".mp4"
			}
			var refusedAsks atomic.Int32
			serveEngineInMemory(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path := strings.TrimPrefix(r.URL.Path, "/")
				if path == "playlist.m3u8" {
					var b strings.Builder
					b.WriteString("#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
					if tc.fmp4 {
						b.WriteString(`#EXT-X-MAP:URI="init0.mp4"` + "\n")
					}
					for i := range segs {
						fmt.Fprintf(&b, "#EXTINF:2.000,\nseg%d%s\n", i, ext)
					}
					b.WriteString("#EXT-X-ENDLIST\n")
					fmt.Fprint(w, b.String())
					return
				}
				if path == tc.refused && refusedAsks.Add(1) == 1 {
					w.WriteHeader(tc.status) // a transient edge refusal
					return
				}
				if path == "init0.mp4" {
					fmt.Fprint(w, "(INIT)")
					return
				}
				fmt.Fprintf(w, "[%s]", strings.TrimSuffix(path, ext))
			}))

			synctest.Test(t, func(t *testing.T) {
				out := filepath.Join(t.TempDir(), "video"+ext)
				d := NewSegmentDownloader(DownloaderOptions{
					BaseURL:    inMemoryOrigin + "/playlist.m3u8",
					OutputFile: out,
					StartSeq:   -1,
					IsHls:      true, // StopOnGap false: ENDLIST routes to runHlsVodParallel
				})
				var mu sync.Mutex
				var gaps []DownloadGap
				d.OnGap = func(g DownloadGap) { mu.Lock(); gaps = append(gaps, g); mu.Unlock() }

				if err := d.Start(context.Background()); err != nil {
					t.Fatalf("Start = %v, want the refused %s asked again and the VOD archived", err, tc.refused)
				}
				var want strings.Builder
				if tc.fmp4 {
					want.WriteString("(INIT)")
				}
				for i := range segs {
					fmt.Fprintf(&want, "[seg%d]", i)
				}
				if got, _ := os.ReadFile(out); string(got) != want.String() {
					t.Errorf("output = %q, want %q (no hole where the refusal was)", got, want.String())
				}
				mu.Lock()
				defer mu.Unlock()
				if len(gaps) != 0 {
					t.Errorf("gaps = %+v, want none: the refusal was transient", gaps)
				}
				if got := refusedAsks.Load(); got != 2 {
					t.Errorf("%s asked for %d times, want 2 (refused, then fetched)", tc.refused, got)
				}
			})
		})
	}
}

// TestHeadlessRefusalIsBounded pins the bound on G33's retry and the DASH
// semantics it leaves alone. With no head known, a 403 or 410 is asked
// headlessRefusalAttempts (3) times in all, 500 ms and then 1 s apart
// (singleGoneRetry doubled, on the bubble's clock), and then returned as the
// permanent error it always was — errSegmentGone for a 410, so
// fetchInitSegment still never refreshes an eviction. A refusal whose own
// answer carries X-Head-Seqnum — every GVS answer — has a head, and stays
// permanent on its first answer: past the head a 403 is how a finished
// stream says "no such segment".
//
// Mutant: `d.headSeq.Load() < 0` → `true` — the X-Head-Seqnum rows ask three
// times (TestForbiddenAtHeadStaysPermanent and TestGoneAlwaysPermanent fail
// too). Mutant: returning ErrSegmentPermanent for a headless 410 — the 410
// row gets a 403's error. Mutant: dropping the backoff sleep — the headless
// rows wait 0 s.
func TestHeadlessRefusalIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		head      bool // the answer carries X-Head-Seqnum
		wantErr   error
		wantAsks  int32
		wantWaits time.Duration
	}{
		{"a 403 with no head", http.StatusForbidden, false, ErrSegmentPermanent, headlessRefusalAttempts, 1500 * time.Millisecond},
		{"a 410 with no head", http.StatusGone, false, errSegmentGone, headlessRefusalAttempts, 1500 * time.Millisecond},
		{"a 403 whose answer names the head", http.StatusForbidden, true, ErrSegmentPermanent, 1, 0},
		{"a 410 whose answer names the head", http.StatusGone, true, errSegmentGone, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asks atomic.Int32
			serveEngineInMemory(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asks.Add(1)
				if tc.head {
					w.Header().Set("X-Head-Seqnum", "100")
				}
				w.WriteHeader(tc.status)
			}))
			synctest.Test(t, func(t *testing.T) {
				d := NewSegmentDownloader(DownloaderOptions{BaseURL: inMemoryOrigin + "/v?x=1", OutputFile: filepath.Join(t.TempDir(), "o")})
				d.currentSeq.Store(101) // at the head, when one is named
				start := time.Now()
				_, err := d.fetchSegmentWithRetry(context.Background(), inMemoryOrigin+"/seg101.ts", nil)
				waited := time.Since(start)
				if err != tc.wantErr {
					t.Errorf("fetchSegmentWithRetry = %v, want %v", err, tc.wantErr)
				}
				if got := asks.Load(); got != tc.wantAsks {
					t.Errorf("asked %d times, want %d", got, tc.wantAsks)
				}
				if waited != tc.wantWaits {
					t.Errorf("waited %v on the bubble's clock, want %v", waited, tc.wantWaits)
				}
			})
		})
	}
}
