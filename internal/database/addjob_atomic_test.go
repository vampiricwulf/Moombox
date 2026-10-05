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
