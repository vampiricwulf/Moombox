package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// reorderBuffer accumulates out-of-order segments fetched by catch-up's
// worker pool until they can be written to disk in ascending sequence
// order. admit blocks a non-head segment once resident bytes reach limit,
// so raising SegmentWorkers costs connections, not memory (see
// catchUpBufferBytes) — but the segment matching head is ALWAYS admitted
// immediately regardless of fullness: refusing it would deadlock the whole
// buffer, since nothing can ever free room without it landing and
// flushing. The ceiling check runs BEFORE the admitted segment's bytes are
// added, so resident bytes can transiently overshoot limit by up to one
// segment's size — and a worker that hasn't reached admit() yet (still
// fetching, or blocked waiting for room) holds its slice on its own stack,
// uncounted by residentBytes. True peak process memory for one catch-up
// call is therefore closer to limit + workers*maxSegmentSize than to limit
// alone.
//
// markFailed / minFailedSeq record the lowest sequence known to be
// permanently unavailable in this batch — gone, retries exhausted, or
// skipped because the download was cancelled. Once set, admit() drops
// (does not buffer) any segment above that point instead of blocking for
// room: nothing can ever flush past a permanent hole in a single catch-up
// call, so buffering data beyond it is guaranteed waste, discarded at
// return either way. This also closes a deadlock: a worker already
// blocked waiting for room for a segment above the failure point would
// otherwise wait forever, since nothing will ever free room by flushing
// past a gap that will never fill in. This is deliberately a targeted
// drop keyed on seq, not a blanket ceiling bypass — a 403 episode is
// exactly the kind of head-stall condition that fills the buffer in the
// first place, so disabling the ceiling entirely on any single failure
// would reopen the full pre-ceiling exposure (up to maxCatchupBatch *
// segment size) for the remainder of the call. minFailedSeq only ever
// decreases: the lowest failure seen is what governs what's still worth
// buffering.
//
// release permanently disables the ceiling for every seq (every future
// admit returns immediately without storing) and wakes any blocked
// callers. It is reserved for teardown unrelated to any specific
// segment's availability — the consumer returning early (e.g. a disk
// write error) has nothing to do with which seq failed, so there's no
// seq to hand markFailed. Once nothing will ever read from this buffer
// again, no worker should be left stranded waiting for room.
//
// claim / beginClaims turn the buffer into the rolling-window work source
// for catch-up's worker pool: instead of a feeder goroutine pushing one
// fixed batch, each worker asks claim() for the next sequence to fetch.
// Claims are handed out contiguously, gated by a sliding window anchored
// at the flush position (head): a worker may claim seq S only while
// S < head + window(). Because the consumer advances head via setHead()
// after every write, the window refills as segments land on disk — the
// pool never waits for a batch boundary. Claims end (claim returns false,
// permanently) when the target is reached, when any failure is recorded
// (nothing can flush past a permanent hole, so fetching beyond it is
// guaranteed waste), or on release. target and window are read live under
// the buffer's own lock, so the target chases a head that advances during
// catch-up and a post-failure damped window regrows mid-call.
type reorderBuffer struct {
	mu    sync.Mutex
	cond  *sync.Cond
	seg   map[int][]byte
	bytes int
	limit int
	head  int

	// failed / minFailedSeq: see the type doc.
	failed       bool
	minFailedSeq int

	// released: see the type doc.
	released bool

	// claimNext / claimTarget / claimWindow / claimsOver: see the type doc.
	// claimTarget and claimWindow must be lock-free (they are called with
	// mu held); both closures read only atomics and immutable options.
	claimNext   int
	claimTarget func() int
	claimWindow func() int
	claimsOver  bool
}

func newReorderBuffer(limit, head int) *reorderBuffer {
	rb := &reorderBuffer{seg: make(map[int][]byte), limit: limit, head: head}
	rb.cond = sync.NewCond(&rb.mu)
	return rb
}

