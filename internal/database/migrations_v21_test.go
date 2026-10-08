package database

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrationV21UpgradesAV20Database pins the UPGRADE half of v21 (D-T9),
// which TestFreshSchemaMatchesMigratedSchema cannot: its databases are both
// built through createSchema, so a deleted ALTER still passes there. This
// presents migrateV21 with a table that lacks the column — the shape every
// existing install has — and checks the existing rows come out readable with
// an EMPTY preference, the mark BackfillTwitchQualityPreference keys on.
//
// Mutants: the v21 ALTER deleted (the column is missing and every job read
// fails); the column added without its empty default (a NULL scans into no
// string and the row is unreadable).
func TestMigrationV21UpgradesAV20Database(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "v20.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddJob(&Job{ID: "tw_1", VideoID: "1", URL: "https://twitch.tv/x", Platform: "twitch",
		Status: StatusFinished, TwitchQuality: "chunked", QualityPreference: "720p"}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`ALTER TABLE jobs DROP COLUMN twitch_quality_preference`); err != nil {
		t.Fatalf("make a v20-shaped table: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 20`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db2, err := Open(path) // replays the version < 21 block
	if err != nil {
		t.Fatalf("v20 -> v21 upgrade failed: %v", err)
	}
	defer db2.Close()
	if v, _ := db2.readUserVersion(); v != 21 {
		t.Errorf("user_version = %d, want 21", v)
	}
	j, err := db2.GetJob("tw_1")
	if err != nil || j == nil {
		t.Fatalf("existing row unreadable after upgrade: %v", err)
	}
	if j.TwitchQualityPreference != "" {
		t.Errorf("upgraded row's preference = %q, want empty until the startup backfill", j.TwitchQualityPreference)
	}
	if j.TwitchQuality != "chunked" {
		t.Errorf("twitch_quality = %q, want it untouched", j.TwitchQuality)
	}
}

// TestMigrationV21Idempotent: a crash mid-block re-runs the whole block on
// next startup (user_version is written last), so the guarded ALTER must be a
// no-op on a migrated table, not an error.
func TestMigrationV21Idempotent(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()
	if err := db.migrateV21(); err != nil {
		t.Fatalf("second run: %v", err)
	}
}

// TestBackfillTwitchQualityPreference pins the startup pass that fills the
// rows v21 left empty: only Twitch rows without a preference are offered to
// the rule, an empty answer is stored as "best", a recorded preference is
// never overwritten, nothing is announced, and a second pass finds nothing.
//
// Mutants: the WHERE clause's platform filter dropped (the YouTube row is
// written); its empty-preference filter dropped (the row that already holds
// one is overwritten); the "best" default dropped (an empty answer is stored
// and the row is offered again on every start); the write bumping updated_at.
func TestBackfillTwitchQualityPreference(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	defer db.Close()

	for _, j := range []*Job{
		{ID: "tw_legacy", VideoID: "1", URL: "https://twitch.tv/abc", Platform: "twitch", Status: StatusError, QualityPreference: "720p"},
		{ID: "tw_nopref", VideoID: "2", URL: "https://twitch.tv/def", Platform: "twitch", Status: StatusFinished},
		{ID: "tw_new", VideoID: "3", URL: "https://twitch.tv/ghi", Platform: "twitch", Status: StatusUpcoming, TwitchQualityPreference: "480p"},
		{ID: "yt_row", VideoID: "yt_row", URL: "u", Platform: "youtube", Status: StatusFinished},
	} {
		if _, err := db.AddJob(j); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := db.GetJob("tw_legacy")

	offered := map[string]string{}
	n, err := db.BackfillTwitchQualityPreference(func(j *Job) string {
		offered[j.ID] = j.URL
		return j.QualityPreference // "720p" for tw_legacy, "" for tw_nopref
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("wrote %d rows, want 2", n)
	}
	if len(offered) != 2 || offered["tw_legacy"] != "https://twitch.tv/abc" || offered["tw_nopref"] == "" {
		t.Errorf("rows offered to the rule = %v, want exactly tw_legacy and tw_nopref with their URLs", offered)
	}
	for id, want := range map[string]string{"tw_legacy": "720p", "tw_nopref": "best", "tw_new": "480p", "yt_row": ""} {
		j, err := db.GetJob(id)
		if err != nil || j == nil {
			t.Fatalf("GetJob %s: %v", id, err)
		}
		if j.TwitchQualityPreference != want {
			t.Errorf("%s preference = %q, want %q", id, j.TwitchQualityPreference, want)
		}
	}
	if after, _ := db.GetJob("tw_legacy"); after.UpdatedAt != before.UpdatedAt {
		t.Errorf("updated_at moved from %q to %q — the backfill must be silent", before.UpdatedAt, after.UpdatedAt)
	}

	again, err := db.BackfillTwitchQualityPreference(func(j *Job) string {
		t.Errorf("row %s offered again on a second pass", j.ID)
		return ""
	})
	if err != nil || again != 0 {
		t.Errorf("second pass = %d, %v; want 0, nil", again, err)
	}
}
