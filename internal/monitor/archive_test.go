package monitor

import (
	"context"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
)

// foundCall records one OnVideoFound emission — the job pipeline's endpoint
// for the ARCHIVE tests. The host wiring creates the actual job; these tests
// assert the callback contract itself: which video, with which disposition.
type foundCall struct {
	videoID string
	d       JobDisposition
}

// recordVideoFound wires fm.OnVideoFound to a recorder and returns a pointer
// to the recorded calls, in emission order.
func recordVideoFound(fm *FeedMonitor) *[]foundCall {
	calls := &[]foundCall{}
	fm.OnVideoFound = func(videoID, title, url string, ch *config.ChannelConfig, d JobDisposition) {
		*calls = append(*calls, foundCall{videoID: videoID, d: d})
	}
	return calls
}

// TestArchive_HeadlineRegression is THE bug (spec §1): an RSS 404 cycle plus
// a membership listing must not archive a 3-week-old members VOD on a 3-day
// window — stored, never probed, never jobbed. The second, in-window members
// VOD pins the other half of the same §11 gate: even in-window past content
// is not archived until the channel is established (RSS success or backfill),
// so a fresh install whose first cycles 404 cannot treat membership items as
// the whole channel. The moment RSS succeeds, the in-window VOD is archived
// as backlog — and the old one still is not.
func TestArchive_HeadlineRegression(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	memb := membWith(memberVideo("mvOld", "3 weeks ago"), memberVideo("mvNew", "2 days ago"))
	var order []string
	probe := probeReturningTrueAges(now, map[string]int{"mvNew": 2}, &order)

	// Cycle 1: RSS 404s — the original bug's trigger cycle (window default 3,
	// include_non_live_content=true via runCycleForTest).
	fm := newTestFeedMonitor(t, db, withRSS(rss404()), withMembership(memb), withProbe(probe), withNow(now))
	found := recordVideoFound(fm)
	fm.runCycleForTest(t, "UC1")

	if len(*found) != 0 {
		t.Fatalf("RSS-404 cycle archived %v — the headline regression", *found)
	}
	if contains(order, "mvOld") {
		t.Fatalf("the 3-week-old members VOD was probed (order=%v); it is outside the window and in neither Q2 arm", order)
	}
	if got := mustGetFeedItem(t, db, "UC1", "mvOld"); got.Status != "unknown" || got.DatePrecision != "coarse" {
		t.Fatalf("old VOD must be STORED untouched: %+v", got)
	}
	mustGetFeedItem(t, db, "UC1", "mvNew") // the in-window VOD is stored too

	// Cycle 2, ten minutes later (cycles never share an instant — a
	// re-sighting must not read as inserted-now): RSS succeeds (200, zero
	// entries — §11: the FETCH is the gate). The channel establishes THIS
	// cycle, before ARCHIVE reads the gate, so the in-window VOD is admitted
	// now — as backlog (its row predates this cycle). The old VOD stays out
	// forever.
	fm2 := newTestFeedMonitor(t, db, withRSS(rssWith()), withMembership(memb), withProbe(probe), withNow(now.Add(10*time.Minute)))
	found2 := recordVideoFound(fm2)
	fm2.runCycleForTest(t, "UC1")

	if len(*found2) != 1 || (*found2)[0].videoID != "mvNew" || (*found2)[0].d != DispositionBacklogVOD {
		t.Fatalf("established cycle: found=%v, want [{mvNew backlog_vod}]", *found2)
	}
	if contains(order, "mvOld") {
		t.Fatalf("old VOD probed after establishment (order=%v)", order)
	}
}

