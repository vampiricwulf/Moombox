package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// keyMsg builds a KeyPressMsg for the special keys the search box handles.
func keyMsg(name string) tea.KeyPressMsg {
	switch name {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	default:
		return tea.KeyPressMsg{Code: rune(name[0]), Text: name}
	}
}

func searchTestList() *TaskListModel {
	m := NewTaskListModel()
	m.SetSize(60, 20)
	m.SetJobs([]*database.Job{
		{ID: "1", Title: "Minecraft with Chika", ChannelName: "Nanashi Mumei", VideoID: "aaaaaaaaaaa", Status: database.StatusLive},
		{ID: "2", Title: "Zatsudan Morning", ChannelName: "Mori Calliope", VideoID: "bbbbbbbbbbb", Status: database.StatusUpcoming},
		{ID: "3", Title: "APEX ranked", ChannelName: "Chika Fansub", VideoID: "ccccccccccc", Status: database.StatusUpcoming},
	})
	return m
}

func visibleJobIDs(m *TaskListModel) []string {
	var ids []string
	for _, it := range m.list.Items() {
		if ti, ok := it.(taskItem); ok && ti.job != nil {
			ids = append(ids, ti.job.ID)
		}
	}
	return ids
}

// TestPassesSearch covers the fuzzy match across title, channel, and video ID.
func TestPassesSearch(t *testing.T) {
	m := searchTestList()
	j := m.jobs[0] // "Minecraft with Chika" / "Nanashi Mumei" / aaaaaaaaaaa

	cases := []struct {
		query string
		want  bool
	}{
		{"", true},                // empty matches all
		{"minecraft", true},       // title exact-ish
		{"MINECRAFT", true},       // case-insensitive
		{"mchi", true},            // fuzzy subsequence in title
		{"mumei", true},           // channel match
		{"aaaaaaaaaaa", true},     // video ID match
		{"zatsudan", false},       // belongs to another job
	}
	for _, c := range cases {
		m.searchQuery = c.query
		if got := m.passesSearch(j); got != c.want {
			t.Errorf("passesSearch(%q) = %v, want %v", c.query, got, c.want)
		}
	}
}

// TestSearchFiltersVisibleList: a live query narrows the rendered rows, and
// clearing restores the full set.
func TestSearchFiltersVisibleList(t *testing.T) {
	m := searchTestList()
	if got := len(visibleJobIDs(m)); got != 3 {
		t.Fatalf("baseline visible = %d, want 3", got)
	}

	// "chika" fuzzy-matches job 1 (title) and job 3 (channel "Chika Fansub").
	m.searchQuery = "chika"
	m.rebuildVirtualList()
	ids := visibleJobIDs(m)
	if len(ids) != 2 {
		t.Fatalf("search 'chika' visible = %v, want 2 (jobs 1,3)", ids)
	}

	// A miss empties the list.
	m.searchQuery = "nonexistentquery"
	m.rebuildVirtualList()
	if got := len(visibleJobIDs(m)); got != 0 {
		t.Errorf("search miss visible = %d, want 0", got)
	}

	// Clearing restores everything.
	m.searchQuery = ""
	m.rebuildVirtualList()
	if got := len(visibleJobIDs(m)); got != 3 {
		t.Errorf("after clear visible = %d, want 3", got)
	}
}

// TestSearchLifecycle: open the box, apply a query via Enter, then clear it.
func TestSearchLifecycle(t *testing.T) {
	m := searchTestList()

	m.StartSearch()
	if !m.IsSearching() {
		t.Fatal("StartSearch should open the box")
	}

	// Enter applies the input value as the live query and closes the box.
	m.searchInput.SetValue("apex")
	if _, consumed := m.HandleSearchKey(keyMsg("enter")); !consumed {
		t.Fatal("Enter should be consumed by the open search box")
	}
	if m.IsSearching() {
		t.Error("Enter should close the box")
	}
	if m.searchQuery != "apex" {
		t.Errorf("query after Enter = %q, want %q", m.searchQuery, "apex")
	}
	if got := visibleJobIDs(m); len(got) != 1 || got[0] != "3" {
		t.Errorf("after Enter visible = %v, want [3]", got)
	}

	// ClearSearch (box closed with active query) restores the full list.
	if !m.ClearSearch() {
		t.Error("ClearSearch should report it cleared an active query")
	}
	if m.searchQuery != "" || len(visibleJobIDs(m)) != 3 {
		t.Errorf("after ClearSearch query=%q visible=%d, want empty/3", m.searchQuery, len(visibleJobIDs(m)))
	}
	if m.ClearSearch() {
		t.Error("ClearSearch with nothing active should return false")
	}
}

// TestSearchClosesOnFocusLoss: losing panel focus (e.g. a mouse click on
// another panel, which bypasses HandleSearchKey) must close the open box so
// it can't strand dead on an unfocused panel — while keeping the applied
// query as a filter.
func TestSearchClosesOnFocusLoss(t *testing.T) {
	m := searchTestList()
	m.SetFocused(true)
	m.StartSearch()
	m.searchInput.SetValue("chika")
	if _, consumed := m.HandleSearchKey(keyMsg("enter")); !consumed {
		t.Fatal("Enter should apply the query")
	}
	if len(visibleJobIDs(m)) != 2 {
		t.Fatalf("query applied visible = %d, want 2", len(visibleJobIDs(m)))
	}
	// Reopen the box, then lose focus mid-search.
	m.StartSearch()
	if !m.IsSearching() {
		t.Fatal("box should be open before focus loss")
	}
	m.SetFocused(false)
	if m.IsSearching() {
		t.Error("focus loss should close the search box")
	}
	// The applied query survives as a filter.
	if m.searchQuery != "chika" || len(visibleJobIDs(m)) != 2 {
		t.Errorf("after focus loss query=%q visible=%d, want chika/2", m.searchQuery, len(visibleJobIDs(m)))
	}
}

// TestSearchEscClearsQuery: Esc while the box is open closes it and clears an
// applied query in one press.
func TestSearchEscClearsQuery(t *testing.T) {
	m := searchTestList()
	m.searchQuery = "apex"
	m.rebuildVirtualList()

	m.StartSearch() // reopen over the active query
	if _, consumed := m.HandleSearchKey(keyMsg("esc")); !consumed {
		t.Fatal("Esc should be consumed by the open search box")
	}
	if m.IsSearching() || m.searchQuery != "" {
		t.Errorf("after Esc searching=%v query=%q, want closed/empty", m.IsSearching(), m.searchQuery)
	}
	if len(visibleJobIDs(m)) != 3 {
		t.Errorf("after Esc visible = %d, want 3", len(visibleJobIDs(m)))
	}
}
