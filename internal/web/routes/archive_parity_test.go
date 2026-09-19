package routes

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobfilter"
)

// filterJobsByAge is one of the four classifiers CORE-8 folded into
// jobfilter.IsArchived. It ran the same time.Duration(days*24)*time.Hour
// truncation as the two in cmd/moombox, so a threshold below one hour
// ("0.02d" ≈ 29 min, which FlexDuration accepts) collapsed to a zero window
// and moved every Finished job to /api/jobs/archived while the dashboard
// kept it in the active panel for another 29 minutes (WEB-8).
//
// The same statuses × ages × thresholds are asserted in cmd/moombox
// (TestArchiveClassifiersAgree) and internal/tui
// (TestTaskListArchiveVerdictsMatchTheSharedPredicate). No age sits exactly
// on a threshold — filterJobsByAge reads time.Now() itself, and the
// exactly-at-the-cutoff rule is pinned where the clock is injectable
// (internal/jobfilter.TestIsArchivedRules).
//
// Mutant: restoring hideAge := time.Duration(hideAgeDays*24) * time.Hour —
// every 0.02 and 0.5 row with a Finished job disagrees with the predicate,
// in both the active and the archived partition.
func TestFilterJobsByAgeMatchesTheSharedPredicate(t *testing.T) {
	dir := t.TempDir()
	store := config.NewStore(config.Defaults(), filepath.Join(dir, "config.toml"))

	now := time.Now()
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }

	statuses := []database.JobStatus{
		database.StatusFinished, database.StatusError,
		database.StatusCancelled, database.StatusDownloading,
	}
	ages := []time.Duration{
		time.Second, 20 * time.Minute, 29 * time.Minute,
		11 * time.Hour, 13 * time.Hour, 35 * time.Hour, 37 * time.Hour,
		40 * 24 * time.Hour,
	}

	var jobs []*database.Job
	for _, st := range statuses {
		for _, age := range ages {
			jobs = append(jobs, &database.Job{ID: string(st) + age.String(), Status: st, UpdatedAt: at(age)})
		}
	}
	jobs = append(jobs, &database.Job{ID: "malformed", Status: database.StatusFinished, UpdatedAt: "yesterday"})
	jobs = append(jobs, &database.Job{ID: "notimestamp", Status: database.StatusFinished})

	for _, days := range []float64{-1, 0, 0.02, 0.5, 1.5, 30} {
		// Validate rejects a negative threshold, so write it under the
		// store's lock — the same bypass TestJobsListNegativeHideAgeKeepsAll
		// uses. This exercises the route-level branch, not persistence.
		mu := store.RWMutex()
		mu.Lock()
		store.Config().Monitors.HideFinishedAgeDays = config.FlexDuration{Value: days}
		mu.Unlock()

		inActive := make(map[string]bool, len(jobs))
		for _, j := range filterJobsByAge(jobs, false, store) {
			inActive[j.ID] = true
		}
		inArchived := make(map[string]bool, len(jobs))
		for _, j := range filterJobsByAge(jobs, true, store) {
			inArchived[j.ID] = true
		}

		for _, j := range jobs {
			want := jobfilter.IsArchivedAt(j, days, now)
			if inArchived[j.ID] != want || inActive[j.ID] == want {
				t.Errorf("days=%v job=%s: active=%v archived=%v, predicate says archived=%v",
					days, j.ID, inActive[j.ID], inArchived[j.ID], want)
			}
		}
	}
}
