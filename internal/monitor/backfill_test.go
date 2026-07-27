package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// ---- test scaffolding ------------------------------------------------------

// backfillOpt configures a BackfillWorker built by newTestBackfillWorker —
// the same option shape feedMonitorOpt uses for FeedMonitor (feed_test.go).
type backfillOpt func(*BackfillWorker)

// withTabFetch injects a scripted tab-page fetcher in place of the real
// youtube.Service.FetchChannelTabPage wiring, via the FetchTabPage seam.
func withTabFetch(fn TabPageFetchFunc) backfillOpt {
	return func(bw *BackfillWorker) { bw.FetchTabPage = fn }
}

// withBackfillNow pins bw.now to a fixed instant. scanChannel reads it
// exactly once per scan (the one-`now` rule), so this makes the coarse
// (now - Age) and assumed (now) date math deterministic in tests.
func withBackfillNow(t time.Time) backfillOpt {
	return func(bw *BackfillWorker) { bw.now = func() time.Time { return t } }
}

// newTestBackfillWorker builds a BackfillWorker over a real (temp-file) db,
// paging instantly (pageInterval 0 — the 1 page/sec production throttle
// would make every multi-page test take seconds).
func newTestBackfillWorker(t *testing.T, db *database.Database, opts ...backfillOpt) *BackfillWorker {
	t.Helper()
	bw := NewBackfillWorker(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	bw.pageInterval = 0
	for _, opt := range opts {
		opt(bw)
	}
	return bw
}

// fetchCall records one FetchTabPage invocation.
type fetchCall struct {
	tab          string
	continuation string
}

// scriptedPage is one expected FetchTabPage call for a tab: the continuation
// the scanner MUST pass (asserted — this is how the resume test pins the
// cursor round-trip) and either the page to return or an error.
type scriptedPage struct {
	wantCont string
	page     *TabPage
	err      error
}

// scriptedFetcher serves per-tab page scripts front-to-back and records every
// call. A call against an exhausted (or absent) tab script is a test failure:
// tests express "this tab must not be fetched (again)" by simply not
// scripting the page.
type scriptedFetcher struct {
	t     *testing.T
	mu    sync.Mutex
	pages map[string][]scriptedPage
	calls []fetchCall
}

func newScriptedFetcher(t *testing.T, pages map[string][]scriptedPage) *scriptedFetcher {
	return &scriptedFetcher{t: t, pages: pages}
}

func (f *scriptedFetcher) fetch(_ context.Context, _, tab, continuation string) (*TabPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fetchCall{tab: tab, continuation: continuation})
	script := f.pages[tab]
	if len(script) == 0 {
		f.t.Errorf("unexpected fetch: tab=%q continuation=%q (script exhausted)", tab, continuation)
		return nil, fmt.Errorf("unexpected fetch for tab %q", tab)
	}
	next := script[0]
	f.pages[tab] = script[1:]
	if continuation != next.wantCont {
		f.t.Errorf("tab %q: fetched with continuation %q, want %q", tab, continuation, next.wantCont)
	}
	if next.err != nil {
		return nil, next.err
	}
	return next.page, nil
}

// tabCalls returns how many times tab was fetched.
func (f *scriptedFetcher) tabCalls(tab string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.tab == tab {
			n++
		}
	}
	return n
}

func (f *scriptedFetcher) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// coarseItem builds a datable tab item ("Streamed <age> ago" ⇒ Age > 0).
func coarseItem(id string, age time.Duration) TabItem {
	return TabItem{VideoID: id, Title: id, Age: age}
}

// undatedItem builds an Age==0 item — a live/upcoming badge or an item the
// parser could not date; the scanner cannot tell which (spec §11).
func undatedItem(id string) TabItem {
	return TabItem{VideoID: id, Title: id, Age: 0}
}

// emptyTabScript is the one-page script of a tab with no content at all:
// zero items, no continuation — natural exhaustion.
func emptyTabScript() []scriptedPage {
	return []scriptedPage{{wantCont: "", page: &TabPage{}}}
}

// loadTestCursor parses channel_state.backfill_state for chID.
func loadTestCursor(t *testing.T, db *database.Database, chID string) *backfillCursor {
	t.Helper()
	raw, err := db.LoadBackfillCursor(chID)
	if err != nil {
		t.Fatalf("LoadBackfillCursor: %v", err)
	}
	if raw == "" {
		t.Fatalf("no backfill cursor saved for %s", chID)
	}
	cur := &backfillCursor{}
	if err := json.Unmarshal([]byte(raw), cur); err != nil {
		t.Fatalf("cursor unmarshal: %v (raw %q)", err, raw)
	}
	return cur
}

// mustFeedItem fails the test unless (chID, videoID) exists, returning the row.
func mustFeedItem(t *testing.T, db *database.Database, chID, videoID string) *database.FeedItem {
	t.Helper()
	it, err := db.GetFeedItem(chID, videoID)
	if err != nil {
		t.Fatalf("GetFeedItem(%s): %v", videoID, err)
	}
	if it == nil {
		t.Fatalf("feed item %s not persisted", videoID)
	}
	return it
}

const backfillTestChannel = "UCbackfilltest000000000a"

func backfillTestCh() *config.ChannelConfig {
	return &config.ChannelConfig{ID: backfillTestChannel, Name: "Backfill Test"}
}

// ---- the five scanner tests (Plan 5 Task 2 brief) --------------------------

// (a) Page-granular window stop: a page MIXED in/out-of-window keeps paging;
// the first page whose items are ALL older than the window stops the tab —
// and the rows of BOTH pages are persisted (write per page, no buffering).
func TestScanChannel_PageGranularWindowStop(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	fetch := newScriptedFetcher(t, map[string][]scriptedPage{
		"videos": {
			// page 1: v1 inside the 3-day window, v2 outside ⇒ mixed ⇒ keep paging.
			{wantCont: "", page: &TabPage{
				Items:        []TabItem{coarseItem("v1", 24*time.Hour), coarseItem("v2", 240*time.Hour)},
				Continuation: "TOK2",
			}},
			// page 2: ALL items outside ⇒ arm (a) fires; TOK3 must never be followed.
			{wantCont: "TOK2", page: &TabPage{
				Items:        []TabItem{coarseItem("v3", 264*time.Hour), coarseItem("v4", 288*time.Hour)},
				Continuation: "TOK3",
			}},
		},
		"streams": emptyTabScript(),
	})
	bw := newTestBackfillWorker(t, db, withTabFetch(fetch.fetch), withBackfillNow(now))

	if err := bw.scanChannel(context.Background(), backfillTestCh(), backfillTestChannel, 3, false); err != nil {
		t.Fatalf("scanChannel: %v", err)
	}

	if got := fetch.tabCalls("videos"); got != 2 {
		t.Errorf("videos tab fetched %d times, want 2 (arm (a) must stop before TOK3)", got)
	}

	// Rows from BOTH pages persisted, with the §11 classification. The
	// catalog_pos values are post-ordering-pass — here identical to the
	// provisional per-tab index, because the single tab listed newest-first
	// (v1 > v2 > v3 > v4 by published, same order the pages arrived in).
	for i, id := range []string{"v1", "v2", "v3", "v4"} {
		it := mustFeedItem(t, db, backfillTestChannel, id)
		if it.DatePrecision != "coarse" {
			t.Errorf("%s precision = %q, want coarse", id, it.DatePrecision)
		}
		if it.Source != "videos" {
			t.Errorf("%s source = %q, want videos", id, it.Source)
		}
		if it.Status != "unknown" {
			t.Errorf("%s status = %q, want unknown", id, it.Status)
		}
		if it.CatalogPos != i {
			t.Errorf("%s catalog_pos = %d, want %d", id, it.CatalogPos, i)
		}
	}
	if it := mustFeedItem(t, db, backfillTestChannel, "v1"); it.Published != now.Add(-24*time.Hour).Format(time.RFC3339) {
		t.Errorf("v1 published = %q, want now-24h", it.Published)
	}

	// Both tabs ended cleanly ⇒ the scan completed: backfilled_at is set and
	// the cursor is cleared (Task 3 — the deep completion assertions live in
	// TestScanChannel_CompletionOrdersAndRecords).
	if cb, err := db.GetChannelBackfill(backfillTestChannel); err != nil || cb.At == "" {
		t.Errorf("backfilled_at = %q (err %v), want set on a clean scan", cb.At, err)
	}
	if raw, err := db.LoadBackfillCursor(backfillTestChannel); err != nil || raw != "" {
		t.Errorf("backfill_state = %q (err %v), want cleared on completion", raw, err)
	}
}

