package worker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobverbs"
)

// TestListJobFlagsStagingOnlyWhereResumeIsPossible is owner ruling R-B2: a
// list row carries hasStaging exactly where Resume can be offered — a
// resumable status (Finished only with an incomplete tail) on a platform
// that resumes. Every other row carries none, and its staging directory is
// not read: the flag had been on every terminal row, one directory read for
// each archived Finished row and each Twitch row, whose answer nothing used.
//
// The table is every declared status x incompleteTail x platform, over a
// staging directory that holds a file and one that holds none, so a flagged
// row is seen to report the directory and not a constant.
//
// Mutants: stagingFlag ignoring the platform (a Twitch Error row is
// flagged); ignoring the row's tail (a plain Finished row is flagged);
// requiring the tail on every status (an Error row loses its flag);
// HasStaging constant (the empty-directory half fails).
func TestListJobFlagsStagingOnlyWhereResumeIsPossible(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "staged"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "staged", "video_stream"), []byte("m"), 0o644); err != nil {
		t.Fatal(err)
	}
	flagged := 0
	statuses := []database.JobStatus{
		database.StatusQueued, database.StatusUpcoming, database.StatusLive,
		database.StatusDownloading, database.StatusMuxing, database.StatusFinished,
		database.StatusError, database.StatusCancelled, database.StatusCookies,
	}
	for _, status := range statuses {
		for _, tail := range []bool{false, true} {
			for _, platform := range []string{"youtube", "twitch", ""} {
				for _, id := range []string{"staged", "empty"} {
					job := &database.Job{ID: id, Status: status, IncompleteTail: tail, Platform: platform}
					row := ListJob(base, job)
					want := jobverbs.ResumableStatus(status, tail) && jobverbs.ResumeSupported(platform)
					if !want {
						if row.HasStaging != nil {
							t.Errorf("%s tail=%v %q %s: hasStaging = %v, want none (Resume is never offered here)", status, tail, platform, id, *row.HasStaging)
						}
						continue
					}
					flagged++
					if row.HasStaging == nil {
						t.Errorf("%s tail=%v %q %s: no hasStaging on a row Resume can be offered on", status, tail, platform, id)
					} else if *row.HasStaging != (id == "staged") {
						t.Errorf("%s tail=%v %q %s: hasStaging = %v", status, tail, platform, id, *row.HasStaging)
					}
				}
			}
		}
	}
	// Error, Cancelled and COOKIES? with either tail, Finished with one: 7
	// status/tail pairs, YouTube only, two directories each.
	if flagged != 14 {
		t.Errorf("%d flagged rows, want 14", flagged)
	}

	// The rows the ruling names, spelled out so the table's use of the
	// predicates cannot hide a change to them.
	for _, c := range []struct {
		name string
		job  database.Job
		want bool
	}{
		{"youtube Error", database.Job{Status: database.StatusError, Platform: "youtube"}, true},
		{"youtube Finished with an incomplete tail", database.Job{Status: database.StatusFinished, Platform: "youtube", IncompleteTail: true}, true},
		{"youtube Finished", database.Job{Status: database.StatusFinished, Platform: "youtube"}, false},
		{"twitch Error", database.Job{Status: database.StatusError, Platform: "twitch"}, false},
		{"twitch Finished with an incomplete tail", database.Job{Status: database.StatusFinished, Platform: "twitch", IncompleteTail: true}, false},
		{"youtube Downloading", database.Job{Status: database.StatusDownloading, Platform: "youtube"}, false},
	} {
		c.job.ID = "staged"
		if got := ListJob(base, &c.job).HasStaging != nil; got != c.want {
			t.Errorf("%s: flagged = %v, want %v", c.name, got, c.want)
		}
	}
	if row := ListJob(base, nil); row.HasStaging != nil || row.Job != nil {
		t.Error("a nil job must list as an empty row")
	}
}
