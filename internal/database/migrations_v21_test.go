package database

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// jobsHasColumn reports whether the jobs table at path has a column named
// name, read straight from the file rather than through the package.
func jobsHasColumn(t *testing.T, path, name string) bool {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var n int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// TestMigrationV21UpgradesAV20Database: v21 is a version bump with nothing to
// apply. Development builds' v21 added jobs.twitch_quality_preference; the
// preference was folded back into quality_preference before any release, so
// a v20 database — every released install — reaches v21 with its rows and
// its columns as they were.
//
// Mutants: the version write dropped from the v21 block (user_version stays
// 20); the old ALTER restored in it (the column appears).
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
	if _, err := db.db.Exec(`PRAGMA user_version = 20`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db2, err := Open(path) // replays the version < 21 block
	if err != nil {
		t.Fatalf("v20 -> v21 upgrade failed: %v", err)
	}
	if v, _ := db2.readUserVersion(); v != 21 {
		t.Errorf("user_version = %d, want 21", v)
	}
	j, err := db2.GetJob("tw_1")
	db2.Close()
	if err != nil || j == nil {
		t.Fatalf("existing row unreadable after upgrade: %v", err)
	}
	if j.QualityPreference != "720p" || j.TwitchQuality != "chunked" {
		t.Errorf("upgraded row = quality_preference %q, twitch_quality %q; want 720p and chunked untouched",
			j.QualityPreference, j.TwitchQuality)
	}
	if jobsHasColumn(t, path, "twitch_quality_preference") {
		t.Error("the v21 upgrade added twitch_quality_preference; it is a version bump alone")
	}
}

// TestMigrationV21DevelopmentDatabaseKeepsWorking: a database a development
// build migrated to v21 has jobs.twitch_quality_preference, which this binary
// never reads or names. It must open without a downgrade refusal or a
// migration, and every read and write must work around the extra column: its
// NOT NULL empty-string default fills it on an insert that leaves it out.
//
// Mutant: schemaVersion put back to 20, as removing v21 would (the database
// reads 21 and Open refuses it as a downgrade).
func TestMigrationV21DevelopmentDatabaseKeepsWorking(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "dev-v21.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`ALTER TABLE jobs ADD COLUMN twitch_quality_preference TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatalf("make a development v21 table: %v", err)
	}
	if _, err := db.AddJob(&Job{ID: "tw_dev", VideoID: "1", URL: "https://twitch.tv/x", Platform: "twitch",
		Status: StatusFinished, TwitchQuality: "720p60", QualityPreference: "720p"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE jobs SET twitch_quality_preference = '720p' WHERE id = 'tw_dev'`); err != nil {
		t.Fatalf("give the row the value a development build wrote: %v", err)
	}
	if _, err := db.db.Exec(`PRAGMA user_version = 21`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("a development v21 database no longer opens: %v", err)
	}
	defer db2.Close()
	if v, _ := db2.readUserVersion(); v != 21 {
		t.Errorf("user_version = %d, want 21", v)
	}
	if j, err := db2.GetJob("tw_dev"); err != nil || j == nil || j.QualityPreference != "720p" {
		t.Fatalf("the development-build row reads %+v, %v; want it with quality_preference 720p", j, err)
	}
	if added, err := db2.AddJob(&Job{ID: "tw_new", VideoID: "2", URL: "https://twitch.tv/y", Platform: "twitch",
		Status: StatusUpcoming, QualityPreference: "best"}); err != nil || !added {
		t.Fatalf("AddJob beside the extra column = %v, %v", added, err)
	}
	if j := db2.UpdateJobFields("tw_new", map[string]any{"twitch_quality": "chunked"}); j == nil || j.TwitchQuality != "chunked" {
		t.Errorf("UpdateJobFields beside the extra column returned %+v", j)
	}
	if all, err := db2.GetAllJobs(); err != nil || len(all) != 2 {
		t.Errorf("GetAllJobs = %d jobs, %v; want both", len(all), err)
	}
}
