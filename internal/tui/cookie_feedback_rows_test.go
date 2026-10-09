package tui

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// heldProfileErr is the refusal singletonLockHolder
// (internal/cookies/autocookies_chromium.go) gives for a SingletonLock that
// names another machine, word for word: it is the sentence both R C's Last
// cookie error and R F's failure carry, and the longest one either is handed.
func heldProfileErr(lock string) error {
	return fmt.Errorf("%w by %s — a browser on that machine (pid %d there) holds its lock; close it there, or delete %q if no browser on %s is using that profile, and the next pass will run",
		cookies.ErrProfileInUse, "desktop-pc", 4242, lock, "desktop-pc")
}

// drawnFrame renders one result the way the operator sees it — through the
// real Update path, at a real terminal size — and returns the frame's rows,
// failing the test unless the frame keeps exactly the terminal's height and
// no row is wider than it.
func drawnFrame(t *testing.T, width, height int, yt, tw bool, msg tea.Msg) []string {
	t.Helper()
	app := NewApp()
	app.width, app.height = width, height
	app.recalcLayout()
	app.statusBar.SetActivePlatforms(yt, tw)
	app.Update(msg)
	frame := strings.Split(app.View().Content, "\n")
	if len(frame) != height {
		t.Fatalf("%dx%d: the frame is %d rows — the feedback block must be written over it, never added to it",
			width, height, len(frame))
	}
	for _, row := range frame {
		if w := ansi.StringWidth(row); w > width {
			t.Fatalf("%dx%d: a %d-cell row: %q", width, height, w, stripANSI(row))
		}
	}
	for i := range frame {
		frame[i] = strings.TrimSpace(stripANSI(frame[i]))
	}
	return frame
}

// TestHeldProfileLockPathIsDrawnWhole: a refresh skipped because another
// machine's browser holds the profile names the lock to delete, and the TUI
// never showed it. R C's line and R F's failure were one row cut to the
// terminal's width, and the path starts about 175 columns into R C's line
// (150 into R F's), so at 80, 120, 160 and even 200 columns the row ended in
// an ellipsis before or inside it. Both now wrap onto the rows above the status
// bar: the quoted path is drawn whole on one row, the whole sentence is on
// screen, and the block stays up 3 s per row it takes.
//
// 60x20 is the smallest frame the TUI draws (minTermWidth, minTermHeight),
// where the R C block needs 7 rows: more than a third of the terminal, which
// is why the cap is half.
//
// Mutants (checked): setWrappedFeedback leaving wrap false, or View ignoring
// it — every leg loses the path; R C back on setFeedbackWithSeverity, or R F's
// error arm back on setFeedback — that leg loses it; wrapFeedback breaking at
// hyphens as ansi.Wrap does — at 60 columns the path splits at
// "browser-profile"; feedbackRowCap at height/3 — at 60x20 the cap eats the
// tail; the hold left at one row's 3 s — the hold assertion fails.
func TestHeldProfileLockPathIsDrawnWhole(t *testing.T) {
	const lock = "/home/brandon/moombox/browser-profile/SingletonLock"
	held := heldProfileErr(lock)
	quoted := strconv.Quote(lock)

	for _, size := range []struct{ w, h int }{{60, 20}, {80, 24}, {120, 30}, {160, 40}, {200, 50}} {
		for _, leg := range []struct {
			name string
			msg  tea.Msg
			// sentence is the whole line the leg's composer writes, which
			// the block has to show end to end.
			sentence string
		}{
			{"R C", cookieRecheckResultMsg{
				YouTube: cookies.RefreshOK, Twitch: cookies.RefreshOK, LastError: held.Error(),
			}, "Cookies: YouTube OK, Twitch OK | Last cookie error: " + held.Error()},
			{"R F", cookieForceRefreshResultMsg{Err: held},
				"Browser cookie refresh failed: " + held.Error()},
		} {
			t.Run(fmt.Sprintf("%s/%dx%d", leg.name, size.w, size.h), func(t *testing.T) {
				frame := drawnFrame(t, size.w, size.h, true, true, leg.msg)
				onOneRow := false
				for _, row := range frame {
					onOneRow = onOneRow || strings.Contains(row, quoted)
				}
				if !onOneRow {
					t.Errorf("no row carries the lock's quoted path %s whole — the file the "+
						"operator is told to delete:\n%s", quoted, strings.Join(frame, "\n"))
				}
				// The block's rows were broken at single spaces, so the
				// frame's rows rejoined by one give the sentence back
				// verbatim — the remedy's tail included.
				if !strings.Contains(strings.Join(frame, " "), leg.sentence) {
					t.Errorf("the whole line is not on screen; want %q in:\n%s",
						leg.sentence, strings.Join(frame, "\n"))
				}
			})
		}
	}

	t.Run("held 3 s per row", func(t *testing.T) {
		app := NewApp()
		app.width, app.height = 80, 24
		app.recalcLayout()
		app.statusBar.SetActivePlatforms(true, true)
		app.Update(cookieRecheckResultMsg{
			YouTube: cookies.RefreshOK, Twitch: cookies.RefreshOK, LastError: held.Error(),
		})
		frame := strings.Split(stripANSI(app.View().Content), "\n")
		rows := 0
		for i, row := range frame {
			if strings.Contains(row, "Cookies: YouTube OK") {
				rows = len(frame) - 1 - i // down to the status bar
				break
			}
		}
		if rows < 2 {
			t.Fatalf("the R C block took %d rows at 80 columns, want several:\n%s",
				rows, strings.Join(frame, "\n"))
		}
		// A second's slack for the time between Update and this read.
		if got, want := time.Until(app.feedback.until), time.Duration(rows)*feedbackRowHold-time.Second; got < want {
			t.Errorf("a %d-row block is held %v, want about %v — 3 s is one row's time, "+
				"not long enough to read a path and copy it", rows, got.Round(time.Second),
				time.Duration(rows)*feedbackRowHold)
		}
	})
}

