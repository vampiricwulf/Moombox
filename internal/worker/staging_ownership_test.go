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
			// The claim changes hands while the verb waits (released and
			// taken again by another recovery in one step, as the waiter
			// sees it): the waiter wakes, finds it held, and waits on
			// without a second line (R-A1: ONE line naming the op).
			waitUntil(t, func() bool {
				w.stagingClaimMu.Lock()
				defer w.stagingClaimMu.Unlock()
				_, ok := w.stagingClaimFreed["j-rec"]
				return ok
			})
			w.stagingClaimMu.Lock()
			close(w.stagingClaimFreed["j-rec"])
			delete(w.stagingClaimFreed, "j-rec")
			w.stagingClaimMu.Unlock()
			waitUntil(t, func() bool { // the waiter is back asleep on a fresh channel
				w.stagingClaimMu.Lock()
				defer w.stagingClaimMu.Unlock()
				_, ok := w.stagingClaimFreed["j-rec"]
				return ok
			})
			if n := strings.Count(log.joined(), "set-aside recovery"); n != 1 {
				t.Errorf("the wait logged %d lines naming the recovery, want exactly 1:\n%s", n, log.joined())
			}
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

// TestARecoveryCountsItsOwnDownloadSlot: a set-aside recovery and a run of
// the same job can overlap — the scheduler admitting a Queued row, the
// heartbeat enqueueing one the credential sweep revived — and each starts an
// FFmpeg. Keyed by the job, the second holder was told it already held the
// slot and the first release freed it under the other (an under-count past
// num_parallel_downloads); before that, it counted twice and leaked one. The
// recovery's slot is its own: both count, each release frees its own.
//
// Mutant: acquireRecoverySlot keyed by jobID — the run's acquire succeeds on
// a full pool and the count reads 1.
func TestARecoveryCountsItsOwnDownloadSlot(t *testing.T) {
	w, _ := testWorkerSetup(t)
	w.SetParallelDownloads(1)
	releaseRecovery, ok := w.acquireRecoverySlot(context.Background(), "j-slot")
	if !ok {
		t.Fatal("the recovery's slot")
	}
	if w.queue.TryAcquireDownloadSlot("j-slot") {
		t.Fatal("the job's run took a slot from a full pool: the recovery's slot was handed to it")
	}
	w.SetParallelDownloads(2)
	if !w.queue.TryAcquireDownloadSlot("j-slot") {
		t.Fatal("the job's run found no slot in a pool with one free")
	}
	if got := w.queue.ActiveCount(); got != 2 {
		t.Errorf("ActiveCount with the recovery and the run both downloading = %d, want 2", got)
	}
	w.queue.ReleaseDownloadSlot("j-slot") // the run's
	if got := w.queue.ActiveCount(); got != 1 {
		t.Errorf("ActiveCount once the run let go = %d, want 1 (the recovery still muxing)", got)
	}
	releaseRecovery()
	releaseRecovery() // idempotent, as the queue's releases are
	if got := w.queue.ActiveCount(); got != 0 {
		t.Errorf("ActiveCount after both released = %d, want 0", got)
	}
}

