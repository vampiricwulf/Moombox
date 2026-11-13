package worker

import (
	"fmt"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobstate"
)

const (
	// backlogRetryLimit bounds how many times in a row one backlog job goes
	// back to Queued — after a transient pre-download failure or a download
	// that ran out of disk — before the failure is taken at its word and the
	// job ends in Error. A video that is really gone answers the same way
	// every time, and must still reach Error.
	backlogRetryLimit = 3
	// backlogRetryBackoff is the first retry's delay; each later one doubles
	// it (5, 10, 20 minutes), so the budget spans about half an hour — past
	// a blip, and past the minutes a connectivity monitor can take to call
	// an outage, after which the scheduler holds admission entirely.
	backlogRetryBackoff = 5 * time.Minute
)

// requeueBacklogAfterTransientFailure sends a backlog VOD whose pre-download
// fetch failed transiently back to Queued, held from re-admission for a
// backoff, instead of ending it in Error. Reports whether it did; when it did
// not, the error to record is returned — err itself, or err saying the retry
// budget is spent.
//
// The case it exists for: during an outage the scheduler kept admitting a
// channel's backlog, each admitted VOD failed its first GetVideoInfo straight
// into Error and freed its archive slot for the next, and the whole Queued
// backlog drained into Error in minutes. Nothing re-created them — the
// archival pass skips videos it has history for — so nothing retried them.
// The scheduler now holds admission while the connectivity monitor reports
// offline, but a monitor calls an outage some seconds in, and not every
// failure that will pass is an outage.
//
// Transient is classifyProbeErr's verdict — network, timeout, 429/5xx and
// everything it cannot place; a definitive refusal (a 404, a playability
// verdict) is not retried. A stale VOD's re-extraction returns its verdict
// on the video as an error, whose text classifyProbeErr cannot place, so
// that answer is recognised first (vodRefreshVerdict): a backlog VOD that
// went private or members-only while it queued ends where the verdict sends
// it, as one refused up front does. Which jobs qualify is requeueBacklog's
// rule. An Innertube 429 that reaches processJob typed
// (youtube.HTTPRateLimitError) never comes here: it is YouTube rate-limiting
// the session, which outlastRateLimit waits out (rate_limit.go).
func (w *DownloadWorker) requeueBacklogAfterTransientFailure(job *database.Job, err error) (bool, error) {
	if isVodRefreshVerdict(err) || classifyProbeErr(err) != classNetwork {
		return false, err
	}
	return w.requeueBacklog(job, err, "backlog VOD's pre-download fetch failed; back to Queued for a retry")
}

// requeueBacklogAfterDiskFull is the same retry for a backlog VOD whose
// download — or the mux after it — failed because the disk is full
// (isDiskFull). The scheduler admits no backlog while the volume reads at
// or past disk_critical_percent, but a VOD admitted below it can still fill
// what is left, and a full disk is something the operator fixes: ending the
// job in Error left it there after they had, with nothing to retry it. Held
// for the backoff, and then for as long as the disk gate stays closed.
//
// Staging is kept. A whole-file VOD resumes from its last resume
// checkpoint; one whose MUX ran out of space downloads again, since a
// completed download clears its resume state.
func (w *DownloadWorker) requeueBacklogAfterDiskFull(job *database.Job, err error) (bool, error) {
	if !isDiskFull(err) {
		return false, err
	}
	return w.requeueBacklog(job, err, "backlog VOD ran out of disk space; back to Queued for a retry")
}

// requeueBacklog is the requeue both of the above make, once their own
// predicate has called the failure worth retrying.
//
// Backlog only, and only where Queued can be left again: CookieResumeStatus —
// the rule a cookie repair's re-queue follows — answers Queued for a
// queue_priority 1 job of a channel, and nothing else. A broadcast or a
// manually added video fails visibly, as before, rather than waiting in a
// state the operator did not put it in. A backlog row of a removed channel is
// requeued like the rest: the scheduler admits it with no feed_items partner.
//
// The budget counts the job's runs that ended back in Queued, of either
// kind, and only a run that ends some other way resets it
// (forgetBacklogRetries) — not a fetch that succeeds, since a download that
// then runs out of disk follows one every time — or an operator's verb that
// takes the job out of the loop (endBacklogStreak). A run a rate-limit
// verdict sent back to Queued (requeueBacklogAfterRateLimit) is neither
// counted nor resets it.
func (w *DownloadWorker) requeueBacklog(job *database.Job, err error, what string) (bool, error) {
	if w.scheduler == nil {
		return false, err
	}
	if CookieResumeStatus(job) != database.StatusQueued {
		return false, err
	}
	attempt := w.noteBacklogRetry(job.ID)
	if attempt > backlogRetryLimit {
		w.forgetBacklogRetries(job.ID)
		return false, fmt.Errorf("%w (gave up after %d retries)", err, backlogRetryLimit)
	}
	delay := backlogRetryBackoff << (attempt - 1)
	if !w.backToQueued(job, delay, attempt) {
		return false, err
	}
	w.logger.Warn(what,
		"jobID", job.ID, "attempt", attempt, "of", backlogRetryLimit, "retryIn", delay, "err", err)
	return true, nil
}

