package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobverbs"
)

// The operator verbs, as DownloadWorker methods: Resume, Reinitialize,
// auto-reinitialize, Asides, Mux and Recover Asides, with afterJobExit, the
// wait a reset verb makes for the job's run and staging claim. Their status
// gates and the refusal ErrNotAllowed are in verbs.go; the staging claim they
// take is in worker_staging_claims.go.

// ResumeJob resumes a cancelled/errored YouTube job from its saved state.
// Preserves staging files, progress, and seq numbers. Resets auto_retry_count
// so any future error fires its notification — Resume is user-driven, so the
// "suppress retry-failure notifications" guard in setJobError must not apply.
//
// Returns ErrResetPending when a different verb is already waiting to run on
// the job (afterJobExit), ErrAlreadyPending when a resume already is (not a
// refusal: the operator's callers answer it as success), ErrJobNotFound when
// the row is gone; nil when the resume ran or will run once the job's
// staging is free. Applied only while the row still reads the status it read
// when the verb was accepted, so an operator's Cancel landing during the wait
// wins (acceptedStatus, which also lets it apply over a cookie refresh's own
// resume of the row).
func (w *DownloadWorker) ResumeJob(jobID string) error {
	return w.afterJobExit(jobID, string(opResume), true, resumeGate, func(seen database.JobStatus) {
		from, ok := w.acceptedStatus(jobID, seen)
		applied := ok && w.db.UpdateJobFieldsIf(jobID, from, map[string]any{
			"status":           database.StatusDownloading,
			"error":            "",
			"park_reason":      database.ParkReasonNone,
			"park_identity":    "",
			"auto_retry_count": 0,
			// A re-run — an operator's, or the monitor's auto-resume or
			// auto-reinitialize — is of a job already known: a "Stream Found"
			// still owed from a run that never reached its first fetch (a
			// panic, a park) would announce a discovery long past.
			"announce_found": false,
		})
		if !applied {
			w.logVerbOverridden(jobID, "resume", seen)
			return
		}
		// A fresh backlog retry budget too, whatever ended the last run.
		w.endBacklogStreak(jobID)
		w.enqueueHeld(jobID)
	})
}

// ErrResetPending: a different verb is already waiting to run on this job
// (afterJobExit) — a Resume clicked while a Reinitialize waits out a recovery,
// say. Wrapped with the pending verb's name ("reinitialize already pending");
// the REST layer answers 409 with it and the TUI shows it. The same verb
// again is not refused: it is already on its way.
var ErrResetPending = errors.New("already pending")

// ErrAlreadyPending: the same verb is already waiting to run on this job, or
// running (afterJobExit). Not a refusal — the verb is on its way — so the
// operator's callers (REST, the TUI) answer it as the success it is (owner
// ruling R-A3); it exists so an automatic caller that offers the verb on
// every poll (the Twitch monitor's auto-recovery, the YouTube auto-resume)
// can tell a repeat from an acceptance and not log one each poll.
var ErrAlreadyPending = errors.New("already on its way")

// ErrJobNotFound: the verb's job has no row (afterJobExit). Nothing was
// accepted; the REST layer answers 404.
var ErrJobNotFound = errors.New("job not found")

// afterJobExitTimeout bounds how long a deferred Resume/Reinitialize waits for
// the job's previous run to unwind (a chat capture's shutdown grace is the
// long pole) before giving up rather than racing it. The wait for a staging
// claim that follows has no cut-off (owner ruling R-A1).
const afterJobExitTimeout = 60 * time.Second

// logVerbOverridden records a deferred verb that found its row changed while
// it waited — an operator's Cancel, another verb, a deletion — and so did
// nothing.
func (w *DownloadWorker) logVerbOverridden(jobID, what string, seen database.JobStatus) {
	now := database.JobStatus("") // "" when the row is gone
	if cur, err := w.db.GetJob(jobID); err == nil && cur != nil {
		now = cur.Status
	}
	w.logger.Info(what+" not applied: the job changed while it waited", "jobID", jobID,
		"was", seen, "now", now)
}