// TestAutomaticStartsSkipAJobWhoseStagingIsClaimed: a recovery accepts a
// COOKIES? or Queued row (neither is active), and the credential sweep, the
// heartbeat and the scheduler's admission start runs without the claim — so
// a run could start writing the staging a recovery was muxing out of. The
// heartbeat and the admission now skip a job whose claim is held, and a
// claim's release wakes both so the job is not left for the next tick.
//
// Mutant: enqueueRunnable without the claim check — the job is queued while
// the recovery holds its staging.
// Mutant (Arc C): EnqueueJob without its stagingClaimed check.
func TestAutomaticStartsSkipAJobWhoseStagingIsClaimed(t *testing.T) {
	w, db := testWorkerSetup(t)
	ownershipJob(t, w, db, "j-auto", database.StatusUpcoming)
	release, err := w.claimJobOperation("j-auto", opRecoverAsides)
	if err != nil {
		t.Fatal(err)
	}
	job, _ := db.GetJob("j-auto")
	w.enqueueRunnable([]*database.Job{job})
	if w.queue.isPending("j-auto") {
		t.Fatal("the heartbeat queued a job whose staging a recovery holds")
	}
	// Nor does EnqueueJob (a monitor finding the stream), which a reset
	// verb's Upcoming written ahead of its RemoveAll would otherwise invite.
	w.EnqueueJob("j-auto")
	if w.queue.isPending("j-auto") {
		t.Fatal("EnqueueJob queued a job whose staging a recovery holds")
	}
	if w.scheduler.stagingBusy == nil || !w.scheduler.stagingBusy("j-auto") {
		t.Error("the scheduler is not told the job's staging is claimed")
	}
	select { // drain
	case <-w.notifyJob:
	default:
	}
	release()
	select {
	case <-w.notifyJob:
	default:
		t.Error("the claim's release did not wake the heartbeat")
	}
	if w.scheduler.stagingBusy("j-auto") {
		t.Error("the scheduler still reads the released claim as busy")
	}
	w.enqueueRunnable([]*database.Job{job})
	if !w.queue.isPending("j-auto") {
		t.Error("the heartbeat did not queue the job once the claim was released")
	}
}

// TestSchedulerAdmitsNoJobWhoseStagingIsClaimed: the admission half of the
// test above, on the scheduler alone.
func TestSchedulerAdmitsNoJobWhoseStagingIsClaimed(t *testing.T) {
	s, db, log := testSchedulerSetup(t, 1)
	chID := "UC_busy"
	addSchedJob(t, db, &chID, "v_busy", database.StatusQueued, 1)
	addFeedItemRow(t, db, chID, "v_busy", "2026-07-01T00:00:00Z")
	busy := true
	s.stagingBusy = func(id string) bool { return busy && id == "v_busy" }
	s.sweep()
	if got := log.admitted(); len(got) != 0 {
		t.Fatalf("sweep admitted %v while the job's staging was claimed; want none", got)
	}
	if row, _ := db.GetJob("v_busy"); row.Status != database.StatusQueued {
		t.Fatalf("status = %s, want the row left Queued", row.Status)
	}
	busy = false
	s.sweep()
	if got := log.admitted(); len(got) != 1 || got[0] != "v_busy" {
		t.Fatalf("sweep admitted %v once the claim was released; want [v_busy]", got)
	}
}

// TestAPendingVerbAppliesOverTheCookieRefreshResume: a run parked in
// COOKIES? refreshes its cookies in its own tail and, restored, writes
// COOKIES? -> Upcoming (Queued for a backlog VOD) before it hands the
// enqueue to afterJobExit. An operator's Resume or Reinitialize accepted
// before that (seen = COOKIES?) refused the hand-off with ErrResetPending
// and then found the row no longer COOKIES?, so neither enqueued: the job
// sat for the heartbeat, and a Reinitialize became a resume. The refresh's
// resume is now one the waiting verb applies over.
func TestAPendingVerbAppliesOverTheCookieRefreshResume(t *testing.T) {
	for _, tc := range []struct {
		name string
		verb func(w *DownloadWorker, id string) error
		want database.JobStatus
		gone bool
	}{
		{"resume", (*DownloadWorker).ResumeJob, database.StatusDownloading, false},
		{"reinitialize", (*DownloadWorker).ReinitializeJob, database.StatusUpcoming, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			dir := ownershipJob(t, w, db, "j-ck", database.StatusCookies)
			w.queue.Enqueue("j-ck", database.StatusDownloading)
			if got, _, ok := w.queue.Dequeue(context.Background()); !ok || got != "j-ck" {
				t.Fatalf("Dequeue = %q, %v", got, ok)
			}
			if err := tc.verb(w, "j-ck"); err != nil {
				t.Fatal(err)
			}
			// The refresh's resume, as attemptCookieRefresh writes it.
			if !db.UpdateJobFieldsIf("j-ck", database.StatusCookies, map[string]any{"status": database.StatusUpcoming}) {
				t.Fatal("the refresh's write")
			}
			err := w.afterJobExit("j-ck", "cookie-refresh resume", true, nil, func(database.JobStatus) {
				w.queue.Enqueue("j-ck", database.StatusUpcoming)
			})
			if !errors.Is(err, ErrResetPending) {
				t.Fatalf("the refresh's hand-off = %v, want ErrResetPending", err)
			}
			w.queue.Complete("j-ck") // the parked run exits
			w.wg.Wait()
			if row, _ := db.GetJob("j-ck"); row.Status != tc.want {
				t.Errorf("status = %s, want %s: the operator's %s was dropped", row.Status, tc.want, tc.name)
			}
			if _, err := os.Stat(dir); tc.gone != os.IsNotExist(err) {
				t.Errorf("staging stat = %v, want gone=%v", err, tc.gone)
			}
			if !w.queue.isPending("j-ck") {
				t.Error("nothing queued the job: the refresh stepped aside and the verb did not run")
			}
		})
	}
}

