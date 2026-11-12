package worker

import (
	"fmt"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobstate"
)

// The automatic cookie retry budget (owner ruling R2-C1).
//
// A job parked in COOKIES? on its credentials is resumed automatically when
// a cookie refresh, an import or a setup ends with the platform verified.
// Nothing limited that: a job the platform kept refusing while the session
// verified (a bot-checked IP is the likely case) asked for a refresh, was
// "restored" and retried about once a minute, each lap a full extraction, a
// headless-browser launch, a rewrite of cookies.txt and an "Authentication
// Required" notification, until the operator cancelled it.
//
// Each job now gets jobstate.MaxCookieAutoRetries automatic retries. Every
// automatic resume of an auth park counts one (NoteCookieRetry): the
// worker's own after the refresh it asked for, and each credential sweep's
// (cmd/moombox resumeCookieParkedJobs). With none left the job asks for no
// refresh and no cookie-writing gesture resumes it; it stays in COOKIES?
// with an error text that says the cookies are not the cause.
//
// The count resets jobstate.CookieRetryWindow after the last retry, when a
// run passes the playability check (processJob), and when the operator
// deletes, cancels, resumes or reinitializes the job. It lives in memory, so
// a restart resets it too. A membership park is never counted: an account
// change settles it, one retry per change (cmd/moombox sweepShouldResume).

// cookieRetryState is one job's automatic cookie retries so far.
type cookieRetryState struct {
	count int
	last  time.Time
}

func (w *DownloadWorker) cookieRetryTime() time.Time {
	if w.cookieRetryClock != nil {
		return w.cookieRetryClock()
	}
	return time.Now()
}

// cookieRetryCount is how many automatic cookie retries jobID has used
// inside the window; 0 once the window has passed.
func (w *DownloadWorker) cookieRetryCount(jobID string) int {
	now := w.cookieRetryTime()
	w.cookieRetryMu.Lock()
	defer w.cookieRetryMu.Unlock()
	st, ok := w.cookieRetries[jobID]
	if !ok || !now.Before(st.last.Add(jobstate.CookieRetryWindow)) {
		return 0
	}
	return st.count
}

// CookieRetryLeft reports whether jobID still has an automatic cookie retry.
func (w *DownloadWorker) CookieRetryLeft(jobID string) bool {
	return w.cookieRetryCount(jobID) < jobstate.MaxCookieAutoRetries
}

// NoteCookieRetry counts one automatic cookie retry for jobID and returns
// the count, which never passes the budget: a resume the budget does not
// gate (a real auth recovery or account change) still shows as "3 of 3".
// The caller has just resumed the job out of an auth park.
func (w *DownloadWorker) NoteCookieRetry(jobID string) int {
	now := w.cookieRetryTime()
	w.cookieRetryMu.Lock()
	st := w.cookieRetries[jobID]
	if !now.Before(st.last.Add(jobstate.CookieRetryWindow)) {
		st.count = 0
	}
	if st.count < jobstate.MaxCookieAutoRetries {
		st.count++
	}
	st.last = now
	if w.cookieRetries == nil {
		w.cookieRetries = map[string]cookieRetryState{}
	}
	w.cookieRetries[jobID] = st
	w.cookieRetryMu.Unlock()
	w.noteCookieRetriesChanged()
	return st.count
}

// forgetCookieRetries gives jobID its full budget back: a run of it passed
// the playability check, or the operator deleted, cancelled, resumed or
// reinitialized it.
func (w *DownloadWorker) forgetCookieRetries(jobID string) {
	w.cookieRetryMu.Lock()
	_, had := w.cookieRetries[jobID]
	delete(w.cookieRetries, jobID)
	w.cookieRetryMu.Unlock()
	if had {
		w.noteCookieRetriesChanged()
	}
}

// forgetCookieRetriesGoneFrom drops the count of every job whose row is not
// in jobs, an OnJobsChange full list: a bulk delete fires no OnJobDeleted
// (endStreaksGoneFrom has the same reason).
func (w *DownloadWorker) forgetCookieRetriesGoneFrom(jobs []*database.Job) {
	exists := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		exists[j.ID] = true
	}
	changed := false
	w.cookieRetryMu.Lock()
	for id := range w.cookieRetries {
		if !exists[id] {
			delete(w.cookieRetries, id)
			changed = true
		}
	}
	w.cookieRetryMu.Unlock()
	if changed {
		w.noteCookieRetriesChanged()
	}
}

// cookieRetrySnapshot is every job's count inside the window at now, for
// AdmissionHolds; nil when there is none.
func (w *DownloadWorker) cookieRetrySnapshot(now time.Time) map[string]jobstate.CookieRetry {
	w.cookieRetryMu.Lock()
	defer w.cookieRetryMu.Unlock()
	var out map[string]jobstate.CookieRetry
	for id, st := range w.cookieRetries {
		if st.count <= 0 || !now.Before(st.last.Add(jobstate.CookieRetryWindow)) {
			continue
		}
		if out == nil {
			out = map[string]jobstate.CookieRetry{}
		}
		out[id] = jobstate.CookieRetry{Count: st.count, Of: jobstate.MaxCookieAutoRetries, Last: st.last}
	}
	return out
}

// noteCookieRetriesChanged has the new snapshot pushed to both UIs, on the
// signal the scheduler's holds use (AdmissionHoldsChanged). Never called
// with cookieRetryMu held.
func (w *DownloadWorker) noteCookieRetriesChanged() {
	if w.scheduler != nil {
		w.scheduler.noteHoldsChanged()
	}
}

// cookieRetriesSpentText is the error text of a job parked with no automatic
// cookie retry left: the failure it parked on, then why nothing retries it.
func cookieRetriesSpentText(platform, errMsg string) string {
	name := "the platform"
	switch platform {
	case "youtube":
		name = "YouTube"
	case "twitch":
		name = "Twitch"
	}
	note := fmt.Sprintf("The cookies verify as signed in and %s still refuses this video after %d automatic retries, so the cookies are not the cause. No more automatic retries for now.",
		name, jobstate.MaxCookieAutoRetries)
	if errMsg == "" {
		return note
	}
	return errMsg + " — " + note
}

// parkWithCookieRetriesSpent leaves job in COOKIES? with no refresh asked
// for: one Warn, and the row's error text says why (owner ruling R2-C1).
// Only while the row is still parked, so an operator's verb that landed
// since stands.
func (w *DownloadWorker) parkWithCookieRetriesSpent(job *database.Job, errMsg string) {
	w.logger.Warn("no automatic cookie retry left for this job — it stays parked and no cookie refresh is requested: the session verifies and the platform still refuses the video, so the cookies are not the cause",
		"jobID", job.ID,
		"videoID", job.VideoID,
		"platform", job.Platform,
		"retries", jobstate.MaxCookieAutoRetries,
		"next", "it retries by itself again 24 hours after the last retry; Resume or Reinitialize retries it now")
	w.db.UpdateJobFieldsIf(job.ID, database.StatusCookies, map[string]any{
		"error": cookieRetriesSpentText(job.Platform, errMsg),
	})
}
