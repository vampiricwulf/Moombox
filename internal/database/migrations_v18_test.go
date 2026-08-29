package database

import "testing"

// TestMigrationV18 mirrors TestMigrationV17's shape: newTestDB already runs
// createSchema at the current schemaVersion, so this pins the fresh-install
// side — a legacy-shaped row (INSERT omitting park_reason) must read back the
// column's default below.
//
//	DEFAULT ''
//
// That default is load-bearing beyond tidiness: it is ParkReasonNone, which
// every sweep treats as ParkReasonAuth, so a pre-v18 COOKIES? row keeps
// exactly the resume behavior it had before the column existed.
func TestMigrationV18(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	if _, err := db.db.Exec(`INSERT INTO jobs (id, video_id, url, title, status, created_at, updated_at)
		VALUES ('legacy18','legacy18','u','t','COOKIES?','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("legacy insert: %v", err)
	}
	var pr string
	if err := db.db.QueryRow(`SELECT park_reason FROM jobs WHERE id='legacy18'`).Scan(&pr); err != nil {
		t.Fatal(err)
	}
	if pr != "" {
		t.Fatalf("park_reason default = %q, want %q", pr, "")
	}
	// The Go-side read path is asserted in TestMigrateV18ParkReason via
	// AddJob; a hand-rolled INSERT this sparse trips the pre-existing
	// NULL-into-string scan on unrelated columns (stream_start_time et al).
}

// TestMigrationV18Idempotent mirrors TestMigrationV17Idempotent: re-running
// the guarded ALTER block on an already-migrated DB must be a no-op, not an
// error (a crash mid-block re-runs the whole block on next startup, because
// user_version is written last).
func TestMigrationV18Idempotent(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	if err := db.migrateV18(); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if err := db.migrateV18(); err != nil {
		t.Fatalf("third run: %v", err)
	}
}

// TestMigrateV18ParkReason pins the behavioral contract end to end: a fresh
// job defaults to ParkReasonNone, UpdateJobFields can write it, and GetJob /
// GetAllJobs both round-trip the change. GetAllJobs matters specifically —
// the auth-recovery sweep reads jobs through it, so a missing column there
// would hand the sweep an empty reason for every job and silently restore the
// bug this field exists to fix.
func TestMigrateV18ParkReason(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	job := &Job{ID: "yt_park1", VideoID: "park1", URL: "https://youtube.com/watch?v=park1", Status: StatusUpcoming}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	got, err := db.GetJob("yt_park1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.ParkReason != ParkReasonNone {
		t.Fatalf("fresh job ParkReason = %q, want ParkReasonNone", got.ParkReason)
	}

	if updated := db.UpdateJobFields("yt_park1", map[string]any{
		"status":      StatusCookies,
		"park_reason": ParkReasonMembership,
	}); updated == nil {
		t.Fatal("UpdateJobFields(park_reason) returned nil")
	}

	got, err = db.GetJob("yt_park1")
	if err != nil {
		t.Fatalf("GetJob after update: %v", err)
	}
	if got.ParkReason != ParkReasonMembership {
		t.Fatalf("ParkReason = %q, want ParkReasonMembership", got.ParkReason)
	}

	all, err := db.GetAllJobs()
	if err != nil {
		t.Fatalf("GetAllJobs: %v", err)
	}
	var seen bool
	for _, j := range all {
		if j.ID == "yt_park1" {
			seen = true
			if j.ParkReason != ParkReasonMembership {
				t.Fatalf("GetAllJobs ParkReason = %q, want ParkReasonMembership", j.ParkReason)
			}
		}
	}
	if !seen {
		t.Fatal("GetAllJobs did not return yt_park1")
	}

	// Clearing back to none must work too — un-parking a job has to erase the
	// reason or a stale "membership" would suppress a later legitimate sweep.
	if updated := db.UpdateJobFields("yt_park1", map[string]any{
		"status":      StatusUpcoming,
		"park_reason": ParkReasonNone,
	}); updated == nil {
		t.Fatal("UpdateJobFields(park_reason=none) returned nil")
	}
	got, err = db.GetJob("yt_park1")
	if err != nil {
		t.Fatalf("GetJob after clear: %v", err)
	}
	if got.ParkReason != ParkReasonNone {
		t.Fatalf("ParkReason after clear = %q, want ParkReasonNone", got.ParkReason)
	}
}
