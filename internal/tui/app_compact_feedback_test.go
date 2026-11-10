package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// frameShows reports whether every word of msg is on screen, in order, on
// the frame's bottom rows: the feedback line, wrapped or not, with nothing
// cut off it. A " | " a row break took the place of is not looked for.
func frameShows(a *App, msg string) bool {
	words := func(s string) string {
		return strings.Join(strings.Fields(strings.ReplaceAll(s, " | ", " ")), " ")
	}
	lines := strings.Split(plainFrame(a), "\n")
	want := words(msg)
	for top := len(lines) - 1; top >= 0; top-- {
		if words(strings.Join(lines[top:], " ")) == want {
			return true
		}
	}
	return false
}

// A feedback line too long for one compact row wraps upward and is shown
// whole. A I's confirm names what the reset deletes (owner rulings PR-M1,
// R-D2), and that clause is on screen at 40 columns and at the 30x6 floor.
func TestCompactFeedbackWraps(t *testing.T) {
	for _, size := range [][2]int{{30, 6}, {40, 10}, {59, 19}} {
		r := newCompactRig(t, size[0], size[1])
		a := r.app
		r.selectJob(t, "err")
		r.press("a", "i")
		msg := a.feedback.msg
		if !strings.HasPrefix(msg, "Press I to confirm reinit") || !strings.Contains(msg, "(3s)") {
			t.Fatalf("%v: premise lost: no reinit confirm: %q", size, msg)
		}
		if size[0] <= 40 && ansi.StringWidth(msg) <= size[0] {
			t.Fatalf("%v: premise lost: the confirm fits one row: %q", size, msg)
		}
		if !frameShows(a, msg) {
			t.Errorf("%v: the reinit confirm is cut:\n%s", size, plainFrame(a))
		}
		for i, line := range frameLines(a) {
			if w := ansi.StringWidth(line); w != size[0] || i >= size[1] {
				t.Errorf("%v: row %d is %d cells", size, i, w)
			}
		}

		// A line its writer asked to wrap, and one it did not.
		long := "Cookies: YouTube OK | Last cookie error: the profile held no session at all"
		a.setWrappedFeedback(long, severityWarning)
		if !frameShows(a, long) {
			t.Errorf("%v: a wrapped line is cut:\n%s", size, plainFrame(a))
		}
		a.setFeedback("Deleted: \"A stream with a title of some length, and more\" (3s)")
		if !frameShows(a, a.feedback.msg) {
			t.Errorf("%v: a long plain line is cut:\n%s", size, plainFrame(a))
		}
		// A short line is one row, at the left edge, as before.
		a.setFeedback("Checking all monitors now…")
		lines := strings.Split(plainFrame(a), "\n")
		if lines[len(lines)-1] != "Checking all monitors now…" || !strings.Contains(lines[len(lines)-3], "───") {
			t.Errorf("%v: a short line is not one row under the status line:\n%s", size, plainFrame(a))
		}
		// Past half the terminal the tail is cut with an ellipsis, and the
		// top half of the screen stays.
		a.setFeedback(strings.Repeat("word ", 400))
		lines = strings.Split(plainFrame(a), "\n")
		if !strings.HasSuffix(lines[len(lines)-1], "…") || strings.HasPrefix(lines[size[1]-size[1]/2-1], "word") ||
			!strings.HasPrefix(lines[size[1]-size[1]/2], "word") {
			t.Errorf("%v: a runaway line is not capped at half the terminal:\n%s", size, plainFrame(a))
		}
	}
}

// hintKeys reads the keys a prefix hint offers, from either form: "Action: R
// Resume | D Delete (3s)" or "Action: R D (3s)".
func hintKeys(hint string) []string {
	_, list, _ := strings.Cut(strings.TrimSuffix(hint, " (3s)"), ": ")
	fields := strings.Fields(list)
	keysOnly := !strings.Contains(list, " | ")
	for _, f := range fields {
		if len(f) != 1 {
			keysOnly = false
		}
	}
	if keysOnly {
		return fields
	}
	var keys []string
	for _, seg := range strings.Split(list, " | ") {
		keys = append(keys, strings.Fields(seg)[0])
	}
	return keys
}

