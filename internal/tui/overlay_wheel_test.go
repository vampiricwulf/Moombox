package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// The two answers an overlay gives the mouse wheel (owner ruling R-C2).
const (
	wheelScrolls = "wheel scrolls"
	wheelInert   = "deliberately inert"
)

// overlayWheel is the census of what the mouse wheel does over each overlay.
// Every overlay states its answer here, so a new one has to decide. One that
// scrolls needs a row in overlayWheelCases too, which proves it.
//
// The inert ones have nothing to scroll: cookie import is one path field,
// and the yt-dlp plugin and statistics overlays are fixed screens that fit.
var overlayWheel = map[string]string{
	"settings":       wheelScrolls, // the section under the cursor
	"help":           wheelScrolls, // three rows a notch
	"release notes":  wheelScrolls, // three rows a notch
	"ffmpeg check":   wheelScrolls, // the install-script review, three rows a notch
	"setup wizard":   wheelScrolls, // the channel list's cursor
	"action menu":    wheelScrolls, // the menu's cursor
	"import":         wheelScrolls, // the file picker's cursor
	"cookie import":  wheelInert,
	"chapter import": wheelScrolls, // the paste box's cursor
	"add video":      wheelScrolls, // the format table's cursor
	"trim":           wheelScrolls, // the delete list's cursor
	"files":          wheelScrolls, // the list's cursor
	"client tokens":  wheelScrolls, // the list's cursor
	"ytdlp plugin":   wheelInert,
	"stats":          wheelInert,
	"job log":        wheelScrolls, // three rows a notch
}

// overlayWheelCase puts one scrolling overlay, already open, into the state
// the wheel acts on, and reads the position the wheel moves.
type overlayWheelCase struct {
	prepare func(t *testing.T, a *App)
	pos     func(a *App) string
	// rows is how far one notch moves pos when pos is a row number: 3 for a
	// text overlay, 1 for a cursor. 0: pos is not a number, only compared.
	rows int
	// clicks: the overlay had a mouse handler before R-C2 and a click does
	// something there, so the "a click changes nothing" check is skipped.
	clicks bool
}

