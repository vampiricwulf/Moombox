package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestHlsLive_AdBreakAcrossWindowSlide pins the part-split footgun the
// seq-advance-on-skip exists for: an ad break at the tail of one playlist
// window, then the window slides COMPLETELY past the ads on the next refresh.
// If the skip failed to advance currentSeq, the second refresh would see
// curSeq < MediaSequence with bytes on disk and return ErrGapDetected — a
// spurious part split for content that was deliberately skipped. With the
// advance, the recording continues seamlessly into the post-ad content.
func TestHlsLive_AdBreakAcrossWindowSlide(t *testing.T) {
	var refresh atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n")
		if refresh.Add(1) == 1 {
			// Window 1: two content segments then the ad break begins.
			b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
			b.WriteString(`#EXT-X-DATERANGE:ID="stitched-ad-1",CLASS="twitch-stitched-ad",START-DATE="2026-07-01T00:00:02.000Z",DURATION=2.000` + "\n")
			b.WriteString("#EXT-X-PROGRAM-DATE-TIME:2026-07-01T00:00:00.000Z\n#EXTINF:1.000,live\nseg0.ts\n")
			b.WriteString("#EXT-X-PROGRAM-DATE-TIME:2026-07-01T00:00:01.000Z\n#EXTINF:1.000,live\nseg1.ts\n")
			b.WriteString("#EXT-X-PROGRAM-DATE-TIME:2026-07-01T00:00:02.000Z\n#EXTINF:1.000,Amazon\nad2.ts\n")
			b.WriteString("#EXT-X-PROGRAM-DATE-TIME:2026-07-01T00:00:03.000Z\n#EXTINF:1.000,Amazon\nad3.ts\n")
		} else {
			// Window 2: slid entirely past the ads — starts at seq 4.
			b.WriteString("#EXT-X-MEDIA-SEQUENCE:4\n")
			b.WriteString("#EXT-X-PROGRAM-DATE-TIME:2026-07-01T00:00:04.000Z\n#EXTINF:1.000,live\nseg4.ts\n")
			b.WriteString("#EXT-X-PROGRAM-DATE-TIME:2026-07-01T00:00:05.000Z\n#EXTINF:1.000,live\nseg5.ts\n")
			b.WriteString("#EXT-X-ENDLIST\n")
		}
		fmt.Fprint(w, b.String())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".ts")
		fmt.Fprintf(w, "[%s]", name)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tmp := t.TempDir()
	outFile := filepath.Join(tmp, "video_stream")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/playlist.m3u8",
		OutputFile: outFile,
		StartSeq:   -1,
		IsHls:      true,
		StopOnGap:  true,
	})
	var gapMu sync.Mutex
	var gaps []DownloadGap
	d.OnGap = func(g DownloadGap) {
		gapMu.Lock()
		gaps = append(gaps, g)
		gapMu.Unlock()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// A lagging seq would surface here as ErrGapDetected.
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v (ErrGapDetected here means the ad skip failed to advance currentSeq)", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "[seg0][seg1][seg4][seg5]"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	if got := d.CurrentSeq(); got != 6 {
		t.Errorf("CurrentSeq() = %d, want 6", got)
	}
	gapMu.Lock()
	defer gapMu.Unlock()
	if len(gaps) != 0 {
		t.Errorf("no gap should be recorded across an ad-break window slide, got %+v", gaps)
	}
}

// TestHlsLive_TwitchStitchedAdSkipped drives the LIVE sequential path
// (StopOnGap=true, the Twitch live configuration) through a playlist with a
// stitched-ad break in the middle. The ad segments must be skipped without
// being fetched or written, currentSeq must advance past them (or the sliding
// window would read as a CDN gap → spurious part split), and OnGap must NOT
// fire (ads are deliberate skips, not lost content).
func TestHlsLive_TwitchStitchedAdSkipped(t *testing.T) {
	// 5 segments @2s starting 2026-07-01T00:00:00Z; segments 2-3 are the ad.
	var adFetches atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
		b.WriteString(`#EXT-X-DATERANGE:ID="stitched-ad-9",CLASS="twitch-stitched-ad",START-DATE="2026-07-01T00:00:04.000Z",DURATION=4.000,X-TV-TWITCH-AD-POD-LENGTH="2"` + "\n")
		for i := 0; i < 5; i++ {
			fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:2026-07-01T00:00:%02d.000Z\n", i*2)
			name := fmt.Sprintf("seg%d", i)
			if i == 2 || i == 3 {
				name = fmt.Sprintf("ad%d", i)
			}
			fmt.Fprintf(&b, "#EXTINF:2.000,live\n%s.ts\n", name)
		}
		b.WriteString("#EXT-X-ENDLIST\n") // StopOnGap keeps the sequential path even for ENDLIST
		fmt.Fprint(w, b.String())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".ts")
		if strings.HasPrefix(name, "ad") {
			adFetches.Add(1) // the skip must happen BEFORE the fetch
		}
		fmt.Fprintf(w, "[%s]", name)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tmp := t.TempDir()
	outFile := filepath.Join(tmp, "video_stream")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/playlist.m3u8",
		OutputFile: outFile,
		StartSeq:   -1,
		IsHls:      true,
		StopOnGap:  true, // Twitch live configuration
	})

	var gapMu sync.Mutex
	var gaps []DownloadGap
	d.OnGap = func(g DownloadGap) {
		gapMu.Lock()
		gaps = append(gaps, g)
		gapMu.Unlock()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "[seg0][seg1][seg4]"; got != want {
		t.Errorf("output = %q, want %q (ad segments must not be written)", got, want)
	}
	if n := adFetches.Load(); n != 0 {
		t.Errorf("ad segments were fetched %d times; skip must happen before the fetch", n)
	}
	// Position must be index-aligned past the skipped ads: MediaSequence 0 +
	// 5 playlist entries = 5. A lagging seq (3) is the part-split footgun.
	if got := d.CurrentSeq(); got != 5 {
		t.Errorf("CurrentSeq() = %d, want 5 (must advance past skipped ads)", got)
	}
	// Ads are deliberate skips, not lost content — no gap rows.
	gapMu.Lock()
	defer gapMu.Unlock()
	if len(gaps) != 0 {
		t.Errorf("OnGap fired for ad skip: %+v", gaps)
	}
}
