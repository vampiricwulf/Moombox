package tui

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestReinitializeIsConfirmGatedAndNamesWhatItDeletes pins owner decision
// PR-M1 in the TUI: A I no longer reinitializes on two keypresses. "a i"
// arms a confirm whose line names what the reset deletes, from the
// ReinitializeSummary callback for the job the prompt names; only the third
// "i" runs OnReinitializeJob. A batch asks the callback once for the jobs
// it will act on, and its line names their totals.
//
// Mutants: NeedsConfirm dropped from A I (the second key already
// reinitializes); ConfirmDetail not wired to ReinitializeSummary, or
// confirmDetail not appended (the line lacks "deletes"); the batch passing
// the whole selection rather than the eligible jobs (the Finished one is
// asked about).
func TestReinitializeIsConfirmGatedAndNamesWhatItDeletes(t *testing.T) {
	// Resume and Reinitialize run as a command (tui-5): the key's command is
	// run and its answer delivered, as the program would.
	press := func(app *App, r rune) {
		pressApply(app, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	app := NewApp()
	var reinit []string
	var asked [][]string
	app.OnReinitializeJob = func(id string) error { reinit = append(reinit, id); return nil }
	app.ReinitializeSummary = func(ids []string) string {
		asked = append(asked, append([]string(nil), ids...))
		return "deletes 2 set-aside recordings"
	}
	app.taskList.SetJobs([]*database.Job{
		{ID: "a", Title: "Alpha", Status: database.StatusError, CreatedAt: "2026-10-04T10:00:00Z"},
		{ID: "b", Title: "Beta", Status: database.StatusCancelled, CreatedAt: "2026-10-04T09:00:00Z"},
		{ID: "c", Title: "Gamma", Status: database.StatusFinished, CreatedAt: "2026-10-04T08:00:00Z"},
	})

	press(app, 'a')
	press(app, 'i')
	if len(reinit) != 0 {
		t.Fatalf("A I reinitialized %v without a confirm", reinit)
	}
	if !strings.Contains(app.feedback.msg, `Press I to confirm reinit: deletes 2 set-aside recordings — "Alpha" (3s)`) {
		t.Errorf("confirm line = %q, want it to name what the reset deletes", app.feedback.msg)
	}
	if len(asked) != 1 || len(asked[0]) != 1 || asked[0][0] != "a" {
		t.Errorf("summary asked for %v, want [[a]]", asked)
	}
	press(app, 'i')
	if len(reinit) != 1 || reinit[0] != "a" {
		t.Fatalf("A I I reinitialized %v, want [a]", reinit)
	}

	// A batch: the line names the totals of the jobs it will act on.
	reinit, asked = nil, nil
	for _, id := range []string{"a", "b", "c"} {
		app.taskList.ToggleSelection(id)
	}
	press(app, 'a')
	press(app, 'i')
	if len(reinit) != 0 {
		t.Fatalf("batch A I reinitialized %v without a confirm", reinit)
	}
	if !strings.Contains(app.feedback.msg, "Press I to confirm reinit 2 jobs: deletes 2 set-aside recordings (3s)") {
		t.Errorf("batch confirm line = %q", app.feedback.msg)
	}
	if len(asked) == 1 {
		slices.Sort(asked[0])
	}
	if len(asked) != 1 || strings.Join(asked[0], ",") != "a,b" {
		t.Errorf("batch summary asked for %v, want the two reinitializable jobs", asked)
	}
	press(app, 'i')
	slices.Sort(reinit)
	if strings.Join(reinit, ",") != "a,b" {
		t.Errorf("batch A I I reinitialized %v, want a and b", reinit)
	}
}

// TestActionMenuReinitializeFooterNamesWhatItDeletes: the menu's own confirm
// for A I — the second Enter on the job — says what the reset deletes, from
// the same ConfirmDetail, asked once the first Enter armed it.
//
// Mutant: the footer not carrying confirmDetail.
func TestActionMenuReinitializeFooterNamesWhatItDeletes(t *testing.T) {
	m := NewActionMenuModel()
	m.SetSize(120, 40)
	job := &database.Job{ID: "a", Title: "Alpha", Status: database.StatusError}
	m.SetJobs([]*database.Job{job})
	m.Open([]ActionMenuItem{{Chord: "A I", Label: "Reinitialize Job", Category: "Action", NeedsJob: true, NeedsConfirm: true,
		ConfirmDetail: func(ids []string) string { return "deletes 3 recorded parts (" + strings.Join(ids, ",") + ")" }}})
	m.skipMainForward()
	m.HandleKey(keyEnter) // pick the action
	m.HandleKey(keyEnter) // pick the job: arms the confirm
	if !m.jobConfirm {
		t.Fatal("premise lost: the first Enter on the job did not arm the confirm")
	}
	// The footer wraps inside the box: read its words in order.
	words := strings.Join(strings.Fields(strings.ReplaceAll(ansi.Strip(m.View()), "│", " ")), " ")
	if v := words; !strings.Contains(v, "deletes 3 recorded parts (a)") {
		t.Errorf("menu confirm footer lacks what the reset deletes:\n%s", v)
	}
}

// TestReinitializeConfirmKeepsTheCancelWarningOnANarrowTerminal: the A I
// confirm line is one row, cut to the terminal's width, so whatever sits at
// its end is the first thing a narrow terminal drops. The warning that the
// reset cancels a pending automatic mux (owner ruling R-D2) leads the clause
// (ReinitializePreview.Summary), and the clause leads the job's title, so a
// long title on an 80-column terminal still shows it.
//
// Mutant: the clause back after the title (or the warning back after the
// deletes list) — "cancels" is cut off.
func TestReinitializeConfirmKeepsTheCancelWarningOnANarrowTerminal(t *testing.T) {
	app := NewApp()
	app.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	app.ReinitializeSummary = func([]string) string {
		return "cancels the pending automatic mux, and deletes 40 staged files (850.0MB)"
	}
	title := "StreamerX — " + strings.Repeat("a very long broadcast title ", 3)
	app.taskList.SetJobs([]*database.Job{
		{ID: "tw_m", Title: title, Platform: "twitch", Status: database.StatusError, CreatedAt: "2026-10-04T10:00:00Z"},
	})
	app.handleKey(tea.KeyPressMsg{Code: 'a', Text: "a"})
	app.handleKey(tea.KeyPressMsg{Code: 'i', Text: "i"})
	if !strings.Contains(app.feedback.msg, "cancels the pending automatic mux") {
		t.Fatalf("premise: confirm line = %q", app.feedback.msg)
	}
	if v := ansi.Strip(app.View().Content); !strings.Contains(v, "cancels the pending automatic mux") {
		t.Errorf("the 80-column view lost the cancel warning; confirm line = %q", app.feedback.msg)
	}
}

// TestActionMenuReinitializeFooterKeepsTheCancelWarning: the menu's confirm
// footer at 80 columns still carries the R-D2 warning, which leads the
// summary.
func TestActionMenuReinitializeFooterKeepsTheCancelWarning(t *testing.T) {
	m := NewActionMenuModel()
	m.SetSize(80, 30)
	job := &database.Job{ID: "tw_m", Title: strings.Repeat("a very long broadcast title ", 3), Platform: "twitch", Status: database.StatusError}
	m.SetJobs([]*database.Job{job})
	m.Open([]ActionMenuItem{{Chord: "A I", Label: "Reinitialize Job", Category: "Action", NeedsJob: true, NeedsConfirm: true,
		ConfirmDetail: func([]string) string {
			return "cancels the pending automatic mux, and deletes 2 set-aside recordings, 3 recorded parts (1.2GB) and 40 staged files (850.0MB)"
		}}})
	m.skipMainForward()
	m.HandleKey(keyEnter)
	m.HandleKey(keyEnter)
	words := strings.Join(strings.Fields(strings.ReplaceAll(ansi.Strip(m.View()), "│", " ")), " ")
	if !strings.Contains(words, "cancels the pending automatic mux") {
		t.Errorf("menu confirm footer at 80 columns lost the cancel warning:\n%s", words)
	}
}
