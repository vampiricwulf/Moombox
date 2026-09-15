package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hlsAdvancingServer serves a one-segment live window whose media sequence
// advances by exactly 1 per reload (so currentSeq advances every iteration
// and never gaps), then an EXT-X-ENDLIST playlist after liveReloads reloads
// so the loop exits on its own.
func hlsAdvancingServer(t *testing.T, liveReloads int) *httptest.Server {
	t.Helper()
	var reloads atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, _ *http.Request) {
		n := int(reloads.Add(1))
		seq := 100 + n - 1
		end := ""
		if n > liveReloads {
			end = "#EXT-X-ENDLIST\n"
		}
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n"+
			"#EXT-X-MEDIA-SEQUENCE:%d\n#EXTINF:1.0,\nseg%d.ts\n%s", seq, seq, end)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "[%s]", strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".ts"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestHlsResumeSaveIsRateLimited pins T2-14: the live loop fsync+renamed the
// resume sidecar on EVERY playlist reload (~2 s on Twitch) because the seq
// advances every reload. With the floor, a burst of reloads inside one
// interval costs ONE write, and the loop's deferred final save still records
// the last sequence.
//
// Mutant this kills: dropping the time floor (or comparing against a clock
// that is reset on every iteration) — writes then equal the reload count,
// 12+ instead of 2.
func TestHlsResumeSaveIsRateLimited(t *testing.T) {
	const liveReloads = 12
	srv := hlsAdvancingServer(t, liveReloads)

	var mu sync.Mutex
	var saved []int
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/playlist.m3u8",
		OutputFile: filepath.Join(t.TempDir(), "video.ts"),
		StartSeq:   -1,
		IsHls:      true,
		// StopOnGap keeps the final ENDLIST reload (which also carries the
		// last new segment) on the sequential per-iteration path instead of
		// the VOD-parallel finish (runHlsVodParallel, downloader_hls.go
		// :471-478) — that path is an unrelated, pre-existing feature with
		// its own unconditional final saveResume, and without this the test
		// would observe 3 writes (one extra from that path) instead of the
		// 2 this test is pinning on the live loop's own save sites.
		StopOnGap: true,
	})
	d.delays = fastDelays()
	// A floor far longer than the whole test: every reload after the first
	// falls inside one interval, so only the first save and the deferred
	// final save may write.
	d.delays.hlsResumeSave = 10 * time.Second
	d.onResumeSaved = func(lastSeq int) {
		mu.Lock()
		saved = append(saved, lastSeq)
		mu.Unlock()
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start() = %v, want nil (EXT-X-ENDLIST finish)", err)
	}

	mu.Lock()
	got := append([]int(nil), saved...)
	mu.Unlock()

	if len(got) != 2 {
		t.Fatalf("sidecar written %d times (LastSeq %v) across %d reloads, want 2 — one at loop entry and one from the deferred final save", len(got), got, liveReloads)
	}
	if want := 100 + liveReloads; got[len(got)-1] != want {
		t.Errorf("final sidecar LastSeq = %d, want %d — the deferred save must still record the true final position", got[len(got)-1], want)
	}
}

// TestDefaultDelaysIncludesResumeSave is the production-timing pin for the
// new field, in the same spirit as TestDefaultDelaysMatchConstants: a floor
// silently defaulting to 0 would restore the every-reload fsync with every
// other test still green.
func TestDefaultDelaysIncludesResumeSave(t *testing.T) {
	if got, want := defaultDelays().hlsResumeSave, 15*time.Second; got != want {
		t.Errorf("defaultDelays().hlsResumeSave = %v, want %v", got, want)
	}
	d := NewSegmentDownloader(DownloaderOptions{OutputFile: filepath.Join(t.TempDir(), "o.ts")})
	if d.delays.hlsResumeSave != hlsResumeSaveInterval {
		t.Errorf("NewSegmentDownloader installed hlsResumeSave = %v, want %v", d.delays.hlsResumeSave, hlsResumeSaveInterval)
	}
}
