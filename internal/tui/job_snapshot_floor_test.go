package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

func floorJob(id string, st database.JobStatus, v uint64) *database.Job {
	return &database.Job{ID: id, VideoID: id, Title: id, Platform: "youtube", Status: st, Version: v}
}

func floorApp() *App {
	a := NewApp()
	a.width, a.height = 120, 40
	a.recalcLayout()
	return a
}

// A catch-up snapshot is read after the events a stalled TUI still has
// queued, and it is applied before them. Those older events must not be put
// back over it: a tick queued before a dropped Finished write counted the job
// active again (title, R U confirm, the fast progress tick) with nothing left
// to correct it, and a queued Muxing write put the row itself back (tui-2,
// owner ruling R2-T2).
//
// Mutant: dropping the raiseJobVersionFloor call from the JobsUpdateMsg
// handler: the tick re-creates the progress entry and the active count, and
// the status write regresses the row.
func TestQueuedEventsOlderThanASnapshotAreIgnored(t *testing.T) {
	a := floorApp()
	// Before the stall: the job is downloading, last applied write 10.
	a.Update(JobsUpdateMsg{Jobs: []*database.Job{floorJob("a", database.StatusDownloading, 0)}})
	a.Update(JobUpdateMsg{Change: &database.JobChange{Changes: []string{"progress"}, Job: floorJob("a", database.StatusDownloading, 10)}})

	// The stall: writes 11..40 queue or drop; the Finished write is 41 and
	// is dropped. The snapshot is read with floor 41 and applied first.
	a.Update(JobsUpdateMsg{Jobs: []*database.Job{floorJob("a", database.StatusFinished, 0)}, Floor: 41})

	// The backlog, older than the snapshot: a tick and a status write.
	a.Update(JobUpdateMsg{Change: &database.JobChange{Changes: []string{"progress"}, Job: floorJob("a", database.StatusDownloading, 20)}})
	a.Update(JobUpdateMsg{Change: &database.JobChange{Changes: []string{"status"}, Job: floorJob("a", database.StatusMuxing, 30)}})

	if got := a.statusMap["a"]; got != database.StatusFinished {
		t.Errorf("status map = %q, want Finished", got)
	}
	if a.progressStore.Get("a") != nil {
		t.Error("a queued tick re-created the finished job's progress entry")
	}
	if got := a.taskList.GetJobByID("a").Status; got != database.StatusFinished {
		t.Errorf("the row went back to %q", got)
	}
	if n := a.activeDownloadCount(); n != 0 {
		t.Errorf("%d downloads counted active, want 0", n)
	}

	// A write newer than the snapshot still applies.
	a.Update(JobUpdateMsg{Change: &database.JobChange{Changes: []string{"status"}, Job: floorJob("a", database.StatusDownloading, 42)}})
	if got := a.taskList.GetJobByID("a").Status; got != database.StatusDownloading {
		t.Errorf("a write newer than the floor was dropped: row is %q", got)
	}
	if a.statusMap["a"] != database.StatusDownloading || a.progressStore.Get("a") == nil {
		t.Error("a write newer than the floor did not reach the status map and the progress store")
	}
	a.Update(JobUpdateMsg{Change: &database.JobChange{Changes: []string{"progress"}, Job: &database.Job{ID: "a", Status: database.StatusDownloading, Percent: 77, Version: 43}}})
	if p := a.progressStore.Get("a"); p == nil || p.Percent != 77 {
		t.Errorf("a tick newer than the floor was dropped: %+v", p)
	}
}

// A snapshot with no floor (a bulk write's list) claims nothing: the version
// rule is exactly what it was, and a floor already applied is not lowered.
//
// Mutants: raising to the floor even when it is 0 is harmless, but clearing
// jobVersions on a floor-less snapshot (the "drop absent entries" step run
// unconditionally) lets the older write 5 back in; assigning the floor
// instead of taking the higher one lets write 30 back in after the second
// snapshot.
func TestASnapshotWithoutAFloorChangesNoVersion(t *testing.T) {
	a := floorApp()
	a.Update(JobsUpdateMsg{Jobs: []*database.Job{floorJob("a", database.StatusDownloading, 0)}})
	a.Update(JobUpdateMsg{Change: &database.JobChange{Changes: []string{"status"}, Job: floorJob("a", database.StatusMuxing, 10)}})

	a.Update(JobsUpdateMsg{Jobs: []*database.Job{floorJob("a", database.StatusMuxing, 0)}})
	a.Update(JobUpdateMsg{Change: &database.JobChange{Changes: []string{"status"}, Job: floorJob("a", database.StatusDownloading, 5)}})
	if got := a.taskList.GetJobByID("a").Status; got != database.StatusMuxing {
		t.Errorf("an older write got in after a floor-less snapshot: row is %q", got)
	}
	a.Update(JobUpdateMsg{Change: &database.JobChange{Changes: []string{"status"}, Job: floorJob("a", database.StatusFinished, 11)}})
	if got := a.taskList.GetJobByID("a").Status; got != database.StatusFinished {
		t.Errorf("a newer write was dropped after a floor-less snapshot: row is %q", got)
	}

	a.Update(JobsUpdateMsg{Jobs: []*database.Job{floorJob("a", database.StatusFinished, 0)}, Floor: 40})
	a.Update(JobsUpdateMsg{Jobs: []*database.Job{floorJob("a", database.StatusFinished, 0)}})
	a.Update(JobUpdateMsg{Change: &database.JobChange{Changes: []string{"status"}, Job: floorJob("a", database.StatusMuxing, 30)}})
	if got := a.taskList.GetJobByID("a").Status; got != database.StatusFinished {
		t.Errorf("a floor-less snapshot lowered the floor: row is %q", got)
	}
}

