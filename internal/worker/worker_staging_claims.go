package worker

import (
	"context"
	"fmt"
)

// The one staging-claim registry (CLAUDE.md, State machines, rule 4): a
// job's lease on its staging outside a queue run, held under the name of the
// operation that took it. The names (jobOp, its constants and IsResetVerb)
// are in worker.go, with opChapterImport and opBootCleanup beside their
// operations.

// claimJobOperation takes one job's staging claim for op and returns the
// release. It refuses with ErrStagingBusy, naming the operation in the way,
// when somebody already holds it.
//
// Taken SYNCHRONOUSLY by the caller, before it spawns anything, so a refusal
// is reported to the operator rather than discovered by two FFmpeg processes.
// The release is always deferred by the goroutine that got it, so a panicking
// run cannot leave a job stuck for the process's life.
func (w *DownloadWorker) claimJobOperation(jobID string, op jobOp) (func(), error) {
	w.stagingClaimMu.Lock()
	defer w.stagingClaimMu.Unlock()
	if held, busy := w.stagingClaims[jobID]; busy {
		return nil, fmt.Errorf("%w (%s in progress)", ErrStagingBusy, held.op)
	}
	return w.takeStagingClaimLocked(jobID, op), nil
}

// stagingClaim is one holder of a job's staging claim: the operation, and
// the generation that tells this holding apart from a later one of the same
// job (stagingClaimRelease).
type stagingClaim struct {
	op  jobOp
	gen uint64
}

// takeStagingClaimLocked records op as jobID's claim holder and returns the
// release. Caller holds stagingClaimMu and has seen the claim free.
func (w *DownloadWorker) takeStagingClaimLocked(jobID string, op jobOp) func() {
	if w.stagingClaims == nil {
		w.stagingClaims = map[string]stagingClaim{}
	}
	w.stagingClaimGen++
	w.stagingClaims[jobID] = stagingClaim{op: op, gen: w.stagingClaimGen}
	return w.stagingClaimRelease(jobID, w.stagingClaimGen)
}

// stagingClaimRelease is the release every claim hands back: it frees the
// claim and wakes whoever waits on it (awaitStagingClaim) — and, outside the
// lock, the heartbeat and the scheduler, which skip a claimed job
// (enqueueRunnable, Scheduler.stagingBusy) and would otherwise leave it for
// their next tick. Both wakes are non-blocking.
//
// Only while the claim is still the one it was handed (gen): a release
// called twice, or late, after the claim was freed and taken anew, used to
// delete the newer holder's claim and let a second operation in under it.
func (w *DownloadWorker) stagingClaimRelease(jobID string, gen uint64) func() {
	return func() {
		w.stagingClaimMu.Lock()
		if held, ok := w.stagingClaims[jobID]; !ok || held.gen != gen {
			w.stagingClaimMu.Unlock()
			return
		}
		delete(w.stagingClaims, jobID)
		if freed, ok := w.stagingClaimFreed[jobID]; ok {
			close(freed)
			delete(w.stagingClaimFreed, jobID)
		}
		w.stagingClaimMu.Unlock()
		select {
		case w.notifyJob <- struct{}{}:
		default:
		}
		if w.scheduler != nil {
			w.scheduler.Wake()
		}
	}
}

// stagingClaimed reports whether any operation holds jobID's staging claim.
func (w *DownloadWorker) stagingClaimed(jobID string) bool {
	_, held := w.StagingClaimHolder(jobID)
	return held
}

// anyStagingClaim is awaitStagingClaim's filter for the reset verbs: every
// operation holding the claim is in their way.
func anyStagingClaim(jobOp) bool { return true }

// isChapterImport is the filter for what waits only on a chapter import: a
// finalize (waitChapterImport) and the automatic mux (afterJobExit).
func isChapterImport(op jobOp) bool { return op == opChapterImport }

// tryStagingClaim is awaitStagingClaim without the wait: it reports whether
// nothing that blocks holds jobID's claim right now and, when take, takes it
// for what in the same step. The release is a no-op when nothing was taken.
func (w *DownloadWorker) tryStagingClaim(jobID, what string, blocks func(jobOp) bool, take bool) (func(), bool) {
	w.stagingClaimMu.Lock()
	defer w.stagingClaimMu.Unlock()
	if held, ok := w.stagingClaims[jobID]; ok && (take || blocks(held.op)) {
		return nil, false
	}
	if !take {
		return func() {}, true
	}
	return w.takeStagingClaimLocked(jobID, jobOp(what)), true
}

// awaitStagingClaim returns once no operation that blocks holds jobID's
// staging claim — taking it for what in the same step when take — or reports
// false when ctx ends first (the release is then a no-op). What waits (what)
// and on which operation is logged once, when there is something to wait
// for. No timeout: the operations that hold the claim are bounded by their
// own FFmpeg, which Stop cancels through the mux root.
func (w *DownloadWorker) awaitStagingClaim(ctx context.Context, jobID, what string, blocks func(jobOp) bool, take bool) (func(), bool) {
	logged := false
	for {
		w.stagingClaimMu.Lock()
		claim, held := w.stagingClaims[jobID]
		op := claim.op
		// Taking the claim waits for whoever holds it, whatever blocks says.
		if !held || (!take && !blocks(op)) {
			release := func() {}
			if take {
				release = w.takeStagingClaimLocked(jobID, jobOp(what))
			}
			w.stagingClaimMu.Unlock()
			return release, true
		}
		if w.stagingClaimFreed == nil {
			w.stagingClaimFreed = map[string]chan struct{}{}
		}
		freed, ok := w.stagingClaimFreed[jobID]
		if !ok {
			freed = make(chan struct{})
			w.stagingClaimFreed[jobID] = freed
		}
		w.stagingClaimMu.Unlock()
		if !logged {
			if op == opChapterImport {
				w.logger.Info("waiting for the chapter import rewriting the archive to finish before "+what, "jobID", jobID)
			} else {
				w.logger.Info("waiting for the "+string(op)+" holding the job's staging to finish before "+what, "jobID", jobID)
			}
			logged = true
		}
		select {
		case <-freed:
			// Asked again: the claim may have been taken anew since.
		case <-ctx.Done():
			return func() {}, false
		}
	}
}

// StagingClaimHolder reports which operation holds jobID's staging claim, if
// any: the REST layer's question before Resume, Retry and Reinitialize, which
// it refuses with 409 naming the operation while one does. A Resume started a
// run that writes the staging a recovery or a Mux is reading; a Reinitialize
// deleted it under FFmpeg.
func (w *DownloadWorker) StagingClaimHolder(jobID string) (jobOp, bool) {
	w.stagingClaimMu.Lock()
	defer w.stagingClaimMu.Unlock()
	held, ok := w.stagingClaims[jobID]
	return held.op, ok
}

// stagingClaimCount is how many jobs' staging claims are held — the
// scheduler's allowance for the Queued rows it skips as claimed
// (Scheduler.stagingClaimedCount).
func (w *DownloadWorker) stagingClaimCount() int {
	w.stagingClaimMu.Lock()
	defer w.stagingClaimMu.Unlock()
	return len(w.stagingClaims)
}