// admit blocks until there is room for a non-head segment, then stores it
// and returns true. Returns false without storing when the segment is
// provably unflushable in this batch (see markFailed) or the buffer has
// been released for teardown (see release) — callers must treat a false
// return as "this data can be discarded, nothing will ever read it," not
// as an error. The segment matching the buffer's current head is always
// stored immediately, no matter how full the buffer is or whether the
// buffer has a recorded failure: head can never itself sit above
// minFailedSeq, since nothing flushes past a gap, so head simply stops
// advancing there.
func (b *reorderBuffer) admit(seq int, data []byte) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for {
		if b.released {
			return false
		}
		if b.failed && seq > b.minFailedSeq && seq != b.head {
			return false
		}
		if seq == b.head || b.bytes < b.limit {
			b.seg[seq] = data
			b.bytes += len(data)
			return true
		}
		b.cond.Wait()
	}
}

// take removes and returns the segment for seq, if present, freeing its
// share of the ceiling and waking any admit() callers blocked on room.
func (b *reorderBuffer) take(seq int) ([]byte, bool) {
	b.mu.Lock()
	data, ok := b.seg[seq]
	if ok {
		delete(b.seg, seq)
		b.bytes -= len(data)
	}
	b.mu.Unlock()
	if ok {
		b.cond.Broadcast()
	}
	return data, ok
}

// has reports whether seq is currently buffered, without removing it. Used
// to find how far a gap extends once nextSeq itself can't be found.
func (b *reorderBuffer) has(seq int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.seg[seq]
	return ok
}

// setHead updates the sequence exempt from the ceiling and wakes admit()
// callers so they can re-check whether they are now the head.
func (b *reorderBuffer) setHead(seq int) {
	b.mu.Lock()
	b.head = seq
	b.mu.Unlock()
	b.cond.Broadcast()
}

// markFailed records seq as permanently unavailable in this batch,
// tightening the lowest known failure point when seq is lower than any
// previously recorded value. Wakes every blocked admit() so a segment
// above the (possibly new, lower) threshold drops out immediately instead
// of waiting for room that will never come — see the type doc for why
// this is scoped to seq rather than a blanket ceiling bypass.
func (b *reorderBuffer) markFailed(seq int) {
	b.mu.Lock()
	if !b.failed || seq < b.minFailedSeq {
		b.failed = true
		b.minFailedSeq = seq
	}
	b.mu.Unlock()
	b.cond.Broadcast()
}

// release permanently disables the ceiling for every seq, waking every
// blocked admit() call regardless of how it relates to any failure.
// Reserved for teardown unrelated to any specific segment's availability
// — see the type doc. Idempotent and safe to call more than once.
func (b *reorderBuffer) release() {
	b.mu.Lock()
	b.released = true
	b.mu.Unlock()
	b.cond.Broadcast()
}

// residentBytes reports the current buffered byte total.
func (b *reorderBuffer) residentBytes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bytes
}

// beginClaims arms the rolling claim window (see the type doc): claims
// start at next and are handed out while claimNext < target() (exclusive)
// and claimNext < head + window(). Must be called before any claim().
func (b *reorderBuffer) beginClaims(next int, target, window func() int) {
	b.mu.Lock()
	b.claimNext = next
	b.claimTarget = target
	b.claimWindow = window
	b.mu.Unlock()
}

// claim hands out the next sequence for a worker to fetch, blocking while
// the sliding window is full (claimNext >= head + window()). Returns
// (seq, true) with a claim the caller MUST resolve — by admitting the
// fetched data or by markFailed(seq) — or (0, false) permanently once
// claiming is over: the target was reached, a failure was recorded, or the
// buffer was released for teardown. An unresolved claim stalls the flush
// (and therefore the window) forever, so even a worker that abandons its
// item (cancellation, panic) must mark it failed.
//
// Liveness: a claim() blocked on a full window is woken by the same
// broadcasts that rescue a blocked admit() — setHead (a flush advanced the
// window), take (room freed), markFailed, and release (both of which flip
// claim() to its false return). Every sequence below claimNext is owned by
// exactly one past claim, and each owner either flushes it (window slides),
// fails it (claims end), or is itself blocked in admit() and rescued by the
// head-always-admitted / drop-above-failure guarantees — so a blocked
// claim() can never be stranded.
func (b *reorderBuffer) claim() (int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for {
		if b.released || b.failed || b.claimsOver || b.claimTarget == nil {
			return 0, false
		}
		if b.claimNext >= b.claimTarget() {
			// Latch rather than re-check later: the pool winds down as one
			// once the span is covered, instead of thinning worker by worker
			// while in-flight harvests nudge the target forward. The caller
			// re-enters catch-up if the head has meaningfully advanced.
			b.claimsOver = true
			b.cond.Broadcast() // wake window-blocked siblings so they exit too
			return 0, false
		}
		if b.claimNext < b.head+b.claimWindow() {
			seq := b.claimNext
			b.claimNext++
			return seq, true
		}
		b.cond.Wait()
	}
}

