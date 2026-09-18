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

// TestHlsVodParallelBufferIsBounded pins ENGINE-2: while the head-of-order
// segment is blocked, the workers must stop fetching once the reorder buffer
// reaches its byte ceiling instead of racing the whole VOD into RAM.
//
// Mutant: restoring `buffer := make(map[int][]byte)` (no ceiling) — all 40
// segments are fetched while segment 0 is blocked, which is the unbounded
// growth the row measured at 0.6-2.5 GB in the field.
func TestHlsVodParallelBufferIsBounded(t *testing.T) {
	const (
		totalSegs = 40
		segSize   = 64 << 10
		workers   = 8
	)

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
	d.hlsVodBufferBytesOverride = 2 * segSize // room for two non-head segments

	done := make(chan error, 1)
	go func() { done <- d.Start(context.Background()) }()

	held := waitForVodFetchFixedPoint(t, &fetched)
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

// oneShotCancelledContext answers exactly ONE Err() call with
// context.Canceled once armed, and every later call truthfully. Done() is
// the parent's and is never closed, so nothing else in the stack (the HTTP
// transport in particular) changes behaviour.
//
// It exists because the review's finding-1 interleaving cannot be produced
// by timing. Work items leave the `work` channel in FIFO index order, so the
// set of indices a teardown drains is normally a SUFFIX — above every index
// a worker is parked on, which the consumer then never has to reach. The
// hazard needs the opposite: one worker descheduled between its receive from
// `work` and its cancellation check, long enough for a LATER index to be
// fetched and parked on the ceiling above the resulting hole. That window is
// a single atomic load wide. Its observable effect, though, is exactly "one
// worker-loop iteration saw a cancelled ctx and the iterations after it did
// not" — which is what this reproduces, deterministically.
type oneShotCancelledContext struct {
	context.Context
	armed atomic.Bool
}

func (c *oneShotCancelledContext) Err() error {
	if c.armed.CompareAndSwap(true, false) {
		return context.Canceled
	}
	return c.Context.Err()
}

// TestHlsVodParallelDrainedIndexReleasesParkedWorkers pins the Task 6 review's
// finding 1. Once the ceiling can PARK workers, the worker loop's teardown
// branch (`continue // drain channel`) became a LIVENESS hazard, not just an
// incompleteness one: it dequeues an index and emits no buffer entry for it,
// so the consumer's take() can never succeed there. Every worker parked above
// that hole then waits on room only the consumer could free, wg.Wait never
// returns, results is never closed, and the deferred rb.release() only runs
// once the function returns — so Start hangs FOREVER, past Cancel() and past
// ctx, holding a download slot for the life of a 24/7 process. The drain
// branch must tell the buffer the index is never coming (markFailed), which
// drops those workers and truncates cleanly instead.
//
// Mutant: delete `rb.markFailed(item.idx)` from the drain branch — five
// workers stay parked on segments 13-17 and this row fails on its 5 s bound.
// deadlineTestContext keeps that mutant failing FAST: cleanups run LIFO, so
// its cancel fires before the server's Close and a wedged run reports instead
// of hanging the package run.
func TestHlsVodParallelDrainedIndexReleasesParkedWorkers(t *testing.T) {
	const (
		// 18 segments and a work channel of `workers` means the feeder has
		// pushed every item and exited by the time the pool saturates, so at
		// the fixed point nothing but the worker loop can call ctx.Err().
		totalSegs = 18
		segSize   = 64 << 10
		workers   = 8
	)

	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
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
	// Cleanups run LIFO, so register the server's Close FIRST: the ctx cancel
	// and the head's release both have to fire before it, or Close waits on
	// the still-blocked seg0 handler.
	t.Cleanup(srv.Close)
	parent, _ := deadlineTestContext(t)
	t.Cleanup(releaseOnce)
	ctx := &oneShotCancelledContext{Context: parent}

	out := filepath.Join(t.TempDir(), "video.ts")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:        srv.URL + "/playlist.m3u8",
		OutputFile:     out,
		IsHls:          true,
		SegmentWorkers: workers,
	})
	d.delays = fastDelays()
	d.hlsVodBufferBytesOverride = 2 * segSize

	done := make(chan error, 1)
	go func() { done <- d.Start(ctx) }()

	// Fixed point: one worker blocked on the head, two segments resident in
	// the buffer, the other six parked in admit() each holding one fetched
	// segment. Indices 1..held are in hand, held+1.. are still queued, and
	// nobody is calling ctx.Err() — so the arm below is guaranteed to land on
	// a worker-loop iteration and not on a fetch.
	held := waitForVodFetchFixedPoint(t, &fetched)

	// Arm, then let the head land. The head's worker flushes segment 0, loops
	// for the next item, and that ONE iteration sees a cancelled ctx: index
	// held+1 is drained with no buffer entry. Every iteration after it is
	// normal, so the remaining segments are fetched and pile up on the
	// ceiling ABOVE the hole — the inversion the field race produces.
	ctx.armed.Store(true)
	releaseOnce()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("Start did not return within 5s — the drained index %d left the consumer waiting for a buffer entry that never arrives, with workers parked above it", held+1)
	}

	// Back-pressure never drops a fetched segment: the head plus everything
	// already in hand is on disk, and the write stops at the drained index —
	// a clean prefix, not a reordered or short one.
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat output: %v", err)
	}
	if want := int64(held+1) * segSize; info.Size() != want {
		t.Fatalf("wrote %d bytes, want %d — the head plus the %d segments already fetched when the drain landed", info.Size(), want, held)
	}
}

// waitForVodFetchFixedPoint blocks until the fetch counter stops changing —
// a genuine fixed point rather than a wall-clock guess. With the head blocked
// and the ceiling full, every worker is parked in admit(), so the count
// settles and stays settled; a slow box waits longer, it cannot under-count.
// (With no ceiling the count settles only once the WHOLE playlist has been
// fetched, which is exactly what the bound assertion above catches.) A count
// still at zero is never a fixed point — that is a broken fixture, not a
// saturated pool, so it fails rather than reporting 0.
func waitForVodFetchFixedPoint(t *testing.T, fetched *atomic.Int32) int32 {
	t.Helper()
	var held int32
	settleDeadline := time.Now().Add(15 * time.Second)
	for stable := 0; stable < 20 && time.Now().Before(settleDeadline); {
		time.Sleep(10 * time.Millisecond)
		if n := fetched.Load(); n == held && n > 0 {
			stable++
		} else {
			held, stable = n, 0
		}
	}
	if held == 0 {
		t.Fatal("no segment was fetched within 15s — the fixture never started downloading")
	}
	return held
}
