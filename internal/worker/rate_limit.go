package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/chat"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// A YouTube rate limit (youtube.PlayabilityRateLimited): the player refused
// the video with "This content isn't available, try again later", which
// yt-dlp explains as the account or session being rate-limited by YouTube
// for up to an hour (26feac3dd, #12958), or with a captcha challenge
// (f484c5159, #12939) — or, in its HTTP form, with a 429 on every client of
// the cascade (youtube.HTTPRateLimitError; isRateLimited). Upstream words
// the refusal (_video.py:4074-4081 at b77c9304e) and, unless --wait-for-video
// is given, ends the extraction (raise_no_formats, common.py:1264-1271) —
// one command, one video, and the operator runs it again later. With the
// flag the refusal is only warned of, and the extraction is run again on the
// user's flat interval, with no bound (YoutubeDL._wait_for_video's
// ReExtractInfo, YoutubeDL.py:1755-1799, which _handle_extraction_exceptions
// re-extracts on, 1723-1736). An archiver is the one that runs it again, so
// here the verdict is transient (ErrRateLimited) and every job waits it out
// rather than ending in Error, as if the flag were always given, and goes
// beyond it in how:
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
//     its probe running, and confirms its go-live again once the limit has
//     been waited out (loopRateLimit).
//
// Four rules bound and shape the wait (the owner's decisions after W5), each
// beyond upstream as well, whose --wait-for-video waits on one flat interval
// for as long as the refusals last, and tells no one:
//
//   - a spell of refusals ends the job in Error once it has lasted
//     rateLimitCap, a day, AND held at least rateLimitCapRefusals (3) refused
//     attempts, with the refusal as yt-dlp words it — 24 hourly requeues of a
//     backlog VOD, 48 half-hourly tries of a job waiting in place, or a
//     waiting loop's counted full fetches refused as long. Measured on the
//     wall clock from the spell's first (counted) refusal (rateLimitSpells,
//     loopRateLimit.capSince), so neither a Retry-After that lengthens a wait
//     nor a requeue admitted late stretches it; a fetch YouTube answers ends
//     the spell, and so does a dashboard Retry. A limit that outlasts a day is
//     not the hour upstream says one lasts, and was waited out for good: a
//     captcha nobody solves, or a refusal the verdict cannot tell from one.
//     The refusal floor (owner decision on FA's third follow-up) keeps a day
//     that saw only one or two attempts — a backlog VOD the scheduler
//     re-admitted a day later, a broadcast refused once on either side of a
//     day of waiting — from ending on what is not a day of refusals at all;
//   - a waiting loop counts its refusals toward that cap only once the
//     stream has begun (owner decision on FA's second follow-up): while the
//     status probe still answers upcoming, its periodic full fetch refused
//     costs the broadcast nothing, since nothing it airs is lost to the wait;
//     counting starts once the probe, or the schedule probe, says it is live
//     or over, when the go-live confirmation is what is waited on. A probe
//     that could not tell (inconclusive) counts as begun only when the last
//     one that could said so (loopRateLimit.begun);
//   - the waiting loop confirms a go-live YouTube refused again 2, 5, 10 and
//     20 minutes after the spell's first refused confirmation, then every
//     rateLimitRetryInterval (rateLimitConfirmTaper; the owner's reading of
//     the taper, counted from the first refusal, not as gaps between them):
//     a broadcast that has just gone live loses what it airs while its
//     confirmation waits, where it has no DVR window, and a limit that lifts
//     in minutes cost it the whole half hour;
//   - the backlog hold outlives a restart, saved beside the disk gate's hold
//     in open-alerts.json (Scheduler.RecordRateLimitHold,
//     RestoreRateLimitHold), and still runs out on its own;
//   - and a captcha (errCaptchaRequired) is waited out like a rate limit,
//     under its own progress line (captchaWaitLine), and tells the operator
//     once a spell, however many jobs it refuses (captchaAlert): a captcha
//     passes only when someone solves it, in a browser signed in with the
//     same account.

// rateLimitBackoff is how long a backlog VOD a rate-limit verdict sent back
// to Queued is held from re-admission, and how long the verdict holds every
// backlog admission: the hour yt-dlp says the limit lasts.
const rateLimitBackoff = time.Hour

