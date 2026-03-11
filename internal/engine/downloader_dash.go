package engine

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// errStreamDone is a sentinel used internally by DASH error handlers to signal
// that the download loop should exit cleanly (return nil to the caller).
var errStreamDone = errors.New("stream done")

// runDashLoop is the main DASH download loop.
func (d *SegmentDownloader) runDashLoop(ctx context.Context) error {
	// Save resume state on exit; only clear on clean stream completion.
	// On shutdown (context cancel) or user cancel, keep the resume file
	// so the download can continue where it left off on restart.
	defer func() {
		// Snapshot fields under lock so we don't race with LastSeq() callers
		d.mu.Lock()
		seq := d.currentSeq
		head := d.headSeq
		d.mu.Unlock()

		// Emit final progress so the UI reflects the definitive last state
		if d.OnProgress != nil && seq > 0 {
			d.OnProgress(DownloadProgress{
				Seq:     seq - 1,
				Bytes:   d.bytesWritten.Load(),
				HeadSeq: head,
			})
		}
		d.saveResume()
		if d.streamEnded.Load() {
			d.ClearResume()
		}
	}()

	consecutiveGoneErrors := 0
	hasStartedDownloading := false
	segsSinceResume := 0
	d.lastSegTime = time.Now() // Initialize to avoid premature NoSegmentTimeout

	// Same-segment retry tracking with exponential backoff (matches TS handleFailedSegmentDownload)
	sameSegRetries := 0
	lastRetrySeq := -1
	sameHeadRetryDelay := 0
	lastConfirmedHead := -1

	delayCap := d.opts.RetryDelayCap
	if delayCap <= 0 {
		delayCap = DefaultRetryDelayCap
	}
	liveCheckThreshold := d.opts.LiveCheckRetries
	if liveCheckThreshold <= 0 {
		liveCheckThreshold = 16
	}

	for {
		if d.isCancelled() || ctx.Err() != nil {
			return d.cancelErr(ctx)
		}

		// Check end sequence
		if d.opts.EndSeq >= 0 && d.currentSeq > d.opts.EndSeq {
			return nil
		}

		// Probe head sequence (use dedicated timer, not lastSegTime -- matches TS lastHeadProbeTime)
		if d.headSeq < 0 || time.Since(d.lastHeadProbeTime) > HeadProbeInterval {
			if headSeq, err := d.probeHeadSequence(ctx); err == nil {
				d.headSeq = headSeq
			}
			d.lastHeadProbeTime = time.Now()
		}

		// Parallel catch-up if far behind
		if d.headSeq > 0 {
			segsBehind := d.headSeq - d.currentSeq
			if segsBehind >= CatchupThreshold {
				nextSeq, err := d.runParallelCatchUp(ctx)
				if err != nil {
					return err
				}
				d.currentSeq = nextSeq
				hasStartedDownloading = true
				// Re-probe head after catch-up (matches TS behavior)
				if headSeq, err := d.probeHeadSequence(ctx); err == nil {
					d.headSeq = headSeq
				}
				d.lastHeadProbeTime = time.Now()
				// Only re-enter loop if catch-up closed the gap (TS: returns false if stillFarBehind)
				stillFarBehind := d.headSeq > 0 && (d.headSeq-d.currentSeq) >= CatchupThreshold
				if !stillFarBehind {
					continue
				}
				// Still far behind -- fall through to sequential download to avoid infinite catch-up loop
			}
		}

		// Download single segment
		segURL := d.buildSegmentURL(d.currentSeq)
		data, statusCode, err := d.fetchSegment(ctx, segURL)

		if err != nil || statusCode >= 400 {
			herr := d.handleDashError(ctx, statusCode, &consecutiveGoneErrors, hasStartedDownloading,
				&sameSegRetries, &lastRetrySeq, &sameHeadRetryDelay, &lastConfirmedHead, delayCap, liveCheckThreshold)
			if herr == errStreamDone {
				return nil // Clean exit
			}
			if herr != nil {
				return herr // Real error (ErrQualityLost, etc.)
			}
			continue // nil means retry
		}

		// Write segment
		n, writeErr := d.outputFile.Write(data)
		if writeErr != nil {
			return fmt.Errorf("write segment %d: %w", d.currentSeq, writeErr)
		}
		d.bytesWritten.Add(int64(n))
		d.lastSegTime = time.Now()
		consecutiveGoneErrors = 0
		sameHeadRetryDelay = 0
		sameSegRetries = 0
		hasStartedDownloading = true

		// Emit progress
		if d.OnProgress != nil {
			d.OnProgress(DownloadProgress{
				Seq:     d.currentSeq,
				Bytes:   d.bytesWritten.Load(),
				HeadSeq: d.headSeq,
			})
		}

		d.currentSeq++
		segsSinceResume++

		// Save resume state periodically
		if segsSinceResume >= ResumeSeqInterval {
			d.saveResume()
			segsSinceResume = 0
		}
	}
}

