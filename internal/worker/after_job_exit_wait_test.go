package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// unwindingJob adds a Cancelled job whose run is still registered with the
// queue: the cancel route writes Cancelled before the run has stopped.
func unwindingJob(t *testing.T, w *DownloadWorker, db *database.Database, id string) {
	t.Helper()
	if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, URL: "u", Platform: "youtube", Status: database.StatusDownloading}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	w.queue.Enqueue(id, database.StatusDownloading)
	if got, _, ok := w.queue.Dequeue(context.Background()); !ok || got != id {
		t.Fatalf("Dequeue = %q, %v", got, ok)
	}
	db.UpdateJobFields(id, map[string]any{"status": database.StatusCancelled})
}

func (l *lockedLogger) said(part string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if msg, _ := line[0].(string); strings.Contains(msg, part) {
			return true
		}
	}
	return false
}

// TestADeferredVerbWaitsAsLongAsTheRunTakes is owner ruling R2-W2. A
// Reinitialize accepted while the job's cancelled run is still unwinding used
// to be dropped after 60 s with one Warn in the global log, after the caller
// had been told it was accepted. It now waits for the run however long that
// takes: past the notice interval it says once that it is still waiting,
// stays pending (the same verb again is "already on its way", a different one
// is refused), and is applied when the run returns.
//
// Mutants: awaitJobExit returning false at the notice (the old cut-off: the
// row stays Cancelled after the run exits); the notice line dropped.
func TestADeferredVerbWaitsAsLongAsTheRunTakes(t *testing.T) {
	old := afterJobExitNotice
	afterJobExitNotice = 20 * time.Millisecond
	t.Cleanup(func() { afterJobExitNotice = old })
	w, db := testWorkerSetup(t)
	log := &lockedLogger{}
	w.logger = log
	unwindingJob(t, w, db, "j-long")

	if err := w.ReinitializeJob("j-long"); err != nil {
		t.Fatalf("ReinitializeJob while the run unwinds = %v, want it accepted", err)
	}
	waitUntil(t, func() bool {
		return log.said("reinitialize is still waiting for the job's previous run to stop")
	})
	if err := w.ReinitializeJob("j-long"); !errors.Is(err, ErrAlreadyPending) {
		t.Errorf("the same verb after the notice = %v, want ErrAlreadyPending: the verb was dropped", err)
	}
	if err := w.ResumeJob("j-long"); !errors.Is(err, ErrResetPending) {
		t.Errorf("a Resume while the Reinitialize waits = %v, want ErrResetPending", err)
	}
	if job, _ := db.GetJob("j-long"); job.Status != database.StatusCancelled {
		t.Fatalf("status = %s while the run is still unwinding, want Cancelled", job.Status)
	}

	w.queue.Complete("j-long") // the run returns
	waitUntil(t, func() bool {
		job, _ := db.GetJob("j-long")
		return job != nil && job.Status == database.StatusUpcoming
	})
}

// TestADeferredVerbStopsWaitingAtShutdown: the wait has no cut-off, so
// shutdown has to end it (owner ruling R2-W2) — the goroutine is one Stop
// waits for. The verb is not applied, and says so.
//
// Mutant: awaitJobExit not selecting on the worker's context — the wait never
// ends and the pending entry is never cleared.
func TestADeferredVerbStopsWaitingAtShutdown(t *testing.T) {
	w, db := testWorkerSetup(t)
	log := &lockedLogger{}
	w.logger = log
	ctx, cancel := context.WithCancel(context.Background())
	w.runCtx.Store(&ctx)
	unwindingJob(t, w, db, "j-stop")

	if err := w.ReinitializeJob("j-stop"); err != nil {
		t.Fatalf("ReinitializeJob = %v", err)
	}
	cancel()
	waitUntil(t, func() bool {
		_, pending := w.afterExitPending.Load("j-stop")
		return !pending
	})
	if !log.said("reinitialize not applied: Moombox is shutting down") {
		t.Error("the verb gave up without saying so")
	}
	if job, _ := db.GetJob("j-stop"); job.Status != database.StatusCancelled {
		t.Errorf("status = %s, want the Cancelled the run left: the verb was applied at shutdown", job.Status)
	}
}

// TestADeferredVerbAsksItsGateAgainAfterTheWait: the wait can be long, and a
// row can stop allowing the verb without its status moving (a Finished row is
// resumable by its incomplete_tail flag alone). The gate is asked again once
// the run has returned and the claim is held, and a refusal there applies
// nothing.
//
// Mutant: the re-check dropped — fn runs on the row the gate now refuses.
func TestADeferredVerbAsksItsGateAgainAfterTheWait(t *testing.T) {
	w, db := testWorkerSetup(t)
	unwindingJob(t, w, db, "j-gate")
	asked := 0
	gate := func(*database.Job) error {
		asked++
		if asked > 1 {
			return &NotAllowedError{Verb: "resumed", Status: database.StatusCancelled}
		}
		return nil
	}
	ran := make(chan struct{}, 1)
	if err := w.afterJobExit("j-gate", "resume", true, gate, func(database.JobStatus) { ran <- struct{}{} }); err != nil {
		t.Fatalf("afterJobExit = %v", err)
	}
	w.queue.Complete("j-gate")
	waitUntil(t, func() bool {
		_, pending := w.afterExitPending.Load("j-gate")
		return !pending
	})
	select {
	case <-ran:
		t.Error("the verb was applied to a row its gate refused after the wait")
	default:
	}
}