// rateLimitRetryInterval is how long a job that is not backlog waits in
// place before its stream processing fetches again, and how long the
// waiting loop holds off a go-live confirmation YouTube refused once the
// taper is spent (rateLimitConfirmTaper): the waiting loop's own full-fetch
// cadence (fullFetchInterval), the rate at which a waiting broadcast already
// makes the full extraction a refusal comes back on. A fetch in the meantime
// would only be the same request sent into the same limit, and a broadcast
// that went live while it waited is captured from its start, as any
// live-from-start capture is. A 429's Retry-After can lengthen the wait in
// place (rateLimitWait).
const rateLimitRetryInterval = fullFetchInterval

// rateLimitCap is how long a job's spell of rate-limit refusals may last
// before the job ends in Error (rateLimitGaveUp): a day, measured from the
// spell's first refusal on the wall clock. yt-dlp says the limit lasts up to
// an hour (_video.py:4079 at b77c9304e); twenty-four of them is a limit that
// is not going to pass on its own.
const rateLimitCap = 24 * time.Hour

// rateLimitCapRefusals is the fewest refused attempts a spell must hold,
// beside lasting rateLimitCap, before the job ends in Error — on every path:
// a backlog VOD's requeues, a job waiting in place, a waiting loop's counted
// refusals (the owner's decision on FA's third follow-up). A day measured from
// one refusal to a second one a day later is two looks, not a limit that
// would not pass.
const rateLimitCapRefusals = 3

// rateLimitConfirmTaper is when the waiting loop makes its go-live
// confirmations again after a spell's first refused one, counted from that
// refusal — 2, 5, 10 and 20 minutes after it — and every
// rateLimitRetryInterval after the last (confirmationHoldAfter). The status
// probe and the waiting-room chat run on through it.
var rateLimitConfirmTaper = []time.Duration{2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 20 * time.Minute}

// confirmationHoldAfter is the hold after a refused go-live confirmation made
// elapsed after the spell's first refused one (zero for that one itself): up
// to the next instant of the taper still ahead of it — so the retries land at
// +2, +5, +10 and +20 minutes from the first refusal, not 2, 5, 10 and 20
// minutes apart, which put them at +2, +7, +17 and +37 (the owner's reading
// of the taper, FA's first follow-up) — and rateLimitRetryInterval once the
// taper is spent. A confirmation a Retry-After floor pushed past one instant
// of the taper waits for the next one still ahead.
func confirmationHoldAfter(elapsed time.Duration) time.Duration {
	for _, at := range rateLimitConfirmTaper {
		if at > elapsed {
			return at - elapsed
		}
	}
	return rateLimitRetryInterval
}

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

// captchaWaitProgress opens the progress line a job waiting out a captcha
// shows in place of rateLimitWaitProgress: the wait is the same, but what
// ends it is someone solving the captcha, which the operator has been told
// to do (captchaAlert).
const captchaWaitProgress = "YouTube asks for a captcha"

// captchaWaitLine is that line with the time left, "YouTube asks for a
// captcha... (24m 0s left)", in the same wait-marker shape.
func captchaWaitLine(left time.Duration) string {
	return captchaWaitProgress + "... (" + utils.FormatDurationHuman(left) + " left)"
}

// waitLineFor is the wait line a refusal shows: the captcha's or the rate
// limit's.
func waitLineFor(captcha bool) func(time.Duration) string {
	if captcha {
		return captchaWaitLine
	}
	return rateLimitWaitLine
}

// errCaptchaRequired is the sentinel a rate-limit verdict that is a captcha
// challenge carries in ErrRateLimited's place (playabilityVerdict). It IS an
// ErrRateLimited — every errors.Is that waits a rate limit out waits a
// captcha out with it — and reads as one, so the job's error says what it
// said before; what it adds is that someone has to solve it (isCaptcha).
var errCaptchaRequired error = captchaRequired{}

// captchaRequired is errCaptchaRequired's type: comparable, so errors.Is
// matches the value, and unwrapping to ErrRateLimited.
type captchaRequired struct{}

func (captchaRequired) Error() string { return ErrRateLimited.Error() }
func (captchaRequired) Unwrap() error { return ErrRateLimited }

// isCaptchaReason reports whether a rate_limited reason is a captcha's: the
// youtube parser ends one with yt-dlp's sentence
// (youtube.CaptchaChallengeNote, _video.py:4075), which is how the verdict
// is told apart from a "try again later".
func isCaptchaReason(reason string) bool {
	return strings.HasSuffix(reason, youtube.CaptchaChallengeNote)
}

