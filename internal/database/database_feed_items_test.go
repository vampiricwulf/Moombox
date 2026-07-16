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
