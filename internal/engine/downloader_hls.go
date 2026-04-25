package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// runHlsLoop is the main HLS download loop.
func (d *SegmentDownloader) runHlsLoop(ctx context.Context) error {
	// Save resume state on exit so interrupted downloads can continue on restart.
	// Only clear the resume file when the stream ends naturally.
	defer func() {
		d.saveResume()
		if d.streamEnded.Load() {
			d.ClearResume()
		}
	}()

	staleCount := 0
	consecutiveErrors := 0
	// Track retries for a single segment sequence so we don't spin forever
	// on a segment the CDN is returning 5xx for. When we hit the cap, we
	// escalate to the stream-status check path so the loop can exit if the
	// stream actually ended, or advance past the segment if not.
	stuckSeq := int64(-1)
	stuckSeqRetries := 0

	for {
		if d.isCancelled() || ctx.Err() != nil {
			return d.cancelErr(ctx)
		}

		// Fetch playlist
		data, plStatus, err := d.fetchSegment(ctx, d.opts.BaseURL)
		if err != nil {
			// 404/410 on playlist fetch -- variant may have been removed
			if plStatus == 404 || plStatus == 410 {
				if d.opts.IsOnline != nil && !d.opts.IsOnline() {
					d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
					if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
						return err
					}
					consecutiveErrors = 0
					continue
				}
				if d.opts.CheckStreamStatus != nil {
					ended, checkErr := d.opts.CheckStreamStatus(ctx)
					if checkErr != nil {
						d.logger.Warn("stream status check failed, assuming ended", "err", checkErr)
					} else if !ended {
						return ErrQualityLost
					}
				}
				d.streamEnded.Store(true)
				return nil
			}
			consecutiveErrors++
			if consecutiveErrors > 5 {
				if d.opts.IsOnline != nil && !d.opts.IsOnline() {
					d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
					if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
						return err
					}
					consecutiveErrors = 0
					continue
				}
				// Before giving up, check if stream is still live (quality may have changed)
				if d.opts.CheckStreamStatus != nil {
					ended, checkErr := d.opts.CheckStreamStatus(ctx)
					if checkErr != nil {
						d.logger.Warn("stream status check failed, assuming ended", "err", checkErr)
					} else if !ended {
						return ErrQualityLost
					}
				}
				return fmt.Errorf("HLS playlist fetch failed after %d consecutive errors: %w", consecutiveErrors, err)
			}
			utils.Sleep(ctx, 5*time.Second)
			continue
		}

		result := ParseHls(string(data), d.opts.BaseURL)
		if result == nil || result.Playlist == nil {
			consecutiveErrors++
			// Log a snippet of the response so "parse failure" surfaces the
			// actual content — often CDNs return HTML error pages with
			// 200 OK that ParseHls silently rejects. First 200 bytes is
			// enough to tell m3u8 from HTML/JSON/text.
			snippet := string(data)
			if len(snippet) > 200 {
				snippet = snippet[:200] + "..."
			}
			if strings.HasPrefix(strings.TrimSpace(snippet), "<") {
				d.logger.Warn("[Downloader] HLS playlist returned HTML instead of m3u8",
					"snippet", snippet, "attempt", consecutiveErrors)
			} else {
				d.logger.Debug("[Downloader] HLS playlist parse failed",
					"snippet", snippet, "attempt", consecutiveErrors)
			}
			if consecutiveErrors > 5 {
				return fmt.Errorf("failed to parse HLS playlist after %d consecutive errors", consecutiveErrors)
			}
			utils.Sleep(ctx, 5*time.Second)
			continue
		}
		pl := result.Playlist

		// Initialize currentSeq if needed
		curSeq := int(d.currentSeq.Load())
		if curSeq < 0 {
			d.currentSeq.Store(int64(pl.MediaSequence))
			curSeq = pl.MediaSequence
		}

		// Handle gap: if currentSeq < mediaSequence, segments expired from CDN
		if curSeq < pl.MediaSequence {
			if d.OnGap != nil {
				d.OnGap(DownloadGap{From: curSeq, To: pl.MediaSequence - 1})
			}
			d.currentSeq.Store(int64(pl.MediaSequence))
			curSeq = pl.MediaSequence
		}

		// Identify new segments (only those >= currentSeq)
		var newSegments []HlsSegment
		for i, seg := range pl.Segments {
			segSeq := pl.MediaSequence + i
			if segSeq >= curSeq {
				newSegments = append(newSegments, seg)
			}
		}

		// VOD: parallel download only the filtered segments (not already downloaded)
		if pl.EndList && len(newSegments) > 0 {
			filteredPl := &HlsPlaylist{
				Segments:       newSegments,
				MediaSequence:  curSeq,
				EndList:        pl.EndList,
				TargetDuration: pl.TargetDuration,
			}
			return d.runHlsVodParallel(ctx, filteredPl)
		}

		// Live: download available segments
		segFailed := false
		for _, seg := range newSegments {
			if d.isCancelled() || ctx.Err() != nil {
				return d.cancelErr(ctx)
			}

			segData, _, segErr := d.fetchSegment(ctx, seg.URL)
			if segErr != nil {
				// Track repeated failures of the same sequence so we can
				// escalate when a permanently-unavailable segment is
				// stuck in the playlist.
				curSeqNow := d.currentSeq.Load()
				if curSeqNow == stuckSeq {
					stuckSeqRetries++
				} else {
					stuckSeq = curSeqNow
					stuckSeqRetries = 1
				}
				// Don't skip -- break to re-fetch playlist and retry.
				// If CDN purged it, gap detection handles it next iteration.
				d.logger.Debug("[Downloader] HLS segment failed, will retry after playlist refresh",
					"seq", curSeqNow, "error", segErr, "stuckRetries", stuckSeqRetries)
				if stuckSeqRetries >= MaxSegmentRetries {
					// Escalate: is the stream actually still live? If so,
					// advance past this segment (treat as gap) so we don't
					// hang forever. Mirrors DASH's stuckOnSegment path.
					ended := false
					if d.opts.CheckStreamStatus != nil {
						var checkErr error
						ended, checkErr = d.opts.CheckStreamStatus(ctx)
						if checkErr != nil {
							d.logger.Warn("[Downloader] HLS stream status check failed while stuck",
								"err", checkErr)
						}
					}
					if ended {
						d.streamEnded.Store(true)
						return nil
					}
					d.logger.Warn("[Downloader] HLS segment stuck, advancing past it as gap",
						"seq", curSeqNow, "retries", stuckSeqRetries)
					if d.OnGap != nil {
						d.OnGap(DownloadGap{From: int(curSeqNow), To: int(curSeqNow)})
					}
					d.currentSeq.Add(1)
					stuckSeq = -1
					stuckSeqRetries = 0
				}
				utils.Sleep(ctx, 2*time.Second)
				segFailed = true
				break
			}
			// Success — reset stuck tracking.
			stuckSeq = -1
			stuckSeqRetries = 0
			hlsSeq := int(d.currentSeq.Load())
			n, writeErr := d.outputFile.Write(segData)
			if writeErr != nil {
				return fmt.Errorf("write HLS segment %d: %w", hlsSeq, writeErr)
			}
			d.bytesWritten.Add(int64(n))
			d.currentSeq.Add(1)
			d.lastSegTime.StoreNow()

			if d.OnProgress != nil {
				d.OnProgress(DownloadProgress{
					Seq:   int(d.currentSeq.Load()),
					Bytes: d.bytesWritten.Load(),
				})
			}
		}

		if segFailed {
			continue // Retry from playlist refresh
		}

		// Stale detection: no new segments available
		if len(newSegments) == 0 {
			staleCount++
			if staleCount >= 5 {
				if d.opts.IsOnline != nil && !d.opts.IsOnline() {
					d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
					if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
						return err
					}
					staleCount = 0
					continue
				}
				if d.opts.CheckStreamStatus != nil {
					ended, _ := d.opts.CheckStreamStatus(ctx)
					if ended {
						d.streamEnded.Store(true)
						return nil
					}
				}
			}
		} else {
			staleCount = 0
		}

		// Reset consecutive errors on successful iteration
		consecutiveErrors = 0

		d.saveResume()

		// Check if stream ended (EXT-X-ENDLIST present)
		if pl.EndList {
			return nil
		}

		// Wait before next refresh
		targetDur := pl.TargetDuration
		if targetDur <= 0 {
			targetDur = 2.0
		}
		utils.Sleep(ctx, time.Duration(targetDur*float64(time.Second)))
	}
}

