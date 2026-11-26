package monitor

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// markedItem builds an Age==0 item the listing explicitly labelled live or
// upcoming.
func markedItem(id string) TabItem {
	return TabItem{VideoID: id, Title: id, Marked: true}
}

// A tab that lists only upcoming or live streams — a channel before its debut,
// or one that removes every VOD — is a page the parser read, not a parser
// failure (owner ruling R3-3). The scan completes and the items are stored
// 'assumed', as marked items on a mixed page are.
func TestScanChannel_AllMarkedPageCompletes(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	fetch := newScriptedFetcher(t, map[string][]scriptedPage{
		"videos":  emptyTabScript(),
		"streams": {{wantCont: "", page: &TabPage{Items: []TabItem{markedItem("up1"), markedItem("live2")}}}},
	})
	bw := newTestBackfillWorker(t, db, withTabFetch(fetch.fetch), withBackfillNow(now))

	if err := bw.scanChannel(context.Background(), backfillTestCh(), backfillTestChannel, 3, false); err != nil {
		t.Fatalf("scanChannel: %v (a page of marked live/upcoming items is not a parser failure)", err)
	}
	for _, id := range []string{"up1", "live2"} {
		it := mustFeedItem(t, db, backfillTestChannel, id)
		if it.DatePrecision != "assumed" || it.Published != now.Format(time.RFC3339) {
			t.Errorf("%s = %q/%q, want assumed/published=now", id, it.DatePrecision, it.Published)
		}
	}
	if cb, err := db.GetChannelBackfill(backfillTestChannel); err != nil || cb.At == "" {
		t.Errorf("channel not recorded backfilled (%+v, err %v)", cb, err)
	}
}

// The arm still fires when a page with nothing datable holds even one item the
// listing said nothing about: marked neighbours do not vouch for it.
func TestScanChannel_UnmarkedItemAmongMarkedStillFiresTheArm(t *testing.T) {
	db := newTestDB(t)
	fetch := newScriptedFetcher(t, map[string][]scriptedPage{
		"videos":  emptyTabScript(),
		"streams": {{wantCont: "", page: &TabPage{Items: []TabItem{markedItem("up1"), undatedItem("u2")}}}},
	})
	bw := newTestBackfillWorker(t, db, withTabFetch(fetch.fetch))

	err := bw.scanChannel(context.Background(), backfillTestCh(), backfillTestChannel, 3, false)
	if !errors.Is(err, errUndatablePage) {
		t.Fatalf("scanChannel error = %v, want errUndatablePage", err)
	}
	if got := undatableTabs(err); len(got) != 1 || got[0] != "streams" {
		t.Errorf("undatableTabs = %v, want [streams]", got)
	}
	if it, dbErr := db.GetFeedItem(backfillTestChannel, "up1"); dbErr != nil || it != nil {
		t.Errorf("the failed page's rows were written (%+v, err %v)", it, dbErr)
	}
}

