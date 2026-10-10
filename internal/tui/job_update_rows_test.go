package tui

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// rowsHarness is the production path from a database write to the task list,
// without cmd/moombox: a real database, an App seeded from GetAllJobs (as
// tui_wiring's first snapshot is), OnSetWatched wired as tui_wiring wires it,
// and every OnJobChange event delivered as the JobUpdateMsg its forwarder
// sends.
type rowsHarness struct {
	t      *testing.T
	db     *database.Database
	app    *App
	events []*database.JobChange
}

// newRowsHarness holds one Finished job with a trim, a gap and two parts —
// the child rows GetAllJobs attaches and the trim dialog, the details panel
// and the parts list read.
func newRowsHarness(t *testing.T) *rowsHarness {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "rows.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.AddJob(&database.Job{ID: "vid", VideoID: "vid", URL: "u", Title: "t", Platform: "youtube",
		Status: database.StatusFinished, Gaps: []database.Gap{{From: 10, To: 12, Stream: "video"}}}); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"t - part1.mp4", "t - part2.mp4"} {
		if err := db.AddSegment(&database.Segment{JobID: "vid", SegmentIndex: i, Quality: "1080p", Filename: name}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AddTrim(&database.TrimRecord{ID: "t1", JobID: "vid", StartTime: 1, EndTime: 2, Filename: "t.mp4",
		CreatedAt: "2026-10-09T00:00:00Z", Duration: 1}); err != nil {
		t.Fatal(err)
	}
	all, err := db.GetAllJobs()
	if err != nil {
		t.Fatal(err)
	}
	h := &rowsHarness{t: t, db: db, app: NewApp()}
	h.app.Update(JobsUpdateMsg{Jobs: all})
	db.OnJobChange(func(ev *database.JobChange) { h.events = append(h.events, ev) })
	// cmd/moombox/tui_wiring.go's OnSetWatched for one id.
	h.app.OnSetWatched = func(ids []string, watched bool) error {
		v := 0
		if watched {
			v = 1
		}
		db.UpdateJobFields(ids[0], map[string]any{"watched": v, "resume_position": nil})
		return nil
	}
	return h
}

// deliver hands the App every event the database fired since the last call.
func (h *rowsHarness) deliver() {
	for _, ev := range h.events {
		h.app.Update(JobUpdateMsg{Change: ev})
	}
	h.events = nil
}

// row is the task list's copy of the job — what A W and the trim dialog read.
func (h *rowsHarness) row() *database.Job {
	h.t.Helper()
	j := h.app.taskList.GetJobByID("vid")
	if j == nil {
		h.t.Fatal("the job left the task list")
	}
	return j
}

// keepsChildRows checks the row still holds the child rows GetAllJobs loaded.
func (h *rowsHarness) keepsChildRows(after string) {
	h.t.Helper()
	j := h.row()
	if len(j.Trims) != 1 || len(trimInfosFromJob(j)) != 1 || len(j.Segments) != 2 || len(j.Gaps) != 1 {
		h.t.Errorf("after %s the row holds trims/segments/gaps %d/%d/%d (the trim dialog would list %d), want 1/2/1",
			after, len(j.Trims), len(j.Segments), len(j.Gaps), len(trimInfosFromJob(j)))
	}
}

// TestSingleJobAWTogglesTheRow: A W on one Finished job writes watched and
// resume_position through UpdateJobFields, and the TUI learns of it from the
// OnJobChange event. Neither column was in the twelve the update handler
// rebuilt the row for, so the task list kept the old row: the dim • never
// appeared, and the next A W computed !job.Watched from that stale row and
// marked the job watched again — a single job could never be unmarked. Two
// presses here must mark and then unmark it, in the database and on screen,
// and the row must keep its trims, parts and gaps through both.
//
// Mutants: hasDisplayChange back to an allow-list without watched (the first
// press leaves the row unwatched); the database's read-back without the child
// rows (the row loses its trims, parts and gaps).
func TestSingleJobAWTogglesTheRow(t *testing.T) {
	h := newRowsHarness(t)
	for i, want := range []bool{true, false} {
		_, cmd := h.app.dispatchAction("A W", h.row())
		if cmd == nil {
			t.Fatalf("press %d: A W dispatched nothing", i+1)
		}
		result := cmd()
		h.deliver()
		h.app.Update(result)

		stored, _ := h.db.GetJob("vid")
		if stored.Watched != want {
			t.Fatalf("press %d: the database row has watched=%v, want %v", i+1, stored.Watched, want)
		}
		row := h.row()
		if row.Watched != want {
			t.Errorf("press %d: the task list row has Watched=%v, want %v — the next A W toggles from it", i+1, row.Watched, want)
		}
		if got := strings.Contains(stripANSI(h.app.taskList.renderJob(row, false, false)), watchedGlyph); got != want {
			t.Errorf("press %d: the row shows the watched dot = %v, want %v", i+1, got, want)
		}
		h.keepsChildRows("an A W")
	}
}

// TestJobUpdatesKeepTheRowsChildRows: a write the TUI replaces its row for
// carries the child rows (a rename here), and a progress tick — whose row
// the database reads back without them — does not replace the row at all.
//
// Mutants: the database's read-back without the child rows (the rename
// fails); hasDisplayChange true for a tick too (the tick replaces the row
// with one without trims, parts or gaps).
func TestJobUpdatesKeepTheRowsChildRows(t *testing.T) {
	h := newRowsHarness(t)

	h.db.UpdateJobFields("vid", map[string]any{"title": "renamed"})
	h.deliver()
	if h.row().Title != "renamed" {
		t.Fatalf("the rename did not reach the row: %q", h.row().Title)
	}
	h.keepsChildRows("a rename")

	h.db.UpdateJobFields("vid", map[string]any{"total_chat_messages": 42})
	h.deliver()
	h.keepsChildRows("a progress tick")
}
