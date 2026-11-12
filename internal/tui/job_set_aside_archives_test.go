package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// setAsideArchiveRows is the details panel's "Set-aside Archives"
// block: its header row and the rows under it.
func setAsideArchiveRows(m *JobDetailsModel) []detailRow {
	for i, r := range m.rows {
		if r.kind != rowHeader || !strings.HasPrefix(r.label, "Set-aside Archives") {
			continue
		}
		end := i + 1
		for end < len(m.rows) && m.rows[end].kind == rowField {
			end++
		}
		return m.rows[i:end]
	}
	return nil
}

// The details panel lists the archives a Reinitialize of the job set aside
// (owner ruling R2-W1b), each on the line the dashboard shows
// (setAsideArchiveText, pinned on the same file and size in
// web/tests/job-set-aside-archives.test.mjs), and drops the block when the
// cursor moves to another job.
//
// Mutants: no block; the list not cleared by SetJob on a job switch (the next
// job shows this one's files).
func TestDetailsPanelListsTheArchivesAReinitializeSetAside(t *testing.T) {
	m := NewJobDetailsModel()
	m.SetSize(80, 40)
	m.SetJob(&database.Job{ID: "j1", Title: "t", VideoID: "v", Status: database.StatusUpcoming, Platform: "youtube"})
	if rows := setAsideArchiveRows(m); len(rows) != 0 {
		t.Fatalf("a job with no set-aside archive grew a %d-row block", len(rows))
	}

	m.SetSetAsideArchives([]SetAsideArchiveEntry{{Name: "Show.restart-1700000000.mp4", Size: 3 << 30}})
	rows := setAsideArchiveRows(m)
	if len(rows) < 2 || rows[0].label != "Set-aside Archives (1)" {
		t.Fatalf("rows = %+v, want the block with its count", rows)
	}
	var joined string
	for _, r := range rows[1:] {
		joined += r.value
	}
	if want := "Show.restart-1700000000.mp4 (3.0GB, kept, never replaced)"; joined != want {
		t.Errorf("the block reads %q, want %q", joined, want)
	}

	m.SetJob(&database.Job{ID: "j2", Title: "t", VideoID: "v2", Status: database.StatusFinished, Platform: "youtube"})
	if rows := setAsideArchiveRows(m); len(rows) != 0 {
		t.Errorf("another job shows the first one's set-aside archives: %+v", rows)
	}
}

// setAsideSend gives the App one message and hands back what the commands
// it queued for the set-aside probe answer with, in order: it takes them the
// way Bubble Tea does, out of Update's returned command, and runs them here,
// with *inUpdate false for as long as they run.
func setAsideSend(a *App, msg tea.Msg, inUpdate *bool) []tea.Msg {
	*inUpdate = true
	_, cmd := a.Update(msg)
	*inUpdate = false
	defer func() { *inUpdate = true }()
	var out []tea.Msg
	var run func(tea.Cmd)
	run = func(c tea.Cmd) {
		if c == nil {
			return
		}
		switch m := c().(type) {
		case tea.BatchMsg:
			for _, sub := range m {
				run(sub)
			}
		case setAsideArchivesMsg, setAsideReaskMsg, setAsideSelectMsg:
			out = append(out, m)
		}
	}
	run(cmd)
	return out
}

