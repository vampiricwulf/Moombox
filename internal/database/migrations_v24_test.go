package database

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
)

// v24Row is one feed row an upgraded install holds, with the status it was
// stored at.
type v24Row struct {
	ch, id, published, source, status string
}

// The window the reset tests read the scope through: a cutoff, as the walk
// computes it (the cycle's instant less the archive window).
const v24Cutoff = "2026-07-01T00:00:00Z"

// v24Rows is an install's feed store across the cases the reset tells apart:
// a not_a_stream row inside the scope and one dated before its cutoff, a
// membership not_a_stream row inside it (in scope only while membership
// discovery is on), rows stored at other statuses inside it, and a second
// channel's in-scope not_a_stream row.
var v24Rows = []v24Row{
	{"UC1", "misfiled", "2026-07-10T00:00:00Z", "rss", "not_a_stream"},
	{"UC1", "oldUpload", "2026-06-01T00:00:00Z", "rss", "not_a_stream"},
	{"UC1", "members", "2026-07-11T00:00:00Z", "membership", "not_a_stream"},
	{"UC1", "aVOD", "2026-07-12T00:00:00Z", "rss", "vod"},
	{"UC1", "anUpcoming", "2026-07-13T00:00:00Z", "rss", "upcoming"},
	{"UC1", "anUnknown", "2026-07-14T00:00:00Z", "rss", "unknown"},
	{"UC2", "otherChannel", "2026-07-10T00:00:00Z", "rss", "not_a_stream"},
}