// isCaptcha reports whether err is a captcha refusal (errCaptchaRequired) —
// a verdict from stream processing or a stale VOD's re-extraction. An
// Innertube 429 never is.
func isCaptcha(err error) bool {
	return errors.Is(err, errCaptchaRequired)
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
	if isCaptchaReason(reason) {
		return reason
	}
	who := "The current session"
	if auth == youtube.SessionAuthLoggedIn {
		who = "Your account"
	}
	return fmt.Sprintf("%s. %s has been rate-limited by YouTube for up to an hour",
		strings.TrimSuffix(strings.TrimSpace(reason), "."), who)
}

// rateLimitGaveUpNote is what a job's error adds to the refusal once its
// spell has lasted rateLimitCap.
var rateLimitGaveUpNote = fmt.Sprintf(" (gave up after %d hours of refusals)", int(rateLimitCap/time.Hour))

// rateLimitGaveUp is the error a job ends with once its spell of refusals
// has lasted rateLimitCap (outlastRateLimit, giveUpRateLimited): the refusal
// as yt-dlp words it, or the 429, saying the job gave up. Not an ErrRateLimited any more, which processJob
// would wait out again, and without the sentinel's own "(rate-limited by
// YouTube)", which the wording already says.
func rateLimitGaveUp(err error) error {
	return errors.New(strings.TrimSuffix(err.Error(), " ("+ErrRateLimited.Error()+")") + rateLimitGaveUpNote)
}

// rateLimitedFetch reports whether a full fetch came back refused for a rate
// limit with nothing to download. The waiting loop reads it as no answer at
// all (loopRateLimit): neither a stream still upcoming, to be confirmed again
// on the next probe — the whole cascade into the limit every 30 s — nor a
// go-live, which wrote Live and went on to a download with no formats.
func rateLimitedFetch(info *youtube.VideoInfo) bool {
	return info != nil && info.PlayabilityError == youtube.PlayabilityRateLimited && probeFormatCount(info) == 0
}

