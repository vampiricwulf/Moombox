package database

import (
	"strings"
	"testing"
	"time"
)

func fi(ch, vid, pub, prec, src, status string, pos int) FeedItem {
	return FeedItem{ChannelID: ch, VideoID: vid, Published: pub, DatePrecision: prec,
		CatalogPos: pos, Source: src, Status: status, FirstSeen: "2026-07-16T00:00:00Z"}
}

func TestUpsertFeedItem_GuardAndAlwaysArms(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()
	// Insert coarse; insertedNow true; status forced 'unknown' whatever caller sets.
	it := fi("UC1", "v1", "2026-07-10T00:00:00Z", "coarse", "membership", "vod", 3)
	ins, err := db.UpsertFeedItem(it)
	if err != nil || !ins {
		t.Fatalf("insert: ins=%v err=%v", ins, err)
	}
	got := mustGetFeedItem(t, db, "UC1", "v1")
	if got.Status != "unknown" {
		t.Fatalf("INSERT wrote status %q; a listing supplies a DATE, never a classification (§6)", got.Status)
	}
	// Re-sight with a BETTER date (exact) and a new source: date upgrades, source
	// and catalog_pos always update, insertedNow false, first_seen unchanged.
	it2 := fi("UC1", "v1", "2026-07-12T09:00:00Z", "exact", "rss", "unknown", 0)
	it2.FirstSeen = "2026-07-16T01:00:00Z"
	ins, err = db.UpsertFeedItem(it2)
	if err != nil || ins {
		t.Fatalf("update: ins=%v err=%v (RETURNING first_seen must reveal update-vs-insert)", ins, err)
	}
	got = mustGetFeedItem(t, db, "UC1", "v1")
	if got.DatePrecision != "exact" || got.Source != "rss" || got.CatalogPos != 0 || got.FirstSeen != "2026-07-16T00:00:00Z" {
		t.Fatalf("after upgrade: %+v", got)
	}
	// Re-sight with a WORSE date (coarse): date arms untouched, source still moves.
	it3 := fi("UC1", "v1", "2026-07-01T00:00:00Z", "coarse", "membership", "unknown", 7)
	db.UpsertFeedItem(it3)
	got = mustGetFeedItem(t, db, "UC1", "v1")
	if got.Published != "2026-07-12T09:00:00Z" || got.DatePrecision != "exact" {
		t.Fatal("a worse estimate overwrote a better one — the guard is broken")
	}
	if got.Source != "membership" || got.CatalogPos != 7 {
		t.Fatal("source/catalog_pos are ALWAYS arms and must move on every sighting")
	}
}

func TestApplyProbeToFeedItem(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()
	db.UpsertFeedItem(fi("UC1", "v1", "2026-07-16T00:00:00Z", "assumed", "membership", "unknown", 0))
	// Probe: vod with a started date → status flips, date upgrades, source untouched.
	if err := db.ApplyProbeToFeedItem("UC1", "v1", "vod", "Real Title", "2026-07-14T20:00:00Z", "started", FeedRecheck{}); err != nil {
		t.Fatal(err)
	}
	got := mustGetFeedItem(t, db, "UC1", "v1")
	if got.Status != "vod" || got.DatePrecision != "started" || got.Title != "Real Title" || got.Source != "membership" {
		t.Fatalf("probe write: %+v", got)
	}
	// A later probe with a WORSE date must not downgrade; a stale-listing-shaped
	// status write is the caller's responsibility to prevent, but 'Unknown Title'
	// must never overwrite a real title.
	db.ApplyProbeToFeedItem("UC1", "v1", "vod", "Unknown Title", "2026-07-13T00:00:00Z", "day", FeedRecheck{})
	got = mustGetFeedItem(t, db, "UC1", "v1")
	if got.DatePrecision != "started" || got.Title != "Real Title" {
		t.Fatalf("probe downgrade leaked: %+v", got)
	}
	// upcoming supplies no date: rank 0 no-op, status still lands.
	if err := db.ApplyProbeToFeedItem("UC1", "v1", "upcoming", "", "", "", FeedRecheck{}); err != nil {
		t.Fatal(err)
	}
	got = mustGetFeedItem(t, db, "UC1", "v1")
	if got.Status != "upcoming" || got.Published != "2026-07-14T20:00:00Z" {
		t.Fatalf("dateless probe: %+v (must never write published='')", got)
	}
}

