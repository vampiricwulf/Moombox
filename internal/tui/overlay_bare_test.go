package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// bareTableSizes are the sizes every overlay is drawn at: the compact floor,
// a middling compact size, the largest compact size, a wide-short and a
// narrow-tall terminal (both compact), and the smallest normal one.
var bareTableSizes = [][2]int{{30, 6}, {40, 10}, {59, 19}, {200, 8}, {30, 50}, {60, 20}}

// wantExact fails unless frame is exactly h rows of exactly w cells.
func wantExact(t *testing.T, what, frame string, w, h int) {
	t.Helper()
	lines := strings.Split(frame, "\n")
	if len(lines) != h {
		t.Errorf("%s: %d rows, want %d", what, len(lines), h)
	}
	for i, line := range lines {
		if got := ansi.StringWidth(line); got != w {
			t.Errorf("%s: row %d is %d cells, want %d: %q", what, i, got, w, ansi.Strip(line))
		}
	}
}

// Every overlay at every size is drawn the way its census row says: its own
// view at 60x20, its bare view (already exactly the terminal's size) where
// it has one that fits, and otherwise the notice naming the size it needs.
//
// Mutant: dropping the bare field of an entry — its row here expects a bare
// frame and gets the notice. Mutant: a bare view one row short — wantExact.
func TestEveryOverlayIsDrawnForItsSize(t *testing.T) {
	for i, c := range overlayCases(t) {
		for _, size := range bareTableSizes {
			w, h := size[0], size[1]
			name := fmt.Sprintf("%s at %dx%d", c.name, w, h)
			a := overlayTestApp(t)
			c.open(a)
			a.Update(tea.WindowSizeMsg{Width: w, Height: h})
			o := a.activeOverlay()
			if o != &overlayRegistry[i] {
				t.Fatalf("%s: premise lost: it is not the overlay on top", name)
			}
			frame := a.View().Content

			want := drawHeld
			switch {
			case w >= minTermWidth && h >= minTermHeight:
				want = drawNormal
			case c.bareW > 0 && w >= c.bareW && h >= c.bareH:
				want = drawBare
			}
			if got := a.overlayDrawFor(o); got != want {
				t.Errorf("%s: draw mode %d, want %d", name, got, want)
				continue
			}
			if a.screenHeld() != (want == drawHeld) {
				t.Errorf("%s: screenHeld = %v", name, a.screenHeld())
			}
			switch want {
			case drawNormal:
				if frame != c.view(a) {
					t.Errorf("%s: the screen is not the overlay's own view", name)
				}
			case drawBare:
				// The overlay's own view, with nothing left for
				// App.View's fitBlock to cut or pad.
				if own := c.view(a); frame != own {
					t.Errorf("%s: App.View changed the bare view:\n%s\nown:\n%s", name, frame, own)
				}
				wantExact(t, name, frame, w, h)
			case drawHeld:
				needW, needH := 60, 20
				if c.bareW > 0 {
					needW, needH = c.bareW, c.bareH
				}
				plain := plainFrame(a)
				if !strings.HasPrefix(plain, heldOverlayNotice(needW, needH)+"\n") {
					t.Errorf("%s: the notice does not name %dx%d:\n%s", name, needW, needH, plain)
				}
				for row, line := range frameLines(a) {
					if lw := ansi.StringWidth(line); lw > w || row >= h {
						t.Errorf("%s: the notice does not fit (row %d, %d cells)", name, row, lw)
					}
				}
			}
		}
	}
}

