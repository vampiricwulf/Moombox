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
	"testing"
	"time"
)

// TestHlsVodParallel_EarlyGapBoundedAndSkipped drives the VOD-parallel path
// (ENDLIST + !StopOnGap) with an early segment that permanently 403s. The
// gap-sentinel fix must: skip the missing segment (not wedge the consumer and
// buffer the whole VOD), fire OnGap exactly for it, and write the rest in
// order — while completing (no hang).
func TestHlsVodParallel_EarlyGapBoundedAndSkipped(t *testing.T) {
	const total = 40
	const goneIdx = 2 // an early hole — the worst case for the old buffer wedge

	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n")
		for i := 0; i < total; i++ {
			fmt.Fprintf(&b, "#EXTINF:1.0,\nseg%d.ts\n", i)
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		fmt.Fprint(w, b.String())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".ts")
		if name == fmt.Sprintf("seg%d", goneIdx) {
			w.WriteHeader(http.StatusForbidden) // permanent gone -> gap sentinel
			return
		}
		fmt.Fprintf(w, "[%s]", name)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tmp := t.TempDir()
	outFile := filepath.Join(tmp, "video.ts")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/playlist.m3u8",
		OutputFile: outFile,
		StartSeq:   -1,
		IsHls:      true,
		// StopOnGap:false (default) -> ENDLIST routes to runHlsVodParallel.
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
	// Expect every segment except the gone one, in order.
	var want strings.Builder
	for i := 0; i < total; i++ {
		if i == goneIdx {
			continue
		}
		fmt.Fprintf(&want, "[seg%d]", i)
	}
	if string(data) != want.String() {
		t.Errorf("output mismatch.\n got=%q\nwant=%q", string(data), want.String())
	}

	// Exactly one gap, covering exactly the gone index.
	gapMu.Lock()
	defer gapMu.Unlock()
	if len(gaps) != 1 {
		t.Fatalf("gaps = %+v, want exactly 1 covering idx %d", gaps, goneIdx)
	}
	if gaps[0].From != goneIdx || gaps[0].To != goneIdx {
		t.Errorf("gap = %+v, want From=To=%d", gaps[0], goneIdx)
	}
}

// TestHlsVodParallel_TrailingGapClosed covers a gap that runs to the end of
// the playlist — closeGap(totalSegs-1) must fire for it.
func TestHlsVodParallel_TrailingGapClosed(t *testing.T) {
	const total = 20
	goneFrom := 17 // last 3 segments gone

	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n")
		for i := 0; i < total; i++ {
			fmt.Fprintf(&b, "#EXTINF:1.0,\nseg%d.ts\n", i)
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		fmt.Fprint(w, b.String())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".ts")
		var idx int
		fmt.Sscanf(name, "seg%d", &idx)
		if idx >= goneFrom {
			w.WriteHeader(http.StatusGone)
			return
		}
		fmt.Fprintf(w, "[%s]", name)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tmp := t.TempDir()
	outFile := filepath.Join(tmp, "video.ts")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/playlist.m3u8",
		OutputFile: outFile,
		StartSeq:   -1,
		IsHls:      true,
	})
	var gaps []DownloadGap
	var mu sync.Mutex
	d.OnGap = func(g DownloadGap) { mu.Lock(); gaps = append(gaps, g); mu.Unlock() }

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gaps) != 1 || gaps[0].From != goneFrom || gaps[0].To != total-1 {
		t.Errorf("trailing gap = %+v, want one From=%d To=%d", gaps, goneFrom, total-1)
	}
}