// afterJobExit runs fn now when jobID has no run in flight, or once the run
// that is still unwinding has exited. The cancel route writes Cancelled
// before the run has stopped, so Retry and Resume appear at once — and one
// clicked in that window used to race the run: Enqueue dropped the job as
// still processing, handleCancellation then wrote Cancelled over the fresh
// status, and Reinitialize deleted the staging the run was still writing its
// chat resume sidecar into. Callers stay synchronous (the TUI calls these on
// its update goroutine); the wait happens here, one per job.
//
// It waits, too, for a chapter import that holds the job's claim (owner
// decision PR-M5): a Resume, a retry or a Reinitialize let through while the
// import re-muxes the archive started a run whose finalize the re-mux's
// rename then wrote the old, shorter archive back over, or deleted the
// staging and parts beside an archive mid-replace. The REST verbs refuse
// with 409 first (StagingClaimHolder); this is the wait for the callers
// that cannot be refused — the TUI's, the monitor's auto-resume. The import
// is bounded by its own re-mux, which Stop cancels.
//
// And for ANY other operation holding the job's staging claim (owner ruling
// R-A1) — a set-aside recovery, a Mux, the boot cleanup: a Reinitialize let
// through during a recovery ran os.RemoveAll under the FFmpeg reading the
// asides, and a Resume started a run that took the recovery's download slot.
// That wait has no cut-off, like the import's, and logs one line naming the
// operation. The REST verbs refuse first (StagingClaimHolder).
//
// hold makes the verb take the claim itself (op = what) for the length of
// fn, atomically with finding it free, so nothing that claims staging — a
// recovery, a Mux, an import — can start between the wait and fn's reset,
// and once it lets go the job is queued, which those refuse on
// (queuedOrRunning). A verb whose fn takes the claim its own way (the
// automatic mux, through muxJob) passes false and waits only for a chapter
// import, as before: its fn treats any other holder as the deferral
// ErrStagingBusy is, and the next poll tries again (autoMuxNow).
//
// fn gets the job's status as read when the verb was accepted (seen), the
// status its write must still find (UpdateJobFieldsIf): an operator's Cancel
// landing during the wait wins.
//
// One verb waits per job (afterExitPending, which names it). The same verb
// again answers ErrAlreadyPending, which the operator's callers treat as
// success and an automatic one as "nothing new"; a different verb is refused
// with ErrResetPending, so the caller can say so rather than report a click
// that was dropped.
//
// A verb whose row cannot be read is not accepted: seen would be "", which no
// row ever reads, so fn's conditional write would drop it after the caller
// had reported success. ErrJobNotFound when there is no row, the read's own
// error otherwise.
//
// gate, when not nil, is the verb's status predicate (resumeGate,
// reinitGate): a row it refuses is not accepted, and its *NotAllowedError is
// the answer — the TUI calls these verbs on the row it last drew, and the
// REST route on the row it read a moment before.
func (w *DownloadWorker) afterJobExit(jobID, what string, hold bool, gate func(*database.Job) error, fn func(seen database.JobStatus)) error {
	if prev, waiting := w.afterExitPending.LoadOrStore(jobID, what); waiting {
		if prev.(string) == what {
			return fmt.Errorf("%s %w", what, ErrAlreadyPending)
		}
		return fmt.Errorf("%s %w", prev, ErrResetPending)
	}
	job, err := w.db.GetJob(jobID)
	if err != nil || job == nil {
		w.afterExitPending.Delete(jobID)
		if err != nil {
			return fmt.Errorf("%s: reading the job: %w", what, err)
		}
		return fmt.Errorf("%s: %w", what, ErrJobNotFound)
	}
	if gate != nil {
		if err := gate(job); err != nil {
			w.afterExitPending.Delete(jobID)
			return err
		}
	}
	seen := job.Status
	blocks := anyStagingClaim
	if !hold {
		blocks = isChapterImport
	}
	running := w.queue.IsProcessing(jobID)
	if !running {
		if release, ok := w.tryStagingClaim(jobID, what, blocks, hold); ok {
			defer w.afterExitPending.Delete(jobID)
			defer release()
			fn(seen)
			return nil
		}
	}
	if running {
		w.logger.Info("waiting for the job's previous run to stop before "+what, "jobID", jobID)
	}
	w.wg.Go(func() {
		defer w.afterExitPending.Delete(jobID)
		defer func() {
			if r := recover(); r != nil {
				w.logger.Error("panic in deferred "+what, "jobID", jobID, "panic", fmt.Sprint(r))
			}
		}()
		if running && !w.WaitForJobExit(jobID, afterJobExitTimeout) {
			w.logger.Warn(what+" skipped: the job's previous run did not stop", "jobID", jobID,
				"waited", afterJobExitTimeout)
			return
		}
		release, _ := w.awaitStagingClaim(context.Background(), jobID, what, blocks, hold)
		defer release()
		fn(seen)
	})
	return nil
}

// clearJobParts removes a job's persisted parts for a fresh restart: the
// on-disk part files (video + per-part chat, best-effort) and then the
// segment/gap rows. Called only by user-initiated Reinitialize.
func (w *DownloadWorker) clearJobParts(jobID string) {
	if segs, err := w.db.GetSegments(jobID); err == nil {
		for _, s := range segs {
			if s.FilePath != "" {
				if rmErr := os.Remove(s.FilePath); rmErr != nil && !os.IsNotExist(rmErr) {
					w.logger.Warn("reinit: failed to remove stale part file", "path", s.FilePath, "err", rmErr)
				}
			}
			if s.ChatFile != "" {
				if rmErr := os.Remove(s.ChatFile); rmErr != nil && !os.IsNotExist(rmErr) {
					w.logger.Warn("reinit: failed to remove stale part chat", "path", s.ChatFile, "err", rmErr)
				}
			}
		}
		// The parts' chapter list goes with them (owner decision FE-16): it
		// is derived from them, named after their base, on no column.
		removeSplitArchiveChapters(segs, w.logger)
	}
	if err := w.db.ClearJobSegmentsAndGaps(jobID); err != nil {
		w.logger.Warn("reinit: failed to clear segment/gap rows", "jobID", jobID, "err", err)
	}
}

// ReinitializeJob resets a job to a fresh state and re-enqueues it.
// Clears all progress fields and deletes the staging directory — after the
// job's previous run has stopped, when one is still unwinding (afterJobExit).
//
// Returns ErrResetPending when a different verb is already waiting on the job
// (see ResumeJob); the reset applies only while the row still reads the
// status it read when the verb was accepted.
func (w *DownloadWorker) ReinitializeJob(jobID string) error {
	return w.afterJobExit(jobID, string(opReinitialize), true, reinitGate, func(seen database.JobStatus) { w.reinitializeNow(jobID, seen) })
}

