package tui

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"charm.land/glamour/v2"
	"github.com/charmbracelet/x/ansi"
)

// listItemStart matches a rendered list item's first row, after ANSI is
// stripped: its indent, then a bullet, an enumeration or a task checkbox. The
// second group's end is the column the item's text starts at.
var listItemStart = regexp.MustCompile(`^( *)(• |\d+\. |\[[ ✓]\] )`)

// renderedRows renders notes through the overlay's own body renderer at width
// and returns the rows as the reader sees them: ANSI stripped (ansi.Strip, so
// a hyperlink's OSC sequence cannot hide in a width either).
func renderedRows(t *testing.T, notes string, width int, dark bool) []string {
	t.Helper()
	o := newReleaseNotesOverlay()
	o.isDark = dark
	o.rawNotes = notes
	return strings.Split(ansi.Strip(o.renderBody(width)), "\n")
}

// checkHangingIndent asserts that every continuation row of every list item
// starts exactly under the item's text, and reports how many it checked.
func checkHangingIndent(t *testing.T, rows []string, width int) int {
	t.Helper()
	checked := 0
	hang := -1 // the open item's text column; -1 when no item is open
	for i, row := range rows {
		switch m := listItemStart.FindStringSubmatch(row); {
		case m != nil:
			hang = len(m[1]) + ansi.StringWidth(m[2])
		case strings.TrimSpace(row) == "":
			hang = -1
		case hang >= 0:
			checked++
			lead := len(row) - len(strings.TrimLeft(row, " "))
			if lead != hang {
				t.Errorf("width %d, row %d: a list item's continuation starts at column %d, want %d — under the text after its bullet:\n%q",
					width, i, lead, hang, row)
			}
		}
	}
	return checked
}

// TestReleaseNotesListItemsHangUnderTheirText is D-glamour's verification, on
// the real RELEASE_NOTES.md at 40, 60 and 100 columns: a long bullet's
// continuation rows start under its text, not flush with the bullet, and no
// row reaches past the edge glamour itself kept (the width less its document
// margin, which is also never past the width).
//
// Mutants: put glamour's wrap back (WithWordWrap(width) in renderBody) — the
// rows come back flush with the bullet; ignore the mark's column
// (`hang = ansi.StringWidth(line[:i])` deleted) — same; wrap to `width`
// instead of `width - margin` — the right-margin assertion fails.
func TestReleaseNotesListItemsHangUnderTheirText(t *testing.T) {
	raw, err := os.ReadFile("../../RELEASE_NOTES.md")
	if err != nil {
		t.Fatalf("read RELEASE_NOTES.md: %v", err)
	}
	notes := string(raw)
	for _, width := range []int{40, 60, 100} {
		rows := renderedRows(t, notes, width, true)
		checked := checkHangingIndent(t, rows, width)
		if width == 40 && checked == 0 {
			t.Skip("RELEASE_NOTES.md holds no list item long enough to wrap at 40 columns — " +
				"TestReleaseNotesHangCoversEveryListShape still pins the rule")
		}
		for i, row := range rows {
			if w := ansi.StringWidth(row); w > width-2 {
				t.Errorf("width %d, row %d is %d columns — past the right margin glamour kept (and %d past the width):\n%q",
					width, i, w, w-width, row)
			}
		}
	}
}