// claimedUpTo reports the exclusive upper bound of handed-out claims.
// Stable once every worker has exited (nothing else calls claim()); the
// consumer uses it as the bound for gap reporting — sequences above it
// were never attempted, so they are future work, not a gap.
func (b *reorderBuffer) claimedUpTo() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.claimNext
}

// runParallelCatchUp downloads segments in parallel to catch up to the live
// edge, staying stayBehindSegments behind it to avoid in-flight segments.
//
// One worker pool stays alive across the whole catch-up span: workers claim
// the next sequence from rb's rolling window (see reorderBuffer.claim) and
// the consumer flushes to disk in strictly ascending order as segments
// arrive, advancing the window with every write. The previous structure fed
// exactly one catchUpBatchLimit-sized batch, waited for EVERY worker, and
// returned — so the slowest segment in a batch idled the whole pool at each
// boundary and runDashLoop paid a dedicated head probe per batch (measured
// ~55% of the throughput the same connection count sustains). The target
// chases the live head as workers harvest X-Head-Seqnum from segment
// responses, so one call drains an arbitrarily-far-behind gap; memory stays
// bounded exactly as before because the claim window (catchUpBatchLimit,
// read live so post-failure damping and regrowth still apply) caps how far
// fetches may run ahead of the flush position, and rb's byte ceiling caps
// what those fetches may hold resident.
//
// Returns nextSeq = the first sequence NOT written: the (exclusive) end of
// the covered span when it completes, or the sequence AT a permanent gap —
// with OnGap / "stopping at gap" reporting — so the sequential loop's
// full-retry recovery re-engages exactly as it always has.
func (d *SegmentDownloader) runParallelCatchUp(ctx context.Context) (int, error) {
	curSeq := int(d.currentSeq.Load())
	head := int(d.headSeq.Load())
	// initialTarget is EXCLUSIVE and is the floor of the live target below:
	// the span [curSeq, initialTarget) is committed even if head never
	// advances again.
	initialTarget := max(head-stayBehindSegments, curSeq+1) // at least 1 segment
	// Respect endSeq limit (for timestamp-based trimming)
	if d.opts.EndSeq >= 0 && initialTarget > d.opts.EndSeq+1 {
		initialTarget = d.opts.EndSeq + 1
	}
	if initialTarget <= curSeq {
		return curSeq, nil
	}

	if d.OnProgress != nil {
		// Seq follows the last-WRITTEN convention like every other emission
		// (the mid-loop and exit bookends already do). Reporting the seeded
		// next-to-download value here persisted last_video_seq one ahead of
		// reality before any byte landed — a crash inside the catch-up
		// window then made the next restart's seeding skip that segment.
		d.OnProgress(DownloadProgress{
			Seq:        max(curSeq-1, 0),
			Bytes:      d.bytesWritten.Load(),
			HeadSeq:    head,
			CatchingUp: true,
		})
	}

	// workers is the operative concurrency for this download (see
	// segmentWorkers) — computed once so the results-channel sizing and the
	// worker-pool spawn below agree with each other.
	workers := d.segmentWorkers()
	// results only carries a notification that SOME segment landed in rb —
	// the data itself lives in rb, admitted by the worker before it sends
	// here, so the consumer doesn't need the payload to decide what to
	// flush next (it always re-checks from nextSeq forward).
	results := make(chan int, workers)
	// rb is the byte-bounded reorder buffer: workers admit fetched segments
	// into it (blocking there, not on `results`, once resident bytes reach
	// catchUpBufferBytes) and the consumer below drains it in ascending
	// order. See reorderBuffer's doc for the head-always-admitted and
	// markFailed/release deadlock-avoidance guarantees.
	bufLimit := catchUpBufferBytes
	if d.catchUpBufferBytesOverride > 0 {
		bufLimit = d.catchUpBufferBytesOverride
	}
	rb := newReorderBuffer(bufLimit, curSeq)
	// The live target the rolling window chases: how far catch-up may fetch
	// right now, re-read on every claim. Workers harvest X-Head-Seqnum from
	// every segment response (noteHeadSeqFromResponse), so d.headSeq — and
	// with it this target — keeps advancing while catch-up runs, and one
	// call can drain an arbitrarily-far-behind gap without ever draining the
	// pool at a batch boundary. Floored at initialTarget (headSeq is
	// monotonic within a downloader, so the target never shrinks below what
	// was committed at entry) and always clamped to EndSeq.
	target := func() int {
		t := int(d.headSeq.Load()) - stayBehindSegments
		t = max(t, initialTarget)
		if d.opts.EndSeq >= 0 {
			t = min(t, d.opts.EndSeq+1)
		}
		return t
	}
	// The window width is catchUpBatchLimit read LIVE: the post-failure
	// damping floor (one full parallel wave) and its one-segment-per-
	// interval regrowth apply to the rolling window exactly as they did to
	// the per-call batch — a call entered damped widens as it goes.
	rb.beginClaims(curSeq, target, d.catchUpBatchLimit)
	// done is closed when this function returns so workers blocked on
	// `results <-` can unblock and exit. Without it, an early consumer
	// return (e.g. write error below) would leave workers wedged on a
	// full results channel — wg.Wait then blocks forever and the closer
	// goroutine never closes results, leaking the entire pool.
	done := make(chan struct{})
	defer close(done)
	// A worker can also be blocked inside rb.admit() waiting for buffer
	// room rather than on `results <-`. release() wakes those too, so wire
	// it to the same teardown signal. This is the ONE place release() (the
	// blanket bypass) is correct rather than markFailed: the consumer
	// returning early has nothing to do with any specific seq's
	// availability, so there's no seq to hand markFailed.
	go func() {
		defer func() {
			if r := recover(); r != nil && d.logger != nil {
				d.logger.Error("catch-up buffer-release watcher panic", "panic", r)
			}
		}()
		<-done
		rb.release()
	}()
	var wg sync.WaitGroup

	// Spawn fixed worker pool. Workers pull claims from rb's rolling window
	// until claiming ends (target reached, failure recorded, or teardown) —
	// there is no feeder goroutine and no batch to drain between.
	for range workers {
		wg.Go(func() {
			// A claimed sequence MUST resolve — flush or markFailed — or the
			// flush position stalls there forever and with it the rolling
			// window, wedging every other worker. claimed tracks the one
			// in-flight claim so the recover below can resolve it even on a
			// panic; -1 means nothing is pending.
			claimed := -1
			defer func() {
				if r := recover(); r != nil {
					if claimed >= 0 {
						rb.markFailed(claimed)
					}
					if d.logger != nil {
						d.logger.Error("catch-up parallel download worker panic", "panic", r)
					}
				}
			}()
			for {
				seq, ok := rb.claim()
				if !ok {
					return
				}
				claimed = seq
				if d.isCancelled() || ctx.Err() != nil {
					// This exact seq will never be fetched (each claim is
					// owned by exactly one worker) — mark it failed so any
					// OTHER worker already blocked in admit() for a higher
					// seq drops out instead of waiting forever for room a
					// cancelled download will never free, and so claim()
					// starts returning false pool-wide. This is the only
					// path that abandons a claim without ever calling
					// fetchSegmentWithRetry, so the failure-path markFailed
					// below never runs for it.
					rb.markFailed(seq)
					claimed = -1
					continue // next claim() observes the failure and exits
				}
				segURL := d.buildSegmentURL(seq)
				// Rebuild from the CURRENT base on a mid-retry credential
				// refresh (Task 3 follow-up) — a sig/n-param rotation only
				// helps once the retry actually re-derives the URL from the
				// refreshed base, not just the refreshed PO token.
				data, fetchErr := d.fetchSegmentWithRetry(ctx, segURL, func() string { return d.buildSegmentURL(seq) })
				if fetchErr == nil {
					// Blocks here (not on `results <-`) once rb is at its
					// byte ceiling, unless seq is the current head — see
					// reorderBuffer.admit. A false return means the data
					// was dropped (provably unflushable, or the call is
					// tearing down) — nothing to notify the consumer about.
					// Either way the claim is resolved: stored data flushes
					// or is discarded at return, and a drop implies a
					// recorded failure/release already stalls the flush
					// below this seq.
					stored := rb.admit(seq, data)
					claimed = -1
					if stored {
						select {
						case results <- seq:
						case <-done:
							return
						}
					}
					continue
				}
				// Any fetch failure — permanent, retries exhausted, or a
				// cancelled download — means this exact seq will not arrive
				// in this call (each claim is owned by exactly one worker).
				// Record it as the (possibly new, lower) point beyond which
				// nothing can ever flush, so admit() drops later segments
				// instead of buffering data that's guaranteed to be
				// discarded at return, wakes any worker already blocked
				// waiting for room past this point, and ends claiming.
				// Deliberately NOT a blanket release(): a 403 episode is
				// exactly the head-stall condition that fills the buffer,
				// so disabling the ceiling for every seq on any single
				// failure would reopen the full pre-ceiling exposure this
				// type exists to close.
				rb.markFailed(seq)
				claimed = -1
				// Audit reports/engine.md #17: surface the failure mode so the
				// gap-detection downstream isn't blind to "transient" vs "gone".
				switch {
				case errors.Is(fetchErr, ErrSegmentPermanent):
					d.noteCatchUpFailureEpisode()
					d.logger.Debug("[Downloader] catch-up segment permanently gone (403/410)",
						"seq", seq)
				case errors.Is(fetchErr, ErrSegmentRetriesExhausted):
					d.noteCatchUpFailureEpisode()
					d.logger.Debug("[Downloader] catch-up segment retries exhausted",
						"seq", seq)
				}
			}
		})
	}

	// Close results when all workers complete
	go func() {
		defer func() {
			if r := recover(); r != nil && d.logger != nil {
				d.logger.Error("panic in parallel results closer", "panic", r)
			}
		}()
		wg.Wait()
		close(results)
	}()

	// Stream write: drain rb's out-of-order segments, write in order as they arrive
	nextSeq := curSeq
	segsSinceResume := 0
	for range results {
		// Flush consecutive segments from rb to disk
		for {
			data, ok := rb.take(nextSeq)
			if !ok {
				break
			}

			n, err := d.outputFile.Write(data)
			if err != nil {
				return nextSeq, fmt.Errorf("write segment %d: %w", nextSeq, err)
			}
			d.bytesWritten.Add(int64(n))
			d.lastSegTime.StoreNow()

			if d.OnProgress != nil {
				d.OnProgress(DownloadProgress{
					Seq:        nextSeq,
					Bytes:      d.bytesWritten.Load(),
					HeadSeq:    int(d.headSeq.Load()),
					CatchingUp: true,
				})
			}

			nextSeq++
			// Always keep currentSeq in sync so a crash mid-catch-up loses at
			// most the one segment currently being written rather than up to
			// ResumeCatchupInterval. saveResume is still periodic since it
			// touches disk. (Audit reports/engine.md Finding 1.)
			d.currentSeq.Store(int64(nextSeq))
			// Advances which seq is exempt from rb's byte ceiling and wakes
			// any worker blocked in rb.admit() waiting for room.
			rb.setHead(nextSeq)
			segsSinceResume++
			if segsSinceResume >= ResumeCatchupInterval {
				d.saveResume()
				segsSinceResume = 0
			}
		}
	}

	// Final progress emission -- CatchingUp: false signals catch-up is complete
	if d.OnProgress != nil {
		d.OnProgress(DownloadProgress{
			Seq:        nextSeq - 1,
			Bytes:      d.bytesWritten.Load(),
			HeadSeq:    int(d.headSeq.Load()),
			CatchingUp: false,
		})
	}

	// Stop at the first gap instead of writing past it. DASH fMP4 segments
	// are not self-contained — writing seg N then skipping N+1..N+k then
	// writing N+k+1 produces a file with a hole in the middle that will
	// confuse the muxer. By returning nextSeq pointing at the gap, the DASH
	// main loop will re-fetch the missing segments sequentially with the
	// full retry logic in handleDashError rather than the one-shot
	// fetchSegmentWithRetry used by catch-up workers. We still report the
	// gap range so callers can track what's about to be retried.
	// (Audit reports/engine.md Finding 4.)
	//
	// The bound is what was actually CLAIMED, not the live target: claims
	// are contiguous, so every sequence below claimedUpTo was attempted and
	// an unflushed one is a real hole — while sequences at/above it were
	// never handed to a worker and are simply future work. Every claim
	// flushed means nextSeq == claimedUpTo and no gap fires.
	claimedUpTo := rb.claimedUpTo()
	if nextSeq < claimedUpTo {
		// Find how far the gap extends so the gap event covers the whole
		// contiguous missing range up to the next buffered segment (or the
		// end of the claimed span if the tail is all missing).
		gapEnd := nextSeq
		for gapEnd < claimedUpTo {
			if rb.has(gapEnd) {
				break
			}
			gapEnd++
		}
		if d.OnGap != nil {
			d.OnGap(DownloadGap{From: nextSeq, To: gapEnd - 1})
		}
		d.logger.Info("[Downloader] Parallel catch-up stopping at gap; main loop will re-fetch",
			"gapFrom", nextSeq, "gapTo", gapEnd-1)
	}

	return nextSeq, nil
}

