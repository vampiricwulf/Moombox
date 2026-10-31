package worker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// A YouTube rate limit (youtube.PlayabilityRateLimited): the player refused
// the video with "This content isn't available, try again later", which
// yt-dlp explains as the account or session being rate-limited by YouTube
// for up to an hour (26feac3dd, #12958), or with a captcha challenge
// (f484c5159, #12939). Upstream words the refusal and ends the extraction —
// one command, one video, and the operator runs it again later. An archiver
// is the one that runs it again, so here the verdict is transient
// (ErrRateLimited) and a job waits it out rather than ending in Error, which
// is going beyond upstream:
//
//   - a backlog VOD goes back to Queued, held for rateLimitBackoff, outside
//     backlogRetryLimit — a wall the whole session hits says nothing about
//     the video, and three of them in a row ended a backlog that had done
//     nothing wrong in Error (requeueBacklogAfterRateLimit);
//   - and the verdict holds every backlog admission for as long, so the
//     archive slot it frees does not feed the channel's next VOD into the
//     same wall (Scheduler.holdForRateLimit) — before, the backlog drained
//     into Error one admission after another while the limit lasted;
//   - a job that is not backlog — a broadcast, a newly published video, a
//     manual add — keeps its row's status and fetches again every
//     rateLimitRetryInterval (outlastRateLimit). Its "Stream Found" waits for
//     the fetch that answers (Process). A broadcast whose first fetch was
//     refused used to end in Error before it aired, and nothing created its
//     job again, since the feed monitor never re-creates a video it has a job
//     for.

// rateLimitBackoff is how long a backlog VOD a rate-limit verdict sent back
// to Queued is held from re-admission, and how long the verdict holds every
// backlog admission: the hour yt-dlp says the limit lasts.
const rateLimitBackoff = time.Hour

// rateLimitRetryInterval is how long a job that is not backlog waits in
// place before its stream processing fetches again: the waiting loop's own
// full-fetch cadence (fullFetchInterval), the rate at which a waiting
// broadcast already makes the full extraction a refusal comes back on. A
// fetch in the meantime would only be the same request sent into the same
// limit, and a broadcast that went live while it waited is captured from its
// start, as any live-from-start capture is.
const rateLimitRetryInterval = fullFetchInterval

// rateLimitWaitRefresh is how often the waiting job's progress line is
// rewritten with the time left. The wait is half an hour, so once a minute.
const rateLimitWaitRefresh = time.Minute

// rateLimitWaitProgress opens the progress line a job waiting out a rate
// limit shows (outlastRateLimit), as one waiting out an outage shows
// networkWaitProgress; rateLimitWaitLine adds the time left.
const rateLimitWaitProgress = "Rate-limited by YouTube; fetching again"

// rateLimitWaitLine is the progress line itself, "Rate-limited by YouTube;
// fetching again... (24m 0s left)". The "... (" is the TUI's wait marker
// (internal/tui isWaitProgress), which every wait line carries.
func rateLimitWaitLine(left time.Duration) string {
	return rateLimitWaitProgress + "... (" + utils.FormatDurationHuman(left) + " left)"
}

// rateLimitVerdictMessage words a rate_limited verdict as yt-dlp does
// (_video.py:4074-4081 at b77c9304e). A captcha's reason already ends with
// upstream's sentence (youtube.CaptchaChallengeNote); a rate limit's gets
// upstream's explanation, naming the account when the request was signed in
// (upstream's is_authenticated; here YouTube's own word on the session,
// SessionAuth) and the session otherwise. Upstream's advice to add `-t sleep`
// between requests is its CLI's; what this archiver does instead is the
// caller's log line.
func rateLimitVerdictMessage(reason string, auth youtube.SessionAuthState) string {
	if strings.HasSuffix(reason, youtube.CaptchaChallengeNote) {
		return reason
	}
	who := "The current session"
	if auth == youtube.SessionAuthLoggedIn {
		who = "Your account"
	}
	return fmt.Sprintf("%s. %s has been rate-limited by YouTube for up to an hour",
		strings.TrimSuffix(strings.TrimSpace(reason), "."), who)
}

// rateLimitedFetch reports whether a full fetch came back refused for a rate
// limit with nothing to download. The waiting loop's go-live confirmation
// reads it as no answer at all: neither a stream still upcoming, to be
// confirmed again on the next probe — the whole cascade into the limit every
// 30 s — nor a go-live, which wrote Live and went on to a download with no
// formats. completeStreamTransition hands it back as the refusal it is.
func rateLimitedFetch(info *youtube.VideoInfo) bool {
	return info != nil && info.PlayabilityError == youtube.PlayabilityRateLimited && probeFormatCount(info) == 0
}

