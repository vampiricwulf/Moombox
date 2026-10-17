package monitor

import (
	"errors"
	"testing"
	"time"
)

// newRowsCall records one OnNewRows invocation.
type newRowsCall struct {
	chID string
	rows int
}

// A scan that INSERTS feed rows calls OnNewRows once, with the count, so
// the host can walk them now rather than a poll interval later. Driven
// through the real scanner and the worker's consumer (runScan is where the
// hook fires): a completing scan, then a forced re-scan that only
// re-sights the same rows, which must stay silent — a sweep-retry or an
// R B over a settled channel has nothing new to walk.
//
// Mutants: the hook never called; called on a scan that inserted nothing;
// the count not reset between scans (the second scan reports the first's).
func TestScan_NewRowsNudgeOnceAndOnlyForInserts(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	page := func() *TabPage {
		return &TabPage{Items: []TabItem{coarseItem("v1", 24*time.Hour), coarseItem("v2", 30*time.Hour)}}
	}
	fetch := newScriptedFetcher(t, map[string][]scriptedPage{
		"videos":  {{wantCont: "", page: page()}, {wantCont: "", page: page()}},
		"streams": {{wantCont: "", page: &TabPage{}}, {wantCont: "", page: &TabPage{}}},
	})
	bw := newTestBackfillWorker(t, db, withTabFetch(fetch.fetch), withBackfillNow(now))
	calls := make(chan newRowsCall, 4)
	bw.OnNewRows = func(chID string, rows int) { calls <- newRowsCall{chID, rows} }
	done := make(chan string, 4)
	bw.OnProgress = func(_, _ string, _ int, state string) {
		if state == "done" {
			done <- state
		}
	}
	startWorker(t, bw)
	ch := backfillTestCh()

	bw.Sweep([]ChannelRef{refFor(ch, 3, false)}, false)
	// The scan's in-flight entry, taken while Sweep's enqueue still holds
	// it (nil only if the scan already ran and unwound). "done" and the
	// nudge fire from inside the scan, ahead of the cleanup that drops the
	// entry, and a forced Sweep skips a channel that still has one — so
	// the re-scan below waits for the entry's done, not for the progress
	// event alone.
	first := func() *inFlight { inflight, _ := inflightState(bw); return inflight[backfillTestChannel] }()
	if got := recvWithin(t, calls, "the first scan's OnNewRows"); got != (newRowsCall{backfillTestChannel, 2}) {
		t.Fatalf("OnNewRows = %+v, want {%s 2}", got, backfillTestChannel)
	}
	recvWithin(t, done, "the first scan's completion")
	if first != nil {
		recvWithin(t, first.done, "the first scan's cleanup")
	}

	// Forced: the channel is complete, so only force re-scans it. Every row
	// is already present — no nudge. A later `now`, as any real re-scan
	// has: UpsertFeedItem tells an insert from a re-sighting by first_seen,
	// which is the scan's own clock.
	bw.now = func() time.Time { return now.Add(time.Hour) }
	bw.Sweep([]ChannelRef{refFor(ch, 3, false)}, true)
	recvWithin(t, done, "the re-scan's completion")
	select {
	case got := <-calls:
		t.Fatalf("OnNewRows = %+v after a re-scan that inserted nothing, want no call", got)
	default:
	}
}

// An INCOMPLETE scan does not nudge, though it wrote rows: the cycle a
// nudge runs sweeps again, and the sweep re-queues a channel whose scan
// ended short, so a tab failing every other page would cost a whole feed
// cycle per page of progress. Its rows wait for the sweep-retry's cycle.
//
// Mutant: the hook fired on the row count alone, whatever the outcome.
func TestScan_IncompleteScanDoesNotNudge(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	fetch := newScriptedFetcher(t, map[string][]scriptedPage{
		"videos":  {{wantCont: "", err: errors.New("browse: 503")}},
		"streams": {{wantCont: "", page: &TabPage{Items: []TabItem{coarseItem("s1", 24*time.Hour)}}}},
	})
	bw := newTestBackfillWorker(t, db, withTabFetch(fetch.fetch), withBackfillNow(now))
	calls := make(chan newRowsCall, 4)
	bw.OnNewRows = func(chID string, rows int) { calls <- newRowsCall{chID, rows} }
	ended := make(chan string, 4)
	bw.OnProgress = func(_, _ string, _ int, state string) {
		if state == "error" {
			ended <- state
		}
	}
	startWorker(t, bw)

	bw.Sweep([]ChannelRef{refFor(backfillTestCh(), 3, false)}, false)
	recvWithin(t, ended, "the incomplete scan's end")
	mustFeedItem(t, db, backfillTestChannel, "s1") // the row IS there; only the nudge is withheld
	select {
	case got := <-calls:
		t.Fatalf("OnNewRows = %+v for an incomplete scan, want no call", got)
	default:
	}
}
