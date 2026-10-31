package worker

import (
	"context"
	"errors"
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
// (f484c5159, #12939) — or, in its HTTP form, with a 429 on every client of
// the cascade (youtube.HTTPRateLimitError; isRateLimited). Upstream words
// the refusal and ends the extraction — one command, one video, and the
// operator runs it again later. An archiver is the one that runs it again,
// so here the verdict is transient (ErrRateLimited) and a job waits it out
// rather than ending in Error, which is going beyond upstream:
//
//   - a backlog VOD goes back to Queued, held for rateLimitBackoff, outside
//     backlogRetryLimit — a wall the whole session hits says nothing about
//     the video, and three of them in a row ended a backlog that had done
//     nothing wrong in Error (requeueBacklogAfterRateLimit);
//   - and the verdict holds every backlog admission for as long, so the
//     archive slot it frees does not feed the channel's next VOD into the
//     same wall (Scheduler.holdForRateLimit) — before, the backlog drained
//     into Error one admission after another while the limit lasted. A job
//     that waits in place holds it on its first refusal only, never on the
//     ones it meets as it fetches again (outlastRateLimit);
//   - a job that is not backlog — a broadcast, a newly published video, a
//     manual add — keeps its row's status and fetches again every
//     rateLimitRetryInterval (outlastRateLimit). Its "Stream Found" waits for
//     the fetch that answers (Process). A broadcast whose first fetch was
//     refused used to end in Error before it aired, and nothing created its
//     job again, since the feed monitor never re-creates a video it has a job
//     for;
//   - and a broadcast already in its waiting loop stays there, its chat and
//     its probe running, and confirms its go-live again on the same cadence
//     (loopRateLimit).

// rateLimitBackoff is how long a backlog VOD a rate-limit verdict sent back
// to Queued is held from re-admission, and how long the verdict holds every
// backlog admission: the hour yt-dlp says the limit lasts.
const rateLimitBackoff = time.Hour

// rateLimitRetryInterval is how long a job that is not backlog waits in
// place before its stream processing fetches again, and how long the
// waiting loop holds off a go-live confirmation YouTube refused
// (loopRateLimit): the waiting loop's own full-fetch cadence
// (fullFetchInterval), the rate at which a waiting broadcast already makes
// the full extraction a refusal comes back on. A fetch in the meantime would
// only be the same request sent into the same limit, and a broadcast that
// went live while it waited is captured from its start, as any
// live-from-start capture is. A 429's Retry-After can lengthen the wait in
// place (rateLimitWait).
const rateLimitRetryInterval = fullFetchInterval

// rateLimitWaitRefresh is how often the waiting job's progress line is
// rewritten with the time left. The wait is half an hour, so once a minute.
const rateLimitWaitRefresh = time.Minute

// rateLimitWaitProgress opens the progress line a job waiting out a rate
// limit shows (outlastRateLimit, loopRateLimit), as one waiting out an
// outage shows networkWaitProgress; rateLimitWaitLine adds the time left.
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
// limit with nothing to download. The waiting loop reads it as no answer at
// all (loopRateLimit): neither a stream still upcoming, to be confirmed again
// on the next probe — the whole cascade into the limit every 30 s — nor a
// go-live, which wrote Live and went on to a download with no formats.
func rateLimitedFetch(info *youtube.VideoInfo) bool {
	return info != nil && info.PlayabilityError == youtube.PlayabilityRateLimited && probeFormatCount(info) == 0
}

// loopRateLimit is the waiting loop's memory of the rate limit its full
// fetches met (waitForLive). A broadcast that is waiting stays in its loop
// through one — its early chat running, its status probe polling — rather
// than going back to processJob to wait the limit out: it used to be handed
// back as the refusal, which stopped the waiting-room chat and left the job
// with no probe at all for the half hour processJob waits, so a stream that
// had just gone live lost its chat, and on a stream without DVR its video,
// for as long as the wait lasted, however soon the limit cleared.
//
// What the loop holds off is the go-live confirmation, the full cascade the
// live and VOD arms make when the status probe says the stream has begun:
// once YouTube has refused one, the next is made rateLimitRetryInterval
// after it, as processJob's wait in place would make it, under the same
// progress line, and the arms read the probe's "live" as nothing until then.
// The periodic full fetch keeps its own half-hourly cadence.
type loopRateLimit struct {
	// refusing: the last full fetch was refused. The first refusal of a spell
	// places the backlog hold, as outlastRateLimit's does.
	refusing bool
	// until: zero, or when a go-live confirmation may be made again.
	until time.Time
}

// confirmationHeld reports whether a go-live confirmation must wait.
func (l *loopRateLimit) confirmationHeld() bool { return !l.until.IsZero() }

// noteFetch reads a full fetch the waiting loop made, and reports whether
// YouTube refused it for a rate limit (rateLimitedFetch). A refusal's spell
// holds backlog admission on its first (onRateLimited); a refused go-live
// confirmation also holds the next one off for rateLimitRetryInterval under
// the wait's progress line. A fetch that answered ends the spell, and lifts
// a hold on confirmations the periodic fetch finds the limit gone under.
func (sp *StreamProcessor) noteFetch(job *database.Job, l *loopRateLimit, info *youtube.VideoInfo, confirmation bool) bool {
	if !rateLimitedFetch(info) {
		l.refusing = false
		if l.confirmationHeld() {
			l.until = time.Time{}
			sp.db.UpdateJobFields(job.ID, map[string]any{"progress": ""})
		}
		return false
	}
	first := !l.refusing
	l.refusing = true
	if first && sp.onRateLimited != nil {
		sp.onRateLimited()
	}
	logRefusal := sp.logger.Debug
	if first {
		logRefusal = sp.logger.Warn
	}
	if !confirmation {
		logRefusal("YouTube rate-limited the waiting loop's periodic full fetch",
			"videoID", job.VideoID, "reason", info.PlayabilityReason)
		return true
	}
	l.until = time.Now().Add(rateLimitRetryInterval)
	sp.db.UpdateJobFields(job.ID, map[string]any{"progress": rateLimitWaitLine(rateLimitRetryInterval)})
	logRefusal("YouTube rate-limited the go-live confirmation; the job keeps waiting, its chat with it, and confirms again",
		"videoID", job.VideoID, "retryIn", rateLimitRetryInterval, "reason", info.PlayabilityReason)
	return true
}

// tickRateLimit runs once per answered probe, ahead of the loop's reading
// of it: while confirmations are held it rewrites the progress line with the
// time left, and once the hold has run out it lifts it, the line with it.
func (sp *StreamProcessor) tickRateLimit(job *database.Job, l *loopRateLimit, now time.Time) {
	if !l.confirmationHeld() {
		return
	}
	if !now.Before(l.until) {
		l.until = time.Time{}
		sp.db.UpdateJobFields(job.ID, map[string]any{"progress": ""})
		return
	}
	sp.db.UpdateJobFields(job.ID, map[string]any{"progress": rateLimitWaitLine(l.until.Sub(now))})
}

// endRateLimit clears the wait's progress line from a loop that returns
// while confirmations are held: a cancel, a shutdown, a give-up.
func (sp *StreamProcessor) endRateLimit(job *database.Job, l *loopRateLimit) {
	if l.confirmationHeld() {
		sp.db.UpdateJobFields(job.ID, map[string]any{"progress": ""})
	}
}

// isRateLimited reports whether err is YouTube rate-limiting the session: a
// rate-limit verdict (ErrRateLimited), or an Innertube 429 on the chain
// (youtube.HTTPRateLimitError) — the HTTP form of the same limit, which
// GetVideoInfo wraps when the cascade's last client was told too many
// requests and nothing else answered (finishExtraction). The 429 went to
// requeueBacklogAfterTransientFailure as any transient failure goes: a
// broadcast or a manual add ended in Error on it, and a backlog VOD spent its
// three retries in the 35 minutes they span — well inside the hour a limit
// lasts — and ended in Error too, with no backlog hold placed. A 429 whose
// typed error a wrap flattened to text is not recognised here, and is still
// the transient failure classifyProbeErr reads in "HTTP 429".
func isRateLimited(err error) bool {
	if errors.Is(err, ErrRateLimited) {
		return true
	}
	_, ok := errors.AsType[*youtube.HTTPRateLimitError](err)
	return ok
}

// rateLimitWait is how long a job that is not backlog waits in place on err:
// rateLimitRetryInterval, or the Retry-After a 429 named
// (youtube.HTTPRateLimitError) where that is longer — the least YouTube asked
// for — up to rateLimitBackoff, so a Retry-After no one could mean does not
// strand the job. A backlog VOD's hour is already past any of them.
func rateLimitWait(err error) time.Duration {
	d := rateLimitRetryInterval
	if rl, ok := errors.AsType[*youtube.HTTPRateLimitError](err); ok {
		d = max(d, min(rl.RetryAfter, rateLimitBackoff))
	}
	return d
}

// outlastRateLimit is processJob's handling of a run YouTube rate-limited
// (isRateLimited) — its verdict or a 429 — from stream processing or from a
// stale VOD's re-extraction. The limit holds backlog admission first,
// whatever the job is. A backlog VOD then goes back to Queued for its backoff
// (requeueBacklogAfterRateLimit), or, when it cannot, ends as setJobError
// ends it — a Cancel that landed meanwhile is reported as one. Any other job
// gives back its slots, waits in place under the wait's progress line
// (rateLimitWait), and is run again from stream processing.
//
// Returns the row as it now reads when the run goes on, or nil when it is
// over: the backlog VOD's requeue or its Error, or a run whose ctx ended, or
// whose row was deleted or left the states the queue processes while it
// waited (waitOutOutage's re-read) — which this hands to handleCancellation.
//
// since is when the run's current spell of refusals began (zero: none yet,
// or the last pass answered — processJob resets it). Only the spell's first
// refusal places the backlog hold and is logged at Warn: the ones a job
// waiting in place meets every half hour after it are the same limit, seen
// again by the same job, and placed each time they pushed the hour on
// before it ran out — so one video YouTube kept refusing, a captcha or a
// "try again later" that was never going to pass, held every channel's
// backlog for as long as the job waited, which nothing bounds. A spell's
// first refusal is still fresh evidence, and any other job's — a backlog
// VOD's above all, one refusal per run — still extends the hold.
func (w *DownloadWorker) outlastRateLimit(ctx context.Context, job *database.Job, err error, since *time.Time) *database.Job {
	first := since.IsZero()
	if first {
		*since = time.Now()
		if w.scheduler != nil {
			w.scheduler.holdForRateLimit(*since, rateLimitBackoff)
		}
	}
	if CookieResumeStatus(job) == database.StatusQueued {
		if requeued, err := w.requeueBacklogAfterRateLimit(job, err); !requeued {
			w.setJobError(job, err)
		}
		return nil
	}
	logWait := w.logger.Debug
	if first {
		logWait = w.logger.Warn
	}
	wait := rateLimitWait(err)
	logWait("YouTube rate-limited this job's fetch; the job keeps its status and fetches again",
		"jobID", job.ID, "status", job.Status, "retryIn", wait, "err", err)
	w.queue.ReleaseSlots(job.ID)
	if !w.waitOutRateLimit(ctx, job.ID, wait) {
		w.handleCancellation(job)
		return nil
	}
	cur, gerr := w.db.GetJob(job.ID)
	if gerr != nil {
		// A database error must not end a job that is waiting for nothing
		// else: go on, as waitOutOutage does.
		return job
	}
	if cur == nil || !ShouldProcess(cur) {
		w.handleCancellation(job)
		return nil
	}
	return cur
}

// waitOutRateLimit holds jobID's run for wait under the wait's progress
// line, rewritten with the time left and cleared however the wait ends, and
// reports false when ctx ends first.
func (w *DownloadWorker) waitOutRateLimit(ctx context.Context, jobID string, wait time.Duration) bool {
	until := time.Now().Add(wait)
	w.db.UpdateJobFields(jobID, map[string]any{"progress": rateLimitWaitLine(wait)})
	defer w.db.UpdateJobFields(jobID, map[string]any{"progress": ""})
	done := time.NewTimer(wait)
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
	// A hold that has run out opens afresh, whether or not a sweep has
	// looked at it since: sweep returns ahead of the gate while offline,
	// while the disk gate is closed or when its read fails, and a hold that
	// ran out under one of those was still "held" here, so the next limit,
	// hours later, was logged at Debug as an extension and the Warn that
	// tells the operator the backlog is held never fired for it.
	opening := !s.rateHeld || !now.Before(s.rateLimitedUntil)
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