// (b) Parser-failure arm: a NON-EMPTY page with no datable item stops the tab
// after that ONE page, the scan reports incomplete (non-nil error — Task 3
// must not set backfilled_at), the page's rows are NOT persisted and the
// cursor does NOT advance past the failed page, so the sweep-retry refetches
// it once the parser is fixed. Other tabs still scan — tab failures are
// independent.
func TestScanChannel_ParserFailureArm(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	fetch := newScriptedFetcher(t, map[string][]scriptedPage{
		"videos": {
			// One scripted page only: a second videos fetch would fail the test
			// (script exhausted) — "stops after one full undatable page".
			{wantCont: "", page: &TabPage{
				Items:        []TabItem{undatedItem("u1"), undatedItem("u2")},
				Continuation: "TOK2",
			}},
		},
		"streams": emptyTabScript(),
	})
	bw := newTestBackfillWorker(t, db, withTabFetch(fetch.fetch), withBackfillNow(now))

	err := bw.scanChannel(context.Background(), backfillTestCh(), backfillTestChannel, 3, false)
	if err == nil {
		t.Fatal("scanChannel = nil, want parser-failure error (completion must NOT be reported)")
	}
	if !errors.Is(err, errUndatablePage) {
		t.Errorf("scanChannel error = %v, want errUndatablePage", err)
	}

	// The undatable page's rows are garbage dates (published=now for the whole
	// back catalogue) — they must not enter the store.
	for _, id := range []string{"u1", "u2"} {
		if it, dbErr := db.GetFeedItem(backfillTestChannel, id); dbErr != nil || it != nil {
			t.Errorf("undatable item %s persisted (%+v, err %v), want absent", id, it, dbErr)
		}
	}

	cur := loadTestCursor(t, db, backfillTestChannel)
	if tc := cur.Tabs["videos"]; tc != nil && (tc.Done || tc.Continuation != "") {
		t.Errorf("videos cursor advanced past the failed page: %+v", tc)
	}
	if tc := cur.Tabs["streams"]; tc == nil || !tc.Done {
		t.Errorf("streams tab should have completed independently: %+v", tc)
	}
}

// (c) Resume: the cursor is saved after every page, so a NEW scanner over the
// same DB resumes the interrupted tab at its saved continuation (asserted by
// the stub) instead of page 1 — the carried ruling: transient-fetch recovery
// is cursor + sweep retry, never an in-scanner retry loop.
func TestScanChannel_ResumesFromCursor(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

	fetch1 := newScriptedFetcher(t, map[string][]scriptedPage{
		"videos": {
			{wantCont: "", page: &TabPage{
				Items:        []TabItem{coarseItem("v1", 24*time.Hour), coarseItem("v2", 30*time.Hour)},
				Continuation: "TOK2",
			}},
			// page 2 fails mid-scan (transient) — the cursor already holds TOK2.
			{wantCont: "TOK2", err: errors.New("browse http 503")},
		},
		"streams": emptyTabScript(),
	})
	bw1 := newTestBackfillWorker(t, db, withTabFetch(fetch1.fetch), withBackfillNow(now))
	if err := bw1.scanChannel(context.Background(), backfillTestCh(), backfillTestChannel, 30, false); err == nil {
		t.Fatal("scanChannel = nil, want transient fetch error")
	}

	cur := loadTestCursor(t, db, backfillTestChannel)
	if tc := cur.Tabs["videos"]; tc == nil || tc.Done || tc.Continuation != "TOK2" || tc.NextPos != 2 {
		t.Fatalf("videos cursor after failure = %+v, want continuation TOK2, next_pos 2, not done", tc)
	}

	// New scanner, same DB. The videos script's wantCont pins the resume token;
	// streams has NO script — its cursor is done, so fetching it again would
	// fail the test.
	fetch2 := newScriptedFetcher(t, map[string][]scriptedPage{
		"videos": {
			{wantCont: "TOK2", page: &TabPage{
				Items:        []TabItem{coarseItem("v3", 26*30*time.Hour)},
				Continuation: "TOK3",
			}},
		},
	})
	bw2 := newTestBackfillWorker(t, db, withTabFetch(fetch2.fetch), withBackfillNow(now))
	if err := bw2.scanChannel(context.Background(), backfillTestCh(), backfillTestChannel, 30, false); err != nil {
		t.Fatalf("resumed scanChannel: %v", err)
	}

	// v3's final position is 2 — both readings agree here: the provisional
	// per-tab index resumed at 2 after v1/v2 (pinned via next_pos above), and
	// the completed scan's ordering pass keeps it at 2 (v3 is the oldest of
	// the three by published).
	if it := mustFeedItem(t, db, backfillTestChannel, "v3"); it.CatalogPos != 2 {
		t.Errorf("v3 catalog_pos = %d, want 2", it.CatalogPos)
	}
}

// (d) An Age==0 item (live badge) AMONG datable items is stored as
// assumed/unknown and does NOT trigger arm (b) — the arm needs a whole
// non-empty page with no datable item.
func TestScanChannel_LiveItemAmongDatable(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	fetch := newScriptedFetcher(t, map[string][]scriptedPage{
		"videos": {
			{wantCont: "", page: &TabPage{
				Items: []TabItem{undatedItem("live1"), coarseItem("v2", 2*time.Hour)},
			}},
		},
		"streams": emptyTabScript(),
	})
	bw := newTestBackfillWorker(t, db, withTabFetch(fetch.fetch), withBackfillNow(now))

	if err := bw.scanChannel(context.Background(), backfillTestCh(), backfillTestChannel, 3, false); err != nil {
		t.Fatalf("scanChannel: %v (a live item among datable items is not a parser failure)", err)
	}

	live := mustFeedItem(t, db, backfillTestChannel, "live1")
	if live.DatePrecision != "assumed" || live.Published != now.Format(time.RFC3339) {
		t.Errorf("live1 = %q/%q, want assumed/published=now", live.DatePrecision, live.Published)
	}
	if live.Status != "unknown" {
		t.Errorf("live1 status = %q, want unknown", live.Status)
	}
	v2 := mustFeedItem(t, db, backfillTestChannel, "v2")
	if v2.DatePrecision != "coarse" || v2.Published != now.Add(-2*time.Hour).Format(time.RFC3339) {
		t.Errorf("v2 = %q/%q, want coarse/published=now-2h", v2.DatePrecision, v2.Published)
	}
	if got := fetch.tabCalls("videos"); got != 1 {
		t.Errorf("videos tab fetched %d times, want 1", got)
	}
}

