package monitor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// This file is the feed-history backfill (spec §11): populate the feed_items
// list once per channel from /videos + /streams (+ /membership when
// eligible), page by page, to the window depth, then — when every eligible
// tab ended cleanly — renumber catalog_pos channel-globally and write the
// completion record (backfilled_at). The every-cycle Sweep decides WHO
// scans (the four condition arms + in-flight widen detection), ONE serial
// consumer goroutine runs the scans, and CancelAndPrune removes departed
// channels' data (cancel → wait observed → prune last).

// backfillPageInterval is the global page throttle: one page per second,
// globally — a constant, not config (spec §11 operational rules). Scans run
// strictly serially on the worker's one consumer goroutine, so the
// worker-level clock in waitPage IS the global one.
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
// (spec §11): cleared by completeScan's completion write and by runScan on
// any cancel — resume applies ONLY to interrupted scans, never cancelled ones.
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

// ChannelRef is one configured channel with its CALLER-resolved backfill
// parameters (spec §11): WindowDays is the resolved archive window (the
// per-channel archive_window_days override, else the global) and
// WithMembership is membershipEligibleNow — MembershipDiscoveryEnabled() &&
// HasAuthCookies(), resolved per sweep. Resolution lives at the caller
// (cmd/moombox builds refs the way it builds the archive-slots resolver) so
// the sweep compares plain values.
type ChannelRef struct {
	Ch             *config.ChannelConfig
	ChID           string
	WindowDays     int
	WithMembership bool
}

// inFlight is one queued-or-running scan's control block. windowDays and
// withMembership RECORD what the scan was launched with: mid-scan,
// backfilled_at IS NULL makes the DB sweep condition trivially true and
// backfilled_window_days is only written at completion, so the recorded
// values are the ONLY way Sweep can tell "scanning at 3, config now 30"
// from "scanning at 30" (§11). done closes when the scan has fully left the
// channel — cleanup finished, entry removed — and is CancelAndPrune's
// wait-observed synchronization.
type inFlight struct {
	cancel         context.CancelFunc
	windowDays     int
	withMembership bool
	done           chan struct{}
}

// scanItem is one queue entry: the ref to scan, its cancellable context,
// and its control block.
type scanItem struct {
	ref ChannelRef
	ctx context.Context
	fl  *inFlight
}

// scanFunc is scanChannel's signature — the seam the worker loop invokes
// (bw.scanChannel in production; the sweep tests stub it to script scan
// outcomes without paging fixtures).
type scanFunc func(ctx context.Context, ch *config.ChannelConfig, chID string, windowDays int, withMembership bool) error

