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

	// credentialRefreshCooldown is the minimum gap between two
	// OnCredentialRefresh invocations. Each refresh costs a player-response
	// round trip plus a PO-token mint, and a 403 burst produces hundreds of
	// failures per second, so without this the recovery path would hammer
	// YouTube harder than the failure did. yt-dlp gates its equivalent
	// (url_feed's delay argument) at 5s once fragments start failing.
	credentialRefreshCooldown = 5 * time.Second

	// forbiddenRefreshAttempts bounds how many times one segment is retried
	// through a credential refresh before it is reported permanently gone.
	// Kept small: the refresh itself is cooldown-gated, so a higher number
	// mostly buys sleep, and the caller (catch-up or the sequential loop)
	// re-attempts the segment on its next pass anyway.
	// INVARIANT: the retry window this produces MUST exceed
	// credentialRefreshCooldown, or a segment can burn every attempt inside a
	// closed cooldown and be declared permanently gone without ever seeing
	// fresh credentials. The first version violated it — 3 attempts with a
	// flat 500ms delay is a 1s window against a 5s cooldown, so a segment
	// that started failing in the ~4s after a refresh never got one, became a
	// gap, and left the sequential loop to grind it out. Measured cost: live
	// catch-up ran at 0.80 seg/s against the 3.1 seg/s it manages on fresh
	// credentials (field data, 2026-08-15).
	//
	// 5 attempts with the doubling delay below span 7.5s, so every failing
	// segment now outlives one full cooldown and gets a real refresh.
	forbiddenRefreshAttempts = 5
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
	d.lastSegTime.StoreNow() // Initialize to avoid premature MaxTimeout

	// Same-segment retry tracking with exponential backoff: when the same
	// sequence keeps failing we ramp the delay so we don't hammer the CDN.
	sameSegRetries := 0
	lastRetrySeq := -1
	sameHeadRetryDelay := 0
	lastConfirmedHead := -1

	// Backoff-sleep cap for at-edge retries (fixed). The operator knob is now
	// MaxTimeout (config.MaximumTimeout) — how long to keep waiting/verifying
	// before force-finalizing — not a separate retry-delay cap or check count.
	delayCap := DefaultRetryDelayCap

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
				d.noteHeadSeqFromProbe(headSeq)
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
				// Catch-up progress is the same recovery signal as a
				// sequential write: the gone streak is broken and any latched
				// "ended" verdict was proven transient — re-arm both so a
				// later burst re-verifies with the API instead of finalizing
				// a still-live stream on a stale verdict.
				if nextSeq > preCatchupSeq {
					consecutiveGoneErrors = 0
					d.streamEndVerified = false
				}
				// No dedicated head probe here: catch-up's workers harvest
				// X-Head-Seqnum from every segment response
				// (noteHeadSeqFromResponse), so headSeq is at most one
				// segment-response stale when catch-up returns — and the
				// rolling window chased that live head internally, so a
				// still-far-behind return means a gap stopped it, not that
				// the batch ended. The probe that used to sit here ran once
				// per batch, an out-of-band round trip on the hot path; the
				// HeadProbeInterval-paced probe at the top of the loop
				// remains the backstop for a stalled/quiet edge.
				curHead := int(d.headSeq.Load())
				curSeqNow := int(d.currentSeq.Load())
				stillFarBehind := curHead > 0 && (curHead-curSeqNow) >= stayBehindSegments+CatchupThreshold
				// Re-enter catch-up while it keeps making progress and a real
				// window remains — after a mid-span gap (the only way a
				// rolling catch-up returns still-far-behind with progress),
				// re-entering retries the gap sequence as the new
				// head-of-window with a fresh damped window. Fall through to
				// the sequential path ONLY on ZERO progress: the
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
				&sameSegRetries, &lastRetrySeq, &sameHeadRetryDelay, &lastConfirmedHead, delayCap)
			if herr == errStreamDone {
				return nil // Clean exit
			}
			if herr != nil {
				return herr // Real error (ErrQualityLost, etc.)
			}
			continue // nil means retry
		}

		// Segment body in hand — record the arrival before the write so the
		// fetched-bytes signal covers every path uniformly (the parallel
		// catch-up workers record theirs inside fetchSegmentWithRetry).
		d.noteFetch(len(data))

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
		// Re-arm end verification: a landed segment proves the burst that
		// latched the verdict (if any) was transient.
		d.streamEndVerified = false

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
	delayCap int) error {

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
		return d.handleGoneError(ctx, statusCode, consecutiveGoneErrors, hasStartedDownloading)
	}

	if statusCode == 429 {
		return d.handleRateLimitError(ctx, sameHeadRetryDelay, delayCap)
	}

	if statusCode >= 400 {
		return d.handleHTTPError(ctx, hasStartedDownloading, sameSegRetries, lastRetrySeq,
			sameHeadRetryDelay, lastConfirmedHead, delayCap)
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

func (d *SegmentDownloader) handleGoneError(ctx context.Context, statusCode int, consecutiveGoneErrors *int, hasStartedDownloading bool) error {
	*consecutiveGoneErrors++

	// The sequential loop calls fetchSegment directly (runDashLoop), not
	// fetchSegmentWithRetry, so it never gets that function's behind-head
	// credential-refresh retry on its own — only the parallel catch-up
	// worker does. Mirror the recovery here: once a 403 burst has persisted
	// past postBytes403CipherThreshold — the same threshold that already
	// gates the cipher-solver invalidation above in handleDashError, so
	// "persisted" means the same thing in both places — AND
	// behindHeadTailPending() confirms the segments demonstrably exist, ask
	// for fresh credentials before the existing retry/sleep below.
	// refreshCredentials is cooldown-gated (credentialRefreshCooldown), so
	// calling it again on every remaining gone iteration in the burst is
	// safe: at most one player-response round trip actually goes out per
	// cooldown window. This does not touch the finalize/verdict logic below
	// — it only runs a side effect before it.
	//
	// statusCode == 403 only: handleGoneError is the shared handler for
	// both 403 and 410 (handleDashError routes both here), but a refresh
	// only makes sense for 403 — stale credentials are a 403 signature.
	// 410 means genuinely evicted (marathon-stream eviction is a documented
	// real scenario below head, not a credential problem), and firing a
	// refresh on every 410 in an eviction burst would be a doomed call each
	// time: a watch-page fetch plus a cold BotGuard mint plus
	// invalidate403Caches wiping the shared cipher solver (forcing a
	// player-JS recompile that also hits the sibling downloader), burning
	// the stall budget on retries that cannot possibly help during exactly
	// the event where retries matter most.
	if statusCode == 403 && hasStartedDownloading && *consecutiveGoneErrors >= postBytes403CipherThreshold && d.behindHeadTailPending() {
		d.refreshCredentials()
	}

	if hasStartedDownloading && *consecutiveGoneErrors > goneRetryDuringDownload {
		if d.opts.IsOnline != nil && !d.opts.IsOnline() {
			d.emitActivity(ActivityReconnecting)
			d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
			if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
				return err
			}
			// Offline pauses the clock (mirrors handleHTTPError's offline
			// branches): behindHeadTailPending charges its MaxTimeout budget
			// against lastSegTime, so an outage longer than MaxTimeout would
			// otherwise finalize a behind-head recording on the first
			// post-reconnect gone instead of granting a fresh budget.
			d.lastSegTime.StoreNow()
			*consecutiveGoneErrors = 0
			return nil // Continue loop
		}
		d.emitActivity(ActivityVerifyingEnd)
		// Verify the stream's status. A CONFIRMED "ended" latches
		// (streamEndVerified) so the behind-head retry loop below doesn't
		// re-spend the API call every gone iteration; a landed segment
		// re-arms it. A check ERROR does NOT latch — the next interval
		// re-asks, keeping the ErrQualityLost refresh reachable for a
		// still-live stream that hit a transient status-probe failure.
		// Re-checks are throttled to streamStatusCheckInterval within a
		// burst.
		verdictKnown := d.streamEndVerified || d.opts.CheckStreamStatus == nil
		if !verdictKnown && d.lastStreamStatusCheck.Since() >= streamStatusCheckInterval {
			d.lastStreamStatusCheck.StoreNow()
			ended, checkErr := d.opts.CheckStreamStatus(ctx)
			switch {
			case checkErr != nil:
				d.logger.Warn("stream status check failed; deferring end verdict", "err", checkErr)
			case !ended:
				return ErrQualityLost
			default:
				d.streamEndVerified = true
				verdictKnown = true
			}
		}
		// Behind-head guard (yt-dlp 8c1f07d81 port): X-Head-Seqnum harvested
		// from segment responses says how many segments exist. An "ended"
		// verdict while currentSeq is still strictly below head means the 403
		// burst is URL/POT staleness or a CDN blip — the unfetched tail
		// demonstrably exists (post-live segments stay fetchable for hours
		// while YouTube processes the VOD). Finalizing here was the silent-
		// truncation bug: keep retrying until the MaxTimeout budget runs out.
		// OnCipherFailure has already had its shot at swapping in a fresh URL
		// (it fires at postBytes403CipherThreshold, below the gone threshold).
		if d.behindHeadTailPending() {
			d.emitActivity(ActivityWaitingForSegment)
			utils.Sleep(ctx, singleGoneRetryDelay)
			return nil // Continue loop
		}
		if !verdictKnown && d.lastSegTime.Since() < d.opts.MaxTimeout {
			// No confirmed end and budget remains — keep retrying rather
			// than finalizing on an assumption (the pre-guard behavior
			// treated a failed status check as "ended", which silently
			// truncated on a transient probe failure).
			d.emitActivity(ActivityWaitingForSegment)
			utils.Sleep(ctx, singleGoneRetryDelay)
			return nil // Continue loop
		}
		if d.finalizeBehindHead() {
			// Known-incomplete finalize: leave streamEnded unset so the
			// runDashLoop defer keeps the resume sidecar — a later retry
			// appends the missing tail instead of truncating the recording
			// and starting over. (handleHTTPError's errStreamDone exits
			// never set streamEnded either; only a confirmed-complete end
			// clears resume.)
			return errStreamDone
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
	// Single GONE while downloading -- the next segment isn't there yet. Surface
	// the wait (the tracker's 2s segment grace suppresses it for a healthy
	// stream); it escalates to VerifyingEnd above once gones pile up.
	d.emitActivity(ActivityWaitingForSegment)
	utils.Sleep(ctx, singleGoneRetryDelay)
	return nil // Continue loop
}

// behindHeadTailPending reports whether the known head sequence implies
// unfetched segments remain (currentSeq strictly below head) and the
// MaxTimeout budget since the last written segment hasn't expired — i.e.
// an "ended" verdict should be deferred rather than acted on. head == 0 or
// unknown (-1) disables the guard: with no head knowledge we can't
// distinguish a truncation from a clean end, so legacy behavior applies.
func (d *SegmentDownloader) behindHeadTailPending() bool {
	head := int(d.headSeq.Load())
	cur := int(d.currentSeq.Load())
	return head > 0 && cur < head && d.lastSegTime.Since() < d.opts.MaxTimeout
}

// warnIfFinalizingBehindHead logs loudly when the downloader is about to
// finalize with currentSeq still below the known head — the recording is
// missing a tail that existed at some point. This keeps an unavoidable
// truncation (segments unavailable, MaxTimeout exhausted) visible instead
// of masquerading as a clean finish. Returns true when finalizing behind
// head so callers can keep the resume sidecar: post-live jobs have no
// other resume mechanism (dbResumeSeq returns 0 for non-live), and a
// cleared sidecar would make a later retry O_TRUNC the file and restart
// from scratch — while a KEPT sidecar lets the retry append exactly the
// missing tail once YouTube finishes processing.
func (d *SegmentDownloader) warnIfFinalizingBehindHead() bool {
	head := int(d.headSeq.Load())
	cur := int(d.currentSeq.Load())
	if head > 0 && cur < head {
		d.logger.Warn("[Downloader] finalizing with unfetched tail — segments below head stayed unavailable",
			"currentSeq", cur, "headSeq", head, "missing", head-cur,
			"gapSinceLastSegment", d.lastSegTime.Since().Round(time.Second))
		return true
	}
	return false
}

// finalizeBehindHead calls warnIfFinalizingBehindHead and, when it fires,
// latches finalizedBehindHead in the same step — the three errStreamDone
// escalation sites (handleGoneError, and handleHTTPError's confirmed-end and
// MaxTimeout-backstop branches) all route through this single call so the
// warning and the worker-visible flag can never drift apart.
func (d *SegmentDownloader) finalizeBehindHead() bool {
	if d.warnIfFinalizingBehindHead() {
		d.finalizedBehindHead.Store(true)
		return true
	}
	return false
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
	delayCap int) error {

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
			d.noteHeadSeqFromProbe(headSeq)
		}
		d.lastHeadProbeTime.StoreNow()
	}

	head := int(d.headSeq.Load())
	behindHead := head > 0 && curSeq < head
	stuckOnSegment := *sameSegRetries >= MaxSegmentRetries

	if behindHead && !stuckOnSegment {
		// Transient failure while behind head -- retry with small delay. Surface
		// the wait (2s grace suppresses it for a stream that recovers quickly).
		d.emitActivity(ActivityWaitingForSegment)
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

	// Time-based stream-status verification. A live segment is ~1s of media
	// arriving about once a second, so once the gap since the last segment
	// crosses streamStatusCheckInterval (30s) the wait itself is the signal to
	// verify the stream ended — re-checked at most once per interval so an ended
	// stream finalizes within ~30s (rather than waiting out MaxTimeout), while
	// brief hiccups (the lastSegTime gate) don't spend an API call.
	if d.opts.CheckStreamStatus != nil &&
		d.lastSegTime.Since() >= streamStatusCheckInterval &&
		d.lastStreamStatusCheck.Since() >= streamStatusCheckInterval {
		if d.opts.IsOnline != nil && !d.opts.IsOnline() {
			d.emitActivity(ActivityReconnecting)
			d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
			if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
				return err
			}
			// Offline pauses the clock: reset lastSegTime so the outage doesn't
			// count toward MaxTimeout (a recovered stream gets a fresh budget),
			// matching the backstop's offline branch below. Without this, a long
			// outage detected here first would leave lastSegTime aged and could
			// force-finalize a still-live stream on the next non-gone HTTP error.
			d.lastSegTime.StoreNow()
			d.lastStreamStatusCheck.StoreNow()
			*sameHeadRetryDelay = 0
			return nil
		}
		d.lastStreamStatusCheck.StoreNow()
		ended, checkErr := d.opts.CheckStreamStatus(ctx)
		if checkErr != nil {
			d.logger.Warn("stream status check failed while waiting for segment", "err", checkErr)
		} else if ended {
			// Share the confirmed verdict with handleGoneError's latch so a
			// mixed 403/other-error burst doesn't re-spend a CheckStreamStatus
			// call learning what this path already knows (both handlers run on
			// the single download-loop goroutine).
			d.streamEndVerified = true
			// Behind-head guard — same rationale as handleGoneError: an ended
			// stream whose head says more segments exist shouldn't finalize on
			// a stall; the tail stays fetchable through the post-live window.
			// Fall through to the backoff below until MaxTimeout expires (the
			// backstop further down owns the eventual force-finalize).
			if !d.behindHeadTailPending() {
				d.finalizeBehindHead()
				return errStreamDone
			}
		} else if behindHead && hasStartedDownloading {
			// Still live, but a segment we KNOW exists (curSeq < head) won't
			// come — the format/URL likely rotated; refresh via ErrQualityLost.
			return ErrQualityLost
		}
		// Still live and simply at the live edge — keep waiting for the next segment.
	}

	// Surface the wait: "waiting for next segment" for the earlier at-edge gap
	// (the segment simply hasn't published yet — the normal steady state for a
	// healthy stream caught up to the head), escalating to "verifying end" once
	// the no-segment gap crosses the verify interval and we start checking with
	// YouTube. The tracker's 2s segment grace suppresses both when the next
	// segment arrives promptly, so a healthy stream shows neither.
	if d.lastSegTime.Since() >= streamStatusCheckInterval {
		d.emitActivity(ActivityVerifyingEnd)
	} else {
		d.emitActivity(ActivityWaitingForSegment)
	}

	// Maximum-timeout backstop: once the no-segment gap exceeds the configured
	// MaxTimeout, force-finalize the recording even if YouTube still reports the
	// stream live — its status can lag or stick. Offline is the one exception:
	// pause the clock and wait for connectivity rather than give up. The clock
	// resets whenever a segment lands (lastSegTime updates), so a recovered
	// stream gets a fresh MaxTimeout budget.
	if d.lastSegTime.Since() >= d.opts.MaxTimeout {
		if d.opts.IsOnline != nil && !d.opts.IsOnline() {
			d.emitActivity(ActivityReconnecting)
			d.logger.Warn("stream end signal suppressed — device offline, waiting for connectivity")
			if err := waitForConnectivity(ctx, d.opts.IsOnline); err != nil {
				return err
			}
			d.lastSegTime.StoreNow() // reset the timeout clock on recovery
			*sameHeadRetryDelay = 0
			return nil
		}
		d.logger.Info("[Downloader] maximum timeout reached while waiting for segment; finalizing",
			"maxTimeout", d.opts.MaxTimeout, "gap", d.lastSegTime.Since().Round(time.Second))
		d.finalizeBehindHead()
		return errStreamDone
	}

	utils.Sleep(ctx, time.Duration(*sameHeadRetryDelay)*time.Second)
	return nil // Continue loop
}