// The probe behind the block reads the output directory, which can be a slow
// or unreachable volume, and updateSelectedJob runs on the update goroutine
// on every cursor move and every job write. So the probe runs only inside a
// command: once for the job the cursor comes to rest on (a row the cursor
// only passes is never read), once when its status changes and once more a
// moment after that change (a Reinitialize writes the status before it
// renames the archive aside), and never periodically. An answer for a job the
// cursor has left, or for an older ask, is dropped.
//
// Mutants: the probe called from setAsideArchivesFor (a disk read on the
// loop); an empty answer asked again on a later update (the 2 s re-probe this
// replaced); the status left out of the key; no delayed ask after a status
// change (an archive renamed just after the first read is never shown); a
// delayed ask after a plain selection; a repeating delayed ask; the
// generation check dropped (the answer for the job the cursor left is shown
// on the next one); a selection asked about at once, or its settle tick not
// checked against the generation (every row the cursor passes is probed).
func TestSetAsideArchivesAreProbedOffTheUpdateLoop(t *testing.T) {
	old, oldSelect := setAsideReaskDelay, setAsideSelectDelay
	setAsideReaskDelay, setAsideSelectDelay = time.Millisecond, time.Millisecond
	t.Cleanup(func() { setAsideReaskDelay, setAsideSelectDelay = old, oldSelect })

	a := NewApp()
	inUpdate := true
	probes, onLoop := 0, 0
	answers := map[string][]SetAsideArchiveEntry{}
	tooEarly := false // the next probe reads before the reset's rename
	a.JobSetAsideArchives = func(id string) []SetAsideArchiveEntry {
		probes++
		if inUpdate {
			onLoop++
		}
		if tooEarly {
			tooEarly = false
			return nil
		}
		return answers[id]
	}
	// step runs the queued commands off the loop and feeds their messages
	// back, as the runtime does; it returns how many of each kind there were.
	type nop struct{}
	var pending []tea.Msg
	step := func() (asks, reasks int) {
		msgs := append(pending, setAsideSend(a, nop{}, &inUpdate)...)
		pending = nil
		for len(msgs) > 0 {
			m := msgs[0]
			msgs = msgs[1:]
			switch m.(type) {
			case setAsideSelectMsg:
				// The cursor rested: the ask it starts is this step's.
				msgs = append(msgs, setAsideSend(a, m, &inUpdate)...)
				continue
			case setAsideReaskMsg:
				reasks++
			default:
				asks++
			}
			pending = append(pending, setAsideSend(a, m, &inUpdate)...)
		}
		return asks, reasks
	}
	shown := func() int { return len(setAsideArchiveRows(a.details)) }

	j1 := &database.Job{ID: "j1", Title: "t", VideoID: "v1", Status: database.StatusError, Platform: "youtube"}
	j2 := &database.Job{ID: "j2", Title: "t", VideoID: "v2", Status: database.StatusError, Platform: "youtube"}
	a.taskList.SetJobs([]*database.Job{j1, j2})
	if sel := a.taskList.SelectedJob(); sel == nil || sel.ID != "j1" {
		t.Fatalf("selected %+v, want j1", sel)
	}

	// Selected: one ask, no delayed one, and nothing more however many
	// updates follow an empty answer.
	a.updateSelectedJob()
	a.updateSelectedJob()
	if asks, reasks := step(); asks != 1 || reasks != 0 {
		t.Fatalf("a selection queued %d asks and %d delayed asks, want 1 and 0", asks, reasks)
	}
	for range 3 {
		a.updateSelectedJob()
		if asks, reasks := step(); asks != 0 || reasks != 0 {
			t.Fatalf("an empty answer was asked again: %d asks, %d delayed asks", asks, reasks)
		}
	}
	if probes != 1 {
		t.Fatalf("%d probes for one job in one status, want 1", probes)
	}

	// The status changes (a Reinitialize): one ask, which reads before the
	// rename, and one delayed ask, which reads after it. No third.
	reset := *j1
	reset.Status = database.StatusUpcoming
	a.taskList.UpdateJob(&reset)
	answers["j1"] = []SetAsideArchiveEntry{{Name: "Show.restart-1700000000.mp4", Size: 1}}
	tooEarly = true
	a.updateSelectedJob()
	if asks, reasks := step(); asks != 1 || reasks != 1 || shown() != 0 {
		t.Fatalf("a status change queued %d asks and %d delayed asks with %d rows shown, want 1, 1 and none", asks, reasks, shown())
	}
	if asks, reasks := step(); asks != 1 || reasks != 0 || shown() == 0 {
		t.Fatalf("the delayed ask: %d asks, %d delayed asks, %d rows shown; want 1, 0 and the archive", asks, reasks, shown())
	}
	if asks, reasks := step(); asks != 0 || reasks != 0 || probes != 3 {
		t.Fatalf("after the delayed ask: %d asks, %d delayed asks, %d probes in all; want none and 3", asks, reasks, probes)
	}

	// The cursor passes j2 and comes back before it has rested there: j2 is
	// never read, and j1 is read once.
	answers["j2"] = []SetAsideArchiveEntry{{Name: "Other.restart-1700000001.mp4", Size: 1}}
	a.taskList.MoveUp() // the list draws j2 above j1
	a.updateSelectedJob()
	passed := setAsideSend(a, nop{}, &inUpdate)
	a.taskList.MoveDown()
	a.updateSelectedJob()
	if shown() != 0 {
		t.Errorf("back on j1 before its answer: %d rows shown, want none until it is read again", shown())
	}
	rested := setAsideSend(a, nop{}, &inUpdate)
	if len(passed) != 1 || len(rested) != 1 || probes != 3 {
		t.Fatalf("two cursor moves queued %d and %d messages with %d probes in all; want one settle tick each and no probe yet", len(passed), len(rested), probes)
	}
	if got := setAsideSend(a, passed[0], &inUpdate); len(got) != 0 || probes != 3 {
		t.Fatalf("the tick of a row the cursor left started %d asks (%d probes in all), want none", len(got), probes)
	}
	pending = rested
	if asks, _ := step(); asks != 1 || probes != 4 || shown() == 0 {
		t.Fatalf("the row the cursor rests on: %d asks, %d probes in all, %d rows shown; want 1, 4 and the archive", asks, probes, shown())
	}

	// A late answer: j2 is asked, the cursor returns to j1 before j2's
	// answer lands. It is dropped, and j1's own answer is shown.
	a.taskList.MoveUp()
	a.updateSelectedJob()
	var late []tea.Msg
	for _, tick := range setAsideSend(a, nop{}, &inUpdate) {
		late = append(late, setAsideSend(a, tick, &inUpdate)...)
	}
	if len(late) != 1 {
		t.Fatalf("resting on j2 queued %d asks, want 1", len(late))
	}
	a.taskList.MoveDown()
	a.updateSelectedJob()
	var own []tea.Msg
	for _, tick := range setAsideSend(a, nop{}, &inUpdate) {
		own = append(own, setAsideSend(a, tick, &inUpdate)...)
	}
	for _, m := range append(own, late...) { // j2's answer lands last
		setAsideSend(a, m, &inUpdate)
	}
	a.updateSelectedJob()
	var joined string
	for _, r := range setAsideArchiveRows(a.details) {
		joined += r.value
	}
	if !strings.Contains(joined, "Show.restart-1700000000.mp4") || strings.Contains(joined, "Other") {
		t.Errorf("j1 shows %q, want its own archive", joined)
	}

	if onLoop != 0 {
		t.Errorf("the disk probe ran %d times inside Update, want never", onLoop)
	}
}