// answeredFetch reports whether a full fetch was answered with no rate-limit
// verdict at all: what ends the session's captcha spell (captchaAlert.clear).
// An extraction that came back with formats under a rate-limit verdict is
// downloaded (playabilityVerdict), but the verdict still says the session is
// walled, so it ends nothing.
func answeredFetch(info *youtube.VideoInfo) bool {
	return info != nil && info.PlayabilityError != youtube.PlayabilityRateLimited
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
// live and VOD arms make when the status probe says the stream has begun,
// and the one the default arm makes on every probe the cookied probe cannot
// read (membersOnly), which confirms a stream it cannot see: once YouTube has
// refused one, the next is made at the taper's next instant counted from the
// spell's first refused confirmation (confirmationHoldAfter), from two
// minutes to rateLimitRetryInterval, under the wait's progress line, and the arms read the probe's answer as
// nothing until then. The loop's probe timer wakes at the hold's end
// (heldFor), so the confirmation is made then, not on the first probe past
// it. The periodic full fetch keeps its own half-hourly cadence. A refusal is
// the verdict (noteFetch) or a 429 on the whole cascade, the limit's HTTP
// form (noteFetchErr). A spell whose counted refusals have lasted
// rateLimitCap, rateLimitCapRefusals of them at least, ends the job
// (outlasted); a refusal counts once the stream has begun (counts).
type loopRateLimit struct {
	// refusing: the last full fetch was refused. The first refusal of a spell
	// places the backlog hold, as outlastRateLimit's does.
	refusing bool
	// since: when the spell's first refusal came; zero while none stands.
	since time.Time
	// firstConfirmation: when the spell's first refused go-live confirmation
	// came, which the taper counts from (confirmationHoldAfter); zero while
	// none of this spell's was refused.
	firstConfirmation time.Time
	// capSince and capRefusals: when the spell's first counted refusal came
	// (counts), and how many it has had — what rateLimitCap and
	// rateLimitCapRefusals are read against (outlasted).
	capSince    time.Time
	capRefusals int
	// begun: the status probe's last conclusive answer — or the schedule
	// probe's, when it overtook it — said the stream is live or over
	// (noteStatus). While it says upcoming, a refused periodic full fetch is
	// not counted toward the cap. It outlives a spell (noteFetch's reset
	// leaves it): it records the stream's state, not the refusals.
	begun bool
	// until: zero, or when a go-live confirmation may be made again.
	until time.Time
	// captcha: the refusal holding confirmations off was a captcha's, so
	// the progress line says so (waitLineFor).
	captcha bool
}

// confirmationHeld reports whether a go-live confirmation must wait.
func (l *loopRateLimit) confirmationHeld() bool { return !l.until.IsZero() }

// heldFor is how long a held confirmation has left to wait at now: what the
// loop's probe timer is cut to, so the confirmation is made at the hold's
// end. Zero when none is held or the hold has run out — a probe that failed
// left a hold that ran out standing, and the loop then waits its own
// interval, as it would without one, rather than spin.
func (l *loopRateLimit) heldFor(now time.Time) time.Duration {
	if !l.confirmationHeld() {
		return 0
	}
	return max(l.until.Sub(now), 0)
}

// outlasted reports whether the spell's counted refusals have lasted
// rateLimitCap at now, rateLimitCapRefusals of them at least.
func (l *loopRateLimit) outlasted(now time.Time) bool {
	return l.refusing && l.capRefusals >= rateLimitCapRefusals && now.Sub(l.capSince) >= rateLimitCap
}

// noteStatus records a conclusive answer on the stream's state — the status
// probe's, or the schedule probe's when it overtakes it (waitForLive): live
// or over sets begun, upcoming clears it, and anything else — an unclear
// answer, the inconclusive probe's stand-in, which the loop never passes
// here — leaves it as the last one that could tell said.
func (l *loopRateLimit) noteStatus(status youtube.StreamStatus) {
	switch status {
	case youtube.StreamLive, youtube.StreamVOD, youtube.StreamPostLive:
		l.begun = true
	case youtube.StreamUpcoming:
		l.begun = false
	}
}

// counts reports whether a refusal counts toward the cap: every go-live
// confirmation's — the probe has said the stream began, or cannot read it at
// all (the default arm) — and the periodic full fetch's once begun says so.
// A broadcast still upcoming loses nothing it airs to the wait, and a
// days-out premiere under a captcha nobody solved ended in Error before it
// began (the owner's decision on FA's second follow-up).
func (l *loopRateLimit) counts(confirmation bool) bool {
	return confirmation || l.begun
}

// noteFetch reads a full fetch the waiting loop made, and reports whether
// YouTube refused it for a rate limit (rateLimitedFetch). A refusal's spell
// holds backlog admission on its first (onRateLimited), and a captcha's
// tells the operator, once a spell (captchaAlert); a refused go-live
// confirmation also holds the next one off for its turn of the taper under
// the wait's progress line. A fetch that answered ends the spell — and the
// session's captcha spell, when it carries no verdict at all — and lifts a
// hold on confirmations the periodic fetch finds the limit gone under. The
// caller reads outlasted after a refusal.
func (sp *StreamProcessor) noteFetch(job *database.Job, l *loopRateLimit, info *youtube.VideoInfo, confirmation bool) bool {
	if !rateLimitedFetch(info) {
		l.refusing, l.since, l.firstConfirmation = false, time.Time{}, time.Time{}
		l.capSince, l.capRefusals = time.Time{}, 0
		if answeredFetch(info) {
			sp.captcha.clear()
		}
		if l.confirmationHeld() {
			l.until = time.Time{}
			sp.db.UpdateJobFields(job.ID, map[string]any{"progress": ""})
		}
		return false
	}
	sp.noteRefusal(job, l, confirmation, isCaptchaReason(info.PlayabilityReason), 0, "reason", info.PlayabilityReason)
	return true
}

// noteFetchErr reads a full fetch the waiting loop made that failed, and
// reports whether the failure was YouTube rate-limiting the session: a 429
// on every client of the cascade (isRateLimited; youtube.HTTPRateLimitError),
// the HTTP form of the limit noteFetch reads in a verdict, and a refusal as
// that is — the spell's backlog hold, the taper, the line, the day's cap. It
// went the way of a blip: a go-live confirmation was made again on the next
// probe, the whole cascade sent into the limit every 30 s with nothing held
// and nothing bounding it. The 429's Retry-After is a floor on the
// confirmation's hold, the least YouTube asked for, up to the hour, as on the
// wait in place (rateLimitWait). Any other failure is no answer: it reports
// false and changes nothing, so a spell — and a hold on confirmations —
// stands through it.
func (sp *StreamProcessor) noteFetchErr(job *database.Job, l *loopRateLimit, err error, confirmation bool) bool {
	if !isRateLimited(err) {
		return false
	}
	var floor time.Duration
	if rl, ok := errors.AsType[*youtube.HTTPRateLimitError](err); ok {
		floor = min(rl.RetryAfter, rateLimitBackoff)
	}
	sp.noteRefusal(job, l, confirmation, isCaptcha(err), floor, "err", err)
	return true
}

// noteRefusal is a refusal of the waiting loop's full fetch (noteFetch,
// noteFetchErr): the spell's first holds backlog admission, a captcha opens
// the session's captcha spell, and a refused confirmation holds the next one
// off for its turn of the taper — or for floor, when that is longer — under
// the wait's progress line. what and why name the refusal in the log.
func (sp *StreamProcessor) noteRefusal(job *database.Job, l *loopRateLimit, confirmation, captcha bool, floor time.Duration, what string, why any) {
	now := time.Now()
	first := !l.refusing
	if first {
		l.refusing, l.since = true, now
		if sp.onRateLimited != nil {
			sp.onRateLimited()
		}
	}
	if captcha {
		sp.captcha.raise(job)
	}
	if l.counts(confirmation) {
		if l.capRefusals == 0 {
			l.capSince = now
		}
		l.capRefusals++
	}
	logRefusal := sp.logger.Debug
	if first {
		logRefusal = sp.logger.Warn
	}
	if !confirmation {
		logRefusal("YouTube rate-limited the waiting loop's periodic full fetch",
			"videoID", job.VideoID, "counted", l.counts(false), what, why)
		return
	}
	if l.firstConfirmation.IsZero() {
		l.firstConfirmation = now
	}
	hold := max(confirmationHoldAfter(now.Sub(l.firstConfirmation)), floor)
	l.until, l.captcha = now.Add(hold), captcha
	sp.db.UpdateJobFields(job.ID, map[string]any{"progress": waitLineFor(captcha)(hold)})
	logRefusal("YouTube rate-limited the go-live confirmation; the job keeps waiting, its chat with it, and confirms again",
		"videoID", job.VideoID, "retryIn", hold, what, why)
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
	sp.db.UpdateJobFields(job.ID, map[string]any{"progress": waitLineFor(l.captcha)(l.until.Sub(now))})
}

// endRateLimit clears the wait's progress line from a loop that returns
// while confirmations are held: a cancel, a shutdown, a give-up.
func (sp *StreamProcessor) endRateLimit(job *database.Job, l *loopRateLimit) {
	if l.confirmationHeld() {
		sp.db.UpdateJobFields(job.ID, map[string]any{"progress": ""})
	}
}

// giveUpRateLimited is the waiting loop's result once its spell's counted
// refusals have lasted rateLimitCap (loopRateLimit.outlasted): the refusal — the
// verdict info carries, as yt-dlp words it, or err, a 429 on the whole
// cascade — saying the job gave up (rateLimitGaveUp), with no sentinel —
// processJob would wait an ErrRateLimited out again — so the job ends in
// Error (setJobError, which never writes over an operator's Cancel). The
// waiting room's chat stops with it.
func (sp *StreamProcessor) giveUpRateLimited(job *database.Job, l *loopRateLimit, info *youtube.VideoInfo, err error, chatDl *chat.ChatDownloader) *StreamProcessResult {
	sp.stopEarlyChat(chatDl)
	refusal := err
	if refusal == nil {
		msg, _ := playabilityVerdict(info)
		refusal = errors.New(msg)
	}
	sp.logger.Warn("YouTube has refused the waiting loop's full fetches for a day; giving up",
		"videoID", job.VideoID, "since", l.capSince.UTC().Format(time.RFC3339), "refusals", l.capRefusals, "refusal", refusal)
	return &StreamProcessResult{VideoInfo: info, ShouldDownload: false, Error: rateLimitGaveUp(refusal).Error()}
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

// rateLimitSpells is when each job's current spell of rate-limit refusals
// began, on the wall clock (outlastRateLimit), what rateLimitCap is measured
// from. A backlog VOD's spell spans its runs — each refusal sends it back to
// Queued, and the hourly run after it is a new one — so it cannot live in a
// run. In memory, as the rest of the backlog retry state is (backlogRetries,
// the scheduler's holds): a restart starts every spell over, which errs on
// the side of waiting, and needs no column. A pass whose fetch YouTube
// answered ends the spell (processJob), and so does a run that ends other
// than back in Queued or an operator's verb (forgetBacklogRetries,
// endBacklogStreak) — a dashboard Retry starts the job over. A transient
// failure, which is no answer, leaves it standing. Lazily allocated.
type rateLimitSpells struct {
	mu    sync.Mutex
	began map[string]rateLimitSpell
}

// rateLimitSpell is one job's spell: when its first refusal came, and how
// many refusals it has had (rateLimitCapRefusals).
type rateLimitSpell struct {
	began    time.Time
	refusals int
}

// note records a refusal of jobID at now and returns when its spell began —
// now, for the first of one — and how many refusals it holds, this one
// included.
func (s *rateLimitSpells) note(jobID string, now time.Time) (time.Time, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.began == nil {
		s.began = map[string]rateLimitSpell{}
	}
	sp, ok := s.began[jobID]
	if !ok {
		sp.began = now
	}
	sp.refusals++
	s.began[jobID] = sp
	return sp.began, sp.refusals
}

// end forgets jobID's spell.
func (s *rateLimitSpells) end(jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.began, jobID)
}

// ids lists the jobs with a spell standing.
func (s *rateLimitSpells) ids() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.began))
	for id := range s.began {
		out = append(out, id)
	}
	return out
}

