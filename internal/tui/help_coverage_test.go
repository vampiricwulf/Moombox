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
// categoryHelpTitles (Action, Request, Open); every other category is
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
