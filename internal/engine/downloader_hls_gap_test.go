package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestHlsLoop_StopOnGapReturnsErrGapDetected pins the part-split contract:
// when the live playlist window moves past the downloader's position and the
// output file already holds data, StopOnGap ends the loop with ErrGapDetected
// and the file contains exactly the pre-gap segments — nothing from after the
// gap is appended.
func TestHlsLoop_StopOnGapReturnsErrGapDetected(t *testing.T) {
	var playlistCalls atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		if playlistCalls.Add(1) == 1 {
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n"+
				"#EXT-X-MEDIA-SEQUENCE:100\n"+
				"#EXTINF:1.0,\nseg100.ts\n#EXTINF:1.0,\nseg101.ts\n")
			return
		}
		// Window has moved on: segments 102..199 expired from the CDN.
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n"+
			"#EXT-X-MEDIA-SEQUENCE:200\n"+
			"#EXTINF:1.0,\nseg200.ts\n")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Segment bodies carry their name so the output file is checkable.
		fmt.Fprintf(w, "[%s]", strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".ts"))
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

	var gap atomic.Value
	d.OnGap = func(g DownloadGap) { gap.Store(g) }

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := d.Start(ctx)

	if !errors.Is(err, ErrGapDetected) {
		t.Fatalf("Start() = %v, want ErrGapDetected", err)
	}
	data, readErr := os.ReadFile(outFile)
	if readErr != nil {
		t.Fatalf("read output: %v", readErr)
	}
	if got, want := string(data), "[seg100][seg101]"; got != want {
		t.Errorf("output file = %q, want %q (pre-gap segments only)", got, want)
	}
	// The gap report belongs to the SUCCESSOR downloader (seeded at this
	// position with an empty file, it takes the skip-forward branch and
	// fires OnGap once) — the stopping downloader must stay silent or the
	// same gap would be recorded twice.
	if g, ok := gap.Load().(DownloadGap); ok {
		t.Errorf("OnGap fired on the stopping downloader: %+v (successor records the gap)", g)
	}
	if got := d.CurrentSeq(); got != 102 {
		t.Errorf("CurrentSeq() = %d, want 102 (un-advanced, seeds the next part)", got)
	}
}

// TestHlsLoop_StopOnGapEmptyFileSkipsForward covers the no-data case: a
// downloader positioned behind the window but with nothing on disk has no
// part to finalize, so it skips to the window start like the default
// behavior instead of erroring.
func TestHlsLoop_StopOnGapEmptyFileSkipsForward(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n"+
			"#EXT-X-MEDIA-SEQUENCE:200\n"+
			"#EXTINF:1.0,\nseg200.ts\n#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "[%s]", strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".ts"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tmp := t.TempDir()
	outFile := filepath.Join(tmp, "video_stream")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL: srv.URL + "/playlist.m3u8",
		// Orchestrator-provided position far behind the window, but no file
		// on disk — fresh start, bytesWritten stays 0 at gap-check time.
		StartSeq:      50,
		ForceStartSeq: true,
		OutputFile:    outFile,
		IsHls:         true,
		StopOnGap:     true,
	})

	var gap atomic.Value
	d.OnGap = func(g DownloadGap) { gap.Store(g) }

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start() = %v, want nil (skip forward, download from window)", err)
	}
	data, readErr := os.ReadFile(outFile)
	if readErr != nil {
		t.Fatalf("read output: %v", readErr)
	}
	if got, want := string(data), "[seg200]"; got != want {
		t.Errorf("output file = %q, want %q", got, want)
	}
	// This is the successor role from the part-split flow: the empty-file
	// skip records the gap exactly once.
	g, ok := gap.Load().(DownloadGap)
	if !ok {
		t.Fatal("OnGap was not invoked on the skip-forward path")
	}
	if g.From != 50 || g.To != 199 {
		t.Errorf("OnGap = %+v, want From=50 To=199", g)
	}
}