func longText(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

var overlayWheelCases = map[string]overlayWheelCase{
	"settings": {
		prepare: func(t *testing.T, a *App) {},
		pos:     func(a *App) string { return a.settings.View() },
		clicks:  true,
	},
	"help": {
		prepare: func(t *testing.T, a *App) { a.help.SetSize(80, 14) },
		pos:     func(a *App) string { return fmt.Sprint(a.help.viewport.YOffset()) },
		rows:    3,
	},
	"release notes": {
		prepare: func(t *testing.T, a *App) {
			a.releaseNotesPopup.open("v9.9.9", longText(200), 80, 12)
		},
		pos:  func(a *App) string { return fmt.Sprint(a.releaseNotesPopup.vp.YOffset()) },
		rows: 3,
	},
	"ffmpeg check": {
		prepare: func(t *testing.T, a *App) {
			a.ffmpegCheck.SetSize(a.width, a.height)
			a.ffmpegCheck.ShowReview(longText(60), "token")
		},
		pos:  func(a *App) string { return fmt.Sprint(a.ffmpegCheck.reviewViewport.YOffset()) },
		rows: 3,
	},
	"setup wizard": {
		prepare: func(t *testing.T, a *App) {
			a.setupWiz.mode = setupModeSimple
			a.setupWiz.simpleStage = setupSimpleChannels
			a.setupWiz.channelMode = "list"
			a.setupWiz.channels = []config.ChannelConfig{{ID: "UCa"}, {ID: "UCb"}, {ID: "UCc"}}
		},
		pos:  func(a *App) string { return fmt.Sprint(a.setupWiz.channelIndex) },
		rows: 1,
	},
	"action menu": {
		prepare: func(t *testing.T, a *App) {},
		pos:     func(a *App) string { return fmt.Sprint(a.actionMenu.mainList.Index()) },
		clicks:  true,
	},
	"import": {
		prepare: func(t *testing.T, a *App) {
			dir := t.TempDir()
			for _, name := range []string{"a.zip", "b.zip", "c.zip"} {
				if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			a.importDlg.SetSize(a.width, a.height)
			// The picker lists the directory when its read command answers.
			a.importDlg.UpdateComponents(a.importDlg.Open(dir)())
		},
		pos: func(a *App) string { return a.importDlg.View() },
	},
	"chapter import": {
		prepare: func(t *testing.T, a *App) {
			a.chapterDlg.text.SetValue("0:00 One\n1:00 Two\n2:00 Three\n3:00 Four")
			a.chapterDlg.text.MoveToBegin()
		},
		pos:  func(a *App) string { return fmt.Sprint(a.chapterDlg.text.Line()) },
		rows: 1,
	},
	"add video": {
		prepare: func(t *testing.T, a *App) {
			a.addVideo.SetSize(a.width, a.height)
			a.addVideo.SetFormats(&FormatsData{VideoID: "AAAAAAAAAAA", VideoFormats: []VideoFormat{
				{Itag: 1, MimeType: "video/mp4", Height: 1080}, {Itag: 2, MimeType: "video/mp4", Height: 720},
				{Itag: 3, MimeType: "video/mp4", Height: 480}, {Itag: 4, MimeType: "video/mp4", Height: 360},
			}})
			a.addVideo.step = AddStepVideoFormat
			// Leave Auto first, as the first arrow key does: from here a
			// notch is a row.
			a.addVideo.UpdateComponents(tea.KeyPressMsg{Code: tea.KeyDown})
		},
		pos:  func(a *App) string { return fmt.Sprint(a.addVideo.videoTable.Cursor()) },
		rows: 1,
	},
	"trim": {
		prepare: func(t *testing.T, a *App) {
			a.trimDlg.SetTrims([]TrimInfo{{ID: "t1"}, {ID: "t2"}, {ID: "t3"}})
			a.trimDlg.toggleMode()
		},
		pos:  func(a *App) string { return fmt.Sprint(a.trimDlg.selectedTrimIdx) },
		rows: 1,
	},
	"files": {
		prepare: func(t *testing.T, a *App) {
			a.filesDlg.SetSize(a.width, a.height)
			a.filesDlg.SetFiles([]OrphanedFileEntry{
				{Path: "/a/1.ts", RelPath: "1.ts"}, {Path: "/a/2.ts", RelPath: "2.ts"}, {Path: "/a/3.ts", RelPath: "3.ts"},
			})
			a.filesDlg.SetHistory(nil)
		},
		pos:  func(a *App) string { return fmt.Sprint(a.filesDlg.list.Index()) },
		rows: 1,
	},
	"client tokens": {
		prepare: func(t *testing.T, a *App) {
			a.clientTokensDlg.SetSize(a.width, a.height)
			a.clientTokensDlg.SetTokens([]*database.ClientToken{{ID: "t1"}, {ID: "t2"}, {ID: "t3"}})
		},
		pos:  func(a *App) string { return fmt.Sprint(a.clientTokensDlg.list.Index()) },
		rows: 1,
	},
	"job log": {
		prepare: func(t *testing.T, a *App) {
			a.jobLog.SetSize(a.width, a.height)
			a.jobLog.SetLines(strings.Split(longText(300), "\n"))
		},
		pos: func(a *App) string { return a.jobLog.View() },
	},
}

func wheel(a *App, button tea.MouseButton) {
	_, _ = a.handleMouse(tea.MouseWheelMsg{X: a.width / 2, Y: a.height / 2, Button: button})
}

// Every overlay states what the wheel does over it, and the registry agrees:
// a handler exactly where the census says "wheel scrolls".
func TestOverlayWheelCensus(t *testing.T) {
	for _, e := range overlayRegistry {
		want, stated := overlayWheel[e.name]
		if !stated {
			t.Errorf("overlay %q is not in overlayWheel: state %q or %q", e.name, wheelScrolls, wheelInert)
			continue
		}
		if want != wheelScrolls && want != wheelInert {
			t.Errorf("overlay %q: census says %q", e.name, want)
		}
		if got := e.mouse != nil; got != (want == wheelScrolls) {
			t.Errorf("overlay %q: mouse handler set = %v, census says %q", e.name, got, want)
		}
		if _, has := overlayWheelCases[e.name]; has != (want == wheelScrolls) {
			t.Errorf("overlay %q: census says %q, a row in overlayWheelCases = %v", e.name, want, has)
		}
	}
	if len(overlayWheel) != len(overlayRegistry) {
		t.Errorf("overlayWheel has %d rows, the registry %d", len(overlayWheel), len(overlayRegistry))
	}
}

// The wheel does what the census says over every overlay (R-C2): a text
// overlay scrolls three rows a notch, a list or table overlay moves its
// cursor one row, and an inert one does not change. A click over an overlay
// that had no mouse handler before changes nothing, as before.
//
// Mutants: dropping a registry entry's mouse handler; scrolling a list
// overlay three rows; letting a click reach the key path.
func TestOverlayWheel(t *testing.T) {
	for _, c := range overlayCases(t) {
		t.Run(c.name, func(t *testing.T) {
			a := overlayTestApp(t)
			c.open(a)
			if !c.visible(a) {
				t.Fatalf("did not open")
			}
			if overlayWheel[c.name] == wheelInert {
				before := c.view(a)
				wheel(a, tea.MouseWheelDown)
				wheel(a, tea.MouseWheelUp)
				if c.view(a) != before || !c.visible(a) {
					t.Errorf("the wheel changed an overlay the census calls inert")
				}
				return
			}

			w := overlayWheelCases[c.name]
			w.prepare(t, a)
			if !c.visible(a) {
				t.Fatalf("not open after prepare")
			}
			// A list that opens at its end (the job log follows the tail)
			// moves on the way up instead; either direction proves the wheel.
			start := w.pos(a)
			wheel(a, tea.MouseWheelDown)
			down := w.pos(a)
			wheel(a, tea.MouseWheelUp)
			wheel(a, tea.MouseWheelUp)
			up := w.pos(a)
			if down == start && up == down {
				t.Fatalf("the wheel moved nothing (at %q)", start)
			}
			if w.rows > 0 {
				var from, to int
				fmt.Sscan(start, &from)
				fmt.Sscan(down, &to)
				if to-from != w.rows {
					t.Errorf("one notch down moved %d rows, want %d", to-from, w.rows)
				}
			}
			if w.clicks {
				return
			}
			before, view := w.pos(a), c.view(a)
			_, _ = a.handleMouse(tea.MouseClickMsg{X: a.width / 2, Y: a.height / 2, Button: tea.MouseLeft})
			_, _ = a.handleMouse(tea.MouseReleaseMsg{X: a.width / 2, Y: a.height / 2, Button: tea.MouseLeft})
			if w.pos(a) != before || c.view(a) != view || !c.visible(a) {
				t.Errorf("a click changed the overlay")
			}
		})
	}
}

// The wheel stops at the ends of the trim delete list, where the arrow keys
// wrap round.
func TestWheelDoesNotWrapTheTrimList(t *testing.T) {
	a := overlayTestApp(t)
	a.trimDlg.Open("j1", "a job")
	a.trimDlg.SetTrims([]TrimInfo{{ID: "t1"}, {ID: "t2"}})
	a.trimDlg.toggleMode()
	wheel(a, tea.MouseWheelUp)
	if a.trimDlg.selectedTrimIdx != 0 {
		t.Fatalf("a notch up at the top moved to row %d", a.trimDlg.selectedTrimIdx)
	}
	wheel(a, tea.MouseWheelDown)
	wheel(a, tea.MouseWheelDown)
	if a.trimDlg.selectedTrimIdx != 1 {
		t.Fatalf("notches down past the end left row %d", a.trimDlg.selectedTrimIdx)
	}
}

// Outside the list each overlay keeps, the wheel is not an arrow key: it must
// not move between a form's fields or change a value.
func TestWheelIsInertOutsideTheList(t *testing.T) {
	// Add Video's URL step: the arrow keys there belong to the field.
	a := overlayTestApp(t)
	a.addVideo.Open()
	before := a.addVideo.View()
	wheel(a, tea.MouseWheelDown)
	if a.addVideo.View() != before {
		t.Errorf("add video, URL step: the wheel changed the dialog")
	}

	// The wizard's mode menu.
	a = overlayTestApp(t)
	a.setupWiz.Open()
	choice := a.setupWiz.modeChoice
	wheel(a, tea.MouseWheelDown)
	if a.setupWiz.modeChoice != choice {
		t.Errorf("setup wizard, mode menu: the wheel moved the choice")
	}

	// Chapter import with the offset field focused.
	a = overlayTestApp(t)
	a.chapterDlg.Open(a.taskList.GetJobByID("j1"))
	a.chapterDlg.text.SetValue("0:00 One\n1:00 Two")
	_, _ = a.handleKey(tea.KeyPressMsg{Code: tea.KeyTab})
	line := a.chapterDlg.text.Line()
	wheel(a, tea.MouseWheelUp)
	if a.chapterDlg.text.Line() != line {
		t.Errorf("chapter import, offset focused: the wheel moved the paste cursor")
	}

	// The trim dialog's create step.
	a = overlayTestApp(t)
	a.trimDlg.Open("j1", "a job")
	a.trimDlg.SetTrims([]TrimInfo{{ID: "t1"}, {ID: "t2"}})
	before = a.trimDlg.View()
	wheel(a, tea.MouseWheelDown)
	if a.trimDlg.View() != before {
		t.Errorf("trim, create step: the wheel changed the dialog")
	}
}
