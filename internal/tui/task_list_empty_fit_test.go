package tui

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// TestTaskListEmptyStateFitsThePanel: the empty states render outside the
// list (which pads and clips itself), and the panel's Height only pads — so
// with Logs focused, where the task panel is a quarter of the screen, the
// one-line hint wrapped into rows the panel did not have, and the three-line
// setup message pushed its top border off the screen.
//
// Mutant: dropping the MaxHeight/Width render of the empty block — the
// short panels grow past their height.
func TestTaskListEmptyStateFitsThePanel(t *testing.T) {
	for _, setup := range []bool{false, true} {
		for _, size := range [][2]int{{28, 4}, {40, 5}, {60, 6}, {80, 24}} {
			m := NewTaskListModel()
			m.JustCompletedSetup = setup
			m.SetSize(size[0], size[1])
			view := m.View()
			where := fmt.Sprintf("setup=%v %dx%d", setup, size[0], size[1])
			if got := lipgloss.Height(view); got != size[1] {
				t.Errorf("%s: panel is %d rows, want %d", where, got, size[1])
			}
			if !strings.Contains(view, "Tasks") {
				t.Errorf("%s: the header is gone", where)
			}
		}
	}
}

// The plain empty state names the chord that adds a video: A alone is only
// the Action prefix.
func TestTaskListEmptyStateNamesTheAddChord(t *testing.T) {
	m := NewTaskListModel()
	m.SetSize(80, 24)
	if view := m.View(); !strings.Contains(view, "A A to add a video") {
		t.Errorf("empty state does not name A A:\n%s", view)
	}
}