// handleDashError processes HTTP errors during DASH segment downloads.
// Returns:
//   - nil: retry (continue the loop)
//   - errStreamDone: clean exit (stream ended or gave up)
//   - other error: stop with that error (ErrQualityLost, etc.)
func (d *SegmentDownloader) handleDashError(ctx context.Context, statusCode int,
	consecutiveGoneErrors *int, hasStartedDownloading bool,
	sameSegRetries *int, lastRetrySeq *int,
	sameHeadRetryDelay *int, lastConfirmedHead *int,
	delayCap, liveCheckThreshold int) error {

	if statusCode == 403 || statusCode == 410 {
		return d.handleGoneError(ctx, consecutiveGoneErrors, hasStartedDownloading)
	}

	if statusCode == 429 {
		return d.handleRateLimitError(ctx, sameHeadRetryDelay, delayCap)
	}

	if statusCode >= 400 {
		return d.handleHTTPError(ctx, hasStartedDownloading, sameSegRetries, lastRetrySeq,
			sameHeadRetryDelay, lastConfirmedHead, delayCap, liveCheckThreshold)
	}

	// Generic non-HTTP error (timeout, network, etc.) -- simple 2s retry (matches TS)
	*consecutiveGoneErrors = 0
	sleepCtx(ctx, 2*time.Second)
	return nil
}

func (d *SegmentDownloader) handleGoneError(ctx context.Context, consecutiveGoneErrors *int, hasStartedDownloading bool) error {
	*consecutiveGoneErrors++

	if hasStartedDownloading && *consecutiveGoneErrors > 10 {
		// Check if stream is actually ended, or if our format just disappeared
		if d.opts.CheckStreamStatus != nil {
			ended, checkErr := d.opts.CheckStreamStatus(ctx)
			if checkErr != nil {
				d.logger.Warn("stream status check failed, assuming ended", "err", checkErr)
			} else if !ended {
				return ErrQualityLost
			}
		}
		d.streamEnded.Store(true)
		return errStreamDone
	}
	if !hasStartedDownloading && *consecutiveGoneErrors <= 20 {
		d.currentSeq++
		sleepCtx(ctx, 100*time.Millisecond)
		return nil // Continue loop
	}
	if !hasStartedDownloading && *consecutiveGoneErrors > 20 {
		return errStreamDone // Failed to find valid starting segment
	}
	// Single GONE while downloading -- small delay before retry (matches TS)
	sleepCtx(ctx, 500*time.Millisecond)
	return nil // Continue loop
}

func (d *SegmentDownloader) handleRateLimitError(ctx context.Context, sameHeadRetryDelay *int, delayCap int) error {
	*sameHeadRetryDelay++
	if *sameHeadRetryDelay > delayCap {
		*sameHeadRetryDelay = delayCap
	}
	backoff := time.Duration(*sameHeadRetryDelay*2) * time.Second
	d.logger.Warn("segment download rate-limited (429), backing off", "seq", d.currentSeq, "delay", backoff)
	sleepCtx(ctx, backoff)
	return nil // Continue loop
}

func (d *SegmentDownloader) handleHTTPError(ctx context.Context, hasStartedDownloading bool,
	sameSegRetries *int, lastRetrySeq *int,
	sameHeadRetryDelay *int, lastConfirmedHead *int,
	delayCap, liveCheckThreshold int) error {

	if d.currentSeq == *lastRetrySeq {
		*sameSegRetries++
	} else {
		*sameSegRetries = 1
		*lastRetrySeq = d.currentSeq
	}

	// Re-probe head
	if headSeq, probeErr := d.probeHeadSequence(ctx); probeErr == nil {
		d.headSeq = headSeq
	}

	behindHead := d.headSeq > 0 && d.currentSeq < d.headSeq
	stuckOnSegment := *sameSegRetries >= MaxSegmentRetries

	if behindHead && !stuckOnSegment {
		// Transient failure while behind head -- retry with small delay
		sleepCtx(ctx, 1*time.Second)
		return nil // Continue loop
	}

	// At/past live edge or stuck -- backoff with status checks

	// Reset backoff if head moved
	if d.headSeq > 0 && d.headSeq != *lastConfirmedHead {
		*lastConfirmedHead = d.headSeq
		*sameHeadRetryDelay = 0
	}

	*sameHeadRetryDelay++
	if *sameHeadRetryDelay > delayCap {
		*sameHeadRetryDelay = delayCap
	}

	// Check stream status at threshold
	if *sameHeadRetryDelay == liveCheckThreshold && d.opts.CheckStreamStatus != nil {
		ended, _ := d.opts.CheckStreamStatus(ctx)
		if ended {
			return errStreamDone
		}
	}

	// Check status on every probe at cap
	if *sameHeadRetryDelay >= delayCap && d.opts.CheckStreamStatus != nil {
		ended, _ := d.opts.CheckStreamStatus(ctx)
		if ended {
			return errStreamDone
		}
		// Stream still live but we can't get segments -- format may have changed
		if hasStartedDownloading {
			return ErrQualityLost
		}
	}

	// Also check no-segment timeout
	if time.Since(d.lastSegTime) > NoSegmentTimeout {
		if d.opts.CheckStreamStatus != nil && hasStartedDownloading {
			ended, checkErr := d.opts.CheckStreamStatus(ctx)
			if checkErr != nil {
				d.logger.Warn("stream status check failed, assuming ended", "err", checkErr)
			} else if !ended {
				return ErrQualityLost
			}
		}
		return errStreamDone
	}

	sleepCtx(ctx, time.Duration(*sameHeadRetryDelay)*time.Second)
	return nil // Continue loop
}
