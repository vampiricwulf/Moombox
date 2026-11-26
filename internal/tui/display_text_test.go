package tui

import (
	"bytes"
	"strings"
	"testing"

	"charm.land/bubbles/v2/list"
	"github.com/charmbracelet/x/ansi"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// A title or channel name can carry control characters (an imported title
// is stored as the client sent it). Every TUI site that draws one takes it
// through displayText: a newline must not break a one-line row in two, a tab
// must not draw wider than it was measured, and an escape must not carry a
// hyperlink into the frame.
const hostileTitle = "Stream\npart two\twith a tab \x1b]8;;https://example.test\x1b\\link\x07end"

func assertNoControl(t *testing.T, where, s string) {
	t.Helper()
	// Styling the frame adds its own SGR escapes, so ESC itself is allowed;
	// the title's hyperlink and its other control characters are not.
	if strings.ContainsAny(s, "\n\t\x07") {
		t.Errorf("%s: a control character reached the screen: %q", where, s)
	}
	if strings.Contains(s, "\x1b]8;;https://example.test") {
		t.Errorf("%s: the title's hyperlink reached the screen: %q", where, s)
	}
}

// Mutant: renderJob or resetMarquee taking job.Title raw again.
func TestNormalRowDrawsATitlesControlCharactersAsSpaces(t *testing.T) {
	for _, tc := range []struct {
		name  string
		title string
	}{
		{"short, unselected", hostileTitle},
		{"long, on the scrolling marquee", hostileTitle + strings.Repeat(" more words", 20)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewTaskListModel()
			m.SetSize(80, 10)
			job := &database.Job{ID: "j", Title: tc.title, Status: database.StatusDownloading, Platform: "youtube", CreatedAt: "2026-11-25T10:00:00Z"}
			m.SetJobs([]*database.Job{job})
			m.resetMarquee()
			for _, selected := range []bool{false, true} {
				row := m.renderJob(job, selected, false)
				assertNoControl(t, tc.name, row)
				if strings.Contains(row, "\n") {
					t.Errorf("selected=%v: the row took more than one line: %q", selected, row)
				}
				if w := ansi.StringWidth(row); w > 78 {
					t.Errorf("selected=%v: row width %d, wider than the list", selected, w)
				}
			}
			if !strings.Contains(tc.name, "marquee") {
				return
			}
			if !m.marquee.NeedsScroll() {
				t.Fatal("premise lost: the long title does not scroll")
			}
		})
	}
}

// Mutant: the details pane's Title or Channel field taking the stored value.
func TestDetailsPaneDrawsTitleAndChannelControlCharactersAsSpaces(t *testing.T) {
	m := NewJobDetailsModel()
	m.SetSize(80, 30)
	m.SetJob(&database.Job{ID: "j", Title: hostileTitle, ChannelName: "Chan\nnel\x1b[31m", Status: database.StatusFinished, Platform: "youtube"})
	view := m.View()
	// The pane has many lines of its own; the title's newline is the one
	// that must not be there.
	assertNoControl(t, "details", strings.ReplaceAll(view, "\n", " "))
	if strings.Contains(ansi.Strip(view), "Stream\npart") {
		t.Error("the title's newline broke the Title field")
	}
	if !strings.Contains(ansi.Strip(view), "Chan nel") {
		t.Errorf("the channel field is not drawn with the newline as a space:\n%s", ansi.Strip(view))
	}
	if strings.Contains(view, "\x1b[31m") {
		t.Error("the channel name's own escape reached the frame")
	}
}

// The feedback and confirm lines quote the title: one line, nothing raw.
// Mutant: the A X arm, the confirm or the Job Log header taking job.Title.
func TestFeedbackConfirmAndLogHeaderQuoteTheTitleClean(t *testing.T) {
	job := &database.Job{ID: "j", Title: hostileTitle, Status: database.StatusDownloading, Platform: "youtube", CreatedAt: "2026-11-25T10:00:00Z"}

	a := NewApp()
	a.OnCancelJob = func(string) (bool, error) { return true, nil }
	a.dispatchAction("A X", job)
	assertNoControl(t, "cancel feedback", a.feedback.msg)

	a = NewApp()
	a.OnCancelJob = func(string) (bool, error) { return true, nil }
	a.taskList.SetJobs([]*database.Job{job})
	pressRune(a, 'a')
	pressRune(a, 'x')
	if !strings.HasPrefix(a.feedback.msg, "Press X to confirm") {
		t.Fatalf("premise lost: no confirm prompt, line %q", a.feedback.msg)
	}
	assertNoControl(t, "confirm", a.feedback.msg)

	l := NewJobLogModel()
	l.SetSize(80, 24)
	l.Open(job)
	assertNoControl(t, "job log header", l.log.title)
}

// The action menu's job rows and its refusal lines quote the title too.
// Mutant: menuJobDelegate.Render or menuRefusal taking job.Title raw.
func TestActionMenuJobRowAndRefusalQuoteTheTitleClean(t *testing.T) {
	job := &database.Job{ID: "j", Title: hostileTitle, Status: database.StatusDownloading}
	l := list.New([]list.Item{menuJobItem{job: job}}, menuJobDelegate{}, 80, 10)
	var buf bytes.Buffer
	menuJobDelegate{}.Render(&buf, l, 0, menuJobItem{job: job})
	assertNoControl(t, "menu job row", buf.String())
	if strings.Contains(buf.String(), "\n") {
		t.Errorf("the menu row took more than one line: %q", buf.String())
	}

	item := &ActionMenuItem{Chord: "A X", Label: "Cancel Job", HintLabel: "Cancel"}
	assertNoControl(t, "refusal, status changed", menuRefusal(item, "A X", nil, job))
	assertNoControl(t, "refusal, filter", menuRefusal(item, "A X", job, job))
}
