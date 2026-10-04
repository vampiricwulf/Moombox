package tui

import (
	"fmt"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestTaskListClickOutsideThePageSelectsNothing pins the row bound on
// SelectAtOffset. handleMouse hands it the click's offset inside the panel
// content below the header; the panel's bottom border sits at offset perPage,
// and so does the search box while it is open. Without the bound both
// resolved to Page*perPage + perPage — the first row of the NEXT page — so a
// click on the border silently moved the selection off screen.
//
// Mutant: drop the `y >= perPage` check — the border click selects row
// perPage and reports true.
func TestTaskListClickOutsideThePageSelectsNothing(t *testing.T) {
	m := NewTaskListModel()
	m.SetSize(60, 10)
	jobs := make([]*database.Job, 20)
	for i := range jobs {
		jobs[i] = &database.Job{ID: fmt.Sprintf("j%02d", i), Title: fmt.Sprintf("job %d", i), Status: database.StatusDownloading}
	}
	m.SetJobs(jobs)

	perPage := m.list.Paginator.PerPage
	if perPage <= 0 || perPage >= len(jobs) {
		t.Fatalf("premise lost: perPage=%d with %d jobs — the list must paginate", perPage, len(jobs))
	}

	// The last row of the page is a row.
	if !m.SelectAtOffset(perPage-1) || m.list.Index() != perPage-1 {
		t.Fatalf("offset %d (last row) selected index %d, want it selected", perPage-1, m.list.Index())
	}
	m.list.Select(0)

	// The bottom border is not.
	if m.SelectAtOffset(perPage) {
		t.Errorf("offset %d (the bottom border) reported a selection", perPage)
	}
	if m.list.Index() != 0 || m.list.Paginator.Page != 0 {
		t.Errorf("the border click moved the selection to index %d page %d", m.list.Index(), m.list.Paginator.Page)
	}
	if m.SelectAtOffset(-1) {
		t.Error("a negative offset reported a selection")
	}

	// With the search box open the list gives up a row, and the box sits on
	// the row the bound now excludes.
	m.StartSearch()
	searching := m.list.Paginator.PerPage
	if searching != perPage-1 {
		t.Fatalf("premise lost: the search box took %d rows, want 1", perPage-searching)
	}
	if m.SelectAtOffset(searching) {
		t.Errorf("offset %d (the search box row) reported a selection", searching)
	}
	if m.list.Index() != 0 || m.list.Paginator.Page != 0 {
		t.Errorf("the search-box click moved the selection to index %d page %d", m.list.Index(), m.list.Paginator.Page)
	}
}
