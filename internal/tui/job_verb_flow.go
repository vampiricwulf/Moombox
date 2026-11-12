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
}

func (f *jobVerbFlow) begin(label string) uint64 {
	f.gen++
	f.busy = true
	f.label = label
	return f.gen
}

// end closes the run gen names and reports whether there was one.
func (f *jobVerbFlow) end(gen uint64) bool {
	if !f.busy || gen != f.gen {
		return false
	}
	f.busy = false
	f.label = ""
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
	gen := a.jobVerb.begin(w.noun)
	if title != "" {
		a.setFeedback(fmt.Sprintf("%s: %s", w.doing, title))
	} else {
		a.setFeedback(w.doing + " " + jobCount(len(ids)) + "...")
	}
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

// finishJobVerb applies a run's answer: the same lines the verbs gave when
// they ran inline. An answer for a run that is not the one in flight is
// dropped.
func (a *App) finishJobVerb(msg jobVerbResultMsg) {
	if !a.jobVerb.end(msg.Gen) {
		return
	}
	w := jobVerbWordsFor(msg.Chord)
	if msg.Title != "" {
		if msg.Refused.n > 0 {
			a.setFeedbackWithSeverity(msg.Refused.first.Error(), severityError)
			return
		}
		a.setFeedback(fmt.Sprintf("%s: %s", w.doing, msg.Title))
		return
	}
	a.setBatchFeedback(w.done+" "+jobCount(msg.Count), msg.Refused)
}
