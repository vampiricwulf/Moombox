package tui

import (
	"errors"
	"image/color"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestAdvisoryFeedbackStatesItsSeverity drives the call sites whose wording
// the fallback scan reads as SUCCESS although nothing succeeded: the batch
// arms that found nothing to act on, the chords whose callback is not wired,
// O C on a job with no stream URL, and a refused update skip. Each now states
// its severity, so the colour no longer depends on a substring the sentence
// does not contain. The last row is the control that keeps the fix honest: the
// skip that succeeds is still green.
//
// Mutant: any one site back on plain setFeedback — its row renders green.
func TestAdvisoryFeedbackStatesItsSeverity(t *testing.T) {
	finished := func() *database.Job {
		return &database.Job{ID: "f", Title: "done", Status: database.StatusFinished}
	}
	downloading := func() *database.Job {
		return &database.Job{ID: "d", Title: "live", Status: database.StatusDownloading}
	}
	for _, tc := range []struct {
		name  string
		drive func(*App)
		want  color.Color
	}{
		{"A C with nothing cancellable in the selection", func(a *App) {
			a.OnCancelJob = func(string) bool { return true }
			a.taskList.SetJobs([]*database.Job{finished()})
			a.taskList.ToggleSelection("f")
			a.dispatchAction("A C", nil)
		}, ColorYellow},
		{"A D with nothing deletable in the selection", func(a *App) {
			a.OnDeleteJob = func(string) {}
			a.taskList.SetJobs([]*database.Job{downloading()})
			a.taskList.ToggleSelection("d")
			a.dispatchAction("A D", nil)
		}, ColorYellow},
		{"A W with no callback wired", func(a *App) {
			a.dispatchAction("A W", finished())
		}, ColorYellow},
		{"A W with nothing finished in the selection", func(a *App) {
			a.OnSetWatched = func([]string, bool) error { return nil }
			a.taskList.SetJobs([]*database.Job{downloading()})
			a.taskList.ToggleSelection("d")
			a.dispatchAction("A W", nil)
		}, ColorYellow},
		{"E Y with no status callback", func(a *App) {
			a.dispatchAction("E Y", nil)
		}, ColorYellow},
		{"E T with no stats callback", func(a *App) {
			a.dispatchAction("E T", nil)
		}, ColorYellow},
		{"R N with no fetch callback", func(a *App) {
			a.dispatchAction("R N", nil)
		}, ColorYellow},
		{"O C on a job with no stream URL", func(a *App) {
			a.dispatchAction("O C", &database.Job{ID: "t", Platform: "twitch", VideoID: "123"})
		}, ColorYellow},
		{"a refused skip", func(a *App) {
			a.Update(dismissUpdateResultMsg{Tag: "v1.2.3", Err: errors.New("boom")})
		}, ColorRed},
		{"control: a skip that succeeded", func(a *App) {
			a.Update(dismissUpdateResultMsg{Tag: "v1.2.3"})
		}, ColorGreen},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := NewApp()
			tc.drive(app)
			if app.feedback.msg == "" {
				t.Fatal("premise lost: the path set no feedback line")
			}
			if got := feedbackColor(app.feedback.msg, app.feedback.sev); got != tc.want {
				t.Errorf("%q rendered %v, want %v", app.feedback.msg, got, tc.want)
			}
		})
	}
}