// Shrinking an open overlay to its smallest drawing (or under the notice),
// through a wide-short and a narrow-tall terminal, and growing back to 60x20
// leaves the frame a never-shrunk overlay draws: nothing the bare form sets
// on a shared component outlives it.
//
// Mutant: a bare SetSize that leaves a viewport at the bare width — the
// frame after growing back differs.
func TestOverlayFramesSurviveAShrinkAndBack(t *testing.T) {
	for _, c := range overlayCases(t) {
		open := func() *App {
			a := overlayTestApp(t)
			a.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
			c.open(a)
			// Each overlay is sized by its opener or by the next resize; a
			// resize to the same size settles both apps the same way.
			a.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
			return a
		}
		never := open()
		want := never.View().Content
		if want != c.view(never) {
			t.Fatalf("%s: premise lost: 60x20 is not the overlay's own view", c.name)
		}

		a := open()
		small := tea.WindowSizeMsg{Width: compactMinWidth, Height: compactMinHeight}
		if c.bareW > 0 {
			small = tea.WindowSizeMsg{Width: c.bareW, Height: c.bareH}
		}
		for _, size := range []tea.WindowSizeMsg{small, {Width: 200, Height: 8}, {Width: 30, Height: 50}, small} {
			a.Update(size)
			_ = a.View()
		}
		a.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
		if got := a.View().Content; got != want {
			t.Errorf("%s: the 60x20 frame changed across a shrink:\n%s\nnever shrunk:\n%s", c.name, ansi.Strip(got), ansi.Strip(want))
		}
	}
}

// A bare view is exactly the terminal's size at every compact size, not
// only the table's: every width and height the compact layout has for the
// two readers.
func TestBareReadersFillEveryCompactSize(t *testing.T) {
	notes := "## Features\n\n- Compact layout for terminals from 30x6 up to 59x19\n- Overlays draw a bare form\n\n> a quote that is long enough to wrap in a narrow terminal\n\n```\ncode --flag value\n```\n"
	for w := compactMinWidth; w <= 210; w += 7 {
		for h := compactMinHeight; h <= 52; h += 3 {
			if layoutModeFor(w, h) != layoutCompact {
				continue
			}
			help := NewHelpModel()
			help.SetMenuItems(newCompactRig(t, w, h).app.buildMenuItems())
			help.SetSize(w, h)
			help.Toggle()
			wantExact(t, fmt.Sprintf("help at %dx%d", w, h), help.View(), w, h)

			for _, pending := range []bool{false, true} {
				o := newReleaseNotesOverlay()
				o.open("v2.10.12-rc.1", notes, w, h)
				o.setPending(pending)
				wantExact(t, fmt.Sprintf("release notes at %dx%d pending=%v", w, h, pending), o.View(), w, h)
				o.withdraw("No longer pending: v2.10.12-rc.1 was skipped")
				wantExact(t, fmt.Sprintf("withdrawn release notes at %dx%d", w, h), o.View(), w, h)
			}
		}
	}
}

