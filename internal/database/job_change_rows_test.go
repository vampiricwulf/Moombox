package database

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// childRowsJob opens a database holding one Muxing job with a gap, two parts
// and a trim — the child rows GetJob loads beside the jobs row.
func childRowsJob(t *testing.T) *Database {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "rows.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.AddJob(&Job{ID: "vid", VideoID: "vid", URL: "u", Status: StatusMuxing,
		Gaps: []Gap{{From: 10, To: 12, Stream: "video"}}}); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"a - part1.mp4", "a - part2.mp4"} {
		if err := db.AddSegment(&Segment{JobID: "vid", SegmentIndex: i, Quality: "1080p", Filename: name}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AddTrim(&TrimRecord{ID: "t1", JobID: "vid", StartTime: 1, EndTime: 2, Filename: "t.mp4",
		CreatedAt: "2026-10-09T00:00:00Z", Duration: 1}); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestUpdateEventsCarryTheChildRows: the row UpdateJobFields and its
// conditional forms hand their subscribers, and return, is the row GetJob
// returns — gaps, trims and parts included. The read-back used to scan the
// jobs row alone, and both UIs replace the row they hold with it: every status
// transition, rename or Mark Watched swapped a row that had its child rows
// for one that did not (the fields are omitempty, so the dashboard's JSON
// lost the keys), and the details dialog lost its Parts, Trims and Gaps, the
// trimmer loaded /video instead of the parts, and the TUI trim dialog listed
// no trims, until a reload.
//
// A progress tick is the one write that leaves them out (JobChange): no
// subscriber replaces a row with a tick's Job, and it is the ~60 Hz path.
//
// Mutants: drop the loadChildRows call from updateJobFieldsWhere's read-back
// (every write row fails); load the child rows for a tick too (the tick row
// fails); IsProgressOnlyChange answering true when any one column is a
// progress one (the status-with-progress row fails).
func TestUpdateEventsCarryTheChildRows(t *testing.T) {
	db := childRowsJob(t)
	stored, err := db.GetJob("vid")
	if err != nil || stored == nil {
		t.Fatalf("GetJob: %v", err)
	}
	if len(stored.Gaps) != 1 || len(stored.Trims) != 1 || len(stored.Segments) != 2 {
		t.Fatalf("fixture: GetJob has gaps/trims/segments %d/%d/%d, want 1/1/2",
			len(stored.Gaps), len(stored.Trims), len(stored.Segments))
	}

	var change *JobChange
	var update *Job
	db.OnJobChange(func(ev *JobChange) { change = ev })
	db.OnJobUpdate(func(j *Job) { update = j })

	full := func(t *testing.T, what string, j *Job) {
		t.Helper()
		if j == nil {
			t.Fatalf("%s: no row", what)
		}
		if len(j.Gaps) != len(stored.Gaps) || len(j.Trims) != len(stored.Trims) || len(j.Segments) != len(stored.Segments) {
			t.Errorf("%s: gaps/trims/segments %d/%d/%d, want GetJob's %d/%d/%d", what,
				len(j.Gaps), len(j.Trims), len(j.Segments), len(stored.Gaps), len(stored.Trims), len(stored.Segments))
			return
		}
		if j.Trims[0].ID != "t1" || j.Segments[1].Filename != "a - part2.mp4" || j.Gaps[0].From != 10 {
			t.Errorf("%s: child rows %+v / %+v / %+v are not the job's own", what, j.Trims, j.Segments, j.Gaps)
		}
		raw, _ := json.Marshal(j)
		var keys map[string]json.RawMessage
		json.Unmarshal(raw, &keys)
		for _, k := range []string{"gaps", "trims", "segments"} {
			if _, ok := keys[k]; !ok {
				t.Errorf("%s: the JSON the dashboard is sent has no %q key", what, k)
			}
		}
	}

	writes := []struct {
		name  string
		write func() *Job // the row the call returns, nil for the conditional forms
	}{
		{"the mux's Finished write", func() *Job { return db.UpdateJobFields("vid", map[string]any{"status": StatusFinished}) }},
		{"a rename", func() *Job { return db.UpdateJobFields("vid", map[string]any{"title": "renamed"}) }},
		{"a Mark Watched", func() *Job {
			return db.UpdateJobFields("vid", map[string]any{"watched": 1, "resume_position": nil})
		}},
		{"a status riding with progress", func() *Job {
			return db.UpdateJobFields("vid", map[string]any{"status": StatusFinished, "progress": "", "percent": 100.0})
		}},
		{"UpdateJobFieldsIf", func() *Job {
			db.UpdateJobFieldsIf("vid", StatusFinished, map[string]any{"title": "again"})
			return nil
		}},
		{"UpdateJobFieldsUnless", func() *Job {
			db.UpdateJobFieldsUnless("vid", StatusCancelled, map[string]any{"error": ""})
			return nil
		}},
	}
	for _, w := range writes {
		t.Run(w.name, func(t *testing.T) {
			change, update = nil, nil
			ret := w.write()
			if change == nil {
				t.Fatal("no OnJobChange event")
			}
			full(t, "OnJobChange's Job", change.Job)
			full(t, "OnJobUpdate's Job", update)
			if ret != nil {
				full(t, "the returned Job", ret)
			}
		})
	}

	t.Run("a progress tick", func(t *testing.T) {
		change = nil
		db.UpdateJobFields("vid", map[string]any{"progress": "V:5 A:5", "percent": 50.0, "speed": "1 MB/s"})
		if change == nil {
			t.Fatal("no OnJobChange event")
		}
		if !IsProgressOnlyChange(change.Changes) {
			t.Fatalf("changes %v are not classified as a tick", change.Changes)
		}
		if j := change.Job; len(j.Gaps)+len(j.Trims)+len(j.Segments) != 0 {
			t.Errorf("a tick's Job carries %d/%d/%d child rows, want none: three more queries on the ~60 Hz path",
				len(j.Gaps), len(j.Trims), len(j.Segments))
		}
	})
}
