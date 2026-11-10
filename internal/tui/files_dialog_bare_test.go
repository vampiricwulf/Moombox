package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// ---- Cookie file import ----

func TestBareCookieImportFrames(t *testing.T) {
	m := bareCookie(40, 10)
	wantFrame(t, "the path step at 40x10", m.View(), 40, 10, []string{
		"Import cookies.txt",
		"> ~/Downloads/cookies.txt",
		"Path to a Netscape-format cookies.txt",
		"exported from your browser",
		"", "", "", "", "",
		"Enter import  Esc cancel",
	})
	m.SetResult(rolledBackImport(), nil)
	wantFrame(t, "the result at 40x10", m.View(), 40, 10, []string{
		"Cookie import",
		"YouTube: rolled-back (authenticates)",
		"Twitch:  imported (ok)",
		"rolled back = old cookies kept",
		"", "", "", "", "",
		"Enter/Esc close",
	})

	m = bareCookie(30, 6)
	m.input.SetValue(`C:\Users\wulf\Downloads\exports\youtube\cookies.txt`)
	m.HandleKey("enter")
	wantFrame(t, "a path error at 30x6", m.View(), 30, 6, []string{
		"Import cookies.txt",
		`> exports\youtube\cookies.txt`,
		"That path does not exist:",
		`C:\Users\wulf\Downloads\export`,
		`s\youtube\cookies.txt`,
		"Enter import  Esc cancel",
	})
	m.SetImporting()
	if rows := plainRows(m.View()); !strings.HasSuffix(rows[1], "Importing, verifying...") || rows[5] != "Esc close (import continues)" {
		t.Errorf("running at 30x6:\n%s", strings.Join(rows, "\n"))
	}
	m.SetResult(rolledBackImport(), nil)
	wantFrame(t, "the result at 30x6", m.View(), 30, 6, []string{
		"Cookie import",
		"YT rolled-back (authenticates)",
		"TW imported (ok)",
		"rolled back = old cookies kept",
		"",
		"Enter/Esc close",
	})
	m.SetResult(cookies.ImportResult{}, errors.New("the file holds no cookies for youtube.com or twitch.tv"))
	wantFrame(t, "a failed import at 30x6", m.View(), 30, 6, []string{
		"Cookie import",
		"Import failed",
		"the file holds no cookies for",
		"youtube.com or twitch.tv",
		"",
		"Enter/Esc close",
	})
}

