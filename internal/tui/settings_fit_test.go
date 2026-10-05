package tui

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// TestSettingsFieldSectionsFitTheScreen focuses every field of every field
// section at the two supported floor sizes and checks the frame fits. The
// focused field's help line (up to ~170 characters) was appended as one row
// the content budget counted once, and the box word-wrapped it into two or
// three: at 80x24 "Segment workers" drew 25 lines, and bubbletea drops the
// overflow from the TOP — the header and tab strip went first.
//
// Mutant: dropping infoRows from settingsContentHeight — the long-help fields
// overflow again.
func TestSettingsFieldSectionsFitTheScreen(t *testing.T) {
	for _, size := range [][2]int{{60, 20}, {80, 24}} {
		w, h := size[0], size[1]
		m := newSettingsModelForSave(t)
		m.SetSize(w, h)
		for si, sec := range sections {
			if sec.fields == nil {
				continue // Channels / Integrations render their own lists
			}
			for fi := range sec.fields {
				m.sectionIndex, m.fieldIndex, m.scrollOffset = si, fi, 0
				m.ensureFieldVisible()
				view := m.View()
				where := fmt.Sprintf("%dx%d %s/%s", w, h, sec.name, sec.fields[fi].label)
				if got := lipgloss.Height(view); got > h {
					t.Errorf("%s: frame is %d rows on a %d-row screen", where, got, h)
				}
				if !strings.Contains(view, "Settings") {
					t.Errorf("%s: the header is not on screen", where)
				}
				if !strings.Contains(view, sec.fields[fi].label) {
					t.Errorf("%s: the focused field is not on screen", where)
				}
			}
		}
	}
}
