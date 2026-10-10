package monitor

import (
	"context"
	"errors"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// Every scan's context is released when the scan ends, however it ends. The
// context derives from the worker's base context (enqueueLocked), and a
// context.WithCancel child stays registered on its parent until it is
// cancelled — but runScan cancelled none: only a widen, a pause or a prune
// did. Every scan that completed, failed or was skipped left its context on
// the base context for the life of the process, one more per channel per
// rescan. Releasing it must not read as a cancellation to the cleanup that
// clears the cursor: a failed scan resumes from its cursor on the retry, a
// skipped one when its channel is enabled again.
//
// Mutants killed: runScan's cleanup without its fl.cancel() — every row's
// context is still live after the scan ended; the cancel moved ahead of the
// cursor check — the failed and skipped rows' cursor is cleared.
func TestRunScan_ReleasesTheScanContextWhenTheScanEnds(t *testing.T) {
	cases := []struct {
		name    string
		outcome error
		skip    bool
	}{
		{name: "completed", outcome: nil},
		{name: "failed", outcome: errors.New("browse http 503")},
		{name: "skipped: channel disabled since it was queued", skip: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			ch := backfillTestCh()
			if err := db.SaveBackfillCursor(ch.ID, pauseCursor); err != nil {
				t.Fatalf("SaveBackfillCursor: %v", err)
			}
			bw := newTestBackfillWorker(t, db, withScan(func(context.Context, *config.ChannelConfig, string, int, bool) error {
				return tc.outcome
			}))
			bw.ChannelEnabled = func(string) bool { return !tc.skip }

			// Queue before the consumer starts, so the item's context can be
			// read from the queue before anything pops it.
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			bw.mu.Lock()
			bw.baseCtx = ctx
			bw.enqueueLocked(refFor(ch, 3, false))
			item := bw.queue[0]
			bw.baseCtx = nil // Start refuses a worker it has already started
			bw.mu.Unlock()
			bw.Start(ctx)

			recvWithin(t, item.fl.done, "the scan's cleanup")
			if err := item.ctx.Err(); !errors.Is(err, context.Canceled) {
				t.Errorf("the scan's context after the scan ended: Err() = %v, want context.Canceled — a live context stays registered on the base context for the life of the process", err)
			}
			if ctx.Err() != nil {
				t.Fatal("releasing the scan's context cancelled the worker's")
			}
			if tc.outcome == nil && !tc.skip {
				return // a completed scan's cursor is completeScan's business
			}
			if raw, err := db.LoadBackfillCursor(ch.ID); err != nil || raw != pauseCursor {
				t.Errorf("cursor = %q (err %v), want %q kept: releasing the context read as a cancellation", raw, err, pauseCursor)
			}
		})
	}
}