// outlastRateLimit is processJob's handling of a run YouTube rate-limited
// (isRateLimited) — its verdict or a 429 — from stream processing or from a
// stale VOD's re-extraction. The limit holds backlog admission first,
// whatever the job is, and a captcha tells the operator (captchaAlert). A
// spell that has lasted rateLimitCap ends the job in Error, saying so
// (rateLimitGaveUp; setJobError, which never writes over an operator's
// Cancel). Otherwise a backlog VOD goes back to Queued for its backoff
// (requeueBacklogAfterRateLimit), or, when it cannot, ends as setJobError
// ends it — a Cancel that landed meanwhile is reported as one. Any other job
// gives back its slots, waits in place under the wait's progress line
// (rateLimitWait; the captcha's own, captchaWaitLine), and is run again from
// stream processing.
//
// Returns the row as it now reads when the run goes on, or nil when it is
// over: the backlog VOD's requeue or its Error, or a run whose ctx ended, or
// whose row was deleted or left the states the queue processes while it
// waited (waitOutOutage's re-read) — which this hands to handleCancellation.
// The cap needs the day and rateLimitCapRefusals refusals both: a backlog VOD
// re-admitted a day after its first refusal is requeued again, not ended.
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
// VOD's above all, one refusal per run — still extends the hold. The cap is
// measured from the job's spell (rateLimitSpells), which a backlog VOD's
// runs share.
func (w *DownloadWorker) outlastRateLimit(ctx context.Context, job *database.Job, err error, since *time.Time) *database.Job {
	now := time.Now()
	first := since.IsZero()
	if first {
		*since = now
		if w.scheduler != nil {
			w.scheduler.holdForRateLimit(*since, rateLimitBackoff)
		}
	}
	captcha := isCaptcha(err)
	if captcha {
		w.captcha.raise(job)
	}
	if began, refusals := w.rateLimitSpells.note(job.ID, now); now.Sub(began) >= rateLimitCap && refusals >= rateLimitCapRefusals {
		w.logger.Warn("YouTube has refused this job's fetches for a day; giving up",
			"jobID", job.ID, "since", began.UTC().Format(time.RFC3339), "refusals", refusals, "err", err)
		w.setJobError(job, rateLimitGaveUp(err))
		return nil
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
		"jobID", job.ID, "status", job.Status, "retryIn", wait, "captcha", captcha, "err", err)
	w.queue.ReleaseSlots(job.ID)
	if !w.waitOutRateLimit(ctx, job.ID, wait, waitLineFor(captcha)) {
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

// noteAnswered is what a pass whose fetch YouTube answered ends (processJob):
// the job's spell of refusals. The session's captcha spell ends at the fetch
// itself (Process), which sees every answer, not only those that go on to
// download.
func (w *DownloadWorker) noteAnswered(job *database.Job) {
	w.rateLimitSpells.end(job.ID)
}

// waitOutRateLimit holds jobID's run for wait under the wait's progress
// line (line, with the time left), rewritten as it runs and cleared however
// the wait ends, and reports false when ctx ends first.
func (w *DownloadWorker) waitOutRateLimit(ctx context.Context, jobID string, wait time.Duration, line func(time.Duration) string) bool {
	until := time.Now().Add(wait)
	w.db.UpdateJobFields(jobID, map[string]any{"progress": line(wait)})
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
			w.db.UpdateJobFields(jobID, map[string]any{"progress": line(time.Until(until))})
		case <-ctx.Done():
			return false
		}
	}
}