// requeueBacklogAfterRateLimit sends a backlog VOD that YouTube refused with
// a rate-limit verdict (ErrRateLimited) back to Queued, held for
// rateLimitBackoff (rate_limit.go). Outside backlogRetryLimit: the limit is
// the session's, so the job's streak neither counts it nor ends with it, and
// a rate limit that outlasts three hourly retries does not end a backlog VOD
// in Error. Only the rate-limit cap does: a spell of refusals that has lasted
// rateLimitCap with at least rateLimitCapRefusals of them, which
// outlastRateLimit checks before it calls this. Which jobs qualify is
// requeueBacklog's rule; a job that does not is outlastRateLimit's to wait
// for in place.
func (w *DownloadWorker) requeueBacklogAfterRateLimit(job *database.Job, err error) (bool, error) {
	if w.scheduler == nil || CookieResumeStatus(job) != database.StatusQueued {
		return false, err
	}
	if !w.backToQueued(job, rateLimitBackoff, 0) {
		return false, err
	}
	w.logger.Warn("YouTube rate-limited the backlog VOD's fetch; back to Queued, retried in about an hour (not counted against its retries)",
		"jobID", job.ID, "retryIn", rateLimitBackoff, "err", err)
	return true, nil
}

// backToQueued is the requeue itself, once a caller has decided on it: the
// job's slots released and its row back in Queued, held from re-admission
// for delay. attempt is the retry attempt the hold is for, 0 for a
// rate-limit requeue (Scheduler.ownHold). Reports whether it happened.
//
// The hold is placed BEFORE the status write, so no sweep can see the row
// Queued and unheld; the slots are released before it, so the next download
// does not wait for this run's exit.
func (w *DownloadWorker) backToQueued(job *database.Job, delay time.Duration, attempt int) bool {
	w.queue.ReleaseSlots(job.ID)
	w.scheduler.holdUntil(job.ID, time.Now().Add(delay), attempt)
	// Never over an operator's Cancel, which the scheduler would then have
	// admitted again — the job downloading after all: not when one flagged
	// this run before the requeue settled (JobQueue.settle), and not over a
	// Cancelled row. Not requeued, the failure goes to setJobError, which
	// ends the run as a cancelled one. Requeued, the run is settled, and a
	// Cancel of the Queued row is its caller's to report.
	if w.queue.settle(job.ID) || !w.db.UpdateJobFieldsUnless(job.ID, database.StatusCancelled, map[string]any{
		"status": database.StatusQueued,
		"error":  "",
	}) {
		w.scheduler.unhold(job.ID)
		return false
	}
	return true
}

// BacklogRetryState is jobID's place in its retry streak and its own hold:
// the retry attempt its hold was placed for (0 for a hold a rate-limit
// requeue placed, which the streak does not count, whatever the streak
// stands at — Scheduler.ownHold), the budget, and when the scheduler may
// admit it again. ok is false when no hold of its own stands at now.
// Read-only, for both UIs' Queued-row line (owner ruling R-D3); the streak
// itself lives in memory, so a restart forgets it, by design.
func (w *DownloadWorker) BacklogRetryState(jobID string, now time.Time) (hold jobstate.BacklogHold, ok bool) {
	if w.scheduler == nil {
		return hold, false
	}
	own, held := w.scheduler.Holds(now).Jobs[jobID]
	if !held {
		return hold, false
	}
	return jobstate.BacklogHold{Attempt: own.Attempt, Of: backlogRetryLimit, Until: own.Until}, true
}

