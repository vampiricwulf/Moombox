package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// runParallelCatchUp downloads segments in parallel to catch up to the live edge.
// Stays stayBehindSegments behind the live edge to avoid downloading in-flight segments.
func (d *SegmentDownloader) runParallelCatchUp(ctx context.Context) (int, error) {
	curSeq := int(d.currentSeq.Load())
	head := int(d.headSeq.Load())
	// targetSeq is EXCLUSIVE: the catch-up downloads seqs [curSeq, targetSeq).
	targetSeq := head - stayBehindSegments
	targetSeq = max(targetSeq, curSeq+1) // At least catch up 1 segment
	// Bound the batch so the reorder buffer below can't grow to the whole
	// gap on a far-behind resume (see maxCatchupBatch). runDashLoop loops
	// back into catch-up to drain a larger gap in bounded chunks.
	targetSeq = min(targetSeq, curSeq+d.catchUpBatchLimit())
	// Respect endSeq limit (for timestamp-based trimming)
	if d.opts.EndSeq >= 0 && targetSeq > d.opts.EndSeq+1 {
		targetSeq = d.opts.EndSeq + 1
	}
	if targetSeq <= curSeq {
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

	type segWork struct {
		seq int
	}
	type segResult struct {
		seq  int
		data []byte
	}

	// workers is the operative concurrency for this download (see
	// segmentWorkers) — computed once so the channel sizing and worker-pool
	// spawn below agree with each other and with catchUpBatchLimit's ceiling.
	workers := d.segmentWorkers()
	bufferCap := workers * 3
	work := make(chan segWork, workers)
	results := make(chan segResult, bufferCap)
	// done is closed when this function returns so workers blocked on
	// `results <-` can unblock and exit. Without it, an early consumer
	// return (e.g. write error below) would leave workers wedged on a
	// full results channel — wg.Wait then blocks forever and the closer
	// goroutine never closes results, leaking the entire pool.
	done := make(chan struct{})
	defer close(done)
	var wg sync.WaitGroup

	// Spawn fixed worker pool
	for range workers {
		wg.Go(func() {
			defer func() {
				if r := recover(); r != nil && d.logger != nil {
					d.logger.Error("catch-up parallel download worker panic", "panic", r)
				}
			}()
			for item := range work {
				if d.isCancelled() || ctx.Err() != nil {
					continue // drain channel
				}
				seq := item.seq
				segURL := d.buildSegmentURL(seq)
				// Rebuild from the CURRENT base on a mid-retry credential
				// refresh (Task 3 follow-up) — a sig/n-param rotation only
				// helps once the retry actually re-derives the URL from the
				// refreshed base, not just the refreshed PO token.
				data, fetchErr := d.fetchSegmentWithRetry(ctx, segURL, func() string { return d.buildSegmentURL(seq) })
				if fetchErr == nil {
					select {
					case results <- segResult{seq: item.seq, data: data}:
					case <-done:
						return
					}
					continue
				}
				// Audit reports/engine.md #17: surface the failure mode so the
				// gap-detection downstream isn't blind to "transient" vs "gone".
				switch {
				case errors.Is(fetchErr, ErrSegmentPermanent):
					d.noteCatchUpFailureEpisode()
					d.logger.Debug("[Downloader] catch-up segment permanently gone (403/410)",
						"seq", item.seq)
				case errors.Is(fetchErr, ErrSegmentRetriesExhausted):
					d.noteCatchUpFailureEpisode()
					d.logger.Debug("[Downloader] catch-up segment retries exhausted",
						"seq", item.seq)
				}
			}
		})
	}

	// Feed work to workers. The send must select on done: when the consumer
	// below returns early (write error), the workers drain away and nothing
	// reads work anymore — a bare send would block this goroutine forever.
	go func() {
		defer func() {
			if r := recover(); r != nil && d.logger != nil {
				d.logger.Error("catch-up feeder goroutine panic", "panic", r)
			}
		}()
		defer close(work)
		for seq := curSeq; seq < targetSeq; seq++ {
			if d.isCancelled() || ctx.Err() != nil {
				return
			}
			select {
			case work <- segWork{seq: seq}:
			case <-done:
				return
			}
		}
	}()

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

	// Stream write: buffer out-of-order segments, write in order as they arrive
	buffer := make(map[int][]byte)
	nextSeq := curSeq
	segsSinceResume := 0
	for r := range results {
		buffer[r.seq] = r.data

		// Flush consecutive segments from buffer to disk
		for {
			data, ok := buffer[nextSeq]
			if !ok {
				break
			}
			delete(buffer, nextSeq) // Free memory immediately after write

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
	if nextSeq < targetSeq {
		// Find how far the gap extends so the gap event covers the whole
		// contiguous missing range up to the next buffered segment (or the
		// end of the catch-up target if the tail is all missing).
		gapEnd := nextSeq
		for gapEnd < targetSeq {
			if _, ok := buffer[gapEnd]; ok {
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
// throttles the next batches via catchUpBatchLimit.
func (d *SegmentDownloader) noteCatchUpFailureEpisode() {
	d.lastCatchUpFailure.StoreNow()
}

// catchUpBatchLimit is the per-call segment ceiling for parallel catch-up:
// the full maxCatchupBatch() normally, and after a failure episode a value
// that starts at the damped floor — one full parallel wave, i.e.
// d.segmentWorkers() — and regrows by one segment per catchUpRegrowInterval.
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