func TestFeedScope_Q1UnionQ2(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	cutoff := now.AddDate(0, 0, -3).Format(time.RFC3339)
	old := now.AddDate(0, 0, -30).Format(time.RFC3339)
	recent := now.AddDate(0, 0, -1).Format(time.RFC3339)

	db.UpsertFeedItem(fi("UC1", "in", recent, "exact", "rss", "unknown", 0))   // Q1
	db.UpsertFeedItem(fi("UC1", "out", old, "coarse", "videos", "unknown", 1)) // neither
	db.UpsertFeedItem(fi("UC1", "up", old, "assumed", "rss", "unknown", 2))    // Q2 via status
	db.ApplyProbeToFeedItem("UC1", "up", "upcoming", "", "", "", FeedRecheck{})
	db.UpsertFeedItem(fi("UC1", "unres", old, "assumed", "membership", "unknown", 3)) // Q2 unresolved arm (§7)
	db.UpsertFeedItem(fi("UC1", "memb", recent, "coarse", "membership", "unknown", 4))

	got, err := db.FeedScope("UC1", cutoff, now.Format(time.RFC3339), true)
	if err != nil {
		t.Fatal(err)
	}
	ids := idsOf(got)
	for _, want := range []string{"in", "up", "unres", "memb"} {
		if !ids[want] {
			t.Errorf("scope missing %q", want)
		}
	}
	if ids["out"] {
		t.Error("out-of-window coarse row leaked into scope")
	}
	// The read arm is the OPERATOR'S toggle: membership rows drop from BOTH arms.
	got, _ = db.FeedScope("UC1", cutoff, now.Format(time.RFC3339), false)
	ids = idsOf(got)
	if ids["memb"] || ids["unres"] {
		t.Error("membership rows visible with the toggle off")
	}
	if !ids["in"] || !ids["up"] {
		t.Error("read arm removed non-membership rows")
	}
}

// An RSS <published> is the announcement time, so a stream scheduled further
// ahead than the window is older than the cutoff the moment it is first seen
// — when its channel is added, or after downtime. It used to sit in neither
// arm and was never probed, even once live. An unknown RSS row now stays in
// scope while its first sighting is inside the window; a row first seen
// before the window, and an unknown exact row from any other source, do not.
//
// Mutants: the arm without the first_seen bound (stale row in scope), and
// without the source check (old 'videos' row in scope).
func TestFeedScope_UnresolvedRSSRowFirstSeenInsideTheWindow(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	cutoff := now.AddDate(0, 0, -3).Format(time.RFC3339)
	announced := now.AddDate(0, 0, -10).Format(time.RFC3339)
	seenNow := now.Format(time.RFC3339)

	add := func(vid, src, firstSeen string, pos int) {
		t.Helper()
		it := fi("UC1", vid, announced, "exact", src, "unknown", pos)
		it.FirstSeen = firstSeen
		if _, err := db.UpsertFeedItem(it); err != nil {
			t.Fatal(err)
		}
	}
	add("scheduled", "rss", seenNow, 0)
	add("stale", "rss", now.AddDate(0, 0, -20).Format(time.RFC3339), 1)
	add("listed", "videos", seenNow, 2)

	got, err := db.FeedScope("UC1", cutoff, now.Format(time.RFC3339), false)
	if err != nil {
		t.Fatal(err)
	}
	ids := idsOf(got)
	if !ids["scheduled"] {
		t.Error("an unresolved RSS row first seen inside the window must be in scope, whatever its announcement date")
	}
	if ids["stale"] {
		t.Error("an unresolved RSS row first seen before the window must stay out of scope")
	}
	if ids["listed"] {
		t.Error("an unresolved exact row from a dated listing must stay out of scope")
	}

	// Once a probe resolves it, the row leaves the arm: an upcoming one is
	// carried by status, a VOD is judged by its date like any other.
	if err := db.ApplyProbeToFeedItem("UC1", "scheduled", "vod", "", "", "", FeedRecheck{}); err != nil {
		t.Fatal(err)
	}
	got, _ = db.FeedScope("UC1", cutoff, now.Format(time.RFC3339), false)
	if idsOf(got)["scheduled"] {
		t.Error("a resolved VOD older than the window must leave scope")
	}
}

