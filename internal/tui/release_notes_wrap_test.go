package tui

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"charm.land/glamour/v2"
	gansi "charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
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
// nested bullet past its own bullet, a task past its checkbox, a bullet whose
// Markdown source is hard-wrapped onto a second line hangs that line too —
// and a CODE line that begins "1. " gets no hang at all, because only
// glamour's list prefixes carry the mark. The mark itself never reaches the
// screen.
//
// Mutants: drop the mark from Enumeration, or from Task — that row's
// continuation lands at the margin; replace the mark-detecting branch with a
// "• or N. " pattern over the text — the code line hangs; drop the list arm's
// releaseNotesUnmark.Replace — U+E000 is on screen; drop
// wrapReleaseNotesLines's `default:` arm (the pre-fix rule) — the hard-wrapped
// bullet's second source line and its rows start flush with the bullet.
func TestReleaseNotesHangCoversEveryListShape(t *testing.T) {
	const notes = "## Shapes\n\n" +
		"1. A numbered item that is long enough to wrap over several rows at forty columns.\n" +
		"2. Short.\n\n" +
		"- A bullet whose Markdown source is hard-wrapped\n  onto a second line that is long enough to wrap at forty.\n\n" +
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
		if checked := checkHangingIndent(t, rows[:code], 40); checked < 11 {
			t.Errorf("dark=%v: only %d continuation rows checked — every long item in the fixture wraps twice, and the hard-wrapped bullet adds three", dark, checked)
		}
		// One row per shape, spelled out, so a shape the generic checker
		// misread cannot pass unseen.
		for _, want := range []string{
			"     enough to wrap over several rows",  // "  1. " — five columns
			"      enough to wrap over several rows", // "    • " (nested) and "  [✓] " — six
			"      to wrap over several rows at",     // "  [ ] " — six
			"    onto a second line that is long",    // "  • ", its second source line — four
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

// TestReleaseNotesItemSourceLinesHangUnderTheText: glamour keeps a list
// item's source line breaks (a paragraph's it turns into spaces) and starts
// each later line at the item's BULLET column, unmarked. Every one of those
// lines belongs under the item's text — the item's own, a nested item's, and
// the outer item's again once the nested list is over, which glamour gives no
// blank line. What is not the item's text keeps the margin rule: a code line
// inside the item continues at the document margin as glamour's wrap left
// it, and the paragraph after the list is not part of any item at all.
//
// Mutants: drop the `default:` arm's re-indent (keep only the hang) — each
// second source line's first row stays at the bullet column; drop owner's pop
// loop — the outer item's paragraph after the nested list stays at the
// bullet column; `> indent` → `>= indent` in it — every second source line
// closes its own item; `== indent` → `<= indent` in owner — the code line
// hangs under the item's text; drop `open = open[:0]` — the paragraph after
// the list is hung like the item before it.
func TestReleaseNotesItemSourceLinesHangUnderTheText(t *testing.T) {
	const notes = "- An outer item whose source\n  runs onto a second line, long enough to wrap.\n" +
		"  - A nested item whose source\n    runs on as well, and long enough to wrap again.\n\n" +
		"  The outer item's next paragraph, long enough to wrap.\n\n" +
		"  ```\n  a code line in the item, long enough to wrap at forty\n  ```\n\n" +
		"A paragraph after the list, long enough to wrap at forty.\n"
	want := []string{
		"",
		"  ",
		"  • An outer item whose source",
		"    runs onto a second line, long",
		"    enough to wrap.",
		"    • A nested item whose source",
		"      runs on as well, and long enough",
		"      to wrap again.",
		"    The outer item's next paragraph,",
		"    long enough to wrap.",
		"    a code line in the item, long",
		"  enough to wrap at forty",
		"  ",
		"  ",
		"  A paragraph after the list, long",
		"  enough to wrap at forty.",
		"",
		"",
	}
	for _, dark := range []bool{true, false} {
		if got := renderedRows(t, notes, 40, dark); !slices.Equal(got, want) {
			t.Errorf("dark=%v:\n--- got\n%s\n--- want\n%s", dark, strings.Join(got, "\n"), strings.Join(want, "\n"))
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
// leaving a non-positive width to wrap to (which wraps nothing at all). A
// second source line, moved under the item's text, is held to the same edge.
//
// A blockquote's bar is held to it too — on its own and moved under a list
// item's text — and a quote that no longer fits beside its bar wraps without
// it, both marks gone from the screen.
//
// Mutants: delete `if hang >= limit { hang = 0 }` — the item comes back as one
// unwrapped row; drop `open[n-1].text < limit` from owner — at three columns
// the second source line's first row is the four spaces it was given and a
// letter; delete wrapReleaseNotesQuote's `width >= limit` fallback — the
// quote's lines wrap to a non-positive width, which wraps nothing; drop the
// quote mark from releaseNotesUnmark — that fallback puts U+E001 on screen.
func TestReleaseNotesNarrowestBodyNeverSpills(t *testing.T) {
	for _, notes := range []string{
		"- A bullet with words in it\n  and a second source line\n",
		"> A quote with words in it\n",
		"- A bullet\n\n  > and a quote in it\n",
	} {
		for width := 3; width <= 8; width++ {
			rows := renderedRows(t, notes, width, true)
			for i, row := range rows {
				if w := ansi.StringWidth(row); w > width {
					t.Errorf("%q at width %d, row %d is %d columns: %q", notes, width, i, w, row)
				}
			}
			if joined := strings.Join(rows, "\n"); strings.ContainsAny(joined, releaseNotesListMark+releaseNotesQuoteMark) {
				t.Errorf("%q at width %d: a mark reached the screen:\n%q", notes, width, joined)
			}
		}
	}
}

// TestReleaseNotesBlockquoteKeepsItsBarOnEveryRow: a long blockquote, rendered
// at 40, 60 and 100 columns in both themes, carries its bar on EVERY row.
// Glamour's own wrap repeated "  │ " down the whole quote; the re-wrap that
// replaced it kept the bar on each source line's first row only, so a quoted
// paragraph's later rows read as the paragraph after the quote. No row is
// wider than the width — nor past the right margin glamour kept, which is
// stricter — no word of the quote is lost, the paragraph after it carries no
// bar, and neither mark reaches the screen.
//
// Mutants: drop `bar += releaseNotesQuoteMark` from releaseNotesStyle — the
// rows after each source line's first lose the bar; wrap the quote's lines to
// `limit` instead of `limit-width` in wrapReleaseNotesQuote — rows run past
// the edge by the bar; prefix the rows with spaces as wide as the bar instead
// of the bar — no row carries it.
func TestReleaseNotesBlockquoteKeepsItsBarOnEveryRow(t *testing.T) {
	const p1 = "A blockquote in the release notes that runs long enough to wrap at every width " +
		"the overlay is drawn at, one hundred columns included, so that each of its rows can be " +
		"checked for the bar that marks it as quoted text rather than the paragraph after it."
	const p2 = "Its second paragraph, after a quoted blank line, wraps the same way and keeps the " +
		"same bar on every row it takes, the last row of the quote included."
	const after = "A paragraph after the quote."
	notes := "## Quoted\n\nA paragraph before the quote.\n\n> " + p1 + "\n>\n> " + p2 + "\n\n" + after + "\n"
	for _, dark := range []bool{true, false} {
		for _, width := range []int{40, 60, 100} {
			rows := renderedRows(t, notes, width, dark)
			joined := strings.Join(rows, "\n")
			if strings.ContainsAny(joined, releaseNotesListMark+releaseNotesQuoteMark) {
				t.Fatalf("dark=%v, width %d: a mark reached the screen:\n%s", dark, width, joined)
			}
			first, last := -1, -1
			for i, row := range rows {
				if strings.Contains(row, "│") {
					if first < 0 {
						first = i
					}
					last = i
				}
				if w := ansi.StringWidth(row); w > width-2 {
					t.Errorf("dark=%v, width %d, row %d is %d columns — past the right margin glamour kept (and %d past the width):\n%q",
						dark, width, i, w, w-width, row)
				}
				if strings.Contains(row, after) && strings.HasPrefix(row, "  │") {
					t.Errorf("dark=%v, width %d: the paragraph after the quote carries the bar:\n%q", dark, width, row)
				}
			}
			if first < 0 {
				t.Fatalf("dark=%v, width %d: no row carries the bar at all:\n%s", dark, width, joined)
			}
			quote := rows[first : last+1]
			// Three source lines: two paragraphs and the quoted blank between.
			if len(quote) <= 3 {
				t.Errorf("dark=%v, width %d: the quote took %d rows — it did not wrap, so nothing here was checked:\n%s",
					dark, width, len(quote), strings.Join(quote, "\n"))
			}
			var text []string
			for i, row := range quote {
				if !strings.HasPrefix(row, "  │") {
					t.Errorf("dark=%v, width %d, quote row %d has no bar — glamour's own wrap put \"  │ \" on every row:\n%s",
						dark, width, i, strings.Join(quote, "\n"))
					break
				}
				text = append(text, strings.TrimPrefix(row, "  │"))
			}
			if got, want := strings.Fields(strings.Join(text, " ")), strings.Fields(p1+" "+p2); !slices.Equal(got, want) {
				t.Errorf("dark=%v, width %d: the quote's words changed:\n got %q\nwant %q", dark, width, got, want)
			}
		}
	}
}

// TestReleaseNotesQuoteShapes pins what a quote holds and what holds a quote,
// row for row at forty columns: a list in a quote hangs under its text with
// the bar in front — its second source line and a nested item included — a
// nested quote repeats both bars, and a quote in a list item is moved under
// the item's text, as the item's later source lines are, bar and all, down to
// the item's text after it.
//
// Mutants: end the quote's run at its first line (`end := j + 1` with no
// loop) — each line is a quote of its own, so its second source line no
// longer hangs under the item it belongs to; leave shift at 0 (no owner call
// in the quote arm) — the quote in the item stays at the item's bullet column.
func TestReleaseNotesQuoteShapes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		notes string
		want  []string
	}{
		{
			"a list in a quote",
			"> - A list item in a quote whose source\n>   runs onto a second line, long enough to wrap.\n" +
				">   - A nested item in the quote, long enough to wrap at forty.\n",
			[]string{
				"",
				"  ",
				"  │ ",
				"  │ • A list item in a quote whose",
				"  │   source",
				"  │   runs onto a second line, long",
				"  │   enough to wrap.",
				"  │   • A nested item in the quote,",
				"  │     long enough to wrap at forty.",
				"  │ ",
				"",
				"",
			},
		},
		{
			"a quote in a list item",
			"1. A numbered item\n\n   > A quote in the item, long enough to wrap over several rows.\n   >\n" +
				"   > Its second paragraph, long enough to wrap as well.\n\n" +
				"   The item's text after the quote, long enough to wrap.\n",
			[]string{
				"",
				"  ",
				"  1. A numbered item",
				"     │ A quote in the item, long",
				"     │ enough to wrap over several",
				"     │ rows.",
				"     │ ",
				"     │ Its second paragraph, long",
				"     │ enough to wrap as well.",
				"     The item's text after the quote,",
				"     long enough to wrap.",
				"",
				"",
			},
		},
		{
			"a quote in a quote",
			"> An outer quote, long enough to wrap at forty columns.\n>\n" +
				"> > An inner quote, long enough to wrap at forty columns too.\n",
			[]string{
				"",
				"  ",
				"  │ An outer quote, long enough to",
				"  │ wrap at forty columns.",
				"  │ ",
				"  │ │ An inner quote, long enough to",
				"  │ │ wrap at forty columns too.",
				"",
				"",
			},
		},
	} {
		for _, dark := range []bool{true, false} {
			if got := renderedRows(t, tc.notes, 40, dark); !slices.Equal(got, tc.want) {
				t.Errorf("%s, dark=%v:\n--- got\n%s\n--- want\n%s", tc.name, dark, strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		}
	}
}

// TestReleaseNotesALinesFirstMarkDecidesItIsQuoted: a line that carries both
// marks is a quote line when the bar's comes first — a list item behind a
// bar. Glamour opens every quoted list with a blank quoted line, so such a
// line is always inside a run some other line opened; this one opens it, and
// it must still keep the bar on every row and hang under the item's text.
//
// Mutant: `q < i` dropped from the quote arm's test (`q >= 0 && i < 0`) — the
// line is wrapped as a list item, and its later rows lose the bar.
func TestReleaseNotesALinesFirstMarkDecidesItIsQuoted(t *testing.T) {
	line := "  │ " + releaseNotesQuoteMark + "• " + releaseNotesListMark + "an item behind a bar, long enough to wrap twice over"
	want := []string{
		"  │ • an item behind a",
		"  │   bar, long enough to",
		"  │   wrap twice over",
	}
	if got := strings.Split(ansi.Strip(wrapReleaseNotes(line, 26)), "\n"); !slices.Equal(got, want) {
		t.Errorf("\n--- got\n%s\n--- want\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestReleaseNotesStyleLeavesGlamoursBarAlone: the quote's bar is a *string
// the style copy shares with glamour's package-level configs, so marking it
// must replace the pointer, never write through it — or every later render,
// this overlay's included, would find the bar already marked and mark it
// again.
//
// Mutant: `*cfg.BlockQuote.IndentToken = bar` in place of
// `cfg.BlockQuote.IndentToken = &bar` — the package-level bar carries the mark.
func TestReleaseNotesStyleLeavesGlamoursBarAlone(t *testing.T) {
	for _, dark := range []bool{true, false} {
		marked := releaseNotesStyle(dark)
		if tok := marked.BlockQuote.IndentToken; tok == nil || !strings.HasSuffix(*tok, releaseNotesQuoteMark) {
			t.Fatalf("dark=%v: the overlay's bar is not marked: %v", dark, tok)
		}
	}
	for name, cfg := range map[string]gansi.StyleConfig{"dark": styles.DarkStyleConfig, "light": styles.LightStyleConfig} {
		if tok := cfg.BlockQuote.IndentToken; tok != nil && strings.Contains(*tok, releaseNotesQuoteMark) {
			t.Errorf("glamour's %s style's bar was written through: %q", name, *tok)
		}
	}
}

// TestReleaseNotesQuoteBarClosesItsStyle: the bar's span is still open where
// the quote mark sits, so every row closes it before its own text — a row
// that sets no style of its own must not come out in the bar's. The standard
// styles colour the bar like the text and so cannot show it; a bar in red
// over plain text can.
//
// Mutant: drop the ansi.ResetStyle between the bar and each row in
// wrapReleaseNotesQuote — the continuation rows open in red.
func TestReleaseNotesQuoteBarClosesItsStyle(t *testing.T) {
	const red = "\x1b[31m"
	line := "  " + red + "│ " + releaseNotesQuoteMark + ansi.ResetStyle + "plain words long enough to wrap at twenty columns twice over"
	rows := strings.Split(wrapReleaseNotes(line, 20), "\n")
	if len(rows) < 3 {
		t.Fatalf("the fixture did not wrap at twenty columns: %q", rows)
	}
	for i, row := range rows {
		if !strings.HasPrefix(row, "  "+red+"│ "+ansi.ResetStyle) {
			t.Errorf("row %d does not close the bar's style before its text: %q", i, row)
		}
	}
}