// AdmissionHolds is the snapshot both UIs show on Queued rows (owner ruling
// R-D3): the scheduler's holds (Scheduler.Holds), each held row's attempt
// the one its hold was placed for, with the retry budget filled in. The zero
// value when the worker has no scheduler. The snapshot also carries each
// job's automatic cookie retries (cookieRetrySnapshot, owner ruling R2-C1),
// which hold no admission: it is what already reaches both UIs.
func (w *DownloadWorker) AdmissionHolds() jobstate.AdmissionHolds {
	if w.scheduler == nil {
		return jobstate.AdmissionHolds{}
	}
	now := time.Now()
	h := w.scheduler.Holds(now)
	for id, own := range h.Jobs {
		own.Of = backlogRetryLimit
		h.Jobs[id] = own
	}
	h.CookieRetries = w.cookieRetrySnapshot(w.cookieRetryTime())
	return h
}

// AdmissionHoldsChanged is signalled whenever AdmissionHolds may answer
// differently (Scheduler.noteHoldsChanged); cmd/moombox drains it and pushes
// the new snapshot to the dashboard (admission_holds) and the TUI. nil with
// no scheduler.
func (w *DownloadWorker) AdmissionHoldsChanged() <-chan struct{} {
	if w.scheduler == nil {
		return nil
	}
	return w.scheduler.holdsChanged
}

// noteBacklogRetry counts one more consecutive requeue for jobID and returns
// the count.
func (w *DownloadWorker) noteBacklogRetry(jobID string) int {
	w.backlogRetryMu.Lock()
	defer w.backlogRetryMu.Unlock()
	if w.backlogRetries == nil {
		w.backlogRetries = map[string]int{}
	}
	w.backlogRetries[jobID]++
	return w.backlogRetries[jobID]
}

// forgetBacklogRetries resets jobID's count: a run of it ended other than
// back in Queued — its download finished, it ended in Error or COOKIES?, or
// it was cancelled — or its budget is spent and it is ending in Error. Its
// spell of rate-limit refusals goes with it (rateLimitSpells): a run that
// ended is not waiting a limit out, and the next one starts the day afresh.
func (w *DownloadWorker) forgetBacklogRetries(jobID string) {
	w.rateLimitSpells.end(jobID)
	w.backlogRetryMu.Lock()
	defer w.backlogRetryMu.Unlock()
	delete(w.backlogRetries, jobID)
}

// endBacklogStreak ends jobID's retry streak from outside a run: its count
// and its hold both go. A job waiting in Queued for its backoff or for disk
// space has no run to end it, so an operator's Cancel there — and a Retry or
// Resume after it — kept the count: the retried job's first failure that
// went back to Queued was counted on top of the old streak, and after three
// requeues it ended in Error, "gave up after 3 retries", without one retry of
// its own. CancelJob, the two restart verbs and a deleted row call it; a
// hold left behind would also widen every sweep's NextQueuedJobs for a row
// that is no longer Queued.
func (w *DownloadWorker) endBacklogStreak(jobID string) {
	w.forgetBacklogRetries(jobID)
	if w.scheduler != nil {
		w.scheduler.unhold(jobID)
	}
}

// endStreaksGoneFrom ends the streak of every job counted here whose row is
// not in jobs — an OnJobsChange full list, the rows that exist. The channel
// prune deletes a departed channel's Queued rows in bulk and fires no
// OnJobDeleted for them, so a row pruned mid-backoff left its count and hold
// behind: when the channel was added back, the rescan's new row for the same
// video waited out a backoff it never earned and counted its first requeue on
// top of the old streak, and the hold widened every sweep's NextQueuedJobs
// until a restart.
//
// The counts and the rate-limit spells are the whole set to look through:
// requeueBacklog notes the retry before it places the hold, and
// outlastRateLimit notes the spell before a rate limit's requeue places its
// hold, which no count notes; and every hold is dropped with its count or
// spell or gone before either is (sweep admits a job only once its hold has
// run out, and held forgets it then), so a held job is always one of them.
//
// The list is the bulk write's own snapshot, delivered after it returns. A
// row created after the snapshot was read is not on it, but it has had no run
// yet to earn a streak of its own, so whatever this ends for it is stale.
func (w *DownloadWorker) endStreaksGoneFrom(jobs []*database.Job) {
	exists := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		exists[j.ID] = true
	}
	var gone []string
	w.backlogRetryMu.Lock()
	for id := range w.backlogRetries {
		if !exists[id] {
			gone = append(gone, id)
		}
	}
	w.backlogRetryMu.Unlock()
	// And the rate-limit spells, which a backlog VOD the limit alone sent
	// back to Queued holds with no count (requeueBacklogAfterRateLimit).
	for _, id := range w.rateLimitSpells.ids() {
		if !exists[id] {
			gone = append(gone, id)
		}
	}
	for _, id := range gone {
		w.endBacklogStreak(id)
	}
}
