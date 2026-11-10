package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// A text input shared by the bare and the normal form of an overlay is laid
// out again whenever the form changes, and whenever the bare width does. The
// input itself only does that on the next edit, and only when the cursor has
// left its window, so a value entered at one width used to keep that width's
// window at the next.

// relayLong is a value longer than any normal input draws.
var relayLong = "https://www.youtube.com/watch?v=" + strings.Repeat("abcdefghij", 10)

// relayCursorCell reports whether the focused input's cursor cell (the one
// reverse-video cell of the frame) is on screen.
func relayCursorCell(frame string) bool {
	return strings.Contains(frame, "\x1b[7m") || strings.Contains(frame, "[7;")
}

// relayTo sends the sizes one after the other, drawing a frame at each as the
// program would.
func relayTo(a *App, sizes ...[2]int) {
	for _, s := range sizes {
		a.Update(tea.WindowSizeMsg{Width: s[0], Height: s[1]})
		_ = a.View()
	}
}

// relayCase is one overlay with a shared input: how it is opened, and how
// the long value gets into the input.
type relayCase struct {
	name string
	open func(t *testing.T, r *compactRig)
}

var relayCases = []relayCase{
	{"add video", func(t *testing.T, r *compactRig) { r.press("a", "a") }},
	{"cookie import", func(t *testing.T, r *compactRig) { r.press("e", "i") }},
	{"job log search", func(t *testing.T, r *compactRig) {
		r.press("o", "l")
		if !r.app.jobLog.IsVisible() {
			t.Fatalf("O L did not open the Job Log: %q", r.app.feedback.msg)
		}
		r.press("/")
	}},
	{"settings field", func(t *testing.T, r *compactRig) {
		r.press("`")
		toSection(t, r.app.settings, "Downloader")
		toField(t, r.app.settings, "output_template")
	}},
}

// TestSharedInputIsLaidOutAgainAcrossForms: a long value entered at any
// size, with the cursor at its end or back inside it, draws at 60x20 and at
// 120x40 exactly as it does in a terminal that was never any other size.
func TestSharedInputIsLaidOutAgainAcrossForms(t *testing.T) {
	for _, c := range relayCases {
		for _, back := range []int{0, 5, 40} {
			for _, normal := range [][2]int{{60, 20}, {120, 40}} {
				enter := func(r *compactRig) {
					c.open(t, r)
					_ = r.app.View() // the program draws before the first key arrives
					typeIntoApp(r.app, relayLong)
					for range back {
						r.press("left")
					}
					_ = r.app.View()
				}
				never := newRelayRig(t, normal[0], normal[1])
				enter(never)
				want := never.app.View().Content
				if rows := strings.Count(want, "\n") + 1; rows != normal[1] {
					t.Fatalf("%s: the never-compact frame is %d rows", c.name, rows)
				}
				for _, start := range [][2]int{{40, 10}, {200, 8}, {30, 6}, {30, 50}} {
					what := fmt.Sprintf("%s, cursor back %d, entered at %dx%d, grown to %dx%d", c.name, back, start[0], start[1], normal[0], normal[1])
					r := newRelayRig(t, start[0], start[1])
					enter(r)
					wantExact(t, what+" (before growing)", r.app.View().Content, start[0], start[1])
					if !relayCursorCell(r.app.View().Content) {
						t.Errorf("%s: no cursor cell in the bare frame:\n%s", what, plainFrame(r.app))
					}
					relayTo(r.app, normal)
					if got := r.app.View().Content; got != want {
						t.Errorf("%s:\n%s\nnever compact:\n%s", what, ansi.Strip(got), ansi.Strip(want))
					}
					// Through a bare size, a size too small for anything, and back.
					relayTo(r.app, [2]int{40, 10}, [2]int{20, 5}, normal)
					if got := r.app.View().Content; got != want {
						t.Errorf("%s, then through 20x5:\n%s\nnever compact:\n%s", what, ansi.Strip(got), ansi.Strip(want))
					}
				}
			}
		}
	}
}

// TestSharedInputKeepsItsCursorOnScreenAfterAShrink: a long value entered at
// a normal size, the terminal then shrunk: the bare frame is whole and its
// cursor cell is on screen, wherever in the value the cursor is.
func TestSharedInputKeepsItsCursorOnScreenAfterAShrink(t *testing.T) {
	for _, c := range relayCases {
		for _, back := range []int{0, 20, 60} {
			r := newRelayRig(t, 120, 40)
			c.open(t, r)
			_ = r.app.View() // the program draws before the first key arrives
			typeIntoApp(r.app, relayLong)
			for range back {
				r.press("left")
			}
			_ = r.app.View()
			for _, s := range [][2]int{{59, 19}, {200, 8}, {30, 6}, {40, 10}, {30, 50}} {
				relayTo(r.app, s)
				what := fmt.Sprintf("%s, cursor back %d, shrunk to %dx%d", c.name, back, s[0], s[1])
				frame := r.app.View().Content
				wantExact(t, what, frame, s[0], s[1])
				if !relayCursorCell(frame) {
					t.Errorf("%s: the cursor cell is off screen:\n%s", what, ansi.Strip(frame))
				}
			}
		}
	}
}