// Keys, the wheel and a paste reach an overlay drawn bare, and none of them
// reaches the task list it covers.
//
// Mutant: handleMouse asking the compact layout before the overlay — the
// wheel moves the list's cursor. Mutant: the handleKey size guard holding
// every overlay below the normal layout — Down does not scroll the help.
func TestABareOverlayTakesTheInput(t *testing.T) {
	r := newCompactRig(t, 40, 10)
	a := r.app
	r.selectJob(t, "err")
	cursor := a.taskList.list.Index()
	untouched := func(step string) {
		t.Helper()
		if a.taskList.list.Index() != cursor || a.taskList.SelectedCount() != 0 || a.taskList.IsSearching() || a.taskList.Query() != "" {
			t.Errorf("%s reached the task list under the overlay", step)
		}
		if len(r.deleted)+len(r.cancelled)+len(r.reinit)+r.restarted != 0 {
			t.Errorf("%s ran a chord under the overlay", step)
		}
	}

	r.press("?")
	if !a.help.IsVisible() || a.screenHeld() {
		t.Fatalf("? did not open the help bare at 40x10: feedback %q", a.feedback.msg)
	}
	if got := a.help.viewport.YOffset(); got != 0 {
		t.Fatalf("premise lost: the help opened at row %d", got)
	}
	r.press("down")
	if got := a.help.viewport.YOffset(); got != 1 {
		t.Errorf("Down moved the help to row %d, want 1", got)
	}
	untouched("Down")
	a.Update(tea.MouseWheelMsg{X: 5, Y: 3, Button: tea.MouseWheelDown})
	if got := a.help.viewport.YOffset(); got != 1+overlayWheelRows {
		t.Errorf("a wheel notch moved the help to row %d, want %d", got, 1+overlayWheelRows)
	}
	untouched("the wheel")
	a.Update(tea.MouseClickMsg{X: 5, Y: 3, Button: tea.MouseLeft})
	untouched("a click")
	// Keys that act on the list or arm a chord when no overlay is open.
	r.press("space", "/", "z", "a", "d", "d", "a", "x", "x", "q", "q", "m", "f")
	untouched("a typed chord")
	if a.chord.prefix != "" {
		t.Errorf("a key armed the chord %q under the overlay", a.chord.prefix)
	}
	a.Update(tea.PasteMsg{Content: "pasted"})
	untouched("a paste")
	if !a.help.IsVisible() {
		t.Fatal("a key other than ? or Esc closed the help")
	}
	if strings.Contains(plainFrame(a), "pasted") {
		t.Error("the paste is on screen")
	}
	r.press("esc")
	if a.hasActiveOverlay() {
		t.Fatal("Esc did not close the bare help")
	}
	if got := plainFrame(a); !strings.Contains(got, "ERR  A broken stream") {
		t.Errorf("the compact list is not back:\n%s", got)
	}
	untouched("Esc")

	// The release notes: the wheel and the arrows scroll them, S skips.
	var skipped []string
	a.OnDismissUpdate = func(tag string) error { skipped = append(skipped, tag); return nil }
	a.updateAvailable.ReleaseNotes = strings.Repeat("- one more line of the notes\n", 40)
	r.press("r", "n")
	o := a.releaseNotesPopup
	if !o.isOpen() || a.screenHeld() {
		t.Fatalf("R N did not open the notes bare at 40x10: feedback %q", a.feedback.msg)
	}
	a.Update(tea.MouseWheelMsg{X: 5, Y: 3, Button: tea.MouseWheelDown})
	r.press("down")
	if got := o.vp.YOffset(); got != overlayWheelRows+1 {
		t.Errorf("the wheel and Down moved the notes to row %d, want %d", got, overlayWheelRows+1)
	}
	untouched("scrolling the notes")
	r.pressRunLast("s")
	if len(skipped) != 1 || skipped[0] != "v9.9.9" {
		t.Errorf("S skipped %v, want [v9.9.9]", skipped)
	}
	r.press("esc")
	if a.hasActiveOverlay() {
		t.Error("Esc did not close the bare notes")
	}
}

// An overlay opened bare and grown to 60x20 is the overlay a 60x20 terminal
// opens; one opened at 60x20 and shrunk is the bare one.
func TestABareOverlayFollowsTheSize(t *testing.T) {
	r := newCompactRig(t, 30, 6)
	a := r.app
	r.press("?")
	wantExact(t, "help opened at 30x6", a.View().Content, 30, 6)
	a.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	grown := a.View().Content

	n := newCompactRig(t, 60, 20)
	n.press("?")
	if want := n.app.View().Content; grown != want {
		t.Errorf("a help opened bare and grown is not the help opened at 60x20:\n%s\nwant:\n%s", ansi.Strip(grown), ansi.Strip(want))
	}
	n.app.Update(tea.WindowSizeMsg{Width: 30, Height: 6})
	a.Update(tea.WindowSizeMsg{Width: 30, Height: 6})
	if got, want := n.app.View().Content, a.View().Content; got != want {
		t.Errorf("a help shrunk to 30x6 is not the help opened there:\n%s\nwant:\n%s", ansi.Strip(got), ansi.Strip(want))
	}
}

