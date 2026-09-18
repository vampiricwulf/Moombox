package engine

import (
	"context"
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

// TestHlsVodParallelBufferIsBounded pins ENGINE-2: while the head-of-order
// segment is blocked, the workers must stop fetching once the reorder buffer
// reaches its byte ceiling instead of racing the whole VOD into RAM.
//
// Mutant: restoring `buffer := make(map[int][]byte)` (no ceiling) — all 40
// segments are fetched while segment 0 is blocked, which is the unbounded
// growth the row measured at 0.6-2.5 GB in the field.
//
// Do not add t.Parallel(): it shrinks the package-global hlsVodBufferBytes.
func TestHlsVodParallelBufferIsBounded(t *testing.T) {
	const (
		totalSegs = 40
		segSize   = 64 << 10
		workers   = 8
	)
	prev := hlsVodBufferBytes
	t.Cleanup(func() { hlsVodBufferBytes = prev })
	hlsVodBufferBytes = 2 * segSize // room for two non-head segments

	release := make(chan struct{})
	var fetched atomic.Int32
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
		if r.URL.Path == "/seg0.ts" {
			<-release
		}
		fetched.Add(1)
		w.Write(make([]byte, segSize))
	}))
	t.Cleanup(srv.Close)

	out := filepath.Join(t.TempDir(), "video.ts")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:        srv.URL + "/playlist.m3u8",
		OutputFile:     out,
		IsHls:          true,
		SegmentWorkers: workers,
	})
	d.delays = fastDelays()

	done := make(chan error, 1)
	go func() { done <- d.Start(context.Background()) }()

	// Let the pool saturate against the blocked head.
	time.Sleep(300 * time.Millisecond)
	held := fetched.Load()
	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start = %v, want nil", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Start did not return after the head segment was released — the buffer deadlocked")
	}

	// Ceiling (2 segments) + one in-flight slice per worker + the head.
	if maxHeld := int32(2 + workers + 1); held > maxHeld {
		t.Fatalf("%d segments fetched while segment 0 was blocked, want at most %d — the reorder buffer is unbounded", held, maxHeld)
	}
	info, err := os.Stat(out)
	if err != nil || info.Size() != int64(totalSegs*segSize) {
		t.Fatalf("output = %v/%v, want %d bytes — every segment must still land", info, err, totalSegs*segSize)
	}
}
