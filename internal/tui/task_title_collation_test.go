package tui

import (
	"slices"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// Active rows are ordered by title the way the dashboard orders them
// (localeCompare with sensitivity "base"): case and accents ignored. A
// lowercased byte compare sank every accented title below "z" — the order
// below is what the dashboard showed for these titles while the TUI showed
// apple, Apple, oak, zebra, Zed, école, Émile, Ölwechsel.
//
// Mutant: the strings.ToLower byte compare restored.
func TestActiveRowsSortByTitleLikeTheDashboard(t *testing.T) {
	m := NewTaskListModel()
	m.SetSize(100, 30)
	titles := []string{"Zed", "Émile", "apple", "Apple", "école", "zebra", "Ölwechsel", "oak"}
	var jobs []*database.Job
	for i, title := range titles {
		jobs = append(jobs, &database.Job{ID: string(rune('a' + i)), Title: title, Status: database.StatusLive, Platform: "youtube"})
	}
	m.SetJobs(jobs)
	var got []string
	for _, it := range m.list.Items() {
		if ti, ok := it.(taskItem); ok && ti.job != nil {
			got = append(got, ti.job.Title)
		}
	}
	want := []string{"apple", "Apple", "école", "Émile", "oak", "Ölwechsel", "zebra", "Zed"}
	if !slices.Equal(got, want) {
		t.Errorf("order %v, want the dashboard's %v", got, want)
	}
}