// The refusal names the size the refused overlay needs (owner ruling R-C5
// as amended), through every route: the predicate, a chord, the M key and a
// prefix whose chords are all refused.
func TestTheRefusalNamesTheOverlaysSize(t *testing.T) {
	saved := overlayRegistry
	t.Cleanup(func() { overlayRegistry = saved })
	overlayRegistry = append([]overlayEntry(nil), saved...)
	for i := range overlayRegistry {
		switch overlayRegistry[i].name {
		case "stats":
			overlayRegistry[i].bare = &overlayBare{openW: 30, openH: 10}
		case "action menu":
			overlayRegistry[i].bare = &overlayBare{openW: 44, openH: 6}
		case "ytdlp plugin", "cookie import":
			// As an overlay with no bare form, so that every E chord but
			// E T is refused at any compact size.
			overlayRegistry[i].bare = nil
		}
	}

	for _, tt := range []struct {
		name       string
		w, h       int
		ok         bool
		wantW, wnH int
	}{
		{"stats", 30, 9, false, 30, 10},
		{"stats", 30, 10, true, 30, 10},
		{"stats", 200, 8, false, 30, 10},
		{"stats", 60, 20, true, 30, 10},
		{"stats", 29, 10, false, 30, 10}, // below the compact floor nothing opens
		{"setup wizard", 59, 19, false, 60, 20},
		{"setup wizard", 200, 19, false, 60, 20},
		{"setup wizard", 60, 20, true, 60, 20},
		{"settings", 30, 6, true, 30, 6},
		{"settings", 200, 6, true, 30, 6},
		{"help", 30, 6, true, 30, 6},
		{"no such overlay", 40, 10, false, 60, 20},
	} {
		ok, w, h := overlayOpensAt(tt.name, tt.w, tt.h)
		if ok != tt.ok || w != tt.wantW || h != tt.wnH {
			t.Errorf("overlayOpensAt(%q, %d, %d) = %v, %dx%d; want %v, %dx%d", tt.name, tt.w, tt.h, ok, w, h, tt.ok, tt.wantW, tt.wnH)
		}
	}

	r := newCompactRig(t, 40, 9)
	r.press("e", "t")
	if got, want := r.app.feedback.msg, "Enlarge the terminal to 30x10 for this"; got != want || r.app.hasActiveOverlay() {
		t.Errorf("E T at 40x9: feedback %q, want %q", got, want)
	}
	r.press("m")
	if got, want := r.app.feedback.msg, "Enlarge the terminal to 44x6 for this"; got != want || r.app.hasActiveOverlay() {
		t.Errorf("M at 40x9: feedback %q, want %q", got, want)
	}
	// E with every chord refused: the hint names the largest size among
	// them, the one that makes them all work.
	r.press("e")
	if got, want := r.app.feedback.msg, "Enlarge the terminal to 60x20 for this"; got != want {
		t.Errorf("E at 40x9: feedback %q, want %q", got, want)
	}
	// With room for the statistics, E offers that chord alone.
	r = newCompactRig(t, 40, 10)
	r.press("e")
	if got := r.app.feedback.msg; got != "Extras: T Stats (3s)" {
		t.Errorf("E at 40x10: feedback %q", got)
	}
}

// An overlay whose current state needs more room than its first screen is
// held in that state only, and the notice names the state's size.
func TestAnOverlayIsHeldBelowItsCurrentStatesSize(t *testing.T) {
	saved := overlayRegistry
	t.Cleanup(func() { overlayRegistry = saved })
	overlayRegistry = append([]overlayEntry(nil), saved...)
	deep := false
	for i := range overlayRegistry {
		if overlayRegistry[i].name == "help" {
			overlayRegistry[i].bare = &overlayBare{openW: 30, openH: 6, now: func(*App) (int, int) {
				if deep {
					return 30, 10
				}
				return 30, 6
			}}
		}
	}
	r := newCompactRig(t, 30, 8)
	a := r.app
	r.press("?")
	if !a.help.IsVisible() || a.screenHeld() {
		t.Fatal("premise lost: the help did not open bare at 30x8")
	}
	deep = true
	if !a.screenHeld() || !strings.HasPrefix(plainFrame(a), "Enlarge to 30x10 to continue\n") {
		t.Errorf("the deeper state is not held under a notice naming 30x10:\n%s", plainFrame(a))
	}
	r.press("down")
	if a.help.viewport.YOffset() != 0 {
		t.Error("a key reached the overlay under the notice")
	}
	a.Update(tea.WindowSizeMsg{Width: 30, Height: 10})
	if a.screenHeld() {
		t.Error("the deeper state is still held at the size it needs")
	}
	wantExact(t, "the deeper state at 30x10", a.View().Content, 30, 10)
}

