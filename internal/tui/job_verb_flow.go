package tui

import (
	"errors"
	"fmt"

	tea "charm.land/bubbletea/v2"
)

// jobVerbFlow is the one Resume (A R), Reinitialize (A I) or batch Cancel
// (A X over a selection) run in flight.
//
// The worker's verbs do their work on the caller's goroutine when the job is
// idle: a reinitialize writes the row, deletes the staging directory and the
// part files, and enqueues. A long capture's staging holds tens of thousands
// of files, so on the update goroutine one reset froze the screen and the
// keys for seconds, a batch for much longer, and job events backed up behind
// it. The run is a command instead, as A D's delete already is.
//
// A batch Cancel is one committed row write per job, and a selection can be
// the whole list (Ctrl+A over a backlog of Queued rows), so it runs here too
// and shares the one busy state: while it runs, Resume, Reinitialize and
// Cancel are refused (owner ruling R3-15). A single job's Cancel is one write
// and stays on the update loop.
//
// One busy field, written only by begin and end, both called on the update
// goroutine (CLAUDE.md, State machines, rule 6). gen names the run: an answer
// carrying any other gen belongs to no run this App is waiting for and is
// dropped.
type jobVerbFlow struct {
	busy  bool
	gen   uint64
	label string // "Resume", "Reinitialize" or "Cancel", for the busy refusal
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
	switch chord {
	case "A I":
		return jobVerbWords{"Reinitialize", "Reinitializing", "Reinitialized"}
	case "A X":
		return jobVerbWords{"Cancel", "Cancelling", "Cancelled"}
	}
	return jobVerbWords{"Resume", "Resuming", "Resumed"}
}

// jobVerbBusy refuses a new Resume, Reinitialize or batch Cancel while a run
// is still in flight, saying so on the feedback line. The selection is left
// as it is.
func (a *App) jobVerbBusy() bool {
	if !a.jobVerb.busy {
		return false
	}
	a.setFeedbackWithSeverity(a.jobVerb.label+" is still running: wait for it to finish", severityWarning)
	return true
}

// cancelBusy refuses a Cancel that may not run beside the run in flight. A
// batch Cancel shares the one busy state, so any run refuses it. A single
// job's Cancel runs on the update loop and is refused only while a batch
// Cancel runs.
func (a *App) cancelBusy(batch bool) bool {
	if !a.jobVerb.busy || (!batch && a.jobVerb.label != jobVerbWordsFor("A X").noun) {
		return false
	}
	return a.jobVerbBusy()
}

// applied adapts a verb that answers with an error alone (OnResumeJob,
// OnReinitializeJob) to runJobVerb: no error means it applied.
func applied(verb func(jobID string) error) func(string) (bool, error) {
	return func(id string) (bool, error) { return true, verb(id) }
}

// runJobVerb starts verb (OnResumeJob or OnReinitializeJob through applied,
// or OnCancelJob) on ids as a command and shows that it has started. verb
// says whether it applied: false with no error is a job that had already
// ended (a Cancel that found it Finished, Error or Cancelled). title is the
// job's, for a single job; "" makes it a batch, reported by count. The
// caller has checked jobVerbBusy and chosen the eligible ids.
func (a *App) runJobVerb(chord string, verb func(jobID string) (bool, error), ids []string, title string) tea.Cmd {
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
			switch ok, err := verb(id); {
			case err != nil:
				res.Refused.add(err)
			case ok:
				res.Count++
			default:
				res.Ended++
			}
		}
		return res
	}, func(text string) tea.Msg {
		// A panic must still end the run, or the busy state would refuse
		// every later Resume, Reinitialize and Cancel for the session.
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
// unless a chord is in progress; for a batch Cancel that count line includes
// the jobs that had already ended, held back with it.
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
	if msg.Chord == "A X" {
		a.setCancelBatchFeedback(msg.Count, msg.Ended, msg.Refused)
		return
	}
	a.setBatchFeedback(w.done+" "+jobCount(msg.Count), msg.Refused)
}

// setCancelBatchFeedback reports a batch Cancel: how many jobs it cancelled,
// how many had already ended by the time it reached them (OnCancelJob leaves
// those as they ended), and the first refusal.
func (a *App) setCancelBatchFeedback(count, ended int, refused verbRefusals) {
	switch {
	case refused.n > 0:
		// A refusal (a job already finishing) is the line's news, in red,
		// after what became of the rest — the ended jobs too.
		done := "Cancelled " + jobCount(count)
		if ended > 0 {
			done += "; " + jobCount(ended) + " had already ended"
		}
		a.setBatchFeedback(done, refused)
	case ended > 0 && count > 0:
		a.setFeedbackWithSeverity(fmt.Sprintf("Cancelled %s; %s had already ended", jobCount(count), jobCount(ended)), severityWarning)
	case ended > 0:
		a.setFeedbackWithSeverity("Not cancelled: "+jobCount(ended)+" had already ended", severityWarning)
	default:
		a.setFeedback("Cancelled " + jobCount(count))
	}
}
