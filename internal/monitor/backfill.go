package monitor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// This file is the feed-history backfill scanner (spec §11): populate the
// feed_items list once per channel from /videos + /streams (+ /membership
// when eligible), page by page, to the window depth, then — when every
// eligible tab ended cleanly — renumber catalog_pos channel-globally and
// write the completion record (backfilled_at). The sweep / in-flight set
// that drives scanChannel is Task 4's.

// backfillPageInterval is the global page throttle: one page per second,
// globally — a constant, not config (spec §11 operational rules). Scans run
// strictly serially on one goroutine (Task 4's queue), so the worker-level
// clock in waitPage IS the global one.
const backfillPageInterval = time.Second

// TabItem is one video listed on a channel tab page. It mirrors
// youtube.TabItem (browse.go) but is declared here so the monitor package
// stays decoupled from the youtube package — the wiring closure in
// cmd/moombox adapts between the two, exactly like MembershipVideo does for
// FetchMembership. Age 0 means live/upcoming badge or undatable ("now");
// only a dated listing text yields a non-zero coarse age.
type TabItem struct {
	VideoID string
	Title   string
	Age     time.Duration
}

// TabPage is one page of a channel tab. Continuation "" means the tab is
// exhausted. Mirrors youtube.TabPage; see TabItem.
type TabPage struct {
	Items        []TabItem
	Continuation string
}

// ErrTabContinuationLoop mirrors youtube.ErrContinuationLoop across the
// package seam — see TabPageFetchFunc for the translation contract.
var ErrTabContinuationLoop = errors.New("tab continuation loop detected")

// errUndatablePage is the parser-failure arm's error (spec §11 second stop
// arm): a NON-EMPTY page with no datable item.
var errUndatablePage = errors.New("non-empty page with no datable item (parser-failure arm)")

// TabPageFetchFunc fetches ONE page of a channel tab ("videos", "streams",
// "membership"): continuation "" requests page 1. Wired to
// youtube.Service.FetchChannelTabPage via an adapter closure in cmd/moombox;
// the adapter MUST translate youtube.ErrContinuationLoop into
// ErrTabContinuationLoop (errors.Is-compatible) so the scanner can tell a
// loop from a transient failure without importing the youtube package.
type TabPageFetchFunc func(ctx context.Context, channelID, tab, continuation string) (*TabPage, error)

// backfillCursor is the channel_state.backfill_state JSON (spec §11 "per-tab
// cursor"): everything a scan interrupted by a crash, restart, or transient
// failure needs to resume without refetching completed work. Lifecycle
// (spec §11): cleared by Task 3's completion write and by Task 4 on any
// cancel — resume applies ONLY to interrupted scans, never cancelled ones.
//
//	{"tabs":{"videos":{"continuation":"TOK","next_pos":60},
//	         "streams":{"done":true}}}
type backfillCursor struct {
	Tabs map[string]*backfillTabCursor `json:"tabs"`
}

// backfillTabCursor is one tab's resume state.
type backfillTabCursor struct {
	// Continuation is the token for the tab's NEXT page; "" with Done unset
	// means the tab hasn't started (page 1).
	Continuation string `json:"continuation,omitempty"`
	// NextPos is the provisional per-tab catalog_pos for the next row — the
	// running page-item index across the tab's pages, so a resumed scan
	// continues numbering where the interrupted one stopped. Task 3's
	// ordering pass renumbers channel-globally at completion.
	NextPos int `json:"next_pos,omitempty"`
	// Done marks a CLEAN ending only — the arm-(a) window stop or natural
	// exhaustion. A resumed scan skips done tabs; incomplete tabs (arm (b),
	// loop, fetch failure) stay Done=false so the sweep-retry rescans them.
	Done bool `json:"done,omitempty"`
}

// tab returns (creating if needed) the named tab's cursor.
func (c *backfillCursor) tab(name string) *backfillTabCursor {
	tc := c.Tabs[name]
	if tc == nil {
		tc = &backfillTabCursor{}
		c.Tabs[name] = tc
	}
	return tc
}

