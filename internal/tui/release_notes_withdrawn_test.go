package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// errNoUpdatePendingForTest stands in for routes.ErrNoUpdatePending, which
// this package does not import.
var errNoUpdatePendingForTest = errors.New("no update pending")

// notesOnPending opens R N's notes beside the pending v9.9.1, as the chord
// does, with S wired to record what it skips.
func notesOnPending(t *testing.T) (*App, *[]string) {
	t.Helper()
	a := NewApp()
	a.width, a.height = 100, 40
	var skipped []string
	a.OnDismissUpdate = func(tag string) error { skipped = append(skipped, tag); return nil }
	a.updateAvailable = &UpdateStatusMsg{Version: "9.9.1", TagName: "v9.9.1", ReleaseNotes: "notes"}
	a.dispatchAction("R N", nil)
	if !a.releaseNotesPopup.pending || !strings.Contains(stripANSI(a.releaseNotesPopup.View()), "U: Apply update") {
		t.Fatal("R N beside the pending v9.9.1 must offer U and S")
	}
	return a, &skipped
}

// assertWithdrawn checks the notes are still open on v9.9.1 with U and S gone
// from the footer, and the overlay and the feedback line both saying why.
func assertWithdrawn(t *testing.T, a *App, notice, line string) {
	t.Helper()
	o := a.releaseNotesPopup
	if !o.isOpen() || o.tag != "v9.9.1" {
		t.Fatalf("the notes must stay open where they were: open=%v tag=%q", o.isOpen(), o.tag)
	}
	view := stripANSI(o.View())
	if o.pending || strings.Contains(view, "U: Apply update") || strings.Contains(view, "S: Skip") {
		t.Errorf("the footer still offers U and S for a release no longer pending:\n%s", view)
	}
	if !strings.Contains(view, notice) {
		t.Errorf("the overlay does not say %q:\n%s", notice, view)
	}
	if a.feedback.msg != line {
		t.Errorf("feedback = %q, want %q", a.feedback.msg, line)
	}
}

// R N's notes beside a pending update kept offering "U: Apply update  S:
// Skip" after their release was skipped elsewhere, pulled or replaced, and
// both keys then did nothing (their tag gate refuses a release other than the
// one pending). The footer now drops them and the overlay — which hides the
// main screen's feedback line — says why, as the feedback line does.
//
// Mutants: followPendingRelease's call dropped from UpdateStatusMsg's clear
// arm (the clear case fails), from its found arm (the replaced case), or from
// updateCheckResultMsg (the pulled case); withdraw leaving pending set (the
// footer keeps U and S); the newer-release branch dropped (the replaced case
// says "elsewhere"); its same-release return dropped (a re-announce of the
// release on screen withdraws it); the View not rendering the notice; close
// not resetting it (the next release's notes open under it).
func TestReleaseNotesDropTheirKeysWhenTheReleaseStopsBeingPending(t *testing.T) {
	t.Run("skipped elsewhere", func(t *testing.T) {
		a, skipped := notesOnPending(t)
		a.Update(UpdateStatusMsg{TagName: "v9.9.1"})
		assertWithdrawn(t, a,
			"No longer pending — skipped, applied or withdrawn elsewhere",
			"v9.9.1 is no longer pending — skipped, applied or withdrawn elsewhere")
		if _, cmd := a.handleKey(tea.KeyPressMsg{Code: 's', Text: "s"}); cmd != nil {
			cmd()
		}
		if len(*skipped) != 0 {
			t.Errorf("S skipped %q with no release pending", *skipped)
		}
		// Closed and opened on the next pending release, the notes offer it.
		a.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
		a.updateAvailable = &UpdateStatusMsg{Version: "9.9.2", TagName: "v9.9.2", ReleaseNotes: "newer"}
		a.dispatchAction("R N", nil)
		if view := stripANSI(a.releaseNotesPopup.View()); strings.Contains(view, "No longer pending") ||
			!strings.Contains(view, "U: Apply update") {
			t.Errorf("the next release's notes kept the withdrawn notice:\n%s", view)
		}
	})
	t.Run("replaced by a newer release", func(t *testing.T) {
		a, _ := notesOnPending(t)
		a.Update(UpdateStatusMsg{Version: "9.9.2", TagName: "v9.9.2", ReleaseNotes: "newer"})
		assertWithdrawn(t, a,
			"No longer pending — v9.9.2 is; close and R N for its notes",
			"v9.9.1 is no longer pending — v9.9.2 is; R N for its notes")
	})
	t.Run("pulled, a check finds", func(t *testing.T) {
		a, _ := notesOnPending(t)
		a.Update(updateCheckResultMsg{Seen: a.updateAvailable})
		assertWithdrawn(t, a,
			"No longer pending — a check found it withdrawn",
			"v9.9.1 is no longer pending — a check found it withdrawn")
	})
	t.Run("the same release announced again", func(t *testing.T) {
		a, _ := notesOnPending(t)
		a.Update(UpdateStatusMsg{Version: "9.9.1", TagName: "v9.9.1", ReleaseNotes: "notes"})
		if !a.releaseNotesPopup.pending || a.releaseNotesPopup.notice != "" {
			t.Error("a re-announce of the release on screen withdrew its notes")
		}
	})
}