// TestArchive_Q2CarriesUpcomingPastWindow covers spec §7's Q2 first arm: a
// stream announced 5 days out under a 3-day window leaves Q1 two days before
// it airs — Q2 must keep carrying it, and the cycle it goes live it is jobbed
// DispositionBroadcast. Never missed.
func TestArchive_Q2CarriesUpcomingPastWindow(t *testing.T) {
	db := newTestDB(t)
	now1 := fixedNow()

	// Cycle 1: the announcement lands via RSS (<published> = the announcement
	// instant, §12); the probe classifies it upcoming — dateless, like every
	// broadcast probe.
	fm := newTestFeedMonitor(t, db,
		withRSS(rssWith(rssItem{ID: "up1", Title: "up1", Published: now1.Format(time.RFC3339)})),
		withMembership(membWith()),
		withProbe(func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
			return &VideoProbeResult{StreamStatus: "upcoming", Title: "up1"}, nil
		}),
		withNow(now1))
	found := recordVideoFound(fm)
	fm.runCycleForTest(t, "UC1")
	if len(*found) != 1 || (*found)[0].d != DispositionBroadcast {
		t.Fatalf("announcement cycle: found=%v, want one broadcast job", *found)
	}
	if got := mustGetFeedItem(t, db, "UC1", "up1").Status; got != "upcoming" {
		t.Fatalf("status after cycle 1 = %q, want upcoming", got)
	}

	// Cycle 2, four days later: the announcement date is outside Q1 (window
	// 3), the upcoming status carries the row via Q2, and the probe now
	// returns live ⇒ broadcast job.
	fm2 := newTestFeedMonitor(t, db,
		withRSS(rssWith()), withMembership(membWith()),
		withProbe(func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
			return &VideoProbeResult{StreamStatus: "live", Title: "up1"}, nil
		}),
		withNow(now1.Add(4*24*time.Hour)))
	found2 := recordVideoFound(fm2)
	fm2.runCycleForTest(t, "UC1")
	if len(*found2) != 1 || (*found2)[0].videoID != "up1" || (*found2)[0].d != DispositionBroadcast {
		t.Fatalf("airing cycle: found=%v, want [{up1 broadcast}]", *found2)
	}
}

