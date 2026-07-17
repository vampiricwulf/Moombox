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
	db := newTestDB(t)
	defer db.Close()
	db.UpsertFeedItem(fi("UC1", "v1", "2026-07-16T00:00:00Z", "assumed", "membership", "unknown", 0))
	// Probe: vod with a started date → status flips, date upgrades, source untouched.
	if err := db.ApplyProbeToFeedItem("UC1", "v1", "vod", "Real Title", "2026-07-14T20:00:00Z", "started"); err != nil {
		t.Fatal(err)
	}
	got := mustGetFeedItem(t, db, "UC1", "v1")
	if got.Status != "vod" || got.DatePrecision != "started" || got.Title != "Real Title" || got.Source != "membership" {
		t.Fatalf("probe write: %+v", got)
	}
	// A later probe with a WORSE date must not downgrade; a stale-listing-shaped
	// status write is the caller's responsibility to prevent, but 'Unknown Title'
	// must never overwrite a real title.
	db.ApplyProbeToFeedItem("UC1", "v1", "vod", "Unknown Title", "2026-07-13T00:00:00Z", "day")
	got = mustGetFeedItem(t, db, "UC1", "v1")
	if got.DatePrecision != "started" || got.Title != "Real Title" {
		t.Fatalf("probe downgrade leaked: %+v", got)
	}
	// upcoming supplies no date: rank 0 no-op, status still lands.
	if err := db.ApplyProbeToFeedItem("UC1", "v1", "upcoming", "", "", ""); err != nil {
		t.Fatal(err)
	}
	got = mustGetFeedItem(t, db, "UC1", "v1")
	if got.Status != "upcoming" || got.Published != "2026-07-14T20:00:00Z" {
		t.Fatalf("dateless probe: %+v (must never write published='')", got)
	}
}

func TestFeedScope_Q1UnionQ2(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	cutoff := now.AddDate(0, 0, -3).Format(time.RFC3339)
	old := now.AddDate(0, 0, -30).Format(time.RFC3339)
	recent := now.AddDate(0, 0, -1).Format(time.RFC3339)

	db.UpsertFeedItem(fi("UC1", "in", recent, "exact", "rss", "unknown", 0))   // Q1
	db.UpsertFeedItem(fi("UC1", "out", old, "coarse", "videos", "unknown", 1)) // neither
	db.UpsertFeedItem(fi("UC1", "up", old, "assumed", "rss", "unknown", 2))    // Q2 via status
	db.ApplyProbeToFeedItem("UC1", "up", "upcoming", "", "", "")
	db.UpsertFeedItem(fi("UC1", "unres", old, "assumed", "membership", "unknown", 3)) // Q2 unresolved arm (§7)
	db.UpsertFeedItem(fi("UC1", "memb", recent, "coarse", "membership", "unknown", 4))

	got, err := db.FeedScope("UC1", cutoff, true)
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
	got, _ = db.FeedScope("UC1", cutoff, false)
	ids = idsOf(got)
	if ids["memb"] || ids["unres"] {
		t.Error("membership rows visible with the toggle off")
	}
	if !ids["in"] || !ids["up"] {
		t.Error("read arm removed non-membership rows")
	}
}

func TestFeedScope_QueryPlan(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	vid := func(i int) string {
		return "v" + strings.Repeat("x", 2) + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26))
	}
	for i := 0; i < 1500; i++ { // enough rows that a bad plan is visible
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
	for i := 0; i < 1450; i++ {
		db.ApplyProbeToFeedItem("UC1", vid(i), "vod", "T", "2026-01-01T00:00:00Z", "exact")
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

func TestGetChannelRSSOK(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()
	if ts, err := db.GetChannelRSSOK("UC1"); err != nil || ts != "" {
		t.Fatalf("no row yet: ts=%q err=%v", ts, err)
	}
	if err := db.SetChannelRSSOK("UC1", "2026-07-16T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if ts, err := db.GetChannelRSSOK("UC1"); err != nil || ts != "2026-07-16T00:00:00Z" {
		t.Fatalf("got ts=%q err=%v", ts, err)
	}
}

func TestGetChannelEstablished(t *testing.T) {
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
		if err := db.ApplyProbeToFeedItem("UC1", vid, "vod", vid, p[0], p[1]); err != nil {
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
	err := db.db.QueryRow(`SELECT title, published, date_precision, catalog_pos, source, status, first_seen
		FROM feed_items WHERE channel_id = ? AND video_id = ?`, channelID, videoID).Scan(
		&it.Title, &it.Published, &it.DatePrecision, &it.CatalogPos, &it.Source, &it.Status, &it.FirstSeen)
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