// acceptedStatus reports the status an operator's Resume or Reinitialize
// still applies over — the one its conditional write must find — or false
// when the row changed under it (a Cancel, a deletion). The reset verbs ask
// before they delete anything, under the staging claim they hold, so a Cancel
// that landed while they waited keeps the staging as well as the row; their
// conditional status write covers the instant after.
//
// Normally that is seen, the status read when the verb was accepted. One
// change is not the operator's to lose: a verb accepted on a COOKIES? row
// whose run then restored its cookies in its own tail finds the row written
// Upcoming (Queued for a backlog VOD) by that refresh, whose own enqueue then
// stepped aside for this verb (ErrResetPending). Refused here as well, the job
// sat unqueued and the operator's Reinitialize became a resume. That status
// is accepted — but only while nothing has queued a run for it yet, which
// would be writing the staging a Reinitialize deletes.
func (w *DownloadWorker) acceptedStatus(jobID string, seen database.JobStatus) (database.JobStatus, bool) {
	cur, err := w.db.GetJob(jobID)
	if err != nil || cur == nil {
		return "", false
	}
	if cur.Status == seen {
		return seen, true
	}
	if seen == database.StatusCookies &&
		(cur.Status == database.StatusUpcoming || cur.Status == database.StatusQueued) &&
		w.queuedOrRunning(jobID) == "" {
		return cur.Status, true
	}
	return "", false
}

// reinitializeNow is ReinitializeJob's reset, once the job is free. The row
// is claimed first — the reset written Upcoming, conditionally on the status
// the verb was accepted on (acceptedStatus) — and only once that write has
// applied are the staging directory, the parts and an imported chapter list
// deleted: the deletion cannot be undone, so it follows the one step that
// can still be refused. Deleted first, a row that had moved on meanwhile (a
// Cancel, a stale TUI read) lost its staging to a reset that was then not
// applied. The job is invisible to everything that starts a run while the
// verb holds its staging claim (the heartbeat, the scheduler, EnqueueJob), so
// the Upcoming row is not picked up before its staging is gone; the verb
// queues it itself once it is.
func (w *DownloadWorker) reinitializeNow(jobID string, seen database.JobStatus) {
	from, ok := w.acceptedStatus(jobID, seen)
	if !ok {
		w.logVerbOverridden(jobID, "reinitialize", seen)
		return
	}
	// Read before the claim, which clears output_file: a single-file
	// archive's imported chapter list (owner decision CJ-2) is beside it. The
	// re-download is a new recording whose timeline can differ from the one
	// the list was fitted to. Dropped here, as the reset clears output_file,
	// and not only by the new archive's finalize (dropImportedArchiveChapters):
	// a re-download Cancelled and then muxed by the Mux action is a
	// re-finalize (JobContext.Refinalize), which would otherwise carry the old
	// recording's list onto the new one.
	var priorOutput string
	if prior, err := w.db.GetJob(jobID); err == nil && prior != nil {
		priorOutput = prior.OutputFile
	}

	// Clear all non-input fields. auto_retry_count resets here because
	// user-driven reinit grants the job a fresh budget; auto-recovery uses
	// AutoReinitializeJob, which spends one of it instead.
	beforeResetClaim(jobID)
	if !w.db.UpdateJobFieldsIf(jobID, from, reinitResetFields(0)) {
		w.logVerbOverridden(jobID, "reinitialize", seen)
		return
	}

	w.removeStaging(jobID, "reinitialize")
	// Fresh start: discard any parts from a prior quality/gap-split attempt.
	// Without this the stale segment rows survive the reset and muxAndFinalize
	// would finalize the clean re-download as multi-part from the OLD part
	// files, silently discarding the freshly-downloaded media. (AutoReinit
	// deliberately does NOT do this — see that method.)
	w.clearJobParts(jobID)
	if priorOutput != "" {
		dropImportedArchiveChapters(priorOutput, w.logger)
	}
	// The backlog retry budget is in memory, not a column: a fresh one too.
	w.endBacklogStreak(jobID)
	w.enqueueHeld(jobID)
}

// beforeResetClaim runs just ahead of a reinitialize's conditional claim: a
// test seam, to land a status change between the verb's last read and its
// write. Production never assigns it.
var beforeResetClaim = func(string) {}

// removeStaging deletes a job's staging directory for a reset verb (what
// names it in the warning a failure logs).
func (w *DownloadWorker) removeStaging(jobID, what string) {
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) {
		stagingBase = c.Paths.EffectiveStagingDir()
	})
	stagingDir := filepath.Join(stagingBase, jobID)
	if err := os.RemoveAll(stagingDir); err != nil {
		w.logger.Warn("failed to remove staging directory on "+what, "path", stagingDir, "err", err)
	}
}

// AutoReinitializeJob is the auto-recovery sibling of ReinitializeJob: same
// state reset (clears progress fields, deletes staging dir, sets status to
// Upcoming, re-enqueues), but increments auto_retry_count instead of
// resetting it. Called by the Twitch monitor's OnStreamRecover callback
// when an errored job's underlying broadcast is still live and the error
// matches a recoverable shape.
//
// Capped at MaxTwitchAutoRetries (the caller is expected to pre-check the
// budget; this method blindly increments).
//
// Unlike ReinitializeJob, this deliberately does NOT clear the job's segment
// rows: auto-recovery only fires for the SAME still-live broadcast (the caller
// guards on sameBroadcastStart), so already-captured parts 0..N are real
// footage from this broadcast and the recovered capture continues at part N+1
// (discoverResumeSegment returns maxRecorded+1). Clearing here would throw away
// captured footage of a live broadcast — the opposite of recovery.
func (w *DownloadWorker) AutoReinitializeJob(jobID string) error {
	// The monitor reacts to the Error row the moment setJobError writes it,
	// while that run is still finishing its tail: wait for it to exit, as a
	// user's Reinitialize does, rather than reset the row and staging under
	// it (and have the Enqueue dropped as a duplicate).
	// An error (ErrResetPending) means nothing was applied, so no retry of
	// the budget was spent.
	return w.afterJobExit(jobID, string(opAutoReinitialize), true, reinitGate, func(seen database.JobStatus) { w.autoReinitializeNow(jobID, seen) })
}

