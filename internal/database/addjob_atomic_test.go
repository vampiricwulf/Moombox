package database

import (
	"path/filepath"
	"testing"
)

// TestAddJobIsAtomicWithItsGaps: a gap insert that failed left the job row
// behind while AddJob reported an error and fired no JobAdded — a job that
// existed in every list after a restart, that its creator believed was never
// made. The row and its gaps now commit together or not at all.
//
// Mutant: inserting the gaps outside the transaction again — the row
// survives the failed gap.
func TestAddJobIsAtomicWithItsGaps(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.db.Exec(`CREATE TRIGGER no_gaps BEFORE INSERT ON gaps BEGIN SELECT RAISE(ABORT, 'gap refused'); END`); err != nil {
		t.Fatal(err)
	}
	added := 0
	defer db.OnJobAdded(func(*JobAdded) { added++ })()

	ok, err := db.AddJob(&Job{ID: "j1", VideoID: "j1", URL: "u", Status: StatusUpcoming,
		Gaps: []Gap{{From: 1, To: 2, Stream: "video"}}})
	if ok || err == nil {
		t.Fatalf("AddJob = %v, %v; want the gap failure reported", ok, err)
	}
	if got, _ := db.GetJob("j1"); got != nil {
		t.Error("the job row survived its failed gap insert")
	}
	if added != 0 {
		t.Errorf("JobAdded fired %d times for a job that was not added", added)
	}
}

// TestJobAddedCarriesTheStoredRow: the INSERT names a fixed column list, so a
// field outside it takes the schema default whatever the caller's struct held
// — and JobAdded used to carry the struct, so subscribers saw a job the
// database did not hold. It now carries the row read back.
//
// Mutant: notifying with the caller's struct — IncompleteTail reads true.
func TestJobAddedCarriesTheStoredRow(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got *Job
	defer db.OnJobAdded(func(e *JobAdded) { got = e.Job })()

	if ok, err := db.AddJob(&Job{ID: "j1", VideoID: "j1", URL: "u", Status: StatusUpcoming,
		IncompleteTail: true, Gaps: []Gap{{From: 1, To: 2, Stream: "video"}}}); !ok || err != nil {
		t.Fatalf("AddJob = %v, %v", ok, err)
	}
	stored, _ := db.GetJob("j1")
	if got == nil || stored == nil {
		t.Fatal("no JobAdded, or no row")
	}
	if got.IncompleteTail != stored.IncompleteTail {
		t.Errorf("JobAdded IncompleteTail = %v, the row holds %v", got.IncompleteTail, stored.IncompleteTail)
	}
	if len(got.Gaps) != 1 || got.Gaps[0].ID == 0 {
		t.Errorf("JobAdded gaps = %+v, want the stored gap with its id", got.Gaps)
	}
}

// TestWriteVersionsFollowWriteOrder: every UpdateJobFields and AddJob read-back
// carries a Version drawn under the write lock, so a later write always has a
// larger one — what lets a subscriber drop an older row that reached it last.
// A plain read makes no claim (0).
//
// Mutant: stamping the version outside the lock or not at all — the order or
// the non-zero checks fail.
func TestWriteVersionsFollowWriteOrder(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var added uint64
	defer db.OnJobAdded(func(e *JobAdded) { added = e.Job.Version })()
	if _, err := db.AddJob(&Job{ID: "j1", VideoID: "j1", URL: "u", Status: StatusUpcoming}); err != nil {
		t.Fatal(err)
	}
	a := db.UpdateJobFields("j1", map[string]any{"status": StatusDownloading})
	b := db.UpdateJobFields("j1", map[string]any{"status": StatusMuxing})
	if added == 0 || a == nil || b == nil || !(added < a.Version && a.Version < b.Version) {
		t.Errorf("versions added=%d then %v then %v, want strictly increasing and non-zero", added, a, b)
	}
	if got, _ := db.GetJob("j1"); got == nil || got.Version != 0 {
		t.Errorf("a plain read carries version %v, want 0", got)
	}
}

// TestAddJobInsertsItsSegments: a recording that arrives already split (an
// archive import's) had its part rows inserted after AddJob, and JobAdded —
// the only event a new row gets — carried the row without them, so the
// dashboard's details and trimmer and the TUI's details held the job with no
// parts until a reload. The parts now go in AddJob's transaction, JobAdded
// carries them as GetJob reads them, and a refused part takes the row back
// out with it.
//
// Mutants: not inserting job.Segments (no part rows); the read-back loading
// gaps alone (JobAdded without its parts); inserting the parts after the
// commit (the row survives its refused part).
func TestAddJobInsertsItsSegments(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got *Job
	defer db.OnJobAdded(func(e *JobAdded) { got = e.Job })()

	job := &Job{ID: "j1", VideoID: "j1", URL: "u", Status: StatusFinished, Segments: []Segment{
		{SegmentIndex: 0, Filename: "X - part1.mp4", FilePath: "/out/X - part1.mp4", DurationSeconds: 10},
		{SegmentIndex: 1, Filename: "X - part2.mp4", FilePath: "/out/X - part2.mp4", DurationSeconds: 20},
	}}
	if ok, err := db.AddJob(job); !ok || err != nil {
		t.Fatalf("AddJob = %v, %v", ok, err)
	}
	stored, _ := db.GetSegments("j1")
	if len(stored) != 2 || stored[0].Filename != "X - part1.mp4" || stored[1].DurationSeconds != 20 {
		t.Fatalf("stored parts %+v, want both", stored)
	}
	if got == nil || len(got.Segments) != 2 || got.Segments[0].ID != stored[0].ID || got.Segments[1].ID != stored[1].ID {
		t.Errorf("JobAdded parts %+v, want the stored %+v", got, stored)
	}
	for i, s := range job.Segments {
		if s.ID != stored[i].ID || s.JobID != "j1" {
			t.Errorf("caller's part %d = id %d job %q, want the row's id %d and j1", i, s.ID, s.JobID, stored[i].ID)
		}
	}

	if _, err := db.db.Exec(`CREATE TRIGGER no_parts BEFORE INSERT ON segments BEGIN SELECT RAISE(ABORT, 'part refused'); END`); err != nil {
		t.Fatal(err)
	}
	got = nil
	ok, err := db.AddJob(&Job{ID: "j2", VideoID: "j2", URL: "u", Status: StatusFinished,
		Segments: []Segment{{SegmentIndex: 0, Filename: "Y - part1.mp4"}}})
	if ok || err == nil {
		t.Fatalf("AddJob = %v, %v; want the part failure reported", ok, err)
	}
	if row, _ := db.GetJob("j2"); row != nil {
		t.Error("the job row survived its refused part")
	}
	if got != nil {
		t.Error("JobAdded fired for a job that was not added")
	}
}
