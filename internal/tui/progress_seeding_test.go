package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// isProgressTerminal is the ONE predicate the three progress-store sites
// share. The mutant this kills: a site spelling the condition inline again
// and leaving COOKIES? (a park, not a completion) out of it.
func TestIsProgressTerminal(t *testing.T) {
	for _, tc := range []struct {
		status database.JobStatus
		want   bool
	}{
		{database.StatusFinished, true},
		{database.StatusCancelled, true},
		{database.StatusError, true},
		{database.StatusCookies, true},
		{database.StatusLive, false},
		{database.StatusDownloading, false},
		{database.StatusUpcoming, false},
		{database.StatusQueued, false},
		{database.StatusMuxing, false},
	} {
		if got := isProgressTerminal(tc.status); got != tc.want {
			t.Errorf("isProgressTerminal(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

// A full-list snapshot must not seed progress entries for jobs whose
// progress is over. Mutant: the seeding loop calling Set unconditionally
// (what it did) — a startup snapshot then leaves every Finished job with a
// live entry, and the 16ms tick rebuilds the details panel for whichever one
// is selected, forever.
func TestSnapshotSkipsProgressForTerminalJobs(t *testing.T) {
	app := NewApp()
	jobs := []*database.Job{
		{ID: "live", Status: database.StatusLive, Progress: "V:1 A:1"},
		{ID: "done", Status: database.StatusFinished, Progress: "V:9 A:9"},
		{ID: "bad", Status: database.StatusError},
		{ID: "cook", Status: database.StatusCookies},
		{ID: "gone", Status: database.StatusCancelled},
	}
	app.Update(JobsUpdateMsg{Jobs: jobs})

	if app.progressStore.Get("live") == nil {
		t.Error("a Live job must keep its seeded entry — the tick renders segment/chat counts from it")
	}
	for _, id := range []string{"done", "bad", "cook", "gone"} {
		if app.progressStore.Get(id) != nil {
			t.Errorf("terminal job %q was seeded into the progress store", id)
		}
	}
}

// The JobAdded lifecycle path is the second seeding site: an archive import
// lands Finished rows through it. Mutant: fixing only the snapshot loop.
func TestJobAddedSkipsProgressForTerminalJob(t *testing.T) {
	app := NewApp()
	app.Update(JobAddedMsg{Added: &database.JobAdded{Job: &database.Job{ID: "done", Status: database.StatusFinished}}})
	if app.progressStore.Get("done") != nil {
		t.Error("handleJobAdded seeded a progress entry for a Finished job")
	}
	app.Update(JobAddedMsg{Added: &database.JobAdded{Job: &database.Job{ID: "up", Status: database.StatusUpcoming}}})
	if app.progressStore.Get("up") == nil {
		t.Error("handleJobAdded must still seed a non-terminal job — early chat counts render from it")
	}
}

// The third door: an update to an ALREADY-terminal job. Its status did not
// change, so the transition block never ran, while the unconditional Set at
// the top of handleJobUpdate re-created the entry. A W (toggle watched) on a
// Finished job is exactly this write.
func TestUpdateOfAnAlreadyTerminalJobLeavesTheStoreEmpty(t *testing.T) {
	app := NewApp()
	job := &database.Job{ID: "done", Status: database.StatusFinished}
	app.Update(JobsUpdateMsg{Jobs: []*database.Job{job}})

	job.Watched = true
	app.Update(JobUpdateMsg{Change: &database.JobChange{Job: job, Changes: []string{"watched"}}})

	if app.progressStore.Get("done") != nil {
		t.Error("a non-status update to a Finished job re-seeded its progress entry")
	}
}

// The live path is unchanged: every update to a running job refreshes its
// entry, which is what the 16ms tick renders. Mutant: gating the Set on
// something coarser (e.g. Job.IsTerminal) and starving live progress.
func TestUpdateOfALiveJobStillRefreshesTheStore(t *testing.T) {
	app := NewApp()
	job := &database.Job{ID: "live", Status: database.StatusDownloading, Progress: "V:1 A:1"}
	app.Update(JobsUpdateMsg{Jobs: []*database.Job{job}})

	job.Progress = "V:2 A:2"
	app.Update(JobUpdateMsg{Change: &database.JobChange{Job: job, Changes: []string{"progress"}}})

	p := app.progressStore.Get("live")
	if p == nil || p.Progress != "V:2 A:2" {
		t.Fatalf("live progress update did not reach the store: %+v", p)
	}
}