// autoReinitializeNow is AutoReinitializeJob's reset: reinitializeNow's
// order — the conditional claim first, the staging deleted only once it
// applied — with the retry budget spent rather than renewed, and the job's
// parts kept (see AutoReinitializeJob).
func (w *DownloadWorker) autoReinitializeNow(jobID string, seen database.JobStatus) {
	prev, err := w.db.GetJob(jobID)
	if err != nil || prev == nil {
		w.logger.Warn("AutoReinitializeJob: job not found", "jobID", jobID, "err", err)
		return
	}
	if prev.Status != seen {
		w.logVerbOverridden(jobID, "auto-reinitialize", seen)
		return
	}
	beforeResetClaim(jobID)
	if !w.db.UpdateJobFieldsIf(jobID, seen, reinitResetFields(prev.AutoRetryCount+1)) {
		w.logVerbOverridden(jobID, "auto-reinitialize", seen)
		return
	}
	w.removeStaging(jobID, "auto-reinitialize")
	w.enqueueHeld(jobID)
}

// Asides reports the set-aside recordings in a job's staging directory, and
// whether that directory also still holds the chat capture keepOnlyChatCapture
// preserves. The read half of the recovery verb: RecoverAsides is what acts on
// it, and both UIs render it under the job.
//
// Errors only when the job row cannot be read — an empty report is a perfectly
// ordinary answer, and the caller must be able to tell it from "that job is
// gone".
func (w *DownloadWorker) Asides(jobID string) (AsideReport, error) {
	job, err := w.db.GetJob(jobID)
	if err != nil {
		return AsideReport{}, fmt.Errorf("look up job %s: %w", jobID, err)
	}
	if job == nil {
		return AsideReport{}, fmt.Errorf("job %s not found", jobID)
	}
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) {
		stagingBase = c.Paths.EffectiveStagingDir()
	})
	return ScanAsides(stagingBase, jobID), nil
}

// MuxJob force-muxes a cancelled/errored job's staging files.
// Bypasses the download queue — runs directly in a wg-tracked goroutine.
func (w *DownloadWorker) MuxJob(jobID string) error {
	return w.muxJob(jobID, muxActionFailure, false)
}

// muxActionFailure is the error the Mux action's failure leaves on the row:
// the mux error itself.
func muxActionFailure(err error) string { return err.Error() }