// ---- the two completion tests (Plan 5 Task 3 brief) -------------------------

// (a/Task 3) Completion: when every eligible tab ends cleanly, the ordering
// pass renumbers catalog_pos channel-globally over the DEDUPED row set by
// (published DESC, provisional pos ASC, video_id ASC), and the completion
// record is written — backfilled_at / backfilled_window_days /
// backfilled_with_membership all set, backfill_state cleared in the same
// statement (§11: the cursor's lifecycle ends at completion).
func TestScanChannel_CompletionOrdersAndRecords(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	fetch := newScriptedFetcher(t, map[string][]scriptedPage{
		// One page per tab, no continuation — natural exhaustion, all clean.
		// "vid-new" appears in BOTH tabs (a past stream), so the deduped set
		// is 5 rows, not 6; the streams upsert overwrites its provisional pos
		// to 1. The two tie groups pin the lower sort-key arms:
		//   published now-10h: tie-pos-z (streams pos 0) vs tie-pos-b (videos
		//     pos 1) ⇒ provisional pos ASC must win over video_id ASC
		//     (alphabetically "tie-pos-b" would come first).
		//   published now-20h: tie-id-a (streams pos 2) vs tie-id-b (videos
		//     pos 2) ⇒ equal provisional pos falls through to video_id ASC.
		"videos": {
			{wantCont: "", page: &TabPage{Items: []TabItem{
				coarseItem("vid-new", 1*time.Hour),
				coarseItem("tie-pos-b", 10*time.Hour),
				coarseItem("tie-id-b", 20*time.Hour),
			}}},
		},
		"streams": {
			{wantCont: "", page: &TabPage{Items: []TabItem{
				coarseItem("tie-pos-z", 10*time.Hour),
				coarseItem("vid-new", 1*time.Hour),
				coarseItem("tie-id-a", 20*time.Hour),
			}}},
		},
	})
	bw := newTestBackfillWorker(t, db, withTabFetch(fetch.fetch), withBackfillNow(now))

	if err := bw.scanChannel(context.Background(), backfillTestCh(), backfillTestChannel, 3, false); err != nil {
		t.Fatalf("scanChannel: %v", err)
	}

	// Channel-global catalog_pos over the deduped set, by the §11 sort key.
	want := map[string]int{
		"vid-new":   0, // newest (now-1h)
		"tie-pos-z": 1, // now-10h, provisional pos 0 beats...
		"tie-pos-b": 2, // ...pos 1, despite "b" < "z"
		"tie-id-a":  3, // now-20h, equal provisional pos ⇒ video_id ASC
		"tie-id-b":  4,
	}
	for id, pos := range want {
		if it := mustFeedItem(t, db, backfillTestChannel, id); it.CatalogPos != pos {
			t.Errorf("%s catalog_pos = %d, want %d", id, it.CatalogPos, pos)
		}
	}

	// The completion record: all three backfilled_* columns set. ts is the
	// scan's one `now`; with_membership records the scan's eligibility (false
	// here — written as 0, NOT left NULL: Task 4's sweep arm depends on the
	// distinction).
	cb, err := db.GetChannelBackfill(backfillTestChannel)
	if err != nil {
		t.Fatalf("GetChannelBackfill: %v", err)
	}
	if cb.At != now.Format(time.RFC3339) {
		t.Errorf("backfilled_at = %q, want %q (the scan's one now)", cb.At, now.Format(time.RFC3339))
	}
	if cb.WindowDays == nil || *cb.WindowDays != 3 {
		t.Errorf("backfilled_window_days = %v, want 3", cb.WindowDays)
	}
	if cb.WithMembership == nil || *cb.WithMembership {
		t.Errorf("backfilled_with_membership = %v, want false (set, not NULL)", cb.WithMembership)
	}
	// ...and the cursor cleared with it (§11 lifecycle: a completed scan's
	// stale continuation token must not leak into the next one).
	if raw, err := db.LoadBackfillCursor(backfillTestChannel); err != nil || raw != "" {
		t.Errorf("backfill_state = %q (err %v), want cleared on completion", raw, err)
	}
}

// (b/Task 3) An arm-(b) (parser-failure) tab blocks completion: no renumber
// (persisted rows keep their provisional per-tab catalog_pos), no completion
// record (every backfilled_* column stays NULL), and the cursor stays saved
// for the sweep-retry.
func TestScanChannel_IncompleteTabBlocksCompletion(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	fetch := newScriptedFetcher(t, map[string][]scriptedPage{
		"videos": {
			// Page 1 persists two rows OUT of published order (v2 is newer but
			// sits at provisional pos 1) — a renumber would flip them to 1/0,
			// so the 0/1 assertions below are real evidence none ran.
			{wantCont: "", page: &TabPage{
				Items:        []TabItem{coarseItem("v1", 5*time.Hour), coarseItem("v2", 1*time.Hour)},
				Continuation: "TOK2",
			}},
			// Page 2: non-empty with no datable item ⇒ arm (b), tab incomplete.
			{wantCont: "TOK2", page: &TabPage{
				Items:        []TabItem{undatedItem("u1")},
				Continuation: "TOK3",
			}},
		},
		"streams": emptyTabScript(),
	})
	bw := newTestBackfillWorker(t, db, withTabFetch(fetch.fetch), withBackfillNow(now))

	err := bw.scanChannel(context.Background(), backfillTestCh(), backfillTestChannel, 3, false)
	if !errors.Is(err, errUndatablePage) {
		t.Fatalf("scanChannel error = %v, want errUndatablePage", err)
	}

	// No renumber: the provisional per-tab positions are untouched.
	if it := mustFeedItem(t, db, backfillTestChannel, "v1"); it.CatalogPos != 0 {
		t.Errorf("v1 catalog_pos = %d, want provisional 0 (no renumber on incomplete scan)", it.CatalogPos)
	}
	if it := mustFeedItem(t, db, backfillTestChannel, "v2"); it.CatalogPos != 1 {
		t.Errorf("v2 catalog_pos = %d, want provisional 1 (no renumber on incomplete scan)", it.CatalogPos)
	}

	// No completion record: every backfilled_* column still NULL.
	cb, err := db.GetChannelBackfill(backfillTestChannel)
	if err != nil {
		t.Fatalf("GetChannelBackfill: %v", err)
	}
	if cb.At != "" || cb.WindowDays != nil || cb.WithMembership != nil {
		t.Errorf("completion record written on incomplete scan: %+v", cb)
	}

	// The cursor survives — the sweep-retry resumes from it.
	cur := loadTestCursor(t, db, backfillTestChannel)
	if tc := cur.Tabs["videos"]; tc == nil || tc.Done || tc.Continuation != "TOK2" {
		t.Errorf("videos cursor = %+v, want continuation TOK2, not done", tc)
	}
	if tc := cur.Tabs["streams"]; tc == nil || !tc.Done {
		t.Errorf("streams tab should have completed independently: %+v", tc)
	}
}