func TestFeedScope_QueryPlan(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()
	vid := func(i int) string {
		return "v" + strings.Repeat("x", 2) + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26))
	}
	for i := range 1500 { // enough rows that a bad plan is visible
		db.UpsertFeedItem(fi("UC1", vid(i),
			time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i)*time.Hour).Format(time.RFC3339),
			"coarse", "videos", "unknown", i))
	}
	// Give status real selectivity within the channel — as in production, most
	// feed items resolve to a terminal status over time and only a minority
	// stay in Q2's open set. Without this every one of the 1500 rows shares
	// both channel_id and status, so SQLite's cost-based planner correctly
	// prefers a full scan over the non-covering status index (empirically
	// verified: same query against this exact all-'unknown' population always
	// plans as SCAN, regardless of SQL phrasing — this is not a planner bug).
	for i := range 1450 {
		db.ApplyProbeToFeedItem("UC1", vid(i), "vod", "T", "2026-01-01T00:00:00Z", "exact", FeedRecheck{})
	}
	db.db.Exec(`ANALYZE`)
	assertPlan := func(q string, args []any, mustContain string) {
		rows, err := db.db.Query("EXPLAIN QUERY PLAN "+q, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var all string
		for rows.Next() {
			var a, b, c int
			var d string
			rows.Scan(&a, &b, &c, &d)
			all += d + "\n"
		}
		if !strings.Contains(all, mustContain) || strings.Contains(all, "TEMP B-TREE") {
			t.Fatalf("plan for %q:\n%s\nwant %q, no TEMP B-TREE", q, all, mustContain)
		}
	}
	assertPlan(feedScopeQ1SQL(false), []any{"UC1", "2026-01-01T00:00:00Z"}, "idx_feed_items_window")
	assertPlan(feedScopeQ2SQL(false), []any{"UC1"}, "idx_feed_items_status")
}

