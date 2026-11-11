package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestBareJobLogFrames(t *testing.T) {
	m := bareJobLog(40, 10)
	wantFrame(t, "following at 40x10", m.View(), 40, 10, []string{
		"Karaoke stream part two of … (57) [100%]",
		"12:01:51 INFO  segment 1251 written",
		"12:01:52 WARN  segment 1252 403,",
		"refreshing credentials",
		"12:01:53 INFO  token refreshed, resuming",
		"12:01:54 INFO  segment 1254 written",
		"12:01:55 WARN  segment 1255 403,",
		"refreshing credentials",
		"12:01:56 INFO  token refreshed, resuming",
		" ↑/↓ PgUp/PgDn · / find · Esc close",
	})
	m.testKeys("up", "up")
	wantFrame(t, "paused at 40x10", m.View(), 40, 10, []string{
		"Karaoke stream par… (57) [PAUSED] [97%]",
		"refreshing credentials",
		"12:01:50 INFO  token refreshed, resuming",
		"12:01:51 INFO  segment 1251 written",
		"12:01:52 WARN  segment 1252 403,",
		"refreshing credentials",
		"12:01:53 INFO  token refreshed, resuming",
		"12:01:54 INFO  segment 1254 written",
		"12:01:55 WARN  segment 1255 403,",
		" End follow · / find · Esc close",
	})

	m = bareJobLog(30, 6)
	wantFrame(t, "following at 30x6", m.View(), 30, 6, []string{
		"Karaoke stream pa… (57) [100%]",
		"12:01:55 WARN  segment 1255",
		"403, refreshing credentials",
		"12:01:56 INFO  token",
		"refreshed, resuming",
		" / find · Esc close",
	})
	m.testKeys("up", "up")
	m.testKeys("/", "4", "0", "3")
	wantFrame(t, "typing a search at 30x6", m.View(), 30, 6, []string{
		"Karaoke stream … (57) [PAUSED]",
		"12:01:54 INFO  segment 1254",
		"written",
		"12:01:55 WARN  segment 1255",
		"403, refreshing credentials",
		"/403",
	})
	m.testKeys("enter")
	wantFrame(t, "a search applied at 30x6", m.View(), 30, 6, []string{
		"Karao… (57) [/4…] (19 matches)",
		"12:01:54 INFO  segment 1254",
		"written",
		"12:01:55 WARN  segment 1255",
		"403, refreshing credentials",
		" n/N · End · Esc clear",
	})
}

// The lines get every row but the header and the last one in every state:
// pausing, typing a search and applying one never change the height, so
// none of them can push a row off a six-row screen.
func TestBareJobLogBodyHeightNeverChanges(t *testing.T) {
	compactSizes(func(w, h int) {
		m := bareJobLog(w, h)
		check := func(state string) {
			t.Helper()
			if got := m.log.viewport.Height(); got != h-2 {
				t.Errorf("%dx%d, %s: the lines have %d rows, want %d", w, h, state, got, h-2)
			}
			if got := m.log.viewport.Width(); got != w {
				t.Errorf("%dx%d, %s: the lines are %d cells wide, want %d", w, h, state, got, w)
			}
		}
		check("following")
		m.testKeys("up")
		check("paused")
		m.testKeys("/", "x")
		check("typing")
		m.testKeys("enter")
		check("search applied")
		m.testKeys("esc", "end")
		check("following again")
	})
}

// The normal form says "End to resume" on a row the bare form does not
// have, so the key line names End whenever the view is paused, and it
// always names what Esc does now.
//
// Mutant: bareFooter falling back to the following rungs while paused.
func TestBareJobLogFooterNamesEndWhilePaused(t *testing.T) {
	compactSizes(func(w, h int) {
		m := bareJobLog(w, h)
		footer := func() string {
			rows := plainRows(m.View())
			return rows[len(rows)-1]
		}
		if f := footer(); !strings.Contains(f, "Esc close") || strings.Contains(f, "End") {
			t.Errorf("%dx%d following: footer %q", w, h, f)
		}
		m.testKeys("up")
		if m.log.autoScroll {
			t.Fatalf("%dx%d: premise lost: Up did not pause the view", w, h)
		}
		if f := footer(); !strings.Contains(f, "End follow") || !strings.Contains(f, "Esc close") {
			t.Errorf("%dx%d paused: footer %q", w, h, f)
		}
		m.testKeys("/", "4", "0", "3", "enter")
		if m.log.autoScroll {
			// The match the search lands on can be on the last page.
			m.testKeys("up")
		}
		if f := footer(); !strings.Contains(f, "End") || !strings.Contains(f, "Esc clear") || !strings.Contains(f, "n/N") {
			t.Errorf("%dx%d paused with a search: footer %q", w, h, f)
		}
		m.testKeys("end")
		if f := footer(); !strings.Contains(f, "Esc clear") || !strings.Contains(f, "Q close") || strings.Contains(f, "End") {
			t.Errorf("%dx%d following with a search: footer %q", w, h, f)
		}
	})
}