// TestWrappedFeedbackStopsAtItsCap: a wrapped line is laid out on at most
// feedbackRowCap rows, each inside the width, the last ending in an ellipsis
// when the line runs past them; a word wider than a row is broken where the
// row ends rather than run off the edge; and with no width yet the line is
// one row, whole.
//
// Mutants (checked): dropping the cap — ten rows come back; dropping the
// ellipsis on the capped row — nothing says the line goes on; dropping the
// overlong-word break — a row is wider than the room.
func TestWrappedFeedbackStopsAtItsCap(t *testing.T) {
	long := strings.Repeat("word ", 100) + "end"
	rows := wrapFeedback(long, 60, 4)
	if len(rows) != 4 {
		t.Fatalf("%d rows, want the cap of 4: %q", len(rows), rows)
	}
	for _, r := range rows {
		if w := ansi.StringWidth(r); w > 60-feedbackIndent {
			t.Errorf("a %d-cell row in a %d-cell room: %q", w, 60-feedbackIndent, r)
		}
	}
	if !strings.HasSuffix(rows[3], "…") {
		t.Errorf("the capped row %q does not end in an ellipsis — nothing says the line goes on", rows[3])
	}

	deep := `delete "/` + strings.Repeat("very/deep/", 12) + `SingletonLock"`
	for _, r := range wrapFeedback(deep, 40, 6) {
		if w := ansi.StringWidth(r); w > 40-feedbackIndent {
			t.Errorf("a %d-cell row in a %d-cell room: %q", w, 40-feedbackIndent, r)
		}
	}
	if got := strings.Join(wrapFeedback(deep, 40, 6), ""); got != strings.ReplaceAll(deep, " ", "") {
		t.Errorf("the overlong path broken across rows lost characters: %q", got)
	}

	if got := wrapFeedback(long, 0, 4); len(got) != 1 || got[0] != long {
		t.Errorf("with no width the line came back as %q, want it whole on one row", got)
	}
}