// noteCatchUpFailureEpisode records that catch-up just hit failures, which
// narrows the rolling claim window via catchUpBatchLimit.
func (d *SegmentDownloader) noteCatchUpFailureEpisode() {
	d.lastCatchUpFailure.StoreNow()
}

// catchUpBatchLimit is the width of parallel catch-up's rolling claim
// window — how far fetches may run ahead of the flush position (see
// reorderBuffer.claim; before the rolling window this same value bounded
// the size of one whole catch-up batch): the full maxCatchupBatch()
// normally, and after a failure episode a value that starts at the damped
// floor — one full parallel wave, i.e. d.segmentWorkers() — and regrows by
// one segment per catchUpRegrowInterval. Read live on every claim, so a
// catch-up call entered damped widens as it runs.
//
// The floor and interval were retuned on 2026-08-15 after field evidence. The
// first version collapsed to a single segment and regrew one per 10s, copying
// moonarchive's constants — but moonarchive damps against its HEARTBEAT clock,
// while this resets on every failed segment. With real 403 episodes arriving
// every ~15-30s the ceiling never climbed out of 1-3, and observed catch-up
// batches ran 0-2 segments wide against the 48 they had before, i.e. roughly
// 20-40x slower on exactly the mid-stream-join path this subsystem exists to
// serve.
//
// A hard damp is also no longer what stops a 403 storm: refresh-and-retry is
// (403 volume fell from ~1100 in 18s to ~12 per MINUTE once it landed). So the
// floor is one full parallel wave — narrower than that just serialises the
// worker pool without reducing pressure meaningfully — and the ceiling
// recovers in ~42s instead of ~8 minutes (at the ParallelDownloads=6 default;
// a wider configured pool takes proportionally longer to regrow its wider
// ceiling, at the same one-segment-per-second rate).
func (d *SegmentDownloader) catchUpBatchLimit() int {
	full := d.maxCatchupBatch()
	floor := d.segmentWorkers()
	since := d.lastCatchUpFailure.Since()
	if since >= time.Duration(full-floor)*catchUpRegrowInterval {
		return full
	}
	limit := floor + int(since/catchUpRegrowInterval)
	return min(limit, full)
}