// An apply placed while R N had opened the same release's notes again
// withdraws them, and the main line stays the apply's own.
//
// Mutant: followPendingRelease's call dropped from updateApplyResultMsg, or
// moved after its "Update applied" line (the feedback check fails).
func TestReleaseNotesWithdrawnByAnApplyKeepTheApplysLine(t *testing.T) {
	a, _ := notesOnPending(t)
	a.Update(updateApplyResultMsg{})
	view := stripANSI(a.releaseNotesPopup.View())
	if a.releaseNotesPopup.pending || !strings.Contains(view, "No longer pending — applied, restarting") {
		t.Errorf("notes beside an applied release still pending:\n%s", view)
	}
	if a.feedback.msg != "Update applied — restarting" {
		t.Errorf("feedback = %q, want the apply's own line", a.feedback.msg)
	}
}

// The overlay's own S announces the clear before it answers
// (dismissUpdateFromTUI), so the notes hear their own skip first: that is not
// a skip elsewhere, and the answer closes them with its own line. A second S
// while the first is out sends nothing. A skip refused because the release had
// gone elsewhere withdraws the notes once answered.
//
// Mutants: followPendingRelease without its skipping guard, or S not
// setting skipping (the notes say "elsewhere" over their own skip); the S key
// without its once guard (a second skip is sent); the dismiss arm not
// clearing skipping, or its error arm not calling followPendingRelease (the
// refused skip leaves U and S offered); close not clearing skipping (S on the
// next release's notes is refused).
func TestReleaseNotesOwnSkipIsNotASkipElsewhere(t *testing.T) {
	a, skipped := notesOnPending(t)
	_, cmd := a.handleKey(tea.KeyPressMsg{Code: 's', Text: "s"})
	if cmd == nil {
		t.Fatal("S beside the pending release started no skip")
	}
	if _, again := a.handleKey(tea.KeyPressMsg{Code: 's', Text: "s"}); again != nil {
		again()
	}
	a.Update(UpdateStatusMsg{TagName: "v9.9.1"}) // the skip's own announcement
	if !a.releaseNotesPopup.pending || a.releaseNotesPopup.notice != "" {
		t.Error("the notes read their own skip as one from elsewhere")
	}
	if a.feedback.msg != "Skipping v9.9.1..." {
		t.Errorf("feedback = %q, want the skip's own line", a.feedback.msg)
	}
	a.Update(cmd())
	if len(*skipped) != 1 {
		t.Errorf("skips sent = %q, want one", *skipped)
	}
	if a.releaseNotesPopup.isOpen() || !strings.HasPrefix(a.feedback.msg, "Skipped v9.9.1") {
		t.Errorf("the skip's answer: open=%v feedback=%q", a.releaseNotesPopup.isOpen(), a.feedback.msg)
	}

	// Refused: the release went elsewhere while the skip was out, and the
	// refusal's resync cleared it first.
	b, _ := notesOnPending(t)
	b.OnDismissUpdate = func(string) error { return errNoUpdatePendingForTest }
	_, cmd = b.handleKey(tea.KeyPressMsg{Code: 's', Text: "s"})
	b.Update(UpdateStatusMsg{TagName: "v9.9.1"})
	b.Update(cmd())
	o := b.releaseNotesPopup
	if !o.isOpen() || o.pending || !strings.Contains(stripANSI(o.View()), "No longer pending") {
		t.Errorf("a refused skip of a release gone elsewhere left the notes offering it:\n%s", stripANSI(o.View()))
	}
	if !strings.HasPrefix(b.feedback.msg, "Could not skip v9.9.1") {
		t.Errorf("feedback = %q, want the refusal", b.feedback.msg)
	}
	if o.skipping != "" {
		t.Error("the answered skip left the overlay marked as skipping")
	}

	// Closed while its skip is out, the notes opened on another release
	// skip that one.
	c, skippedC := notesOnPending(t)
	if _, cmd = c.handleKey(tea.KeyPressMsg{Code: 's', Text: "s"}); cmd == nil {
		t.Fatal("S started no skip")
	}
	c.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	c.updateAvailable = &UpdateStatusMsg{Version: "9.9.2", TagName: "v9.9.2", ReleaseNotes: "newer"}
	c.dispatchAction("R N", nil)
	_, cmd = c.handleKey(tea.KeyPressMsg{Code: 's', Text: "s"})
	if cmd == nil {
		t.Fatal("S on the next release's notes was refused as a skip still out")
	}
	cmd()
	if got := *skippedC; len(got) != 1 || got[0] != "v9.9.2" {
		t.Errorf("skips sent = %q, want v9.9.2", got)
	}
}