// tabResult is one tab's scan outcome — the shape Task 3's completion
// decision consumes. complete is true only for the two CLEAN endings (the
// arm-(a) window stop or natural exhaustion); err carries the reason for an
// incomplete tab (arm (b), a continuation loop, or a fetch/DB failure) and
// is nil iff complete.
type tabResult struct {
	tab      string
	complete bool
	err      error
}

// BackfillWorker owns the per-channel backfill scan (spec §11). Task 4 adds
// the sweep, the serial scan queue, and the in-flight set; until then
// scanChannel is driven directly.
type BackfillWorker struct {
	db *database.Database

	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	// FetchTabPage is the injected tab-page fetch — see TabPageFetchFunc.
	FetchTabPage TabPageFetchFunc

	// pageInterval is the global page throttle (backfillPageInterval in
	// production; tests set 0 to page instantly).
	pageInterval time.Duration
	// lastPageAt is when the last page fetch was released, for waitPage.
	// Unguarded by design: scans are strictly serial on one goroutine.
	lastPageAt time.Time

	// now returns the current time. scanChannel reads it exactly ONCE per
	// scan, so every date the scan computes — coarse now-Age, assumed now,
	// first_seen, the window cutoff — derives from a single instant (the
	// one-`now` rule, as checkChannel's cycleNow). Tests pin it via
	// withBackfillNow.
	now func() time.Time
}

