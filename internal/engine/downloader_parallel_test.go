package engine

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestCatchUpBatchLimitDamping ports moonarchive's post-failure throttle: a
// fresh downloader may use the full batch, a downloader that just hit a
// failure episode drops to a small batch, and the ceiling regrows with time
// so a single blip doesn't cripple catch-up for the rest of the archive.
func TestCatchUpBatchLimitDamping(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{BaseURL: "https://example.invalid/v", OutputFile: "x"})
	fullBatch := d.maxCatchupBatch()
	dampedFloor := d.segmentWorkers()

	if got := d.catchUpBatchLimit(); got != fullBatch {
		t.Errorf("fresh limit = %d, want the full %d", got, fullBatch)
	}

	// A failure damps to one full parallel wave, never below it. Collapsing
	// to a single segment is what made real catch-up run 0-2 segments wide
	// (field evidence, 2026-08-15) while 403 episodes arrived every ~15-30s.
	d.noteCatchUpFailureEpisode()
	got := d.catchUpBatchLimit()
	if got != dampedFloor {
		t.Errorf("limit immediately after a failure = %d, want the floor %d", got, dampedFloor)
	}

	// 10s later the ceiling has regrown (1 per second) but not to full.
	d.lastCatchUpFailure.Store(time.Now().Add(-10 * time.Second))
	got = d.catchUpBatchLimit()
	if want := dampedFloor + 10; got != want {
		t.Errorf("limit 10s after a failure = %d, want %d", got, want)
	}

	// A repeating failure cadence must NOT pin catch-up near the floor: with
	// episodes every 20s the ceiling still recovers most of its width, which
	// is the regression this retune fixes.
	d.lastCatchUpFailure.Store(time.Now().Add(-20 * time.Second))
	if got := d.catchUpBatchLimit(); got < 20 {
		t.Errorf("limit 20s after a failure = %d, want >= 20 (a 20s failure cadence must not pin catch-up near the floor)", got)
	}

	// Long after, the full batch is available again.
	d.lastCatchUpFailure.Store(time.Now().Add(-2 * time.Hour))
	if got := d.catchUpBatchLimit(); got != fullBatch {
		t.Errorf("limit long after a failure = %d, want the full %d", got, fullBatch)
	}
}

// TestReorderBufferHeadAlwaysAdmitted pins the deadlock-avoidance guarantee
// called out in the catch-up-throughput task 3 brief: the segment matching
// the buffer's head must be admitted immediately even when doing so leaves
// the buffer over its own ceiling on its own. If admit() ever queued the
// head behind the ceiling check, a saturated buffer could never flush --
// nothing frees room without the head landing first.
func TestReorderBufferHeadAlwaysAdmitted(t *testing.T) {
	const limit = 20
	rb := newReorderBuffer(limit, 0)

	admitted := make(chan struct{})
	go func() {
		rb.admit(0, make([]byte, limit+5)) // bigger than the whole ceiling
		close(admitted)
	}()

	select {
	case <-admitted:
	case <-time.After(2 * time.Second):
		t.Fatal("admit(head) blocked — the head segment must never be refused")
	}

	if got := rb.residentBytes(); got != limit+5 {
		t.Errorf("residentBytes = %d, want %d", got, limit+5)
	}
}

// TestReorderBufferBlocksNonHeadWhenFull is the direct test that the byte
// ceiling actually throttles: this is the behavior that would make the old
// (uncapped) buffer.go regress the memory bound task 3 exists to add. With
// no ceiling enforcement at all, the goroutine below would return
// immediately instead of blocking, and the "still blocked" branch would
// fail the test.
func TestReorderBufferBlocksNonHeadWhenFull(t *testing.T) {
	const limit = 20
	rb := newReorderBuffer(limit, 0)
	rb.admit(0, make([]byte, limit)) // head segment fills the buffer exactly

	nonHeadDone := make(chan struct{})
	go func() {
		rb.admit(1, make([]byte, 3)) // not the head; buffer already at the ceiling
		close(nonHeadDone)
	}()

	select {
	case <-nonHeadDone:
		t.Fatal("admit(non-head) returned while the buffer is at its ceiling — ceiling not enforced")
	case <-time.After(200 * time.Millisecond):
		// Expected: still blocked.
	}

	// Flushing the head frees room and must wake the blocked admit.
	data, ok := rb.take(0)
	if !ok || len(data) != limit {
		t.Fatalf("take(0) = (%d bytes, %v), want (%d bytes, true)", len(data), ok, limit)
	}
	rb.setHead(1)

	select {
	case <-nonHeadDone:
	case <-time.After(2 * time.Second):
		t.Fatal("admit(non-head) never unblocked after room freed")
	}
	if got := rb.residentBytes(); got != 3 {
		t.Errorf("residentBytes after flush+admit = %d, want 3", got)
	}
}