// Notes withdrawn from beside a pending update are not the running version's
// viewer, so a late answer to an earlier R N's fetch does not replace them.
//
// Mutant: releaseNotesFetchedMsg's guard without notice == "" — the notes
// become v1.0.0's.
func TestReleaseNotesLateFetchLeavesWithdrawnNotesAlone(t *testing.T) {
	a := NewApp()
	a.width, a.height = 100, 40
	a.SetVersion("1.0.0")
	a.OnFetchReleaseNotes = func(string) (string, string, error) { return "v1.0.0", "old notes", nil }
	a.dispatchAction("R N", nil)
	a.releaseNotesPopup.close()
	a.updateAvailable = &UpdateStatusMsg{Version: "9.9.1", TagName: "v9.9.1", ReleaseNotes: "notes"}
	a.dispatchAction("R N", nil)
	a.Update(UpdateStatusMsg{TagName: "v9.9.1"})

	a.Update(releaseNotesFetchedMsg{Tag: "v1.0.0", Notes: "old notes"})
	if a.releaseNotesPopup.tag != "v9.9.1" || a.releaseNotesPopup.rawNotes != "notes" {
		t.Errorf("a late viewer fetch replaced the withdrawn notes: tag=%q", a.releaseNotesPopup.tag)
	}
}

// The notice row fits the box at every size, as the footer does: truncated
// to its width, and within the rows applySize leaves spare.
//
// Mutant: the notice rendered untruncated — the box widens past the notes,
// and the backstop then cuts its right border off.
func TestReleaseNotesNoticeFitsTerminal(t *testing.T) {
	for w := 12; w <= 140; w++ {
		for _, h := range []int{minTermHeight, 24, 40} {
			o := newReleaseNotesOverlay()
			o.open("v9.9.1", sampleNotes, w, h)
			o.setPending(true)
			o.withdraw("No longer pending — v9.9.2 is; close and R N for its notes")
			view := o.View()
			if got := widestLine(view); got > w {
				t.Fatalf("%dx%d: overlay rendered %d columns", w, h, got)
			}
			if rows := strings.Count(view, "\n") + 1; rows > h {
				t.Fatalf("%dx%d: overlay rendered %d rows", w, h, rows)
			}
			if !strings.Contains(stripANSI(view), "Esc") {
				t.Fatalf("%dx%d: the notice pushed the Esc hint out", w, h)
			}
			// The box is as wide as the notes it holds: a notice wider than
			// them widened it, or was cut off with the right border.
			for ln := range strings.SplitSeq(stripANSI(view), "\n") {
				if top := strings.TrimSpace(ln); strings.HasPrefix(top, "╭") {
					if got, want := lipgloss.Width(top), o.vp.Width()+2; got != want {
						t.Fatalf("%dx%d: box is %d columns, want the notes' %d", w, h, got, want)
					}
					break
				}
			}
		}
	}
}