// muxJob is MuxJob with the error a failed mux leaves on the row chosen by
// the caller: the Mux action's is the mux error itself, the automatic mux of
// an ended Twitch broadcast says it was automatic (autoMuxFailure).
//
// bootRemux is the boot's re-mux of a row a restart interrupted while it was
// Muxing (enqueueExistingJobs, muxOnRestart): a download that completed, not
// the operator muxing whatever was captured (JobContext.CapturedMux), and the
// one caller that muxes a row already Muxing — and only one still Muxing: a
// row cancelled since the boot pass read it is refused with ErrNotAllowed,
// not muxed as the Mux action would mux a Cancelled row. Every other caller
// muxes a row the Mux action is offered on (jobverbs.MuxableStatus), and the
// Muxing write applies only while the row still reads one of those — so a
// Mux decided on a row a Resume or a Reinitialize has moved on since is
// refused with ErrNotAllowed instead of written over it.
func (w *DownloadWorker) muxJob(jobID string, failure func(error) string, bootRemux bool) error {
	// Read config for staging check
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) {
		stagingBase = c.Paths.EffectiveStagingDir()
	})

	if !HasSegmentFiles(stagingBase, jobID) {
		return fmt.Errorf("no segment files found in staging")
	}

	// The same per-job staging claim RecoverAsides takes. Both verbs mux out
	// of one directory into one output directory, and both reach
	// muxStagedAsides with their own asideOutputPath collision map — so two of
	// them together write the same sibling name over each other. Before the
	// status write, so a refusal leaves the row exactly as it found it.
	release, err := w.claimJobOperation(jobID, opMux)
	if err != nil {
		return err
	}

	// Nor while the job's last run is still registered, or a run is queued
	// to start. The cancel route writes Cancelled before the run has
	// stopped, and both UIs offer Mux on a Cancelled row with staged media at
	// once — while a cancelled Twitch capture still waits out its part muxes
	// and its chat's shutdown grace. A Mux clicked there wrote Muxing and
	// Finished, reclaimed staging, and then the run's own end wrote Cancelled
	// over the Finished archive (or, landing during the mux, cancelled it
	// through the listener below). Retry, Resume and Reinitialize wait for
	// that exit (afterJobExit); a Mux is refused instead, as a conflict the
	// operator can try again in a moment, so the route still answers what
	// became of the click. After the claim, as RecoverAsides asks: a reset
	// verb queues the job while it holds the claim and lets go, so a run
	// queued to start is visible only from here. Still ahead of the status
	// write, so a refusal leaves the row exactly as it found it. The
	// automatic mux (autoMuxNow) runs after the exit and treats the refusal
	// as the deferral ErrStagingBusy is.
	if why := w.queuedOrRunning(jobID); why != "" {
		release()
		return fmt.Errorf("%w (%s)", ErrStagingBusy, why)
	}

	// A boot re-mux is of a download that completed; anything else is the
	// operator (or the automatic Twitch mux) muxing whatever was captured
	// (JobContext.CapturedMux). Passed by the caller rather than read off a
	// Muxing row, which said the same only while no other caller could reach
	// one. prior is read before the Muxing write below overwrites it.
	capturedMux := !bootRemux
	prior, err := w.db.GetJob(jobID)
	if err == nil && prior == nil {
		release()
		return fmt.Errorf("mux: %w", ErrJobNotFound)
	}
	// The boot re-mux was decided on a row read as Muxing, so Muxing is its
	// only source: a row an operator cancelled since is refused here, where
	// the Mux action's set would accept the Cancelled row and archive it.
	sources := jobverbs.MuxableStatuses()
	if bootRemux {
		sources = []database.JobStatus{database.StatusMuxing}
	}
	// A Finished job is muxed only for a part its finalize could not mux
	// (jobverbs.MuxNeedsUnmuxedParts → HasUnmuxedParts), the fact the route
	// and the TUI check on their own earlier read. Checked here too, on this
	// read, and Finished leaves the write's source set unless it holds: a row
	// read as Error that Finishes before the write is not written over.
	unmuxedParts := prior != nil && jobverbs.MuxNeedsUnmuxedParts(prior.Status) && HasUnmuxedParts(w.db, stagingBase, jobID)
	if !unmuxedParts {
		sources = slices.DeleteFunc(sources, jobverbs.MuxNeedsUnmuxedParts)
	}
	if prior != nil && !slices.Contains(sources, prior.Status) {
		release()
		return &NotAllowedError{Verb: "muxed", Status: prior.Status}
	}
	// A mux of the staging ends what an unconfirmed-end marker was waiting
	// for, whoever started it: cleared here, an operator's Mux that fails
	// is not followed by an automatic one when the channel next reads
	// offline (D-T4). On every other row the value is already empty.
	// Conditional on the sources: a Resume or a Reinitialize that moved the
	// row on since it was read (its run queued, not yet registered) is not
	// written over — refused here, before anything is spawned, the row and
	// the staging are left as they are.
	if !w.db.UpdateJobFieldsIfIn(jobID, sources, map[string]any{
		"status":      database.StatusMuxing,
		"park_reason": database.ParkReasonNone,
	}) {
		release()
		cur, err := w.db.GetJob(jobID)
		if err == nil && cur == nil {
			return fmt.Errorf("mux: %w", ErrJobNotFound)
		}
		now := database.JobStatus("")
		if cur != nil {
			now = cur.Status
		}
		return &NotAllowedError{Verb: "muxed", Status: now}
	}
	// failed is the Error write this Mux's failure paths make. A Mux of a
	// Cancelled row (or of one whose earlier Mux after a Cancel failed) puts
	// ParkReasonMuxAfterCancel back with it: the Error status replaces the
	// Cancelled one, and the Cancel's gap-fill close (owner decision FQ-2,
	// twitchMuxTailOpen) would otherwise not outlive the failed Mux.
	failed := func(msg string) map[string]any {
		fields := map[string]any{"status": database.StatusError, "error": msg}
		if prior != nil && (prior.Status == database.StatusCancelled || prior.ParkReason == database.ParkReasonMuxAfterCancel) {
			fields["park_reason"] = database.ParkReasonMuxAfterCancel
		}
		return fields
	}
	// writeFailed makes that write, only over this Mux's own Muxing (an
	// outcome that landed meanwhile stands), and settles an in-flight chat on
	// the Error it applied as setJobError's Error does (settleErrorChat, owner
	// rulings R-E1a, R-H1): this Error is written here, not through
	// setJobError, and a row can reach a Mux still showing one — an Error row from
	// before the settle existed, or one a stray downloader's OnStart wrote
	// over its settle.
	writeFailed := func(msg string) {
		if w.db.UpdateJobFieldsIf(jobID, database.StatusMuxing, failed(msg)) {
			w.settleErrorChat(jobID)
		}
	}

	// From here a Cancel that applies is this run's to announce
	// (CancelJob, endMuxRun).
	w.beginMuxRun(jobID)
	w.wg.Go(func() {
		defer release()
		defer func() {
			if r := recover(); r != nil {
				w.logger.Error("panic in MuxJob", "jobID", jobID, "panic", fmt.Sprint(r))
				// Not over an outcome, as in Start's recover: a mux that
				// wrote Finished and panicked in its tail (the notification,
				// the staging cleanup) turned the archive into an Error, and
				// so did an operator's Cancel that landed on the Muxing row.
				// The Muxing write above came before this goroutine, so
				// whatever the row holds besides Muxing landed during this mux
				// and stands.
				writeFailed(fmt.Sprintf("internal panic: %v", r))
			}
		}()
		// Deferred after the recover, so it runs under it: the "Job
		// Cancelled" a Cancel left to this run, once the row's last word is
		// in (a re-claimed finish has written Finished by now).
		// finished — the mux returned with its archive recorded — is what it
		// goes by when the row cannot be read then.
		finished := false
		defer func() { w.endMuxRun(jobID, prior, finished) }()

		// Owner decision O-E: the orchestrator's mux root, never
		// context.Background() — a Stop reaches this FFmpeg and cancels it.
		root := w.orchestrator.muxRoot()

		// The operator's Cancel reaches this FFmpeg too. Both UIs offer Cancel
		// on a Muxing row and the route writes Cancelled, but queue.Cancel
		// only knows the jobs it dequeued — this one it never saw — so the
		// mux ran on, wrote Finished over the Cancelled row and sent a
		// "Download Finished" embed after the "Job Cancelled" one. Listened
		// for the way ExecuteWithChat listens, and a deleted row stops it the
		// way processJob's listener does. Subscribed BEFORE the row is read,
		// so a cancel that lands first is seen in the row instead.
		ctx, cancelMux := context.WithCancel(root)
		defer cancelMux()
		unsubscribe := w.db.OnJobUpdate(func(updated *database.Job) {
			if updated.ID == jobID && updated.Status == database.StatusCancelled {
				cancelMux()
			}
		})
		defer unsubscribe()
		unsubscribeDel := w.db.OnJobDeleted(func(deleted *database.JobDeleted) {
			if deleted.JobID == jobID {
				cancelMux()
			}
		})
		defer unsubscribeDel()

		job, err := w.db.GetJob(jobID)
		if err != nil {
			w.logger.Error("MuxJob: get job failed", "jobID", jobID, "err", err)
			writeFailed(fmt.Sprintf("mux setup failed: %v", err))
			return
		}
		if job == nil {
			// Row deleted while the mux was queued — nothing to do (and no
			// row left to flag as errored).
			w.logger.Debug("MuxJob: job vanished before muxing", "jobID", jobID)
			return
		}
		if job.Status == database.StatusCancelled {
			cancelMux()
		}

		jobCtx := w.buildJobContext(job)
		jobCtx.CapturedMux = capturedMux
		jobCtx.Refinalize = true
		// Read from the staging's coverage before the mux reclaims it, kept
		// once the row is Finished (owner decision D17).
		gapPlan := w.twitchMuxGapFillPlan(prior, job, jobCtx)

		// This mux takes the same download slot a queued job takes: a boot
		// that finds N interrupted Muxing rows would otherwise start N
		// FFmpegs at once, which num_parallel_downloads exists to prevent.
		// The wait ends on the mux root's cancellation, so a shutdown does not
		// sit here holding the process open.
		if !w.queue.AcquireDownloadSlot(ctx, jobID) {
			if root.Err() == nil {
				w.logger.Info("MuxJob: cancelled while waiting for a mux slot; staging is kept", "jobID", jobID)
				return
			}
			w.logger.Info("MuxJob: shutdown while waiting for a mux slot; the row stays Muxing for the next start", "jobID", jobID)
			return
		}
		defer w.queue.ReleaseDownloadSlot(jobID)

		if err := w.orchestrator.muxFromStaging(ctx, jobCtx); err != nil {
			if errors.Is(err, ErrCancelled) {
				// The finalize's Muxing write found an outcome on the row —
				// an operator's Cancel this listener has yet to hear, or
				// another outcome no cancel follows — and nothing was muxed.
				// It stands: read as a failure, the Error write below landed
				// on it, whatever this mux's ctx said.
				w.logger.Info("MuxJob: the row already holds an outcome; nothing muxed, staging is kept", "jobID", jobID)
				return
			}
			if ctx.Err() != nil && root.Err() == nil {
				// Cancelled (or deleted) by the operator; staging stays for a
				// later Mux or Resume. The row says Cancelled unless the mux's
				// own Muxing write landed just after the route's — re-assert
				// it so the row cannot be left Muxing with nothing running.
				// On a row still Muxing only: read and then written
				// unconditionally, the Cancelled also landed on whatever came
				// after the route's — a Finished archive a racing mux wrote,
				// the operator's Resume of the cancelled row.
				w.logger.Info("MuxJob: cancelled; staging is kept", "jobID", jobID)
				w.db.UpdateJobFieldsIf(jobID, database.StatusMuxing, map[string]any{"status": database.StatusCancelled})
				return
			}
			if ctx.Err() != nil {
				// Cancelled by Stop, not a failure: leave the row Muxing with
				// its staging intact so the restarted child re-muxes it
				// (muxOnRestart only routes a Muxing row — writing Error here
				// would strand the recording).
				w.logger.Info("MuxJob: cancelled by shutdown; the row stays Muxing for the next start", "jobID", jobID)
				return
			}
			w.logger.Error("MuxJob failed", "jobID", jobID, "err", err)
			// Only over this mux's own Muxing: an outcome that landed
			// meanwhile stands.
			writeFailed(failure(err))
			return
		}
		finished = true

		if gapPlan != nil {
			w.orchestrator.keepTwitchGapFillPlan(gapPlan)
		} else if job.Platform == "twitch" && !job.IsVod {
			// A live capture that lacks nothing, as ExecuteTwitch's finalize
			// treats one: a row an earlier archive of this job left (its
			// fill's outcome, owner decision PR-R5 — a Retry or a
			// Reinitialize re-recorded it since, and this Mux finalized the
			// new recording) is about footage this archive does not have,
			// and the job's details would go on showing it.
			_ = w.db.DeleteGapFill(jobID)
		}

		// The recording is in the archive now: reclaim staging exactly the way
		// the queued path does, carve-outs included. Without this the
		// off-queue route kept every restart-muxed job's raw recording forever
		// (sweep-2 Task 2 review, finding 1).
		w.cleanupStagingAfterMux(jobID, jobCtx.StagingDir)
	})

	return nil
}