// TestReleaseNotesHangCoversEveryListShape pins the shapes RELEASE_NOTES.md
// does not happen to contain today: a numbered item hangs past its "1. ", a
// nested bullet past its own bullet, a task past its checkbox — and a CODE
// line that begins "1. " gets no hang at all, because only glamour's list
// prefixes carry the mark. The mark itself never reaches the screen.
//
// Mutants: drop the mark from Enumeration, or from Task — that row's
// continuation lands at the margin; replace the mark-detecting branch with a
// "• or N. " pattern over the text — the code line hangs; drop the
// strings.ReplaceAll — U+E000 is on screen.
func TestReleaseNotesHangCoversEveryListShape(t *testing.T) {
	const notes = "## Shapes\n\n" +
		"1. A numbered item that is long enough to wrap over several rows at forty columns.\n" +
		"2. Short.\n\n" +
		"- Outer bullet\n" +
		"  - A nested bullet that is long enough to wrap over several rows at forty columns.\n\n" +
		"- [x] A ticked task that is long enough to wrap over several rows at forty columns.\n" +
		"- [ ] An open task that is long enough to wrap over several rows at forty columns.\n\n" +
		"```\n1. code that looks like a numbered item and is long enough to wrap twice over\n```\n"
	for _, dark := range []bool{true, false} {
		rows := renderedRows(t, notes, 40, dark)
		joined := strings.Join(rows, "\n")
		if strings.Contains(joined, releaseNotesListMark) {
			t.Fatalf("the list mark U+E000 reached the screen:\n%s", joined)
		}
		// Every list row of the fixture — everything above the code block,
		// whose "1. " the generic checker would take for an item.
		code := slices.IndexFunc(rows, func(r string) bool { return strings.Contains(r, "code that looks like") })
		if code < 0 {
			t.Fatalf("the fixture's code line was not rendered:\n%s", joined)
		}
		if checked := checkHangingIndent(t, rows[:code], 40); checked < 8 {
			t.Errorf("dark=%v: only %d continuation rows checked — every long item in the fixture wraps twice", dark, checked)
		}
		// One row per shape, spelled out, so a shape the generic checker
		// misread cannot pass unseen.
		for _, want := range []string{
			"     enough to wrap over several rows",  // "  1. " — five columns
			"      enough to wrap over several rows", // "    • " (nested) and "  [✓] " — six
			"      to wrap over several rows at",     // "  [ ] " — six
		} {
			if !slices.Contains(rows, want) {
				t.Errorf("dark=%v: no continuation row reads %q:\n%s", dark, want, joined)
			}
		}
		// The code line continues at the document margin, exactly where
		// glamour's own wrap left it — never under its "1. ".
		if next := rows[code+1]; !strings.HasPrefix(next, "  ") || strings.HasPrefix(next, "   ") {
			t.Errorf("dark=%v: a code line beginning \"1. \" was hung like a list item:\n%q\n%q", dark, rows[code], next)
		}
	}
}

// TestReleaseNotesOtherBlocksWrapAsGlamourDid: headings, paragraphs and code
// blocks come out row for row as glamour's own wrap produced them, at every
// width the item names and a narrow one — continuation at the block's margin,
// breaks at the same spaces and hyphens, the same right edge. Compared with
// trailing spaces trimmed: glamour padded every row to the width, which the
// viewport never showed.
//
// Mutants: take a non-list line's hang from its VISIBLE leading spaces (the
// code line's styled indent included) — the code row moves two columns in;
// wrap to `width` instead of `width - margin` — every long row breaks later;
// break at glamour's list-item punctuation (" ,.;-+|") — the paragraph splits
// "v2." from "8.9".
func TestReleaseNotesOtherBlocksWrapAsGlamourDid(t *testing.T) {
	const notes = "## A heading that is long enough to need wrapping at forty columns wide\n\n" +
		"A paragraph that is long enough to wrap at forty columns, yes it is, and then some " +
		"more words, punctuated; oddly-hyphenated, v2.8.9 and so on.\n\n" +
		"```\ncode block line that is quite long indeed and will exceed forty columns\n```\n"
	trimmed := func(rows []string) string {
		for i := range rows {
			rows[i] = strings.TrimRight(rows[i], " ")
		}
		return strings.Join(rows, "\n")
	}
	for _, dark := range []bool{true, false} {
		style := "light"
		if dark {
			style = "dark"
		}
		for _, width := range []int{23, 40, 60, 100} {
			r, err := glamour.NewTermRenderer(glamour.WithStandardStyle(style), glamour.WithWordWrap(width))
			if err != nil {
				t.Fatal(err)
			}
			before, err := r.Render(notes)
			if err != nil {
				t.Fatal(err)
			}
			want := trimmed(strings.Split(ansi.Strip(before), "\n"))
			got := trimmed(renderedRows(t, notes, width, dark))
			if got != want {
				t.Errorf("%s, width %d: not wrapped as glamour wrapped it\n--- got\n%s\n--- glamour\n%s", style, width, got, want)
			}
		}
	}
}

// TestReleaseNotesNarrowestBodyNeverSpills: a box narrower than a bullet's
// hang has no room for text beside it, so the hang is dropped rather than
// leaving a non-positive width to wrap to (which wraps nothing at all).
//
// Mutant: delete `if hang >= limit { hang = 0 }` — the item comes back as one
// unwrapped row.
func TestReleaseNotesNarrowestBodyNeverSpills(t *testing.T) {
	for width := 3; width <= 8; width++ {
		for i, row := range renderedRows(t, "- A bullet with words in it\n", width, true) {
			if w := ansi.StringWidth(row); w > width {
				t.Errorf("width %d, row %d is %d columns: %q", width, i, w, row)
			}
		}
	}
}