// BackfillWorker owns the per-channel backfill scan (spec §11): the
// every-cycle sweep, the in-flight set, the ONE serial scan consumer, and
// the departed-channel prune.
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

	// OnProgress, when set, surfaces scan progress to the host (the monitor
	// package's callback-field idiom — OnVideoFound in feed.go): one call per
	// completed page (state "scanning", pages = pages fetched for that tab
	// THIS session) and one per scan-state change — "done" (clean completion,
	// backfilled_at written), "error" (incomplete — arm (b), a continuation
	// loop, or a fetch/DB failure; the sweep retries), "idle" (scan cancelled
	// or channel pruned — no scan in flight). State-change calls carry tab ""
	// and pages 0: they describe the whole scan, not a tab. Nil-safe; set
	// before Start, like FetchTabPage. Scanning/done/error fire on the serial
	// consumer goroutine; idle can also fire on CancelAndPrune's caller.
	OnProgress func(chID, tab string, pages int, state string)

	// mu guards inflight, queue and baseCtx. Sweep holds it across each
	// channel's check-and-enqueue, so concurrent sweeps (a monitor cycle
	// racing a kickMonitors re-run) can never double-enqueue one channel.
	mu sync.Mutex
	// inflight is the scan-dedup set (§11): channel ID → the queued-or-
	// running scan's control block. Every scan removes its own entry on
	// success, failure AND cancellation (runScan's deferred cleanup) — a
	// leaked entry stalls the channel for the life of the process.
	inflight map[string]*inFlight
	// queue is the pending-scan FIFO consumed by the single goroutine.
	queue []scanItem
	// wakeCh (cap 1) nudges the consumer; a pending nudge coalesces.
	wakeCh chan struct{}
	// baseCtx is Start's context; per-scan contexts derive from it. nil
	// until Start — Sweep no-ops so a pre-start trigger cannot build a
	// queue nothing drains.
	baseCtx context.Context

	// scan is the per-channel scan entrypoint — bw.scanChannel, stubbed in
	// the sweep tests.
	scan scanFunc

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
// before any scan runs, and Start must run before Sweep does anything.
func NewBackfillWorker(db *database.Database, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *BackfillWorker {
	bw := &BackfillWorker{
		db:           db,
		logger:       logger,
		inflight:     make(map[string]*inFlight),
		wakeCh:       make(chan struct{}, 1),
		pageInterval: backfillPageInterval,
		now:          time.Now,
	}
	bw.scan = bw.scanChannel
	return bw
}

// Start launches the ONE scan-consumer goroutine. Its single-ness is load-
// bearing twice over: it is spec §11's "strictly serial across channels"
// rule, and it is the invariant that makes waitPage's unguarded lastPageAt
// a correct GLOBAL 1-page/sec clock — never add a second consumer.
// Idempotent; the worker winds down when ctx is cancelled.
func (bw *BackfillWorker) Start(ctx context.Context) {
	bw.mu.Lock()
	if bw.baseCtx != nil {
		bw.mu.Unlock()
		return
	}
	bw.baseCtx = ctx
	bw.mu.Unlock()

	go func() {
		// Inline panic recovery (project rule). runScan recovers per scan;
		// this outer recover only catches loop-plumbing panics — if it ever
		// fires the consumer is gone and backfills silently stop, so log at
		// Error.
		defer func() {
			if r := recover(); r != nil {
				bw.logger.Error("backfill consumer panic — backfills stopped", "panic", r)
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case <-bw.wakeCh:
			}
			for {
				bw.mu.Lock()
				if len(bw.queue) == 0 {
					bw.mu.Unlock()
					break
				}
				item := bw.queue[0]
				bw.queue = bw.queue[1:]
				bw.mu.Unlock()
				bw.runScan(item)
			}
		}
	}()
}

// runScan executes one queued scan. The deferred cleanup runs on EVERY exit
// path — success, failure, cancellation, panic (§11): reset the cursor if
// the scan was individually cancelled, remove the in-flight entry, close
// done for CancelAndPrune's wait.
func (bw *BackfillWorker) runScan(item scanItem) {
	chID := item.ref.ChID
	defer func() {
		if r := recover(); r != nil {
			bw.logger.Error("backfill scan panic", "channel", chID, "panic", r)
		}
		if item.ctx.Err() != nil && bw.baseCtx.Err() == nil {
			// Cancelled — a widen-restart or a prune, NOT a shutdown (a
			// shutdown is an interruption, and "resumable via cursor"
			// applies only to interrupted scans): clear the cursor. A
			// deeper rescan resuming the shallow cursor would skip exactly
			// the pages it was restarted to fetch; for a prune the whole
			// row is deleted right after anyway (the prune runs last).
			if err := bw.db.SaveBackfillCursor(chID, ""); err != nil {
				bw.logger.Warn("backfill cursor reset failed", "channel", chID, "err", err)
			}
		}
		bw.mu.Lock()
		// A widen-restart replaces the map entry with the deeper scan's
		// while this one is still unwinding — only remove what is OURS.
		if bw.inflight[chID] == item.fl {
			delete(bw.inflight, chID)
		}
		bw.mu.Unlock()
		close(item.fl.done)
	}()

	if item.ctx.Err() != nil {
		// Cancelled while still queued (superseded by a widen, or pruned) —
		// no scan ran, but the state change is still surfaced so a stale
		// entry from an earlier session of this channel is cleared.
		bw.emitProgress(chID, "", 0, "idle")
		return
	}
	err := bw.scan(item.ctx, item.ref.Ch, chID, item.ref.WindowDays, item.ref.WithMembership)
	switch {
	case err == nil:
		// Completed — completeScan wrote backfilled_at; the sweep's first
		// arm goes quiet for this channel.
		bw.emitProgress(chID, "", 0, "done")
	case item.ctx.Err() != nil:
		bw.logger.Info("backfill scan cancelled", "channel", chID)
		bw.emitProgress(chID, "", 0, "idle")
	default:
		// Incomplete: the cursor was saved per page; the sweep retries
		// next cycle (backfilled_at is still NULL) and resumes from it.
		bw.logger.Warn("backfill scan incomplete; sweep will retry", "channel", chID, "err", err)
		bw.emitProgress(chID, "", 0, "error")
	}
}

// emitProgress invokes OnProgress when wired — see the field doc for the
// call contract.
func (bw *BackfillWorker) emitProgress(chID, tab string, pages int, state string) {
	if bw.OnProgress != nil {
		bw.OnProgress(chID, tab, pages, state)
	}
}

// Sweep evaluates the §11 sweep condition for every configured channel,
// enqueues the ones that need (re)scanning, and prunes channels that left
// the config. channels is the CONFIG's channel list with caller-resolved
// values — LEFT-JOIN semantics: a missing channel_state row reads as never
// backfilled. Sweep is IDEMPOTENT: kickMonitors funnels every add, remove,
// reorder, bulk PUT and TUI save here with no discrimination, so an
// already-in-flight, not-stale channel and a completed, condition-false
// channel are both no-ops. Triggered once per feed-monitor cycle — which
// startup and kickMonitors both run — plus the manual re-run.
func (bw *BackfillWorker) Sweep(channels []ChannelRef) {
	bw.mu.Lock()
	if bw.baseCtx == nil || bw.baseCtx.Err() != nil {
		bw.mu.Unlock()
		return // not started, or shutting down — nothing would drain the queue
	}
	active := make(map[string]struct{}, len(channels))
	for _, ref := range channels {
		active[ref.ChID] = struct{}{}
		// YouTube-only ALLOW-list (§11 operational rules), the same gate as
		// scanChannel's defense-in-depth: a non-YouTube channel enqueued
		// here would 404 every tab, never set backfilled_at, and retry
		// forever. Disabled channels are not scanned either — but they STAY
		// in `active`: disabling is a pause, not a removal, and must not
		// prune the channel's history or pending jobs.
		if ref.Ch.GetPlatform() != "youtube" || !ref.Ch.IsEnabled() {
			continue
		}
		if fl, ok := bw.inflight[ref.ChID]; ok {
			// Already queued/scanning: skip UNLESS the running scan is
			// narrower than the config now asks (widen) or membership
			// became eligible mid-scan — the recorded in-flight values are
			// the only signal (§11). Narrowing does nothing: a deeper
			// running scan satisfies every narrower window.
			if fl.windowDays >= ref.WindowDays && (fl.withMembership || !ref.WithMembership) {
				continue
			}
			// Cancel, reset cursor, restart deeper. The cursor reset
			// happens in the CANCELLED scan's cleanup (runScan), which the
			// serial queue runs strictly before the restarted scan —
			// clearing it here instead could race the old scan's final
			// per-page cursor save.
			fl.cancel()
			bw.enqueueLocked(ref)
			continue
		}
		cb, err := bw.db.GetChannelBackfill(ref.ChID)
		if err != nil {
			bw.logger.Warn("backfill sweep: completion read failed", "channel", ref.ChID, "err", err)
			continue
		}
		if !needsBackfill(cb, ref.WindowDays, ref.WithMembership) {
			continue
		}
		bw.enqueueLocked(ref)
	}
	// Departure detection, half 1: in-flight scans of channels no longer in
	// the config. Their rows may not exist yet (a queued scan has written
	// nothing), so the DB diff below can miss them — but a scan of a
	// removed channel must still be cancelled and its footprint pruned.
	departed := make(map[string]struct{})
	for chID := range bw.inflight {
		if _, ok := active[chID]; !ok {
			departed[chID] = struct{}{}
		}
	}
	bw.mu.Unlock()

	// Half 2: channels with feed-history rows not in the config (§11
	// channel removal — "the same sweep prunes departing channels").
	ids, err := bw.db.ListFeedChannelIDs()
	if err != nil {
		bw.logger.Warn("backfill sweep: departure scan failed", "err", err)
	} else {
		for _, id := range ids {
			if _, ok := active[id]; !ok {
				departed[id] = struct{}{}
			}
		}
	}
	for id := range departed {
		bw.CancelAndPrune(id)
	}
}

// needsBackfill is the sweep condition's four DB arms (spec §11), evaluated
// with SQL NULL semantics over GetChannelBackfill's shape (a missing
// channel_state row reads all-NULL — never backfilled):
//
//	backfilled_at IS NULL
//	OR backfilled_window_days IS NULL           -- NULL < x is NULL, not true.
//	OR backfilled_window_days < resolvedWindow  -- Without the NULL arm the
//	                                            -- widen check never fires.
//	OR (backfilled_with_membership = 0 AND membershipEligibleNow)
//
// The membership arm keeps SQL's `= 0` semantics: NULL = 0 is NULL, not
// true — only a RECORDED "scanned without membership" plus current
// eligibility rescans. Toggle-OFF has no arm at all: members rows are
// hidden by the read arm, not deleted, and a completed with-membership scan
// is a superset of what is now eligible.
func needsBackfill(cb database.ChannelBackfill, windowDays int, membershipEligible bool) bool {
	return cb.At == "" ||
		cb.WindowDays == nil ||
		*cb.WindowDays < windowDays ||
		(cb.WithMembership != nil && !*cb.WithMembership && membershipEligible)
}

// enqueueLocked (bw.mu held) records the scan in the in-flight set —
// REPLACING any previous entry, which is the widen-restart path — appends
// it to the serial queue, and nudges the consumer.
func (bw *BackfillWorker) enqueueLocked(ref ChannelRef) {
	ctx, cancel := context.WithCancel(bw.baseCtx)
	fl := &inFlight{cancel: cancel, windowDays: ref.WindowDays,
		withMembership: ref.WithMembership, done: make(chan struct{})}
	bw.inflight[ref.ChID] = fl
	bw.queue = append(bw.queue, scanItem{ref: ref, ctx: ctx, fl: fl})
	select {
	case bw.wakeCh <- struct{}{}:
	default:
	}
	bw.logger.Debug("backfill scan queued", "channel", ref.ChID,
		"windowDays", ref.WindowDays, "withMembership", ref.WithMembership)
}

// CancelAndPrune removes a departed channel's feed-history footprint (§11
// channel removal): cancel any in-flight scan, WAIT until it has observed
// the cancellation and fully left the channel, then prune — the channel's
// never-started jobs (Queued, Upcoming, COOKIES? — together with their
// history rows: an orphaned history row would make HasProcessed lie forever
// and block the rename-and-re-add path) and its feed data (feed_items +
// channel_state). Both deletes run AFTER the wait, so a stale page written
// in the cancel window is cleaned up too. Live/Downloading/Muxing jobs keep
// running; terminal rows stay.
//
// A QUEUED-but-not-running scan is settled directly — spliced out of the
// queue, entry removed, done closed — rather than waited on: its done only
// closes when the serial consumer REACHES the item, which means every scan
// ahead of it running to completion first at 1 page/sec, and CancelAndPrune
// executes synchronously on the feed monitor's cycle goroutine — waiting
// would stall RSS polling and live detection for minutes on a full queue.
// The blocking wait applies only to a genuinely RUNNING scan, which
// observes cancellation within one page.
func (bw *BackfillWorker) CancelAndPrune(chID string) {
	for {
		bw.mu.Lock()
		fl := bw.inflight[chID]
		if fl == nil {
			bw.mu.Unlock()
			break
		}
		spliced := false
		for i := range bw.queue {
			if bw.queue[i].fl == fl {
				// Still queued: the splice-vs-pop race is settled by bw.mu —
				// the consumer pops under the same lock, so the item is
				// either fully ours (never runs, we close done) or fully the
				// consumer's (runScan closes done; we wait below).
				bw.queue = append(bw.queue[:i], bw.queue[i+1:]...)
				delete(bw.inflight, chID) // fl was read under this same lock
				spliced = true
				break
			}
		}
		base := bw.baseCtx
		bw.mu.Unlock()
		fl.cancel() // release the derived context either way
		if spliced {
			close(fl.done)
			continue // re-check: a concurrent sweep may have re-enqueued
		}
		select {
		case <-fl.done:
		case <-base.Done():
			// Shutdown: the consumer may never drain this entry, so don't
			// hang the caller. The channel's rows are still there — the
			// next boot's sweep re-detects the departure and prunes then.
			return
		}
		// Loop: a concurrent widen may have replaced the entry.
	}
	// Delete order (controller-adjudicated, deviating from the brief's
	// listing order): jobs+history FIRST, feed data SECOND. The spec lists
	// the two deletes as unordered bullets, and feed-data-first manufactures
	// the one UN-RETRYABLE orphan class on partial failure: the feed-data
	// delete removes the channel from ListFeedChannelIDs' census, so a
	// failed jobs delete would never be retried — Upcoming jobs re-enqueue
	// every 60s against a deleted channel and history rows block
	// rename-and-re-add forever (the exact §11 harms). Jobs-first is
	// self-healing: the jobs delete is idempotent on retry, and a failed
	// feed-data delete leaves the channel in the census so the next sweep
	// re-runs the whole prune.
	n, err := bw.db.DeleteJobsAndHistoryForChannel(chID,
		[]database.JobStatus{database.StatusQueued, database.StatusUpcoming, database.StatusCookies})
	if err != nil {
		bw.logger.Error("backfill prune: delete jobs failed", "channel", chID, "err", err)
		return // channel still in the census — the next sweep retries the whole prune
	}
	if err := bw.db.DeleteChannelFeedData(chID); err != nil {
		bw.logger.Error("backfill prune: delete feed data failed", "channel", chID, "err", err)
		return // ditto — census entry survives, next sweep re-prunes
	}
	bw.logger.Info("backfill pruned departed channel", "channel", chID, "jobsDeleted", n)
	// The channel is gone: clear any progress state the UIs still hold for
	// it. Covers the spliced-queued prune and the no-in-flight boot prune,
	// where no runScan exit ever fires — for a cancelled RUNNING scan this
	// duplicates runScan's idle, which clears an already-clear entry.
	bw.emitProgress(chID, "", 0, "idle")
}

// scanChannel runs one channel's backfill scan: each eligible tab, page by
// page, persisting every page immediately and advancing the per-tab cursor
// (spec §11 — buffering would break "resumable via cursor"). When EVERY
// eligible tab ends cleanly it runs the ordering pass and writes the
// completion record (completeScan), returning nil; any incomplete tab
// (arm (b), loop, fetch failure) surfaces as an error instead — no renumber,
// no backfilled_at — so the sweep retries, resuming from the saved
// cursor.
func (bw *BackfillWorker) scanChannel(ctx context.Context, ch *config.ChannelConfig, chID string, windowDays int, withMembership bool) error {
	// YouTube channels only — an ALLOW-list, stricter than getYouTubeChannels'
	// twitch-exclusion (spec §11 operational rules: a Twitch channel scanned
	// as youtube.com/channel/<twitch_login> 404s on every tab, never sets
	// backfilled_at, and retries forever). Sweep applies the same gate when
	// selecting channels; this guard is defense-in-depth that
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
	// saved per page, backfilled_at stays NULL, and the sweep
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
	// pages counts the pages COMPLETED (written + cursor saved) for this tab
	// this session, for the OnProgress "scanning" emissions. Session-local
	// like lastDatable — a resumed tab restarts at 1, honestly reporting
	// this session's work rather than reconstructing history.
	pages := 0

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
				// in-scanner retry loop. The sweep re-runs the scan,
				// which resumes from the saved cursor.
				bw.logger.Warn("backfill page fetch failed; tab resumes from cursor on next sweep",
					"channel", chID, "tab", tab, "err", err)
			}
			return fmt.Errorf("backfill %s/%s: %w", chID, tab, err)
		}

		// Re-check cancellation between the fetch and the page WRITE (spec
		// §11): a sweep cancel (widen-restart or prune) landing mid-page is
		// caught here, so a cancellation costs at most one stale page —
		// which the prune, running last, removes.
		if err := ctx.Err(); err != nil {
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
		// Page completion (spec §11 progress surfacing): emitted only after
		// the write + cursor save landed — a failed or parser-arm page is not
		// a completed page and emits nothing (the scan-level "error" from
		// runScan covers those endings).
		pages++
		bw.emitProgress(chID, tab, pages, "scanning")
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