// TestReorderBufferManyWorkersStayBounded is the direct proof of the task's
// headline property: raising the worker count must not multiply memory. It
// stalls the head (never admits seq 0, mirroring "the fake GVS stalls
// seq == curSeq" scenario from the brief) and races far more concurrent
// non-head admits at the buffer than would fit under the ceiling. Without
// byte-based accounting (e.g. the pre-task segmentWorkers*3 COUNT bound),
// this would let all of them through: 40 workers * 5 MB = 200 MB resident
// against a 30 MB ceiling. With the ceiling enforced, residency must
// plateau at (approximately) the limit regardless of worker count.
func TestReorderBufferManyWorkersStayBounded(t *testing.T) {
	const limit = 30 << 20  // 30 MB
	const segSize = 5 << 20 // 5 MB -- in the 3.7-6.2 MB range measured on a 1080p60 stream
	const numWorkers = 40   // far more than limit/segSize (6) would allow if unbounded

	rb := newReorderBuffer(limit, 0) // head = 0, stalled forever in this test

	var wg sync.WaitGroup
	for seq := 1; seq <= numWorkers; seq++ {
		wg.Add(1)
		go func(seq int) {
			defer wg.Done()
			rb.admit(seq, make([]byte, segSize))
		}(seq)
	}

	// Let every goroutine race to admit; anything that fits under the
	// ceiling should land well within this window.
	time.Sleep(300 * time.Millisecond)

	if got := rb.residentBytes(); got > limit {
		t.Errorf("residentBytes = %d (%d workers of %d bytes each), want <= %d (ceiling limit) — "+
			"a wider worker pool must not multiply buffered memory",
			got, numWorkers, segSize, limit)
	}

	// Release so the still-blocked goroutines don't leak past the test.
	rb.release()
	wg.Wait()
}

// TestReorderBufferReleaseUnblocksWithoutRoom pins the other half of the
// deadlock-avoidance contract: release() must free every blocked admit()
// even when no room was ever freed by a flush. This is the safety valve
// runParallelCatchUp relies on when the segment the buffer is waiting on
// permanently fails -- see its call sites for rb.release().
func TestReorderBufferReleaseUnblocksWithoutRoom(t *testing.T) {
	const limit = 10
	rb := newReorderBuffer(limit, 0)
	rb.admit(0, make([]byte, limit)) // fills the buffer; head 0 never advances

	blocked := make(chan struct{})
	go func() {
		rb.admit(1, make([]byte, 1))
		close(blocked)
	}()

	select {
	case <-blocked:
		t.Fatal("admit(non-head) returned before release — ceiling not enforced")
	case <-time.After(200 * time.Millisecond):
	}

	rb.release()

	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("admit(non-head) did not unblock after release()")
	}
}

// TestRunParallelCatchUpOrdersCorrectlyWithByteCeiling drives the real
// runParallelCatchUp (not just the reorderBuffer in isolation) against a
// fake GVS, with enough workers and a small enough batch that segments
// routinely arrive out of order relative to the byte-bounded buffer. Proves
// the reorderBuffer wiring introduced in task 3 didn't break plain
// sequential correctness.
func TestRunParallelCatchUpOrdersCorrectlyWithByteCeiling(t *testing.T) {
	t.Parallel()
	const head = 100
	_, srv := newFakeGVS(t, head, func(seq, attempt int) int {
		return http.StatusOK
	})

	out := filepath.Join(t.TempDir(), "v")
	f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	defer f.Close()

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:        srv.URL + "/videoplayback?id=rb.1&itag=140",
		OutputFile:     out,
		SegmentWorkers: 12,
	})
	d.outputFile = f
	d.currentSeq.Store(0)
	d.headSeq.Store(head)

	nextSeq, err := d.runParallelCatchUp(context.Background())
	if err != nil {
		t.Fatalf("runParallelCatchUp error = %v, want nil", err)
	}
	wantTarget := head - stayBehindSegments // 70; within catchUpBatchLimit (8*12=96)
	if nextSeq != wantTarget {
		t.Errorf("nextSeq = %d, want %d", nextSeq, wantTarget)
	}
	wantSegments(t, out, 0, nextSeq-1)
}

// TestRunParallelCatchUpNoDeadlockOnPermanentGap is the end-to-end version
// of the deadlock-avoidance guarantee: a segment inside the batch 410s
// permanently (never retried) while a wide worker pool races ahead of it.
// Before rb.release() was wired to every fetch failure, workers holding
// later, already-fetched segments could stay parked in admit() forever once
// the buffer filled, because the dead segment can never become head and
// free room. This must complete promptly and report the gap exactly like
// the pre-task unbounded buffer did.
func TestRunParallelCatchUpNoDeadlockOnPermanentGap(t *testing.T) {
	t.Parallel()
	const head = 200
	const failSeq = 20 // permanently gone -- forces rb.release() before the gap
	_, srv := newFakeGVS(t, head, func(seq, attempt int) int {
		if seq == failSeq {
			return http.StatusGone // 410: ErrSegmentPermanent, never retried
		}
		return http.StatusOK
	})

	out := filepath.Join(t.TempDir(), "v")
	f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	defer f.Close()

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:        srv.URL + "/videoplayback?id=rb.2&itag=140",
		OutputFile:     out,
		SegmentWorkers: 8, // wide enough that workers race well past failSeq
	})
	d.outputFile = f
	d.currentSeq.Store(0)
	d.headSeq.Store(head)

	var gapMu sync.Mutex
	var gaps []DownloadGap
	d.OnGap = func(g DownloadGap) {
		gapMu.Lock()
		gaps = append(gaps, g)
		gapMu.Unlock()
	}

	type result struct {
		nextSeq int
		err     error
	}
	done := make(chan result, 1)
	go func() {
		nextSeq, err := d.runParallelCatchUp(context.Background())
		done <- result{nextSeq, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("runParallelCatchUp error = %v, want nil", r.err)
		}
		if r.nextSeq != failSeq {
			t.Errorf("nextSeq = %d, want %d (stopped at the permanently-gone segment)", r.nextSeq, failSeq)
		}
		wantSegments(t, out, 0, failSeq-1)
	case <-time.After(10 * time.Second):
		t.Fatal("runParallelCatchUp did not return within 10s — deadlock on the permanent gap")
	}

	gapMu.Lock()
	defer gapMu.Unlock()
	if len(gaps) != 1 || gaps[0].From != failSeq {
		t.Errorf("gaps reported = %+v, want exactly one gap starting at %d", gaps, failSeq)
	}
}