// captchaAlert is the session's captcha spell: open from the first captcha
// refusal any job meets until a fetch YouTube answers (answeredFetch). Its
// opening sends the operator one notification — "captcha_required" — so
// they know to solve it in a browser signed in with the same account; every
// other captcha refusal of the spell, from that job or any other, sends
// nothing more. A captcha is the account's (or, with no cookies, the
// address's), not the video's, so the whole backlog meets the same one, and
// a notification per job was one per refused video every hour. Its end — the
// first fetch YouTube answers — sends one follow-up, "YouTube Captcha
// Cleared", success-coloured, through the same event (captcha_required) and
// so the same targets, the shape the alert pairs (sidecar_down /
// sidecar_restored, disk_* / disk_ok) close with: the operator told to solve
// a captcha is told it is solved, rather than left to guess from the jobs
// that resumed (the owner's decision on FA's fourth follow-up). The jobs it
// held fetch again on their own cadence. captcha_required stays out of the
// default mention set (config.DefaultMentionEvents), so neither pings
// anyone unless a target opts in. In memory: a restart that meets the
// captcha again tells the operator again, and a spell a restart cut short
// sends no close. Shared by the worker and its
// stream processor; nil-safe, so a processor a test builds without one
// alerts nobody.
type captchaAlert struct {
	notify notifications.Sender // nil: logged only
	log    logger

	mu   sync.Mutex
	open bool
	// firstTitle, firstVideoID: the spell's first refused video, named again
	// in the close.
	firstTitle, firstVideoID string
}