// (e) Empty channel: every tab returns zero items and no continuation —
// NEITHER arm. All tabs complete CLEANLY, the ordering pass runs over zero
// rows, and backfilled_at is set — establishing the channel (its RSS may 404
// forever) for ~3 requests total (spec §11). Misreading the parser arm as
// vacuously true of an empty page would instead report failure and rescan
// empty channels every cycle, forever; not-rescanning-after-completion itself
// is the sweep's backfilled_at arm (Task 4), which never re-invokes a
// completed channel's scan.
func TestScanChannel_EmptyChannelCompletesCleanly(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	fetch := newScriptedFetcher(t, map[string][]scriptedPage{
		"videos":     emptyTabScript(),
		"streams":    emptyTabScript(),
		"membership": emptyTabScript(),
	})
	bw := newTestBackfillWorker(t, db, withTabFetch(fetch.fetch), withBackfillNow(now))

	if err := bw.scanChannel(context.Background(), backfillTestCh(), backfillTestChannel, 3, true); err != nil {
		t.Fatalf("scanChannel on empty channel: %v (empty is natural exhaustion, not a parser failure)", err)
	}
	if got := fetch.totalCalls(); got != 3 {
		t.Errorf("empty channel took %d requests, want 3 (one page per tab)", got)
	}

	// Completion over zero rows: the record is written (all three columns,
	// with_membership true — all three tabs were eligible) and the cursor is
	// cleared. This is the established gate's second key for a channel whose
	// public feed legitimately carries nothing.
	cb, err := db.GetChannelBackfill(backfillTestChannel)
	if err != nil {
		t.Fatalf("GetChannelBackfill: %v", err)
	}
	if cb.At != now.Format(time.RFC3339) {
		t.Errorf("backfilled_at = %q, want %q (empty channel must complete and establish)", cb.At, now.Format(time.RFC3339))
	}
	if cb.WindowDays == nil || *cb.WindowDays != 3 {
		t.Errorf("backfilled_window_days = %v, want 3", cb.WindowDays)
	}
	if cb.WithMembership == nil || !*cb.WithMembership {
		t.Errorf("backfilled_with_membership = %v, want true", cb.WithMembership)
	}
	if raw, err := db.LoadBackfillCursor(backfillTestChannel); err != nil || raw != "" {
		t.Errorf("backfill_state = %q (err %v), want cleared on completion", raw, err)
	}
}

// ---- Task 4 scaffolding: sweep / in-flight / prune --------------------------

// withScan replaces the worker's scan entrypoint (bw.scanChannel by default)
// with a stub, via the scan seam — the sweep tests script scan OUTCOMES; the
// scanner's own behavior is pinned by the scanChannel tests above.
func withScan(fn scanFunc) backfillOpt {
	return func(bw *BackfillWorker) { bw.scan = fn }
}

// startWorker runs the worker's single serial consumer for the test's
// lifetime.
func startWorker(t *testing.T, bw *BackfillWorker) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	bw.Start(ctx)
}

// refFor builds the resolved ChannelRef the host wiring (services.go) would:
// WindowDays and WithMembership are CALLER-resolved values.
func refFor(ch *config.ChannelConfig, windowDays int, withMembership bool) ChannelRef {
	return ChannelRef{Ch: ch, ChID: ch.ID, WindowDays: windowDays, WithMembership: withMembership}
}