// queuedOrRunning names why jobID's staging belongs to a queue run — its last
// run still registered, or a run queued to start — or answers "" when neither.
// RecoverAsides, MuxJob and ClaimChapterImport refuse on it after taking
// their claim: a reset verb enqueues while it holds the claim (afterJobExit),
// so once the claim is theirs a queued run is visible here.
func (w *DownloadWorker) queuedOrRunning(jobID string) string {
	switch {
	case w.queue.IsProcessing(jobID):
		return "the job's last run is still stopping"
	case w.queue.isPending(jobID):
		return "the job is queued to run"
	}
	return ""
}

// restoreLogRouting puts a job's per-job log routing back where cmd/moombox's
// syncJobLogRouting would have it: tracked while non-terminal, untracked (with
// the buffer kept) once terminal.
//
// The counterpart to the TrackJobForLogs RecoverAsides does on the way in.
// That bracket exists because RouteLogToJobs scans only db.logRouted, which
// holds no terminal job (CORE-12: the status write that makes a job terminal
// untracks it, and the boot seed's SyncJobLogTracking tracks none) — so a
// recovery, which runs ONLY on a terminal job and deliberately never writes a
// status, would emit every one of its log lines into nothing. MuxJob has no
// such problem: it flips the row to Muxing first.
//
// Re-reads the row rather than unconditionally untracking, so a job that was
// resurrected while the recovery ran (a /resume, a retry) is left tracked.
func (w *DownloadWorker) restoreLogRouting(jobID string) {
	fresh, err := w.db.GetJob(jobID)
	if err != nil || fresh == nil || fresh.IsTerminal() {
		w.db.UntrackJobForLogs(jobID)
		return
	}
	w.db.TrackJobForLogs(jobID)
}

