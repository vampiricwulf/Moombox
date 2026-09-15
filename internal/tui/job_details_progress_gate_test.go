package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// progressRowValue returns the details panel's "Progress" row value, or "".
// Reads m.rows rather than the rendered view so the assertion is about the
// rebuild, not about styling or wrapping.
func progressRowValue(m *JobDetailsModel) string {
	for _, r := range m.rows {
		if r.kind == rowField && r.label == "Progress" {
			return r.value
		}
	}
	return ""
}

func downloadingJob(id string) *database.Job {
	return &database.Job{ID: id, Title: "Title " + id, Status: database.StatusDownloading, Platform: "youtube"}
}

// Same pointer + same second = the rebuild would produce identical rows, so
// it must not run. The in-place mutation is the probe: nothing in production
// mutates a stored ProgressData (every write allocates a new one), so a row
// carrying the mutated text can only mean the rows were rebuilt.
//
// Mutant: dropping the gate (the 60Hz rebuild returns), or gating on the
// pointer alone (the wall-clock rows would then freeze).
func TestSetProgressSkipsTheRebuildWhenNothingChanged(t *testing.T) {
	m := NewJobDetailsModel()
	m.SetSize(80, 24)
	m.SetJob(downloadingJob("j"))

	p := &ProgressData{Progress: "V:1 A:1"}
	m.SetProgress(p)
	if got := progressRowValue(m); got != "V:1 A:1" {
		t.Fatalf("first SetProgress must build the Progress row, got %q", got)
	}

	p.Progress = "V:2 A:2"
	m.SetProgress(p)
	if got := progressRowValue(m); got != "V:1 A:1" {
		t.Errorf("same pointer in the same second rebuilt the rows (got %q) — the gate is gone", got)
	}

	// A new second must rebuild: Duration, "Starts In" and the relative
	// suffixes are recomputed from time.Now on every build.
	m.lastProgressSec--
	m.SetProgress(p)
	if got := progressRowValue(m); got != "V:2 A:2" {
		t.Errorf("a new second must rebuild, got %q", got)
	}

	// A new pointer always rebuilds, same second or not.
	m.SetProgress(&ProgressData{Progress: "V:3 A:3"})
	if got := progressRowValue(m); got != "V:3 A:3" {
		t.Errorf("a new pointer must rebuild, got %q", got)
	}
}

// Switching jobs clears the overlay, so the gate has to be rearmed with it —
// otherwise the same store pointer coming back within the same second is
// skipped and the Progress rows stay missing for up to a second.
//
// Mutant: resetting progressOverlay in SetJob without resetting lastProgress.
func TestSetJobRearmsTheProgressGate(t *testing.T) {
	m := NewJobDetailsModel()
	m.SetSize(80, 24)
	a, b := downloadingJob("a"), downloadingJob("b")

	m.SetJob(a)
	p := &ProgressData{Progress: "V:1 A:1"}
	m.SetProgress(p)

	m.SetJob(b)
	m.SetJob(a)
	if got := progressRowValue(m); got != "" {
		t.Fatalf("a job switch must clear the overlay, got %q", got)
	}

	m.SetProgress(p)
	if got := progressRowValue(m); got != "V:1 A:1" {
		t.Errorf("the same pointer after a job switch must rebuild, got %q", got)
	}
}

// A same-job re-sync (the SetJob call every display-column change makes)
// rebuilds the rows itself and keeps the overlay, so the gate must not be
// rearmed there — and the rows must still show the overlay afterwards.
func TestSameJobResyncKeepsTheOverlay(t *testing.T) {
	m := NewJobDetailsModel()
	m.SetSize(80, 24)
	job := downloadingJob("j")
	m.SetJob(job)
	m.SetProgress(&ProgressData{Progress: "V:1 A:1"})

	job.Title = "renamed"
	m.SetJob(job)
	if got := progressRowValue(m); got != "V:1 A:1" {
		t.Errorf("a same-job re-sync dropped the progress overlay, got %q", got)
	}
}
