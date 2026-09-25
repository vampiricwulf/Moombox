package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"

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
// Mutant: dropping the gate (the every-tick rebuild returns), or gating on the
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

// descriptionRows returns the wrapped Description lines — every rowField that
// follows the "Description" header. Reads m.rows, not the rendered view, so
// the assertion is about the rebuild rather than about styling.
func descriptionRows(m *JobDetailsModel) []string {
	var out []string
	inDescription := false
	for _, r := range m.rows {
		if r.kind == rowHeader {
			inDescription = r.label == "Description"
			continue
		}
		if inDescription && r.kind == rowField {
			out = append(out, r.value)
		}
	}
	return out
}

// widestDescriptionRow is the widest wrapped Description line, in columns.
func widestDescriptionRow(m *JobDetailsModel) int {
	widest := 0
	for _, line := range descriptionRows(m) {
		widest = max(widest, runewidth.StringWidth(line))
	}
	return widest
}

// buildRows bakes the panel width into the rows it emits — the Description
// and Error blocks are wrapped to the value column there, not at render time
// — so a width change has to REBUILD the rows, not just re-render the ones it
// already has. cycleFocus gives each panel a different share of the terminal,
// so every Tab press resizes this one.
//
// Mutant: SetSize only calling updateViewportContent (what it did). The
// SetProgress gate then holds the stale wrap: the same store pointer in the
// same second returns early, so the description stayed wrapped at the OLD
// width for up to a second — and for a terminal job, whose progress-store
// entry is deleted, until the next 1Hz RefreshRelativeTimes.
func TestSetSizeRewrapsTheDescription(t *testing.T) {
	m := NewJobDetailsModel()
	m.SetSize(120, 24)
	job := downloadingJob("j")
	job.Description = strings.Repeat("archive ", 60)
	m.SetJob(job)

	p := &ProgressData{Progress: "V:1 A:1"}
	m.SetProgress(p)

	const wideWrap = 120 - 2 - labelWidth
	if got := widestDescriptionRow(m); got <= wideWrap/2 {
		t.Fatalf("the fixture must wrap wide first: widest description line is %d columns, want close to %d", got, wideWrap)
	}

	m.SetSize(50, 24)
	const narrowWrap = 50 - 2 - labelWidth
	if got := widestDescriptionRow(m); got == 0 || got > narrowWrap {
		t.Errorf("SetSize left the description wrapped at the old width: widest line is %d columns, the panel wraps at %d", got, narrowWrap)
	}

	// ...and the progress gate must not be what eventually repairs it: the
	// same pointer in the same second returns early, leaving exactly the rows
	// SetSize built. The in-place mutation proves the gate really did fire —
	// a rebuild would have picked the new text up.
	m.lastProgressSec = time.Now().Unix()
	p.Progress = "V:2 A:2"
	m.SetProgress(p)
	if got := progressRowValue(m); got != "V:1 A:1" {
		t.Fatalf("the gate did not fire, so this says nothing about SetSize: Progress row is %q", got)
	}
	if got := widestDescriptionRow(m); got == 0 || got > narrowWrap {
		t.Errorf("after the gated SetProgress the widest description line is %d columns, the panel wraps at %d", got, narrowWrap)
	}
}