func TestBareSheet(t *testing.T) {
	body := func(n int) []string {
		rows := make([]string, n)
		for i := range rows {
			rows[i] = fmt.Sprintf("row %d", i)
		}
		return rows
	}
	plain := func(s string) []string {
		lines := strings.Split(ansi.Strip(s), "\n")
		for i := range lines {
			lines[i] = strings.TrimRight(lines[i], " ")
		}
		return lines
	}

	// Exactly w x h, whatever it is given.
	for _, size := range [][2]int{{30, 6}, {200, 8}, {30, 50}, {1, 1}, {5, 2}} {
		s := bareSheet{left: "A long title that will not fit", right: "Esc close", body: body(60), keys: "Enter save · Esc back",
			msg: strings.Repeat("a message with many words ", 30), keep: 2}
		wantExact(t, fmt.Sprintf("sheet at %v", size), s.render(size[0], size[1]), size[0], size[1])
	}

	// The header keeps its right half whole and cuts the left.
	got := plain(bareSheet{left: "Release Notes v2.10.12-rc.1", right: "U update·S skip·Esc"}.render(30, 6))
	if got[0] != "Release N… U update·S skip·Esc" {
		t.Errorf("header = %q", got[0])
	}

	// No message: header, body, keys on the last row.
	got = plain(bareSheet{left: "T", body: body(9), keys: "keys"}.render(30, 6))
	if want := []string{"T", "row 0", "row 1", "row 2", "row 3", "keys"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("no message: %q", got)
	}
	// No keys row: the body runs to the last row.
	got = plain(bareSheet{left: "T", body: body(9)}.render(30, 6))
	if got[5] != "row 4" {
		t.Errorf("no keys: %q", got)
	}

	// A message takes the keys row first, then climbs over the body from
	// the bottom, and never covers the kept rows.
	got = plain(bareSheet{left: "T", body: body(9), keys: "keys", msg: "short", keep: 1}.render(30, 6))
	if want := []string{"T", "row 0", "row 1", "row 2", "row 3", "short"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("a one-row message: %q", got)
	}
	long := "one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen"
	got = plain(bareSheet{left: "T", body: body(9), keys: "keys", msg: long, keep: 1}.render(30, 6))
	if got[0] != "T" || got[1] != "row 0" || got[2] != "one two three four five six" || got[5] != "fifteen sixteen" {
		t.Errorf("a four-row message: %q", got)
	}
	if words := strings.Fields(strings.Join(got[2:], " ")); strings.Join(words, " ") != long {
		t.Errorf("the message lost words: %q", got)
	}
	// Longer still: the header goes too, the kept row moves up to its place
	// so the message stays in one block under it, and what does not fit
	// ends in an ellipsis.
	got = plain(bareSheet{left: "T", body: body(9), keys: "keys", msg: long + " " + long + " " + long, keep: 1}.render(30, 6))
	if got[0] != "row 0" || !strings.HasPrefix(got[1], "one two") || !strings.HasSuffix(got[5], "…") {
		t.Errorf("a message longer than the sheet: %q", got)
	}
	// The style colours the message rows only.
	styled := bareSheet{left: "T", body: body(2), msg: "bad", msgStyle: lipgloss.NewStyle().Bold(true)}.render(30, 6)
	rows := strings.Split(styled, "\n")
	if rows[5] == ansi.Strip(rows[5]) || rows[1] != ansi.Strip(rows[1]) {
		t.Errorf("message style: %q", rows)
	}
}

