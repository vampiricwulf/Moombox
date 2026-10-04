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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestHlsVodParallelCancelIsNotAGap: a cancel (shutdown, user cancel) while
// workers were mid-fetch turned each cut-short fetch into a gap sentinel. The
// consumer skipped them, advancing currentSeq past segments nobody found
// missing, reported a gap, saved that position as the resume point and
// returned nil — so the resumed download never fetched them and the VOD was
// short by up to SegmentWorkers segments.
//
// Mutant: admit the nil result again (drop the cancellation check in the
// worker) — CurrentSeq is 4 and a 1–3 gap is reported. Mutant: drop the
// cancelErr return — Start returns nil.
func TestHlsVodParallelCancelIsNotAGap(t *testing.T) {
	const (
		totalSegs = 8
		segSize   = 1 << 10
		workers   = 3
	)
	var hung atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/playlist.m3u8") {
			var b strings.Builder
			b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
			for i := range totalSegs {
				fmt.Fprintf(&b, "#EXTINF:2.0,\n/seg%d.ts\n", i)
			}
			b.WriteString("#EXT-X-ENDLIST\n")
			w.Write([]byte(b.String()))
			return
		}
		if r.URL.Path != "/seg0.ts" {
			hung.Add(1)
			<-r.Context().Done() // hangs until the client gives up
			return
		}
		w.Write(make([]byte, segSize))
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := deadlineTestContext(t)

	out := filepath.Join(t.TempDir(), "video.ts")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:        srv.URL + "/playlist.m3u8",
		OutputFile:     out,
		IsHls:          true,
		SegmentWorkers: workers,
	})
	d.delays = fastDelays()
	var mu sync.Mutex
	var gaps []DownloadGap
	d.OnGap = func(g DownloadGap) {
		mu.Lock()
		gaps = append(gaps, g)
		mu.Unlock()
	}

	done := make(chan error, 1)
	go func() { done <- d.Start(ctx) }()

	// Segment 0 written, and every worker inside a hung fetch of 1..3.
	deadline := time.Now().Add(5 * time.Second)
	for d.CurrentSeq() < 1 || hung.Load() < workers {
		if time.Now().After(deadline) {
			t.Fatalf("fixture never reached its fixed point: currentSeq %d, %d fetches hung", d.CurrentSeq(), hung.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()

	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return within 5s of the cancel")
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("Start returned %v, want context.Canceled", err)
	}
	if got := d.CurrentSeq(); got != 1 {
		t.Errorf("CurrentSeq = %d, want 1: the resume would skip segments the cancel cut short", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gaps) != 0 {
		t.Errorf("a cancel reported gaps %+v", gaps)
	}
	if info, statErr := os.Stat(out); statErr != nil || info.Size() != segSize {
		t.Errorf("output: %v, %v; want segment 0 alone (%d bytes)", info, statErr, segSize)
	}
}
