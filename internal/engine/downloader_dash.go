package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// errStreamDone is a sentinel used internally by DASH error handlers to signal
// that the download loop should exit cleanly (return nil to the caller).
var errStreamDone = errors.New("stream done")

// Retry state thresholds and delays for the DASH error handler chain.
// All named here so grep-for-literal tuning is unambiguous.
const (
	// goneRetryDuringDownload is how many consecutive 403/410 responses we
	// tolerate after the first segment has been written before deciding the
	// stream has ended (or the selected quality has disappeared).
	goneRetryDuringDownload = 10
	// goneRetryBeforeFirstSegment is how many 403/410 we tolerate while
	// hunting for the first valid segment (e.g. pre-roll that's not yet
	// published). Each failure advances currentSeq by 1.
	goneRetryBeforeFirstSegment = 20

	// postBytes403CipherThreshold is the number of consecutive post-bytes-
	// written 403s that trips a cipher-solver invalidation. YouTube can
	// rotate its cipher solver mid-download; if we keep getting 403s in a
	// row after bytes have flowed, re-solving is cheaper than continuing
	// the ErrQualityLost → refresh cycle (audit reports/worker.md F11).
	postBytes403CipherThreshold = 5

	// genericRetryDelay is the fixed delay used between retries for
	// non-HTTP-status errors (timeouts, network failures).
	genericRetryDelay = 2 * time.Second
	// firstSegmentHuntDelay is the short delay between advancing past a
	// 403/410 while hunting for the first valid segment.
	firstSegmentHuntDelay = 100 * time.Millisecond
	// singleGoneRetryDelay is the small wait between a single 403/410 hit
	// during normal downloading before the next attempt.
	singleGoneRetryDelay = 500 * time.Millisecond
	// transientFailureRetryDelay is used when behind head but not stuck on
	// a single segment — treated as a quick transient.
	transientFailureRetryDelay = 1 * time.Second
)

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
	// "Started" means bytes are in the output file — including bytes restored
	// by resume. The first-segment hunt in handleGoneError must stay disabled
	// once data exists (it advances currentSeq past 403s, which would skip
	// real segments), and must stay ENABLED while the file is still empty.
	hasStartedDownloading := d.bytesWritten.Load() > 0
	segsSinceResume := 0
	d.lastSegTime.StoreNow() // Initialize to avoid premature NoSegmentTimeout

	// Same-segment retry tracking with exponential backoff: when the same
	// sequence keeps failing we ramp the delay so we don't hammer the CDN.
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

		// Probe head sequence with a dedicated timer rather than reusing
		// lastSegTime — head probes are out-of-band and shouldn't reset the
		// "no segments downloaded recently" stale-detection clock.
		if d.headSeq.Load() < 0 || d.lastHeadProbeTime.Since() > HeadProbeInterval {
			if headSeq, err := d.probeHeadSequence(ctx); err == nil {
				d.headSeq.Store(int64(headSeq))
			}
			d.lastHeadProbeTime.StoreNow()
		}

		// Parallel catch-up when far enough behind to have a real
		// multi-segment window to parallelize. The window is
		// (segsBehind - stayBehindSegments) segments, so gating at
		// CatchupThreshold(10) — below stayBehindSegments(30) — would collapse
		// targetSeq to curSeq+1 and spin the whole worker pool up-and-down
		// every iteration just to fetch ONE segment while merely keeping pace.
		// Require the window to hold at least CatchupThreshold segments.
		head := int(d.headSeq.Load())
		if head > 0 {
			segsBehind := head - curSeq
			if segsBehind >= stayBehindSegments+CatchupThreshold {
				preCatchupSeq := curSeq
				nextSeq, err := d.runParallelCatchUp(ctx)
				if err != nil {
					return err
				}
				d.currentSeq.Store(int64(nextSeq))
				// Catch-up can legitimately write nothing (e.g. the starting
				// segments are permanently evicted) — only bytes on disk turn
				// off the first-segment hunt in handleGoneError.
				hasStartedDownloading = d.bytesWritten.Load() > 0
				// Re-probe head after catch-up so the next iteration sees
				// the updated head position rather than the pre-catch-up
				// snapshot — head can advance significantly during a long
				// catch-up window.
				if headSeq, err := d.probeHeadSequence(ctx); err == nil {
					d.headSeq.Store(int64(headSeq))
				}
				d.lastHeadProbeTime.StoreNow()
				curHead := int(d.headSeq.Load())
				curSeqNow := int(d.currentSeq.Load())
				stillFarBehind := curHead > 0 && (curHead-curSeqNow) >= stayBehindSegments+CatchupThreshold
				// Re-enter catch-up back-to-back while it keeps making progress
				// and a real window remains — batches are bounded by
				// maxCatchupBatch, so draining a large gap means several
				// bounded catch-up cycles rather than one giant in-memory one,
				// and parallel is faster than sequential when head races ahead.
				// Fall through to the sequential path ONLY on ZERO progress: the
				// head-of-window segment is stuck/gone and handleGoneError's
				// backoff + first-segment hunt is what's designed to break it.
				if !stillFarBehind || nextSeq > preCatchupSeq {
					continue
				}
				// Still far behind AND zero progress -- fall through to
				// sequential download to break the stuck head segment.
			}
		}

		// Download single segment
		segURL := d.buildSegmentURL(int(d.currentSeq.Load()))
		data, statusCode, err := d.fetchSegment(ctx, segURL)

		if err != nil || statusCode >= 400 {
			herr := d.handleDashError(ctx, statusCode, err, &consecutiveGoneErrors, hasStartedDownloading,
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

		// Emit progress + aggregate health snapshot. The health update
		// piggy-backs on the same cadence so the UI sees throughput /
		// retry counters tick alongside the per-segment counter. Audit
		// reports/engine.md #31.
		p := DownloadProgress{
			Seq:     writeSeq,
			Bytes:   d.bytesWritten.Load(),
			HeadSeq: int(d.headSeq.Load()),
		}
		if d.OnProgress != nil {
			d.OnProgress(p)
		}
		d.emitHealthUpdate(p)

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
// fetchErr carries the body snippet that fetchSegment captured on
// non-2xx responses; it's inspected here to distinguish cipher-related
// 403s (empty/generic body) from PO-token / bot-challenge 403s (which
// invalidating the cipher won't fix). Returns:
//   - nil: retry (continue the loop)
//   - errStreamDone: clean exit (stream ended or gave up)
//   - other error: stop with that error (ErrQualityLost, etc.)
func (d *SegmentDownloader) handleDashError(ctx context.Context, statusCode int, fetchErr error,
	consecutiveGoneErrors *int, hasStartedDownloading bool,
	sameSegRetries *int, lastRetrySeq *int,
	sameHeadRetryDelay *int, lastConfirmedHead *int,
	delayCap, liveCheckThreshold int) error {

	if statusCode == 403 || statusCode == 410 {
		// 403 fire cases (only on 403; 410 = stream ended, not cipher):
		//   (a) pre-bytes — almost certainly a cipher issue (wrong n-param or sig);
		//   (b) post-bytes burst — >= postBytes403CipherThreshold consecutive
		//       403s after bytes were written usually means YouTube rotated the
		//       cipher solver mid-download; invalidate so the next refresh
		//       re-solves rather than spinning in ErrQualityLost cycles
		//       (audit reports/worker.md F11).
		// CAS rather than Load+Store: video and audio downloaders share a
		// cipherSolver, so concurrent 403s on both could otherwise both pass
		// the Load check, both Store(true), and both invoke OnCipherFailure
		// — duplicating an InvalidateSolver call. CAS guarantees exactly one
		// fire per downloader instance.
		if statusCode == 403 {
			hasBytes := d.bytesWritten.Load() > 0
			fireCipher := !hasBytes || *consecutiveGoneErrors >= postBytes403CipherThreshold
			// Audit engine.md #25: gate the cipher fire on body inspection.
			// A 403 whose body indicates PO-token rejection or bot detection
			// is NOT a cipher problem, and reinitialising the solver wastes
			// ~1s of player-JS recompile work that won't fix anything.
			if fireCipher && !is403LikelyCipher(fetchErr) {
				fireCipher = false
			}
			if fireCipher && d.cipherFailureFired.CompareAndSwap(false, true) &&
				d.OnCipherFailure != nil {
				// Callback returns a freshly-resolved BaseURL (or "")
				// so the engine can swap mid-download instead of
				// burning the next batch of fetches against the old
				// cipher's URL. Empty string falls through to the
				// pre-existing handleGoneError → ErrQualityLost path
				// where the strategy refreshes the manifest. DECISIONS #7.
				if newURL := d.OnCipherFailure(); newURL != "" {
					d.SetBaseURL(newURL)
					d.logger.Info("[Cipher] swapping BaseURL after 403 — fresh n-param decryption",
						"url_prefix", truncateURL(newURL, 120))
				}
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

	// Generic non-HTTP error (timeout, network, etc.) -- simple fixed-delay retry
	*consecutiveGoneErrors = 0
	utils.Sleep(ctx, genericRetryDelay)
	return nil
}

// non403CipherMarkers lists lower-cased substrings that mean the 403
// is NOT a cipher failure — invalidating the solver wouldn't help and
// would waste a ~1s player-JS recompile. Currently covers the PO-token
// rejection and bot-detection paths YouTube returns when their server
// classifies the request as untrusted regardless of cipher correctness.
//
// New patterns get added here as YouTube's anti-bot prose evolves; the
// cost of a false negative (treat a real non-cipher 403 as cipher) is
// one extraneous recompile, so missing entries here are recoverable.
var non403CipherMarkers = []string{
	"missing_pot", // PO token required but absent / rejected
	"po token",    // upstream prose for the same condition
	"bot",         // generic bot-detection language
	"automated",   // "automated requests" prose
	"captcha",     // explicit CAPTCHA challenge
}

// is403LikelyCipher returns false when the fetch error's body content
// matches a known non-cipher 403 signature; true otherwise (including
// nil err, empty body, or unknown patterns — those default to "cipher
// problem" because that's the historically dominant 403 cause and
// recompiling the solver fixes them. Audit engine.md #25.
func is403LikelyCipher(fetchErr error) bool {
	if fetchErr == nil {
		return true
	}
	msg := strings.ToLower(fetchErr.Error())
	for _, marker := range non403CipherMarkers {
		if strings.Contains(msg, marker) {
			return false
		}
	}
	return true
}

func (d *SegmentDownloader) handleGoneError(ctx context.Context, consecutiveGoneErrors *int, hasStartedDownloading bool) error {
	*consecutiveGoneErrors++

	if hasStartedDownloading && *consecutiveGoneErrors > goneRetryDuringDownload {
		if d.opts.IsOnline != nil && !d.opts.IsOnline() {
			d.emitActivity(ActivityReconnecting)
			d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
			if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
				return err
			}
			*consecutiveGoneErrors = 0
			return nil // Continue loop
		}
		d.emitActivity(ActivityVerifyingEnd)
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
	if !hasStartedDownloading && *consecutiveGoneErrors <= goneRetryBeforeFirstSegment {
		d.emitActivity(ActivityFindingFirstSegment)
		d.currentSeq.Add(1)
		utils.Sleep(ctx, firstSegmentHuntDelay)
		return nil // Continue loop
	}
	if !hasStartedDownloading && *consecutiveGoneErrors > goneRetryBeforeFirstSegment {
		return errStreamDone // Failed to find valid starting segment
	}
	// Single GONE while downloading -- small delay before retry
	utils.Sleep(ctx, singleGoneRetryDelay)
	return nil // Continue loop
}

func (d *SegmentDownloader) handleRateLimitError(ctx context.Context, sameHeadRetryDelay *int, delayCap int) error {
	*sameHeadRetryDelay++
	if *sameHeadRetryDelay > delayCap {
		*sameHeadRetryDelay = delayCap
	}
	// Exponential backoff capped at delayCap seconds. 1s, 2s, 4s, 8s, 16s, …
	// gets us out of a sustained 429 storm faster than the previous linear
	// 2s, 4s, 6s ramp, which spent too much time at low values when
	// YouTube's token bucket is fully depleted (audit reports/engine.md #15).
	// Shift count is clamped so the int64 cast can't overflow.
	const maxShift = 6 // 1<<6 == 64s — beyond delayCap default of 60s
	shift := min(max(*sameHeadRetryDelay-1, 0), maxShift)
	backoff := min(time.Duration(int64(1)<<uint(shift))*time.Second, time.Duration(delayCap)*time.Second)
	d.emitActivity(ActivityRateLimited)
	d.logger.Warn("segment download rate-limited (429), backing off", "seq", d.currentSeq.Load(), "delay", backoff)
	utils.Sleep(ctx, backoff)
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

	// Re-probe head, but honor the same HeadProbeInterval throttle the main
	// loop uses: at the caught-up live edge, each not-yet-published segment
	// fetch fails and lands here every ~1-2s, so an unthrottled probe here
	// issued ~5x the intended head-probe round-trips (a steady multiple of
	// wasted CDN requests across video+audio, 24/7). head advances
	// monotonically, so a probe at most HeadProbeInterval stale can only
	// under-report head — never skip a segment (the currentSeq fetch itself
	// discovers new segments), at worst a marginally slower backoff reset.
	if d.lastHeadProbeTime.Since() > HeadProbeInterval {
		if headSeq, probeErr := d.probeHeadSequence(ctx); probeErr == nil {
			d.headSeq.Store(int64(headSeq))
		}
		d.lastHeadProbeTime.StoreNow()
	}

	head := int(d.headSeq.Load())
	behindHead := head > 0 && curSeq < head
	stuckOnSegment := *sameSegRetries >= MaxSegmentRetries

	if behindHead && !stuckOnSegment {
		// Transient failure while behind head -- retry with small delay
		utils.Sleep(ctx, transientFailureRetryDelay)
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
			d.emitActivity(ActivityReconnecting)
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
			d.emitActivity(ActivityReconnecting)
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

	// Only surface "verifying end" once the backoff has escalated toward the
	// stream-status check; a brief at-edge wait (the normal steady state for a
	// healthy live stream that has caught up to the head) is not end-verification.
	if *sameHeadRetryDelay >= liveCheckThreshold {
		d.emitActivity(ActivityVerifyingEnd)
	}

	// Also check no-segment timeout
	if d.lastSegTime.Since() > NoSegmentTimeout {
		if d.opts.IsOnline != nil && !d.opts.IsOnline() {
			d.emitActivity(ActivityReconnecting)
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

	utils.Sleep(ctx, time.Duration(*sameHeadRetryDelay)*time.Second)
	return nil // Continue loop
}
