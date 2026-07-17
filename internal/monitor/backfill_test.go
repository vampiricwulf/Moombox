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

	// Rows from BOTH pages persisted, with the §11 classification and the
	// provisional per-tab catalog_pos running across pages.
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

	cur := loadTestCursor(t, db, backfillTestChannel)
	for _, tab := range []string{"videos", "streams"} {
		if tc := cur.Tabs[tab]; tc == nil || !tc.Done {
			t.Errorf("cursor %s tab not marked done: %+v", tab, tc)
		}
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
	if tc := cur.Tabs["videos"]; tc == nil || tc.Done || tc.Continuation != "TOK2" {
		t.Fatalf("videos cursor after failure = %+v, want continuation TOK2, not done", tc)
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

	// The provisional per-tab index resumed too: v3 continues at 2 after v1/v2.
	if it := mustFeedItem(t, db, backfillTestChannel, "v3"); it.CatalogPos != 2 {
		t.Errorf("v3 catalog_pos = %d, want 2 (per-tab index resumes across scans)", it.CatalogPos)
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

// (e) Empty channel: every tab returns zero items and no continuation —
// NEITHER arm. All tabs complete CLEANLY (Task 3 will set backfilled_at over
// zero rows), no parser failure is recorded, and a second scan does not
// refetch anything (misreading the parser arm as vacuously true would rescan
// empty channels every cycle, forever — spec §11).
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

	cur := loadTestCursor(t, db, backfillTestChannel)
	for _, tab := range []string{"videos", "streams", "membership"} {
		if tc := cur.Tabs[tab]; tc == nil || !tc.Done {
			t.Errorf("cursor %s tab not done after empty exhaustion: %+v", tab, tc)
		}
	}

	// Second scan over the same DB: every tab's cursor is done, so nothing may
	// be refetched (the scripted fetcher fails the test on ANY call).
	fetch2 := newScriptedFetcher(t, map[string][]scriptedPage{})
	bw2 := newTestBackfillWorker(t, db, withTabFetch(fetch2.fetch), withBackfillNow(now))
	if err := bw2.scanChannel(context.Background(), backfillTestCh(), backfillTestChannel, 3, true); err != nil {
		t.Fatalf("second scanChannel: %v", err)
	}
	if got := fetch2.totalCalls(); got != 0 {
		t.Errorf("second scan issued %d fetches, want 0", got)
	}
}
