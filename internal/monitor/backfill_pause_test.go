package monitor

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// The disable-pauses-the-backfill tests (W25-16). Disabling a channel is a
// pause, not a removal: its in-flight scan stops — spliced out of the queue,
// or cancelled while running — and its cursor stays, so enabling it again
// resumes the scan instead of restarting it.

// pauseCursor is the cursor every pause test seeds and then expects to find
// untouched: a widen-cancel would clear it, a pause must not.
const pauseCursor = `{"window_days":3,"tabs":{"videos":{"continuation":"TOK2","next_pos":2}}}`

func disabledCopy(ch *config.ChannelConfig) *config.ChannelConfig {
	off := false
	c := *ch
	c.Enabled = &off
	return &c
}

func assertCursorKept(t *testing.T, bw *BackfillWorker, chID string) {
	t.Helper()
	raw, err := bw.db.LoadBackfillCursor(chID)
	if err != nil || raw != pauseCursor {
		t.Errorf("cursor of %s = %q (err %v), want %q kept: a paused scan must stay resumable", chID, raw, err, pauseCursor)
	}
}

// A scan QUEUED behind another channel's when its channel is disabled is
// dropped from the queue on the next sweep: it never runs, its idle is
// emitted (no runScan exit will send one), and its cursor is kept.
//
// Mutants killed: Sweep skipping a disabled channel without pauseLocked
// (B still in flight and scanned); the pausedIDs emission loop removed (no
// idle for B).
func TestSweep_DisableDropsAQueuedScanAndKeepsItsCursor(t *testing.T) {
	db := newTestDB(t)
	chA := backfillTestCh()
	chB := &config.ChannelConfig{ID: backfillTestChannel2, Name: "B"}
	if err := db.SaveBackfillCursor(chB.ID, pauseCursor); err != nil {
		t.Fatalf("SaveBackfillCursor: %v", err)
	}
	startedA := make(chan struct{})
	releaseA := make(chan struct{})
	doneA := make(chan struct{})
	var scannedB atomic.Bool
	bw := newTestBackfillWorker(t, db, withScan(func(ctx context.Context, _ *config.ChannelConfig, chID string, _ int, _ bool) error {
		if chID == chA.ID {
			close(startedA)
			<-releaseA
			close(doneA)
			return nil
		}
		scannedB.Store(true)
		return nil
	}))
	emitted := recordProgress(bw)
	startWorker(t, bw)

	bw.Sweep([]ChannelRef{refFor(chA, 3, false), refFor(chB, 3, false)}, false)
	recvWithin(t, startedA, "A's scan start")

	bw.Sweep([]ChannelRef{refFor(chA, 3, false), refFor(disabledCopy(chB), 3, false)}, false)
	inflight, queued := inflightState(bw)
	if inflight[chB.ID] != nil || queued != 0 {
		t.Fatalf("after the disable sweep: B in flight = %v, queue depth = %d; want B dropped", inflight[chB.ID] != nil, queued)
	}
	assertEmissions(t, emitted, []progressEmission{{chB.ID, "", 0, "idle"}})

	close(releaseA)
	recvWithin(t, doneA, "A's scan end")
	assertEmissions(t, emitted, []progressEmission{{chA.ID, "", 0, "done"}})
	if scannedB.Load() {
		t.Error("B was disabled while its scan was queued, yet the scan ran")
	}
	assertCursorKept(t, bw, chB.ID)
}

// A RUNNING scan whose channel is disabled is cancelled by the next sweep,
// and its cleanup keeps the cursor (a widen-cancel clears it).
//
// Mutants killed: Sweep skipping a disabled channel without pauseLocked (the
// scan is never cancelled); pauseLocked not setting fl.paused, and runScan's
// cleanup dropping its !isPaused guard (both clear the cursor).
func TestSweep_DisableCancelsARunningScanAndKeepsItsCursor(t *testing.T) {
	db := newTestDB(t)
	ch := backfillTestCh()
	if err := db.SaveBackfillCursor(ch.ID, pauseCursor); err != nil {
		t.Fatalf("SaveBackfillCursor: %v", err)
	}
	started := make(chan struct{})
	cancelled := make(chan struct{})
	bw := newTestBackfillWorker(t, db, withScan(func(ctx context.Context, _ *config.ChannelConfig, _ string, _ int, _ bool) error {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	}))
	emitted := recordProgress(bw)
	startWorker(t, bw)

	bw.Sweep([]ChannelRef{refFor(ch, 3, false)}, false)
	recvWithin(t, started, "scan start")
	inflight, _ := inflightState(bw)
	fl := inflight[ch.ID]

	bw.Sweep([]ChannelRef{refFor(disabledCopy(ch), 3, false)}, false)
	recvWithin(t, cancelled, "the running scan's cancellation")
	recvWithin(t, fl.done, "the paused scan's cleanup")
	assertEmissions(t, emitted, []progressEmission{{ch.ID, "", 0, "idle"}})
	if inflight, _ := inflightState(bw); inflight[ch.ID] != nil {
		t.Error("the paused scan left its in-flight entry behind")
	}
	assertCursorKept(t, bw, ch.ID)
}

