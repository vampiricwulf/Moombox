package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// errStreamDone is a sentinel used internally by DASH error handlers to signal
// that the download loop should exit cleanly (return nil to the caller).
var errStreamDone = errors.New("stream done")

// errSegmentSkipped is refuseSegment's other internal sentinel: it skipped
// the refused sequence in place (currentSeq is already past it and its gap
// row recorded), and the loop carries on with fresh per-segment retry state.
var errSegmentSkipped = errors.New("segment skipped")

// errNotRefused is refuseSegment declining: no later segment is served
// either, so the failure is not one segment's, and the caller's ordinary
// handling goes on. Never leaves the package.
var errNotRefused = errors.New("not a refused segment")

// Retry state thresholds and delays for the DASH error handler chain.
// All named here so grep-for-literal tuning is unambiguous.
const (
	// goneRetryDuringDownload is how many consecutive 403/410 responses we
	// tolerate after the first segment has been written before deciding the
	// stream has ended (or the selected quality has disappeared).
	goneRetryDuringDownload = 10
	// goneRetryBeforeFirstSegment is how many 403/410 we tolerate while
	// hunting for the first valid segment (e.g. pre-roll that's not yet
	// published). A failure advances currentSeq by 1 only where
	// huntMayAdvance allows it — below a known head, and never off an
	// InitInlineAtStart downloader's sq=0; anywhere else the same sequence is
	// asked again, singleGoneRetry apart.
	goneRetryBeforeFirstSegment = 20

	// postBytes403CipherThreshold is the number of consecutive post-bytes-
	// written 403s that trips a cipher-solver invalidation. YouTube can
	// rotate its cipher solver mid-download; if we keep getting 403s in a
	// row after bytes have flowed, re-solving is cheaper than continuing
	// the ErrQualityLost → refresh cycle (audit reports/worker.md F11).
	postBytes403CipherThreshold = 5

	// genericRetryDelay is the base delay between tries of a fetch that got
	// no usable answer where nothing else bounds it (delays.genericRetry: the
	// direct downloader's backoff). The DASH loop no longer uses it: a
	// transport failure there goes through handleHTTPError.
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

	// segmentRetryUnitDelay is the step of fetchSegmentWithRetry's linear
	// ladder: a transient failure (5xx, timeout, empty body) on attempt n
	// waits n+1 of these before the next try — 5, 10, 15, 20 s at the
	// default MaxSegmentRetries.
	segmentRetryUnitDelay = 5 * time.Second
	// interruptionStallRetryDelay paces the retry loop while
	// stallForPossibleResume is deferring a budget-expired finalize. A
	// resume-detection latency of 5s is negligible against the multi-minute
	// (or multi-hour) outages this stall exists for; singleGoneRetryDelay's
	// 500ms would instead poll GVS ~2x/sec per track (~4x/sec across
	// video+audio) for as long as InterruptionTimeout allows — up to ~14k
	// no-op requests per track at the default 2h ceiling.
	interruptionStallRetryDelay = 5 * time.Second

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

// dashRetryState is runDashLoop's per-segment retry state: the counters the
// loop and its error handlers share. It is a local of the loop, never stored
// and never read from another goroutine. The fields are named, so a handler
// cannot be handed two of them in the wrong order.
type dashRetryState struct {
	// consecutiveGoneErrors counts 403/410 answers in a row (handleGoneError).
	consecutiveGoneErrors int
	// Same-segment retry tracking with exponential backoff: when the same
	// sequence keeps failing we ramp the delay so we don't hammer the CDN.
	// sameSegRetries counts the failures on lastRetrySeq.
	sameSegRetries int
	lastRetrySeq   int
	// sameHeadRetryDelay is the at-edge (and 429) backoff step. It restarts
	// when the head moves past lastConfirmedHead.
	sameHeadRetryDelay int
	lastConfirmedHead  int
}

// newDashRetryState returns the state a loop starts with: no failures, and
// no sequence or head seen yet.
func newDashRetryState() *dashRetryState {
	return &dashRetryState{lastRetrySeq: -1, lastConfirmedHead: -1}
}

// onSegmentWritten is the reset a landed segment earns: the gone streak, the
// at-edge backoff and the same-segment retries all start over.
func (rs *dashRetryState) onSegmentWritten() {
	rs.consecutiveGoneErrors = 0
	rs.sameHeadRetryDelay = 0
	rs.sameSegRetries = 0
}

// onSkip is the reset after refuseSegment skipped a sequence in place: the
// next sequence starts with a fresh gone streak and fresh same-segment
// retries.
func (rs *dashRetryState) onSkip() {
	rs.consecutiveGoneErrors, rs.sameSegRetries = 0, 0
}

// onCatchUpProgress is the reset parallel catch-up earns when it advanced:
// the gone streak is broken.
func (rs *dashRetryState) onCatchUpProgress() {
	rs.consecutiveGoneErrors = 0
}

// runDashLoop is the main DASH download loop.
func (d *SegmentDownloader) runDashLoop(ctx context.Context) error {
	// Save resume state on exit; only clear on clean stream completion
	// (the recorded finishReason's clearsSidecar — downloader_finish.go
	// lists every exit and what it does to the sidecar). On shutdown
	// (context cancel) or user cancel, keep the resume file so the
	// download can continue where it left off on restart.
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
		if d.streamEnded() {
			d.ClearResume()
		}
	}()

	// Per-segment retry state: see dashRetryState.
	rs := newDashRetryState()
	// Measurement only: see dashEdgeStats. Logged once, at Debug, on exit.
	edge := &dashEdgeStats{}
	defer func() { edge.logOnExit(d.logger) }()
	// "Started" means bytes are in the output file — including bytes restored
	// by resume. The first-segment hunt in handleGoneError must stay disabled
	// once data exists (it advances currentSeq past 403s, which would skip
	// real segments), and must stay ENABLED while the file is still empty.
	hasStartedDownloading := d.bytesWritten.Load() > 0
	segsSinceResume := 0
	d.lastSegTime.StoreNow() // Initialize to avoid premature MaxTimeout

	// The refused-segment rule's memory (refuseSegment): the sequence the
	// sequential fetch last failed on, and how many credential refreshes had
	// been installed when it first did. A later 403 on the same sequence with
	// that count moved on is the "failed again after fresh credentials" the
	// rule waits for.
	stuckSeq, stuckRenewals := -1, int64(0)
	// afterSkip runs once refuseSegment has skipped a sequence in place. The
	// next sequence starts with fresh per-segment retry state, and — as
	// moonarchive sets `last_seq_auth_expiry = cur_seq` after a skip, "to
	// avoid multiple player requests on sequential 403s" — counts as already
	// asked on fresh credentials: a 403 there meets the rule at once, so a
	// run of refused sequences is walked in one pass instead of one refresh,
	// or one rebuild, per sequence. Each step still needs a later segment
	// served (refuseSegment), so a refusal of everything stops the walk.
	afterSkip := func() {
		stuckSeq, stuckRenewals = int(d.currentSeq.Load()), d.credentialRenewals.Load()-1
		rs.onSkip()
	}

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
			d.record(finishEndSeq)
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
					rs.onCatchUpProgress()
					d.streamEndVerified = false
					// Per-episode reset: this progress proves any interruption
					// stall in progress has resolved. Without this, a SECOND,
					// unrelated interruption hours later would measure its
					// InterruptionTimeout ceiling from the FIRST episode's start
					// (a stale, already-expired clock) and could spuriously latch
					// finalizedDuringInterruption on a burst that never stalled.
					d.interruptionStallStart.Clear()
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
		fetchStart := time.Now()
		data, statusCode, err := d.fetchSegment(ctx, segURL)

		// Count a failed fetch at or past the known head (dashEdgeStats). The
		// head is read after the fetch, as the handlers read it: the failed
		// answer itself may have carried a newer one. A fetch the loop's own
		// cancellation cut short is not an answer and is left out.
		if (err != nil || statusCode >= 400 || len(data) == 0) && ctx.Err() == nil {
			if head := int(d.headSeq.Load()); head > 0 && int(d.currentSeq.Load()) >= head {
				edge.noteFailure(statusCode, time.Since(fetchStart))
			}
		}

		if err != nil || statusCode >= 400 {
			// moonarchive's rule for a segment YouTube keeps refusing
			// (refuseSegment): a 403 on the sequence that already failed
			// before a credential refresh was installed. moonarchive checks
			// it ahead of its own handling (_dash.py, `if
			// last_seq_auth_expiry == cur_seq`), and so does this: once it
			// holds, a further retry is the refusal moonarchive stops
			// asking.
			if failedSeq := int(d.currentSeq.Load()); failedSeq != stuckSeq {
				stuckSeq, stuckRenewals = failedSeq, d.credentialRenewals.Load()
			} else if statusCode == 403 && d.credentialRenewals.Load() > stuckRenewals && d.refusable(failedSeq) {
				herr := d.refuseSegment(ctx, failedSeq)
				if !errors.Is(herr, errNotRefused) {
					if cont, ret := dispatch(herr, afterSkip); !cont {
						return ret
					}
					continue
				}
				// Not one segment's refusal: handled as before, and asked
				// again only after the next fresh credentials.
				stuckRenewals = d.credentialRenewals.Load()
			}
			herr := d.handleDashError(ctx, statusCode, err, rs, hasStartedDownloading, delayCap)
			if cont, ret := dispatch(herr, afterSkip); !cont {
				return ret
			}
			continue
		}

		// A 2xx that carried no body is not a segment. Written as one, it
		// advanced currentSeq, refreshed lastSegTime and broke the gone
		// streak — so past the end of a stream whose edge answers every
		// sequence with an empty 200 (moonarchive's _dash.py meets one "if
		// the live replay is unlisted at the end", and at the end of a stream
		// in general), the loop ran at request speed forever: currentSeq raced
		// past the head, the resume sidecar recorded a LastSeq beyond it, and
		// neither MaxTimeout nor CheckStreamStatus — reached only from the
		// error handlers — ever fired. yt-dlp's HttpFD fails such a fetch
		// ("Did not get any data blocks", downloader/http.py) and moonarchive
		// retries it (EmptyFragmentException) before consulting the stream's
		// status. Here it is a failed fetch of THIS sequence, routed through
		// handleHTTPError: at or past the head that is the at-edge path
		// (backoff, head re-probe, CheckStreamStatus every
		// streamStatusCheckInterval, the MaxTimeout backstop); below it the
		// transient retry, which escalates to the same checks once the
		// segment stays stuck. Never handleGoneError: an empty body says
		// nothing about whether the sequence exists, so it must neither
		// advance the first-segment hunt nor count toward a gone burst.
		if len(data) == 0 {
			d.logger.Debug("[Downloader] segment answered with an empty body — retrying it",
				"seq", d.currentSeq.Load(), "status", statusCode, "headSeq", d.headSeq.Load())
			herr := d.handleHTTPError(ctx, hasStartedDownloading, rs, delayCap)
			if cont, ret := dispatch(herr, afterSkip); !cont {
				return ret
			}
			continue
		}

		// Segment body in hand — record the arrival before the write so the
		// fetched-bytes signal covers every path uniformly (the parallel
		// catch-up workers record theirs inside fetchSegmentWithRetry).
		d.noteFetch(len(data))

		// Write segment
		writeSeq := int(d.currentSeq.Load())
		if initErr := d.ensureInlineInit(ctx, writeSeq); initErr != nil {
			return initErr
		}
		n, writeErr := d.outputFile.Write(data)
		if writeErr != nil {
			return fmt.Errorf("%w: write segment %d: %w", ErrLocalWrite, writeSeq, writeErr)
		}
		d.bytesWritten.Add(int64(n))
		d.lastSegTime.StoreNow()
		d.firstMediaWritten(writeSeq)
		rs.onSegmentWritten()
		edge.segmentsWritten++
		hasStartedDownloading = true
		// Re-arm end verification: a landed segment proves the burst that
		// latched the verdict (if any) was transient.
		d.streamEndVerified = false
		// Per-episode reset — see the identical comment in the catch-up
		// progress branch above.
		d.interruptionStallStart.Clear()

		p := DownloadProgress{
			Seq:     writeSeq,
			Bytes:   d.bytesWritten.Load(),
			HeadSeq: int(d.headSeq.Load()),
		}
		if d.OnProgress != nil {
			d.OnProgress(p)
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
// fetchErr carries the body snippet that fetchSegment captured on
// non-2xx responses; it's inspected here to distinguish cipher-related
// 403s (empty/generic body) from PO-token / bot-challenge 403s (which
// invalidating the cipher won't fix). Returns:
//   - nil: retry (continue the loop)
//   - errStreamDone: clean exit (stream ended or gave up)
//   - other error: stop with that error (ErrQualityLost, etc.)
func (d *SegmentDownloader) handleDashError(ctx context.Context, statusCode int, fetchErr error,
	rs *dashRetryState, hasStartedDownloading bool, delayCap int) error {

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
			fireCipher := !hasBytes || rs.consecutiveGoneErrors >= postBytes403CipherThreshold
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
		return d.handleGoneError(ctx, statusCode, &rs.consecutiveGoneErrors, hasStartedDownloading)
	}

	if statusCode == 429 {
		return d.handleRateLimitError(ctx, &rs.sameHeadRetryDelay, delayCap)
	}

	if statusCode >= 400 {
		return d.handleHTTPError(ctx, hasStartedDownloading, rs, delayCap)
	}

	// No complete HTTP answer: a dial, TLS or DNS failure, a reset, a stalled
	// or cut-off body.
	rs.consecutiveGoneErrors = 0
	if d.opts.IsOnline != nil && !d.opts.IsOnline() {
		// Same shape as the HTTP siblings above (handleGoneError,
		// handleHTTPError): a transport error during an outage used to sit
		// here sleeping genericRetry with nothing on the progress line, so a
		// DASH job rode out a network outage showing a frozen counter while
		// the HLS loop said "Connection lost - reconnecting...".
		d.emitActivity(ActivityReconnecting)
		d.logger.Warn("segment fetch failed — device offline, waiting for connectivity")
		if err := waitForConnectivity(ctx, d.opts.IsOnline, d.delays.connectivityPoll); err != nil {
			return err
		}
		// Offline pauses the clock, as in the siblings: the outage must not
		// count toward MaxTimeout or the interruption ceiling.
		d.lastSegTime.StoreNow()
		d.noteOfflineRecovery()
		return nil
	}
	// Online, a transport failure is bounded exactly like a 5xx (owner ruling
	// R2-E1): the same retry count, stream-status check, hand-back for a
	// rebuild on a fresh player response and MaxTimeout finalize, with no
	// budget of its own. It used to sleep genericRetryDelay and return, for as
	// long as the failure lasted: one googlevideo edge that stopped answering
	// while the device stayed online (a single failing host does not take the
	// connectivity monitor offline) left the job retrying with no status
	// check, no rebuild and no finalize, where the same edge answering 503 got
	// all three. A failed fetch reads no X-Head-Seqnum, so a finalize behind
	// the head after such a spell does not call that head current
	// (HeadCurrentAtFinalize).
	d.logger.Debug("[Downloader] segment fetch failed without an HTTP answer — handled like a server error",
		"seq", d.currentSeq.Load(), "status", statusCode, "error", fetchErr)
	return d.handleHTTPError(ctx, hasStartedDownloading, rs, delayCap)
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
			if err := waitForConnectivity(ctx, d.opts.IsOnline, d.delays.connectivityPoll); err != nil {
				return err
			}
			// Offline pauses the clock (mirrors handleHTTPError's offline
			// branches): behindHeadTailPending charges its MaxTimeout budget
			// against lastSegTime, so an outage longer than MaxTimeout would
			// otherwise finalize a behind-head recording on the first
			// post-reconnect gone instead of granting a fresh budget.
			d.lastSegTime.StoreNow()
			d.noteOfflineRecovery()
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
				return d.handBackForRebuild(ctx, true)
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
			// Retrying, not waiting: the segment is below head, so it exists
			// and the fetch is what keeps failing ("Waiting for next segment"
			// read as a healthy live edge for up to MaxTimeout).
			d.emitActivity(ActivityRetrying)
			utils.Sleep(ctx, d.delays.singleGoneRetry)
			return nil // Continue loop
		}
		if !verdictKnown && d.lastSegTime.Since() < d.opts.MaxTimeout {
			// No confirmed end and budget remains — keep retrying rather
			// than finalizing on an assumption (the pre-guard behavior
			// treated a failed status check as "ended", which silently
			// truncated on a transient probe failure).
			d.emitActivity(ActivityWaitingForSegment)
			utils.Sleep(ctx, d.delays.singleGoneRetry)
			return nil // Continue loop
		}
		// Interruption spec Tier 1/2: a confirmed-ended verdict (streamEndVerified)
		// must finalize immediately regardless of MayResume — only an
		// UNCONFIRMED budget expiry defers for possible resume.
		if !d.streamEndVerified && d.stallForPossibleResume() {
			d.emitActivity(ActivityWaitingResume)
			utils.Sleep(ctx, d.delays.interruptionStallRetry)
			return nil // Continue loop — the refresh path revives in place on resume
		}
		if d.finalizeBehindHead() {
			// Known-incomplete finalize: finishBehindHead keeps the resume
			// sidecar — a later retry appends the missing tail instead of
			// truncating the recording and starting over.
			return d.finish(finishBehindHead)
		}
		if d.finalizedDuringInterruption.Load() {
			// Tier 2: this finalize was reached after stallForPossibleResume
			// deferred at least once above (MayResume was true, then either
			// flipped false or the InterruptionTimeout ceiling expired). The
			// worker needs the resume sidecar intact to give the broadcast a
			// chance to resume, so finishInterrupted keeps it rather than
			// letting the runDashLoop defer's ClearResume() destroy the exact
			// evidence this latch exists to preserve.
			return d.finish(finishInterrupted)
		}
		// The one DASH exit that clears the sidecar. handleHTTPError's
		// confirmed end (finishConfirmedViaHTTP) keeps it — owner ruling
		// R-G1, see downloader_finish.go.
		return d.finish(finishConfirmed)
	}
	if !hasStartedDownloading && *consecutiveGoneErrors <= goneRetryBeforeFirstSegment {
		d.emitActivity(ActivityFindingFirstSegment)
		if !d.huntMayAdvance() {
			// Same sequence again: the segment past it cannot be the first
			// one to look for (see huntMayAdvance). The at-edge pause, not the
			// hunt's: this is a wait for the segment to be served, and the
			// hunt's 100 ms would re-ask a not-yet-published sequence ten
			// times a second for the whole budget.
			//
			// Held BELOW a known head — which only an InitInlineAtStart sq=0
			// is — the segment demonstrably exists, so a 403 that outlives
			// the cipher swap (handleDashError fires OnCipherFailure on the
			// first pre-bytes 403) is stale credentials: a PO token the edge
			// now refuses, whose "missing_pot" body also suppresses that
			// swap. The refresh at the top of this function needs
			// hasStartedDownloading, so without this one the hold re-sent the
			// same refused credentials for its whole budget and ended with
			// nothing written, while the same start with the head 40 or more
			// ahead recovered in place through catch-up's
			// fetchSegmentWithRetry, which refreshes below the head. The gate
			// is that refresh's: a 403 only (a 410 is evicted, and no refresh
			// brings it back), a burst past postBytes403CipherThreshold (the
			// swap gets its turn first), and behindHeadTailPending, so a hold
			// at or past the head, or with the head unknown, never refreshes
			// for a segment that is merely not published yet.
			// refreshCredentials is cooldown-gated: the hold's ~10 s costs at
			// most two player-response round trips.
			if statusCode == 403 && *consecutiveGoneErrors >= postBytes403CipherThreshold && d.behindHeadTailPending() {
				d.refreshCredentials()
			}
			utils.Sleep(ctx, d.delays.singleGoneRetry)
			return nil // Continue loop
		}
		d.currentSeq.Add(1)
		utils.Sleep(ctx, firstSegmentHuntDelay)
		return nil // Continue loop
	}
	if !hasStartedDownloading && *consecutiveGoneErrors > goneRetryBeforeFirstSegment {
		return d.finish(finishNoFirstSegment) // Failed to find valid starting segment
	}
	// Single GONE while downloading -- the next segment isn't there yet. Surface
	// the wait (the tracker's 2s segment grace suppresses it for a healthy
	// stream); it escalates to VerifyingEnd above once gones pile up.
	d.emitActivity(ActivityWaitingForSegment)
	utils.Sleep(ctx, d.delays.singleGoneRetry)
	return nil // Continue loop
}

