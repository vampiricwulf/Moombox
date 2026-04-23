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
		// Atomic loads — no mutex needed for currentSeq/headSeq.
		seq := int(d.currentSeq.Load())
		head := int(d.headSeq.Load())

		// Emit final progress so the UI reflects the definitive last state.
		// Only emit when actual data was written — handleGoneError increments
		// currentSeq on 403/410 failures without writing bytes, so seq > 0 alone
		// is insufficient and would trigger false progress signals.
		if d.OnProgress != nil && seq > 0 && d.bytesWritten.Load() > 0 {
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
	d.lastSegTime.StoreNow() // Initialize to avoid premature NoSegmentTimeout

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
		curSeq := int(d.currentSeq.Load())
		if d.opts.EndSeq >= 0 && curSeq > d.opts.EndSeq {
			return nil
		}

		// Probe head sequence (use dedicated timer, not lastSegTime -- matches TS lastHeadProbeTime)
		if d.headSeq.Load() < 0 || d.lastHeadProbeTime.Since() > HeadProbeInterval {
			if headSeq, err := d.probeHeadSequence(ctx); err == nil {
				d.headSeq.Store(int64(headSeq))
			}
			d.lastHeadProbeTime.StoreNow()
		}

		// Parallel catch-up if far behind
		head := int(d.headSeq.Load())
		if head > 0 {
			segsBehind := head - curSeq
			if segsBehind >= CatchupThreshold {
				nextSeq, err := d.runParallelCatchUp(ctx)
				if err != nil {
					return err
				}
				d.currentSeq.Store(int64(nextSeq))
				hasStartedDownloading = true
				// Re-probe head after catch-up (matches TS behavior)
				if headSeq, err := d.probeHeadSequence(ctx); err == nil {
					d.headSeq.Store(int64(headSeq))
				}
				d.lastHeadProbeTime.StoreNow()
				// Only re-enter loop if catch-up closed the gap (TS: returns false if stillFarBehind)
				curHead := int(d.headSeq.Load())
				curSeqNow := int(d.currentSeq.Load())
				stillFarBehind := curHead > 0 && (curHead-curSeqNow) >= CatchupThreshold
				if !stillFarBehind {
					continue
				}
				// Still far behind -- fall through to sequential download to avoid infinite catch-up loop
			}
		}

		// Download single segment
		segURL := d.buildSegmentURL(int(d.currentSeq.Load()))
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
		writeSeq := int(d.currentSeq.Load())
		n, writeErr := d.outputFile.Write(data)
		if writeErr != nil {
			return fmt.Errorf("write segment %d: %w", writeSeq, writeErr)
		}
		d.bytesWritten.Add(int64(n))
		d.lastSegTime.StoreNow()
		consecutiveGoneErrors = 0
		sameHeadRetryDelay = 0
		sameSegRetries = 0
		hasStartedDownloading = true

		// Emit progress
		if d.OnProgress != nil {
			d.OnProgress(DownloadProgress{
				Seq:     writeSeq,
				Bytes:   d.bytesWritten.Load(),
				HeadSeq: int(d.headSeq.Load()),
			})
		}

		d.currentSeq.Add(1)
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
		// 403 before any bytes written = likely cipher failure (wrong n-param or sig).
		// 410 = stream ended (not cipher). Only fire on 403.
		if statusCode == 403 && !d.cipherFailureFired.Load() && d.bytesWritten.Load() == 0 {
			d.cipherFailureFired.Store(true)
			if d.OnCipherFailure != nil {
				d.OnCipherFailure()
			}
		}
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
		if d.opts.IsOnline != nil && !d.opts.IsOnline() {
			d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
			if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
				return err
			}
			*consecutiveGoneErrors = 0
			return nil // Continue loop
		}
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
		d.currentSeq.Add(1)
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
	d.logger.Warn("segment download rate-limited (429), backing off", "seq", d.currentSeq.Load(), "delay", backoff)
	sleepCtx(ctx, backoff)
	return nil // Continue loop
}

func (d *SegmentDownloader) handleHTTPError(ctx context.Context, hasStartedDownloading bool,
	sameSegRetries *int, lastRetrySeq *int,
	sameHeadRetryDelay *int, lastConfirmedHead *int,
	delayCap, liveCheckThreshold int) error {

	curSeq := int(d.currentSeq.Load())
	if curSeq == *lastRetrySeq {
		*sameSegRetries++
	} else {
		*sameSegRetries = 1
		*lastRetrySeq = curSeq
	}

	// Re-probe head
	if headSeq, probeErr := d.probeHeadSequence(ctx); probeErr == nil {
		d.headSeq.Store(int64(headSeq))
	}

	head := int(d.headSeq.Load())
	behindHead := head > 0 && curSeq < head
	stuckOnSegment := *sameSegRetries >= MaxSegmentRetries

	if behindHead && !stuckOnSegment {
		// Transient failure while behind head -- retry with small delay
		sleepCtx(ctx, 1*time.Second)
		return nil // Continue loop
	}

	// At/past live edge or stuck -- backoff with status checks

	// Reset backoff if head moved
	if head > 0 && head != *lastConfirmedHead {
		*lastConfirmedHead = head
		*sameHeadRetryDelay = 0
	}

	*sameHeadRetryDelay++
	if *sameHeadRetryDelay > delayCap {
		*sameHeadRetryDelay = delayCap
	}

	// Check stream status at threshold
	if *sameHeadRetryDelay == liveCheckThreshold && d.opts.CheckStreamStatus != nil {
		if d.opts.IsOnline != nil && !d.opts.IsOnline() {
			d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
			if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
				return err
			}
			*sameHeadRetryDelay = 0
			return nil
		}
		ended, _ := d.opts.CheckStreamStatus(ctx)
		if ended {
			return errStreamDone
		}
	}

	// Check status on every probe at cap
	if *sameHeadRetryDelay >= delayCap && d.opts.CheckStreamStatus != nil {
		if d.opts.IsOnline != nil && !d.opts.IsOnline() {
			d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
			if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
				return err
			}
			*sameHeadRetryDelay = 0
			return nil
		}
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
	if d.lastSegTime.Since() > NoSegmentTimeout {
		if d.opts.IsOnline != nil && !d.opts.IsOnline() {
			d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
			if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
				return err
			}
			d.lastSegTime.StoreNow() // Reset timer on recovery
			*sameHeadRetryDelay = 0
			return nil
		}
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