// Help below the normal layout lists only the keys that work at the current
// size and says the list is partial (owner ruling R-C12). The chords it
// lists are the chords the prefix hints offer there.
//
// Mutant: bareSections listing every menu item — "A A" (refused at 40x10)
// is on the list.
func TestBareHelpListsOnlyTheKeysThatWork(t *testing.T) {
	for _, size := range [][2]int{{30, 6}, {40, 10}, {59, 19}, {200, 8}, {30, 50}} {
		w, h := size[0], size[1]
		r := newCompactRig(t, w, h)
		a := r.app
		r.selectJob(t, "done")
		r.press("?")
		if !a.help.IsVisible() {
			t.Fatalf("%v: ? did not open the help", size)
		}
		a.help.viewport.SetHeight(500) // the whole list, for reading
		list := strings.Split(ansi.Strip(a.help.viewport.View()), "\n")
		for i := range list {
			list[i] = strings.TrimRight(list[i], " ")
		}
		for len(list) > 0 && list[len(list)-1] == "" {
			list = list[:len(list)-1]
		}
		if last := list[len(list)-1]; last != "More keys at 60x20" {
			t.Errorf("%v: the list ends %q", size, last)
		}
		for _, line := range list {
			if ansi.StringWidth(line) > w {
				t.Errorf("%v: a row is wider than the terminal: %q", size, line)
			}
		}
		text := "\n" + strings.Join(list, "\n") + "\n"

		for _, item := range a.buildMenuItems() {
			if _, isChord := categoryHelpTitles[item.Category]; !isChord {
				continue
			}
			chord := item.Chord
			if item.NeedsConfirm {
				chord += " " + chord[len(chord)-1:]
			}
			refused, _, _ := chordRefusedAt(item.Chord, w, h)
			if listed := strings.Contains(text, "\n"+chord+" "); listed == refused {
				t.Errorf("%v: %s listed = %v, and chordCompact says refused = %v", size, chord, listed, refused)
			}
			// And the prefix hint agrees, for the chords it would offer
			// whatever is selected.
			if !item.NeedsJob {
				hint, _ := a.chordHint(strings.ToLower(item.Chord[:1]))
				offered := false
				for _, k := range hintKeys(hint) {
					offered = offered || k == item.Chord[2:]
				}
				if offered == refused {
					t.Errorf("%v: %s: the %s hint offers it = %v, the help lists it = %v", size, chord, item.Chord[:1], offered, !refused)
				}
			}
		}
		// Nothing of the panels the compact layout does not have, and no
		// key whose overlay is refused here.
		for _, gone := range []string{"Tab", "Details", "Logs", "focus panel"} {
			if strings.Contains(text, gone) {
				t.Errorf("%v: the list still mentions %q", size, gone)
			}
		}
		// The action menu and Settings draw bare at every compact size, so M
		// and ` are listed.
		if !strings.Contains(text, "Action menu") || !strings.Contains(text, "Open settings") {
			t.Errorf("%v: the list lacks the M key:\n%s", size, text)
		}
		for _, kept := range []string{"\n?     Toggle help\n", "\nCtrl+C\n      Quit immediately\n", "\nQ Q   Quit program\n", "\nSpace Select task for batch\n      actions\n"} {
			if w == 30 && !strings.Contains(text, kept) {
				t.Errorf("%v: the list lacks %q:\n%s", size, kept, text)
			}
		}
	}
}

// A static row of the bare help follows its overlay: the Job Log's keys and
// M appear once those overlays open at the size.
func TestBareHelpStaticRowsFollowTheirOverlay(t *testing.T) {
	saved := overlayRegistry
	t.Cleanup(func() { overlayRegistry = saved })
	overlayRegistry = append([]overlayEntry(nil), saved...)
	for i := range overlayRegistry {
		if n := overlayRegistry[i].name; n == "job log" || n == "action menu" {
			overlayRegistry[i].bare = &overlayBare{openW: 30, openH: 8}
		}
	}
	read := func(w, h int) string {
		m := NewHelpModel()
		m.SetSize(w, h)
		m.Toggle()
		m.viewport.SetHeight(500)
		return ansi.Strip(m.viewport.View())
	}
	if got := read(40, 8); !strings.Contains(got, "Action menu") || !strings.Contains(got, "Find text (Job Log)") {
		t.Errorf("at 40x8 the rows of overlays that open there are missing:\n%s", got)
	}
	if got := read(40, 7); strings.Contains(got, "Action menu") || strings.Contains(got, "Job Log") {
		t.Errorf("at 40x7 the rows of overlays refused there are listed:\n%s", got)
	}
}