// outlastRateLimit is processJob's handling of a run YouTube refused with a
// rate-limit verdict (ErrRateLimited), from stream processing or from a stale
// VOD's re-extraction. The verdict holds backlog admission first, whatever
// the job is. A backlog VOD then goes back to Queued for its backoff
// (requeueBacklogAfterRateLimit), or, when it cannot, ends as setJobError
// ends it — a Cancel that landed meanwhile is reported as one. Any other job
// gives back its slots, waits rateLimitRetryInterval in place under the
// wait's progress line, and is run again from stream processing.
//
// Reports whether the run goes on: when it does, the row as it now reads, or
// nil when the run is not to go on — ctx ended, or the row was deleted or
// left the states the queue processes while it waited (waitOutOutage's
// re-read) — which processJob routes to handleCancellation. since is when
// the run first waited on a rate limit (zero: not yet), for the log.
func (w *DownloadWorker) outlastRateLimit(ctx context.Context, job *database.Job, err error, since *time.Time) (*database.Job, bool) {
	if w.scheduler != nil {
		w.scheduler.holdForRateLimit(time.Now(), rateLimitBackoff)
	}
	if CookieResumeStatus(job) == database.StatusQueued {
		if requeued, err := w.requeueBacklogAfterRateLimit(job, err); !requeued {
			w.setJobError(job, err)
		}
		return nil, false
	}
	logWait := w.logger.Warn
	if !since.IsZero() {
		logWait = w.logger.Debug
	} else {
		*since = time.Now()
	}
	logWait("YouTube rate-limited this job's fetch; the job keeps its status and fetches again",
		"jobID", job.ID, "status", job.Status, "retryIn", rateLimitRetryInterval, "err", err)
	w.queue.ReleaseSlots(job.ID)
	if !w.waitOutRateLimit(ctx, job.ID) {
		return nil, true
	}
	cur, gerr := w.db.GetJob(job.ID)
	if gerr != nil {
		// A database error must not end a job that is waiting for nothing
		// else: go on, as waitOutOutage does.
		return job, true
	}
	if cur == nil || !ShouldProcess(cur) {
		return nil, true
	}
	return cur, true
}

// waitOutRateLimit holds jobID's run for rateLimitRetryInterval under the
// wait's progress line, rewritten with the time left and cleared however the
// wait ends, and reports false when ctx ends first.
func (w *DownloadWorker) waitOutRateLimit(ctx context.Context, jobID string) bool {
	until := time.Now().Add(rateLimitRetryInterval)
	w.db.UpdateJobFields(jobID, map[string]any{"progress": rateLimitWaitLine(rateLimitRetryInterval)})
	defer w.db.UpdateJobFields(jobID, map[string]any{"progress": ""})
	done := time.NewTimer(rateLimitRetryInterval)
	defer done.Stop()
	refresh := time.NewTicker(rateLimitWaitRefresh)
	defer refresh.Stop()
	for {
		select {
		case <-done.C:
			return true
		case <-refresh.C:
			w.db.UpdateJobFields(jobID, map[string]any{"progress": rateLimitWaitLine(time.Until(until))})
		case <-ctx.Done():
			return false
		}
	}
}

// holdForRateLimit holds every backlog admission for d from now — or past
// the hold already standing, when that one runs out later. A rate limit is
// the account's or the address's, not the video's: an admission while it
// stands is a backlog VOD sent into the same wall, so sweep admits nothing
// until the hold runs out (rateLimitGateClosed), and a fresh verdict meanwhile
// pushes it on. Only backlog waits, as on a full disk: live, upcoming and
// manually added jobs never pass through the scheduler, and a backlog VOD
// already admitted runs on. All backlog is YouTube's today; a Twitch backlog
// would want its own hold. In memory, like the per-job holds: a restart
// forgets it.
//
// Called from any job's run, while sweep reads it on Run's goroutine.
func (s *Scheduler) holdForRateLimit(now time.Time, d time.Duration) {
	until := now.Add(d)
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	if !until.After(s.rateLimitedUntil) {
		return
	}
	opening := !s.rateHeld
	s.rateLimitedUntil, s.rateHeld = until, true
	if opening {
		s.log.Warn("scheduler: YouTube rate-limited this session; backlog VODs wait in Queued until the hold runs out (live and manually added jobs are not held)",
			"until", until.UTC().Format(time.RFC3339))
		return
	}
	s.log.Debug("scheduler: another rate-limit verdict; the backlog hold is extended",
		"until", until.UTC().Format(time.RFC3339))
}

// rateLimitGateClosed reports whether a rate-limit hold stands at now, and
// logs, once, the sweep that finds it run out.
func (s *Scheduler) rateLimitGateClosed(now time.Time) bool {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	if !s.rateHeld {
		return false
	}
	if now.Before(s.rateLimitedUntil) {
		return true
	}
	s.rateHeld = false
	s.log.Info("scheduler: the YouTube rate-limit hold has run out; backlog admission resumes")
	return false
}