func TestDeleteChannelFeedData(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()
	db.UpsertFeedItem(fi("UC1", "v1", "2026-07-10T00:00:00Z", "coarse", "rss", "unknown", 0))
	db.UpsertFeedItem(fi("UC2", "v2", "2026-07-10T00:00:00Z", "coarse", "rss", "unknown", 0))
	if err := db.SetChannelRSSOK("UC1", "2026-07-16T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetChannelRSSOK("UC2", "2026-07-16T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteChannelFeedData("UC1"); err != nil {
		t.Fatal(err)
	}
	// Both UC1 rows gone (the two DELETEs commit together); UC2 untouched.
	var n int
	db.db.QueryRow(`SELECT COUNT(*) FROM feed_items WHERE channel_id='UC1'`).Scan(&n)
	if n != 0 {
		t.Errorf("feed_items rows remain: %d", n)
	}
	db.db.QueryRow(`SELECT COUNT(*) FROM channel_state WHERE channel_id='UC1'`).Scan(&n)
	if n != 0 {
		t.Errorf("channel_state row remains: %d", n)
	}
	db.db.QueryRow(`SELECT COUNT(*) FROM feed_items WHERE channel_id='UC2'`).Scan(&n)
	if n != 1 {
		t.Errorf("other channel's feed_items affected: %d", n)
	}
	db.db.QueryRow(`SELECT COUNT(*) FROM channel_state WHERE channel_id='UC2'`).Scan(&n)
	if n != 1 {
		t.Errorf("other channel's channel_state affected: %d", n)
	}
}

// TestListFeedChannelIDs pins the sweep's departure census (spec §11): the
// UNION of channel_state and feed_items owners, deduped — a channel present
// in either table is listed once, and a pruned channel drops out entirely.
func TestListFeedChannelIDs(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()

	if ids, err := db.ListFeedChannelIDs(); err != nil || len(ids) != 0 {
		t.Fatalf("empty db: ids=%v err=%v, want none", ids, err)
	}

	// UC1: feed_items only. UC2: channel_state only. UC3: both (must dedupe).
	db.UpsertFeedItem(fi("UC1", "v1", "2026-07-10T00:00:00Z", "coarse", "rss", "unknown", 0))
	if err := db.SetChannelRSSOK("UC2", "2026-07-16T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	db.UpsertFeedItem(fi("UC3", "v3", "2026-07-10T00:00:00Z", "coarse", "rss", "unknown", 0))
	if err := db.SaveBackfillCursor("UC3", `{"tabs":{}}`); err != nil {
		t.Fatal(err)
	}

	ids, err := db.ListFeedChannelIDs()
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]int)
	for _, id := range ids {
		got[id]++
	}
	for _, want := range []string{"UC1", "UC2", "UC3"} {
		if got[want] != 1 {
			t.Errorf("%s listed %d times, want exactly 1 (ids=%v)", want, got[want], ids)
		}
	}
	if len(ids) != 3 {
		t.Errorf("len(ids) = %d, want 3 (%v)", len(ids), ids)
	}

	// After a prune the channel leaves the census — the sweep stops re-firing.
	if err := db.DeleteChannelFeedData("UC3"); err != nil {
		t.Fatal(err)
	}
	ids, err = db.ListFeedChannelIDs()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id == "UC3" {
			t.Errorf("pruned channel still in census: %v", ids)
		}
	}
}

func TestGetFeedItem(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()
	if it, err := db.GetFeedItem("UC1", "missing"); err != nil || it != nil {
		t.Fatalf("missing row: it=%+v err=%v", it, err)
	}
	db.UpsertFeedItem(fi("UC1", "v1", "2026-07-10T00:00:00Z", "coarse", "rss", "unknown", 0))
	it, err := db.GetFeedItem("UC1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if it == nil || it.DatePrecision != "coarse" || it.Source != "rss" {
		t.Fatalf("got %+v", it)
	}
}

