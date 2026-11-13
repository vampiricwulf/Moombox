package tui

import (
	"errors"
	"image/color"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
)

func shownFeedbackColor(a *App) color.Color {
	return feedbackColor(a.feedback.msg, a.feedback.sev)
}

// Lines whose text matches none of feedbackColor's markers state their
// severity where they are written; unstated, each drew in success green
// (tui-6). The text of each is unchanged.
//
// Mutant: any of the five call sites going back to plain setFeedback.
func TestFailureAndPromptLinesStateTheirSeverity(t *testing.T) {
	t.Run("a recovered panic is red", func(t *testing.T) {
		a := NewApp()
		cmd := safeCmd(func() tea.Msg { panic("boom") })
		a.Update(cmd())
		if a.feedback.msg != "unexpected error: boom" {
			t.Fatalf("feedback = %q", a.feedback.msg)
		}
		if got := shownFeedbackColor(a); got != ColorRed {
			t.Errorf("colour = %v, want red", got)
		}
		if got := feedbackColor(a.feedback.msg, severityUnstated); got == ColorRed {
			t.Error("the scan reads this text as an error by itself; the row proves nothing")
		}
	})

	t.Run("the R U confirm is yellow like every other confirm", func(t *testing.T) {
		a := NewApp()
		a.statusMap["a"] = database.StatusDownloading
		a.updateAvailable = &UpdateStatusMsg{TagName: "v9.9.9"}
		a.OnApplyUpdate = func(string) (string, bool) { return "", false }
		if cmd := a.applyUpdateAction(); cmd != nil {
			t.Fatal("the update ran without its second press")
		}
		want := activeRestartClause(1, "update restart") + "; press R U again within 5s to confirm"
		if a.feedback.msg != want {
			t.Fatalf("feedback = %q, want %q", a.feedback.msg, want)
		}
		if got := shownFeedbackColor(a); got != ColorYellow {
			t.Errorf("colour = %v, want yellow", got)
		}
	})

	t.Run("a refresh that could not be confirmed is yellow", func(t *testing.T) {
		a := NewApp()
		a.SetConfigStore(config.NewStore(config.Defaults(), ""))
		a.Update(cookieForceRefreshResultMsg{Result: cookies.RefreshResult{
			Ran: true, Renewed: false, YouTube: cookies.RefreshOK,
			Mechanism: cookies.RefreshMechanismBrowser,
		}})
		if want := "Cookies still work, but this pass could not confirm the browser refreshed them"; a.feedback.msg != want {
			t.Fatalf("feedback = %q, want %q", a.feedback.msg, want)
		}
		if got := shownFeedbackColor(a); got != ColorYellow {
			t.Errorf("colour = %v, want yellow", got)
		}
	})

	t.Run("cookie login and import with no service are yellow", func(t *testing.T) {
		for chord, want := range map[string]string{
			"E L": "Cookie login is unavailable — no auto-cookie service is configured",
			"E I": "Cookie import is unavailable — no auto-cookie service is configured",
		} {
			a := NewApp()
			a.width, a.height = 120, 40
			a.recalcLayout()
			a.dispatchAction(chord, nil)
			if a.feedback.msg != want {
				t.Errorf("%s: feedback = %q, want %q", chord, a.feedback.msg, want)
				continue
			}
			if got := shownFeedbackColor(a); got != ColorYellow {
				t.Errorf("%s: colour = %v, want yellow", chord, got)
			}
		}
	})
}

// The watched line counts jobs the way every other batch line does.
func TestWatchedLineCountsJobsLikeTheOtherBatchLines(t *testing.T) {
	a := NewApp()
	for _, c := range []struct {
		msg  setWatchedResultMsg
		want string
	}{
		{setWatchedResultMsg{Watched: true, Count: 1}, "Marked 1 job watched"},
		{setWatchedResultMsg{Watched: true, Count: 3}, "Marked 3 jobs watched"},
		{setWatchedResultMsg{Watched: false, Count: 2}, "Unmarked 2 jobs watched"},
		{setWatchedResultMsg{Err: errors.New("disk")}, "Watched update failed: disk"},
	} {
		a.Update(c.msg)
		if a.feedback.msg != c.want {
			t.Errorf("feedback = %q, want %q", a.feedback.msg, c.want)
		}
	}
}