// seedV24Rows stores rows at their statuses through the store's own writes,
// each first seen when it was published — an upload RSS listed as it went
// up — so a row dated before the cutoff is outside every arm of the scope,
// Q2's unresolved one included (TestResetMisfiledFeedItemsTakesTheUnresolvedArm
// has the rows first seen later).
func seedV24Rows(t *testing.T, db *Database, rows []v24Row) {
	t.Helper()
	for _, r := range rows {
		it := fi(r.ch, r.id, r.published, "exact", r.source, "unknown", 0)
		it.FirstSeen = r.published
		if _, err := db.UpsertFeedItem(it); err != nil {
			t.Fatal(err)
		}
		if r.status != "unknown" {
			if err := db.ApplyProbeToFeedItem(r.ch, r.id, r.status, "", r.published, "exact", FeedRecheck{}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// upgradedFromV23 is an install that held rows under the v23 binary — the
// status probe filing a refusal that says nothing not_a_stream — opened by
// this one: the rows are stored, the database is put back to v23's shape
// (no feed_reprobe_owed, user_version 23), and Open replays the v24 block.
func upgradedFromV23(t *testing.T, rows []v24Row) (*Database, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v23.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	seedV24Rows(t, db, rows)
	db.Close()

	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP TABLE feed_reprobe_owed`, `PRAGMA user_version = 23`} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("make a v23-shaped database (%s): %v", q, err)
		}
	}
	raw.Close()

	db, err = Open(path) // replays the version < 24 block
	if err != nil {
		t.Fatalf("v23 -> v24 upgrade failed: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

// owedChannels lists feed_reprobe_owed, sorted.
func owedChannels(t *testing.T, db *Database) []string {
	t.Helper()
	rows, err := db.db.Query(`SELECT channel_id FROM feed_reprobe_owed ORDER BY channel_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// statuses reads every row's stored status, keyed by video ID.
func statuses(t *testing.T, db *Database, rows []v24Row) map[string]string {
	t.Helper()
	got := make(map[string]string, len(rows))
	for _, r := range rows {
		it, err := db.GetFeedItem(r.ch, r.id)
		if err != nil || it == nil {
			t.Fatalf("GetFeedItem(%s, %s) = %v, %v", r.ch, r.id, it, err)
		}
		got[r.id] = it.Status
	}
	return got
}

// TestMigrationV24Idempotent: re-running the block on a migrated database is
// a no-op, not an error, and owes nothing twice — a crash mid-block re-runs
// it whole, because user_version is written last.
func TestMigrationV24Idempotent(t *testing.T) {
	t.Parallel()
	db, _ := upgradedFromV23(t, v24Rows)
	for range 2 {
		if err := db.migrateV24(); err != nil {
			t.Fatalf("re-run: %v", err)
		}
	}
	if got := owedChannels(t, db); !slices.Equal(got, []string{"UC1", "UC2"}) {
		t.Errorf("owed after re-runs = %v, want [UC1 UC2] once each", got)
	}
}

// TestMigrationV24OwesTheResetToChannelsWithNotAStreamRows: the upgrade owes
// the reset to every channel holding a not_a_stream row, and to no other; it
// resets nothing itself, since the scope is configuration it cannot read
// (migrateV24). A fresh install owes nothing — its not_a_stream rows were
// filed under the inconclusive rule.
//
// Mutants: the INSERT left out of migrateV24 (nothing owed); its status
// filter dropped (UC3, holding only a vod, is owed too); the migration
// resetting rows itself (misfiled is unknown before any cycle).
func TestMigrationV24OwesTheResetToChannelsWithNotAStreamRows(t *testing.T) {
	t.Parallel()
	rows := append(slices.Clone(v24Rows), v24Row{"UC3", "onlyAVOD", "2026-07-10T00:00:00Z", "rss", "vod"})
	db, _ := upgradedFromV23(t, rows)
	if v, _ := db.readUserVersion(); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}
	if got := owedChannels(t, db); !slices.Equal(got, []string{"UC1", "UC2"}) {
		t.Errorf("owed = %v, want [UC1 UC2]: the channels holding a not_a_stream row", got)
	}
	if got := statuses(t, db, rows)["misfiled"]; got != "not_a_stream" {
		t.Errorf("misfiled = %q before any cycle, want not_a_stream: the migration owes the reset, the walk's cycle makes it", got)
	}

	fresh := newTestDB(t)
	seedV24Rows(t, fresh, v24Rows)
	if got := owedChannels(t, fresh); len(got) != 0 {
		t.Errorf("a fresh install owes %v, want nothing", got)
	}
	if n, err := fresh.ResetMisfiledFeedItems("UC1", v24Cutoff, true); err != nil || n != 0 {
		t.Errorf("a fresh install's reset = %d, %v; want 0, nil", n, err)
	}
}

// TestResetMisfiledFeedItemsResetsTheWalksScopeOnce is the owner's W6
// follow-up (2026-10-31: reset once on upgrade). The first reset after the
// upgrade puts the channel's not_a_stream rows inside the walk's scope back
// to unknown, the state a never-probed row has, so each is probed once under
// the inconclusive rule; a row dated before the cutoff, a membership row
// while membership discovery is off, rows at other statuses and another
// channel's rows are untouched. The scope is FeedScope's own: every
// not_a_stream row it carried before the reset is unknown after it, and no
// row it did not carry has changed. A second run does nothing — not even to
// a row the walk has since settled not_a_stream again, which is a genuine
// non-stream's answer and stands — and neither does reopening the database.
//
// Mutants: the cutoff left out of the predicate (oldUpload is reset); the
// membership clause left out (members is reset with discovery off); the
// channel clause left out (otherChannel is reset); the debt not cleared
// (the second run resets the settled row again); the debt check skipped (a
// channel owed nothing is reset).
func TestResetMisfiledFeedItemsResetsTheWalksScopeOnce(t *testing.T) {
	t.Parallel()
	db, path := upgradedFromV23(t, v24Rows)
	const now = "2026-07-15T12:00:00Z"
	before, err := db.FeedScope("UC1", v24Cutoff, now, false)
	if err != nil {
		t.Fatal(err)
	}

	n, err := db.ResetMisfiledFeedItems("UC1", v24Cutoff, false)
	if err != nil || n != 1 {
		t.Fatalf("reset = %d, %v; want 1 row (misfiled), nil", n, err)
	}
	want := map[string]string{
		"misfiled":     "unknown",      // in scope: probed once more
		"oldUpload":    "not_a_stream", // dated before the cutoff: outside the scope
		"members":      "not_a_stream", // a membership row, discovery off: outside the scope
		"aVOD":         "vod",
		"anUpcoming":   "upcoming",
		"anUnknown":    "unknown",
		"otherChannel": "not_a_stream", // UC2 is settled by UC2's own cycle
	}
	got := statuses(t, db, v24Rows)
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s = %q after the reset, want %q", id, got[id], w)
		}
	}
	for _, row := range before {
		if row.Status == "not_a_stream" && got[row.VideoID] != "unknown" {
			t.Errorf("%s was in the walk's scope stored not_a_stream and is still %q", row.VideoID, got[row.VideoID])
		}
	}
	if it, _ := db.GetFeedItem("UC1", "misfiled"); it.RecheckAt != "" || it.RecheckStep != 0 {
		t.Errorf("misfiled re-check %q step %d, want none: a never-probed row's", it.RecheckAt, it.RecheckStep)
	}
	if got := owedChannels(t, db); !slices.Equal(got, []string{"UC2"}) {
		t.Errorf("owed after UC1's reset = %v, want [UC2]: the debt goes with the reset", got)
	}

	// The walk probes misfiled and it is a genuine upload: it settles again.
	if err := db.ApplyProbeToFeedItem("UC1", "misfiled", "not_a_stream", "", "", "", FeedRecheck{}); err != nil {
		t.Fatal(err)
	}
	if n, err := db.ResetMisfiledFeedItems("UC1", v24Cutoff, false); err != nil || n != 0 {
		t.Errorf("second reset = %d, %v; want 0, nil", n, err)
	}
	db.Close()
	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if n, err := db2.ResetMisfiledFeedItems("UC1", v24Cutoff, true); err != nil || n != 0 {
		t.Errorf("reset after reopening = %d, %v; want 0, nil", n, err)
	}
	if got := statuses(t, db2, v24Rows); got["misfiled"] != "not_a_stream" || got["members"] != "not_a_stream" {
		t.Errorf("misfiled %q, members %q after the second run and a reopen; want both not_a_stream", got["misfiled"], got["members"])
	}

	// UC2's own cycle, membership discovery on, settles UC2's debt alone.
	if n, err := db2.ResetMisfiledFeedItems("UC2", v24Cutoff, true); err != nil || n != 1 {
		t.Errorf("UC2's reset = %d, %v; want 1, nil", n, err)
	}
}

// TestResetMisfiledFeedItemsTakesTheUnresolvedArm: the scope the reset
// reads is the one a row has once reset — an unknown row's, the scope the
// walk was probing it in when the old rule misfiled it — so it takes Q2's
// unresolved arm as well as Q1. An RSS row first seen inside the window is
// in it whatever its published: a waiting room announced before the window
// (an RSS <published> is the announcement), first seen when the channel was
// added or Moombox came back, which the bot wall reached through that arm
// alone. So is an 'assumed'-dated row, its published only a sighting
// instant. An RSS row first seen before the window, and a membership row
// dated before it (the arm reads first_seen on RSS rows only), keep
// not_a_stream; and every row reset is in the walk's scope, as the unknown
// row it now is.
//
// Mutants: the predicate Q1's alone (waitingRoom and assumedRow keep
// not_a_stream); without its first_seen clause (waitingRoom does), or its
// assumed clause (assumedRow does); the first_seen clause not limited to
// RSS rows (oldMembers is reset).
func TestResetMisfiledFeedItemsTakesTheUnresolvedArm(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()
	const before, inside = "2026-06-20T00:00:00Z", "2026-07-10T00:00:00Z"
	for _, it := range []FeedItem{
		{VideoID: "waitingRoom", Published: before, DatePrecision: "exact", Source: "rss", FirstSeen: inside},
		{VideoID: "assumedRow", Published: before, DatePrecision: "assumed", Source: "membership", FirstSeen: before},
		{VideoID: "oldListing", Published: before, DatePrecision: "exact", Source: "rss", FirstSeen: before},
		{VideoID: "oldMembers", Published: before, DatePrecision: "coarse", Source: "membership", FirstSeen: inside},
	} {
		it.ChannelID = "UC1"
		if _, err := db.UpsertFeedItem(it); err != nil {
			t.Fatal(err)
		}
		if err := db.ApplyProbeToFeedItem("UC1", it.VideoID, "not_a_stream", "", "", "", FeedRecheck{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.db.Exec(`INSERT INTO feed_reprobe_owed (channel_id) VALUES ('UC1')`); err != nil {
		t.Fatal(err)
	}

	if n, err := db.ResetMisfiledFeedItems("UC1", v24Cutoff, true); err != nil || n != 2 {
		t.Fatalf("reset = %d, %v; want 2 rows (waitingRoom, assumedRow), nil", n, err)
	}
	for id, want := range map[string]string{
		"waitingRoom": "unknown", "assumedRow": "unknown",
		"oldListing": "not_a_stream", "oldMembers": "not_a_stream",
	} {
		if it, _ := db.GetFeedItem("UC1", id); it == nil || it.Status != want {
			t.Errorf("%s = %+v after the reset, want status %q", id, it, want)
		}
	}
	scope, err := db.FeedScope("UC1", v24Cutoff, "2026-07-15T12:00:00Z", true)
	if err != nil {
		t.Fatal(err)
	}
	in := map[string]bool{}
	for _, it := range scope {
		in[it.VideoID] = true
	}
	if !in["waitingRoom"] || !in["assumedRow"] {
		t.Errorf("scope after the reset = %v: a reset row is outside the walk's scope, never probed", in)
	}
}

// TestResetMisfiledFeedItemsReadsMembershipRowsWhileDiscoveryIsOn: with
// membership discovery on, membership rows are in the walk's scope
// (FeedScope's includeMembership), so the reset takes them too.
//
// Mutant: the membership clause applied whatever the toggle (members stays
// not_a_stream).
func TestResetMisfiledFeedItemsReadsMembershipRowsWhileDiscoveryIsOn(t *testing.T) {
	t.Parallel()
	db, _ := upgradedFromV23(t, v24Rows)
	if n, err := db.ResetMisfiledFeedItems("UC1", v24Cutoff, true); err != nil || n != 2 {
		t.Fatalf("reset = %d, %v; want 2 rows (misfiled, members), nil", n, err)
	}
	if got := statuses(t, db, v24Rows); got["members"] != "unknown" || got["oldUpload"] != "not_a_stream" {
		t.Errorf("members %q, oldUpload %q; want unknown, not_a_stream", got["members"], got["oldUpload"])
	}
}

// TestDeleteChannelFeedDataTakesTheResetDebt: a removed channel's debt goes
// with its feed history, so a re-added channel's freshly scanned rows are
// never reset on an old upgrade's account.
//
// Mutant: DeleteChannelFeedData leaving feed_reprobe_owed alone (UC1 is
// still owed, and the re-added row is reset).
func TestDeleteChannelFeedDataTakesTheResetDebt(t *testing.T) {
	t.Parallel()
	db, _ := upgradedFromV23(t, v24Rows)
	if err := db.DeleteChannelFeedData("UC1"); err != nil {
		t.Fatal(err)
	}
	if got := owedChannels(t, db); !slices.Equal(got, []string{"UC2"}) {
		t.Errorf("owed after UC1's removal = %v, want [UC2]", got)
	}
	readded := []v24Row{{"UC1", "misfiled", "2026-07-10T00:00:00Z", "rss", "not_a_stream"}}
	seedV24Rows(t, db, readded)
	if n, err := db.ResetMisfiledFeedItems("UC1", v24Cutoff, true); err != nil || n != 0 {
		t.Errorf("the re-added channel's reset = %d, %v; want 0, nil", n, err)
	}
}
