package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
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
				Items:        []TabItem{coarseItem("v1", 24 * time.Hour), coarseItem("v2", 240 * time.Hour)},
				Continuation: "TOK2",
			}},
			// page 2: ALL items outside ⇒ arm (a) fires; TOK3 must never be followed.
			{wantCont: "TOK2", page: &TabPage{
				Items:        []TabItem{coarseItem("v3", 264 * time.Hour), coarseItem("v4", 288 * time.Hour)},
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
				Items:        []TabItem{coarseItem("v1", 24 * time.Hour), coarseItem("v2", 30 * time.Hour)},
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
				Items:        []TabItem{coarseItem("v3", 26 * 30 * time.Hour)},
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
				Items: []TabItem{undatedItem("live1"), coarseItem("v2", 2 * time.Hour)},
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
				coarseItem("vid-new", 1 * time.Hour),
				coarseItem("tie-pos-b", 10 * time.Hour),
				coarseItem("tie-id-b", 20 * time.Hour),
			}}},
		},
		"streams": {
			{wantCont: "", page: &TabPage{Items: []TabItem{
				coarseItem("tie-pos-z", 10 * time.Hour),
				coarseItem("vid-new", 1 * time.Hour),
				coarseItem("tie-id-a", 20 * time.Hour),
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
				Items:        []TabItem{coarseItem("v1", 5 * time.Hour), coarseItem("v2", 1 * time.Hour)},
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