// TestAResetVerbHoldsTheClaimWhileItRuns: a reset verb holds the job's
// claim (op = its name) for the length of its reset, so nothing that claims
// staging starts under it; and while it does, the verb is still pending, so
// a repeat is a no-op (ErrAlreadyPending) and a different verb is ErrResetPending — which
// is also why the REST layer lets a reset verb's claim through to the worker
// (IsResetVerb) rather than answering a double-click with "busy".
func TestAResetVerbHoldsTheClaimWhileItRuns(t *testing.T) {
	w, db := testWorkerSetup(t)
	ownershipJob(t, w, db, "j-hold", database.StatusError)
	entered, block, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- w.afterJobExit("j-hold", "reinitialize", true, nil, func(database.JobStatus) {
			close(entered)
			<-block
		})
	}()
	<-entered

	op, held := w.StagingClaimHolder("j-hold")
	if !held || op != "reinitialize" || !op.IsResetVerb() {
		t.Errorf("StagingClaimHolder during the reset = %q, %v (reset verb %v); want \"reinitialize\", a reset verb", op, held, op.IsResetVerb())
	}
	if opRecoverAsides.IsResetVerb() || opMux.IsResetVerb() || opChapterImport.IsResetVerb() {
		t.Error("an off-queue operation reads as a reset verb")
	}
	if err := w.RecoverAsides("j-hold"); !errors.Is(err, ErrStagingBusy) || !strings.Contains(err.Error(), "reinitialize in progress") {
		t.Errorf("RecoverAsides during the reset = %v, want ErrStagingBusy naming the reinitialize", err)
	}
	if _, err := w.ClaimChapterImport("j-hold"); !errors.Is(err, ErrStagingBusy) || !strings.Contains(err.Error(), "reinitialize in progress") {
		t.Errorf("ClaimChapterImport during the reset = %v, want ErrStagingBusy naming the reinitialize", err)
	}
	if err := w.ReinitializeJob("j-hold"); !errors.Is(err, ErrAlreadyPending) {
		t.Errorf("the same verb during its own reset = %v, want ErrAlreadyPending (a no-op)", err)
	}
	if err := w.ResumeJob("j-hold"); !errors.Is(err, ErrResetPending) || err.Error() != "reinitialize already pending" {
		t.Errorf("Resume during the reset = %v, want %q", err, "reinitialize already pending")
	}

	close(block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, held := w.StagingClaimHolder("j-hold"); held {
		t.Error("the claim outlived the reset")
	}
}