// The bare help at the two golden sizes.
func TestBareHelpFrames(t *testing.T) {
	frame := func(w, h int) string {
		m := NewHelpModel()
		m.SetMenuItems([]ActionMenuItem{
			{Chord: "A A", Label: "First-run Setup", Category: "Action", Opens: "setup wizard"},
			{Chord: "A X", Label: "Cancel", Category: "Action", NeedsConfirm: true},
			{Chord: "A M", Label: "Mux", Category: "Action"},
			{Chord: "R N", Label: "View Release Notes (current version)", Category: "Request", Opens: "release notes"},
		})
		m.SetSize(w, h)
		m.Toggle()
		view := m.View()
		wantExact(t, fmt.Sprintf("help at %dx%d", w, h), view, w, h)
		lines := strings.Split(ansi.Strip(view), "\n")
		for i := range lines {
			lines[i] = strings.TrimRight(lines[i], " ")
		}
		return strings.Join(lines, "\n")
	}
	want40 := strings.Join([]string{
		"Help 0%                      ?/Esc close",
		"Action (A)",
		"A X X Cancel",
		"A M   Mux",
		"Request (R)",
		"R N   View Release Notes (current",
		"      version)",
		"Quick Keys",
		"F     Cycle status filter",
		"M     Action menu",
	}, "\n")
	if got := frame(40, 10); got != want40 {
		t.Errorf("40x10:\n%s\nwant:\n%s", got, want40)
	}
	want30 := strings.Join([]string{
		"Help 0%            ?/Esc close",
		"Action (A)",
		"A X X Cancel",
		"A M   Mux",
		"Request (R)",
		"R N   View Release Notes",
	}, "\n")
	if got := frame(30, 6); got != want30 {
		t.Errorf("30x6:\n%s\nwant:\n%s", got, want30)
	}
}