// acquireRecoverySlot takes a download slot for a set-aside recovery of
// jobID — the slot a queued job takes, since the recovery starts an FFmpeg
// and num_parallel_downloads bounds exactly that — and returns its release,
// or false when ctx ends first.
//
// Under a key of its own, not the job's: the queue's slots are the job's
// (TryAcquireDownloadSlot), so a run of the same job overlapping the
// recovery would be told it already held this one and either release would
// free it under the other, leaving one FFmpeg uncounted. Nothing should
// start such a run (the heartbeat and the scheduler skip a claimed job), but
// the count must not depend on that.
func (w *DownloadWorker) acquireRecoverySlot(ctx context.Context, jobID string) (func(), bool) {
	key := jobID + "\x00" + string(opRecoverAsides)
	if !w.queue.AcquireDownloadSlot(ctx, key) {
		return func() {}, false
	}
	return func() { w.queue.ReleaseDownloadSlot(key) }, true
}

// RecoverAsides muxes every recording the engine set aside for this job into
// its own file beside the job's archive, carries the kept chat capture beside
// the first of them, and then reclaims the staging directory — but only when
// nothing a mux still owes is left in it (see the guard at the end of the
// goroutine); the remaining carve-outs are cleanupStagingAfterMux's own.
//
// The off-queue twin of MuxJob, and deliberately NOT part of it: MuxJob means
// "mux the recording" and is gated on HasSegmentFiles, which does not know the
// .restart-<ts> suffix — an aside-only staging dir has no recording to mux
// (spec §5, "recovery is its own verb"). The job's STATUS is never written
// here either: the job finished (or failed, or was cancelled) long ago and
// this is not its lifecycle. Progress and failures reach the operator the way
// the aside mux always has, through the job's log lines.
//
// Returns one of the three typed refusals synchronously; a nil return means
// the recovery is running.
func (w *DownloadWorker) RecoverAsides(jobID string) error {
	job, err := w.db.GetJob(jobID)
	if err != nil {
		return fmt.Errorf("look up job %s: %w", jobID, err)
	}
	if job == nil {
		return fmt.Errorf("job %s not found", jobID)
	}
	if IsActiveJobStatus(job.Status) {
		return fmt.Errorf("%w (status %s)", ErrRecoveryJobActive, job.Status)
	}
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) {
		stagingBase = c.Paths.EffectiveStagingDir()
	})
	if len(ScanAsides(stagingBase, jobID).Groups) == 0 {
		return ErrNoAsides
	}
	release, err := w.claimJobOperation(jobID, opRecoverAsides)
	if err != nil {
		return err
	}
	// After the claim, so nothing that starts a run can do so between this
	// look and the recovery: the reset verbs enqueue only while they hold
	// the claim (afterJobExit), and the heartbeat and the scheduler's
	// admission skip a job whose claim is held (enqueueRunnable,
	// Scheduler.stagingBusy). A run still unwinding (the cancel route writes
	// Cancelled before it stops) or one queued to start writes the staging
	// this would mux out of. MuxJob and ClaimChapterImport make the same
	// check after their claim. The row is re-read for the same reason: a
	// reset that ran before the claim has made it active.
	if why := w.queuedOrRunning(jobID); why != "" {
		release()
		return fmt.Errorf("%w (%s)", ErrStagingBusy, why)
	}
	fresh, err := w.db.GetJob(jobID)
	if err != nil || fresh == nil {
		release()
		if err == nil {
			err = fmt.Errorf("job %s not found", jobID)
		}
		return err
	}
	if IsActiveJobStatus(fresh.Status) {
		release()
		return fmt.Errorf("%w (status %s)", ErrRecoveryJobActive, fresh.Status)
	}
	job = fresh
	// The recovery keeps the row's status, so a Cancelled or Error job's
	// staging still reads as an orphan to the Files sweep — offered for
	// deletion while FFmpeg reads the asides out of it. Claimed for the run.
	releaseStaging := claimOutputStem(jobID, filepath.Join(stagingBase, jobID))
	// The job is terminal, so nothing is routing its log lines (CORE-12).
	// Both UIs point the operator at the job's log for this run's progress, so
	// route to it for the duration and hand it back at the end. Nothing that
	// happens to another job ends it: cmd/moombox re-routes a job only on its
	// own events — its OnJobAdded, a status write through OnJobChange, its
	// OnJobDeleted — the bulk writers' OnJobsChange leaves routing alone
	// (onJobsChange), and the channel prune drops only the rows it deleted
	// (DeleteJobsAndHistoryForChannel). A status write on THIS job does
	// re-route it by its new status, and deleting it ends the routing with
	// the row.
	w.db.TrackJobForLogs(jobID)

	w.wg.Go(func() {
		// Declared FIRST so it runs LAST: the recover handler's own line still
		// has to reach the job's log.
		defer w.restoreLogRouting(jobID)
		defer func() {
			if r := recover(); r != nil {
				// No status write: a panic in the recovery must not turn a
				// Finished job into an Error one. The asides are still in
				// staging, where the shield holds them for another attempt.
				w.logger.Error("panic in RecoverAsides", "jobID", jobID, "panic", fmt.Sprint(r))
			}
		}()
		defer release()
		defer releaseStaging()

		jobCtx := w.buildJobContext(job)
		// The orchestrator's mux root, never context.Background(), for the
		// reason MuxJob gives: a Stop reaches this FFmpeg instead of leaving
		// it writing into a staging dir.
		ctx := w.orchestrator.muxRoot()

		// A download slot of its own (acquireRecoverySlot) — this starts an FFmpeg, and
		// num_parallel_downloads exists to bound exactly that.
		releaseSlot, ok := w.acquireRecoverySlot(ctx, jobID)
		if !ok {
			w.logger.Info("RecoverAsides: shutdown while waiting for a mux slot; the recordings stay in staging", "jobID", jobID)
			return
		}
		defer releaseSlot()

		if err := w.orchestrator.recoverAsides(ctx, jobCtx); err != nil {
			if ctx.Err() != nil {
				w.logger.Info("RecoverAsides: cancelled by shutdown; the recordings stay in staging", "jobID", jobID)
				return
			}
			w.logger.Error("set-aside recovery did not finish; the recordings it could not mux stay in staging",
				"jobID", jobID, "err", err)
			return
		}

		// Every aside is out of staging now — but that is NOT enough to hand
		// the directory to cleanupStagingAfterMux. Its shields are: asides
		// present (just consumed), hasUnmuxedParts (FALSE for a single-file
		// job, because hasUnmuxedSegmentParts returns false with no seg_N
		// dirs), unusedRootRecording (for a single-file job, only a
		// whole-file download beside a live-shape capture — never the one
		// recording a root holds), IncompleteTail, and chat-incomplete. A
		// Cancelled or Error job whose staging holds both the fresh video.mp4
		// and an aside — the commonest shape after a mid-stream restart, and
		// exactly what /mux and A M exist to rescue — falls through every one
		// of them to os.RemoveAll. Recovering the asides would delete the
		// main recording.
		//
		// So: reclaim only when the directory holds no recognised media
		// (discoverStagingMedia, the same discovery muxFromStaging uses) and
		// no unmuxed part. The remaining jobNeedsStaging terms — the
		// incomplete tail and the chat capture — are cleanupStagingAfterMux's
		// own, and it still prunes rather than deletes for the chat one.
		//
		// …and the ROW has to be re-read first, because the decision above is
		// about a directory somebody else may now own. This verb writes no
		// status, so for its whole run the row still reads
		// Error/Cancelled/Finished — exactly what /retry, /resume and
		// /reinitialize accept — and a row parked at COOKIES? is promoted to
		// Upcoming by the credential sweep with no operator action at all. A
		// revival hands the SAME staging directory to a fresh download whose
		// first files are not yet a name discoverStagingMedia knows, so the
		// media guard would wave it through and the cleanup would os.RemoveAll
		// a live capture. MuxJob is immune only because it flips the row to
		// Muxing before it spawns; the claim covers the two off-queue verbs,
		// not the queue.
		fresh, _ := w.db.GetJob(jobID)
		if fresh == nil || IsActiveJobStatus(fresh.Status) {
			status := "row deleted"
			if fresh != nil {
				status = string(fresh.Status)
			}
			w.logger.Warn("set-aside recordings recovered; staging kept: the job is no longer the terminal row this recovery started from",
				"status", status, "path", jobCtx.StagingDir, "jobID", jobID)
			return
		}
		if discoverStagingMedia(jobCtx.StagingDir) == nil && !w.hasUnmuxedParts(jobID, jobCtx.StagingDir) {
			w.cleanupStagingAfterMux(jobID, jobCtx.StagingDir)
			return
		}
		w.logger.Warn("set-aside recordings recovered; staging kept: unmuxed recording present",
			"path", jobCtx.StagingDir, "jobID", jobID)
	})

	return nil
}