// TestASecondDifferentVerbIsRefusedWhileOneIsPending: afterExitPending held
// no verb, so a Resume clicked while a Reinitialize waited was dropped while
// the route answered success. A different verb now gets ErrResetPending
// naming the pending one; the same verb again is a no-op (ErrAlreadyPending).
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
	if err := w.ReinitializeJob("j-pend"); !errors.Is(err, ErrAlreadyPending) {
		t.Errorf("the same verb again = %v, want ErrAlreadyPending (a no-op)", err)
	}
	release()
	w.wg.Wait()
	row, _ := db.GetJob("j-pend")
	if row.Status != database.StatusUpcoming || row.AutoRetryCount != 0 {
		t.Errorf("after the wait: status %s, auto_retry_count %d; want the one Reinitialize applied (Upcoming, 0)",
			row.Status, row.AutoRetryCount)
	}
	// Nothing pending any more: the next verb is accepted (on a row it is
	// offered on).
	db.UpdateJobFields("j-pend", map[string]any{"status": database.StatusError})
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

// TestTheSameVerbAgainIsAlreadyPending: a repeat of the verb already waiting
// on the job was a nil — the success an acceptance is — so the Twitch
// monitor, which offers its auto-recovery on every poll while the previous
// run unwinds, logged "auto-recovered" each poll. A repeat now answers
// ErrAlreadyPending, which is not ErrResetPending (no refusal: REST and the
// TUI answer it as success) and leaves the pending verb in place.
//
// Mutant: afterJobExit returning nil for the same verb.
func TestTheSameVerbAgainIsAlreadyPending(t *testing.T) {
	w, db := testWorkerSetup(t)
	ownershipJob(t, w, db, "j-again", database.StatusError)
	release, err := w.claimJobOperation("j-again", opRecoverAsides)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AutoReinitializeJob("j-again"); err != nil {
		t.Fatalf("the first auto-reinitialize = %v, want it accepted", err)
	}
	for range 3 {
		err := w.AutoReinitializeJob("j-again")
		if !errors.Is(err, ErrAlreadyPending) || errors.Is(err, ErrResetPending) {
			t.Fatalf("a repeat while it waits = %v, want ErrAlreadyPending", err)
		}
	}
	release()
	w.wg.Wait()
	row, _ := db.GetJob("j-again")
	if row.Status != database.StatusUpcoming || row.AutoRetryCount != 1 {
		t.Errorf("after the wait: status %s, auto_retry_count %d; want one auto-reinitialize applied (Upcoming, 1)",
			row.Status, row.AutoRetryCount)
	}
}

// TestAVerbWhoseRowCannotBeReadIsNotAccepted: afterJobExit read the row for
// the status its write must find, and on a missing row or a failed read went
// on with "" — accepted, success reported, and then dropped by the
// conditional write no row could satisfy. It now refuses, ErrJobNotFound for
// a missing row and the read's error otherwise, and leaves no verb pending.
//
// Mutant: seen = "" on a failed read (nil returned).
func TestAVerbWhoseRowCannotBeReadIsNotAccepted(t *testing.T) {
	w, db := testWorkerSetup(t)
	if err := w.ResumeJob("j-none"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("Resume of a job with no row = %v, want ErrJobNotFound", err)
	}
	if _, pending := w.afterExitPending.Load("j-none"); pending {
		t.Error("the refused verb was left pending")
	}
	// A different verb next is not refused as "already pending".
	if err := w.ReinitializeJob("j-none"); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("Reinitialize after the refused Resume = %v, want ErrJobNotFound", err)
	}

	db.Close()
	err := w.ResumeJob("j-none")
	if err == nil || errors.Is(err, ErrJobNotFound) {
		t.Fatalf("Resume when the row cannot be read = %v, want the read's error", err)
	}
	if _, pending := w.afterExitPending.Load("j-none"); pending {
		t.Error("the verb refused on a failed read was left pending")
	}
}

