package engine

import (
	"context"
	"fmt"
	"sync"
)

// runParallelCatchUp downloads segments in parallel to catch up to the live edge.
// Stays stayBehindSegments behind the live edge to avoid downloading in-flight segments.
func (d *SegmentDownloader) runParallelCatchUp(ctx context.Context) (int, error) {
	curSeq := int(d.currentSeq.Load())
	head := int(d.headSeq.Load())
	targetSeq := head - stayBehindSegments
	targetSeq = max(targetSeq, curSeq+1) // At least catch up 1 segment
	// Respect endSeq limit (for timestamp-based trimming)
	if d.opts.EndSeq >= 0 && targetSeq > d.opts.EndSeq+1 {
		targetSeq = d.opts.EndSeq + 1
	}
	if targetSeq <= curSeq {
		return curSeq, nil
	}

	if d.OnProgress != nil {
		d.OnProgress(DownloadProgress{
			Seq:        curSeq,
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

	bufferCap := ParallelDownloads * 3
	work := make(chan segWork, ParallelDownloads)
	results := make(chan segResult, bufferCap)
	var wg sync.WaitGroup

	// Spawn fixed worker pool
	for range ParallelDownloads {
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
				segURL := d.buildSegmentURL(item.seq)
				if data := d.fetchSegmentWithRetry(ctx, segURL); data != nil {
					results <- segResult{seq: item.seq, data: data}
				}
			}
		})
	}

	// Feed work to workers
	go func() {
		defer func() {
			if r := recover(); r != nil && d.logger != nil {
				d.logger.Error("catch-up feeder goroutine panic", "panic", r)
			}
		}()
		for seq := curSeq; seq <= targetSeq; seq++ {
			if d.isCancelled() || ctx.Err() != nil {
				break
			}
			work <- segWork{seq: seq}
		}
		close(work)
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

	// Handle any remaining gaps (use range-based gap detection)
	catchupGapStart := -1
	for nextSeq <= targetSeq {
		if _, ok := buffer[nextSeq]; ok {
			// Close any open gap
			if catchupGapStart >= 0 && d.OnGap != nil {
				d.OnGap(DownloadGap{From: catchupGapStart, To: nextSeq - 1})
				catchupGapStart = -1
			}
			n, writeErr := d.outputFile.Write(buffer[nextSeq])
			if writeErr != nil {
				return 0, fmt.Errorf("write error during catch-up gap flush (segment %d): %w", nextSeq, writeErr)
			}
			d.bytesWritten.Add(int64(n))
			delete(buffer, nextSeq)
		} else {
			if catchupGapStart < 0 {
				catchupGapStart = nextSeq
			}
		}
		nextSeq++
	}
	// Close final gap range
	if catchupGapStart >= 0 && d.OnGap != nil {
		d.OnGap(DownloadGap{From: catchupGapStart, To: nextSeq - 1})
	}

	return nextSeq, nil
}
