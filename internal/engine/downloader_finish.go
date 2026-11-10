package engine

import "errors"

// finishReason is why runDashLoop or runHlsLoop ended WITHOUT an error. It
// is recorded once, at the exit, and decides one thing: whether the loop's
// exit defer clears the resume sidecar (clearsSidecar). Every other exit —
// an error, a cancel — records nothing and keeps the sidecar.
//
// It does not replace the worker-visible latches: FinalizedBehindHead and
// FinalizedDuringInterruption are set where their evidence is found
// (finalizeBehindHead, stallForPossibleResume) and can accompany more than
// one reason (a MaxTimeout backstop that stalled first latches
// finalizedDuringInterruption too), so they stay their own atomics.
type finishReason uint32

const (
	// finishNone: still running, or ended by an error or a cancel.
	finishNone finishReason = iota
	// finishConfirmed: the broadcast is confirmed over and every segment
	// the run could know of is written — DASH's gone path after a confirmed
	// end, and every HLS confirmed end (ENDLIST, a verdict, the consumed
	// VOD playlist). With finishSeqRestart, the only reasons that clear the
	// sidecar.
	finishConfirmed
	// finishConfirmedViaHTTP: DASH, handleHTTPError's status check confirmed
	// the end. KEEPS the sidecar, unlike finishConfirmed — owner ruling
	// R-G1 (Q10) kept the behaviour this exit has always had: staging is
	// wiped after a successful finish anyway, so the kept file is never
	// read, and clearing it would only move resume evidence for no gain.
	finishConfirmedViaHTTP
	// finishBehindHead: DASH finalized knowing the head is ahead
	// (finalizeBehindHead fired). Keeps the sidecar: a retry appends the
	// missing tail.
	finishBehindHead
	// finishInterrupted: DASH's gone path finalized after the interruption
	// stall gave up (Tier 2). Keeps the sidecar the worker preserves.
	finishInterrupted
	// finishMaxTimeout: the no-segment MaxTimeout backstop (DASH at-edge
	// path, YouTube HLS). Not a confirmed end; keeps the sidecar so the
	// worker's still-live refresh resumes from it.
	finishMaxTimeout
	// finishNoFirstSegment: DASH's first-segment hunt ran out with nothing
	// written. Start returns nil with 0 bytes, which the worker classifies
	// (zero-byte eviction diagnosis, min-size checks). Nothing was written,
	// so saveResume writes no sidecar either.
	finishNoFirstSegment
	// finishEndSeq: DASH reached opts.EndSeq, a bounded download. Keeps the
	// sidecar.
	finishEndSeq
	// finishSeqRestart: HLS, the playlist's media sequence restarted far
	// below the position (a new broadcast on the same channel). Ends the
	// recording cleanly and clears the sidecar, which would point into the
	// old numbering.
	finishSeqRestart
)

// clearsSidecar reports whether the loop's exit defer removes the resume
// sidecar after this finish.
func (r finishReason) clearsSidecar() bool {
	switch r {
	case finishConfirmed, finishSeqRestart:
		return true
	case finishNone, finishConfirmedViaHTTP, finishBehindHead, finishInterrupted,
		finishMaxTimeout, finishNoFirstSegment, finishEndSeq:
		return false
	}
	return false
}

// record stores why the run is ending. The loop returns right after.
func (d *SegmentDownloader) record(r finishReason) { d.finishedAs.Store(uint32(r)) }

// finish records r and returns errStreamDone, the DASH handlers' "the loop
// ends cleanly" answer (dispatch turns it into Start's nil).
func (d *SegmentDownloader) finish(r finishReason) error {
	d.record(r)
	return errStreamDone
}

// finishedWith is the recorded finishReason (finishNone while running).
func (d *SegmentDownloader) finishedWith() finishReason { return finishReason(d.finishedAs.Load()) }

// streamEnded reports a confirmed, complete end: the loop's exit defer
// clears the resume sidecar exactly when this is true.
func (d *SegmentDownloader) streamEnded() bool { return d.finishedWith().clearsSidecar() }

// dispatch maps a DASH error handler's answer onto runDashLoop:
//
//	nil                  retry: the loop continues
//	errSegmentSkipped    refuseSegment moved past a hole: afterSkip, continue
//	errStreamDone        clean exit: Start returns nil (the handler recorded why)
//	any other error      stop with it (ErrQualityLost, SegmentRefusedError, …)
//
// cont reports whether the loop continues; ret is what it returns when it
// does not. Sentinels are matched with errors.Is so a wrapped one keeps its
// meaning. The single place the three handler call sites (refuseSegment,
// handleDashError, handleHTTPError on an empty body) decide what an answer
// means; the refuseSegment site handles its own errNotRefused first.
func dispatch(herr error, afterSkip func()) (cont bool, ret error) {
	switch {
	case herr == nil:
		return true, nil
	case errors.Is(herr, errSegmentSkipped):
		afterSkip()
		return true, nil
	case errors.Is(herr, errStreamDone):
		return false, nil
	default:
		return false, herr
	}
}