// runHlsVodParallel downloads all VOD HLS segments in parallel with bounded concurrency.
// Uses a worker pool pattern: ParallelDownloads workers pull from a work channel,
// avoiding N goroutines sitting in memory for large VODs.
func (d *SegmentDownloader) runHlsVodParallel(ctx context.Context, pl *HlsPlaylist) error {
	totalSegs := len(pl.Segments)
	if totalSegs == 0 {
		return nil
	}

	type segWork struct {
		idx    int
		segURL string
	}
	type segResult struct {
		idx  int
		data []byte
	}

	work := make(chan segWork, ParallelDownloads)
	results := make(chan segResult, ParallelDownloads*3)
	// done unblocks workers stuck on a full results channel when the
	// consumer below returns early on write error; without it wg.Wait
	// would never complete and the entire worker pool would leak.
	done := make(chan struct{})
	defer close(done)
	var wg sync.WaitGroup

	// Spawn fixed worker pool
	for range ParallelDownloads {
		wg.Go(func() {
			defer func() {
				if r := recover(); r != nil && d.logger != nil {
					d.logger.Error("VOD parallel download worker panic", "panic", r)
				}
			}()
			for item := range work {
				if d.isCancelled() || ctx.Err() != nil {
					continue // drain channel
				}
				data, permanent := d.fetchSegmentWithRetry(ctx, item.segURL)
				if data != nil {
					select {
					case results <- segResult{idx: item.idx, data: data}:
					case <-done:
						return
					}
					continue
				}
				// Audit reports/engine.md #17: distinguish CDN-evicted segments
				// from retries-exhausted so debugging silent gaps is tractable.
				if permanent {
					d.logger.Debug("[Downloader] HLS VOD segment permanently gone (403/410)",
						"idx", item.idx)
				} else {
					d.logger.Debug("[Downloader] HLS VOD segment retries exhausted",
						"idx", item.idx)
				}
			}
		})
	}

	// Feed work to workers
	go func() {
		defer func() {
			if r := recover(); r != nil && d.logger != nil {
				d.logger.Error("VOD feeder goroutine panic", "panic", r)
			}
		}()
		for i, seg := range pl.Segments {
			if d.isCancelled() || ctx.Err() != nil {
				break
			}
			work <- segWork{idx: i, segURL: seg.URL}
		}
		close(work)
	}()

	// Close results when all workers done
	go func() {
		defer func() {
			if r := recover(); r != nil && d.logger != nil {
				d.logger.Error("panic in HLS VOD results closer", "panic", r)
			}
		}()
		wg.Wait()
		close(results)
	}()

	// Stream write: buffer out-of-order segments, write in order as they arrive.
	// Buffer size is bounded by the number of in-flight workers (ParallelDownloads);
	// segments are flushed in order so the map typically holds only a few entries.
	buffer := make(map[int][]byte)
	nextIdx := 0

	for r := range results {
		buffer[r.idx] = r.data

		// Flush consecutive segments from buffer to disk
		for {
			data, ok := buffer[nextIdx]
			if !ok {
				break
			}
			delete(buffer, nextIdx) // Free memory immediately

			n, err := d.outputFile.Write(data)
			if err != nil {
				return fmt.Errorf("write HLS VOD segment %d: %w", nextIdx, err)
			}
			d.bytesWritten.Add(int64(n))
			d.currentSeq.Add(1)
			nextIdx++

			if d.OnProgress != nil {
				d.OnProgress(DownloadProgress{
					Seq:   int(d.currentSeq.Load()),
					Bytes: d.bytesWritten.Load(),
					Total: totalSegs,
				})
			}

			// Save resume state every ResumeCatchupInterval (50) segments —
			// frequent enough that a crash loses at most that many segments,
			// rare enough that the disk-touching saveResume isn't on the
			// hot path for every download.
			if d.currentSeq.Load()%50 == 0 {
				d.saveResume()
			}
		}
	}

	// Flush remaining buffered segments + detect gaps
	gapStart := -1
	for nextIdx < totalSegs {
		if data, ok := buffer[nextIdx]; ok {
			// Close any open gap
			if gapStart >= 0 && d.OnGap != nil {
				d.OnGap(DownloadGap{From: gapStart, To: nextIdx - 1})
				gapStart = -1
			}
			n, writeErr := d.outputFile.Write(data)
			if writeErr != nil {
				return fmt.Errorf("write error during HLS VOD gap flush (segment %d): %w", nextIdx, writeErr)
			}
			d.bytesWritten.Add(int64(n))
			d.currentSeq.Add(1)
			delete(buffer, nextIdx)
		} else {
			if gapStart < 0 {
				gapStart = nextIdx
			}
		}
		nextIdx++
	}
	// Close final gap
	if gapStart >= 0 && d.OnGap != nil {
		d.OnGap(DownloadGap{From: gapStart, To: totalSegs - 1})
	}

	d.saveResume()
	return nil
}
