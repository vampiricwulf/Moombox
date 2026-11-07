package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestChapterImportableOnlyACompleteFinishedJob: A C and A U are offered only
// for a Finished job not flagged incomplete_tail that names a file — never an
// Error or Cancelled row, which can still be resumed (owner decision,
// 2026-11-07).
//
// Mutant: chapterImportable accepting Error/Cancelled or ignoring IncompleteTail.
func TestChapterImportableOnlyACompleteFinishedJob(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  database.Job
		want bool
	}{
		{"finished", database.Job{Status: database.StatusFinished, OutputFile: "a.mp4"}, true},
		{"finished, missing its tail", database.Job{Status: database.StatusFinished, OutputFile: "a.mp4", IncompleteTail: true}, false},
		{"error with an archive", database.Job{Status: database.StatusError, OutputFile: "a.mp4"}, false},
		{"cancelled with an archive", database.Job{Status: database.StatusCancelled, OutputFile: "a.mp4"}, false},
		{"finished, no file", database.Job{Status: database.StatusFinished}, false},
	} {
		if got := chapterImportable(&tc.job); got != tc.want {
			t.Errorf("%s: chapterImportable = %v, want %v", tc.name, got, tc.want)
		}
	}
}