// NewBackfillWorker creates a backfill worker. FetchTabPage must be wired
// before any scan runs.
func NewBackfillWorker(db *database.Database, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *BackfillWorker {
	return &BackfillWorker{
		db:           db,
		logger:       logger,
		pageInterval: backfillPageInterval,
		now:          time.Now,
	}
}

// scanChannel runs one channel's backfill scan: each eligible tab, page by
// page, persisting every page immediately and advancing the per-tab cursor
// (spec §11 — buffering would break "resumable via cursor"). When EVERY
// eligible tab ends cleanly it runs the ordering pass and writes the
// completion record (completeScan), returning nil; any incomplete tab
// (arm (b), loop, fetch failure) surfaces as an error instead — no renumber,
// no backfilled_at — so the sweep (Task 4) retries, resuming from the saved
// cursor.
func (bw *BackfillWorker) scanChannel(ctx context.Context, ch *config.ChannelConfig, chID string, windowDays int, withMembership bool) error {
	// YouTube channels only — an ALLOW-list, stricter than getYouTubeChannels'
	// twitch-exclusion (spec §11 operational rules: a Twitch channel scanned
	// as youtube.com/channel/<twitch_login> 404s on every tab, never sets
	// backfilled_at, and retries forever). The sweep (Task 4) must apply the
	// same gate when selecting channels; this guard is defense-in-depth that
	// fails loudly in logs instead of scanning a bogus URL.
	if ch.GetPlatform() != "youtube" {
		return fmt.Errorf("backfill %s: platform %q is not backfillable", chID, ch.GetPlatform())
	}

	scanNow := bw.now().UTC()
	cutoff := scanNow.Add(-time.Duration(windowDays) * 24 * time.Hour)

	// /videos and /streams are public and always eligible; /membership only
	// when the caller resolved MembershipDiscoveryEnabled() && HasAuthCookies()
	// (spec §11: members rows written with discovery off would enter scope
	// and be jobbed — the toggle defeated by the indirection that fixes the
	// original bug).
	tabs := []string{"videos", "streams"}
	if withMembership {
		tabs = append(tabs, "membership")
	}

	cur := bw.loadCursor(chID)
	results := make([]tabResult, 0, len(tabs))
	for _, tab := range tabs {
		tc := cur.tab(tab)
		if tc.Done {
			// Crash/failure resume: this tab already ended cleanly in an
			// earlier interrupted scan — do not rescan it. (A COMPLETED
			// channel never reaches here: Task 3 clears the cursor and sets
			// backfilled_at, which stops the sweep from re-invoking at all.)
			results = append(results, tabResult{tab: tab, complete: true})
			continue
		}
		err := bw.scanTab(ctx, chID, tab, cur, tc, cutoff, scanNow)
		results = append(results, tabResult{tab: tab, complete: err == nil, err: err})
	}

	// The ordering pass + completion record (spec §11 steps 2–3) run only
	// when EVERY eligible tab ended cleanly. Any incomplete tab (arm (b),
	// loop, fetch/DB failure) writes nothing here — the cursors are already
	// saved per page, backfilled_at stays NULL, and the sweep (Task 4)
	// retries next cycle, resuming from the cursor.
	var errs []error
	for _, r := range results {
		if !r.complete {
			errs = append(errs, r.err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return bw.completeScan(chID, windowDays, withMembership, scanNow)
}

// completeScan is the §11 completion: ONE channel-global ordering pass over
// the deduped feed_items set, then the completion record. Collect-then-update
// is load-bearing (SetMaxOpenConns(1)): ListFeedOrderRows returns a fully
// collected slice with its cursor closed before RenumberCatalog issues any
// UPDATE. A failure anywhere here leaves backfilled_at NULL with the cursor's
// all-done tab states intact, so the sweep-retry skips every fetch and
// re-runs just this pass — the renumber and record writes are idempotent.
func (bw *BackfillWorker) completeScan(chID string, windowDays int, withMembership bool, scanNow time.Time) error {
	rows, err := bw.db.ListFeedOrderRows(chID)
	if err != nil {
		return fmt.Errorf("backfill %s: read ordering rows: %w", chID, err)
	}
	// The §11 sort key, exactly: published DESC, provisional catalog_pos ASC,
	// video_id ASC. published is RFC3339 UTC everywhere in this codebase, so
	// lexicographic order IS chronological order (the same comparison the
	// idx_feed_items_window index and FeedScope's ORDER BY perform). The key
	// is total — (channel_id, video_id) is the PK, so video_id never ties.
	slices.SortFunc(rows, func(a, b database.FeedOrderRow) int {
		if c := cmp.Compare(b.Published, a.Published); c != 0 {
			return c
		}
		if c := cmp.Compare(a.CatalogPos, b.CatalogPos); c != 0 {
			return c
		}
		return cmp.Compare(a.VideoID, b.VideoID)
	})
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.VideoID
	}
	if err := bw.db.RenumberCatalog(chID, ids); err != nil {
		return fmt.Errorf("backfill %s: renumber: %w", chID, err)
	}
	// ts is the scan's one `now` (the one-`now` rule); SetChannelBackfilled
	// clears backfill_state in the same statement — the cursor's lifecycle
	// ends at completion (§11).
	if err := bw.db.SetChannelBackfilled(chID, windowDays, withMembership, scanNow.Format(time.RFC3339)); err != nil {
		return fmt.Errorf("backfill %s: completion record: %w", chID, err)
	}
	bw.logger.Info("backfill complete", "channel", chID, "rows", len(rows),
		"windowDays", windowDays, "withMembership", withMembership)
	return nil
}

// scanTab pages one tab from its cursor position until a stop condition.
// Clean endings return nil (tc.Done persisted true); everything else returns
// the error and leaves tc.Done false with the cursor still pointing at the
// last good position, so the sweep-retry resumes exactly there.
func (bw *BackfillWorker) scanTab(ctx context.Context, chID, tab string, cur *backfillCursor, tc *backfillTabCursor, cutoff, scanNow time.Time) error {
	firstSeen := scanNow.Format(time.RFC3339)
	// lastDatable drives the ordering-evidence check (spec §11: log any item
	// dated NEWER than an earlier item in the same tab — a violation is
	// evidence the arm-(a) stop's newest-first premise is broken). It is
	// per scan SESSION, deliberately not persisted in the cursor: a resumed
	// scan computes dates from a fresh `now`, and comparing against an
	// earlier session's quantized coarse dates would false-fire on every
	// resume. Only datable items participate — an assumed published=now is a
	// claim of ignorance, not a listing coordinate (same reason the walk's
	// exhaustion only draws from coarse rows).
	var lastDatable time.Time

	for {
		if err := bw.waitPage(ctx); err != nil {
			return fmt.Errorf("backfill %s/%s: %w", chID, tab, err)
		}
		page, err := bw.FetchTabPage(ctx, chID, tab, tc.Continuation)
		if err != nil {
			if errors.Is(err, ErrTabContinuationLoop) {
				// CONTROLLER RULING (carried from Task 1): a continuation loop
				// is INCOMPLETE, never exhaustion. A loop means no forward
				// progress — completing the tab cleanly would set backfilled_at
				// over a tab whose window depth was never reached. Treat it
				// like a fetch failure: log loudly, stop the tab, report
				// incomplete (backfilled_at stays NULL), sweep retries later.
				bw.logger.Error("backfill continuation loop; stopping tab",
					"channel", chID, "tab", tab, "err", err)
			} else {
				// CONTROLLER RULING (carried from Task 1): stale-token /
				// transient-fetch recovery is cursor + sweep retry. The cursor
				// was already saved after the last good page, so stop the tab,
				// report incomplete, and return the error context — NO
				// in-scanner retry loop. The sweep (Task 4) re-runs the scan,
				// which resumes from the saved cursor.
				bw.logger.Warn("backfill page fetch failed; tab resumes from cursor on next sweep",
					"channel", chID, "tab", tab, "err", err)
			}
			return fmt.Errorf("backfill %s/%s: %w", chID, tab, err)
		}

		datable := 0
		for _, it := range page.Items {
			if it.Age > 0 {
				datable++
			}
		}
		if len(page.Items) > 0 && datable == 0 {
			// Second stop arm (spec §11): a NON-EMPTY page with no datable
			// item. An undatable item is dated `now` — inside every window —
			// so the date arm can never fire on a page of them, and under a
			// relativeAgeRe break that is every item on every channel. This is
			// a parser-failure detector, not a completeness rule: log loudly,
			// stop the tab, leave backfilled_at NULL so the sweep retries when
			// the parser is fixed. The page's rows are NOT written and the
			// cursor does NOT advance: the extracted dates are garbage
			// (published=now would drag the channel's whole back catalogue
			// into the window, walk scope, and §10 job creation), and the
			// retry must REFETCH this page with the fixed parser — a cursor
			// advanced past it would instead creep one garbage page deeper
			// every sweep cycle, defeating the arm's own retry purpose.
			// NOTE the non-empty qualifier is load-bearing: an EMPTY page is
			// NEITHER arm (natural exhaustion, handled below) — reading this
			// arm as vacuously true of an empty page would classify every
			// empty channel as a parser failure and rescan it forever.
			bw.logger.Error("backfill parser-failure arm: non-empty page with no datable item; stopping tab",
				"channel", chID, "tab", tab, "items", len(page.Items))
			return fmt.Errorf("backfill %s/%s: %w", chID, tab, errUndatablePage)
		}

		// allOlder seeds from len>0 so an EMPTY page can never fire arm (a)
		// vacuously: an empty page with a continuation keeps paging, and an
		// empty page without one is natural exhaustion — a clean completion
		// either way it ends (spec §11: an empty channel must finish, set
		// backfilled_at, and establish, for ~3 requests total).
		allOlder := len(page.Items) > 0
		for _, it := range page.Items {
			// Classification (spec §11): Age>0 ⇒ coarse, published=now-Age
			// (the NEWEST instant consistent with the listing text, so an
			// out-of-window coarse date is out on any reading — no extra
			// margin needed); Age==0 ⇒ assumed, published=now (live/upcoming
			// badge or undatable — the scanner cannot tell which).
			pub, prec := scanNow, "assumed"
			if it.Age > 0 {
				pub, prec = scanNow.Add(-it.Age), "coarse"
				// IS-SET guard as in the walk's ordering check: the zero value
				// is older than every real date and would never fire anyway,
				// but keep the shape explicit.
				if !lastDatable.IsZero() && pub.After(lastDatable) {
					bw.logger.Warn("backfill listing order violated",
						"channel", chID, "tab", tab, "id", it.VideoID)
				}
				lastDatable = pub
			}
			if !pub.Before(cutoff) {
				allOlder = false
			}
			// Write each page's rows IMMEDIATELY (spec §11: scan-then-merge is
			// incompatible with "resumable via cursor"). The upsert forces
			// status 'unknown' on insert and never touches it on conflict — a
			// listing supplies a date, never a classification. Source is the
			// tab name (the §6 enum: rss|membership|videos|streams).
			// catalog_pos is the provisional per-tab index; Task 3 renumbers
			// channel-globally. A row failure is tab-fatal, not skip-and-go:
			// the backfill runs once, so a skipped row would be a permanent
			// gap — the cursor hasn't advanced past this page, so the retry
			// refetches it and the upserts are idempotent.
			if _, err := bw.db.UpsertFeedItem(database.FeedItem{
				ChannelID: chID, VideoID: it.VideoID, Title: it.Title,
				Published: pub.Format(time.RFC3339), DatePrecision: prec,
				CatalogPos: tc.NextPos, Source: tab, FirstSeen: firstSeen,
			}); err != nil {
				bw.logger.Warn("backfill upsert failed; tab resumes from cursor on next sweep",
					"channel", chID, "tab", tab, "id", it.VideoID, "err", err)
				return fmt.Errorf("backfill %s/%s: upsert %s: %w", chID, tab, it.VideoID, err)
			}
			tc.NextPos++
		}

		// Advance and persist the cursor after every good page — this is the
		// resume point the carried fetch-failure ruling relies on. Done marks
		// the two CLEAN endings only:
		//   - Continuation "" ⇒ natural exhaustion (including the zero-item
		//     page of an empty tab — see the carve-out above);
		//   - allOlder ⇒ arm (a), the page-granular window stop: a whole page
		//     older than the window is the "window depth + margin" evidence
		//     the newest-first assumption makes strong (spec §11) — the
		//     remaining continuation is deliberately not followed.
		tc.Continuation = page.Continuation
		tc.Done = page.Continuation == "" || allOlder
		if err := bw.saveCursor(chID, cur); err != nil {
			return fmt.Errorf("backfill %s/%s: save cursor: %w", chID, tab, err)
		}
		if tc.Done {
			bw.logger.Debug("backfill tab complete",
				"channel", chID, "tab", tab, "rows", tc.NextPos, "windowStop", allOlder)
			return nil
		}
	}
}

// waitPage enforces the global one-page-per-second throttle before a fetch.
// pageInterval <= 0 (tests) pages instantly but still honors cancellation.
func (bw *BackfillWorker) waitPage(ctx context.Context) error {
	if bw.pageInterval <= 0 {
		return ctx.Err()
	}
	if wait := bw.pageInterval - time.Since(bw.lastPageAt); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	bw.lastPageAt = time.Now()
	return nil
}

// loadCursor reads and parses the channel's backfill cursor. Missing, empty,
// or corrupt state all yield a fresh cursor (scan from page 1 of every tab)
// — restarting a scan is always safe, the upserts are idempotent.
func (bw *BackfillWorker) loadCursor(chID string) *backfillCursor {
	fresh := &backfillCursor{Tabs: map[string]*backfillTabCursor{}}
	raw, err := bw.db.LoadBackfillCursor(chID)
	if err != nil {
		bw.logger.Warn("backfill cursor load failed; scanning from page 1", "channel", chID, "err", err)
		return fresh
	}
	if raw == "" {
		return fresh
	}
	cur := &backfillCursor{}
	if err := json.Unmarshal([]byte(raw), cur); err != nil {
		bw.logger.Warn("backfill cursor corrupt; scanning from page 1", "channel", chID, "err", err)
		return fresh
	}
	if cur.Tabs == nil {
		cur.Tabs = map[string]*backfillTabCursor{}
	}
	return cur
}

// saveCursor persists the cursor JSON to channel_state.backfill_state.
func (bw *BackfillWorker) saveCursor(chID string, cur *backfillCursor) error {
	raw, err := json.Marshal(cur)
	if err != nil {
		return err // unreachable for this struct; kept for the linter's sake
	}
	return bw.db.SaveBackfillCursor(chID, string(raw))
}
