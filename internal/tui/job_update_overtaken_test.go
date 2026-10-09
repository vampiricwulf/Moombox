package tui

import (
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestATickThatOvertakesAWriteLeavesTheWriteApplied: UpdateJobFields notifies
// after releasing the database lock, so the next progress tick's event can
// reach the TUI before the event of the write just ahead of it — a quality
// split's twitch_quality, a rename. The tick's row already holds the write,
// but a tick replaces no row; with one version for both stores, the tick's
// was noted all the same, the write then arrived "stale" and was dropped, and
// the row kept the old title and quality until a resync.
//
// The write's own row still has to be applied, without taking the progress
// store back to the progress columns it read before the ticks; and a tick
// older than the progress store's newest — one that overtook nothing — is
// still dropped, as is one older than a whole row applied after it.
//
// MUTANTS: one version for row and progress again (the row keeps the old
// title); the write updating the progress store although a newer tick is
// there (progress reads "", the write's own); a tick's staleness judged
// against the row rather than the progress store (tick X, older than Y,
// puts the older tick's Downloading into the status map); a write's progress
// columns not counted as applied to the progress store (the tick read back
// before Finished puts a progress entry back).
func TestATickThatOvertakesAWriteLeavesTheWriteApplied(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "overtaken.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.AddJob(&database.Job{ID: "live", VideoID: "live", URL: "u", Title: "old title",
		Platform: "twitch", Status: database.StatusDownloading}); err != nil {
		t.Fatal(err)
	}
	all, err := db.GetAllJobs()
	if err != nil {
		t.Fatal(err)
	}
	app := NewApp()
	app.Update(JobsUpdateMsg{Jobs: all})
	var events []*database.JobChange
	db.OnJobChange(func(ev *database.JobChange) { events = append(events, ev) })

	db.UpdateJobFields("live", map[string]any{"title": "new title", "twitch_quality": "720p60"})
	db.UpdateJobFields("live", map[string]any{"progress": "V:5", "percent": 5.0})
	db.UpdateJobFields("live", map[string]any{"progress": "V:6", "percent": 6.0})
	if len(events) != 3 {
		t.Fatalf("%d events, want 3", len(events))
	}
	write, tickA, tickB := events[0], events[1], events[2]
	for _, ev := range []*database.JobChange{tickB, write, tickA} {
		app.Update(JobUpdateMsg{Change: ev})
	}
	row := app.taskList.GetJobByID("live")
	if row == nil || row.Title != "new title" || row.TwitchQuality != "720p60" {
		t.Errorf("task list row %+v after the write's event arrived behind the ticks; want title %q, quality %q",
			row, "new title", "720p60")
	}
	if p := app.progressStore.Get("live"); p == nil || p.Progress != "V:6" {
		t.Errorf("progress store holds %+v, want the newest tick's V:6", p)
	}

	// A tick older than one already applied is dropped although no newer
	// row is held: it would put the older tick's status (Downloading, read
	// back before the Muxing write) into the status map behind the terminal
	// title. The Muxing write, arriving last, still replaces the row, and
	// leaves the newer tick's progress in the store.
	events = nil
	db.UpdateJobFields("live", map[string]any{"progress": "V:7", "percent": 7.0})
	db.UpdateJobFields("live", map[string]any{"status": database.StatusMuxing, "progress": "Muxing"})
	db.UpdateJobFields("live", map[string]any{"progress": "mux 50%", "percent": 50.0})
	tickX, muxing, tickY := events[0], events[1], events[2]
	app.Update(JobUpdateMsg{Change: tickY})
	app.Update(JobUpdateMsg{Change: tickX})
	if st := app.statusMap["live"]; st != database.StatusMuxing {
		t.Errorf("status map holds %q after a tick older than the one applied, want Muxing", st)
	}
	app.Update(JobUpdateMsg{Change: muxing})
	if row := app.taskList.GetJobByID("live"); row == nil || row.Status != database.StatusMuxing {
		t.Errorf("task list row %+v, want the Muxing write's row", row)
	}
	if p := app.progressStore.Get("live"); p == nil || p.Progress != "mux 50%" {
		t.Errorf("progress store holds %+v, want the newer tick's mux 50%%", p)
	}

	// The other direction: a tick read back before the Finished write,
	// delivered after it, is dropped, and does not put back the progress
	// entry the terminal write removed.
	events = nil
	db.UpdateJobFields("live", map[string]any{"progress": "mux 90%", "percent": 90.0})
	db.UpdateJobFields("live", map[string]any{"status": database.StatusFinished})
	tickC, finished := events[0], events[1]
	app.Update(JobUpdateMsg{Change: finished})
	app.Update(JobUpdateMsg{Change: tickC})
	if p := app.progressStore.Get("live"); p != nil {
		t.Errorf("progress store holds %+v for the Finished job: the older tick was applied", p)
	}
	if row := app.taskList.GetJobByID("live"); row == nil || row.Status != database.StatusFinished {
		t.Errorf("task list row %+v, want Finished", row)
	}
}
