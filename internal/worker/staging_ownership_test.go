package worker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestRecoverAsidesRefusedWhileTheRunIsStopping: the cancel route writes
// Cancelled while the run is still unwinding, so the row reads terminal while
// the run still writes staging. MuxJob and ClaimChapterImport refuse there;
// a set-aside recovery must too, or it muxes out of a directory the run is
// writing and both take the job's download slot.
//
// Mutant: no IsProcessing check in RecoverAsides — it answers nil and starts.
func TestRecoverAsidesRefusedWhileTheRunIsStopping(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	staging, _ := muxFixtureJob(t, w, db, "j-stopping")
	writeAsidePair(t, staging, "1700000000", 8, false, false)

	w.queue.Enqueue("j-stopping", database.StatusDownloading)
	if got, _, ok := w.queue.Dequeue(context.Background()); !ok || got != "j-stopping" {
		t.Fatalf("Dequeue = %q, %v", got, ok)
	}
	db.UpdateJobFields("j-stopping", map[string]any{"status": database.StatusCancelled})

	err := w.RecoverAsides("j-stopping")
	if !errors.Is(err, ErrStagingBusy) {
		t.Fatalf("RecoverAsides while the run unwinds = %v, want ErrStagingBusy", err)
	}
	if op, held := w.StagingClaimHolder("j-stopping"); held {
		t.Errorf("the refusal left the staging claim held by %q", op)
	}
	if got := w.queue.ActiveCount(); got != 0 {
		t.Errorf("ActiveCount after the refusal = %d, want 0", got)
	}

	// Queued to run (a Resume enqueued it) is refused the same way.
	w.queue.Complete("j-stopping")
	w.queue.Enqueue("j-stopping", database.StatusUpcoming)
	if err := w.RecoverAsides("j-stopping"); !errors.Is(err, ErrStagingBusy) {
		t.Errorf("RecoverAsides while the job is queued to run = %v, want ErrStagingBusy", err)
	}
}

// TestStagingClaimHolderNamesTheOperation: the routes' and the TUI's refusal
// text comes from here, so it must name whatever holds the claim.
func TestStagingClaimHolderNamesTheOperation(t *testing.T) {
	w, _ := testWorkerSetup(t)
	if _, held := w.StagingClaimHolder("j"); held {
		t.Fatal("an idle job reports a claim")
	}
	release, err := w.claimJobOperation("j", opRecoverAsides)
	if err != nil {
		t.Fatal(err)
	}
	op, held := w.StagingClaimHolder("j")
	if !held || op != opRecoverAsides || !strings.Contains(string(op), "set-aside recovery") {
		t.Errorf("StagingClaimHolder = %q, %v; want %q, true", op, held, opRecoverAsides)
	}
	release()
	if _, held := w.StagingClaimHolder("j"); held {
		t.Error("the claim still reads held after its release")
	}
}
