package tui

import (
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
	rowPlain := m.renderJob(plain, false, false, 58)
	rowWatched := m.renderJob(watched, false, false, 58)
	if strings.Contains(stripANSI(rowPlain), watchedGlyph) {
		t.Errorf("unwatched row carries the glyph: %q", rowPlain)
	}
	if !strings.Contains(stripANSI(rowWatched), watchedGlyph) {
		t.Errorf("watched row lacks the glyph: %q", rowWatched)
	}
	for _, row := range []string{rowPlain, rowWatched} {
		for _, line := range strings.Split(row, "\n") {
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
}
