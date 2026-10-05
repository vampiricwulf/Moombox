package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// openPagedMenu opens the real action menu at a height that splits it over
// several pages.
func openPagedMenu(t *testing.T) *ActionMenuModel {
	t.Helper()
	a := NewApp()
	m := a.actionMenu
	m.SetSize(80, 20)
	m.Open(a.buildMenuItems())
	if m.mainList.Paginator.TotalPages < 3 {
		t.Fatalf("the fixture must span at least 3 pages, got %d", m.mainList.Paginator.TotalPages)
	}
	return m
}

func selectedChord(t *testing.T, m *ActionMenuModel) string {
	t.Helper()
	item := m.selectedAction()
	if item == nil {
		t.Fatalf("the cursor (index %d) is on a header or spacer, not an action", m.mainList.Index())
	}
	return item.Chord
}

// The menu pages rather than scrolls, and its keymap names PgUp/PgDn/Home/End
// — but HandleKey only ever forwarded Up/Down/Enter, so the four were dead
// keys and the Open and Extras sections were reachable only by arrowing past
// every row above them. Each jump must also land on an action, never on a
// category header or blank spacer, where Enter does nothing.
//
// Mutant: dropping any one of the four cases from handleMainKey, or the
// settleMain call after it.
func TestActionMenuPagesWithPgUpPgDnHomeEnd(t *testing.T) {
	m := openPagedMenu(t)
	first := selectedChord(t, m)

	m.HandleKey(keyPgDown)
	if m.mainList.Paginator.Page != 1 {
		t.Fatalf("PgDn left the menu on page %d, want 1", m.mainList.Paginator.Page)
	}
	selectedChord(t, m)

	m.HandleKey(keyPgUp)
	if m.mainList.Paginator.Page != 0 {
		t.Fatalf("PgUp left the menu on page %d, want 0", m.mainList.Paginator.Page)
	}

	m.HandleKey(keyEnd)
	items := m.mainList.Items()
	last := ""
	for i := len(items) - 1; i >= 0; i-- {
		if mi, ok := items[i].(menuActionItem); ok && mi.action != nil {
			last = mi.action.Chord
			break
		}
	}
	if got := selectedChord(t, m); got != last {
		t.Errorf("End selected %q, want the last action %q", got, last)
	}

	m.HandleKey(keyHome)
	if got := selectedChord(t, m); got != first {
		t.Errorf("Home selected %q, want the first action %q", got, first)
	}

	// Every page jump lands on an action, from every starting row.
	for range m.mainList.Paginator.TotalPages + 1 {
		m.HandleKey(keyPgDown)
		selectedChord(t, m)
	}
	for range m.mainList.Paginator.TotalPages + 1 {
		m.HandleKey(keyPgUp)
		selectedChord(t, m)
	}
}

// Bubbles' pagination dots are switched off, so the footer is the only thing
// that says there is more below. One page needs no indicator.
//
// Mutant: dropping withPageIndicator from renderMain, or its TotalPages guard.
func TestActionMenuFooterShowsThePage(t *testing.T) {
	m := openPagedMenu(t)
	pages := m.mainList.Paginator.TotalPages
	if view := m.View(); !strings.Contains(view, fmt.Sprintf("1/%d PgUp/PgDn", pages)) {
		t.Errorf("page 1's footer lacks the page indicator:\n%s", view)
	}
	m.HandleKey(keyPgDown)
	if view := m.View(); !strings.Contains(view, fmt.Sprintf("2/%d PgUp/PgDn", pages)) {
		t.Errorf("page 2's footer lacks the page indicator:\n%s", view)
	}

	m.SetSize(80, 200)
	if m.mainList.Paginator.TotalPages != 1 {
		t.Fatalf("the tall fixture must fit on one page, got %d", m.mainList.Paginator.TotalPages)
	}
	if view := m.View(); strings.Contains(view, "PgUp/PgDn") || strings.Contains(view, "1/1") {
		t.Errorf("a one-page menu shows a page indicator:\n%s", view)
	}

	// Too narrow for the key names: the bare page count, never a wrap.
	if got := withPageIndicator("M to close", m.mainList.Paginator, 20); got != "M to close" {
		t.Errorf("one page: got %q, want the footer alone", got)
	}
	p := m.mainList.Paginator
	p.TotalPages, p.Page = 3, 1
	if got := withPageIndicator("M to close", p, 16); !strings.HasSuffix(got, " "+DimStyle.Render("2/3")) || strings.Contains(got, "PgUp") {
		t.Errorf("narrow footer: got %q, want the bare 2/3", got)
	}
	if got := withPageIndicator("M to close", p, 13); got != "M to close" {
		t.Errorf("too narrow for even 2/3: got %q, want the footer alone", got)
	}
}

// The job picker pages the same way, and a jump retracts an armed confirm
// exactly as Up/Down do — the armed row is no longer the selected one.
//
// Mutant: dropping the PgDn or End case from handleJobSelectKey, or its
// jobConfirm reset.
func TestActionMenuJobPickerPages(t *testing.T) {
	a := NewApp()
	m := a.actionMenu
	jobs := make([]*database.Job, 40)
	for i := range jobs {
		jobs[i] = &database.Job{ID: fmt.Sprintf("j%02d", i), Title: "t", Status: database.StatusError, Platform: "youtube"}
	}
	m.SetJobs(jobs)
	m.SetSize(80, 20)
	m.Open(a.buildMenuItems())
	for selectedChord(t, m) != "A D" {
		before := m.mainList.Index()
		m.HandleKey(keyDown)
		if m.mainList.Index() == before {
			t.Fatal("A D not found in the menu")
		}
	}
	m.HandleKey(keyEnter)
	if m.mode != menuJobSelect {
		t.Fatalf("Enter on A D did not open the job picker (mode %v)", m.mode)
	}
	if m.jobList.Paginator.TotalPages < 2 {
		t.Fatalf("the job fixture must span pages, got %d", m.jobList.Paginator.TotalPages)
	}

	m.HandleKey(keyEnter) // arm
	m.HandleKey(keyPgDown)
	if m.jobList.Paginator.Page != 1 {
		t.Errorf("PgDn left the picker on page %d, want 1", m.jobList.Paginator.Page)
	}
	if m.jobConfirm {
		t.Error("PgDn kept the confirm armed for a row that is no longer selected")
	}

	m.HandleKey(keyEnd)
	if got := m.jobList.Index(); got != len(jobs)-1 {
		t.Errorf("End selected job %d, want the last (%d)", got, len(jobs)-1)
	}
	m.HandleKey(keyHome)
	if got := m.jobList.Index(); got != 0 {
		t.Errorf("Home selected job %d, want 0", got)
	}
	if view := m.View(); !strings.Contains(view, fmt.Sprintf("1/%d", m.jobList.Paginator.TotalPages)) {
		t.Errorf("the picker's footer lacks the page indicator:\n%s", view)
	}
}