// The input row is on screen under every error, with its prompt and the
// end of what was typed, and the error's sentence is whole (owner ruling
// R-C11: a message may never cover the focused input). A path too long for
// the rows left keeps its file name.
func TestBareCookieImportErrorNeverCoversTheInput(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat(`C:\Users\name\Downloads\`, 9) + "the-export.txt"
	compactSizes(func(w, h int) {
		for _, tt := range []struct{ typed, sentence, tail string }{
			{"", "Enter a path to a cookies.txt file", ""},
			{long, "That path does not exist:", "the-export.txt"},
			{dir, "That is a directory:", dir[len(dir)-6:]},
		} {
			m := bareCookie(w, h)
			m.input.SetValue(tt.typed)
			m.HandleKey("enter")
			rows := plainRows(m.View())
			if !strings.HasPrefix(rows[1], "> ") || (tt.typed != "" && !strings.Contains(rows[1], tt.typed[len(tt.typed)-6:])) {
				t.Errorf("%dx%d, %q: the input row is %q", w, h, tt.sentence, rows[1])
			}
			if got := strings.Join(strings.Fields(strings.Join(rows[2:h-1], " ")), " "); !strings.HasPrefix(got, tt.sentence) {
				t.Errorf("%dx%d: the error does not start %q: %q", w, h, tt.sentence, got)
			}
			if got := strings.Join(rows[2:h-1], ""); !strings.HasSuffix(got, tt.tail) {
				t.Errorf("%dx%d, %q: the end of the path is not on screen:\n%s", w, h, tt.sentence, strings.Join(rows, "\n"))
			}
			if rows[h-1] != "Enter import  Esc cancel" {
				t.Errorf("%dx%d: the keys row is %q", w, h, rows[h-1])
			}
		}
	})
}

// E I at the compact floor, through the App: each typed rune lands in the
// input once, Enter on a real file runs the import with the path, and no
// frame on the way shows anything of the file (the chord's ruling: the
// path only, never the content).
//
// Mutant: the handleKey size guard holding the overlay — typing does
// nothing.
func TestBareCookieImportThroughTheApp(t *testing.T) {
	const secret = "SAPISID-very-secret-value"
	file := filepath.Join(t.TempDir(), "c.txt")
	if err := os.WriteFile(file, []byte(".youtube.com\tTRUE\t/\tTRUE\t0\tSAPISID\t"+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newCompactRig(t, 30, 6)
	a := r.app
	var imported []string
	a.OnImportCookieFile = func(path string) (cookies.ImportResult, error) {
		imported = append(imported, path)
		return rolledBackImport(), nil
	}
	var frames []string
	snap := func() { frames = append(frames, plainFrame(a)) }

	r.press("e", "i")
	if !a.cookieImportDlg.IsVisible() || a.screenHeld() {
		t.Fatalf("E I did not open the import bare at 30x6: feedback %q", a.feedback.msg)
	}
	wantExact(t, "the import at 30x6", a.View().Content, 30, 6)
	snap()
	r.press("a", "b", "c")
	if got := a.cookieImportDlg.input.Value(); got != "abc" {
		t.Fatalf("typed abc, the input holds %q", got)
	}
	snap()
	a.cookieImportDlg.input.SetValue(file)
	_, cmd := a.handleKey(keyMsg("enter"))
	snap()
	if !a.cookieImportDlg.IsImporting() {
		t.Fatalf("Enter on a real file did not start the import:\n%s", plainFrame(a))
	}
	applyStatsCmd(a, cmd)
	snap()
	if len(imported) != 1 || imported[0] != file {
		t.Errorf("imported %v, want [%s]", imported, file)
	}
	if got := plainFrame(a); !strings.Contains(got, "YT rolled-back (authenticates)") || !strings.Contains(got, "TW imported (ok)") {
		t.Errorf("the result is not on screen:\n%s", got)
	}
	wantExact(t, "the result at 30x6", a.View().Content, 30, 6)
	r.press("enter")
	if a.hasActiveOverlay() {
		t.Error("Enter did not close the result")
	}
	snap()
	for _, f := range frames {
		if strings.Contains(f, "SAPISID") || strings.Contains(f, secret) {
			t.Errorf("a frame shows cookie content:\n%s", f)
		}
	}
}

// ---- Orphaned items ----

func TestBareOrphansFrames(t *testing.T) {
	m := bareFiles(40, 10)
	wantFrame(t, "40x10", m.View(), 40, 10, []string{
		"Orphans: 3 files, 2 history",
		">stg  job-abc123/segments 1.2GB 2 asides",
		" out  Some Stream Title.mkv        3.4GB",
		" trim clip-0001.mp4               12.0MB",
		" ── history ──",
		" hist dQw4w9WgXcQ                 2h ago",
		" hist aaaaaaaaaaa                 3d ago",
		"",
		"",
		"D delete  A all  R refresh  Esc",
	})
	m = bareFiles(30, 6)
	wantFrame(t, "30x6", m.View(), 30, 6, []string{
		"Orphans: 3 files, 2 history",
		">stg  job-abc123/segmen… 1.2GB",
		" out  Some Stream Title… 3.4GB",
		" trim clip-0001.mp4     12.0MB",
		"holds 2 asides",
		"D del  A all  R refresh  Esc",
	})
	m.HandleKey(namedKey("end"))
	wantFrame(t, "the history rows at 30x6", m.View(), 30, 6, []string{
		"Orphans: 3 files, 2 history",
		" ── history ──",
		" hist dQw4w9WgXcQ       2h ago",
		">hist aaaaaaaaaaa       3d ago",
		"",
		"D del  A all  R refresh  Esc",
	})

	m = NewFilesDialogModel()
	m.SetSize(30, 6)
	m.Open()
	if rows := plainRows(m.View()); !strings.HasSuffix(rows[1], "Scanning...") {
		t.Errorf("scanning:\n%s", strings.Join(rows, "\n"))
	}
	m.SetFiles(nil)
	m.SetHistory(nil)
	wantFrame(t, "nothing orphaned at 30x6", m.View(), 30, 6, []string{
		"Orphans: 0 files, 0 history",
		"No orphaned files or history.",
		"", "", "",
		"R refresh  Esc",
	})
}

// The armed confirms in their short forms (owner ruling R-C11): the kind,
// the set-aside recordings a staging entry holds at every size, the count
// of a whole section, and from 40 columns the entry's name. The row the
// confirm is about stays on screen, marked.
//
// Mutant: reading the confirm off feedbackMsg — the 40-cell-plus normal
// wording is cut at 30 columns and loses what it deletes.
func TestBareOrphansConfirmsAreShortAndWhole(t *testing.T) {
	confirmRow := func(m *FilesDialogModel) string {
		rows := plainRows(m.View())
		return rows[len(rows)-2]
	}
	for _, tt := range []struct {
		w    int
		keys []string
		want string
	}{
		{30, []string{"d"}, "D again: delete + 2 asides"},
		{40, []string{"d"}, "D again: delete + 2 asides"},
		{30, []string{"down", "d"}, "D again: delete this file"},
		{40, []string{"down", "d"}, "D again: delete Some Stream Title.mkv"},
		{30, []string{"a"}, "A again: delete all 3 files"},
		{30, []string{"end", "d"}, "D again: remove this history"},
		{40, []string{"end", "d"}, "D again: remove history aaaaaaaaaaa"},
		{30, []string{"end", "a"}, "A again: remove 2 history"},
	} {
		m := bareFiles(tt.w, 6)
		for _, k := range tt.keys {
			m.HandleKey(namedKey(k))
		}
		if got := confirmRow(m); got != tt.want {
			t.Errorf("width %d, keys %v: confirm %q, want %q", tt.w, tt.keys, got, tt.want)
		}
		if !strings.Contains(ansi.Strip(m.View()), "\n>") {
			t.Errorf("width %d, keys %v: no marked row on screen:\n%s", tt.w, tt.keys, ansi.Strip(m.View()))
		}
	}

	// A long name is cut at its head, so the file name stays.
	m := bareFiles(40, 8)
	m.files[1].RelPath = "a channel folder/with a long archive name/The Final Stream.mkv"
	m.rebuildList()
	m.HandleKey(namedKey("down"))
	m.HandleKey(keyMsg("d"))
	if got := confirmRow(m); got != "D again: delete …me/The Final Stream.mkv" {
		t.Errorf("a long name in the confirm = %q", got)
	}
	// Moving the cursor retracts it, and the row is blank again.
	m.HandleKey(namedKey("down"))
	if got := confirmRow(m); got != "" {
		t.Errorf("after moving off the armed row the message row = %q", got)
	}
	// Armed in the normal form, it is still armed, in the short form, after
	// a shrink.
	m = bareFiles(100, 30)
	m.HandleKey(keyMsg("d"))
	m.SetSize(30, 6)
	if got := confirmRow(m); got != "D again: delete + 2 asides" {
		t.Errorf("armed at 100x30 and shrunk: %q", got)
	}
}

// An error is never cut to the one message row: it wraps upward over the
// list, which keeps the cursor's row, and the title and the keys stay. When
// it is gone the list has its rows back.
//
// Mutant: truncating the error to one row; letting the list keep its
// height, so the message pushes the keys off.
func TestBareOrphansErrorWrapsUpwardOverTheList(t *testing.T) {
	const failure = "Delete failed: remove title.mkv: the file is in use by another process"
	m := bareFiles(30, 8)
	m.HandleKey(namedKey("down"))
	m.SetActionError(failure)
	rows := plainRows(m.View())
	wantExact(t, "an error at 30x8", m.View(), 30, 8)
	if rows[0] != "Orphans: 3 files, 2 history" || rows[7] != "D del  A all  R refresh  Esc" {
		t.Errorf("the title or the keys moved:\n%s", strings.Join(rows, "\n"))
	}
	var msg []string
	listRows := 0
	for _, row := range rows[1:7] {
		if strings.HasPrefix(row, ">") || strings.HasPrefix(row, " ") {
			listRows++
			continue
		}
		msg = append(msg, row)
	}
	if got := strings.Join(msg, " "); got != failure {
		t.Errorf("the error on screen = %q, want it whole", got)
	}
	if listRows != 6-len(msg) || !strings.Contains(strings.Join(rows, "\n"), "\n>out  Some Stream Title") {
		t.Errorf("the cursor's row is not on screen above the error:\n%s", strings.Join(rows, "\n"))
	}

	// At the floor a long error leaves the list one row, the cursor's, and
	// takes the keys row and the title before it is cut (R-C11).
	m.SetSize(30, 6)
	m.SetActionError(longError)
	rows = plainRows(m.View())
	if !strings.HasPrefix(rows[0], ">out ") || !strings.HasPrefix(rows[1], "the database is locked") || !strings.HasSuffix(rows[5], "…") {
		t.Errorf("a long error at 30x6:\n%s", strings.Join(rows, "\n"))
	}

	// Starting a new delete clears the error, and the list is back.
	m.HandleKey(keyMsg("d"))
	rows = plainRows(m.View())
	if m.list.Height() != 3 || rows[4] != "D again: delete this file" || !strings.HasPrefix(rows[1], " stg ") {
		t.Errorf("after the error the list has %d rows:\n%s", m.list.Height(), strings.Join(rows, "\n"))
	}

	// One source failed and the other loaded: the list stays, the error is
	// whole under it.
	m = bareFiles(40, 10)
	m.SetHistoryError("database is locked")
	rows = plainRows(m.View())
	if got := strings.Join(rows[7:9], " "); got != "Couldn't load orphaned history: database is locked" {
		t.Errorf("a load error beside the list:\n%s", strings.Join(rows, "\n"))
	}
	if !strings.HasPrefix(rows[1], ">stg ") {
		t.Errorf("the list is gone under a load error:\n%s", strings.Join(rows, "\n"))
	}
}

// A O at the compact floor, through the App: the wheel moves the cursor,
// D arms and a second D deletes the selected file, and Esc closes.
func TestBareOrphansThroughTheApp(t *testing.T) {
	r := newCompactRig(t, 30, 6)
	a := r.app
	var deleted []string
	a.OnListOrphans = func() ([]OrphanedFileEntry, error) { return nil, nil }
	a.OnListOrphanedHistory = func() ([]OrphanedHistoryEntry, error) { return nil, nil }
	a.OnDeleteOrphan = func(path string) error { deleted = append(deleted, path); return nil }
	r.press("a", "o")
	if !a.filesDlg.IsVisible() || a.screenHeld() {
		t.Fatalf("A O did not open the orphans bare at 30x6: feedback %q", a.feedback.msg)
	}
	fixture := bareFiles(30, 6)
	a.filesDlg.SetFiles(fixture.files)
	a.filesDlg.SetHistory(fixture.history)
	wantExact(t, "the orphans at 30x6", a.View().Content, 30, 6)
	a.Update(tea.MouseWheelMsg{X: 5, Y: 2, Button: tea.MouseWheelDown})
	r.press("d")
	if got := strings.Split(plainFrame(a), "\n")[4]; got != "D again: delete this file" {
		t.Errorf("the confirm row = %q", got)
	}
	if len(deleted) != 0 {
		t.Fatal("one D deleted")
	}
	r.pressRunLast("d")
	if len(deleted) != 1 || deleted[0] != "/o/title.mkv" {
		t.Errorf("D D deleted %v, want [/o/title.mkv]", deleted)
	}
	r.press("esc")
	if a.hasActiveOverlay() {
		t.Error("Esc did not close the orphans")
	}
}
