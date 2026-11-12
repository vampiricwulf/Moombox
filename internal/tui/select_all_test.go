package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

func ctrlAMsg() tea.KeyPressMsg { return tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl} }

// The tests below reach handleKey's comparison through msg.String().
func TestCtrlAFixtureStringifiesToTheKeyConstant(t *testing.T) {
	if got := ctrlAMsg().String(); got != keyCtrlA {
		t.Fatalf("ctrlAMsg().String() = %q, want %q", got, keyCtrlA)
	}
}

func sortedSelection(a *App) []string {
	ids := a.taskList.SelectedIDs()
	slices.Sort(ids)
	return ids
}

// Ctrl+A selects every row the Tasks list shows, in the normal layout and the
// compact one, and says so (owner ruling R2-T1). A second press clears
// nothing; Esc does.
//
// Mutants: SelectAllShown toggling instead of adding (the second press
// empties the selection); the key handled only in the normal layout.
func TestCtrlASelectsEveryShownRow(t *testing.T) {
	for _, size := range [][2]int{{120, 40}, {40, 10}, {30, 6}} {
		r := newCompactRig(t, size[0], size[1])
		a := r.app
		a.Update(ctrlAMsg())
		if got, want := a.taskList.SelectedCount(), len(compactJobs()); got != want {
			t.Fatalf("%v: selected %d rows, want %d", size, got, want)
		}
		if a.feedback.msg != "Selected 7 jobs · Esc clears" {
			t.Errorf("%v: feedback = %q", size, a.feedback.msg)
		}
		if !strings.Contains(plainFrame(a), "7 sel") {
			t.Errorf("%v: the count is not on screen:\n%s", size, plainFrame(a))
		}
		a.Update(ctrlAMsg())
		if got := a.taskList.SelectedCount(); got != 7 {
			t.Errorf("%v: a second Ctrl+A left %d rows selected, want 7", size, got)
		}
		r.press("esc")
		if got := a.taskList.SelectedCount(); got != 0 {
			t.Errorf("%v: Esc left %d rows selected", size, got)
		}
	}
}

// Select-all means the rows on screen: what the filter passes, and archived
// rows only while the archive is open. A row ticked by hand stays ticked.
//
// Mutant: SelectAllShown walking m.jobs instead of the list's items.
func TestCtrlASelectsOnlyWhatTheFilterAndTheArchiveShow(t *testing.T) {
	r := newCompactRig(t, 120, 40)
	a := r.app
	old := time.Now().Add(-90 * 24 * time.Hour).UTC().Format(time.RFC3339)
	jobs := append(compactJobs(), &database.Job{ID: "old", Title: "Long finished", Status: database.StatusFinished,
		Watched: true, Platform: "youtube", VideoID: "hhhhhhhhhhh", UpdatedAt: old})
	a.Update(JobsUpdateMsg{Jobs: jobs})
	if !a.taskList.archivedSet["old"] {
		t.Fatal("the fixture's old job is not archived")
	}

	a.Update(ctrlAMsg())
	if got := sortedSelection(a); slices.Contains(got, "old") || len(got) != 7 {
		t.Fatalf("collapsed archive: selection = %v, want the 7 rows shown", got)
	}
	a.taskList.ToggleArchive()
	a.Update(ctrlAMsg())
	if got := sortedSelection(a); !slices.Contains(got, "old") || len(got) != 8 {
		t.Fatalf("expanded archive: selection = %v, want all 8", got)
	}

	r.press("esc")
	a.taskList.applyQuery("status:error")
	a.Update(ctrlAMsg())
	if got := sortedSelection(a); !slices.Equal(got, []string{"err"}) {
		t.Fatalf("filtered: selection = %v, want [err]", got)
	}
	if a.feedback.msg != "Selected 1 job · Esc clears" {
		t.Errorf("feedback = %q", a.feedback.msg)
	}

	a.taskList.applyQuery("nothing-matches-this")
	a.Update(ctrlAMsg())
	if a.taskList.SelectedCount() != 0 || a.feedback.msg != "No tasks to select" {
		t.Errorf("empty list: %d selected, feedback %q", a.taskList.SelectedCount(), a.feedback.msg)
	}
}

// The rows gain their tick on the frame after the key: the selection is in
// the task list's render-cache key.
func TestCtrlARedrawsTheList(t *testing.T) {
	r := newCompactRig(t, 120, 40)
	a := r.app
	before := a.taskList.View()
	a.Update(ctrlAMsg())
	after := a.taskList.View()
	if before == after || strings.Count(stripANSI(after), "│✓ ") != 7 {
		t.Errorf("the list did not redraw with 7 ticks:\n%s", stripANSI(after))
	}
}