// captchaRequiredTitle and captchaClearedTitle are the spell's opening and
// closing notifications' titles, both sent on the captcha_required event.
const (
	captchaRequiredTitle = "YouTube Captcha Required"
	captchaClearedTitle  = "YouTube Captcha Cleared"
)

func newCaptchaAlert(notify notifications.Sender, log logger) *captchaAlert {
	return &captchaAlert{notify: notify, log: log}
}

// raise opens the spell for a captcha job met, telling the operator if it was
// not open already.
func (a *captchaAlert) raise(job *database.Job) {
	if a == nil {
		return
	}
	title := job.Title
	if title == "" {
		title = job.VideoID
	}
	a.mu.Lock()
	opening := !a.open
	a.open = true
	if opening {
		a.firstTitle, a.firstVideoID = title, job.VideoID
	}
	a.mu.Unlock()
	if !opening {
		return
	}
	a.log.Warn("YouTube asks this session for a captcha; jobs it refuses wait it out, and the operator is told once",
		"jobID", job.ID, "videoID", job.VideoID)
	if a.notify == nil {
		return
	}
	link := job.URL
	if link == "" && job.VideoID != "" {
		link = "https://www.youtube.com/watch?v=" + job.VideoID
	}
	a.notify.Send(captchaRequiredTitle,
		fmt.Sprintf("YouTube asks this session to solve a captcha before it plays videos. Solve it in a browser signed in with the same YouTube account Moombox's cookies are from (with no cookies, a browser on the same network) — opening the video below shows it. Until then every job it refuses waits, up to %d hours, then ends in Error",
			int(rateLimitCap/time.Hour)),
		notifications.TypeWarning,
		notifications.NewFieldBuilder().
			AddIf(title != "", "First Refused", notifications.EscapeMarkdown(title)).
			AddInlineIf(job.VideoID != "", "Video ID", job.VideoID).
			Build(),
		notifications.SendOptions{Event: "captcha_required", URL: link},
	)
}

// clear ends the spell: a fetch YouTube answered. A spell that was open
// sends its one close, "YouTube Captcha Cleared", on the captcha_required
// event; the next captcha opens a new one, and tells the operator again.
func (a *captchaAlert) clear() {
	if a == nil {
		return
	}
	a.mu.Lock()
	was := a.open
	a.open = false
	title, videoID := a.firstTitle, a.firstVideoID
	a.firstTitle, a.firstVideoID = "", ""
	a.mu.Unlock()
	if !was {
		return
	}
	a.log.Info("YouTube answered a fetch again; the captcha spell is over")
	if a.notify == nil {
		return
	}
	a.notify.Send(captchaClearedTitle,
		"YouTube answered a fetch again: the captcha is solved or has passed. The jobs it held fetch again on their own cadence",
		notifications.TypeSuccess,
		notifications.NewFieldBuilder().
			AddIf(title != "", "First Refused", notifications.EscapeMarkdown(title)).
			AddInlineIf(videoID != "", "Video ID", videoID).
			Build(),
		notifications.SendOptions{Event: "captcha_required"},
	)
}