// A prefix hint is the only list of the working chords in the compact
// layout, so every key it offers is on screen at every compact size: with
// its label where that fits, alone where it does not. A prefix whose chords
// all open overlays answers with the size refusal and arms nothing.
func TestCompactPrefixHintsShowEveryKey(t *testing.T) {
	checked := 0
	for _, size := range [][2]int{{30, 6}, {40, 10}, {59, 19}} {
		for _, job := range compactJobs() {
			for _, prefix := range []string{"a", "r", "o"} {
				// The keys a full-size terminal offers that are not
				// refused at this size (chordCompact).
				full := newCompactRig(t, 120, 40)
				full.selectJob(t, job.ID)
				fullHint := full.app.chordFeedback(prefix)
				var want []string
				for _, item := range full.app.buildMenuItems() {
					if !strings.HasPrefix(item.Chord, strings.ToUpper(prefix)+" ") {
						continue
					}
					if refused, _, _ := chordRefusedAt(item.Chord, size[0], size[1]); refused {
						continue
					}
					if strings.Contains(fullHint, " "+item.Chord[2:]+" "+item.HintLabel) {
						want = append(want, item.Chord[2:])
					}
				}
				if len(want) == 0 {
					continue
				}

				r := newCompactRig(t, size[0], size[1])
				r.selectJob(t, job.ID)
				r.press(prefix)
				msg := r.app.feedback.msg
				if !frameShows(r.app, msg) {
					t.Errorf("%v %s %s: the hint is cut:\n%s", size, job.ID, prefix, plainFrame(r.app))
				}
				if got := hintKeys(msg); strings.Join(got, " ") != strings.Join(want, " ") {
					t.Errorf("%v %s %s: the hint offers %v, want %v (%q)", size, job.ID, prefix, got, want, msg)
				}
				for i, line := range frameLines(r.app) {
					if w := ansi.StringWidth(line); w != size[0] || i >= size[1] {
						t.Errorf("%v %s %s: row %d is %d cells", size, job.ID, prefix, i, w)
					}
				}
				checked++
			}
		}
	}
	if checked < 40 {
		t.Errorf("only %d hints were checked", checked)
	}

	// At the floor the Request hint is too long with its labels.
	r := newCompactRig(t, 30, 6)
	r.press("r")
	if got := r.app.feedback.msg; got != "Request: B C F V M N U S P (3s)" {
		t.Errorf("the Request hint at 30x6 = %q", got)
	}
	// With room for them the labels stay, and no key is parted from its
	// label by a row break.
	r = newCompactRig(t, 40, 10)
	r.press("r")
	if !strings.Contains(r.app.feedback.msg, "B Backfill | C Cookies") {
		t.Errorf("the Request hint at 40x10 = %q", r.app.feedback.msg)
	}
	for _, line := range strings.Split(plainFrame(r.app), "\n") {
		if f := strings.Fields(line); len(f) > 0 && len(f[len(f)-1]) == 1 && f[len(f)-1] != "•" {
			t.Errorf("a row of the hint ends on a bare key: %q", line)
		}
	}

	// Every E chord opens an overlay: the prefix answers with the refusal,
	// and so does its second key.
	r = newCompactRig(t, 40, 10)
	r.press("e")
	if r.app.feedback.msg != compactRefusal(60, 20) || r.app.chord.prefix != "e" {
		t.Errorf("E in the compact layout: feedback %q, armed prefix %q", r.app.feedback.msg, r.app.chord.prefix)
	}
	r.press("t")
	if r.app.feedback.msg != compactRefusal(60, 20) || r.app.chord.prefix != "" || r.app.hasActiveOverlay() {
		t.Errorf("E T in the compact layout: feedback %q, armed prefix %q", r.app.feedback.msg, r.app.chord.prefix)
	}
}

// A prefix armed at full size has its hint rewritten when the terminal
// shrinks into the compact layout: it no longer offers the overlay chords
// that are refused there.
func TestAnArmedPrefixHintFollowsTheLayout(t *testing.T) {
	r := newCompactRig(t, 120, 40)
	a := r.app
	r.selectJob(t, "done")
	r.press("a")
	if !strings.Contains(a.feedback.msg, "A Add") {
		t.Fatalf("premise lost: the full-size hint is %q", a.feedback.msg)
	}
	until := a.feedback.until
	a.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	if strings.Contains(a.feedback.msg, "A Add") || !strings.Contains(a.feedback.msg, "D Delete") {
		t.Errorf("the hint was not rewritten for the compact layout: %q", a.feedback.msg)
	}
	if a.chord.prefix != "a" || !a.feedback.until.Equal(until) {
		t.Error("the rewrite disarmed the prefix or restarted the line's hold")
	}
	a.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if !strings.Contains(a.feedback.msg, "A Add") {
		t.Errorf("the hint was not rewritten on growing back: %q", a.feedback.msg)
	}

	// Feedback that replaced the hint is left alone.
	a.setFeedback("Checking all monitors now…")
	a.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	if a.feedback.msg != "Checking all monitors now…" {
		t.Errorf("a resize overwrote unrelated feedback: %q", a.feedback.msg)
	}

	// E armed at full size has nothing left below it.
	r = newCompactRig(t, 120, 40)
	r.press("e")
	if r.app.chord.prefix != "e" {
		t.Fatal("premise lost: E did not arm")
	}
	r.app.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	if r.app.feedback.msg != compactRefusal(60, 20) || r.app.chord.prefix != "e" {
		t.Errorf("E across a shrink: feedback %q, armed prefix %q", r.app.feedback.msg, r.app.chord.prefix)
	}
	r.app.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if !strings.HasPrefix(r.app.feedback.msg, "Extras: ") {
		t.Errorf("E on growing back: feedback %q", r.app.feedback.msg)
	}
}