// Ctrl+A belongs to the Tasks panel on the main view only. Another panel
// ignores it; an armed chord answers it as an invalid chord, so a confirm
// that named its count cannot grow; the filter box keeps it as "start of
// line"; and no overlay lets it through to the list underneath.
//
// Mutants: the key handled above the chord system, above the search
// intercept, or above the overlay dispatch.
func TestCtrlAIsNotTakenOutsideTheTasksList(t *testing.T) {
	t.Run("other panels", func(t *testing.T) {
		for _, p := range []FocusPanel{PanelDetails, PanelLogs} {
			r := newCompactRig(t, 120, 40)
			r.app.focusedPanel = p
			r.app.Update(ctrlAMsg())
			if n := r.app.taskList.SelectedCount(); n != 0 {
				t.Errorf("panel %v: Ctrl+A selected %d rows", p, n)
			}
		}
	})
	t.Run("armed chord", func(t *testing.T) {
		r := newCompactRig(t, 120, 40)
		r.selectJob(t, "err")
		r.press("a", "d")
		if r.app.chord.action == "" {
			t.Fatal("A D did not arm its confirm")
		}
		r.app.Update(ctrlAMsg())
		if n := r.app.taskList.SelectedCount(); n != 0 {
			t.Errorf("Ctrl+A under an armed confirm selected %d rows", n)
		}
		if r.app.chord.prefix != "" {
			t.Error("the chord stayed armed")
		}
		r.pressRunLast("d")
		if len(r.deleted) != 0 {
			t.Errorf("a lone D after it deleted %v", r.deleted)
		}
	})
	t.Run("filter box", func(t *testing.T) {
		r := newCompactRig(t, 120, 40)
		a := r.app
		r.press("/", "e", "r")
		a.Update(ctrlAMsg())
		if n := a.taskList.SelectedCount(); n != 0 {
			t.Fatalf("Ctrl+A in the filter box selected %d rows", n)
		}
		if !a.taskList.IsSearching() {
			t.Fatal("Ctrl+A closed the filter box")
		}
		// Start of line: the next character lands in front.
		r.press("x")
		if got := a.taskList.searchInput.Value(); got != "xer" {
			t.Errorf("filter box holds %q, want %q (Ctrl+A is start of line there)", got, "xer")
		}
	})
	t.Run("overlays", func(t *testing.T) {
		for _, c := range overlayCases(t) {
			a := overlayTestApp(t)
			c.open(a)
			if !c.visible(a) {
				t.Errorf("%s overlay: did not open, so the row proves nothing", c.name)
				continue
			}
			a.Update(ctrlAMsg())
			if n := a.taskList.SelectedCount(); n != 0 {
				t.Errorf("%s overlay: Ctrl+A reached the list and selected %d rows", c.name, n)
			}
		}
	})
}

// Ctrl+C still quits with everything selected.
func TestCtrlCQuitsAfterSelectAll(t *testing.T) {
	r := newCompactRig(t, 120, 40)
	r.app.Update(ctrlAMsg())
	if _, cmd := r.app.handleKey(ctrlCMsg()); !isQuit(cmd) {
		t.Error("Ctrl+C did not quit")
	}
}

// Below 30x6 only Ctrl+C works.
func TestCtrlAIsIgnoredInATooSmallTerminal(t *testing.T) {
	r := newCompactRig(t, 29, 5)
	r.app.Update(ctrlAMsg())
	if n := r.app.taskList.SelectedCount(); n != 0 {
		t.Errorf("Ctrl+A selected %d rows in a terminal with only a notice", n)
	}
}

// Both Help lists name the key.
func TestHelpListsSelectAll(t *testing.T) {
	app := NewApp()
	h := NewHelpModel()
	h.SetMenuItems(app.buildMenuItems())
	h.SetSize(100, 200)
	h.Toggle()
	if view := stripANSI(h.viewport.View()); !strings.Contains(view, "Select every task shown (Tasks)") {
		t.Errorf("help lacks the Ctrl+A row:\n%s", view)
	}
	found := false
	for _, k := range bareNavigationKeys {
		if k.key == "Ctrl+A" {
			found = true
		}
	}
	if !found {
		t.Error("the compact help lacks the Ctrl+A row")
	}
}