// huntMayAdvance reports whether the first-segment hunt may step past the
// sequence that just answered 403/410 before any byte was written. The hunt
// exists for a start that sits on evicted or otherwise refused segments; it
// may only step onto segments the edge has said exist, and never off the one
// segment the file cannot do without.
//
//   - Only below a KNOWN head (currentSeq < headSeq). yt-dlp's live generator
//     never requests a sequence before X-Head-Seqnum says it exists
//     (extractor/youtube/_video.py, _live_adaptive_fragments, from 8c1f07d81
//     #16771 — the manifest-free generator, which keeps the X-Head-Seqnum
//     bound adbc4ec4b #888 gave the manifest one, _live_dash_fragments):
//     with the head unknown, or the refused sequence already at or
//     past it, the segment is most likely not published yet — the go-live
//     pre-roll goneRetryBeforeFirstSegment's own comment names — and stepping
//     past it only asks for a later sequence that exists even less. The hunt
//     used to step twenty sequences past the edge in two seconds, ending
//     with nothing written or beginning the file wherever the edge caught up
//     with it.
//   - Never off sq=0 of an InitInlineAtStart downloader, even below a known
//     head — yt-dlp's own shape: fragment 0 is fatal, retried, never skipped
//     (downloader/dash.py, `is_fatal=lambda idx: idx == 0` from 814bba393
//     #5274; fragment.py). That sq=0 holds the stream's only
//     ftyp+moov, and its media is the first seconds of the recording.
//     Stepping past it — even after OnCipherFailure had just repaired the
//     URL that was refused, which is what that first pre-bytes 403 usually
//     means — began the file at sq=1 with no init. ensureInlineInit alone
//     would rescue the init but not sq=0's media; retrying sq=0 keeps both,
//     and a sq=0 that stays refused ends the hunt with nothing written,
//     exactly as a refused later one does.
//
// The retry is bounded by the same goneRetryBeforeFirstSegment budget, so a
// start that never finds its segment still ends: with zero bytes and
// currentSeq where it began, which is where the orchestrator's refresh and
// diagnoseEvictedStart both expect it.
func (d *SegmentDownloader) huntMayAdvance() bool {
	cur := d.currentSeq.Load()
	if d.opts.InitInlineAtStart && cur == 0 {
		return false
	}
	return cur < d.headSeq.Load()
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
// other resume mechanism (dbResumeSeq returns 0 for non-live), and a KEPT
// usable sidecar is what lets the retry append exactly the missing tail
// once YouTube finishes processing. Clear it and the retry cannot resume:
// it either refuses outright (ErrStagedMediaPresent) or, for the
// manifest-free restart that sets DiscardStaged, sets the recording aside
// as <file>.restart-<ts> and starts over from sq=0 — correct, but a whole
// re-download instead of a tail.
//
// The line says which of two things it saw. A stop within
// EndOfStreamHeadMargin of the head is the shape an ended broadcast has (the
// reported head runs past the last segment YouTube serves), and is logged at
// Info: whether that recording is complete is the worker's verdict, which
// also knows whether the stream is confirmed over and whether anything
// interrupted the capture (worker.incompleteTail, owner ruling R-T1). The
// engine decides nothing by it: the latch and the kept sidecar are the same
// either way. A stop further back is the Warn it always was.
func (d *SegmentDownloader) warnIfFinalizingBehindHead() bool {
	head := int(d.headSeq.Load())
	cur := int(d.currentSeq.Load())
	if head > 0 && cur < head {
		last := cur - 1
		if WithinEndOfStreamMargin(last, head) {
			d.logger.Info("[Downloader] finalizing short of the reported head, within the end-of-stream margin — the segments past the last one written stayed unavailable for the whole retry budget; the worker decides whether the recording is complete",
				"lastSeq", last, "currentSeq", cur, "headSeq", head, "shortOfHead", head-last, "margin", EndOfStreamHeadMargin,
				"gapSinceLastSegment", d.lastSegTime.Since().Round(time.Second))
			return true
		}
		d.logger.Warn("[Downloader] finalizing with unfetched tail — segments below head stayed unavailable",
			"lastSeq", last, "currentSeq", cur, "headSeq", head, "missing", head-cur,
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
		read := d.lastHeadRead.Load()
		d.headCurrentAtFinalize.Store(!read.IsZero() && read.After(d.lastSegTime.Load()) && time.Since(read) <= d.delays.headFreshWindow)
		d.finalizedBehindHead.Store(true)
		return true
	}
	return false
}

// refusable reports whether the refused-segment rule may consider seq: the
// caller can close a part there (SplitOnRefusedSegment), data is already in
// the file — before the first byte the first-segment hunt owns refusals —
// seq is not sq=0, and the tracked head is more than two segments past it.
// It is the cheap gate; refuseSegment confirms the head on a served segment
// before it gives anything up (laterSegmentServed).
//
// The head bound is moonarchive's (`max_seq - 2 > cur_seq`, _dash.py): it
// gives up only on a segment the edge has demonstrably moved well past,
// never one at the live edge that may simply not be served yet, nor one of
// the two sequences X-Head-Seqnum runs ahead of the last real segment once a
// broadcast is over (moonarchive _dash.py: "YouTube will increment the max
// sequence number by 2 without any fragments being present there"). sq=0 is
// yt-dlp's fatal fragment 0 (downloader/dash.py, `is_fatal=lambda idx: idx ==
// 0`): it carries a manifest-free stream's only init.
func (d *SegmentDownloader) refusable(seq int) bool {
	return d.SplitOnRefusedSegment && d.bytesWritten.Load() > 0 && seq > 0 &&
		d.headSeq.Load()-2 > int64(seq)
}

// noteServedHeadSeq advances servedHeadSeq to n when greater (monotonic
// max, as noteHeadSeq): the head a served segment response reported.
func (d *SegmentDownloader) noteServedHeadSeq(n int) {
	for {
		cur := d.servedHeadSeq.Load()
		if int64(n) <= cur || d.servedHeadSeq.CompareAndSwap(cur, int64(n)) {
			return
		}
	}
}

// refuseSegment acts on a segment YouTube keeps refusing although later ones
// are served. Both upstreams give up on such a segment rather than on the
// recording: yt-dlp skips any fragment but the first once fragment_retries
// are spent (downloader/fragment.py, skip_unavailable_fragments, on by
// default; downloader/dash.py makes only index 0 fatal), and moonarchive —
// "It's very rare, but there are instances when YouTube repeatedly responds
// with a 403 on a fragment that should be valid (even unplayable when
// rewinding is enabled). Skip it." (downloaders/youtube/_dash.py) — skips a
// sequence that 403s again after a fresh player response, while
// `max_seq - 2 > cur_seq` (refusable). Moombox never did: once data was
// written the loop could not move past a failing sequence, so a live capture
// cycled refresh, ErrQualityLost and a rebuild at the same sequence for the
// rest of the broadcast, recording nothing after the hole (2026-10-31 audit,
// G15).
//
// It goes beyond moonarchive in one check before it gives anything up: a
// later segment must actually be served now (laterSegmentServed). moonarchive
// skips on its head bound alone, so a refusal of EVERY segment — a PO token
// or an address YouTube stopped accepting, which no fresh player response
// mends — walks its capture forward one dropped segment at a time while it
// lasts. Here such a refusal is not a refused segment: refuseSegment
// declines (errNotRefused) and the caller's old handling — retries, the
// stream-status check, the live loop's rebuild — goes on holding the hole
// until the stream serves again.
//
// It goes beyond both upstreams in what it does with the hole, by owner
// decision D8: the part is CLOSED there and the next one starts past it with
// its own init, so every file stays internally gapless — the shape Twitch's
// StopOnGap split gives a lost segment. It returns a *SegmentRefusedError and
// the caller makes the split. The exception is a new part that could not
// get its init: a manifest-free stream carries its ftyp+moov only inline in
// sq=0 (InitInlineAtStart, InitFromSegment), and on a stream past YouTube's
// retention window sq=0 is no longer served. Then — and only then — the
// segment is skipped in place, as the upstreams skip it: the file goes on
// past a hole, its fMP4 fragments carrying their own decode times (tfdt),
// with a gap row for the missing sequence (errSegmentSkipped). A file that
// holds its init and no media yet (holdsNoMedia) — a part that a split has
// just opened, refused at its very first segment — is skipped in place too:
// there is no part to close, and the file stays gapless from its first
// fragment. Closing it would have opened one init-only part per refused
// sequence of a run.
func (d *SegmentDownloader) refuseSegment(ctx context.Context, seq int) error {
	served, later := d.laterSegmentServed(ctx)
	head := d.servedHeadSeq.Load()
	if !served || head-2 <= int64(seq) {
		d.logger.Warn("[Downloader] segment refused again on fresh credentials, but no later segment is served well past it — not giving it up",
			"seq", seq, "probedSeq", later, "served", served, "servedHeadSeq", head)
		d.wholeStreamRefusedAt.Store(int64(seq) + 1)
		return errNotRefused
	}
	initOnly := d.holdsNoMedia()
	if !initOnly && !d.newPartInitEvicted(ctx) {
		d.logger.Warn("[Downloader] segment refused again on fresh credentials while later segments are served — closing the part at it",
			"seq", seq, "headSeq", head)
		return &SegmentRefusedError{Seq: seq}
	}
	reason := "sq=0 is no longer served for a new part's init"
	if initOnly {
		reason = "the part holds no media yet"
	}
	d.logger.Warn("[Downloader] segment refused again on fresh credentials — skipping it in place",
		"seq", seq, "headSeq", head, "reason", reason)
	if d.OnGap != nil {
		d.OnGap(DownloadGap{From: seq, To: seq})
	}
	d.currentSeq.Store(int64(seq + 1))
	return errSegmentSkipped
}

// laterSegmentServed probes the segment just below the tracked head — past
// the hole, past any run of refused sequences after it, and below the head,
// so it should exist — and reports whether it is served now (a 2xx with a
// body), with the sequence it asked. refusable has already put that
// sequence at least two past the hole. Served, its response also records
// the head refuseSegment then measures the hole against (servedHeadSeq): the
// rule's bound always rests on a served segment's X-Head-Seqnum, as
// moonarchive's max_seq does, never on a refusal's.
func (d *SegmentDownloader) laterSegmentServed(ctx context.Context) (bool, int) {
	later := int(d.headSeq.Load()) - 1
	status, body, err := d.probeSegment(ctx, later)
	return err == nil && status < 300 && len(body) > 0, later
}

// holdsNoMedia reports whether the output file holds no media fragment yet:
// its top-level boxes are the init alone, no moof or mdat among them. It is
// read from the file, not remembered from Start, because the live loop
// rebuilds a part in place — a fresh downloader appending to the same file —
// and that downloader never wrote the init it finds there. A file it cannot
// read as boxes counts as holding media, so D8's default, the split, stands.
func (d *SegmentDownloader) holdsNoMedia() bool {
	f, err := os.Open(d.opts.OutputFile)
	if err != nil {
		return false
	}
	defer f.Close()
	size := d.bytesWritten.Load()
	wk := &fmp4Walker{r: f, size: size}
	for pos := int64(0); pos < size; {
		b, err := wk.readBox(pos)
		if err != nil || b.pastEOF {
			return false
		}
		switch b.typ {
		case "moof", "mdat":
			return false
		}
		pos = b.end()
	}
	return true
}

// newPartInitEvicted reports whether a part started past the hole could not
// get its init: the stream keeps it only inline in sq=0, and sq=0 answers
// 410 — evicted, the one answer fetchInitSegment (downloader_init.go) also
// reads as gone for good. Any other answer is not eviction: a 403 is the
// refusal the new part's init ladder refreshes credentials for, a 429 or a
// 5xx is transient, and a transport error is no answer at all — so each
// reads as "not evicted", and D8's default, the split, stands. A stream whose
// init is a manifest Initialization is never asked: that URL is not a
// segment the edge evicts.
func (d *SegmentDownloader) newPartInitEvicted(ctx context.Context) bool {
	if !d.opts.InitInlineAtStart && !d.opts.InitFromSegment {
		return false
	}
	status, _, err := d.probeSegment(ctx, 0)
	return err == nil && status == http.StatusGone
}

// unservedSegmentPersistence is how long a segment that only FAILS — a 5xx,
// an empty body — must have kept failing since the live loop's first rebuild
// at it before a rebuild-exhausted downloader gives it up
// (handBackForRebuild). A 403 or a 410 is a refusal and is given up at the
// cap; a server error says the server failed, not that the segment is
// refused, and the cap's three rebuilds take about two minutes, short enough
// for a googlevideo 5xx burst to outlast. Ten minutes is DefaultMaxTimeout,
// the budget the DASH loop already gives a failing segment below the head
// of an ended stream.
const unservedSegmentPersistence = 10 * time.Minute

// handBackForRebuild is what the error handlers return where a still-live
// stream's segment below the head will not come: ErrQualityLost, the request
// for the caller to rebuild the download on a fresh player response. At the
// sequence the caller has already rebuilt at as many times as it will
// (NoteRebuildsExhausted), the refused-segment rule takes the segment
// instead: each of those rebuilds was a credential refresh, and the segment
// failed again after every one. That is the path for a 410 or a 5xx, which
// no in-loop refresh answers (the 403 rule in runDashLoop never sees them).
// refused is the handler's verdict on the answer: a 403 or a 410 is taken
// at once, anything else only once it has persisted for
// unservedSegmentPersistence.
func (d *SegmentDownloader) handBackForRebuild(ctx context.Context, refused bool) error {
	seq := int(d.currentSeq.Load())
	if int64(seq) != d.rebuildsExhaustedSeq.Load() || !d.refusable(seq) {
		return ErrQualityLost
	}
	if !refused && time.Since(d.rebuildsExhaustedSince) < unservedSegmentPersistence {
		return ErrQualityLost
	}
	if err := d.refuseSegment(ctx, seq); !errors.Is(err, errNotRefused) {
		return err
	}
	return ErrQualityLost
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
	backoff := min(time.Duration(int64(1)<<uint(shift))*d.delays.atEdgeBackoffUnit, time.Duration(delayCap)*d.delays.atEdgeBackoffUnit)
	d.emitActivity(ActivityRateLimited)
	d.logger.Warn("segment download rate-limited (429), backing off", "seq", d.currentSeq.Load(), "delay", backoff)
	utils.Sleep(ctx, backoff)
	return nil // Continue loop
}

func (d *SegmentDownloader) handleHTTPError(ctx context.Context, hasStartedDownloading bool,
	rs *dashRetryState, delayCap int) error {

	curSeq := int(d.currentSeq.Load())
	if curSeq == rs.lastRetrySeq {
		rs.sameSegRetries++
	} else {
		rs.sameSegRetries = 1
		rs.lastRetrySeq = curSeq
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
	stuckOnSegment := rs.sameSegRetries >= MaxSegmentRetries

	if behindHead && !stuckOnSegment {
		// Transient failure while behind head -- retry with small delay. Surface
		// the retry (2s grace suppresses it for a stream that recovers quickly);
		// the segment exists, so this is not a wait for the next one.
		d.emitActivity(ActivityRetrying)
		utils.Sleep(ctx, d.delays.transientFailureRetry)
		return nil // Continue loop
	}

	// At/past live edge or stuck -- backoff with status checks

	// Reset backoff if head moved
	if head > 0 && head != rs.lastConfirmedHead {
		rs.lastConfirmedHead = head
		rs.sameHeadRetryDelay = 0
	}

	rs.sameHeadRetryDelay++
	if rs.sameHeadRetryDelay > delayCap {
		rs.sameHeadRetryDelay = delayCap
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
			if err := waitForConnectivity(ctx, d.opts.IsOnline, d.delays.connectivityPoll); err != nil {
				return err
			}
			// Offline pauses the clock: reset lastSegTime so the outage doesn't
			// count toward MaxTimeout (a recovered stream gets a fresh budget),
			// matching the backstop's offline branch below. Without this, a long
			// outage detected here first would leave lastSegTime aged and could
			// force-finalize a still-live stream on the next non-gone HTTP error.
			d.lastSegTime.StoreNow()
			d.noteOfflineRecovery()
			d.lastStreamStatusCheck.StoreNow()
			rs.sameHeadRetryDelay = 0
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
				if d.finalizeBehindHead() {
					return d.finish(finishBehindHead)
				}
				// Confirmed over, yet the sidecar is KEPT — unlike the gone
				// path's confirmed end (finishConfirmed), which clears it.
				// Owner ruling R-G1 (Q10) kept this exit as it always was;
				// staging is wiped after a successful finish anyway.
				return d.finish(finishConfirmedViaHTTP)
			}
		} else if behindHead && hasStartedDownloading {
			// Still live, but a segment we KNOW exists (curSeq < head) won't
			// come — the format/URL likely rotated; refresh via ErrQualityLost.
			return d.handBackForRebuild(ctx, false)
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
			if err := waitForConnectivity(ctx, d.opts.IsOnline, d.delays.connectivityPoll); err != nil {
				return err
			}
			d.lastSegTime.StoreNow() // reset the timeout clock on recovery
			d.noteOfflineRecovery()
			rs.sameHeadRetryDelay = 0
			return nil
		}
		// Interruption spec Tier 1/2: same deferral as handleGoneError's
		// backstop — only an unconfirmed budget expiry stalls; a confirmed
		// end (streamEndVerified) above already routed to errStreamDone
		// before reaching here.
		if !d.streamEndVerified && d.stallForPossibleResume() {
			d.emitActivity(ActivityWaitingResume)
			utils.Sleep(ctx, d.delays.interruptionStallRetry)
			return nil // Continue loop — the refresh path revives in place on resume
		}
		d.logger.Info("[Downloader] maximum timeout reached while waiting for segment; finalizing",
			"maxTimeout", d.opts.MaxTimeout, "gap", d.lastSegTime.Since().Round(time.Second))
		// Note: unlike handleGoneError's confirmed-end fallthrough, neither
		// reason here clears the sidecar, so it always survives (including a
		// Tier 2 / finalizedDuringInterruption exit) without needing an
		// equivalent guard.
		if d.finalizeBehindHead() {
			return d.finish(finishBehindHead)
		}
		return d.finish(finishMaxTimeout)
	}

	utils.Sleep(ctx, time.Duration(rs.sameHeadRetryDelay)*d.delays.atEdgeBackoffUnit)
	return nil // Continue loop
}