// Enabling the channel again while its paused scan is still unwinding queues
// a fresh scan rather than mistaking the dying entry for one doing the work,
// and the fresh scan resumes from the kept cursor.
//
// Mutant killed: Sweep's in-flight guard without `&& !fl.paused` (the
// re-enable sweep skips the channel and no scan follows the paused one).
func TestSweep_ReenablingAPausedChannelResumesItsScan(t *testing.T) {
	db := newTestDB(t)
	ch := backfillTestCh()
	if err := db.SaveBackfillCursor(ch.ID, pauseCursor); err != nil {
		t.Fatalf("SaveBackfillCursor: %v", err)
	}
	starts := make(chan string, 4)
	unwinding := make(chan struct{})
	releaseUnwind := make(chan struct{})
	var runs atomic.Int32
	bw := newTestBackfillWorker(t, db, withScan(func(ctx context.Context, _ *config.ChannelConfig, chID string, _ int, _ bool) error {
		raw, _ := db.LoadBackfillCursor(chID)
		starts <- raw
		if runs.Add(1) == 1 {
			<-ctx.Done()
			close(unwinding)
			<-releaseUnwind // still unwinding when the re-enable sweep runs
			return ctx.Err()
		}
		return nil
	}))
	startWorker(t, bw)

	bw.Sweep([]ChannelRef{refFor(ch, 3, false)}, false)
	recvWithin(t, starts, "first scan start")
	bw.Sweep([]ChannelRef{refFor(disabledCopy(ch), 3, false)}, false)
	recvWithin(t, unwinding, "the paused scan unwinding")

	bw.Sweep([]ChannelRef{refFor(ch, 3, false)}, false)
	close(releaseUnwind)
	if raw := recvWithin(t, starts, "the resumed scan's start"); raw != pauseCursor {
		t.Errorf("resumed scan started from cursor %q, want the paused scan's %q", raw, pauseCursor)
	}
}

// A queued scan whose channel was disabled after the sweep that queued it —
// and before any sweep saw the change — is checked against the LIVE config
// as it reaches the consumer, and skipped with its cursor kept.
//
// Mutant killed: runScan without the ChannelEnabled check (B scans).
func TestRunScan_SkipsAChannelDisabledSinceItWasQueued(t *testing.T) {
	db := newTestDB(t)
	chA := backfillTestCh()
	chB := &config.ChannelConfig{ID: backfillTestChannel2, Name: "B"}
	if err := db.SaveBackfillCursor(chB.ID, pauseCursor); err != nil {
		t.Fatalf("SaveBackfillCursor: %v", err)
	}
	startedA := make(chan struct{})
	releaseA := make(chan struct{})
	var scannedB atomic.Bool
	var disabledB atomic.Bool
	bw := newTestBackfillWorker(t, db, withScan(func(ctx context.Context, _ *config.ChannelConfig, chID string, _ int, _ bool) error {
		if chID == chA.ID {
			close(startedA)
			<-releaseA
			return nil
		}
		scannedB.Store(true)
		return nil
	}))
	bw.ChannelEnabled = func(chID string) bool { return chID != chB.ID || !disabledB.Load() }
	emitted := recordProgress(bw)
	startWorker(t, bw)

	bw.Sweep([]ChannelRef{refFor(chA, 3, false), refFor(chB, 3, false)}, false)
	recvWithin(t, startedA, "A's scan start")
	inflight, _ := inflightState(bw)
	flB := inflight[chB.ID]

	disabledB.Store(true) // the config changes; no sweep has run since
	close(releaseA)
	recvWithin(t, flB.done, "B's queue entry settling")
	assertEmissions(t, emitted, []progressEmission{
		{chA.ID, "", 0, "done"},
		{chB.ID, "", 0, "idle"},
	})
	if scannedB.Load() {
		t.Error("B was disabled before its queued scan started, yet the scan ran")
	}
	assertCursorKept(t, bw, chB.ID)
}
