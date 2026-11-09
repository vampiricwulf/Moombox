package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
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

// ownershipJob inserts a row whose staging directory holds a set-aside
// recording, the shape a set-aside recovery runs against, and returns the
// directory.
func ownershipJob(t *testing.T, w *DownloadWorker, db *database.Database, id string, status database.JobStatus) string {
	t.Helper()
	if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, URL: "u", Platform: "youtube", Status: status}); err != nil {
		t.Fatal(err)
	}
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
	dir := filepath.Join(stagingBase, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAsidePair(t, dir, "1700000000", 8, false, false)
	return dir
}

// infoLogger keeps the Info lines, for the wait's one line naming the op.
type infoLogger struct {
	discardLogger
	mu    sync.Mutex
	lines []string
}

func (l *infoLogger) Info(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, msg)
}

func (l *infoLogger) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// TestResetVerbsWaitOutASetAsideRecovery is the data-loss case: a Reinitialize
// (the TUI's A I, the monitor's auto-reinitialize) reaching the worker while a
// set-aside recovery holds the job's staging ran os.RemoveAll under the FFmpeg
// reading the asides, because afterJobExit waited only for a queue run and a
// chapter import. Every verb that cannot be refused now waits for ANY claim,
// with no cut-off, logs one line naming what it waits on, and then runs.
//
// Mutant: afterJobExit asking only ChapterImportRunning — staging is gone
// while the recovery still holds the claim.
func TestResetVerbsWaitOutASetAsideRecovery(t *testing.T) {
	for _, tc := range []struct {
		name string
		verb func(w *DownloadWorker, id string) error
		want database.JobStatus
		gone bool // the verb deletes staging once it runs
	}{
		{"reinitialize", (*DownloadWorker).ReinitializeJob, database.StatusUpcoming, true},
		{"auto-reinitialize", (*DownloadWorker).AutoReinitializeJob, database.StatusUpcoming, true},
		{"resume", (*DownloadWorker).ResumeJob, database.StatusDownloading, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			log := &infoLogger{}
			w.logger = log
			dir := ownershipJob(t, w, db, "j-rec", database.StatusError)

			release, err := w.claimJobOperation("j-rec", opRecoverAsides)
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.verb(w, "j-rec"); err != nil {
				t.Fatalf("%s during a recovery = %v, want it accepted and deferred", tc.name, err)
			}
			waitUntil(t, func() bool { return strings.Contains(log.joined(), "set-aside recovery") })
			if _, err := os.Stat(dir); err != nil {
				t.Fatalf("DATA LOSS: %s removed staging while the recovery held it: %v", tc.name, err)
			}
			if row, _ := db.GetJob("j-rec"); row.Status != database.StatusError {
				t.Fatalf("status = %s while the recovery runs, want the row untouched", row.Status)
			}
			if w.queue.isPending("j-rec") {
				t.Fatal("the job was queued while the recovery held its staging")
			}

			release()
			w.wg.Wait()
			if row, _ := db.GetJob("j-rec"); row.Status != tc.want {
				t.Errorf("status = %s once the recovery let go, want %s", row.Status, tc.want)
			}
			if _, err := os.Stat(dir); tc.gone != os.IsNotExist(err) {
				t.Errorf("staging stat after the verb ran = %v, want gone=%v", err, tc.gone)
			}
			if !w.queue.isPending("j-rec") {
				t.Error("the verb did not queue the job once it ran")
			}
		})
	}
}

// TestResumeDuringARecoveryLeavesTheSlotsBalanced: the recovery holds the
// job's download slot as well as its claim. A Resume waits the recovery out,
// so the resumed VOD run takes the slot only after the recovery released it,
// and activeDownloads returns to zero.
func TestResumeDuringARecoveryLeavesTheSlotsBalanced(t *testing.T) {
	w, db := testWorkerSetup(t)
	ownershipJob(t, w, db, "j-slot", database.StatusError)
	release, err := w.claimJobOperation("j-slot", opRecoverAsides)
	if err != nil {
		t.Fatal(err)
	}
	if !w.queue.AcquireDownloadSlot(context.Background(), "j-slot") {
		t.Fatal("the recovery's slot")
	}
	if err := w.ResumeJob("j-slot"); err != nil {
		t.Fatal(err)
	}
	if w.queue.isPending("j-slot") {
		t.Fatal("Resume queued the run while the recovery held the job's staging and slot")
	}
	// The recovery ends: slot, then claim, as its defers run.
	w.queue.ReleaseDownloadSlot("j-slot")
	release()
	w.wg.Wait()
	// The resumed VOD run.
	if !w.queue.TryAcquireDownloadSlot("j-slot") {
		t.Fatal("the resumed run found no slot")
	}
	w.queue.ReleaseDownloadSlot("j-slot")
	if got := w.queue.ActiveCount(); got != 0 {
		t.Errorf("ActiveCount after the recovery and the resumed run = %d, want 0", got)
	}
}

