package database

import (
	"reflect"
	"testing"
)

// TestMigrationV19 mirrors TestMigrationV18: newTestDB runs createSchema at
// the current schemaVersion, so this pins the fresh-install side — a
// legacy-shaped row (INSERT omitting park_identity) must read back the
// column's DEFAULT ''.
func TestMigrationV19(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	if _, err := db.db.Exec(`INSERT INTO jobs (id, video_id, url, title, status, created_at, updated_at)
		VALUES ('legacy19','legacy19','u','t','COOKIES?','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("legacy insert: %v", err)
	}
	var pi string
	if err := db.db.QueryRow(`SELECT park_identity FROM jobs WHERE id='legacy19'`).Scan(&pi); err != nil {
		t.Fatal(err)
	}
	if pi != "" {
		t.Fatalf("park_identity default = %q, want %q", pi, "")
	}
}

// TestMigrationV19Idempotent: re-running the guarded ALTER on an
// already-migrated DB must be a no-op, not an error — a crash mid-block
// re-runs the whole block on next startup, because user_version is last.
func TestMigrationV19Idempotent(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	if err := db.migrateV19(); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if err := db.migrateV19(); err != nil {
		t.Fatalf("third run: %v", err)
	}
}

// TestMigrateV19ParkIdentity pins the round trip through both read paths.
// GetAllJobs matters specifically: the credential sweeps read jobs through it,
// so a missing column there would hand every job an empty identity and make
// every membership park look like it parked under an unknown account.
func TestMigrateV19ParkIdentity(t *testing.T) {
	db := newTestDB(t)
	defer db.Close()

	job := &Job{ID: "yt_pid1", VideoID: "pid1", URL: "https://youtube.com/watch?v=pid1", Status: StatusUpcoming}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if got, err := db.GetJob("yt_pid1"); err != nil {
		t.Fatalf("GetJob: %v", err)
	} else if got.ParkIdentity != "" {
		t.Fatalf("fresh job ParkIdentity = %q, want empty", got.ParkIdentity)
	}

	if updated := db.UpdateJobFields("yt_pid1", map[string]any{
		"status":        StatusCookies,
		"park_reason":   ParkReasonMembership,
		"park_identity": "fingerprint-A",
	}); updated == nil {
		t.Fatal("UpdateJobFields(park_identity) returned nil")
	}

	got, err := db.GetJob("yt_pid1")
	if err != nil {
		t.Fatalf("GetJob after update: %v", err)
	}
	if got.ParkIdentity != "fingerprint-A" {
		t.Fatalf("ParkIdentity = %q, want fingerprint-A", got.ParkIdentity)
	}

	all, err := db.GetAllJobs()
	if err != nil {
		t.Fatalf("GetAllJobs: %v", err)
	}
	var seen bool
	for _, j := range all {
		if j.ID == "yt_pid1" {
			seen = true
			if j.ParkIdentity != "fingerprint-A" {
				t.Fatalf("GetAllJobs ParkIdentity = %q, want fingerprint-A", j.ParkIdentity)
			}
		}
	}
	if !seen {
		t.Fatal("GetAllJobs did not return yt_pid1")
	}
}

// TestParkIdentityNotSerialized: the fingerprint is credential-derived and of
// no use to any UI, so it must not reach the API or the WebSocket payload.
func TestParkIdentityNotSerialized(t *testing.T) {
	field, ok := jobFieldByName("ParkIdentity")
	if !ok {
		t.Fatal("Job has no ParkIdentity field")
	}
	if tag := field.Tag.Get("json"); tag != "-" {
		t.Errorf("ParkIdentity json tag = %q, want \"-\" — a credential fingerprint must not be serialized to clients", tag)
	}
}

// jobFieldByName looks up a struct field on Job by Go name.
func jobFieldByName(name string) (reflect.StructField, bool) {
	return reflect.TypeFor[Job]().FieldByName(name)
}
