package tui

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobfilter"
)

// The TUI used int(HideFinishedAgeDays), so 0.5 days (12 h — valid config
// the Web UI and the config file both accept) became 0, which its own
// isJobArchived documents as "instantly archive all finished jobs". The list
// was the fourth classifier and it disagreed with the three the shared
// predicate now pins together (CORE-8).
//
// Mutant: int(cfg.Monitors.HideFinishedAgeDays.Days()) in SetConfig — the
// one-minute-old Finished job is archived.
func TestTaskListHonoursAFractionalThreshold(t *testing.T) {
	cfg := config.Defaults()
	cfg.Monitors.HideFinishedAgeDays = config.FlexDuration{Value: 0.5}

	a := NewApp()
	a.SetConfig(cfg)
	if got := a.taskList.HideFinishedAgeDays(); got != 0.5 {
		t.Fatalf("threshold = %v, want 0.5", got)
	}

	now := time.Now()
	fresh := &database.Job{
		ID: "a", Title: "A", Status: database.StatusFinished,
		UpdatedAt: now.Add(-time.Minute).Format(time.RFC3339),
	}
	old := &database.Job{
		ID: "b", Title: "B", Status: database.StatusFinished,
		UpdatedAt: now.Add(-13 * time.Hour).Format(time.RFC3339),
	}
	if isJobArchived(fresh, archiveCutoff(0.5, now)) {
		t.Error("a Finished job one minute old must stay active under a 12-hour threshold")
	}
	if !isJobArchived(old, archiveCutoff(0.5, now)) {
		t.Error("a Finished job thirteen hours old must archive under a 12-hour threshold")
	}
}

// The list's own wrapper must agree with the shared predicate job for job —
// this is the TUI end of the differential check the REST filter and the
// cmd/moombox list filter run against the same table (report row #15). The
// only thing the wrapper adds is the "never archive" reading of a zero
// cutoff, which must line up with jobfilter's negative-threshold guard.
//
// Mutant: restoring the old ceil(hours/24) > ageDays rule — every sub-day
// threshold row and the 1.5d/36h row disagree.
func TestTaskListArchiveVerdictsMatchTheSharedPredicate(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }

	statuses := []database.JobStatus{
		database.StatusFinished, database.StatusError,
		database.StatusCancelled, database.StatusDownloading,
	}
	ages := []time.Duration{
		0, 20 * time.Minute, 29 * time.Minute, 11 * time.Hour,
		12 * time.Hour, 13 * time.Hour, 35 * time.Hour, 37 * time.Hour,
		40 * 24 * time.Hour,
	}
	for _, days := range []float64{-1, 0, 0.02, 0.5, 1.5, 30} {
		cutoff := archiveCutoff(days, now)
		for _, st := range statuses {
			for _, age := range ages {
				j := &database.Job{ID: "j", Status: st, UpdatedAt: at(age)}
				want := jobfilter.IsArchivedAt(j, days, now)
				if got := isJobArchived(j, cutoff); got != want {
					t.Errorf("days=%v status=%s age=%v: tui=%v jobfilter=%v", days, st, age, got, want)
				}
			}
		}
		// A malformed timestamp is never archived on either side.
		bad := &database.Job{ID: "bad", Status: database.StatusFinished, UpdatedAt: "yesterday"}
		if isJobArchived(bad, cutoff) || jobfilter.IsArchivedAt(bad, days, now) {
			t.Errorf("days=%v: an unparseable updated_at must never archive", days)
		}
	}
}

// Widening the threshold to a float64 must not let the render cache outlive
// it: SetHideFinishedAgeDays is one of the mutators the key's rebuildSeq
// covers, so it has to end in a rebuild. Both frames are taken inside one
// wall-clock second because the key carries time.Now().Unix() (spec §5).
//
// Mutant: dropping m.rebuildVirtualList() from SetHideFinishedAgeDays — the
// second frame is the stale cached one and the job never moves.
func TestSetHideFinishedAgeDaysInvalidatesTheRenderCache(t *testing.T) {
	m := NewTaskListModel()
	m.SetSize(60, 12)
	m.SetJobs([]*database.Job{{
		ID: "a", Title: "THIRTEEN HOURS OLD", Status: database.StatusFinished,
		UpdatedAt: time.Now().Add(-13 * time.Hour).Format(time.RFC3339),
	}})

	var before, after string
	var seqBefore, seqAfter uint64
	observeInOneSecond(t, func() {
		// A retry re-enters this closure with whatever the previous
		// (discarded) attempt left behind. Reset to the constructor's
		// default threshold first so every attempt — first try or retry —
		// starts from the same unarchived baseline.
		m.SetHideFinishedAgeDays(30)
		m.renderCache = ""
		before = m.View()
		seqBefore = m.rebuildSeq
		m.SetHideFinishedAgeDays(0.5) // 12 h — the row crosses the boundary
		after = m.View()
		seqAfter = m.rebuildSeq
	})

	if seqAfter == seqBefore {
		t.Error("SetHideFinishedAgeDays must end in a rebuild so the cache key moves")
	}
	if after == before {
		t.Errorf("the frame must change when the threshold archives the only row:\n%s", stripANSI(after))
	}
}
