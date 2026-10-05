package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// blockRows returns the unlabelled rowField lines under the header named
// label — the wrapped body of the Error or Description block.
func blockRows(m *JobDetailsModel, label string) []string {
	var out []string
	in := false
	for _, r := range m.rows {
		if r.kind == rowHeader || r.kind == rowSeparator {
			in = r.kind == rowHeader && r.label == label
			continue
		}
		if in && r.kind == rowField && r.label == "" {
			out = append(out, r.value)
		}
	}
	return out
}

// The Error and Description bodies carry no label and render from column 0,
// so they wrap to the whole content width renderRow is handed — not to the
// value column, which left a labelWidth-wide blank strip down the panel's
// right edge (at an 80-column terminal the 42-column Details panel set an
// error in 28).
//
// Mutant: either block wrapping to m.width-2-labelWidth again.
func TestErrorAndDescriptionWrapToThePanelWidth(t *testing.T) {
	const panelW = 42
	const contentW = panelW - 2
	m := NewJobDetailsModel()
	m.SetSize(panelW, 40)
	job := &database.Job{
		ID: "j", Title: "t", Status: database.StatusError, Platform: "youtube",
		Error:       strings.Repeat("probe: HTTP 403 Forbidden — login required ", 4),
		Description: strings.Repeat("karaoke night archive ", 12),
	}
	m.SetJob(job)

	for _, block := range []string{"Error", "Description"} {
		lines := blockRows(m, block)
		if len(lines) < 2 {
			t.Fatalf("%s: the fixture must wrap, got %d line(s)", block, len(lines))
		}
		widest := 0
		for _, l := range lines {
			w := ansi.StringWidth(l)
			if w > contentW {
				t.Errorf("%s: line %q is %d columns, wider than the %d-column content", block, l, w, contentW)
			}
			widest = max(widest, w)
		}
		if widest <= contentW-labelWidth {
			t.Errorf("%s: widest line is %d columns — still wrapped to the %d-column value column, not the %d-column panel", block, widest, contentW-labelWidth, contentW)
		}
	}
}