// TestMuxAndChapterImportRefuseAQueuedRun: a reset verb queues the job while
// it holds the staging claim and then lets go, so a run queued to start is
// visible only after the claim is taken. RecoverAsides re-checked there;
// MuxJob and ClaimChapterImport asked only whether a run was registered, and
// before their claim, so a Mux or an import started under a queued run that
// was about to write the same staging and archive.
//
// Mutant: either verb without the queuedOrRunning check after its claim.
func TestMuxAndChapterImportRefuseAQueuedRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		verb func(w *DownloadWorker, id string) error
	}{
		{"mux", (*DownloadWorker).MuxJob},
		{"chapter import", func(w *DownloadWorker, id string) error {
			release, err := w.ClaimChapterImport(id)
			if err == nil {
				release()
			}
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			t.Cleanup(w.Stop)
			staging, _ := muxFixtureJob(t, w, db, "j-queued")
			if err := os.WriteFile(filepath.Join(staging, "video.ts"), make([]byte, 64), 0o644); err != nil {
				t.Fatal(err)
			}
			db.UpdateJobFields("j-queued", map[string]any{"status": database.StatusCancelled})
			w.queue.Enqueue("j-queued", database.StatusUpcoming)

			err := tc.verb(w, "j-queued")
			if !errors.Is(err, ErrStagingBusy) || !strings.Contains(err.Error(), "queued to run") {
				t.Fatalf("%s while a run is queued = %v, want ErrStagingBusy naming the queued run", tc.name, err)
			}
			if op, held := w.StagingClaimHolder("j-queued"); held {
				t.Errorf("the refusal left the staging claim held by %q", op)
			}
			if row, _ := db.GetJob("j-queued"); row.Status != database.StatusCancelled {
				t.Errorf("the refusal changed the row: %s", row.Status)
			}
		})
	}
}

// TestAStaleReleaseKeepsANewerClaim: a claim's release deleted the job's
// entry whoever held it, so a release run twice — or late, after the claim
// had been freed and taken anew — dropped the newer holder's claim and let a
// second operation in under it.
//
// Mutant: stagingClaimRelease deleting without comparing generations.
func TestAStaleReleaseKeepsANewerClaim(t *testing.T) {
	w, _ := testWorkerSetup(t)
	first, err := w.claimJobOperation("j-gen", opRecoverAsides)
	if err != nil {
		t.Fatal(err)
	}
	first()
	if _, err := w.claimJobOperation("j-gen", opMux); err != nil {
		t.Fatal(err)
	}
	first() // stale: the claim it was handed is gone
	if op, held := w.StagingClaimHolder("j-gen"); !held || op != opMux {
		t.Fatalf("after a stale release the claim reads %q, %v; want the mux still holding it", op, held)
	}
	if _, err := w.claimJobOperation("j-gen", opChapterImport); !errors.Is(err, ErrStagingBusy) {
		t.Errorf("a second operation under the newer claim = %v, want ErrStagingBusy", err)
	}
}

// TestSchedulerAdmitsItsFullSharePastAClaimedRow: the sweep asked
// NextQueuedJobs for the channel's free slots plus its held rows, so a
// claimed row it skipped took a row of that allowance and the channel
// admitted fewer than its share that sweep. The claimed rows now count into
// the allowance as the held ones do.
//
// Mutant: NextQueuedJobs asked for admit+heldCount() alone.
func TestSchedulerAdmitsItsFullSharePastAClaimedRow(t *testing.T) {
	s, db, log := testSchedulerSetup(t, 1)
	chID := "UC_share"
	addSchedJob(t, db, &chID, "v_claimed", database.StatusQueued, 1)
	addFeedItemRow(t, db, chID, "v_claimed", "2026-07-02T00:00:00Z") // newest: first in line
	addSchedJob(t, db, &chID, "v_free", database.StatusQueued, 1)
	addFeedItemRow(t, db, chID, "v_free", "2026-07-01T00:00:00Z")
	s.stagingBusy = func(id string) bool { return id == "v_claimed" }
	s.stagingClaimedCount = func() int { return 1 }
	s.sweep()
	if got := log.admitted(); len(got) != 1 || got[0] != "v_free" {
		t.Fatalf("sweep admitted %v past the claimed row; want [v_free]", got)
	}
}
