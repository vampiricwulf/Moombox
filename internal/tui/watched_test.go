package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestWatchedGlyphIsCountedInTheRow: a watched Finished job shows the dim •
// between the platform tag and the title, the title budget shrinks by the
// glyph's width, and the rendered row never exceeds the list width.
func TestWatchedGlyphIsCountedInTheRow(t *testing.T) {
	m := NewTaskListModel()
	m.SetSize(60, 10)
	long := strings.Repeat("Title ", 20)
	plain := &database.Job{ID: "a", Title: long, Status: database.StatusFinished, Platform: "youtube"}
	watched := &database.Job{ID: "b", Title: long, Status: database.StatusFinished, Platform: "youtube", Watched: true}

	if got, want := m.titleWidth(plain)-m.titleWidth(watched), watchedGlyphWidth; got != want {
		t.Fatalf("title budget shrank by %d, want %d", got, want)
	}
	rowPlain := m.renderJob(plain, false, false)
	rowWatched := m.renderJob(watched, false, false)
	if strings.Contains(stripANSI(rowPlain), watchedGlyph) {
		t.Errorf("unwatched row carries the glyph: %q", rowPlain)
	}
	if !strings.Contains(stripANSI(rowWatched), watchedGlyph) {
		t.Errorf("watched row lacks the glyph: %q", rowWatched)
	}
	for _, row := range []string{rowPlain, rowWatched} {
		for line := range strings.SplitSeq(row, "\n") {
			if w := runewidth.StringWidth(stripANSI(line)); w > 58 {
				t.Errorf("row wraps: width %d > 58: %q", w, line)
			}
		}
	}
}

// TestToggleWatchedChord: A W is a batch-capable Action chord filtered to
// Finished jobs; dispatch calls OnSetWatched with the flipped value.
func TestToggleWatchedChord(t *testing.T) {
	app := NewApp()
	var gotIDs []string
	var gotWatched bool
	app.OnSetWatched = func(ids []string, watched bool) error { gotIDs, gotWatched = ids, watched; return nil }
	var item *ActionMenuItem
	for i := range app.buildMenuItems() {
		if it := app.buildMenuItems()[i]; it.Chord == "A W" {
			item = &it
		}
	}
	if item == nil || !item.NeedsJob || !item.SupportsBatch {
		t.Fatalf("A W missing or not a batch job chord: %+v", item)
	}
	if item.JobFilter(&database.Job{Status: database.StatusDownloading}) || !item.JobFilter(&database.Job{Status: database.StatusFinished}) {
		t.Fatal("A W must filter to Finished jobs")
	}
	job := &database.Job{ID: "j1", Status: database.StatusFinished, Watched: false}
	if _, cmd := app.dispatchAction("A W", job); cmd != nil {
		cmd()
	}
	if len(gotIDs) != 1 || gotIDs[0] != "j1" || gotWatched != true {
		t.Fatalf("dispatch = (%v, %v), want ([j1], true)", gotIDs, gotWatched)
	}
	job.Watched = true
	if _, cmd := app.dispatchAction("A W", job); cmd != nil {
		cmd()
	}
	if gotWatched != false {
		t.Fatal("a watched job must toggle to unwatched")
	}

	// The batch path, which the single-job case above never reaches: a
	// selection of three, only two of them Finished. The Downloading one must
	// be filtered out (its row has no watched state to flip), and the flag is
	// "watched unless every selected Finished job already is".
	fin1 := &database.Job{ID: "f1", Status: database.StatusFinished}
	fin2 := &database.Job{ID: "f2", Status: database.StatusFinished, Watched: true}
	down := &database.Job{ID: "d1", Status: database.StatusDownloading}
	app.taskList.SetJobs([]*database.Job{fin1, fin2, down})
	for _, id := range []string{"f1", "f2", "d1"} {
		app.taskList.ToggleSelection(id)
	}
	if got := app.taskList.SelectedCount(); got != 3 {
		t.Fatalf("selected %d jobs, want 3 — the batch arm needs a selection", got)
	}
	gotIDs, gotWatched = nil, false
	if _, cmd := app.dispatchAction("A W", nil); cmd != nil {
		cmd()
	}
	// Sorted: SelectedIDs ranges a map, so only the SET is defined.
	if got := slices.Sorted(slices.Values(gotIDs)); len(got) != 2 || got[0] != "f1" || got[1] != "f2" {
		t.Fatalf("batch ids = %v, want [f1 f2] — the Downloading job must be filtered out", got)
	}
	if !gotWatched {
		t.Error("one of the two Finished jobs is unwatched, so the batch must set watched")
	}
	if app.taskList.SelectedCount() != 0 {
		t.Error("a fired batch must clear the selection")
	}

	// Both Finished jobs already watched: the batch flips the other way.
	fin1.Watched = true
	app.taskList.SetJobs([]*database.Job{fin1, fin2, down})
	for _, id := range []string{"f1", "f2", "d1"} {
		app.taskList.ToggleSelection(id)
	}
	gotIDs, gotWatched = nil, true
	if _, cmd := app.dispatchAction("A W", nil); cmd != nil {
		cmd()
	}
	if len(gotIDs) != 2 {
		t.Fatalf("batch ids = %v, want the two Finished jobs", gotIDs)
	}
	if gotWatched {
		t.Error("every selected Finished job was watched, so the batch must unwatch")
	}
}