// TestSetChannelRSSOK reads the column back directly: production reads it
// only through GetChannelEstablished (a getter for it alone had no caller).
func TestSetChannelRSSOK(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()
	if err := db.SetChannelRSSOK("UC1", "2026-07-16T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	var ts string
	if err := db.db.QueryRow(`SELECT last_rss_ok_at FROM channel_state WHERE channel_id = ?`, "UC1").Scan(&ts); err != nil || ts != "2026-07-16T00:00:00Z" {
		t.Fatalf("got ts=%q err=%v", ts, err)
	}
	if est, err := db.GetChannelEstablished("UC1"); err != nil || !est {
		t.Errorf("GetChannelEstablished after an RSS ok = %v, %v; want true", est, err)
	}
}

func TestGetChannelEstablished(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()
	// No channel_state row at all: NOT established (§11).
	if est, err := db.GetChannelEstablished("UC1"); err != nil || est {
		t.Fatalf("missing row: est=%v err=%v, want false", est, err)
	}
	// last_rss_ok_at alone establishes.
	if err := db.SetChannelRSSOK("UC1", "2026-07-16T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if est, err := db.GetChannelEstablished("UC1"); err != nil || !est {
		t.Fatalf("last_rss_ok_at set: est=%v err=%v, want true", est, err)
	}
	// backfilled_at alone establishes too (a channel whose RSS 404s forever).
	if _, err := db.db.Exec(`INSERT INTO channel_state (channel_id, backfilled_at) VALUES ('UC2', '2026-07-16T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if est, err := db.GetChannelEstablished("UC2"); err != nil || !est {
		t.Fatalf("backfilled_at set: est=%v err=%v, want true", est, err)
	}
	// A row present with BOTH keys NULL is still not established.
	if _, err := db.db.Exec(`INSERT INTO channel_state (channel_id, backfill_state) VALUES ('UC3', '{}')`); err != nil {
		t.Fatal(err)
	}
	if est, err := db.GetChannelEstablished("UC3"); err != nil || est {
		t.Fatalf("both keys NULL: est=%v err=%v, want false", est, err)
	}
}

// TestFeedItems_PublishedFirstSeenAlwaysZ is the permanent guard on the
// Z-suffix invariant: every published/first_seen string entering feed_items
// is RFC3339 UTC ("Z"-suffixed), because the schema's contract is
// "lexicographic order IS chronological order" — FeedScope's Q1 window
// compare, the idx_feed_items_window ORDER BY, the archive window re-check
// and the walk's exhaustion math all compare these strings lexically against
// Z-format cutoffs. An offset-bearing value ("+00:00", "-07:00") mis-orders
// by hours and, at day/started rank, can never be corrected (rank ties don't
// overwrite).
//
// All three production writers funnel through UpsertFeedItem or
// ApplyProbeToFeedItem, so the guard exercises representative inputs from
// each, built with the writers' own expressions:
//   - the STORE step's RSS arm (time.Parse(<published>).UTC().Format — the
//     offset input here is the shape YouTube feeds actually emit), its
//     membership coarse/assumed arms, and its zero-date assumed arm;
//   - the backfill's tab-page writes (scanNow-derived coarse/assumed);
//   - the probe write (extractPublishedAt's Z-normalized started/day output —
//     the seam the youtube-side producer fix closes).
//
// This is a TEST-ONLY invariant by design: the DB layer deliberately carries
// no runtime validation, so this test is the tripwire for any future writer
// (or writer change) that would smuggle a non-Z string into the table.
func TestFeedItems_PublishedFirstSeenAlwaysZ(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()
	cycleNow := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC) // checkChannel: fm.now().UTC()
	first := cycleNow.Format(time.RFC3339)

	// STORE, RSS arm: feeds emit offset-bearing <published> ("+00:00"); the
	// step parses then re-formats via .UTC() — production's exact expression.
	rssPub, err := time.Parse(time.RFC3339, "2026-07-13T04:00:00+00:00")
	if err != nil {
		t.Fatal(err)
	}
	upsert := func(vid, pub, prec, src string, pos int) {
		t.Helper()
		if _, err := db.UpsertFeedItem(FeedItem{
			ChannelID: "UC1", VideoID: vid, Title: vid,
			Published: pub, DatePrecision: prec,
			CatalogPos: pos, Source: src, FirstSeen: first,
		}); err != nil {
			t.Fatalf("upsert %s: %v", vid, err)
		}
	}
	upsert("rssExact", rssPub.UTC().Format(time.RFC3339), "exact", "rss", 0)
	upsert("rssNoDate", first, "assumed", "rss", 1) // zero-date arm: assumed/cycle-now
	// STORE, membership arms: coarse now-Age / assumed now.
	upsert("membCoarse", cycleNow.Add(-21*24*time.Hour).Format(time.RFC3339), "coarse", "membership", 2)
	upsert("membAssumed", first, "assumed", "membership", 3)
	// Backfill writes: scanNow-derived, same shapes, tab-name sources.
	scanNow := cycleNow.Add(30 * time.Minute)
	upsert("bfCoarse", scanNow.Add(-7*24*time.Hour).Format(time.RFC3339), "coarse", "videos", 4)
	upsert("bfAssumed", scanNow.Format(time.RFC3339), "assumed", "streams", 5)

	// Probe writes: extractPublishedAt's Z-normalized output — 'started' is
	// the normalized form of YouTube's "+00:00" startTimestamp, 'day' covers
	// both the bare-date T23:59:59Z arm and the normalized "-07:00" form.
	probes := map[string][2]string{
		"rssExact":   {"2026-07-13T02:00:00Z", "started"},
		"membCoarse": {"2026-06-24T23:59:59Z", "day"},
		"bfCoarse":   {"2026-07-08T20:00:00Z", "day"},
	}
	for vid, p := range probes {
		if err := db.ApplyProbeToFeedItem("UC1", vid, "vod", vid, p[0], p[1], FeedRecheck{}); err != nil {
			t.Fatalf("probe write %s: %v", vid, err)
		}
	}

	rows, err := db.db.Query(`SELECT video_id, published, first_seen FROM feed_items`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var vid, pub, seen string
		if err := rows.Scan(&vid, &pub, &seen); err != nil {
			t.Fatal(err)
		}
		n++
		if !strings.HasSuffix(pub, "Z") {
			t.Errorf("%s: published %q is not Z-suffixed — lexicographic order is no longer chronological", vid, pub)
		}
		if !strings.HasSuffix(seen, "Z") {
			t.Errorf("%s: first_seen %q is not Z-suffixed", vid, seen)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 6 {
		t.Fatalf("scanned %d rows, want 6 — the guard must see every writer's row", n)
	}
}

func mustGetFeedItem(t *testing.T, db *Database, channelID, videoID string) FeedItem {
	t.Helper()
	it := FeedItem{ChannelID: channelID, VideoID: videoID}
	err := db.db.QueryRow(`SELECT title, published, date_precision, catalog_pos, source, status, first_seen, recheck_at, recheck_step
		FROM feed_items WHERE channel_id = ? AND video_id = ?`, channelID, videoID).Scan(
		&it.Title, &it.Published, &it.DatePrecision, &it.CatalogPos, &it.Source, &it.Status, &it.FirstSeen, &it.RecheckAt, &it.RecheckStep)
	if err != nil {
		t.Fatalf("mustGetFeedItem(%s, %s): %v", channelID, videoID, err)
	}
	return it
}

func idsOf(items []FeedItem) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, it := range items {
		out[it.VideoID] = true
	}
	return out
}

// An unconfirmed broadcast end's re-check (FeedItem.RecheckAt) only ever
// adds a row to scope: due, Q3 carries it wherever its date is; pending, it
// is in Q1 or Q2 exactly where any row would be — the in-window row, the
// unresolved assumed row — and out of scope where any row would be (the
// vod outside the window). The membership toggle reaches Q3 like the other
// arms.
//
// Mutants: a pending row kept out of Q1, as 69342f6b did (the in-window
// pending row missing — the restart carve-out never sees it), or out of Q2
// (the assumed pending row missing); Q3 dropped (the due rows outside the
// window missing); Q3 without its due bound (the pending row outside the
// window in scope); Q3 ignoring the membership toggle.
func TestFeedScope_TheEndRecheckDecidesScope(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	nowStr := now.Format(time.RFC3339)
	cutoff := now.AddDate(0, 0, -3).Format(time.RFC3339)
	recent := now.AddDate(0, 0, -1).Format(time.RFC3339)
	old := now.AddDate(0, 0, -30).Format(time.RFC3339)
	pending := FeedRecheck{At: now.Add(time.Hour).Format(time.RFC3339), Step: 1}
	due := FeedRecheck{At: now.Add(-time.Minute).Format(time.RFC3339), Step: 2}
	add := func(vid, pub, prec, src, status string, rc FeedRecheck) {
		t.Helper()
		if _, err := db.UpsertFeedItem(fi("UC1", vid, pub, prec, src, "unknown", 0)); err != nil {
			t.Fatal(err)
		}
		if err := db.ApplyProbeToFeedItem("UC1", vid, status, vid, pub, prec, rc); err != nil {
			t.Fatal(err)
		}
	}
	add("inWindowPending", recent, "started", "rss", "vod", pending)
	add("assumedPending", old, "assumed", "rss", "unknown", pending)
	add("oldPending", old, "exact", "rss", "vod", pending)
	add("oldDue", old, "exact", "rss", "vod", due)
	add("inWindowDue", recent, "exact", "rss", "vod", due)
	add("membDue", old, "exact", "membership", "vod", due)
	add("settled", old, "exact", "rss", "vod", FeedRecheck{})

	got, err := db.FeedScope("UC1", cutoff, nowStr, true)
	if err != nil {
		t.Fatal(err)
	}
	ids := idsOf(got)
	for _, want := range []string{"oldDue", "inWindowDue", "membDue"} {
		if !ids[want] {
			t.Errorf("scope missing the due re-check %q", want)
		}
	}
	for _, want := range []string{"inWindowPending", "assumedPending"} {
		if !ids[want] {
			t.Errorf("scope missing %q: a pending re-check must not take a row out of the arm that carries it", want)
		}
	}
	for _, not := range []string{"oldPending", "settled"} {
		if ids[not] {
			t.Errorf("%q is in scope: an out-of-window vod is in no arm until its re-check is due", not)
		}
	}
	for _, it := range got {
		if it.VideoID == "oldDue" && (it.RecheckAt != due.At || it.RecheckStep != due.Step) {
			t.Errorf("scope row carries re-check %q step %d, want %q %d", it.RecheckAt, it.RecheckStep, due.At, due.Step)
		}
	}
	got, _ = db.FeedScope("UC1", cutoff, nowStr, false)
	if idsOf(got)["membDue"] {
		t.Error("a membership row's re-check reached scope with the toggle off")
	}
}

// The re-check rides the probe write, in the same statement: written with
// the status that needs it, and cleared by the next write that does not.
//
// Mutants: the recheck columns left out of the UPDATE (nothing written);
// written only when non-zero (a settled write leaves the old one pending).
func TestApplyProbeToFeedItem_WritesAndClearsTheRecheck(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()
	db.UpsertFeedItem(fi("UC1", "v1", "2026-07-10T00:00:00Z", "exact", "rss", "unknown", 0))
	rc := FeedRecheck{At: "2026-07-16T13:00:00Z", Step: 1}
	if err := db.ApplyProbeToFeedItem("UC1", "v1", "vod", "", "", "", rc); err != nil {
		t.Fatal(err)
	}
	if got := mustGetFeedItem(t, db, "UC1", "v1"); got.RecheckAt != rc.At || got.RecheckStep != rc.Step {
		t.Fatalf("re-check %q step %d, want %q %d", got.RecheckAt, got.RecheckStep, rc.At, rc.Step)
	}
	if err := db.ApplyProbeToFeedItem("UC1", "v1", "upcoming", "", "", "", FeedRecheck{}); err != nil {
		t.Fatal(err)
	}
	if got := mustGetFeedItem(t, db, "UC1", "v1"); got.RecheckAt != "" || got.RecheckStep != 0 {
		t.Fatalf("a settled write left re-check %q step %d", got.RecheckAt, got.RecheckStep)
	}
}

// Q3 reads through its own index, in order, whatever the rest of the
// channel holds.
//
// Mutant: the index dropped (or not covering recheck_at) — a SCAN.
func TestFeedScope_Q3QueryPlan(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()
	for i := range 600 {
		vid := "v" + strings.Repeat("y", 2) + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
		db.UpsertFeedItem(fi("UC1", vid, "2026-01-01T00:00:00Z", "exact", "videos", "unknown", i))
		rc := FeedRecheck{}
		if i%50 == 0 {
			rc = FeedRecheck{At: "2026-07-16T13:00:00Z", Step: 1}
		}
		db.ApplyProbeToFeedItem("UC1", vid, "vod", "T", "2026-01-01T00:00:00Z", "exact", rc)
	}
	db.db.Exec(`ANALYZE`)
	rows, err := db.db.Query("EXPLAIN QUERY PLAN "+feedScopeQ3SQL(false), "UC1", "2026-07-16T14:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var all string
	for rows.Next() {
		var a, b, c int
		var d string
		rows.Scan(&a, &b, &c, &d)
		all += d + "\n"
	}
	if !strings.Contains(all, "idx_feed_items_recheck") || strings.Contains(all, "TEMP B-TREE") {
		t.Fatalf("plan for Q3:\n%s\nwant idx_feed_items_recheck, no TEMP B-TREE", all)
	}
}
