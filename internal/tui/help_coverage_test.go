package tui

import (
	"strings"
	"testing"
)

// TestHelpCoversEveryChord enforces the invariant buildMenuItems claims in
// its own doc comment — "the single source of truth for all chords, menu
// entries, feedback hints, and help text" — which nothing was checking.
//
// The help overlay derives sections only for the categories in
// categoryHelpTitles (Action, Request, Open, Extras); every other category is
// dropped by sectionsFromMenu and covered by hand in quickKeys. That split
// is deliberate (the static text is written for newcomers and is richer
// than the terse menu labels), but it silently drops any chord added to a
// category the derived path skips. This test is the seam: add "A X" and
// help picks it up automatically; add an Other-category chord and this
// fails until it is documented somewhere the reader can actually see.
func TestHelpCoversEveryChord(t *testing.T) {
	app := NewApp()
	items := app.buildMenuItems()
	if len(items) == 0 {
		t.Fatal("buildMenuItems returned nothing — the invariant cannot be checked")
	}

	// Every chord reachable from the help overlay: the derived sections
	// plus the three static ones.
	h := NewHelpModel()
	h.SetMenuItems(items)
	documented := map[string]bool{}
	for _, sec := range h.orderedSections() {
		for _, k := range sec.keys {
			documented[strings.TrimSpace(k.key)] = true
		}
	}

	for _, it := range items {
		chord := strings.TrimSpace(it.Chord)
		if documented[chord] {
			continue
		}
		// A NeedsConfirm chord is deliberately DISPLAYED with its confirm
		// keypress ("A C" -> "A C C"), because that is what the operator
		// actually has to type. Accept either spelling rather than
		// re-deriving the rule here, so this stays a coverage check and not
		// a copy of sectionsFromMenu's formatting.
		if parts := strings.Fields(chord); len(parts) == 2 && documented[chord+" "+parts[1]] {
			continue
		}
		t.Errorf("chord %q (%q, category %q) appears in no help section — "+
			"add its category to categoryHelpTitles or list it in quickKeys",
			it.Chord, it.Label, it.Category)
	}
}

// TestHelpRowsWrapInsteadOfBeingCut pins the fix for the reported bug: a
// description longer than the viewport's content width used to be silently
// CUT by the viewport's own line truncation (SoftWrap is off), so the filter
// query-language rows in navigationKeys — themselves written to teach the
// syntax to a newcomer — lost their tail at any width narrower than roughly
// 92 columns. buildContent must now wrap long descriptions itself so every
// row survives at a normal 80-column terminal.
func TestHelpRowsWrapInsteadOfBeingCut(t *testing.T) {
	app := NewApp()
	h := NewHelpModel()
	h.SetMenuItems(app.buildMenuItems())
	h.SetSize(80, 100)
	h.Toggle()

	view := stripANSI(h.View())
	for _, want := range []string{
		`Filter: status:live channel:"name" platform:youtube`,
		`-negate  a|b (either)  "quoted phrase"`,
	} {
		if !strings.Contains(view, want) {
			t.Errorf("help view at width 80 lacks %q — the row was cut instead of wrapped:\n%s", want, view)
		}
	}

	// The longest quickKeys row (F's description) is the worst case at this
	// width; it must wrap rather than disappear too. The wrap point falls
	// between "log" and "level", so the tail is checked on its own rather
	// than as one contiguous phrase spanning the line break.
	if !strings.Contains(view, "level (Logs)") {
		t.Errorf("help view at width 80 lost the tail of the F row:\n%s", view)
	}
}

// TestHelpWrapIndentsContinuationLines pins the visual shape of a wrapped
// row: the continuation must land under the description column (2-space
// indent + the 14-cell key column), not at the left margin, or a wrapped
// row reads as a new unlabeled entry rather than the tail of the one above.
//
// Reads the viewport's own content directly rather than the full overlay
// View(), which centers the box in the terminal and pads every line with a
// leading margin plus a border glyph — noise this check has no interest in.
func TestHelpWrapIndentsContinuationLines(t *testing.T) {
	app := NewApp()
	h := NewHelpModel()
	h.SetMenuItems(app.buildMenuItems())
	h.SetSize(80, 100)
	h.Toggle()

	content := stripANSI(h.viewport.View())
	lines := strings.Split(content, "\n")
	found := false
	for i, line := range lines {
		if !strings.Contains(line, "Cycle status filter (Tasks)") {
			continue
		}
		found = true
		if i+1 >= len(lines) {
			t.Fatalf("F row has no continuation line at width 80:\n%s", content)
		}
		cont := lines[i+1]
		const wantIndent = 16 // 2-space margin + 14-cell key column
		if !strings.HasPrefix(cont, strings.Repeat(" ", wantIndent)) {
			t.Errorf("continuation line %q is not indented %d cells under the description column", cont, wantIndent)
		}
		if strings.TrimSpace(cont) == "" {
			t.Errorf("continuation line is blank — the wrap produced no visible tail")
		}
	}
	if !found {
		t.Fatal("F row not found in the rendered help view")
	}
}
