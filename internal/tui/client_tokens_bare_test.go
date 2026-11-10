package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/ytdlpplugin"
)

func TestBareClientTokensFrames(t *testing.T) {
	m := bareTokens(40, 10)
	wantFrame(t, "40x10", m.View(), 40, 10, []string{
		"Tokens (3)",
		">Firefox on the desktop upstairs  2h ago",
		" Phone                             never",
		" Old laptop                      12d ago",
		"", "", "", "",
		"192.168.1.20",
		"D revoke  R refresh  Esc",
	})
	m.HandleKey(keyMsg("d"))
	if got := plainRows(m.View())[8]; got != `D again: revoke "Firefox on the deskto…"` {
		t.Errorf("the armed revoke at 40 columns = %q", got)
	}

	m = bareTokens(30, 6)
	wantFrame(t, "30x6", m.View(), 30, 6, []string{
		"Tokens (3)",
		">Firefox on the deskto… 2h ago",
		" Phone                   never",
		" Old laptop            12d ago",
		"192.168.1.20",
		"D revoke  R refresh  Esc",
	})
	m.HandleKey(keyMsg("d"))
	wantFrame(t, "armed at 30x6", m.View(), 30, 6, []string{
		"Tokens (3)",
		">Firefox on the deskto… 2h ago",
		" Phone                   never",
		" Old laptop            12d ago",
		`D again: revoke "Firefox on …"`,
		"D revoke  R refresh  Esc",
	})
	// Moving off the armed row retracts the confirm; a full IPv6 address
	// keeps both its ends.
	m.HandleKey(namedKey("down"))
	m.HandleKey(namedKey("down"))
	if got := plainRows(m.View())[4]; got != "2001:0db8:85a3:…8a2e:0370:7334" {
		t.Errorf("the detail row for an IPv6 address = %q", got)
	}
	m.SetSize(40, 10)
	if got := plainRows(m.View())[8]; got != "2001:0db8:85a3:0000:0000:8a2e:0370:7334" {
		t.Errorf("a full IPv6 address at 40 columns = %q", got)
	}

	m = bareTokens(30, 6)
	m.SetError("failed to list client tokens: the database is locked by another process")
	wantFrame(t, "an error at 30x6", m.View(), 30, 6, []string{
		"Tokens (3)",
		"failed to list client tokens:",
		"the database is locked by",
		"another process",
		"",
		"R refresh  Esc",
	})
}

// The armed revoke is derived from the confirm state, so it survives a
// resize between the normal form, the bare form and back, and names the
// label at every compact width (owner ruling R-C11).
func TestBareClientTokensConfirmNamesTheLabel(t *testing.T) {
	m := bareTokens(100, 30)
	m.HandleKey(namedKey("down"))
	m.HandleKey(keyMsg("d"))
	if !strings.Contains(ansi.Strip(m.View()), "Press D again to revoke this token") {
		t.Fatal("premise lost: the normal form does not show the armed revoke")
	}
	compactSizes(func(w, h int) {
		m.SetSize(w, h)
		rows := plainRows(m.View())
		if got := rows[h-2]; got != `D again: revoke "Phone"` {
			t.Errorf("%dx%d: the confirm row = %q", w, h, got)
		}
		if !strings.Contains(strings.Join(rows[1:h-2], "\n"), ">Phone") {
			t.Errorf("%dx%d: the armed row is not on screen:\n%s", w, h, strings.Join(rows, "\n"))
		}
	})
	m.SetSize(100, 30)
	if !strings.Contains(ansi.Strip(m.View()), "Press D again to revoke this token") {
		t.Error("the armed revoke did not survive the resizes")
	}
}

// The cursor's row is on screen whichever token is selected, in a list
// longer than the screen, with that token's address under the list.
func TestBareClientTokensKeepTheCursorOnScreen(t *testing.T) {
	m := NewClientTokensDialogModel()
	m.SetSize(30, 6)
	m.Open()
	var tokens []*database.ClientToken
	for i := range 14 {
		tokens = append(tokens, &database.ClientToken{ID: fmt.Sprint("t", i), Label: fmt.Sprint("client ", i), LastIP: fmt.Sprint("10.0.0.", i)})
	}
	m.SetTokens(tokens)
	for i := range 14 {
		rows := plainRows(m.View())
		if !strings.Contains(strings.Join(rows[1:4], "\n"), fmt.Sprintf(">client %d ", i)) || rows[4] != fmt.Sprint("10.0.0.", i) {
			t.Fatalf("token %d selected:\n%s", i, strings.Join(rows, "\n"))
		}
		m.HandleKey(namedKey("down"))
	}
}