// The bare release notes: the tag and the keys on the first row, the notes
// from the second with no margin and no leading blank row.
func TestBareReleaseNotesFrames(t *testing.T) {
	notes := "## Features\n\n- Compact layout for terminals from 30x6 up to 59x19\n- Overlays draw a bare form in the compact layout\n\n## Bug Fixes\n\n- One\n"
	frame := func(o *releaseNotesOverlay, w, h int) []string {
		view := o.View()
		wantExact(t, fmt.Sprintf("notes at %dx%d", w, h), view, w, h)
		lines := strings.Split(ansi.Strip(view), "\n")
		for i := range lines {
			lines[i] = strings.TrimRight(lines[i], " ")
		}
		return lines
	}

	o := newReleaseNotesOverlay()
	o.open("v2.9.9", notes, 40, 10)
	o.setPending(true)
	got := frame(o, 40, 10)
	want := []string{
		"v2.9.9     U update · S skip · Esc close",
		"## Features",
		"",
		"• Compact layout for terminals from 30x6",
		"  up to 59x19",
		"• Overlays draw a bare form in the",
		"  compact layout",
		"",
		"## Bug Fixes",
		"",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("40x10 pending:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// At the floor the keys keep their words and the tag gives way.
	for _, tt := range []struct{ tag, header string }{
		{"v2.9.9", "v2.9.9     U update·S skip·Esc"},
		{"v2.10.12", "v2.10.12   U update·S skip·Esc"},
		{"v2.10.12-rc.1", "v2.10.12-… U update·S skip·Esc"},
	} {
		o = newReleaseNotesOverlay()
		o.open(tt.tag, notes, 30, 6)
		o.setPending(true)
		got = frame(o, 30, 6)
		if got[0] != tt.header {
			t.Errorf("30x6 header for %s = %q, want %q", tt.tag, got[0], tt.header)
		}
		if got[1] != "## Features" || got[3] != "• Compact layout for terminals" {
			t.Errorf("30x6 notes for %s:\n%s", tt.tag, strings.Join(got, "\n"))
		}
	}

	// Viewer mode offers no U and no S.
	o = newReleaseNotesOverlay()
	o.open("v2.9.9", notes, 40, 10)
	if got = frame(o, 40, 10); got[0] != "v2.9.9            ↑/↓ scroll · Esc close" {
		t.Errorf("viewer header = %q", got[0])
	}
	o.setSize(30, 6)
	if got = frame(o, 30, 6); got[0] != "v2.9.9  ↑/↓ scroll · Esc close" {
		t.Errorf("viewer header at 30 columns = %q", got[0])
	}

	// No notes at all.
	o = newReleaseNotesOverlay()
	o.open("v2.9.9", "  ", 30, 6)
	if got = frame(o, 30, 6); got[1] != "No release notes available for" {
		t.Errorf("empty notes:\n%s", strings.Join(got, "\n"))
	}
}

// A withdrawn release's notice takes a row of its own under the header, and
// the notes lose that row from the bottom: the last row of the notes can
// still be scrolled into view.
func TestBareReleaseNotesNoticeHidesNoNotes(t *testing.T) {
	var sb strings.Builder
	for i := 1; i <= 12; i++ {
		fmt.Fprintf(&sb, "line %d\n\n", i)
	}
	o := newReleaseNotesOverlay()
	o.open("v2.9.9", sb.String(), 30, 6)
	o.setPending(true)
	o.withdraw("No longer pending: v2.9.9 was skipped")
	lines := strings.Split(ansi.Strip(o.View()), "\n")
	wantExact(t, "withdrawn notes", o.View(), 30, 6)
	if !strings.HasPrefix(lines[0], "v2.9.9") || strings.Contains(lines[0], "skip") || strings.Contains(lines[0], "update") {
		t.Errorf("the header still offers U or S: %q", lines[0])
	}
	// Wrapped, never cut to one row: the second half says what to do.
	if strings.TrimRight(lines[1], " ") != "No longer pending: v2.9.9 was" || strings.TrimRight(lines[2], " ") != "skipped" {
		t.Errorf("the notice rows = %q, %q", lines[1], lines[2])
	}
	if strings.TrimSpace(lines[3]) != "line 1" {
		t.Errorf("the notes do not start under the notice: %q", lines[3])
	}
	// The longer notice keeps its instruction at the floor, and is whole
	// from 10 rows.
	const next = "No longer pending — v2.10.12 is; close and R N for its notes"
	for _, s := range [][2]int{{30, 6}, {30, 10}, {59, 6}, {200, 8}} {
		n := newReleaseNotesOverlay()
		n.open("v2.9.9", sb.String(), s[0], s[1])
		n.setPending(true)
		n.withdraw(next)
		wantExact(t, "the longer notice", n.View(), s[0], s[1])
		got := strings.Join(strings.Fields(ansi.Strip(n.View())), " ")
		if !strings.Contains(got, "close and R N") {
			t.Errorf("%dx%d: the notice lost its instruction:\n%s", s[0], s[1], ansi.Strip(n.View()))
		}
		if s[1] >= 10 && !strings.Contains(got, next) {
			t.Errorf("%dx%d: the notice is not whole:\n%s", s[0], s[1], ansi.Strip(n.View()))
		}
		if !strings.Contains(got, "line 1") {
			t.Errorf("%dx%d: the notice left the notes no row:\n%s", s[0], s[1], ansi.Strip(n.View()))
		}
	}
	o.vp.GotoBottom()
	if view := ansi.Strip(o.View()); !strings.Contains(view, "line 12") {
		t.Errorf("the last row of the notes is hidden:\n%s", view)
	}
	// A resize keeps the place the reader was at.
	o.vp.SetYOffset(4)
	o.setSize(34, 8)
	if o.vp.YOffset() != 4 {
		t.Errorf("a resize moved the reader to row %d", o.vp.YOffset())
	}
}

// The bare notes never write through glamour's shared style: the normal
// form keeps its margin after a bare render.
func TestBareReleaseNotesLeaveTheNormalRenderAlone(t *testing.T) {
	notes := "## Features\n\n- A long bullet that wraps at any width a terminal is likely to have, twice over\n"
	fresh := newReleaseNotesOverlay()
	fresh.open("v1", notes, 100, 30)
	want := fresh.View()

	o := newReleaseNotesOverlay()
	o.open("v1", notes, 30, 6)
	_ = o.View()
	o.setSize(100, 30)
	if got := o.View(); got != want {
		t.Errorf("the normal frame changed after a bare render:\n%s\nwant:\n%s", ansi.Strip(got), ansi.Strip(want))
	}
}