// newRelayRig is a compact rig with a job selected, so O L opens.
func newRelayRig(t *testing.T, w, h int) *compactRig {
	t.Helper()
	r := newCompactRig(t, w, h)
	r.app.OnGetJobLogs = func(string) []string { return bareLogLines(57) }
	r.selectJob(t, "done")
	return r
}

// An error too long for the rows under a list's cursor row takes the keys
// row and then the title before any of it is cut (owner ruling R-C11), and
// stays in one block under the cursor's row.
func TestBareListErrorsTakeTheKeysAndTheTitle(t *testing.T) {
	// 113 cells: four rows at 30 columns, one more than the body has under
	// the cursor's row at 30x6, so it needs the keys row and nothing else.
	const failure = "Delete failed: remove title.mkv: the file is in use by another process, close the player and try once more"
	whole := func(what string, rows []string, from int) {
		t.Helper()
		if got := strings.Join(strings.Fields(strings.Join(rows[from:], " ")), " "); got != failure {
			t.Errorf("%s: the error on screen is %q, want it whole:\n%s", what, got, strings.Join(rows, "\n"))
		}
	}

	files := bareFiles(30, 6)
	files.HandleKey(namedKey("down"))
	files.SetActionError(failure)
	wantExact(t, "orphans, an error at 30x6", files.View(), 30, 6)
	rows := plainRows(files.View())
	if rows[0] != "Orphans: 3 files, 2 history" || !strings.HasPrefix(rows[1], ">") {
		t.Errorf("orphans: the title or the cursor's row moved:\n%s", strings.Join(rows, "\n"))
	}
	whole("orphans", rows, 2)

	trim := bareTrim(30, 6, "delete error")
	trim.SetError(failure)
	wantExact(t, "trim delete, an error at 30x6", trim.View(), 30, 6)
	rows = plainRows(trim.View())
	if !strings.HasPrefix(rows[0], "Delete trim") || !strings.HasPrefix(rows[1], "> 1:05:00-2:06:00") {
		t.Errorf("trim: the title or the selected trim moved:\n%s", strings.Join(rows, "\n"))
	}
	whole("trim", rows, 2)

	// Nothing listed and a load error longer than the body: it takes the
	// keys row and the title too.
	empty := NewFilesDialogModel()
	empty.SetSize(30, 6)
	empty.Open()
	empty.SetFiles(nil)
	empty.SetHistory(nil)
	empty.SetFilesError(strings.Repeat("the scan failed and failed ", 3) + "end")
	wantExact(t, "orphans, a load error at 30x6", empty.View(), 30, 6)
	if got := ansi.Strip(empty.View()); !strings.Contains(got, "end") {
		t.Errorf("the load error is cut while rows are left:\n%s", got)
	}
}

// An action error that arrives while a delete is armed is shown, and the
// armed confirm moves to the keys row: neither hides the other.
func TestBareOrphansErrorDoesNotHideBehindAnArmedConfirm(t *testing.T) {
	m := bareFiles(40, 10)
	m.HandleKey(keyMsg("d"))
	if m.bareConfirm() == "" {
		t.Fatal("premise lost: D did not arm")
	}
	m.SetActionError("Delete failed: access denied")
	wantExact(t, "armed with an error", m.View(), 40, 10)
	rows := plainRows(m.View())
	if !strings.Contains(strings.Join(rows, "\n"), "Delete failed: access denied") || !strings.HasPrefix(rows[9], "D again: delete") {
		t.Errorf("armed with an error:\n%s", strings.Join(rows, "\n"))
	}
}

// The cookie import's failure is whole wherever the screen has the cells
// for it: no row is held back for a heading, and the keys row and the title
// give way before any of it is cut (owner ruling R-C11).
func TestBareCookieImportFailureIsWhole(t *testing.T) {
	flat := func(m *CookieImportDialogModel) string {
		return strings.Join(strings.Fields(ansi.Strip(m.View())), " ")
	}
	for _, c := range []struct {
		err  error
		w, h int
	}{
		{cookies.ErrImportNoCredential, 30, 6},
		{cookies.ErrImportNotNetscape, 59, 6},
		{cookies.ErrImportNotNetscape, 40, 8},
		{cookies.ErrImportNoCredential, 40, 10},
	} {
		m := bareCookie(c.w, c.h)
		m.SetResult(cookies.ImportResult{}, c.err)
		wantExact(t, fmt.Sprintf("the failure at %dx%d", c.w, c.h), m.View(), c.w, c.h)
		if got := flat(m); !strings.Contains(got, c.err.Error()) {
			t.Errorf("%dx%d: the failure is not whole:\n%s", c.w, c.h, ansi.Strip(m.View()))
		}
	}
	// With room to spare the title and the keys stay.
	m := bareCookie(40, 10)
	m.SetResult(cookies.ImportResult{}, cookies.ErrImportNoCredential)
	rows := plainRows(m.View())
	if rows[0] != "Cookie import" || rows[9] != "Enter/Esc close" || !strings.HasPrefix(rows[1], "Import failed: ") {
		t.Errorf("the failure at 40x10:\n%s", strings.Join(rows, "\n"))
	}
	// Longer than the screen: every row is the error's, and it ends in an
	// ellipsis.
	m = bareCookie(30, 6)
	m.SetResult(cookies.ImportResult{}, errors.New(strings.Repeat("open /home/name/cookies.txt failed ", 6)))
	wantExact(t, "a failure longer than the screen", m.View(), 30, 6)
	rows = plainRows(m.View())
	if !strings.HasPrefix(rows[0], "open /home") || !strings.HasSuffix(rows[5], "…") {
		t.Errorf("a failure longer than the screen:\n%s", strings.Join(rows, "\n"))
	}
}

