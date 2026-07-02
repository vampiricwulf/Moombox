package engine

import (
	"context"
	"errors"
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
	// lastSavedSeq tracks the currentSeq at the last resume-state save so the
	// per-iteration save can skip no-progress refreshes (see below). -1 forces
	// the first save.
	lastSavedSeq := -1
	// consecutiveStuckSkips bounds termination when EVERY segment fails
	// (expired auth, dead variant): each skip costs ~12s of retries, and an
	// ended stream with a long listed backlog would otherwise grind through
	// it skip by skip before EXT-X-ENDLIST/stale handling could fire.
	consecutiveStuckSkips := 0

	for {
		if d.isCancelled() || ctx.Err() != nil {
			return d.cancelErr(ctx)
		}

		// Fetch playlist
		data, plStatus, err := d.fetchSegment(ctx, d.getBaseURL())
		if err != nil {
			// 404/410 on playlist fetch -- variant may have been removed
			if plStatus == 404 || plStatus == 410 {
				if d.opts.IsOnline != nil && !d.opts.IsOnline() {
					d.emitActivity(ActivityReconnecting)
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
					d.emitActivity(ActivityReconnecting)
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
			d.emitActivity(ActivityRetrying)
			utils.Sleep(ctx, 5*time.Second)
			continue
		}

		result := ParseHls(string(data), d.getBaseURL())
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
				// Mirror the fetch-error escalation above: garbage playlists
				// during an outage (router/captive-portal error pages served
				// with 200) must wait for connectivity, and a live stream
				// whose CDN serves transient garbage should re-resolve the
				// variant rather than hard-fail the recording.
				if d.opts.IsOnline != nil && !d.opts.IsOnline() {
					d.emitActivity(ActivityReconnecting)
					d.logger.Warn("playlist parse failures while device offline, waiting for connectivity")
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
				return fmt.Errorf("failed to parse HLS playlist after %d consecutive errors", consecutiveErrors)
			}
			d.emitActivity(ActivityRetrying)
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
			if d.opts.StopOnGap && d.bytesWritten.Load() > 0 {
				// The file on disk ends exactly where data stopped being
				// available — return without advancing currentSeq so the
				// caller can mux it as a gapless part and seed a fresh
				// downloader from CurrentSeq. Covers both mid-session CDN
				// gaps and the resume-after-restart case (resume state put
				// currentSeq at the old position; the first playlist fetch
				// reveals whether the window still covers it). OnGap is
				// deliberately NOT fired here: the successor downloader —
				// seeded at this position with an empty file — takes the
				// skip-forward branch below on its first fetch and records
				// the gap exactly once.
				d.logger.Warn("[Downloader] Unrecoverable gap in live playlist, stopping for part split",
					"from", curSeq, "to", pl.MediaSequence-1,
					"missing", pl.MediaSequence-curSeq)
				return ErrGapDetected
			}
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

		// VOD: parallel download only the filtered segments (not already downloaded).
		// StopOnGap recordings take the sequential path below even for the
		// final ENDLIST playlist: the VOD-parallel downloader skips
		// permanently-failed segments and keeps writing, which would bury a
		// discontinuity inside the final part — the sequential path's
		// stuck-segment escalation preserves the gapless-part contract. The
		// tail is at most one playlist window, so the lost parallelism is
		// seconds.
		if pl.EndList && len(newSegments) > 0 && !d.opts.StopOnGap {
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

			segData, segStatus, segErr := d.fetchSegment(ctx, seg.URL)
			if segErr != nil {
				// HLS 403s commonly indicate cipher rotation or POT expiry.
				// Fire OnCipherFailure once per downloader so the strategy can
				// invalidate cipher / POT / visitor-data caches; the next
				// orchestrator-driven refresh (quality monitor, stream-status
				// escalation) will then rebuild the variant URL with fresh
				// values. Mirrors the DASH path; HLS variant URL has POT in
				// its path so we can't hot-swap mid-loop, hence the empty
				// return is treated as a no-op here.
				if segStatus == 403 && d.cipherFailureFired.CompareAndSwap(false, true) &&
					d.OnCipherFailure != nil {
					if newURL := d.OnCipherFailure(); newURL != "" {
						d.SetBaseURL(newURL)
						d.logger.Info("[Cipher] HLS swapping BaseURL after 403",
							"url_prefix", truncateURL(newURL, 120))
					}
				}
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
					if d.opts.StopOnGap && d.bytesWritten.Load() > 0 {
						// Same contract as the playlist-window gap above: the
						// skipped segment would leave a discontinuity inside
						// the file, so end this part instead. currentSeq is
						// deliberately NOT advanced and OnGap NOT fired: the
						// resume sidecar must stay honest (LastSeq = last
						// byte actually in the file — advancing here let a
						// crash-resume append past a hole), and the SUCCESSOR
						// downloader — seeded at this position with an empty
						// file — retries the stuck segment once more and, if
						// it still fails, skips it via the empty-file rule,
						// recording the gap exactly once.
						d.logger.Warn("[Downloader] HLS segment permanently failing, stopping for part split",
							"seq", curSeqNow, "retries", stuckSeqRetries)
						return ErrGapDetected
					}
					// Skip past the stuck segment unconditionally — the
					// previous ended-check-first ordering abandoned the
					// still-listed tail when the stream happened to end
					// during the retries (the final window's segments stay
					// fetchable after the broadcast is over). With the skip,
					// an ended stream terminates a few iterations later via
					// EXT-X-ENDLIST or the stale-playlist escalation, both of
					// which fetch the remaining tail first.
					d.logger.Warn("[Downloader] HLS segment stuck, advancing past it as gap",
						"seq", curSeqNow, "retries", stuckSeqRetries)
					if d.OnGap != nil {
						d.OnGap(DownloadGap{From: int(curSeqNow), To: int(curSeqNow)})
					}
					d.currentSeq.Add(1)
					stuckSeq = -1
					stuckSeqRetries = 0
					consecutiveStuckSkips++
					// Several skips in a row without a single success means
					// nothing is fetchable — consult the stream status once
					// and exit cleanly if the broadcast is over, rather than
					// grinding through the remaining backlog. A localized
					// CDN hole resets the counter on the next good segment,
					// so a fetchable tail is still captured first.
					if consecutiveStuckSkips >= 3 && d.opts.CheckStreamStatus != nil {
						if ended, checkErr := d.opts.CheckStreamStatus(ctx); checkErr == nil && ended {
							d.logger.Warn("[Downloader] Stream ended and segments are unfetchable; stopping",
								"consecutiveSkips", consecutiveStuckSkips)
							d.streamEnded.Store(true)
							return nil
						}
					}
				}
				d.emitActivity(ActivityRetrying)
				utils.Sleep(ctx, 2*time.Second)
				segFailed = true
				break
			}
			// Success — reset stuck tracking.
			stuckSeq = -1
			stuckSeqRetries = 0
			consecutiveStuckSkips = 0
			hlsSeq := int(d.currentSeq.Load())
			n, writeErr := d.outputFile.Write(segData)
			if writeErr != nil {
				return fmt.Errorf("write HLS segment %d: %w", hlsSeq, writeErr)
			}
			d.bytesWritten.Add(int64(n))
			d.currentSeq.Add(1)
			d.lastSegTime.StoreNow()

			if d.OnProgress != nil {
				// Seq reports the just-WRITTEN sequence — the same convention
				// as the DASH downloaders — so the persisted last_video_seq
				// means one thing regardless of strategy. (It used to report
				// post-increment here, which made restart seeding guess the
				// writer's convention and skip a segment when it guessed
				// wrong across an HLS<->DASH strategy flip.)
				d.OnProgress(DownloadProgress{
					Seq:   hlsSeq,
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
					d.emitActivity(ActivityReconnecting)
					d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
					if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
						return err
					}
					staleCount = 0
					continue
				}
				d.emitActivity(ActivityVerifyingEnd)
				if d.opts.CheckStreamStatus != nil {
					ended, _ := d.opts.CheckStreamStatus(ctx)
					if ended {
						d.streamEnded.Store(true)
						return nil
					}
				}
				// Stream is live but our position sits FAR beyond the
				// playlist's entire window: the media-sequence numbering
				// restarted under us (e.g. a resumed job whose channel ended
				// one broadcast and started another while we were down). The
				// margin matters — a lagging CDN replica can legitimately
				// serve a window a few segments behind our edge position for
				// several refreshes, and ending the recording on that
				// transient would permanently lose the rest of the stream.
				// A genuine numbering restart overshoots by hundreds of
				// segments, not a handful. End cleanly so the recording
				// muxes; the monitor picks the new broadcast up as a fresh
				// job.
				const seqRegressionMargin = 60 // ~2 min of 2s segments
				if curSeq > pl.MediaSequence+len(pl.Segments)+seqRegressionMargin {
					d.logger.Warn("[Downloader] HLS media sequence regressed far below resume position, ending recording",
						"curSeq", curSeq, "playlistEnd", pl.MediaSequence+len(pl.Segments))
					d.streamEnded.Store(true)
					return nil
				}
				// Reset so the next status probe waits another full stale
				// window instead of firing on every playlist refresh (~2s)
				// for as long as the stall lasts.
				staleCount = 0
			}
		} else {
			staleCount = 0
		}

		// Reset consecutive errors on successful iteration
		consecutiveErrors = 0

		// Persist resume state only when the position actually advanced. The
		// loop iterates every ~TargetDuration (~2s for Twitch) and reaches
		// here even on no-progress refreshes (stale window, verifying-end);
		// an unconditional saveResume there fsync+renamed the sidecar every
		// ~2s for zero recovery benefit (only the timestamp changed, which
		// the loader ignores within the 7-day window). bytesWritten only
		// changes alongside currentSeq, so the seq is a complete progress
		// signal. The deferred saveResume still guarantees a final flush.
		if curSeqNow := int(d.currentSeq.Load()); curSeqNow != lastSavedSeq {
			d.saveResume()
			lastSavedSeq = curSeqNow
		}

		// Check if stream ended (EXT-X-ENDLIST present)
		if pl.EndList {
			// Natural end: mark ended so the deferred ClearResume removes the
			// sidecar (matches the DASH loop). Without this, streamEnded stays
			// false, the defer re-saves a fresh sidecar, and ClearResume is
			// skipped — leaving an orphaned .resume.json (masked today only
			// because the worker wipes staging, but a contract violation).
			d.streamEnded.Store(true)
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
				data, fetchErr := d.fetchSegmentWithRetry(ctx, item.segURL)
				if fetchErr != nil {
					// Audit reports/engine.md #17: distinguish CDN-evicted
					// segments from retries-exhausted so silent gaps are debuggable.
					switch {
					case errors.Is(fetchErr, ErrSegmentPermanent):
						d.logger.Debug("[Downloader] HLS VOD segment permanently gone (403/410)",
							"idx", item.idx)
					case errors.Is(fetchErr, ErrSegmentRetriesExhausted):
						d.logger.Debug("[Downloader] HLS VOD segment retries exhausted",
							"idx", item.idx)
					}
					// Emit a nil-data GAP SENTINEL rather than dropping the
					// segment silently. Without it, a failed segment at the
					// consumer's nextIdx never arrives, so nextIdx never
					// advances while the other workers race the rest of the
					// playlist into the reorder buffer — accumulating the
					// ENTIRE remaining VOD in RAM (a multi-GB OOM on a long VOD
					// with one early muted/deleted segment). The sentinel lets
					// the consumer flush past the hole and keeps the buffer
					// bounded to the in-flight window.
					data = nil
				}
				select {
				case results <- segResult{idx: item.idx, data: data}:
				case <-done:
					return
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
				d.logger.Error("VOD feeder goroutine panic", "panic", r)
			}
		}()
		defer close(work)
		for i, seg := range pl.Segments {
			if d.isCancelled() || ctx.Err() != nil {
				return
			}
			select {
			case work <- segWork{idx: i, segURL: seg.URL}:
			case <-done:
				return
			}
		}
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

	// Stream write: buffer out-of-order segments, write in order as they
	// arrive. Every index produces exactly one result (segment data or a
	// nil GAP SENTINEL from a failed worker), so the reorder buffer is
	// bounded by the in-flight window (~ParallelDownloads + results cap) —
	// a failed segment can no longer wedge nextIdx and accumulate the whole
	// VOD in RAM. gapStart coalesces consecutive missing indices into one
	// OnGap range and persists across the outer loop.
	buffer := make(map[int][]byte)
	nextIdx := 0
	gapStart := -1

	closeGap := func(endIdx int) {
		if gapStart >= 0 {
			if d.OnGap != nil {
				d.OnGap(DownloadGap{From: gapStart, To: endIdx})
			}
			gapStart = -1
		}
	}

	for r := range results {
		buffer[r.idx] = r.data

		// Flush consecutive entries (segments and gap sentinels) in order.
		for {
			data, ok := buffer[nextIdx]
			if !ok {
				break
			}
			delete(buffer, nextIdx) // Free memory immediately

			if data == nil {
				// Gap sentinel: skip without writing, coalescing runs of
				// missing segments. currentSeq MUST advance here (like the live
				// loop's gap-skip) so it stays in playlist-index space: the
				// resume filter in runHlsLoop is index-based
				// (segSeq = MediaSequence+i >= curSeq), so if currentSeq lagged
				// the index by the gap count, a resume would re-fetch and append
				// already-written segments — a duplicated/mis-ordered TS spliced
				// into the VOD.
				if gapStart < 0 {
					gapStart = nextIdx
				}
				d.currentSeq.Add(1)
				nextIdx++
				continue
			}
			closeGap(nextIdx - 1) // a real segment ends any open gap

			n, err := d.outputFile.Write(data)
			if err != nil {
				return fmt.Errorf("write HLS VOD segment %d: %w", nextIdx, err)
			}
			d.bytesWritten.Add(int64(n))
			// Seq reports the just-WRITTEN sequence — the same last-written
			// convention as every other emitter (see the live loop above) —
			// so the persisted seq columns mean one thing on every path.
			writtenSeq := int(d.currentSeq.Load())
			d.currentSeq.Add(1)
			nextIdx++

			if d.OnProgress != nil {
				d.OnProgress(DownloadProgress{
					Seq:   writtenSeq,
					Bytes: d.bytesWritten.Load(),
					Total: totalSegs,
				})
			}

			// Save resume state periodically — frequent enough that a crash
			// loses at most that many segments, rare enough that the
			// disk-touching saveResume isn't on the hot path for every segment.
			if d.currentSeq.Load()%50 == 0 {
				d.saveResume()
			}
		}
	}
	// Close a gap that runs to the end of the playlist.
	closeGap(totalSegs - 1)

	// The whole VOD playlist has been consumed — natural end. Mark ended so
	// runHlsLoop's deferred ClearResume removes the sidecar (see the ENDLIST
	// path above).
	if !d.isCancelled() && ctx.Err() == nil {
		d.streamEnded.Store(true)
	}
	d.saveResume()
	return nil
}