// TestArchive_WindowRecheckOnProbeDate covers §10's crux: a coarse "1 week
// ago" row is admitted to a 10-day window on an upper bound, but the refresh
// probe reveals a true age of 13 days ⇒ NO job, the exact date is written
// back, and from the next cycle the row is out of Q1 entirely — never probed
// again.
func TestArchive_WindowRecheckOnProbeDate(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	// A vod row: the walk never probes plain vods (§8 — only the 'started'
	// restart carve-out), so the ARCHIVE step's refresh probe is the only
	// probe this row can get.
	seedRow(t, db, "UC1", "w1", now.Add(-7*24*time.Hour), "coarse", "membership", "vod")

	var order []string
	probe := probeReturningTrueAges(now, map[string]int{"w1": 13}, &order)
	fm := newTestFeedMonitor(t, db, withRSS(rssWith()), withMembership(membWith()),
		withProbe(probe), withWindowDays(10), withNow(now))
	found := recordVideoFound(fm)

	fm.runCycleForTest(t, "UC1")
	if len(*found) != 0 {
		t.Fatalf("a 13-day-old VOD was jobbed against a 10-day window: %v", *found)
	}
	if len(order) != 1 || order[0] != "w1" {
		t.Fatalf("refresh probes = %v, want exactly [w1]", order)
	}
	got := mustGetFeedItem(t, db, "UC1", "w1")
	wantPub := now.Add(-13 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if got.Published != wantPub || got.DatePrecision != "day" {
		t.Fatalf("the probe's date must be written back: %+v, want published=%s precision=day", got, wantPub)
	}

	// Next cycle: the corrected date has dropped the row out of Q1, and vod
	// is in neither Q2 arm — no probe, still no job.
	order = nil
	fm.runCycleForTest(t, "UC1")
	if len(order) != 0 || len(*found) != 0 {
		t.Fatalf("out-of-window row re-probed or jobbed next cycle: probes=%v found=%v", order, *found)
	}
}

// TestArchive_FreshReuseNoDoubleProbe covers §10's FRESH-reuse rule: a row
// the walk probed this cycle must NOT be probed again by the archive step —
// exactly one probe per row per cycle — and is still jobbed. It also pins the
// disposition split: the row already in the store jobs as backlog, the row
// first inserted THIS cycle jobs as a new VOD.
func TestArchive_FreshReuseNoDoubleProbe(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	// f1: backlog — in the store before this cycle.
	seedRow(t, db, "UC1", "f1", now.Add(-24*time.Hour), "coarse", "membership", "unknown")

	var order []string
	probe := probeReturningTrueAges(now, map[string]int{"f1": 1, "f2": 1}, &order)
	// f2: new — first listed by RSS this cycle.
	fm := newTestFeedMonitor(t, db,
		withRSS(rssWith(rssItem{ID: "f2", Title: "f2", Published: now.Add(-24 * time.Hour).UTC().Format(time.RFC3339)})),
		withMembership(membWith()), withProbe(probe), withNow(now))
	found := recordVideoFound(fm)
	fm.runCycleForTest(t, "UC1")

	counts := map[string]int{}
	for _, id := range order {
		counts[id]++
	}
	if len(order) != 2 || counts["f1"] != 1 || counts["f2"] != 1 {
		t.Fatalf("probe calls = %v, want exactly one per row (walk probes, archive reuses FRESH)", order)
	}
	got := map[string]JobDisposition{}
	for _, c := range *found {
		got[c.videoID] = c.d
	}
	if len(*found) != 2 || got["f1"] != DispositionBacklogVOD || got["f2"] != DispositionNewVOD {
		t.Fatalf("found = %v, want f1 backlog (pre-existing row) and f2 new (inserted this cycle)", *found)
	}
}

// TestArchive_UnknownNeverJobs_HasProcessedOnlyGatesVod pins the two ends of
// §10's status dispatch. (a) unknown never jobs: an in-window row whose probe
// errors stays unknown and produces nothing this cycle. (b) HasProcessed
// gates ONLY vod/not_a_stream: a history row with no job row (the DECAPI
// give-up shape) must NOT stop a live probe result from jobbing — history
// rows exist without job rows, and gating the live arm on them loses the
// stream forever (§10's goal-3 guard).
func TestArchive_UnknownNeverJobs_HasProcessedOnlyGatesVod(t *testing.T) {
	t.Run("unknown_never_jobs", func(t *testing.T) {
		db := newTestDB(t)
		now := fixedNow()
		seedRow(t, db, "UC1", "u1", now.Add(-1*time.Hour), "exact", "rss", "unknown")
		fm := newTestFeedMonitor(t, db, withRSS(rssWith()), withMembership(membWith()),
			withProbe(stubProbeErrored()), withNow(now))
		found := recordVideoFound(fm)
		fm.runCycleForTest(t, "UC1")
		if len(*found) != 0 {
			t.Fatalf("an unknown row jobbed: %v — we do not know what it is", *found)
		}
		if got := mustGetFeedItem(t, db, "UC1", "u1").Status; got != "unknown" {
			t.Fatalf("status = %q, want unknown (an errored probe writes nothing)", got)
		}
	})

	t.Run("history_never_gates_live", func(t *testing.T) {
		db := newTestDB(t)
		now := fixedNow()
		seedRow(t, db, "UC1", "u2", now.Add(-1*time.Hour), "exact", "rss", "unknown")
		if err := db.AddToHistory("u2"); err != nil { // history row, NO job row
			t.Fatalf("AddToHistory: %v", err)
		}
		// rss404: the live arm is gated by neither HasProcessed NOR the
		// established gate — only past content is.
		fm := newTestFeedMonitor(t, db, withRSS(rss404()), withMembership(membWith()),
			withProbe(func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
				return &VideoProbeResult{StreamStatus: "live", Title: "u2"}, nil
			}), withNow(now))
		found := recordVideoFound(fm)
		fm.runCycleForTest(t, "UC1")
		if len(*found) != 1 || (*found)[0].videoID != "u2" || (*found)[0].d != DispositionBroadcast {
			t.Fatalf("found = %v, want [{u2 broadcast}] — HasProcessed must never gate live/upcoming", *found)
		}
	})
}