// TestASecondDifferentVerbIsRefusedWhileOneIsPending: afterExitPending held
// no verb, so a Resume clicked while a Reinitialize waited was dropped while
// the route answered success. A different verb now gets ErrResetPending
// naming the pending one; the same verb again is a quiet no-op.
func TestASecondDifferentVerbIsRefusedWhileOneIsPending(t *testing.T) {
	w, db := testWorkerSetup(t)
	ownershipJob(t, w, db, "j-pend", database.StatusError)
	release, err := w.claimJobOperation("j-pend", opRecoverAsides)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.ReinitializeJob("j-pend"); err != nil {
		t.Fatal(err)
	}
	err = w.ResumeJob("j-pend")
	if !errors.Is(err, ErrResetPending) || err.Error() != "reinitialize already pending" {
		t.Errorf("Resume while a Reinitialize is pending = %v, want ErrResetPending %q", err, "reinitialize already pending")
	}
	if err := w.AutoReinitializeJob("j-pend"); !errors.Is(err, ErrResetPending) {
		t.Errorf("auto-reinitialize while a Reinitialize is pending = %v, want ErrResetPending", err)
	}
	if err := w.ReinitializeJob("j-pend"); err != nil {
		t.Errorf("the same verb again = %v, want a no-op success", err)
	}
	release()
	w.wg.Wait()
	row, _ := db.GetJob("j-pend")
	if row.Status != database.StatusUpcoming || row.AutoRetryCount != 0 {
		t.Errorf("after the wait: status %s, auto_retry_count %d; want the one Reinitialize applied (Upcoming, 0)",
			row.Status, row.AutoRetryCount)
	}
	// Nothing pending any more: the next verb is accepted.
	if err := w.ReinitializeJob("j-pend"); err != nil {
		t.Errorf("Reinitialize once nothing is pending = %v", err)
	}
}

// TestAWaitingVerbYieldsToACancel: the deferred verbs wrote their status
// unconditionally after the wait, so an operator's Cancel landing while an
// auto-reinitialize (or a Resume) waited was overwritten, and staging went.
// They now apply only while the row still reads what it read when the verb
// was accepted.
func TestAWaitingVerbYieldsToACancel(t *testing.T) {
	for _, tc := range []struct {
		name string
		verb func(w *DownloadWorker, id string) error
	}{
		{"auto-reinitialize", (*DownloadWorker).AutoReinitializeJob},
		{"reinitialize", (*DownloadWorker).ReinitializeJob},
		{"resume", (*DownloadWorker).ResumeJob},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			dir := ownershipJob(t, w, db, "j-cx", database.StatusError)
			release, err := w.claimJobOperation("j-cx", opRecoverAsides)
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.verb(w, "j-cx"); err != nil {
				t.Fatal(err)
			}
			db.UpdateJobFields("j-cx", map[string]any{"status": database.StatusCancelled})
			release()
			w.wg.Wait()
			row, _ := db.GetJob("j-cx")
			if row.Status != database.StatusCancelled || row.AutoRetryCount != 0 {
				t.Errorf("after the wait: status %s, auto_retry_count %d; want the Cancel kept (Cancelled, 0)",
					row.Status, row.AutoRetryCount)
			}
			if _, err := os.Stat(dir); err != nil {
				t.Errorf("staging removed by a verb the Cancel overrode: %v", err)
			}
			if w.queue.isPending("j-cx") {
				t.Error("the job was queued by a verb the Cancel overrode")
			}
		})
	}
}

// waitUntil polls cond for up to 5 s.
func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
