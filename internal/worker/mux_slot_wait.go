package worker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobstate"
)

// muxSlotWaitProgress opens the progress line a Muxing row shows while its
// off-queue mux — the Mux action, the automatic Twitch mux, a re-mux after a
// restart — queues for a download slot (acquireMuxSlot); muxSlotWaitLine adds
// how long it has waited. The queued path never waits here: it gives its slot
// back before it muxes.
const muxSlotWaitProgress = "Waiting for a download slot to mux..."

// muxSlotWaitLine is the line itself, "Waiting for a download slot to mux...
// (3m)" ("(<1m)" in its first minute), built by waitLine like every wait
// line, so the TUI's task list tags the row "⋯ wait" and the dashboard and
// both details views show the line as it is (owner ruling R2-W5).
func muxSlotWaitLine(waited time.Duration) string {
	return waitLine(muxSlotWaitProgress, wholeMinutes(waited))
}

// acquireMuxSlot takes the download slot an off-queue mux of jobID runs
// under, and reports false only when ctx ended first. Which slot, and who
// gets it next, are AcquireDownloadSlot's: this adds only what the row says
// while it waits. A mux that finds a slot free writes nothing.
//
// One that has to queue used to say nothing: the row read Muxing with the
// download's counters (or a killed mux's "Muxing 42%") for as long as the
// pool stayed full. It now shows muxSlotWaitLine, rewritten every
// vodSlotWaitRefresh with the time waited. Every write is conditional on the
// row still reading Muxing, so the line never lands on a row an operator
// cancelled during the wait.
//
// The line is taken off again however the wait ends, after the refresher has
// stopped, and the line the row held before the wait is put back: the mux
// readout saves that line and returns it to a row whose mux fails, and a
// recording missing its tail keeps it into the Finished row. Two lines are
// not put back — a readout line a killed mux left ("Muxing 42%"), which
// newMuxReadout drops for the same reason, and this wait's own line, left by
// a process that died during the wait. A row that left Muxing during the wait
// (a Cancel) has the line taken off under the status it reads now; the mux's
// staging claim is still held, so no run of the job is writing its line.
func (w *DownloadWorker) acquireMuxSlot(ctx context.Context, jobID string) bool {
	if w.queue.TryAcquireDownloadSlot(jobID) {
		return true
	}
	if w.db == nil { // nil only in a zero-value test worker
		return w.queue.AcquireDownloadSlot(ctx, jobID)
	}
	saved := ""
	if row, err := w.db.GetJob(jobID); err == nil && row != nil {
		saved = row.Progress
	}
	if _, _, ok := jobstate.MuxReadout(saved); ok || isMuxSlotWaitLine(saved) {
		saved = ""
	}
	since := time.Now()
	write := func(line string) bool {
		return w.db.UpdateJobFieldsIf(jobID, database.StatusMuxing, map[string]any{"progress": line})
	}
	write(muxSlotWaitLine(0))
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		defer func() {
			if r := recover(); r != nil {
				w.logger.Error("mux-slot wait refresher panicked", "jobID", jobID, "panic", fmt.Sprint(r))
			}
		}()
		t := time.NewTicker(vodSlotWaitRefresh)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				write(muxSlotWaitLine(time.Since(since)))
			}
		}
	}()
	defer func() {
		close(stop)
		<-stopped
		if write(saved) {
			return
		}
		row, err := w.db.GetJob(jobID)
		if err != nil || row == nil || !isMuxSlotWaitLine(row.Progress) {
			return
		}
		w.db.UpdateJobFieldsIf(jobID, row.Status, map[string]any{"progress": saved})
	}()
	return w.queue.AcquireDownloadSlot(ctx, jobID)
}

// isMuxSlotWaitLine reports whether a progress line is one muxSlotWaitLine
// built.
func isMuxSlotWaitLine(s string) bool {
	return strings.HasPrefix(s, muxSlotWaitProgress) && jobstate.IsWaitLine(s)
}