// recvWithin receives from ch or fails the test after a generous timeout —
// scans run on the worker goroutine, so every expectation is a channel wait,
// never a sleep.
func recvWithin[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// inflightState snapshots the in-flight set and queue depth. Sweep's
// enqueue decisions are synchronous, so tests can assert "nothing was
// enqueued" deterministically right after a Sweep returns.
func inflightState(bw *BackfillWorker) (inflight map[string]*inFlight, queued int) {
	bw.mu.Lock()
	defer bw.mu.Unlock()
	return maps.Clone(bw.inflight), len(bw.queue)
}

// scanCall records one stub-scanner invocation.
type scanCall struct {
	chID           string
	windowDays     int
	withMembership bool
}

const backfillTestChannel2 = "UCbackfilltest000000000b"

// ---- the five sweep tests (Plan 5 Task 4 brief) -----------------------------

// The §11 sweep condition's four DB arms as a pure predicate, with SQL NULL
// semantics pinned: a missing channel_state row reads all-NULL (never
// backfilled); NULL < x is NULL, not true — without the second arm the widen
// check never fires over a NULL window; NULL = 0 is NULL — a NULL
// with_membership never triggers the membership arm on its own. Toggle-OFF
// has no arm at all: a completed with-membership scan is a superset of what
// is now eligible.
func TestNeedsBackfill_FourArms(t *testing.T) {
	iptr := func(v int) *int { return &v }
	bptr := func(v bool) *bool { return &v }
	cases := []struct {
		name     string
		cb       database.ChannelBackfill
		window   int
		eligible bool
		want     bool
	}{
		{"arm 1: missing row / backfilled_at NULL", database.ChannelBackfill{}, 3, false, true},
		{"arm 2: window NULL (NULL < x is NULL, not true)",
			database.ChannelBackfill{At: "2026-07-15T12:00:00Z", WithMembership: bptr(false)}, 3, false, true},
		{"arm 3: widen 3 -> 30",
			database.ChannelBackfill{At: "2026-07-15T12:00:00Z", WindowDays: iptr(3), WithMembership: bptr(false)}, 30, false, true},
		{"arm 4: membership newly eligible",
			database.ChannelBackfill{At: "2026-07-15T12:00:00Z", WindowDays: iptr(30), WithMembership: bptr(false)}, 30, true, true},
		{"narrow 30 -> 3 does NOT fire",
			database.ChannelBackfill{At: "2026-07-15T12:00:00Z", WindowDays: iptr(30), WithMembership: bptr(false)}, 3, false, false},
		{"membership toggle-off does NOT fire",
			database.ChannelBackfill{At: "2026-07-15T12:00:00Z", WindowDays: iptr(30), WithMembership: bptr(true)}, 30, false, false},
		{"completed with membership, still eligible: no-op",
			database.ChannelBackfill{At: "2026-07-15T12:00:00Z", WindowDays: iptr(30), WithMembership: bptr(true)}, 30, true, false},
		{"membership NULL never fires the membership arm (NULL = 0 is NULL)",
			database.ChannelBackfill{At: "2026-07-15T12:00:00Z", WindowDays: iptr(30)}, 30, true, false},
	}
	for _, tc := range cases {
		if got := needsBackfill(tc.cb, tc.window, tc.eligible); got != tc.want {
			t.Errorf("%s: needsBackfill = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// (a) A fresh channel with NO channel_state row at all IS swept (LEFT-JOIN
// semantics: a missing row reads as never-backfilled, not "no rows
// returned") — and, the carried Task 2/3 pin: once backfilled_at is set and
// neither the window nor membership eligibility changed, NO sweep ever
// re-invokes the scanner again. kickMonitors fires on every add / remove /
// reorder / bulk PUT / TUI save with no discrimination, so this no-op IS the
// idempotence the funnel relies on.
func TestSweep_FreshChannelScansOnce_CompletedNeverRescanned(t *testing.T) {
	db := newTestDB(t)
	calls := make(chan scanCall, 8)
	bw := newTestBackfillWorker(t, db, withScan(func(_ context.Context, _ *config.ChannelConfig, chID string, wd int, wm bool) error {
		calls <- scanCall{chID, wd, wm}
		return nil
	}))
	startWorker(t, bw)
	ch := backfillTestCh()
	refs := []ChannelRef{refFor(ch, 3, false)}

	bw.Sweep(refs, false)
	if got := recvWithin(t, calls, "fresh channel's scan"); got != (scanCall{backfillTestChannel, 3, false}) {
		t.Fatalf("scan invoked with %+v, want {%s 3 false}", got, backfillTestChannel)
	}

	// Completed — as Task 3's completeScan records it.
	if err := db.SetChannelBackfilled(ch.ID, 3, false, "2026-07-15T12:00:00Z"); err != nil {
		t.Fatalf("SetChannelBackfilled: %v", err)
	}

	// Repeated sweeps with unchanged conditions enqueue NOTHING — assert
	// synchronously (Sweep's decisions happen before it returns).
	for range 3 {
		bw.Sweep(refs, false)
	}
	if inflight, queued := inflightState(bw); len(inflight) != 0 || queued != 0 {
		t.Fatalf("completed channel re-enqueued: inflight=%d queued=%d, want 0/0", len(inflight), queued)
	}

	// FIFO cross-check: sweep the completed channel ALONGSIDE a fresh one.
	// The queue is strictly serial and FIFO, so if the completed channel had
	// been (wrongly) enqueued — in this sweep or any earlier one — its scan
	// would arrive before the fresh channel's.
	ch2 := &config.ChannelConfig{ID: backfillTestChannel2, Name: "Fresh Two"}
	bw.Sweep([]ChannelRef{refFor(ch, 3, false), refFor(ch2, 3, false)}, false)
	if got := recvWithin(t, calls, "second fresh channel's scan"); got.chID != backfillTestChannel2 {
		t.Fatalf("scan invoked for %q, want %q (completed channel must NEVER be re-invoked)", got.chID, backfillTestChannel2)
	}
	select {
	case got := <-calls:
		t.Fatalf("unexpected extra scan %+v after the fresh channel's", got)
	default:
	}
}

// (b, positive) A widen 3 -> 30 mid-scan cancels the running scan, RESETS the
// per-tab cursor, and restarts deeper. Mid-scan, backfilled_at IS NULL makes
// the DB condition trivially true — the in-flight entry's recorded
// windowDays is the ONLY signal a running scan is stale (§11).
func TestSweep_WidenMidScanCancelsResetsCursorAndRestartsDeeper(t *testing.T) {
	db := newTestDB(t)
	started := make(chan int, 4)
	firstCancelled := make(chan struct{})
	bw := newTestBackfillWorker(t, db, withScan(func(ctx context.Context, _ *config.ChannelConfig, _ string, wd int, _ bool) error {
		started <- wd
		if wd == 3 {
			<-ctx.Done()
			close(firstCancelled)
			return ctx.Err()
		}
		return nil
	}))
	startWorker(t, bw)
	ch := backfillTestCh()

	// A leftover shallow cursor the widen MUST reset: a deeper rescan
	// resuming it would skip exactly the pages it was restarted to fetch.
	if err := db.SaveBackfillCursor(ch.ID, `{"tabs":{"videos":{"continuation":"TOK2","next_pos":2}}}`); err != nil {
		t.Fatalf("SaveBackfillCursor: %v", err)
	}

	bw.Sweep([]ChannelRef{refFor(ch, 3, false)}, false)
	if wd := recvWithin(t, started, "first scan"); wd != 3 {
		t.Fatalf("first scan windowDays = %d, want 3", wd)
	}

	bw.Sweep([]ChannelRef{refFor(ch, 30, false)}, false)
	recvWithin(t, firstCancelled, "first scan's cancellation")
	if wd := recvWithin(t, started, "restarted (deeper) scan"); wd != 30 {
		t.Fatalf("restarted scan windowDays = %d, want 30", wd)
	}
	// The restart began, so the cancelled scan's cleanup already ran — the
	// shallow cursor must be gone (§11: resume applies only to INTERRUPTED
	// scans, never cancelled ones).
	if raw, err := db.LoadBackfillCursor(ch.ID); err != nil || raw != "" {
		t.Errorf("cursor = %q (err %v), want reset on widen-cancel", raw, err)
	}
}

// (b, membership variant) Membership becoming eligible mid-scan restarts the
// running scan the same way a widen does — the in-flight entry's recorded
// withMembership is the only signal.
func TestSweep_MembershipNewlyEligibleMidScanRestarts(t *testing.T) {
	db := newTestDB(t)
	started := make(chan bool, 4)
	bw := newTestBackfillWorker(t, db, withScan(func(ctx context.Context, _ *config.ChannelConfig, _ string, _ int, wm bool) error {
		started <- wm
		if !wm {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}))
	startWorker(t, bw)
	ch := backfillTestCh()

	bw.Sweep([]ChannelRef{refFor(ch, 3, false)}, false)
	if wm := recvWithin(t, started, "first scan"); wm {
		t.Fatalf("first scan withMembership = true, want false")
	}
	bw.Sweep([]ChannelRef{refFor(ch, 3, true)}, false)
	if wm := recvWithin(t, started, "restarted scan"); !wm {
		t.Fatalf("restarted scan withMembership = false, want true")
	}
}

// (b, negative) Narrowing 30 -> 3 mid-scan does NOTHING: a running deeper
// scan records a deeper depth, which satisfies every narrower window it will
// be asked for. No cancel, no restart.
func TestSweep_NarrowMidScanDoesNotRestart(t *testing.T) {
	db := newTestDB(t)
	started := make(chan int, 4)
	block := make(chan struct{})
	var gotCancel atomic.Bool
	bw := newTestBackfillWorker(t, db, withScan(func(ctx context.Context, _ *config.ChannelConfig, _ string, wd int, _ bool) error {
		started <- wd
		select {
		case <-block:
			return nil
		case <-ctx.Done():
			gotCancel.Store(true)
			return ctx.Err()
		}
	}))
	startWorker(t, bw)
	ch := backfillTestCh()

	bw.Sweep([]ChannelRef{refFor(ch, 30, false)}, false)
	if wd := recvWithin(t, started, "deep scan"); wd != 30 {
		t.Fatalf("scan windowDays = %d, want 30", wd)
	}

	bw.Sweep([]ChannelRef{refFor(ch, 3, false)}, false) // narrower — must be a no-op
	inflight, queued := inflightState(bw)
	fl := inflight[ch.ID]
	if fl == nil || fl.windowDays != 30 || queued != 0 {
		t.Fatalf("narrow sweep disturbed the running scan: fl=%+v queued=%d, want windowDays 30, queue empty", fl, queued)
	}

	close(block) // let the deep scan finish cleanly
	recvWithin(t, fl.done, "scan exit")
	if gotCancel.Load() {
		t.Error("narrow sweep cancelled the running deeper scan")
	}
	if inflight, queued := inflightState(bw); len(inflight) != 0 || queued != 0 {
		t.Errorf("after clean finish: inflight=%d queued=%d, want 0/0", len(inflight), queued)
	}
	select {
	case wd := <-started:
		t.Fatalf("unexpected restart at windowDays %d after a narrow sweep", wd)
	default:
	}
}

// (c) Membership toggling ON after a completed without-membership scan
// triggers a rescan (the backfilled_with_membership = 0 arm); toggling OFF
// after a completed with-membership scan does not (no arm exists — the
// completed scan is a superset, and members rows are hidden by the read arm,
// not deleted).
func TestSweep_MembershipToggleOnRescans_ToggleOffDoesNot(t *testing.T) {
	db := newTestDB(t)
	calls := make(chan scanCall, 8)
	release := make(chan struct{})
	bw := newTestBackfillWorker(t, db, withScan(func(_ context.Context, _ *config.ChannelConfig, chID string, wd int, wm bool) error {
		calls <- scanCall{chID, wd, wm}
		<-release
		return nil
	}))
	startWorker(t, bw)
	ch := backfillTestCh()
	ch2 := &config.ChannelConfig{ID: backfillTestChannel2, Name: "With Members"}

	// ch completed WITHOUT membership; ch2 completed WITH membership.
	if err := db.SetChannelBackfilled(ch.ID, 30, false, "2026-07-15T12:00:00Z"); err != nil {
		t.Fatalf("SetChannelBackfilled(ch): %v", err)
	}
	if err := db.SetChannelBackfilled(ch2.ID, 30, true, "2026-07-15T12:00:00Z"); err != nil {
		t.Fatalf("SetChannelBackfilled(ch2): %v", err)
	}

	// Eligibility OFF: neither channel rescans (ch's recorded 0 needs
	// membershipEligibleNow to fire; ch2's toggle-off has no arm).
	bw.Sweep([]ChannelRef{refFor(ch, 30, false), refFor(ch2, 30, false)}, false)
	if inflight, queued := inflightState(bw); len(inflight) != 0 || queued != 0 {
		t.Fatalf("eligibility-off sweep enqueued: inflight=%d queued=%d, want 0/0", len(inflight), queued)
	}

	// Eligibility ON: ch rescans (recorded without membership), ch2 does not
	// (recorded with). The stub blocks, so the in-flight set is inspectable
	// while ch's scan runs.
	bw.Sweep([]ChannelRef{refFor(ch, 30, true), refFor(ch2, 30, true)}, false)
	got := recvWithin(t, calls, "toggle-on rescan")
	if got != (scanCall{backfillTestChannel, 30, true}) {
		t.Fatalf("rescan = %+v, want {%s 30 true}", got, backfillTestChannel)
	}
	inflight, queued := inflightState(bw)
	if _, ok := inflight[ch2.ID]; ok || queued != 0 {
		t.Fatalf("already-with-membership channel was re-enqueued (queued=%d)", queued)
	}
	close(release)
}

// (Task 6, manual re-run) force=true treats every channel's backfilled_at as
// NULL: a COMPLETED channel — for which the normal sweep is a pinned no-op —
// is rescanned. The in-flight rules are NOT part of force's remit; the
// companion test below pins that half.
func TestSweep_ForceRescansCompletedChannel(t *testing.T) {
	db := newTestDB(t)
	calls := make(chan scanCall, 8)
	bw := newTestBackfillWorker(t, db, withScan(func(_ context.Context, _ *config.ChannelConfig, chID string, wd int, wm bool) error {
		calls <- scanCall{chID, wd, wm}
		return nil
	}))
	startWorker(t, bw)
	ch := backfillTestCh()
	refs := []ChannelRef{refFor(ch, 3, false)}

	// Completed at the same window + membership — every DB arm false, so the
	// normal sweep enqueues nothing (assert synchronously, as the fresh-
	// channel test does).
	if err := db.SetChannelBackfilled(ch.ID, 3, false, "2026-07-15T12:00:00Z"); err != nil {
		t.Fatalf("SetChannelBackfilled: %v", err)
	}
	bw.Sweep(refs, false)
	if inflight, queued := inflightState(bw); len(inflight) != 0 || queued != 0 {
		t.Fatalf("normal sweep of a completed channel enqueued: inflight=%d queued=%d, want 0/0", len(inflight), queued)
	}

	bw.Sweep(refs, true)
	if got := recvWithin(t, calls, "forced rescan"); got != (scanCall{backfillTestChannel, 3, false}) {
		t.Fatalf("forced rescan = %+v, want {%s 3 false}", got, backfillTestChannel)
	}
}

// (Task 6, manual re-run) force leaves the IN-FLIGHT rules untouched: an
// already-running, non-stale scan is skipped, not cancelled — a user
// double-tapping the manual re-run must never restart a scan that is already
// doing the work. (A STALE running scan still restarts via the existing
// widen logic, force or no force.)
func TestSweep_ForceDoesNotCancelRunningScan(t *testing.T) {
	db := newTestDB(t)
	started := make(chan int, 4)
	block := make(chan struct{})
	var gotCancel atomic.Bool
	bw := newTestBackfillWorker(t, db, withScan(func(ctx context.Context, _ *config.ChannelConfig, _ string, wd int, _ bool) error {
		started <- wd
		select {
		case <-block:
			return nil
		case <-ctx.Done():
			gotCancel.Store(true)
			return ctx.Err()
		}
	}))
	startWorker(t, bw)
	ch := backfillTestCh()

	bw.Sweep([]ChannelRef{refFor(ch, 3, false)}, true) // the manual re-run starts the scan
	if wd := recvWithin(t, started, "first scan"); wd != 3 {
		t.Fatalf("scan windowDays = %d, want 3", wd)
	}

	bw.Sweep([]ChannelRef{refFor(ch, 3, false)}, true) // double-tap — must be a no-op
	inflight, queued := inflightState(bw)
	fl := inflight[ch.ID]
	if fl == nil || queued != 0 {
		t.Fatalf("forced re-sweep disturbed the running scan: fl=%+v queued=%d, want in-flight entry intact, queue empty", fl, queued)
	}

	close(block) // let the scan finish cleanly
	recvWithin(t, fl.done, "scan exit")
	if gotCancel.Load() {
		t.Error("forced re-sweep cancelled the running non-stale scan")
	}
	select {
	case wd := <-started:
		t.Fatalf("unexpected second scan at windowDays %d after a forced re-sweep", wd)
	default:
	}
}

// (d) Removal mid-scan: the sweep whose channel list no longer carries the
// channel cancels the in-flight scan, WAITS for it to observe, then prunes —
// LAST, so even a stale page written in the cancel window is cleaned. Feed
// rows and channel_state gone (no resurrected rows), Queued + Upcoming +
// COOKIES? jobs AND their history rows gone, the Downloading job (and its
// history) untouched.
func TestSweep_RemovalMidScanCancelsThenPrunes(t *testing.T) {
	db := newTestDB(t)
	ch := backfillTestCh()
	chID := ch.ID
	now := "2026-07-15T12:00:00Z"

	// Jobs as the monitor host creates them: AddToHistory at creation.
	seed := func(id string, st database.JobStatus) {
		t.Helper()
		added, err := db.AddJob(&database.Job{ID: id, VideoID: id, URL: "u", Status: st, ChannelID: &chID})
		if err != nil || !added {
			t.Fatalf("AddJob(%s): added=%v err=%v", id, added, err)
		}
		if err := db.AddToHistory(id); err != nil {
			t.Fatalf("AddToHistory(%s): %v", id, err)
		}
	}
	doomed := []string{"d-queued", "d-upcoming", "d-cookies"}
	for i, st := range []database.JobStatus{database.StatusQueued, database.StatusUpcoming, database.StatusCookies} {
		seed(doomed[i], st)
	}
	seed("k-downloading", database.StatusDownloading)

	feedRow := func(id string) database.FeedItem {
		return database.FeedItem{ChannelID: chID, VideoID: id, Title: id,
			Published: now, DatePrecision: "coarse", Source: "videos", FirstSeen: now}
	}
	scanStarted := make(chan struct{})
	staleWritten := make(chan struct{})
	bw := newTestBackfillWorker(t, db, withScan(func(ctx context.Context, _ *config.ChannelConfig, _ string, _ int, _ bool) error {
		if _, err := db.UpsertFeedItem(feedRow("mid-scan")); err != nil {
			t.Errorf("mid-scan upsert: %v", err)
		}
		close(scanStarted)
		<-ctx.Done()
		// The §11 race, deliberately: one stale page lands AFTER the
		// cancellation is observed but before the scan returns. The prune
		// runs LAST, so even this write must be cleaned up.
		if _, err := db.UpsertFeedItem(feedRow("stale-page")); err != nil {
			t.Errorf("stale upsert: %v", err)
		}
		if err := db.SaveBackfillCursor(chID, `{"tabs":{"videos":{"continuation":"TOK9"}}}`); err != nil {
			t.Errorf("stale cursor save: %v", err)
		}
		close(staleWritten)
		return ctx.Err()
	}))
	startWorker(t, bw)

	bw.Sweep([]ChannelRef{refFor(ch, 3, false)}, false)
	recvWithin(t, scanStarted, "scan start")

	// The channel is REMOVED from config: the next sweep's list no longer
	// carries it. Sweep is synchronous through the prune.
	bw.Sweep(nil, false)

	select {
	case <-staleWritten:
	default:
		t.Fatal("prune returned before the cancelled scan exited the channel (wait-observed broken)")
	}

	for _, id := range []string{"mid-scan", "stale-page"} {
		if it, err := db.GetFeedItem(chID, id); err != nil || it != nil {
			t.Errorf("feed row %s survived the prune (%+v, err %v), want deleted", id, it, err)
		}
	}
	if raw, err := db.LoadBackfillCursor(chID); err != nil || raw != "" {
		t.Errorf("backfill_state = %q (err %v), want gone after prune", raw, err)
	}
	if cb, err := db.GetChannelBackfill(chID); err != nil || cb.At != "" || cb.WindowDays != nil || cb.WithMembership != nil {
		t.Errorf("channel_state survived the prune: %+v (err %v)", cb, err)
	}

	assertJob := func(id string, wantJob, wantHistory bool) {
		t.Helper()
		job, err := db.GetJob(id)
		if err != nil {
			t.Fatalf("GetJob(%s): %v", id, err)
		}
		if got := job != nil; got != wantJob {
			t.Errorf("%s: job exists = %v, want %v", id, got, wantJob)
		}
		has, err := db.HasProcessed(id)
		if err != nil {
			t.Fatalf("HasProcessed(%s): %v", id, err)
		}
		if has != wantHistory {
			t.Errorf("%s: history exists = %v, want %v", id, has, wantHistory)
		}
	}
	for _, id := range doomed {
		assertJob(id, false, false) // job AND history gone — no orphan
	}
	assertJob("k-downloading", true, true) // running download keeps going

	if inflight, queued := inflightState(bw); len(inflight) != 0 || queued != 0 {
		t.Errorf("in-flight entry leaked past the prune: inflight=%d queued=%d", len(inflight), queued)
	}
}

// (d, queued variant — review fix 1) Pruning a channel that is QUEUED
// behind a long-running scan must settle it directly (splice + close done)
// and return promptly — NOT wait for the consumer to reach the item, which
// would stall the feed monitor's cycle goroutine behind every scan ahead in
// the queue. The running scan is untouched: never cancelled, completes
// cleanly. Channel-synced throughout; no sleeps.
func TestSweep_PruneOfQueuedChannelDoesNotWaitBehindRunningScan(t *testing.T) {
	db := newTestDB(t)
	chA := backfillTestCh()
	chB := &config.ChannelConfig{ID: backfillTestChannel2, Name: "Queued Behind"}
	now := "2026-07-15T12:00:00Z"

	startedA := make(chan struct{})
	releaseA := make(chan struct{})
	var cancelledA atomic.Bool
	bw := newTestBackfillWorker(t, db, withScan(func(ctx context.Context, _ *config.ChannelConfig, chID string, _ int, _ bool) error {
		if chID != chA.ID {
			t.Errorf("queued channel %s must never scan (it was pruned while queued)", chID)
			return nil
		}
		close(startedA)
		select {
		case <-releaseA:
			return nil
		case <-ctx.Done():
			cancelledA.Store(true)
			return ctx.Err()
		}
	}))
	startWorker(t, bw)

	// chB owns a feed row so the prune's effect is observable.
	if _, err := db.UpsertFeedItem(database.FeedItem{ChannelID: chB.ID, VideoID: "b1", Title: "b1",
		Published: now, DatePrecision: "coarse", Source: "videos", FirstSeen: now}); err != nil {
		t.Fatalf("UpsertFeedItem: %v", err)
	}

	// chA starts scanning (and blocks); chB sits QUEUED behind it.
	bw.Sweep([]ChannelRef{refFor(chA, 3, false), refFor(chB, 3, false)}, false)
	recvWithin(t, startedA, "long-running scan start")
	inflight, queued := inflightState(bw)
	if inflight[chB.ID] == nil || queued != 1 {
		t.Fatalf("precondition: chB should be queued (inflight=%v queued=%d)", inflight[chB.ID] != nil, queued)
	}

	// chB removed from config. The prune-sweep must complete while chA's
	// scan is still blocked — a wait on chB's done would hang here forever.
	pruned := make(chan struct{})
	go func() {
		bw.Sweep([]ChannelRef{refFor(chA, 3, false)}, false)
		close(pruned)
	}()
	recvWithin(t, pruned, "prune of the queued channel (must not wait behind the running scan)")

	inflight, queued = inflightState(bw)
	if inflight[chB.ID] != nil || queued != 0 {
		t.Errorf("queued entry not spliced: inflight[chB]=%v queued=%d", inflight[chB.ID] != nil, queued)
	}
	if inflight[chA.ID] == nil {
		t.Error("running scan's entry disturbed by the queued prune")
	}
	if it, err := db.GetFeedItem(chB.ID, "b1"); err != nil || it != nil {
		t.Errorf("chB feed row survived the prune (%+v, err %v)", it, err)
	}
	if cancelledA.Load() {
		t.Error("running scan was cancelled by a prune of a DIFFERENT channel")
	}

	// The running scan finishes cleanly, uncancelled.
	flA := inflight[chA.ID]
	close(releaseA)
	recvWithin(t, flA.done, "running scan's clean exit")
	if cancelledA.Load() {
		t.Error("running scan observed a cancellation it should never have gotten")
	}
}

// (e) A scan that FAILS removes its in-flight entry — a leaked entry is a
// permanent silent stall — so the next sweep retries the channel.
func TestSweep_FailedScanRetriedByNextSweep(t *testing.T) {
	db := newTestDB(t)
	calls := make(chan int, 4)
	hold := make(chan struct{})
	var n atomic.Int32
	bw := newTestBackfillWorker(t, db, withScan(func(_ context.Context, _ *config.ChannelConfig, _ string, _ int, _ bool) error {
		calls <- int(n.Add(1))
		<-hold
		return errors.New("browse http 503")
	}))
	startWorker(t, bw)
	ch := backfillTestCh()
	refs := []ChannelRef{refFor(ch, 3, false)}

	bw.Sweep(refs, false)
	if got := recvWithin(t, calls, "first attempt"); got != 1 {
		t.Fatalf("first attempt = %d, want 1", got)
	}
	inflight, _ := inflightState(bw)
	fl := inflight[ch.ID]
	if fl == nil {
		t.Fatal("no in-flight entry while the scan is running")
	}
	hold <- struct{}{} // release attempt 1 -> it fails
	recvWithin(t, fl.done, "failed scan's exit")
	if inflight, queued := inflightState(bw); len(inflight) != 0 || queued != 0 {
		t.Fatalf("failed scan leaked its in-flight entry: inflight=%d queued=%d", len(inflight), queued)
	}

	bw.Sweep(refs, false) // retry: backfilled_at is still NULL
	if got := recvWithin(t, calls, "retry attempt"); got != 2 {
		t.Fatalf("retry attempt = %d, want 2", got)
	}
	hold <- struct{}{}
}

// ---- the Task 5 progress-emission tests -------------------------------------

// progressEmission records one OnProgress call — the Task 5 seam the host
// wires to the WS broadcast and the TUI channel.
type progressEmission struct {
	chID  string
	tab   string
	pages int
	state string
}

// recordProgress wires bw.OnProgress to a recorder channel.
func recordProgress(bw *BackfillWorker) chan progressEmission {
	emitted := make(chan progressEmission, 32)
	bw.OnProgress = func(chID, tab string, pages int, state string) {
		emitted <- progressEmission{chID, tab, pages, state}
	}
	return emitted
}

// assertEmissions receives exactly want from emitted, in order, then asserts
// silence.
func assertEmissions(t *testing.T, emitted chan progressEmission, want []progressEmission) {
	t.Helper()
	for i, w := range want {
		got := recvWithin(t, emitted, fmt.Sprintf("emission %d (%+v)", i, w))
		if got != w {
			t.Errorf("emission %d = %+v, want %+v", i, got, w)
		}
	}
	select {
	case extra := <-emitted:
		t.Errorf("unexpected extra emission %+v", extra)
	default:
	}
}

// A clean scripted scan emits "scanning" once per COMPLETED page — per-tab
// session page counts, including the empty page of an exhausted tab — and one
// scan-level "done" (tab "", pages 0) after completion.
func TestBackfillProgress_PagesAndDone(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	fetch := newScriptedFetcher(t, map[string][]scriptedPage{
		"videos": {
			// page 1 mixed (keeps paging), page 2 all-older (arm (a) stop).
			{wantCont: "", page: &TabPage{
				Items:        []TabItem{coarseItem("v1", 24*time.Hour), coarseItem("v2", 240*time.Hour)},
				Continuation: "TOK2",
			}},
			{wantCont: "TOK2", page: &TabPage{
				Items:        []TabItem{coarseItem("v3", 264*time.Hour)},
				Continuation: "TOK3",
			}},
		},
		"streams": emptyTabScript(),
	})
	bw := newTestBackfillWorker(t, db, withTabFetch(fetch.fetch), withBackfillNow(now))
	emitted := recordProgress(bw)
	startWorker(t, bw)

	bw.Sweep([]ChannelRef{refFor(backfillTestCh(), 3, false)}, false)
	assertEmissions(t, emitted, []progressEmission{
		{backfillTestChannel, "videos", 1, "scanning"},
		{backfillTestChannel, "videos", 2, "scanning"},
		{backfillTestChannel, "streams", 1, "scanning"},
		{backfillTestChannel, "", 0, "done"},
	})
}

// A failed page emits NO "scanning" (the page did not complete); the scan
// ends in one scan-level "error" — never "done" — so the UIs report the
// sweep-will-retry state honestly.
func TestBackfillProgress_FetchFailureEmitsError(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	fetch := newScriptedFetcher(t, map[string][]scriptedPage{
		"videos":  {{wantCont: "", err: errors.New("browse http 503")}},
		"streams": emptyTabScript(),
	})
	bw := newTestBackfillWorker(t, db, withTabFetch(fetch.fetch), withBackfillNow(now))
	emitted := recordProgress(bw)
	startWorker(t, bw)

	bw.Sweep([]ChannelRef{refFor(backfillTestCh(), 3, false)}, false)
	assertEmissions(t, emitted, []progressEmission{
		// videos page 1 failed — nothing; streams still scans (tab
		// independence) before the scan-level error.
		{backfillTestChannel, "streams", 1, "scanning"},
		{backfillTestChannel, "", 0, "error"},
	})
}

// Removal mid-scan emits "idle" twice, in order: once from the cancelled
// scan's runScan exit, once from CancelAndPrune after the prune — the second
// covers the spliced-queued and boot-prune paths where no runScan exit fires.
func TestBackfillProgress_PruneEmitsIdle(t *testing.T) {
	db := newTestDB(t)
	scanStarted := make(chan struct{})
	bw := newTestBackfillWorker(t, db, withScan(func(ctx context.Context, _ *config.ChannelConfig, _ string, _ int, _ bool) error {
		close(scanStarted)
		<-ctx.Done()
		return ctx.Err()
	}))
	emitted := recordProgress(bw)
	startWorker(t, bw)

	bw.Sweep([]ChannelRef{refFor(backfillTestCh(), 3, false)}, false)
	recvWithin(t, scanStarted, "scan start")
	bw.Sweep(nil, false) // channel removed from config — cancel, wait, prune
	assertEmissions(t, emitted, []progressEmission{
		{backfillTestChannel, "", 0, "idle"},
		{backfillTestChannel, "", 0, "idle"},
	})
}

// A listing-order violation is evidence the arm-(a) window stop's
// newest-first premise is broken for the tab (spec §11) — so the stop must
// be DISABLED for the rest of that tab's scan session, exactly as the walk's
// noExit disables its early exit (walk.go). Page 2 carries the violation
// (v3 dated newer than v2) while every item computes older than the cutoff;
// without the disable, allOlder would mark the tab Done and TOK3 would never
// be followed — permanently stranding whatever in-window content the broken
// ordering hid deeper in the tab. Page 3 is all-older WITH clean ordering:
// the disable must persist for the session, so the scan keeps paging to
// natural exhaustion (page 4) and still completes CLEANLY.
func TestScanChannel_OrderingViolationDisablesWindowStop(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	fetch := newScriptedFetcher(t, map[string][]scriptedPage{
		"videos": {
			// page 1: in-window, ordered — no violation, no stop.
			{wantCont: "", page: &TabPage{
				Items:        []TabItem{coarseItem("v1", 24*time.Hour)},
				Continuation: "TOK2",
			}},
			// page 2: ALL older than the 3-day cutoff, but v3 (10d) is NEWER
			// than v2 (11d) — the violation and allOlder trip on the same page.
			{wantCont: "TOK2", page: &TabPage{
				Items:        []TabItem{coarseItem("v2", 264*time.Hour), coarseItem("v3", 240*time.Hour)},
				Continuation: "TOK3",
			}},
			// page 3: all older, cleanly ordered — the disable must persist.
			{wantCont: "TOK3", page: &TabPage{
				Items:        []TabItem{coarseItem("v4", 300*time.Hour)},
				Continuation: "TOK4",
			}},
			// page 4: natural exhaustion — the only clean ending left.
			{wantCont: "TOK4", page: &TabPage{}},
		},
		"streams": emptyTabScript(),
	})
	bw := newTestBackfillWorker(t, db, withTabFetch(fetch.fetch), withBackfillNow(now))

	if err := bw.scanChannel(context.Background(), backfillTestCh(), backfillTestChannel, 3, false); err != nil {
		t.Fatalf("scanChannel: %v", err)
	}

	if got := fetch.tabCalls("videos"); got != 4 {
		t.Errorf("videos tab fetched %d times, want 4 (violation must disable arm (a) for the session)", got)
	}
	// Every page's rows persisted; natural exhaustion still completes cleanly.
	for _, id := range []string{"v1", "v2", "v3", "v4"} {
		mustFeedItem(t, db, backfillTestChannel, id)
	}
	if cb, err := db.GetChannelBackfill(backfillTestChannel); err != nil || cb.At == "" {
		t.Errorf("backfilled_at = %q (err %v), want set — natural exhaustion is a clean ending", cb.At, err)
	}
}