// An applied search shows its query and its match count in the header at
// every compact width, "(0 matches)" included: the title gives way, then the
// query, never the count.
//
// Mutant: the normal header's 16-cell title floor — at 30 columns the whole
// suffix is dropped.
func TestBareJobLogHeaderKeepsTheSearch(t *testing.T) {
	for _, w := range []int{30, 31, 36, 40, 59, 200} {
		for _, query := range []string{"403", "nothing matches this", "segment"} {
			m := bareJobLog(w, 8)
			m.testKeys("/")
			for _, r := range query {
				m.testKeys(string(r))
			}
			m.testKeys("enter")
			header := plainRows(m.View())[0]
			if !strings.Contains(header, fmt.Sprintf("] (%d matches)", m.log.matchCount)) || !strings.Contains(header, "[/"+query[:1]) {
				t.Errorf("width %d, query %q: header %q", w, query, header)
			}
			if !strings.Contains(header, "(57)") {
				t.Errorf("width %d, query %q: the line count is gone: %q", w, query, header)
			}
		}
	}
	// The prefix the normal header carries is not in the bare one.
	if header := plainRows(bareJobLog(59, 19).View())[0]; strings.Contains(header, "Job Log") || !strings.HasPrefix(header, "Karaoke stream part two") {
		t.Errorf("header at 59 columns: %q", header)
	}
}

// The bare frame is memoised like the normal one, and every change that
// reaches the screen moves the key: the cached frame equals a fresh one.
func TestBareJobLogCachedFramesMatchFreshFrames(t *testing.T) {
	m := bareJobLog(40, 10)
	check := func(step string) {
		t.Helper()
		cached := m.View()
		m.log.invalidate()
		if fresh := m.View(); cached != fresh {
			t.Errorf("after %s the cached frame is stale:\n%s\nfresh:\n%s", step, ansi.Strip(cached), ansi.Strip(fresh))
		}
	}
	_ = m.View()
	m.testKeys("up")
	check("Up")
	m.SetLines(append(bareLogLines(57), "12:02:00 ERROR one more line"))
	check("a new line")
	m.testKeys("/", "4", "0", "3", "enter")
	check("a search")
	m.testKeys("n")
	check("n")
	m.testKeys("esc")
	check("Esc")
	m.SetSize(30, 6)
	check("a resize")
	m.testKeys("end")
	check("End")
}

// O L at the compact floor, through the App: it opens bare, the wheel and
// the keys reach it, typing a search puts each rune in the box once, and
// Esc leaves the search, then clears it, then closes.
func TestBareJobLogThroughTheApp(t *testing.T) {
	r := newCompactRig(t, 30, 6)
	a := r.app
	a.OnGetJobLogs = func(string) []string { return bareLogLines(57) }
	r.selectJob(t, "done")
	r.press("o")
	_, cmd := a.handleKey(keyMsg("l"))
	if !a.jobLog.IsVisible() || a.screenHeld() {
		t.Fatalf("O L did not open the job log bare at 30x6: feedback %q", a.feedback.msg)
	}
	applyStatsCmd(a, cmd)
	wantExact(t, "the job log at 30x6", a.View().Content, 30, 6)
	if got := plainFrame(a); !strings.Contains(got, "(57)") || !strings.Contains(got, "/ find · Esc close") {
		t.Fatalf("the log did not load:\n%s", got)
	}

	a.Update(tea.MouseWheelMsg{X: 5, Y: 3, Button: tea.MouseWheelUp})
	if a.jobLog.log.autoScroll || !strings.Contains(plainFrame(a), "End follow") {
		t.Errorf("a wheel notch up did not pause the view:\n%s", plainFrame(a))
	}
	r.press("/", "4", "0", "3")
	if got := a.jobLog.log.searchInput.Value(); got != "403" {
		t.Errorf("typed 403, the box holds %q", got)
	}
	if rows := strings.Split(plainFrame(a), "\n"); rows[5] != "/403" {
		t.Errorf("the search box is not the last row: %q", rows)
	}
	r.press("enter")
	if !strings.Contains(plainFrame(a), "matches)") {
		t.Errorf("the applied search is not in the header:\n%s", plainFrame(a))
	}
	r.press("esc")
	if !a.jobLog.IsVisible() || a.jobLog.log.searchQuery != "" {
		t.Error("the first Esc did not clear the search and keep the log open")
	}
	r.press("esc")
	if a.hasActiveOverlay() {
		t.Error("the second Esc did not close the log")
	}
}
