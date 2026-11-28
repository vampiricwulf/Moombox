package database

import "testing"

// TestMigrationV30 is owner ruling RV-19's two columns through a real
// upgrade: a v29 database (the fresh schema with both dropped and
// user_version put back) is migrated, its existing row reads
// media_start_time as "" (NULL, no backfill), and a write reaches both read
// paths. latency_mode is only checked to exist and start NULL. Re-running
// the block is a no-op.
//
// Mutants: the v30 block removed (GetJob's SELECT fails on the missing
// column); media_start_time left out of fieldToColumn (the write is
// ignored); the scan not assigning it (both reads return "").
func TestMigrationV30(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)

	if _, err := db.AddJob(&Job{ID: "legacy30", VideoID: "legacy30", URL: "u", Platform: "youtube", Status: StatusFinished}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	for _, col := range []string{"media_start_time", "latency_mode", "chat_offset_set"} {
		if _, err := db.db.Exec(`ALTER TABLE jobs DROP COLUMN ` + col); err != nil {
			t.Fatalf("make a v29 database: %v", err)
		}
	}
	if err := db.writeUserVersion(29); err != nil {
		t.Fatal(err)
	}
	if err := db.migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if v, _ := db.readUserVersion(); v != schemaVersion || schemaVersion < 30 {
		t.Fatalf("user_version = %d (schemaVersion %d), want 30 or later", v, schemaVersion)
	}
	if err := db.migrateV30(); err != nil {
		t.Fatalf("re-run: %v", err)
	}

	got, err := db.GetJob("legacy30")
	if err != nil {
		t.Fatalf("GetJob after the upgrade: %v", err)
	}
	if got.MediaStartTime != "" {
		t.Errorf("an existing row's media_start_time = %q, want none (no backfill)", got.MediaStartTime)
	}
	var latency *string
	if err := db.db.QueryRow(`SELECT latency_mode FROM jobs WHERE id = 'legacy30'`).Scan(&latency); err != nil {
		t.Fatalf("latency_mode after the upgrade: %v", err)
	}
	if latency != nil {
		t.Errorf("an existing row's latency_mode = %q, want NULL", *latency)
	}

	const anchor = "2026-11-27T22:56:50.408302Z"
	db.UpdateJobFields("legacy30", map[string]any{"media_start_time": anchor})
	if got, _ = db.GetJob("legacy30"); got.MediaStartTime != anchor {
		t.Errorf("GetJob media_start_time = %q, want %s", got.MediaStartTime, anchor)
	}
	all, err := db.GetAllJobs()
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range all {
		if j.ID == "legacy30" && j.MediaStartTime != anchor {
			t.Errorf("GetAllJobs media_start_time = %q, want %s", j.MediaStartTime, anchor)
		}
	}
	db.UpdateJobFields("legacy30", map[string]any{"media_start_time": nil})
	if got, _ = db.GetJob("legacy30"); got.MediaStartTime != "" {
		t.Errorf("media_start_time after a nil write = %q, want none", got.MediaStartTime)
	}
}

// TestMigrationV30LatencyAndChatOffsetSet is the other two v30 columns
// through the same real upgrade: a v29 row with an operator offset of 2.5
// keeps it with the flag defaulting to 0 (jobstate.HasOperatorChatOffset
// reads such a row as set off the value; its parity table pins that); latency_mode reads back as the type through both
// read paths; UpdateChatOffset(0) is an operator's 0 (set), and
// ClearChatOffset returns the row to none.
//
// Mutants: chat_offset_set left out of the v30 block (the SELECT fails);
// the scan not assigning LatencyMode or ChatOffsetSet; UpdateChatOffset not
// writing the flag (an operator's 0 reads as none); ClearChatOffset leaving
// it set.
func TestMigrationV30LatencyAndChatOffsetSet(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)

	if _, err := db.AddJob(&Job{ID: "old", VideoID: "old", URL: "u", Platform: "youtube", Status: StatusFinished}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if !db.UpdateChatOffset("old", 2.5) {
		t.Fatal("UpdateChatOffset on the fresh row")
	}
	for _, col := range []string{"media_start_time", "latency_mode", "chat_offset_set"} {
		if _, err := db.db.Exec(`ALTER TABLE jobs DROP COLUMN ` + col); err != nil {
			t.Fatalf("make a v29 database: %v", err)
		}
	}
	if err := db.writeUserVersion(29); err != nil {
		t.Fatal(err)
	}
	if err := db.migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	got, err := db.GetJob("old")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.ChatOffset != 2.5 || got.ChatOffsetSet {
		t.Errorf("a v29 operator offset after the upgrade: offset %v set %v, want 2.5, false",
			got.ChatOffset, got.ChatOffsetSet)
	}
	if got.LatencyMode != LatencyModeNone {
		t.Errorf("latency_mode after the upgrade = %q, want none", got.LatencyMode)
	}

	db.UpdateJobFields("old", map[string]any{"latency_mode": LatencyModeUltraLow})
	if got, _ = db.GetJob("old"); got.LatencyMode != LatencyModeUltraLow {
		t.Errorf("GetJob latency_mode = %q, want ultra_low", got.LatencyMode)
	}
	all, err := db.GetAllJobs()
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range all {
		if j.ID == "old" && j.LatencyMode != LatencyModeUltraLow {
			t.Errorf("GetAllJobs latency_mode = %q, want ultra_low", j.LatencyMode)
		}
	}

	if !db.UpdateChatOffset("old", 0) {
		t.Fatal("UpdateChatOffset(0)")
	}
	if got, _ = db.GetJob("old"); got.ChatOffset != 0 || !got.ChatOffsetSet {
		t.Errorf("an operator's 0: offset %v set %v, want 0 and set", got.ChatOffset, got.ChatOffsetSet)
	}
	if !db.ClearChatOffset("old") {
		t.Fatal("ClearChatOffset")
	}
	if got, _ = db.GetJob("old"); got.ChatOffset != 0 || got.ChatOffsetSet {
		t.Errorf("after ClearChatOffset: offset %v set %v, want 0 and not set", got.ChatOffset, got.ChatOffsetSet)
	}
	if db.ClearChatOffset("no-such") || db.UpdateChatOffset("no-such", 1) {
		t.Error("an unknown job reported as written")
	}
}

// TestParseLatencyClass pins the mapping from YouTube's latencyClass, and
// that anything unknown or empty is the asked-and-unstated value.
func TestParseLatencyClass(t *testing.T) {
	t.Parallel()
	for class, want := range map[string]LatencyMode{
		"MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_NORMAL":    LatencyModeNormal,
		"MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_LOW":       LatencyModeLow,
		"MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_ULTRA_LOW": LatencyModeUltraLow,
		"MDE_STREAM_OPTIMIZATIONS_RENDERER_LATENCY_SOMETHING": LatencyModeUnstated,
		"": LatencyModeUnstated,
	} {
		if got := ParseLatencyClass(class); got != want {
			t.Errorf("ParseLatencyClass(%q) = %q, want %q", class, got, want)
		}
	}
	for _, m := range LatencyModes {
		if m.Known() != (m.Label() != "") {
			t.Errorf("%q: Known %v but Label %q", m, m.Known(), m.Label())
		}
	}
}