// A K at the compact floor, through the App: the wheel moves the cursor, D
// arms, a second D revokes the selected token, and Esc closes.
func TestBareClientTokensThroughTheApp(t *testing.T) {
	r := newCompactRig(t, 30, 6)
	a := r.app
	var revoked []string
	a.OnDeleteClientToken = func(id string) error { revoked = append(revoked, id); return nil }
	r.press("a", "k")
	if !a.clientTokensDlg.IsVisible() || a.screenHeld() {
		t.Fatalf("A K did not open the tokens bare at 30x6: feedback %q", a.feedback.msg)
	}
	a.clientTokensDlg.SetTokens([]*database.ClientToken{{ID: "t1", Label: "Phone"}, {ID: "t2", Label: "Laptop"}})
	wantExact(t, "the tokens at 30x6", a.View().Content, 30, 6)
	a.Update(tea.MouseWheelMsg{X: 5, Y: 2, Button: tea.MouseWheelDown})
	r.press("d")
	if got := strings.Split(plainFrame(a), "\n")[4]; got != `D again: revoke "Laptop"` {
		t.Errorf("the confirm row = %q", got)
	}
	if len(revoked) != 0 {
		t.Fatal("one D revoked")
	}
	r.pressRunLast("d")
	if len(revoked) != 1 || revoked[0] != "t2" {
		t.Errorf("D D revoked %v, want [t2]", revoked)
	}
	r.press("esc")
	if a.hasActiveOverlay() {
		t.Error("Esc did not close the tokens")
	}
}

// ---- yt-dlp plugin ----

func TestBareYtdlpFrames(t *testing.T) {
	wantFrame(t, "a mismatch at 40x10", bareYtdlp(40, 10, mismatchedPlugin()).View(), 40, 10, []string{
		"yt-dlp plugin: installed",
		"Moombox 774 (https: off)",
		"Plugin  8080 (https: off)",
		"Mismatch: I rewrites it to match",
		`Path C:\Users\name\AppData\Roaming\yt-`,
		`     dlp\plugins\moombox-pot`,
		`Dir  C:\Users\name\AppData\Roaming\yt-`,
		`     dlp\plugins`,
		"",
		"I install  R refresh  Esc",
	})
	wantFrame(t, "a mismatch at 30x6", bareYtdlp(30, 6, mismatchedPlugin()).View(), 30, 6, []string{
		"yt-dlp plugin: installed",
		"Moombox 774 (https: off)",
		"Plugin  8080 (https: off)",
		"Mismatch: I rewrites it",
		"Paths: enlarge to see",
		"I install  R refresh  Esc",
	})
	wantFrame(t, "not installed at 30x6", bareYtdlp(30, 6, ytdlpplugin.Info{CurrentPort: 774, PluginDir: `C:\p`}).View(), 30, 6, []string{
		"yt-dlp plugin: not installed",
		"Moombox 774 (https: off)",
		`Dir  C:\p`,
		"", "",
		"I install  R refresh  Esc",
	})
	unparseable := ytdlpplugin.Info{Installed: true, Unparseable: true, CurrentPort: 65535, HTTPSEnabled: true,
		PluginDir: `C:\Users\name\AppData\Roaming\yt-dlp\plugins`, ExtractedPath: `C:\Users\name\AppData\Roaming\yt-dlp\plugins\moombox-pot`}
	wantFrame(t, "not recognized at 30x6", bareYtdlp(30, 6, unparseable).View(), 30, 6, []string{
		"yt-dlp plugin: installed",
		"Moombox 65535 (https: on)",
		"Not recognized: I reinstalls",
		"Paths: enlarge to see",
		"",
		"I install  R refresh  Esc",
	})
}

