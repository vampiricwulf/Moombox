package tui

import (
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestCancelChordReportsACancelThatDidNotHappen: A C decides on the row the
// list shows, and the job may have finished, failed or been cancelled since.
// OnCancelJob leaves such a job as it ended and says so, and the feedback
// line now does too — it read "Cancelled" for a job nothing cancelled. A
// batch counts only the jobs it cancelled.
//
// Mutants: ignore OnCancelJob's answer on the single-job path — "Cancelled:
// Old Stream"; count every attempted job in the batch — "Cancelled 2 jobs".
func TestCancelChordReportsACancelThatDidNotHappen(t *testing.T) {
	withCancel := func() *App {
		a := NewApp()
		a.OnCancelJob = func(id string) bool { return id != "gone" } // "gone" ended meanwhile
		return a
	}
	check := func(t *testing.T, a *App, want string, warning bool) {
		t.Helper()
		if a.feedback.msg != want {
			t.Errorf("feedback = %q, want %q", a.feedback.msg, want)
		}
		if got := feedbackColor(a.feedback.msg, a.feedback.sev); warning && got != ColorYellow {
			t.Errorf("%q rendered %v, want the warning colour", a.feedback.msg, got)
		}
	}

	t.Run("single", func(t *testing.T) {
		a := withCancel()
		a.dispatchAction("A C", &database.Job{ID: "gone", Title: "Old Stream", Status: database.StatusDownloading})
		check(t, a, "Not cancelled — Old Stream had already ended", true)
		a.dispatchAction("A C", &database.Job{ID: "live", Title: "New Stream", Status: database.StatusDownloading})
		check(t, a, "Cancelled: New Stream", false)
	})

	batch := func(ids ...string) *App {
		a := withCancel()
		var jobs []*database.Job
		for _, id := range ids {
			jobs = append(jobs, &database.Job{ID: id, Title: id, Status: database.StatusDownloading})
		}
		a.taskList.SetJobs(jobs)
		for _, id := range ids {
			a.taskList.ToggleSelection(id)
		}
		a.dispatchAction("A C", nil)
		return a
	}
	t.Run("batch, one ended", func(t *testing.T) {
		check(t, batch("gone", "live"), "Cancelled 1 job; 1 job had already ended", true)
	})
	t.Run("batch, all ended", func(t *testing.T) {
		a := batch("gone")
		check(t, a, "Not cancelled: 1 job had already ended", true)
		if strings.HasPrefix(a.feedback.msg, "Cancelled") {
			t.Error("the batch claimed a cancel")
		}
	})
	t.Run("batch, none ended", func(t *testing.T) {
		check(t, batch("live", "live2"), "Cancelled 2 jobs", false)
	})
}