// TestArchive_OrphanedHistoryClearRejobsInWindowOnly covers the spec §17
// composite "clearing orphaned history re-jobs an in-window VOD, not an
// out-of-window one" end-to-end. A history row with NO job row (the
// orphaned-history shape: DECAPI give-up, legacy rows, deleted jobs) gates
// vod content via HasProcessed — no job, and no probe wasted on the gated
// row. Deleting the history rows (DeleteHistoryEntries — the same API the
// A O overlay's clear path calls) lifts the gate: the next cycle re-jobs
// the in-window VOD as backlog, while the out-of-window one stays out of
// scope entirely — clearing history must never resurrect content the
// window already excludes.
func TestArchive_OrphanedHistoryClearRejobsInWindowOnly(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	// Two vod rows against the default 3-day window: one a day old, one 30.
	seedRow(t, db, "UC1", "hIn", now.Add(-24*time.Hour), "day", "rss", "vod")
	seedRow(t, db, "UC1", "hOut", now.Add(-30*24*time.Hour), "day", "rss", "vod")
	for _, id := range []string{"hIn", "hOut"} {
		if err := db.AddToHistory(id); err != nil { // history rows, NO job rows
			t.Fatalf("AddToHistory(%s): %v", id, err)
		}
	}

	var order []string
	probe := probeReturningTrueAges(now, map[string]int{"hIn": 1, "hOut": 30}, &order)

	// Cycle 1 (RSS 200-empty establishes in FETCH, before ARCHIVE reads the
	// gate): HasProcessed holds — nothing jobbed, nothing probed.
	fm := newTestFeedMonitor(t, db, withRSS(rssWith()), withMembership(membWith()),
		withProbe(probe), withNow(now))
	found := recordVideoFound(fm)
	fm.runCycleForTest(t, "UC1")
	if len(*found) != 0 {
		t.Fatalf("history-gated cycle jobbed %v, want none", *found)
	}
	if len(order) != 0 {
		t.Fatalf("history-gated cycle probed %v — the gate precedes the refresh probe", order)
	}

	// Clear the orphaned history rows.
	if n, err := db.DeleteHistoryEntries([]string{"hIn", "hOut"}); err != nil || n != 2 {
		t.Fatalf("DeleteHistoryEntries: n=%d err=%v, want 2 rows deleted", n, err)
	}

	// Cycle 2, ten minutes later (cycles never share an instant): the
	// in-window VOD is probed and re-jobbed as backlog (its row predates this
	// cycle); the out-of-window one is outside Q1 and in neither Q2 arm —
	// never probed, never jobbed, history clear or not.
	fm2 := newTestFeedMonitor(t, db, withRSS(rssWith()), withMembership(membWith()),
		withProbe(probe), withNow(now.Add(10*time.Minute)))
	found2 := recordVideoFound(fm2)
	fm2.runCycleForTest(t, "UC1")
	if len(*found2) != 1 || (*found2)[0].videoID != "hIn" || (*found2)[0].d != DispositionBacklogVOD {
		t.Fatalf("post-clear cycle: found=%v, want [{hIn backlog_vod}]", *found2)
	}
	if len(order) != 1 || order[0] != "hIn" {
		t.Fatalf("post-clear probes = %v, want exactly [hIn] — the out-of-window VOD must stay unprobed", order)
	}
}

