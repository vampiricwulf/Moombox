package worker

import (
	"errors"
	"fmt"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobstate"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/redact"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// How a run's failure is recorded: which errors park a job in COOKIES? and
// under which park reason, setJobError, the "Job Failed" notice, the
// cookie refresh a credential failure attempts first, and the settling of a
// chat the failed run left in flight.

// cookiesStatusError reports whether err's chain warrants StatusCookies
// rather than StatusError — i.e. the user acting on cookie auth is the fix:
// worker.ErrCookiesRequired (player-API member/login flags),
// twitch.ErrTwitchAuthExpired (GQL 401/403 after token rotation), and
// twitch.ErrSubscriberOnly (usher entitlement restriction — logging into an
// account that has access is the fix). Audit reports/twitch.md #8.
// ErrNotAMember belongs here too: the session is alive but lacks the
// membership, and credentials for an account that has it are still the fix.
//
// twitch.ErrSubscriberOnlyNotEntitled does NOT, and by its absence here, not
// by a check: the same Usher refusal made to a session the playback token
// names as signed in parks in Error (owner decision D20). Unlike ErrNotAMember
// there is no Twitch account fingerprint a credential sweep could resume it on
// (CurrentCredentialIdentity has none for Twitch), so COOKIES? would only park
// it under a "log in" prompt for an operator who already has.
func cookiesStatusError(err error) bool {
	return errors.Is(err, ErrCookiesRequired) ||
		errors.Is(err, ErrNotAMember) ||
		errors.Is(err, twitch.ErrTwitchAuthExpired) ||
		errors.Is(err, twitch.ErrSubscriberOnly)
}

// cookieRefreshWorthAttempting reports whether firing the automatic cookie
// refresh could plausibly fix err. Everything that lands on StatusCookies
// qualifies EXCEPT ErrNotAMember: YouTube already answered that request as a
// signed-in session, so refreshing (rotating) that same session changes
// nothing and its failure message would send the operator after credentials
// that are not the problem.
//
// twitch.ErrSubscriberOnlyNotEntitled is excluded for the same reason (owner
// decision D20): Twitch minted that request's playback token for a signed-in
// account and Usher still refused it. It parks in Error, which never reaches
// the refresh, so the exclusion is the second lock rather than the first: a
// refresh that verified the healthy session would report it restored and
// re-queue the job into the same refusal, every cycle.
func cookieRefreshWorthAttempting(err error) bool {
	return !errors.Is(err, ErrNotAMember) && !errors.Is(err, twitch.ErrSubscriberOnlyNotEntitled)
}

// parkReasonForError classifies err into the database.ParkReason persisted
// alongside StatusCookies. This is the durable half of the same distinction
// cookieRefreshWorthAttempting makes in-process: the automatic refresh decides
// once, here and now, but the auth-recovery sweep in cmd/moombox decides
// again — minutes, days, or a restart later — and needs the answer written
// down rather than re-derived from the job's error prose.
//
// ErrNotAMember is the only membership case: YouTube answered a demonstrably
// signed-in request with "members only", so no amount of restoring or
// rotating THESE credentials can help.
//
// twitch.ErrSubscriberOnly looks similar but is not, for two reasons. It is
// what remains of Usher's 403 once the signed-in refusals have been split off
// (twitch.ErrSubscriberOnlyNotEntitled, which parks in Error and never reaches
// this): no token was sent, the playback token belonged to nobody, or its
// document was unreadable — so working credentials genuinely may be the fix.
// And the retry loop this classification exists to break is structurally
// absent on that side anyway: the auth-recovered sweep only fires on a
// not-auth → auth TRANSITION, and an un-entitled account with healthy Twitch
// auth produces no transitions, so nothing re-runs the job in the first place.
// It stays in the auth class.
//
// Returns ParkReasonNone for anything that does not park at StatusCookies, so
// callers can write the field unconditionally and never leave a stale
// classification behind on a job that failed for an unrelated reason — with
// the Error-row exceptions, each read off the error's type and never its
// text: a live Twitch capture that stopped on its unconfirmed-end latch
// (ErrTwitchEndUnconfirmed) is recorded as ParkReasonTwitchEndUnconfirmed,
// the marker the automatic mux keys on (AutoMuxEndedBroadcast, D-T4); a
// Twitch live job whose stream processing never reached Twitch
// (twitchUnreachableError) as ParkReasonTwitchUnreachable, a marker the
// Twitch monitor's recovery keys on (jobstate.TwitchAutoRetryPending); and a
// stream-processing refusal that names its own reason (parkReasonError —
// the Twitch offline branch's ParkReasonTwitchOffline, the recovery's other
// marker, and ParkReasonTwitchEndedOffline, the Mux tail's, owner ruling
// R-I1) as that reason.
func parkReasonForError(err error) database.ParkReason {
	if !cookiesStatusError(err) {
		if errors.Is(err, ErrTwitchEndUnconfirmed) {
			return database.ParkReasonTwitchEndUnconfirmed
		}
		if _, ok := errors.AsType[*twitchUnreachableError](err); ok {
			return database.ParkReasonTwitchUnreachable
		}
		if named, ok := errors.AsType[*parkReasonError](err); ok {
			return named.reason
		}
		return database.ParkReasonNone
	}
	if errors.Is(err, ErrNotAMember) {
		return database.ParkReasonMembership
	}
	return database.ParkReasonAuth
}

// errorStage answers "which button fixes this" from the only signal the error
// carries: the prefix the orchestrator writes.
//
// Six finalize shapes exist in orchestrator_mux.go and all six are the mux
// stage: muxAndFinalize's fmt.Errorf("mux: %w", err), verifyMuxedDuration's
// short-output refusal "mux produced …", "no media files to mux…",
// "create output dir: %w", and the split finalize's two, "create segment
// output dir: %w" and "no segment files found in staging directory". The
// "mux" prefix also keeps "mux segment N:" on the mux side, whatever that
// wraps. The first four reach setJobError unwrapped — ExecuteWithChat returns
// the finalize error straight through — so the prefix survives. The split
// finalize's two are listed so the answer is right if one ever arrives; today
// neither does. Every caller of muxSegment logs "create segment output dir"
// and carries on, and the staging-directory message is written by the Mux
// verb's own failure write, which sends no Job Failed embed. Everything else
// is the download stage.
//
// No line numbers here on purpose: an earlier draft carried them and every one
// had already drifted by the end of the arc. The citation gate does not read
// Go comments, so nothing would have caught it; the error strings below are
// greppable and cannot drift.
//
// The prefixes are anchored deliberately: an ffmpeg stderr tail from a
// DOWNLOAD failure can mention muxing anywhere in its 500 characters, and a
// substring match would flip the answer for the case that matters most.
func errorStage(errMsg string) string {
	// "mux" alone covers "mux: …", "mux produced …" and "mux segment N: …".
	for _, p := range []string{
		"mux",
		"no media files to mux",
		"create output dir",
		"create segment output dir",
		"no segment files found in staging directory",
	} {
		if strings.HasPrefix(errMsg, p) {
			return "mux"
		}
	}
	return "download"
}

func (w *DownloadWorker) setJobError(job *database.Job, err error) {
	// The run is over whichever way this failure is recorded: its chat
	// downloaders are stopped first, ahead of the Error write and its
	// settle, so none of them writes "downloading" back onto the row
	// (stopRunChats). Most were stopped by the orchestrator's error arm
	// already, and a second Stop is a no-op.
	w.stopRunChats(job.ID)
	// Give the slots back before the tail below (notifications, and an
	// automatic cookie refresh that can take minutes), so the next download
	// does not wait on it. Only the slots: the run stays registered until
	// processJob's deferred Complete — see JobQueue.ReleaseSlots. Whatever
	// re-enqueues this job meanwhile (AutoReinitializeJob, the cookie
	// resume below) waits for that exit through afterJobExit instead of
	// starting a second run beside this one.
	w.queue.ReleaseSlots(job.ID)
	// This run ends out of Queued, so a backlog retry streak is over.
	w.forgetBacklogRetries(job.ID)

	// The one text every sink below shares — the "job error" line, the
	// stored error the dashboard and the TUI show, and the Job Failed embed —
	// so a googlevideo URL's credentials (the GVS PO token, the client's
	// public IP, the signatures) that reached this error by any route are cut
	// out once, here. The producers redact at the source too
	// (redact.MediaError on the fetch errors that quote a media URL); this is
	// the last stop before the text is kept and posted.
	errMsg := redact.MediaText(err.Error())
	w.logger.Error("job error", "jobID", job.ID, "err", errMsg)

	status := database.StatusError
	if cookiesStatusError(err) {
		status = database.StatusCookies
	}

	// park_reason and park_identity are written on EVERY error transition,
	// including the cleared case, so a job that once parked as "membership"
	// and later failed for something else does not carry the old
	// classification — a stale one would suppress a legitimate auth-recovery
	// resume forever, and a stale identity would fake an account change.
	//
	// The identity is captured only for a membership park: it is the account
	// the platform refused this job under, and it is meaningless for any other
	// failure.
	reason := parkReasonForError(err)
	identity := ""
	if reason == database.ParkReasonMembership && w.CurrentCredentialIdentity != nil {
		identity = w.CurrentCredentialIdentity(job.Platform)
	}
	// Never over an operator's Cancel. processJob reads its context before
	// it comes here, and a Cancel that lands after that read — CancelJob
	// cancels the run and writes Cancelled — was overwritten by this write:
	// the job showed Error, sent Job Failed, and the Job Cancelled the cancel
	// route had left to this run (the run was flagged) was never sent. The
	// operator's verdict stands, and the run ends as a cancelled one: when a
	// Cancel flagged it before this failure settled (JobQueue.settle) —
	// though its Cancelled write may not have landed yet — or when the row
	// already reads Cancelled — CancelJob writes before it flags.
	// Settled here, a Cancel from now on is its caller's to report: this
	// run's tail no longer reads the flag.
	if w.queue.settle(job.ID) || !w.db.UpdateJobFieldsUnless(job.ID, database.StatusCancelled, map[string]any{
		"status":        status,
		"error":         errMsg,
		"park_reason":   reason,
		"park_identity": identity,
	}) {
		w.logger.Info("job failure not recorded: the job was cancelled (or deleted) as it failed",
			"jobID", job.ID, "err", errMsg)
		w.handleCancellation(job)
		return
	}
	if status == database.StatusError {
		w.settleErrorChat(job.ID)
	}

	// Suppress notifications for non-actionable errors (matches TS behavior):
	// - Age-restricted content: nothing user can do
	// - Probe timeout: transient, stream may have ended naturally
	// - Twitch monitor-driven retries STILL WITHIN budget: the monitor will
	//   silently AutoReinitializeJob on its next poll, so a failure embed
	//   would be noise on the same job.
	//
	// A TERMINAL failure on a retried job (budget exhausted, or an error
	// shape the recovery predicate rejects) DOES notify: previously any
	// AutoRetryCount>0 was suppressed, so the operator couldn't distinguish
	// "still retrying" from "gave up for good" — the exact moment that needs
	// attention. retryLikely is the monitor's own recovery predicate
	// (jobstate.TwitchAutoRetryPending), asked of the row this failure just
	// wrote: a copy of the job carrying the new status, error and park
	// reason — the reason is what it reads (twitch_offline,
	// twitch_unreachable), so a latched row, which carries its own, is
	// never one; pinned by
	// jobstate.TestOfflineParkReasonsKeepTheRecoveryTruthTable.
	written := *job
	written.Status, written.Error, written.ParkReason = status, errMsg, reason
	retryLikely := jobstate.TwitchAutoRetryPending(&written)
	nonActionable := errors.Is(err, ErrNonActionable)
	suppressNotification := nonActionable || (job.AutoRetryCount > 0 && retryLikely)

	// Send error/auth notification
	if w.notifier != nil && !suppressNotification {
		if status == database.StatusCookies {
			reason := errMsg
			if reason == "" {
				reason = "Members-only content"
			}
			// Guarded, and the title escaped: the two shapes the Job Failed
			// send below keeps. An empty field value is a permanent Discord
			// 400 that drops the whole embed, and a raw title beside an
			// escaped one renders two ways in one embed.
			authFields := notifications.NewFieldBuilder().
				AddInlineIf(job.ChannelName != "", "Channel", notifications.EscapeMarkdown(job.ChannelName)).
				AddInlineIf(job.VideoID != "", notifications.IDLabel(job.Platform), job.VideoID).
				Add("Reason", notifications.EscapeMarkdown(reason)).
				Build()
			// The same row→facts mapper the Job Failed send below uses, for
			// the same three reasons and one more. This is a PER-JOB auth
			// alert — one dead cookie parks N jobs — and isBatchable
			// (internal/notifications/batch.go) admits `auth` into the 5 s
			// coalescing window only when it carries a JobID, so without
			// these the burst this arc exists to fold never folds. The footer
			// reads "Moombox" with no platform and no id for the one alert an
			// operator most needs to tell apart, and the deep link, which
			// keys on JobID and Author both, never applies.
			af := NotifyFacts(job)
			w.notifier.Send("Authentication Required",
				fmt.Sprintf("Cookies needed: %s", notifications.EscapeMarkdown(job.Title)),
				notifications.TypeWarning,
				authFields,
				notifications.SendOptions{
					URL:       af.URL,
					Thumbnail: af.ThumbnailURL,
					Event:     "auth",
					Author:    notifyAuthor(af),
					Platform:  af.Platform,
					JobID:     af.ID,
				},
			)
		} else {
			w.sendJobFailed(job, errMsg, jobstate.PendingAutoAction(&written), false)
		}
	} else if w.notifier != nil && nonActionable && status == database.StatusError {
		// The failure report is suppressed, but an edit-mode target may hold
		// this job's lifecycle message open at "Found" or "Downloading" — and
		// with no terminal send it stayed there for good. An EditOnly send
		// closes it and posts nothing (notifications.SendOptions.EditOnly).
		// The Twitch retry suppression is not this: the monitor restarts that
		// job, and its next event goes on editing the same message.
		w.sendJobFailed(job, errMsg, jobstate.PendingAutoAction(&written), true)
	}

	// Automatic cookie recovery. Deliberately OUTSIDE the notifier branch
	// above: an attempt to fix the session is not a notification, and gating
	// it on w.notifier != nil meant a deployment with no webhook configured
	// silently got neither the recovery nor any log line explaining why.
	// attemptCookieRefresh owns the "should we, and if not why not" decision
	// so that every reason for declining is stated in one place.
	if status == database.StatusCookies {
		w.attemptCookieRefresh(job, err)
	}
}

// sendJobFailed sends the "Job Failed" embed for job. editOnly marks the
// non-actionable case: the report is suppressed and the send only closes an
// open lifecycle message (notifications.SendOptions.EditOnly).
//
// next is the automatic action the row this failure wrote is waiting on
// (jobstate.PendingAutoAction, asked of that row by setJobError: job is the
// row as it was before the write). When there is one the embed says so in a
// "Next" field, in the sentence both UIs show (AutoAction.Text): without it a
// capture the monitor muxes by itself minutes later read, from a phone, as a
// lost recording, and the natural answer to that, Reinitialize, deletes the
// staging the mux would have archived.
func (w *DownloadWorker) sendJobFailed(job *database.Job, errMsg string, next jobstate.AutoAction, editOnly bool) {
	// The one row→facts mapper, so the only job send that is not a
	// builder call still carries the identity every builder sets: the
	// URL with its watch fallback, the author line, the platform and
	// the job id. Arc N3's terminal edit on "error" and Arc N2b's deep
	// link both key on Opts.JobID, and an embed with none can be
	// neither edited nor linked.
	f := NotifyFacts(job)
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
	// "preserved" is the same predicate the resume route gates on
	// (HasStagingFiles), and Resume is YouTube-only in both UIs — a
	// Twitch job told "Resume available" gets a 400.
	staging := "removed"
	if HasStagingFiles(stagingBase, job.ID) {
		staging = "preserved"
		if job.Platform != "twitch" {
			staging = "preserved — Resume available"
		}
	}
	asides := len(ScanAsides(stagingBase, job.ID).Groups)

	fields := notifications.NewFieldBuilder().
		// Guarded for the same reason the builders guard their id
		// field: Field.Value carries no omitempty, clampEmbed never
		// drops an empty value, and Discord answers one with a 400 that
		// discord.go treats as permanent — the whole embed is dropped
		// after a single attempt.
		AddInlineIf(job.ChannelName != "", "Channel", notifications.EscapeMarkdown(job.ChannelName)).
		AddInlineIf(job.VideoID != "", notifications.IDLabel(job.Platform), job.VideoID).
		// Error is ALREADY wrapped by N1 — this Error field is one of
		// the four sites N1 escapes. Carry N1's line through unchanged;
		// a second wrap renders every \* as \\*. ChannelName is NOT one
		// of N1's four, so the wrap above is new and single. (No line
		// number: the one this comment used to carry was stale within
		// the arc, and nothing checks comments.)
		Add("Error", notifications.EscapeMarkdown(errMsg)).
		AddInline("Stage", errorStage(errMsg)).
		AddInline("Staging", staging).
		AddIf(job.AutoRetryCount > 0, "Automatic Retries",
			fmt.Sprintf("gave up after %d/%d", job.AutoRetryCount, MaxTwitchAutoRetries)).
		AddIf(next != jobstate.AutoActionNone, "Next", next.Text()).
		AddIf(asides > 0, "Set-aside recordings",
			fmt.Sprintf("%d — Recover to mux them", asides)).
		Build()
	w.notifier.Send("Job Failed",
		fmt.Sprintf("Job failed for: %s", notifications.EscapeMarkdown(job.Title)),
		notifications.TypeError,
		fields,
		notifications.SendOptions{
			URL:       f.URL,
			Thumbnail: f.ThumbnailURL,
			Event:     "error",
			Author:    notifyAuthor(f),
			Platform:  f.Platform,
			JobID:     f.ID,
			EditOnly:  editOnly,
		},
	)
}

// CookieRefreshOutcome is OnCookieRefreshNeeded's answer for one platform.
type CookieRefreshOutcome int

const (
	// CookieRefreshNotRestored: the refresh failed — the platform's
	// credentials were checked and refused, or there are none, or the pass
	// declined to run or errored. The zero value, so an answer nobody chose leaves the job parked
	// with the advice to replace the cookie file.
	CookieRefreshNotRestored CookieRefreshOutcome = iota
	// CookieRefreshRestored: the named platform ended up authenticated, so
	// the job is retried.
	CookieRefreshRestored
	// CookieRefreshSkipped: the refresh did not run, for a reason the callback
	// has already logged and a person can clear — a browser holds the profile
	// (cookies.ErrProfileInUse). Nothing judged the cookies, so the job stays
	// parked without the advice to replace them.
	CookieRefreshSkipped
	// CookieRefreshUnconfirmed: the pass ran and ended without establishing
	// whether the platform's cookies work (cookies.RefreshUnknown — it could
	// not reach the service or make the check). Nothing judged the cookies, so
	// the job stays parked, and a recheck says whether they work.
	CookieRefreshUnconfirmed
	// CookieRefreshOff: cookies.auto_enabled is off, so nothing was attempted.
	// The job stays parked; replacing the cookie file or turning the refresh
	// on is the way out.
	CookieRefreshOff
)

// attemptCookieRefresh runs (or deliberately declines to run) the automatic
// cookie refresh for a job that just parked at StatusCookies, and — when it
// cannot fix things — says what WILL, in terms the operator can act on.
//
// The advice is deliberately environment-neutral and leads with the cookie
// file. "Re-run setup from Settings" used to be the only thing printed here,
// and it is a dead end wherever the interactive browser login cannot run: it
// needs a headed browser and a person at it, and the setup endpoints are
// loopback-gated so a remote dashboard cannot reach them either. Naming the
// configured cookie file path instead makes the message concrete in every
// deployment — a Docker operator reads "/data/cookies.txt" and knows exactly
// which host file to replace — without probing for an environment we cannot
// reliably detect.
//
// The distinction that holds over time is between LOGGING IN (interactive,
// needs a browser and a human) and SUPPLYING COOKIES (a file Moombox reads).
// Only the first is bounded by the environment; phrase guidance against that
// line rather than against "container", which is a moving target.
func (w *DownloadWorker) attemptCookieRefresh(job *database.Job, err error) {
	// ErrNonActionable means "terminal, stop working this job", and the
	// recovery path does not merely log — on success it sets the job back to
	// Upcoming and re-enqueues it, restarting the probe budget from zero. The
	// two categories can co-occur: a multi-%w error (stream_processor_twitch's
	// probe give-up wraps ErrNonActionable alongside an underlying error that
	// may carry twitch.ErrTwitchAuthExpired) satisfies cookiesStatusError and
	// ErrNonActionable at the same time. That is unreachable today only
	// because classifyProbeErr's default routes "gql auth failure (401)" to
	// the network class — a string heuristic over the error's status-bearing
	// PREFIX (gqlRequest reports only a byte count, never the response body),
	// not an invariant worth relying on for a resurrection hazard.
	if errors.Is(err, ErrNonActionable) {
		w.logger.Warn("skipping automatic cookie refresh — this failure was already classified terminal, and recovery would re-queue the job and reset its retry budget",
			"jobID", job.ID,
			"videoID", job.VideoID)
		return
	}
	if !cookieRefreshWorthAttempting(err) {
		w.logger.Warn("skipping automatic cookie refresh — YouTube answered this request as a SIGNED-IN session, so the credentials are alive and the account simply lacks access",
			"jobID", job.ID,
			"videoID", job.VideoID,
			"fix", "supply cookies from the account that holds the channel membership")
		return
	}
	if w.OnCookieRefreshNeeded == nil {
		// Not wired (no auto-cookie service constructed). Debug rather than
		// Warn: this is a build/wiring fact, not something the operator did.
		w.logger.Debug("no automatic cookie refresh is wired; leaving the job parked",
			"jobID", job.ID, "videoID", job.VideoID)
		return
	}

	// job.Platform verbatim, with no defaulting applied here.
	//
	// Every production creator sets Platform explicitly, and this reads the
	// in-memory struct rather than a row, so the schema default never enters
	// into it. If one ever arrives empty, RefreshResult.Verdict("") is
	// RefreshUnknown — no health claim either way — and the job stays parked
	// for a human. Guessing "youtube" here would trade that safe outcome for
	// a second defaulting rule to keep in sync with the creators.
	w.logger.Info("attempting automatic cookie refresh...", "platform", job.Platform)
	outcome := w.OnCookieRefreshNeeded(job.Platform)
	if outcome == CookieRefreshSkipped {
		// A browser holds the profile, and the callback's skip line says
		// which host and which lock. This used to fall to the advice below:
		// the skip, then "auto cookie refresh failed — the cookie file has
		// to be replaced by hand" one line later, replacement named as the
		// only remedy for cookies nothing had rejected. Freeing the profile
		// is the remedy; replacing the file still works, so it is named
		// second.
		w.logger.Info("the job stays parked — the automatic cookie refresh was skipped, not failed",
			"jobID", job.ID,
			"videoID", job.VideoID,
			"next", "it waits for a refresh that can run (the skip line above names what holds the profile) or a replaced cookie file, and resumes once the cookies work")
		return
	}
	if outcome == CookieRefreshRestored {
		w.logger.Info("cookie refresh succeeded, retrying job", "platform", job.Platform)
		// Upcoming, not Live, so StreamProcessor.Process re-probes and
		// classifies the stream afresh (per audit reports/worker.md
		// Finding 21) — or Queued for a backlog VOD, which re-enters through
		// the scheduler's pacing like the cookie-parked sweep's resumes.
		status := CookieResumeStatus(job)
		// Only while the row is still parked. The refresh can take two
		// minutes, and both UIs offer Cancel on a COOKIES? row: written
		// unconditionally, this turned the operator's Cancelled back into
		// Upcoming (Queued for a backlog VOD) and the hand-off below
		// enqueued it — the cancelled job downloaded after all. setJobError
		// settled this run, so that Cancel was its caller's to report and
		// nothing more is owed here.
		if !w.db.UpdateJobFieldsIf(job.ID, database.StatusCookies, map[string]any{
			"status":        status,
			"error":         "",
			"park_reason":   database.ParkReasonNone,
			"park_identity": "",
		}) {
			// The credential sweep the refresh's own re-check sets off
			// (cmd/moombox resumeCookieParkedJobs) resumes every parked row
			// of the platform, usually this one among them, before the
			// refresh returns. That resume is the one this would have made,
			// and still wants the hand-off: the sweep wakes the scheduler
			// but leaves an Upcoming row to the heartbeat. Anything else —
			// Cancelled, retried, deleted — is not this run's to resume.
			var now database.JobStatus // "" when the row is gone
			if cur, gerr := w.db.GetJob(job.ID); gerr == nil && cur != nil {
				now = cur.Status
			}
			if now != database.StatusUpcoming && now != database.StatusQueued {
				w.logger.Info("the job left COOKIES? while the cookie refresh ran; not resuming it",
					"jobID", job.ID, "status", now)
				return
			}
			status = now
		}
		// This runs inside the parked run's own tail, which is still
		// registered: an Enqueue now would be dropped as a duplicate (and the
		// job left for the heartbeat), so the hand-off waits for the run to
		// exit.
		// An operator's Resume or Reinitialize already waiting on the job
		// (ErrResetPending) is the one that runs: it applies over the resume
		// written above (acceptedStatus) and queues the job itself.
		if err := w.afterJobExit(job.ID, string(opCookieRefreshResume), true, nil, func(database.JobStatus) {
			if status == database.StatusQueued {
				if w.scheduler != nil {
					w.scheduler.Wake()
				}
			} else {
				w.queue.Enqueue(job.ID, database.StatusUpcoming)
			}
		}); err != nil && !errors.Is(err, ErrAlreadyPending) {
			if !errors.Is(err, ErrResetPending) {
				w.logger.Warn("cookie-refresh resume failed", "jobID", job.ID, "err", err)
				return
			}
			w.logger.Info("cookie-refresh resume left to the verb already waiting on the job", "jobID", job.ID, "err", err)
		}
		return
	}

	if outcome == CookieRefreshUnconfirmed {
		// The callback's line above says the pass could not establish
		// whether the cookies work. They may be fine — most of the ways here
		// leave the session healthy — so this does not follow it with the
		// advice to replace them, as it once did for every outcome but a
		// restore. Info, as the skip's line is: nothing was rejected, and a
		// recheck answers the question this pass could not.
		w.logger.Info("auto cookie refresh could not confirm the cookies — the job stays parked; R C / Recheck will tell",
			"jobID", job.ID,
			"videoID", job.VideoID,
			"platform", job.Platform)
		return
	}

	var cookieFile string
	if w.CookieFileInUse != nil {
		cookieFile = w.CookieFileInUse()
	}
	if cookieFile == "" {
		w.readConfig(func(c *config.MoomboxConfig) { cookieFile = c.Cookies.CookieFile })
	}
	if outcome == CookieRefreshOff {
		// Nothing was attempted, so "failed" was the wrong word for it; the
		// two ways out are the operator's, which is why this stays a Warn.
		w.logger.Warn("automatic cookie refresh is off — replace the cookie file or turn it on in Settings",
			"jobID", job.ID,
			"videoID", job.VideoID,
			"cookieFile", cookieFile,
			"setting", "cookies.auto_enabled")
		return
	}
	if cookieFile == "" {
		w.logger.Warn("auto cookie refresh failed — no cookie file is configured",
			"fix", "set cookies.cookie_file to a Netscape cookies.txt exported from a browser signed in to the account")
		return
	}
	w.logger.Warn("auto cookie refresh failed — the cookie file has to be replaced by hand",
		"cookieFile", cookieFile,
		"fix", "export a fresh Netscape cookies.txt from a browser signed in to the account and overwrite that file",
		"why", "browsing on in the source browser profile rotates the session and invalidates an earlier export — export from a private window, then close it",
		"wizard", "the interactive browser login in Settings needs a headed browser and a person at it, so it runs only on the machine hosting Moombox")
}

// settleErrorChat settles the chat_status a failed job leaves behind, on
// either platform (owner rulings R-E1, R-E1a, R-H1). An orchestrator's error
// arm stops the chat downloader and returns without recording a verdict —
// recordChatOutcome runs on the success path, and on ExecuteTwitch's
// unconfirmed-end arm — so an Error row went on reading
// "downloading" (or "pending"), and the Web details badge, which renders any
// non-empty value, showed a capture in progress on a failed job. The value
// settles as a Cancel's does (stoppedChatStatus, R-E1a): a capture that was
// running is "incomplete", the verdict a cut-off capture gets everywhere else,
// and its resume sidecar stays in staging, which an Error row keeps for
// Resume; one still pending has nothing to report and is cleared.
//
// Each in-flight value (database.ChatStatus.InFlight) is settled by its own
// statement, conditional on both the status and that value
// (database.SettleChatStatus): a verdict the run did record is left alone,
// and a Resume that moved the row on before this write — whose run re-arms
// the chat (setupChatDownloader writes "pending" whatever the row reads) —
// wins. Pending is tried first, in ChatStatuses order, so a capture that
// starts between the two statements is still settled as the running one it
// became. Every platform (R-H1; R-E1 first named YouTube alone): a Twitch
// failure that is not the unconfirmed-end arm's — a staging directory that
// could not be made, a stream-processing failure after the capture was
// armed, a panic, a failed Mux — left "pending" or "downloading" on its
// Error row as a YouTube one did. The verdict the unconfirmed-end arm
// records is not in flight and stands. A Cancel is settled by
// handleCancellation, and a COOKIES? park is not an Error. The Error writes
// that come here are all there are: setJobError's, writePanicError's, a
// failed Mux's (muxJob) and the automatic mux that could not start
// (autoMuxNow).
func (w *DownloadWorker) settleErrorChat(jobID string) {
	for _, from := range database.ChatStatuses {
		if !from.InFlight() {
			continue
		}
		to, _ := stoppedChatStatus(from)
		if w.db.SettleChatStatus(jobID, database.StatusError, []database.ChatStatus{from}, to) {
			w.logger.Info("chat capture stopped with the failed download; settling it",
				"jobID", jobID, "from", string(from), "to", string(to))
		}
	}
}