// holdForRateLimit holds every backlog admission for d from now — or past
// the hold already standing, when that one runs out later. A rate limit is
// the account's or the address's, not the video's: an admission while it
// stands is a backlog VOD sent into the same wall, so sweep admits nothing
// until the hold runs out (rateLimitGateClosed), and a fresh verdict meanwhile
// pushes it on. Only backlog waits, as on a full disk: live, upcoming and
// manually added jobs never pass through the scheduler, and a backlog VOD
// already admitted runs on. All backlog is YouTube's today; a Twitch backlog
// would want its own hold. Saved for the next process on every change
// (RecordRateLimitHold), as the disk gate's hold is.
//
// Called from any job's run, while sweep reads it on Run's goroutine.
func (s *Scheduler) holdForRateLimit(now time.Time, d time.Duration) {
	until := now.Add(d)
	s.rateMu.Lock()
	if !until.After(s.rateLimitedUntil) {
		s.rateMu.Unlock()
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
	s.rateMu.Unlock()
	s.reportRateLimitHold()
	if opening {
		s.log.Warn("scheduler: YouTube rate-limited this session; backlog VODs wait in Queued until the hold runs out (live and manually added jobs are not held)",
			"until", until.UTC().Format(time.RFC3339))
		return
	}
	s.log.Debug("scheduler: another rate-limit verdict; the backlog hold is extended",
		"until", until.UTC().Format(time.RFC3339))
}

// rateLimitGateClosed reports whether a rate-limit hold stands at now, and
// logs, once, the sweep that finds it run out — which is saved, so the next
// process starts with none.
func (s *Scheduler) rateLimitGateClosed(now time.Time) bool {
	s.rateMu.Lock()
	if !s.rateHeld {
		s.rateMu.Unlock()
		return false
	}
	if now.Before(s.rateLimitedUntil) {
		s.rateMu.Unlock()
		return true
	}
	s.rateHeld = false
	s.rateMu.Unlock()
	s.reportRateLimitHold()
	s.log.Info("scheduler: the YouTube rate-limit hold has run out; backlog admission resumes")
	return false
}

// RecordRateLimitHold has the rate-limit hold report when it runs out: on
// every hold placed or pushed on, on the sweep that finds it run out (zero:
// none stands), and when RestoreRateLimitHold seeds one. cmd/moombox saves it
// beside the disk gate's hold in open-alerts.json, for the next process's
// RestoreRateLimitHold. In memory alone, a restart inside the hour — an
// update, a crash, a settings save that restarts — reopened admission into
// the limit the last process had stopped admitting into, and the backlog
// spent the rest of the hour finding the wall again, a VOD at a time. Call it
// before the worker starts: record is called from any job's run from then on,
// never with the hold's lock held, and the reports are serialised and each
// reads the hold as it stands, so the last one saved is the current hold.
func (s *Scheduler) RecordRateLimitHold(record func(until time.Time)) {
	s.recordRateLimitHold = record
}

// reportRateLimitHold tells the recorder RecordRateLimitHold set when the
// hold now runs out.
func (s *Scheduler) reportRateLimitHold() {
	if s.recordRateLimitHold == nil {
		return
	}
	s.rateRecordMu.Lock()
	defer s.rateRecordMu.Unlock()
	s.rateMu.Lock()
	var until time.Time
	if s.rateHeld {
		until = s.rateLimitedUntil
	}
	s.rateMu.Unlock()
	s.recordRateLimitHold(until)
}

// RestoreRateLimitHold starts the backlog held until until, for a host whose
// previous process stopped with a rate-limit hold standing (cmd/moombox), and
// reports whether it did. A hold that has run out is a no-op: it would have
// run out had the process gone on. One that ends more than rateLimitBackoff
// from now — a clock set back since it was saved — is held for the hour only,
// the most a hold placed now could be. The first sweep past it reopens
// admission, as it reopens any hold. Call it before the worker starts, after
// RecordRateLimitHold, which it reports to.
func (s *Scheduler) RestoreRateLimitHold(until time.Time) bool {
	now := time.Now()
	if !now.Before(until) {
		s.log.Debug("scheduler: the last run's YouTube rate-limit hold has run out; nothing to restore",
			"until", until.UTC().Format(time.RFC3339))
		return false
	}
	if limit := now.Add(rateLimitBackoff); until.After(limit) {
		until = limit
	}
	s.rateMu.Lock()
	s.rateLimitedUntil, s.rateHeld = until, true
	s.rateMu.Unlock()
	s.reportRateLimitHold()
	s.log.Warn("scheduler: the last run stopped with a YouTube rate-limit hold standing; backlog VODs wait in Queued until it runs out (live and manually added jobs are not held)",
		"until", until.UTC().Format(time.RFC3339))
	return true
}
