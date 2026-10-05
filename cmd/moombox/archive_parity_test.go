package main

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobfilter"
)

// archiveParityThresholds / archiveParityAges / archiveParityStatuses are the
// shared fixture report row #15's differential check runs: the same statuses ×
// ages × thresholds are asserted here, in internal/web/routes
// (TestFilterJobsByAgeMatchesTheSharedPredicate) and in internal/tui
// (TestTaskListArchiveVerdictsMatchTheSharedPredicate), so every Go classifier
// reaches the same verdict for every job.
//
// No age sits exactly on a threshold: filterJobsByAgeThreshold reads
// time.Now() itself, microseconds after this table is built, and the
// exactly-at-the-cutoff rule is pinned where the clock is injectable
// (internal/jobfilter.TestIsArchivedRules).
var (
	archiveParityThresholds = []float64{-1, 0, 0.02, 0.5, 1.5, 30}
	archiveParityAges       = []time.Duration{
		time.Second, 20 * time.Minute, 29 * time.Minute,
		11 * time.Hour, 13 * time.Hour, 35 * time.Hour, 37 * time.Hour,
		40 * 24 * time.Hour,
	}
	archiveParityStatuses = []database.JobStatus{
		database.StatusFinished, database.StatusError,
		database.StatusCancelled, database.StatusDownloading,
	}
)

// The list filter and the clocked predicate (jobfilter.IsArchivedAt, which a
// job_update broadcast gate also used until that gate was removed) are
// classifiers CORE-8 folded into one predicate. They disagreed below one
// hour: both computed time.Duration(days*24)*time.Hour, which truncates the
// float to whole HOURS before multiplying, so a "0.02d" threshold (≈29 min,
// a value FlexDuration accepts) collapsed to a zero window and swept every
// Finished job — while the Web UI's _evaluateArchiveBoundary, which scales
// the float by the whole day, kept showing them.
//
// Mutant: restoring cutoff := time.Now().Add(-time.Duration(hideAgeDays*24) *
// time.Hour) in filterJobsByAgeThreshold — every 0.02 and 0.5 row with a
// Finished job disagrees with the gate.
func TestArchiveClassifiersAgree(t *testing.T) {
	now := time.Now()
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }

	for _, days := range archiveParityThresholds {
		var jobs []*database.Job
		for _, st := range archiveParityStatuses {
			for _, age := range archiveParityAges {
				jobs = append(jobs, &database.Job{
					ID: string(st) + age.String(), Status: st, UpdatedAt: at(age),
				})
			}
		}
		jobs = append(jobs, &database.Job{ID: "malformed", Status: database.StatusFinished, UpdatedAt: "yesterday"})
		jobs = append(jobs, &database.Job{ID: "notimestamp", Status: database.StatusFinished})

		active := make(map[string]bool, len(jobs))
		for _, j := range filterJobsByAgeThreshold(jobs, days) {
			active[j.ID] = true
		}
		cutoff := jobfilter.ArchiveCutoff(now, days)
		for _, j := range jobs {
			// The clocked predicate.
			gate := jobfilter.IsArchivedAt(j, days, now)
			// The bare predicate, cutoff computed once per sweep.
			base := days >= 0 && jobfilter.IsArchived(j, cutoff)
			// The list filter: archived == dropped from the active slice.
			list := !active[j.ID]
			if gate != base || list != gate {
				t.Errorf("days=%v job=%s: list=%v gate=%v predicate=%v", days, j.ID, list, gate, base)
			}
		}
	}
}
