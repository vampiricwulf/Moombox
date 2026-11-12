package tui

import (
	"errors"
	"fmt"

	tea "charm.land/bubbletea/v2"
)

// jobVerbFlow is the one Resume (A R) or Reinitialize (A I) run in flight.
//
// The worker's verbs do their work on the caller's goroutine when the job is
// idle: a reinitialize writes the row, deletes the staging directory and the
// part files, and enqueues. A long capture's staging holds tens of thousands
// of files, so on the update goroutine one reset froze the screen and the
// keys for seconds, a batch for much longer, and job events backed up behind
// it. The run is a command instead, as A D's delete already is.
//
// One busy field, written only by begin and end, both called on the update
// goroutine (CLAUDE.md, State machines, rule 6). gen names the run: an answer
// carrying any other gen belongs to no run this App is waiting for and is
// dropped.
type jobVerbFlow struct {
	busy  bool
	gen   uint64
	label string // "Resume" or "Reinitialize", for the busy refusal
	line  string // what the feedback line says while the run is in flight
}

func (f *jobVerbFlow) begin(label, line string) uint64 {
	f.gen++
	f.busy = true
	f.label = label
	f.line = line
	return f.gen
}

// end closes the run gen names and reports whether there was one.
func (f *jobVerbFlow) end(gen uint64) bool {
	if !f.busy || gen != f.gen {
		return false
	}
	f.busy = false
	f.label = ""
	f.line = ""
	return true
}

// jobVerbWords are the words a verb's feedback lines use.
type jobVerbWords struct{ noun, doing, done string }

func jobVerbWordsFor(chord string) jobVerbWords {
	if chord == "A I" {
		return jobVerbWords{"Reinitialize", "Reinitializing", "Reinitialized"}
	}
	return jobVerbWords{"Resume", "Resuming", "Resumed"}
}

// jobVerbBusy refuses a new Resume or Reinitialize while one is still
// running, saying so on the feedback line. The selection is left as it is.
func (a *App) jobVerbBusy() bool {
	if !a.jobVerb.busy {
		return false
	}
	a.setFeedbackWithSeverity(a.jobVerb.label+" is still running: wait for it to finish", severityWarning)
	return true
}

// runJobVerb starts verb (OnResumeJob or OnReinitializeJob) on ids as a
// command and shows that it has started. title is the job's, for a single
// job; "" makes it a batch, reported by count. The caller has checked
// jobVerbBusy and chosen the eligible ids.
func (a *App) runJobVerb(chord string, verb func(jobID string) error, ids []string, title string) tea.Cmd {
	w := jobVerbWordsFor(chord)
	line := w.doing + " " + jobCount(len(ids)) + "..."
	if title != "" {
		line = fmt.Sprintf("%s: %s", w.doing, title)
	}
	gen := a.jobVerb.begin(w.noun, line)
	a.showJobVerbRunning()
	return safeCmdOr(func() tea.Msg {
		res := jobVerbResultMsg{Gen: gen, Chord: chord, Title: title}
		for _, id := range ids {
			if err := verb(id); err != nil {
				res.Refused.add(err)
				continue
			}
			res.Count++
		}
		return res
	}, func(text string) tea.Msg {
		// A panic must still end the run, or the busy state would refuse
		// every later Resume and Reinitialize for the session.
		res := jobVerbResultMsg{Gen: gen, Chord: chord, Title: title}
		res.Refused.add(errors.New(text))
		return res
	})
}

// showJobVerbRunning puts the run's line on the feedback line with no expiry:
// a batch over large staging directories runs far longer than the 3 s an
// ordinary line is held, and the busy state must stay visible for as long as
// it refuses a second run. Another line may replace it; keepJobVerbShown
// brings it back once that line has gone.
func (a *App) showJobVerbRunning() {
	a.feedback = appFeedback{msg: a.jobVerb.line}
}

// keepJobVerbShown runs on the one-second tick, after the feedback line's
// expiry: while a run is in flight and the line is empty, the run's line
// returns.
func (a *App) keepJobVerbShown() {
	if a.jobVerb.busy && a.feedback.msg == "" {
		a.showJobVerbRunning()
	}
}

// jobVerbRunningNote is the clause a quit or restart prompt carries while a
// run is in flight: leaving now can stop a reset between its row write and
// its file deletion. "" when nothing is running.
func (a *App) jobVerbRunningNote() string {
	if !a.jobVerb.busy {
		return ""
	}
	return a.jobVerb.label + " is still running"
}

// finishJobVerb applies a run's answer. An answer for a run that is not the
// one in flight is dropped.
//
// The answer can arrive seconds after the dispatch, so it must not take the
// feedback line from whatever holds it by then, least of all an armed
// chord's confirm prompt (the chord would stay armed with its prompt gone).
// A refusal is always shown: it is the one thing the operator has to act on.
// A single job's success says nothing new, so it only gives the run's own
// line, if that is still up, the ordinary hold. A batch's count is shown
// unless a chord is in progress.
func (a *App) finishJobVerb(msg jobVerbResultMsg) {
	running := a.jobVerb.line
	if !a.jobVerb.end(msg.Gen) {
		return
	}
	stillUp := a.feedback.msg == running
	w := jobVerbWordsFor(msg.Chord)
	if msg.Title != "" {
		if msg.Refused.n > 0 {
			a.setFeedbackWithSeverity(msg.Refused.first.Error(), severityError)
			return
		}
		if stillUp {
			a.setFeedback(running)
		}
		return
	}
	if msg.Refused.n == 0 && a.chord.prefix != "" {
		if stillUp {
			a.clearFeedback()
		}
		return
	}
	a.setBatchFeedback(w.done+" "+jobCount(msg.Count), msg.Refused)
}
