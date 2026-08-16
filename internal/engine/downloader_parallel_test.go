package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
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
//
// The assertion allows up to one extra segment over the nominal limit:
// admit() checks `bytes < limit` BEFORE adding the segment being admitted
// (see reorderBuffer's doc), so the LAST segment that squeezes in can push
// resident bytes up to limit+segSize-1. Asserting the exact limit would
// only happen to pass here because segSize evenly divides limit (5 MB into
// 30 MB) -- a coincidence of these two constants, not a real invariant.
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

	if got := rb.residentBytes(); got > limit+segSize {
		t.Errorf("residentBytes = %d (%d workers of %d bytes each), want <= limit+segSize (%d) — "+
			"a wider worker pool must not multiply buffered memory",
			got, numWorkers, segSize, limit+segSize)
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
// Without rb.markFailed() wired to every fetch failure, workers holding
// later, already-fetched segments could stay parked in admit() forever once
// the buffer filled, because the dead segment can never become head and
// free room. This must complete promptly and report the gap exactly like
// the pre-task unbounded buffer did.
func TestRunParallelCatchUpNoDeadlockOnPermanentGap(t *testing.T) {
	t.Parallel()
	const head = 200
	const failSeq = 20 // permanently gone -- forces rb.markFailed(failSeq) before the gap
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

// TestReorderBufferMarkFailedDropsSegmentsAboveIt is the direct,
// deterministic test of the CRITICAL fix: a worker already blocked in
// admit() for a higher seq must unblock (and drop, not store) once some
// OTHER, lower seq is marked failed -- even though nothing ever flushed and
// no room was freed. This is exactly what runParallelCatchUp's
// cancellation-skip branch relies on: a worker that skips its work item
// because the download was cancelled never calls fetchSegmentWithRetry, so
// it has to be the one calling markFailed directly, precisely to rescue any
// OTHER worker already parked waiting for room that a cancelled download
// will never free (the scenario that could hang the whole call before this
// fix, since the success path never itself checks cancellation).
func TestReorderBufferMarkFailedDropsSegmentsAboveIt(t *testing.T) {
	const limit = 10
	rb := newReorderBuffer(limit, 0)
	rb.admit(0, make([]byte, limit)) // fills the buffer; head 0 never advances in this test

	blocked := make(chan bool, 1)
	go func() {
		stored := rb.admit(5, make([]byte, 1)) // not head; buffer already at the ceiling
		blocked <- stored
	}()

	select {
	case <-blocked:
		t.Fatal("admit(5) returned before anything freed the buffer or recorded a failure")
	case <-time.After(200 * time.Millisecond):
		// Expected: still blocked.
	}

	// A DIFFERENT, lower seq (2) is marked failed -- e.g. a worker skipped
	// it because the download was cancelled before it ever attempted a
	// fetch. Nothing has flushed and no room has freed; markFailed alone
	// must be enough to release the blocked admit(5).
	rb.markFailed(2)

	select {
	case stored := <-blocked:
		if stored {
			t.Error("admit(5) reported stored=true after seq 2 was marked failed — it should have been dropped, not buffered")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("admit(5) never unblocked after markFailed(2) — this is the CRITICAL cancel-while-blocked permanent hang")
	}

	if rb.has(5) {
		t.Error("seq 5 is stored in the buffer after being dropped by markFailed — it should have been discarded")
	}
}

// TestReorderBufferFailureDoesNotReopenUnboundedGrowth is the direct proof
// of the Important-severity fix: a single failure must NOT reopen the
// buffer to unlimited growth for the rest of the batch. The original
// implementation called a blanket release() on any failure -- disabling
// the ceiling entirely -- which meant the FIRST failing batch of exactly
// the head-stall conditions that fill the buffer (a 403 episode) restored
// the full pre-ceiling exposure (up to maxCatchupBatch * segment size,
// GBs at high worker counts) on hosts the ceiling exists to protect. The
// fix drops (never stores) anything above the known failure point instead
// of admitting it, so residency must stay flat regardless of how many
// segments arrive afterward.
func TestReorderBufferFailureDoesNotReopenUnboundedGrowth(t *testing.T) {
	const limit = 1 << 20 // 1 MB
	rb := newReorderBuffer(limit, 0)
	rb.markFailed(5) // some low seq is known permanently unavailable

	// 100 segments at 100 KB each = 10 MB if buffered -- ten times the
	// ceiling, and nowhere near a real 403 episode's actual segment count
	// or size. None of it may be stored.
	for seq := 6; seq < 106; seq++ {
		if stored := rb.admit(seq, make([]byte, 100<<10)); stored {
			t.Fatalf("admit(%d) stored data after markFailed(5) — want dropped, memory bound reopened", seq)
		}
	}
	if got := rb.residentBytes(); got != 0 {
		t.Errorf("residentBytes = %d, want 0 — nothing above a known failure point should ever be buffered", got)
	}
}

// TestCatchUpRollingWindowThroughput is the throughput-shaped regression
// test for the rolling-window catch-up: a far-behind download must drain its
// whole span in ONE continuously-fed pool rather than batch-quantised waves
// with a dedicated head probe between each. It drives the real runDashLoop
// (d.Start) against a fake GVS where every segment costs a small fixed delay
// and every dedicated head probe costs a large fixed delay, then asserts
// total wall clock well under what the batched structure must pay.
//
// The numbers, and why the margin cannot flake on a busy machine:
//
//	240 segments, 4 workers, so catchUpBatchLimit = 8*4 = 32 -> the batched
//	structure needs ceil(240/32) = 8 runParallelCatchUp calls, and its
//	runDashLoop ran an UNCONDITIONAL probeHeadSequence after every one of
//	them, plus the initial head-discovery probe: 9 serial probes at 500ms
//	each is >= 4.5s of wall clock before a single segment's 5ms delay is
//	counted (batched model total: 9*500ms + 240/4*5ms = 4.8s). The rolling
//	window pays exactly ONE probe (initial head discovery -- segment-header
//	harvests keep head fresh afterward), so its nominal time is
//	500ms + 240/4*5ms + overhead, comfortably under 1.5s even with coarse
//	Windows timers. The 2.5s budget sits >= 2s below the batched floor and
//	>= 1s above the rolling nominal -- it discriminates on structure, not
//	scheduler luck.
func TestCatchUpRollingWindowThroughput(t *testing.T) {
	t.Parallel()
	const (
		head       = 400
		endSeq     = 239 // 240 segments: 0..239 (EndSeq clamp ends the run)
		numWorkers = 4
		segDelay   = 5 * time.Millisecond
		probeDelay = 500 * time.Millisecond
		budget     = 2500 * time.Millisecond
	)
	var probes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Head-Seqnum", strconv.Itoa(head))
		seq, err := strconv.Atoi(r.URL.Query().Get("sq"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if seq > head {
			// Dedicated head-discovery probe (sq=999999999): models the real
			// round trip each probe costs. The batched structure paid this
			// between every batch; the rolling window only at entry.
			probes.Add(1)
			time.Sleep(probeDelay)
			w.WriteHeader(http.StatusOK)
			return
		}
		time.Sleep(segDelay)
		fmt.Fprintf(w, "seg-%05d;", seq)
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "v")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:        srv.URL + "/videoplayback?id=t4.throughput&itag=140",
		OutputFile:     out,
		EndSeq:         endSeq,
		SegmentWorkers: numWorkers,
		MaxTimeout:     time.Minute,
		// Still live -- the loop must exit via the EndSeq clamp, never via
		// end-verification (no gone bursts occur: every seq <= endSeq serves).
		CheckStreamStatus: func(context.Context) (bool, error) { return false, nil },
	})

	start := time.Now()
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start = %v, want nil", err)
	}
	elapsed := time.Since(start)

	// Correctness first: the speed is worthless if the recording has a hole.
	wantSegments(t, out, 0, endSeq)

	if elapsed > budget {
		t.Errorf("caught up %d segments in %v, want < %v -- the pool is draining at batch boundaries "+
			"and paying a head probe per batch (%d probes observed) instead of rolling continuously",
			endSeq+1, elapsed.Round(time.Millisecond), budget, probes.Load())
	}
}

// TestRunParallelCatchUpCancelWhileBlockedAtCeilingUnwinds is an
// end-to-end regression test for the general "cancel while a worker is
// genuinely blocked at the byte ceiling must unwind cleanly" property. The
// head segment never answers (an httptest handler that blocks on its own
// request context, simulating a permanently stuck live segment); every
// other segment succeeds immediately and is sized to saturate the test's
// tiny buffer ceiling almost instantly, so by the time the test cancels,
// at least one worker is provably parked in admit() waiting for room the
// stuck head can never free on its own.
//
// IMPORTANT CAVEAT: this test resolves via the stuck head's OWN
// fetchSegmentWithRetry call returning an error once its in-flight HTTP
// request is aborted by the cancelled ctx, which routes through the
// (already-present) fetch-failure branch's rb.markFailed(item.seq) --
// NOT through the cancellation-skip branch the CRITICAL finding was about
// (`if d.isCancelled() || ctx.Err() != nil { ...; continue }`, hit BEFORE
// a fetch is even attempted). I could not construct a deterministic
// end-to-end reproduction of that exact branch: it only matters when a
// worker has already dequeued a work item but the Go scheduler hasn't yet
// run its cancellation check -- a sub-microsecond preemption race that
// wall-clock HTTP timing can't reliably force (and by the time any OTHER
// item is dispatched after such a race could occur, FIFO dispatch order
// guarantees every lower seq is already claimed, so a "still queued"
// item can never be the one that rescues an already-blocked worker; only
// a same-wave item whose goroutine simply hasn't run yet can be, and that
// is exactly the unforceable part). TestReorderBufferMarkFailedDropsSegmentsAboveIt
// is the deterministic, honest proof of the mechanism the skip branch's
// rb.markFailed(item.seq) call relies on -- that markFailed(lowerSeq)
// unblocks admit(higherSeq) even when nothing has ever flushed and no
// fetch for the higher seq ever failed. This test still has value as
// end-to-end coverage of the same overall failure mode via a reliably
// reproducible path, and would have caught any regression that broke
// rb.markFailed's wiring into the fetch-failure branch too.
func TestRunParallelCatchUpCancelWhileBlockedAtCeilingUnwinds(t *testing.T) {
	t.Parallel()
	const headSeq = 0
	var headRequests atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Head-Seqnum", "999")
		seq, _ := strconv.Atoi(r.URL.Query().Get("sq"))
		if seq == headSeq {
			headRequests.Add(1)
			// Never answers on its own -- only unblocks when the CLIENT's
			// context is cancelled, which net/http propagates down to
			// r.Context(). That's the exact unstick this test verifies:
			// cancelling runParallelCatchUp's ctx must be what breaks this
			// request, not a response from the server.
			<-r.Context().Done()
			return
		}
		w.Write(make([]byte, 200<<10)) // 200 KB -- oversized relative to the ceiling below
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "v")
	f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	defer f.Close()

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:        srv.URL + "/videoplayback?id=rb.cancel&itag=140",
		OutputFile:     out,
		SegmentWorkers: 6,
	})
	d.outputFile = f
	d.currentSeq.Store(int64(headSeq))
	d.headSeq.Store(200)
	// Shrink the ceiling (see catchUpBufferBytesOverride's doc) so one
	// 200 KB non-head segment saturates it -- no need to wait on real
	// production-scale (256 MB) transfers to reach the blocked state this
	// test is exercising.
	d.catchUpBufferBytesOverride = 256 << 10

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct {
		nextSeq int
		err     error
	}
	done := make(chan result, 1)
	go func() {
		nextSeq, err := d.runParallelCatchUp(ctx)
		done <- result{nextSeq, err}
	}()

	// Wait until the head request has actually landed (proves the stall is
	// in effect), then give the other workers a moment to race ahead and
	// saturate the tiny ceiling.
	deadline := time.Now().Add(3 * time.Second)
	for headRequests.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if headRequests.Load() == 0 {
		t.Fatal("head request never arrived — test setup didn't stall the head as intended")
	}
	time.Sleep(200 * time.Millisecond)

	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runParallelCatchUp did not return within 10s after cancel — permanent hang (the CRITICAL cancel-while-blocked bug)")
	}
}