// The parser-failure arm retries by refetching the same page. The first
// failure retries on the next sweep as before; once the same tab has failed
// twice running the channel waits about an hour, with its error state left up
// and nothing emitted by the sweeps that skip it. A manual rescan runs at
// once, and a failure of another kind keeps the every-cycle retry.
func TestSweep_RepeatedParserArmFailuresBackOff(t *testing.T) {
	db := newTestDB(t)
	var clock atomic.Int64
	clock.Store(time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC).Unix())
	var result atomic.Pointer[error]
	armErr := error(&undatableTabError{chID: backfillTestChannel, tab: "streams"})
	result.Store(&armErr)
	scans := make(chan struct{}, 8)
	states := make(chan BackfillState, 8)
	bw := newTestBackfillWorker(t, db, withScan(func(context.Context, *config.ChannelConfig, string, int, bool) error {
		scans <- struct{}{}
		return *result.Load()
	}))
	bw.now = func() time.Time { return time.Unix(clock.Load(), 0).UTC() }
	bw.OnProgress = func(_, _ string, _ int, state BackfillState) { states <- state }
	startWorker(t, bw)
	refs := []ChannelRef{refFor(backfillTestCh(), 3, false)}
	// sweep runs one Sweep and reports whether it scanned, waiting out the
	// scan's cleanup so the next sweep judges a settled channel.
	sweep := func(force bool) bool {
		t.Helper()
		bw.Sweep(refs, force)
		inflight, _ := inflightState(bw)
		if fl := inflight[backfillTestChannel]; fl != nil {
			recvWithin(t, fl.done, "the scan's cleanup")
		}
		select {
		case <-scans:
			if got := recvWithin(t, states, "the scan's state"); got != BackfillError {
				t.Fatalf("scan state = %q, want %q", got, BackfillError)
			}
			return true
		default:
			return false
		}
	}

	// asking sweeps once for the channel with another window or membership
	// eligibility, then puts the fixed request back.
	asking := func(windowDays int, withMembership bool) bool {
		t.Helper()
		refs[0] = refFor(backfillTestCh(), windowDays, withMembership)
		defer func() { refs[0] = refFor(backfillTestCh(), 3, false) }()
		return sweep(false)
	}

	if !sweep(false) {
		t.Fatal("first sweep did not scan")
	}
	if !sweep(false) {
		t.Fatal("the first arm failure must still retry on the next sweep")
	}
	if sweep(false) {
		t.Fatal("a tab the arm stopped twice running was rescanned on the next sweep")
	}
	select {
	case s := <-states:
		t.Fatalf("a skipped sweep emitted %q; the error state must stay up", s)
	default:
	}
	clock.Add(int64(parserArmRetryInterval/time.Second) - 1)
	if sweep(false) {
		t.Fatal("rescanned before the retry interval ran out")
	}
	clock.Add(1)
	if !sweep(false) {
		t.Fatal("not rescanned once the retry interval ran out")
	}
	if sweep(false) {
		t.Fatal("a third failure running must wait again")
	}

	// A config change that asks more of the channel ends the wait: a wider
	// window, or a membership tab newly eligible, is another scan. The count
	// starts over, so the wait is back after two more failures. A narrower
	// window is the scan that failed, and waits.
	if asking(2, false) {
		t.Fatal("a narrowed window must keep the wait")
	}
	if !asking(7, false) {
		t.Fatal("a widened window must scan at once")
	}
	if !asking(7, false) {
		t.Fatal("the widened scan's first failure must retry on the next sweep")
	}
	if asking(7, false) {
		t.Fatal("the widened scan failed twice running and must wait")
	}
	if !asking(7, true) {
		t.Fatal("a membership tab newly eligible must scan at once")
	}
	if !asking(7, true) || asking(7, true) {
		t.Fatal("the membership scan must retry once and then wait")
	}
	if asking(3, false) {
		t.Fatal("asking for less than the scan that failed must keep the wait")
	}
	if !sweep(true) {
		t.Fatal("a manual rescan must run at once")
	}

	// Another kind of failure is not the arm's: it is retried on the next
	// sweep and starts the arm's count over, so the arm failure after it is
	// a first one again (the forced scan above left the count at one).
	fetchErr := errors.New("page fetch failed")
	result.Store(&fetchErr)
	if !sweep(false) {
		t.Fatal("the sweep after one arm failure did not scan")
	}
	result.Store(&armErr)
	if !sweep(false) {
		t.Fatal("a fetch failure must be retried on the next sweep")
	}
	if !sweep(false) {
		t.Fatal("an arm failure that follows a fetch failure is a first one and retries on the next sweep")
	}
}

// A backed-off channel whose new request no longer includes the tab that
// failed does not wait (owner ruling RV-9). The membership tab alone stopped
// the arm twice running; membership discovery is then turned off (or the
// cookies stop being eligible), so the scan now asked would never fetch the
// failing page. It runs on the next sweep and completes, instead of leaving
// the error state up for the rest of the hour. The test above keeps the
// other side: a request without membership still waits when a tab every scan
// fetches is the one that failed.
//
// Mutant: the asksFailingTab clause dropped from Sweep's wait (the last
// sweep scans nothing).
func TestSweep_BackOffEndsWhenTheFailingTabIsNoLongerAsked(t *testing.T) {
	db := newTestDB(t)
	scans := make(chan bool, 8)
	states := make(chan BackfillState, 8)
	bw := newTestBackfillWorker(t, db, withScan(func(_ context.Context, _ *config.ChannelConfig, _ string, _ int, withMembership bool) error {
		scans <- withMembership
		if withMembership {
			return &undatableTabError{chID: backfillTestChannel, tab: "membership"}
		}
		return nil
	}))
	bw.OnProgress = func(_, _ string, _ int, state BackfillState) { states <- state }
	startWorker(t, bw)
	// sweep runs one Sweep and returns the state its scan ended in, "" when
	// it scanned nothing.
	sweep := func(withMembership bool) BackfillState {
		t.Helper()
		bw.Sweep([]ChannelRef{refFor(backfillTestCh(), 7, withMembership)}, false)
		inflight, _ := inflightState(bw)
		if fl := inflight[backfillTestChannel]; fl != nil {
			recvWithin(t, fl.done, "the scan's cleanup")
		}
		select {
		case asked := <-scans:
			if asked != withMembership {
				t.Fatalf("scan asked with membership = %v, want %v", asked, withMembership)
			}
			return recvWithin(t, states, "the scan's state")
		default:
			return ""
		}
	}

	if got := sweep(true); got != BackfillError {
		t.Fatalf("first scan state = %q, want %q", got, BackfillError)
	}
	if got := sweep(true); got != BackfillError {
		t.Fatalf("second scan state = %q, want %q", got, BackfillError)
	}
	if got := sweep(true); got != "" {
		t.Fatalf("the membership tab failed twice running and the same request scanned again (%q)", got)
	}
	if got := sweep(false); got != BackfillDone {
		t.Fatalf("a request without the failing membership tab ended %q, want it scanned at once and %q", got, BackfillDone)
	}
}