// The refusal of a confirm that does not fit names the size for the terminal
// as it is now, not as it was when Enter was pressed, and is gone once the
// summary fits.
func TestBareMenuRefusalFollowsTheSize(t *testing.T) {
	r := bareMenuApp(t, 30, 6)
	a := r.app
	menuTo(t, r, "A I")
	r.press("enter", "enter")
	if a.actionMenu.jobConfirm || !strings.Contains(plainFrame(a), "Enlarge the terminal to 30x9") {
		t.Fatalf("premise lost: no refusal at 30x6:\n%s", plainFrame(a))
	}
	// Wider, the summary wraps to fewer rows: the size named follows.
	a.Update(tea.WindowSizeMsg{Width: 40, Height: 6})
	wantExact(t, "the refusal at 40x6", a.View().Content, 40, 6)
	got := plainFrame(a)
	if strings.Contains(got, "30x9") {
		t.Errorf("the refusal still names the size for 30 columns:\n%s", got)
	}
	if want := a.actionMenu.bareConfirmRefusal(); a.actionMenu.bareConfirmFits() || !strings.Contains(strings.Join(strings.Fields(got), " "), want) {
		t.Errorf("the refusal at 40x6 is not %q:\n%s", want, got)
	}
	// Tall enough for the summary: nothing is refused any more.
	a.Update(tea.WindowSizeMsg{Width: 59, Height: 12})
	if got := plainFrame(a); strings.Contains(got, "Enlarge") {
		t.Errorf("a refusal for a summary that now fits:\n%s", got)
	}
}

// relayLongFolder is a folder of n archives whose names share a long prefix
// and differ in their last cells.
func relayLongFolder(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	for i := range n {
		name := fmt.Sprintf("hololive-karaoke-with-a-long-shared-prefix-2026-11-%02d.zip", i+1)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("PK"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The archive picker after a shrink and back is the picker of a terminal
// that was never smaller, with the cursor low in a folder longer than the
// picker: the window is laid out from the top of the folder again.
func TestBareImportPickerRoundTripInALongFolder(t *testing.T) {
	dir := relayLongFolder(t, 40)
	for _, downs := range []int{0, 5, 25, 39} {
		open := func() *ImportDialogModel {
			m := bareImport(t, 60, 20, dir, "pick")
			for range downs {
				m.UpdateComponents(namedKey("down"))
			}
			return m
		}
		want := open().View()
		m := open()
		for _, s := range [][2]int{{30, 6}, {200, 8}, {30, 50}, {40, 10}} {
			m.SetSize(s[0], s[1])
			wantExact(t, fmt.Sprintf("%d down, shrunk to %dx%d", downs, s[0], s[1]), m.View(), s[0], s[1])
		}
		m.SetSize(60, 20)
		if got := m.View(); got != want {
			t.Errorf("%d down: the 60x20 picker changed across a shrink:\n%s\nnever shrunk:\n%s", downs, ansi.Strip(got), ansi.Strip(want))
		}
		// Through a size too small to draw anything, too.
		m.SetSize(30, 6)
		m.SetSize(20, 5)
		m.SetSize(60, 20)
		if got := m.View(); got != want {
			t.Errorf("%d down: the 60x20 picker changed across 20x5:\n%s\nnever shrunk:\n%s", downs, ansi.Strip(got), ansi.Strip(want))
		}
	}
}

// Two archives whose names differ only at their end are different rows of
// the bare picker: a long name is cut in its middle, never at its end.
func TestBareImportPickerRowsKeepTheirEnd(t *testing.T) {
	dir := relayLongFolder(t, 3)
	for _, s := range [][2]int{{30, 6}, {40, 10}} {
		m := bareImport(t, s[0], s[1], dir, "pick")
		wantExact(t, fmt.Sprintf("the picker at %dx%d", s[0], s[1]), m.View(), s[0], s[1])
		rows := plainRows(m.View())
		if rows[1] == rows[2] || !strings.HasSuffix(rows[1], "-01.zip") || !strings.HasSuffix(rows[2], "-02.zip") || !strings.HasPrefix(rows[1], "> holo") {
			t.Errorf("%dx%d: two archives look the same:\n%s", s[0], s[1], strings.Join(rows, "\n"))
		}
	}
}