// The lifecycle events queued ahead of a snapshot. A queued add for a job the
// snapshot holds must not put the job's first status back in the status map
// and the progress store (the row itself was never duplicated: AddJob skips
// a known ID). A queued add or update for a job deleted before the snapshot
// must not bring it back. An add newer than the snapshot is a new row.
//
// Mutant: dropping the floor check from handleJobAdded: the Finished job is
// counted active again and the deleted one returns to the list.
func TestQueuedLifecycleEventsOlderThanASnapshotAreIgnored(t *testing.T) {
	a := floorApp()
	a.Update(JobsUpdateMsg{Jobs: []*database.Job{floorJob("a", database.StatusFinished, 0)}, Floor: 50})

	a.Update(JobAddedMsg{Added: &database.JobAdded{Job: floorJob("a", database.StatusUpcoming, 7)}})
	if got := a.statusMap["a"]; got != database.StatusFinished {
		t.Errorf("a queued add put status %q back in the status map", got)
	}
	if a.progressStore.Get("a") != nil {
		t.Error("a queued add re-created the finished job's progress entry")
	}
	if n := len(a.taskList.Jobs()); n != 1 {
		t.Errorf("the list holds %d rows, want 1", n)
	}

	a.Update(JobAddedMsg{Added: &database.JobAdded{Job: floorJob("gone", database.StatusUpcoming, 8)}})
	a.Update(JobUpdateMsg{Change: &database.JobChange{Changes: []string{"progress"}, Job: floorJob("gone", database.StatusDownloading, 9)}})
	if a.taskList.GetJobByID("gone") != nil {
		t.Error("a queued add brought back a job deleted before the snapshot")
	}
	if _, ok := a.statusMap["gone"]; ok || a.progressStore.Get("gone") != nil || a.activeDownloadCount() != 0 {
		t.Error("a queued event for a deleted job is counted in the status map or the progress store")
	}

	a.Update(JobAddedMsg{Added: &database.JobAdded{Job: floorJob("new", database.StatusUpcoming, 51)}})
	if a.taskList.GetJobByID("new") == nil {
		t.Error("an add newer than the snapshot was dropped")
	}
	// A row with no version makes no claim and is applied as before.
	a.Update(JobAddedMsg{Added: &database.JobAdded{Job: floorJob("unversioned", database.StatusUpcoming, 0)}})
	if a.taskList.GetJobByID("unversioned") == nil {
		t.Error("an add with no version was dropped")
	}
}

// The versions kept per job follow the snapshot: every job in it is at the
// floor at least (a higher one already applied is kept), and a job it does
// not hold has no entry left.
func TestSnapshotFloorRaisesTheAppliedVersions(t *testing.T) {
	a := floorApp()
	a.Update(JobsUpdateMsg{Jobs: []*database.Job{
		floorJob("a", database.StatusDownloading, 0), floorJob("b", database.StatusDownloading, 0), floorJob("c", database.StatusDownloading, 0)}})
	a.Update(JobUpdateMsg{Change: &database.JobChange{Changes: []string{"status"}, Job: floorJob("a", database.StatusMuxing, 60)}})
	a.Update(JobUpdateMsg{Change: &database.JobChange{Changes: []string{"status"}, Job: floorJob("c", database.StatusMuxing, 3)}})

	a.Update(JobsUpdateMsg{Jobs: []*database.Job{floorJob("a", database.StatusMuxing, 0), floorJob("b", database.StatusDownloading, 0)}, Floor: 40})
	want := map[string]appliedJobVersions{"a": {row: 60, progress: 60}, "b": {row: 40, progress: 40}}
	if len(a.jobVersions) != len(want) {
		t.Fatalf("jobVersions = %+v, want %+v", a.jobVersions, want)
	}
	for id, w := range want {
		if a.jobVersions[id] != w {
			t.Errorf("jobVersions[%s] = %+v, want %+v", id, a.jobVersions[id], w)
		}
	}
}
