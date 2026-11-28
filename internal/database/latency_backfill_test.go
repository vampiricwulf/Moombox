package database

import (
	"slices"
	"testing"
)

// TestLatencyBackfillRows pins which rows the latency-mode backfill walks
// (owner ruling RV-19): Finished, not Twitch (a row with no platform is
// YouTube), with a video ID and no mode, after the cursor, in ID order, up
// to the limit; an archive import's placeholder video ID ("imp_" and eight
// hex digits) is left out, while an ID that only starts with "imp_" is not.
//
// Mutants: the status filter dropped (the Upcoming row is walked); the
// platform filter written = 'youtube' (the NULL-platform row is missed);
// the mode filter dropped (the asked rows are walked); the cursor ignored
// (the second page repeats the first); the placeholder filter dropped (the
// import row is walked) or widened to every "imp_" ID (the eleven-character
// one is missed).
func TestLatencyBackfillRows(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	add := func(id, platform string, status JobStatus, videoID string) {
		t.Helper()
		if _, err := db.AddJob(&Job{ID: id, VideoID: videoID, URL: "u", Platform: platform, Status: status}); err != nil {
			t.Fatalf("AddJob %s: %v", id, err)
		}
	}
	add("a-yt", "youtube", StatusFinished, "a-yt")
	add("b-noplatform", "youtube", StatusFinished, "b-noplatform")
	add("c-twitch", "twitch", StatusFinished, "c-twitch")
	add("d-upcoming", "youtube", StatusUpcoming, "d-upcoming")
	add("e-asked", "youtube", StatusFinished, "e-asked")
	add("f-unstated", "youtube", StatusFinished, "f-unstated")
	add("g-yt", "youtube", StatusFinished, "g-yt")
	add("imp_0123abcd", "youtube", StatusFinished, "imp_0123abcd")
	add("imp_wouldbe", "youtube", StatusFinished, "imp_wouldbe")
	if _, err := db.db.Exec(`UPDATE jobs SET platform = NULL WHERE id = 'b-noplatform'`); err != nil {
		t.Fatal(err)
	}
	db.UpdateJobFields("e-asked", map[string]any{"latency_mode": LatencyModeLow})
	db.UpdateJobFields("f-unstated", map[string]any{"latency_mode": LatencyModeUnstated})

	ids := func(after string, limit int) []string {
		t.Helper()
		rows, err := db.LatencyBackfillRows(after, limit)
		if err != nil {
			t.Fatalf("LatencyBackfillRows: %v", err)
		}
		var out []string
		for _, r := range rows {
			if r.JobID != r.VideoID {
				t.Errorf("row %s carries video ID %q", r.JobID, r.VideoID)
			}
			out = append(out, r.JobID)
		}
		return out
	}
	if got, want := ids("", 10), []string{"a-yt", "b-noplatform", "g-yt", "imp_wouldbe"}; !slices.Equal(got, want) {
		t.Errorf("all = %v, want %v", got, want)
	}
	if got, want := ids("", 2), []string{"a-yt", "b-noplatform"}; !slices.Equal(got, want) {
		t.Errorf("first page = %v, want %v", got, want)
	}
	if got, want := ids("b-noplatform", 2), []string{"g-yt", "imp_wouldbe"}; !slices.Equal(got, want) {
		t.Errorf("second page = %v, want %v", got, want)
	}
}

// TestSetLatencyModeIfUnset pins the backfill's one write: it lands only on
// a row with no mode, never replaces one, leaves updated_at alone (the
// player's picker sorts by it), and tells the subscribers when it applied.
// The stamped path still bumps updated_at.
//
// Mutants: the IS NULL condition dropped (the stored Low is replaced);
// stamp ignored (updated_at moves); the notify skipped (no change seen).
func TestSetLatencyModeIfUnset(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	for _, id := range []string{"fresh", "stored"} {
		if _, err := db.AddJob(&Job{ID: id, VideoID: id, URL: "u", Platform: "youtube", Status: StatusFinished}); err != nil {
			t.Fatal(err)
		}
	}
	const old = "2020-01-02T03:04:05Z"
	if _, err := db.db.Exec(`UPDATE jobs SET updated_at = ?`, old); err != nil {
		t.Fatal(err)
	}
	db.UpdateJobFields("stored", map[string]any{"latency_mode": LatencyModeLow})

	changes := make(chan *JobChange, 4)
	db.OnJobChange(func(ev *JobChange) { changes <- ev })

	if !db.SetLatencyModeIfUnset("fresh", LatencyModeNormal) {
		t.Fatal("the write to a row with no mode did not apply")
	}
	got, _ := db.GetJob("fresh")
	if got.LatencyMode != LatencyModeNormal {
		t.Errorf("latency mode = %q, want normal", got.LatencyMode)
	}
	if got.UpdatedAt != old {
		t.Errorf("updated_at = %q, want it left at %q", got.UpdatedAt, old)
	}
	ev := <-changes
	if ev.Job.ID != "fresh" || !slices.Equal(ev.Changes, []string{"latency_mode"}) {
		t.Errorf("change = %s %v, want fresh [latency_mode]", ev.Job.ID, ev.Changes)
	}

	if db.SetLatencyModeIfUnset("stored", LatencyModeUnstated) {
		t.Error("the write replaced a stored mode")
	}
	if got, _ := db.GetJob("stored"); got.LatencyMode != LatencyModeLow {
		t.Errorf("stored mode = %q, want low kept", got.LatencyMode)
	}
	if db.SetLatencyModeIfUnset("missing", LatencyModeNormal) {
		t.Error("the write applied to a missing row")
	}
	if db.SetLatencyModeIfUnset("fresh", LatencyModeNone) {
		t.Error("an empty mode was written")
	}

	// The stamped path is unchanged.
	db.UpdateJobFields("fresh", map[string]any{"title": "t"})
	if got, _ := db.GetJob("fresh"); got.UpdatedAt == old {
		t.Error("UpdateJobFields no longer bumps updated_at")
	}
}

// TestHasJobWithStatus: the backfill's capture-start check.
func TestHasJobWithStatus(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	if has, err := db.HasJobWithStatus(StatusLive); err != nil || has {
		t.Fatalf("empty db: %v %v", has, err)
	}
	if _, err := db.AddJob(&Job{ID: "x", VideoID: "x", URL: "u", Platform: "youtube", Status: StatusLive}); err != nil {
		t.Fatal(err)
	}
	if has, err := db.HasJobWithStatus(StatusLive); err != nil || !has {
		t.Errorf("a Live row: %v %v", has, err)
	}
	if has, _ := db.HasJobWithStatus(StatusMuxing); has {
		t.Error("no Muxing row, but HasJobWithStatus says one")
	}
}