// TestArchive_CookieLapseLongerThanWindow covers §7/§9's lapse-outlasts-the-
// window case: a members upcoming stream written assumed/unknown before a
// cookie lapse must remain in scope through a lapse LONGER than the window
// (Q2's unresolved arm — its frozen sighting date left Q1 long ago), go
// un-probed while cookies are gone (the probe GATE, not scope, parks it),
// and be probed and jobbed the first cycle cookies return.
func TestArchive_CookieLapseLongerThanWindow(t *testing.T) {
	db := newTestDB(t)
	now1 := fixedNow()

	// Cycle 1, cookies ON: the membership tab lists an undatable item
	// (upcoming ⇒ Age 0 ⇒ assumed/now). Its probe fails — the lapse begins
	// before any successful probe resolves the row.
	fm := newTestFeedMonitor(t, db, withRSS(rss404()),
		withMembership(membWith(memberVideo("mup1", ""))),
		withProbe(stubProbeErrored()), withNow(now1))
	found := recordVideoFound(fm)
	fm.runCycleForTest(t, "UC1")
	if len(*found) != 0 {
		t.Fatalf("cycle 1 jobbed %v, want none (probe errored)", *found)
	}
	if got := mustGetFeedItem(t, db, "UC1", "mup1"); got.Status != "unknown" || got.DatePrecision != "assumed" {
		t.Fatalf("cycle 1 row: %+v, want unknown/assumed", got)
	}

	// Cycle 2, cookies OFF (no membership fetcher wired), 5 days later —
	// past the 3-day window. The sighting date is out of Q1; the unresolved
	// arm still carries the row, and the probe gate parks it: zero probes,
	// zero jobs. (The read arm is the CONFIG toggle, default on — cookie
	// state must not move scope, §9.)
	var probed []string
	fm2 := newTestFeedMonitor(t, db, withRSS(rss404()),
		withProbe(recordingProbe(&probed)), withNow(now1.Add(5*24*time.Hour)))
	found2 := recordVideoFound(fm2)
	fm2.runCycleForTest(t, "UC1")
	if len(probed) != 0 || len(*found2) != 0 {
		t.Fatalf("cookie-lapse cycle: probes=%v found=%v, want none — the gate parks the row, scope keeps it", probed, *found2)
	}

	// Cycle 3, cookies BACK: the row is still in scope, is probed, returns
	// live, and is jobbed — the first cycle it could have been downloaded.
	fm3 := newTestFeedMonitor(t, db, withRSS(rss404()),
		withMembership(membWith()),
		withProbe(func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
			return &VideoProbeResult{StreamStatus: "live", Title: "mup1"}, nil
		}), withNow(now1.Add(5*24*time.Hour)))
	found3 := recordVideoFound(fm3)
	fm3.runCycleForTest(t, "UC1")
	if len(*found3) != 1 || (*found3)[0].videoID != "mup1" || (*found3)[0].d != DispositionBroadcast {
		t.Fatalf("cookie-return cycle: found=%v, want [{mup1 broadcast}]", *found3)
	}
}

// TestArchive_DatelessFreshFallsBackToRowDate: production status probes are
// dateless (ANDROID_VR carries no microformat) — an in-window row holding a
// real date (RSS 'exact') must still archive: the §10 window re-check falls
// back to the row's stored date when the probe supplies none. This is the
// end-to-end half of the hg9j1c9-Z8A reproduction (walk half:
// TestWalk_DatelessProbeHonorsRowDate).
func TestArchive_DatelessFreshFallsBackToRowDate(t *testing.T) {
	db := newTestDB(t)
	now := fixedNow()
	pub := now.Add(-6 * time.Hour).UTC().Format(time.RFC3339)
	fm := newTestFeedMonitor(t, db,
		withRSS(rssWith(rssItem{ID: "dl1", Title: "dateless vod", Published: pub})),
		withMembership(membWith()),
		withProbe(func(ctx context.Context, id string) (*VideoProbeResult, error) {
			return &VideoProbeResult{StreamStatus: "vod", Title: "dateless vod"}, nil
		}),
		withNow(now))
	found := recordVideoFound(fm)
	fm.runCycleForTest(t, "UC1")

	if len(*found) != 1 || (*found)[0].videoID != "dl1" || (*found)[0].d != DispositionNewVOD {
		t.Fatalf("found=%v, want [{dl1 new_vod}] — the row's exact date proves the window", *found)
	}
	if got := mustGetFeedItem(t, db, "UC1", "dl1").Status; got != "vod" {
		t.Fatalf("status = %q, want vod", got)
	}
}