// The verdict rows are on screen at every compact size, and a path is
// either on screen whole or replaced by a row that says it is hidden: a cut
// path is never shown.
//
// Mutant: dropping the fit test — at 30x6 the path's first rows are drawn
// and its end is cut off.
func TestBareYtdlpShowsVerdictsAlwaysAndPathsWholeOrNot(t *testing.T) {
	info := mismatchedPlugin()
	compactSizes(func(w, h int) {
		rows := plainRows(bareYtdlp(w, h, info).View())
		body := strings.Join(rows[1:h-1], "\n")
		for _, want := range []string{"Moombox 774 (https: off)", "Plugin  8080 (https: off)", "Mismatch: I rewrites it"} {
			if !strings.Contains(body, want) {
				t.Errorf("%dx%d: the verdict row %q is missing:\n%s", w, h, want, body)
			}
		}
		if rows[0] != "yt-dlp plugin: installed" || rows[h-1] != "I install  R refresh  Esc" {
			t.Errorf("%dx%d: title %q, keys %q", w, h, rows[0], rows[h-1])
		}
		// Each path: whole (its wrapped rows joined give it back), or said
		// to be hidden.
		joined := strings.ReplaceAll(strings.ReplaceAll(body, "\n     ", ""), "\n", "|") + "|"
		pathWhole := strings.Contains(joined, "Path "+info.ExtractedPath+"|")
		dirWhole := strings.Contains(joined, "Dir  "+info.PluginDir+"|")
		switch {
		case pathWhole && dirWhole:
		case pathWhole && strings.Contains(body, "Dir: enlarge to see") && !strings.Contains(body, "Dir  "):
		case strings.Contains(body, "Paths: enlarge to see") && !strings.Contains(body, "Path ") && !strings.Contains(body, "Dir  "):
		default:
			t.Errorf("%dx%d: a path is neither whole nor said to be hidden:\n%s", w, h, body)
		}
	})
}

// E Y at the compact floor, through the App: it opens bare, I installs and
// Esc closes.
func TestBareYtdlpThroughTheApp(t *testing.T) {
	r := newCompactRig(t, 30, 6)
	a := r.app
	installs := 0
	a.OnYtdlpPluginStatus = func() (ytdlpplugin.Info, error) { return mismatchedPlugin(), nil }
	a.OnInstallYtdlpPlugin = func() error { installs++; return nil }
	r.press("e", "y")
	if !a.ytdlpDlg.IsVisible() || a.screenHeld() {
		t.Fatalf("E Y did not open the plugin status bare at 30x6: feedback %q", a.feedback.msg)
	}
	a.ytdlpDlg.SetStatus(mismatchedPlugin())
	wantExact(t, "the plugin status at 30x6", a.View().Content, 30, 6)
	if !strings.Contains(plainFrame(a), "Mismatch: I rewrites it") {
		t.Fatalf("the status is not on screen:\n%s", plainFrame(a))
	}
	_, cmd := a.handleKey(keyMsg("i"))
	if !a.ytdlpDlg.installing || !strings.Contains(plainFrame(a), "Installing...") {
		t.Errorf("I did not start the install:\n%s", plainFrame(a))
	}
	wantExact(t, "installing at 30x6", a.View().Content, 30, 6)
	applyStatsCmd(a, cmd)
	if installs != 1 {
		t.Errorf("I installed %d times, want 1", installs)
	}
	r.press("esc")
	if a.hasActiveOverlay() {
		t.Error("Esc did not close the plugin status")
	}
}

// An error too long for the rows under the title takes the keys row and
// then the title before any of it is cut (owner ruling R-C11): at 40x10 the
// long error is on screen whole.
func TestBareDialogErrorsWrapUpward(t *testing.T) {
	tk := bareTokens(40, 6)
	tk.SetError(longError)
	yt := bareYtdlp(40, 6, mismatchedPlugin())
	yt.SetError(longError)
	for name, view := range map[string]string{"tokens": tk.View(), "ytdlp": yt.View()} {
		wantExact(t, name, view, 40, 6)
		got := strings.Join(strings.Fields(strings.Join(plainRows(view), " ")), " ")
		// A word broken at a row's end reads with a space in it: compare
		// without spaces.
		if strings.ReplaceAll(got, " ", "") != strings.ReplaceAll(longError, " ", "") {
			t.Errorf("%s: the error on screen is not whole:\n%s", name, strings.Join(plainRows(view), "\n"))
		}
	}
}
