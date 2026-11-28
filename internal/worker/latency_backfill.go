package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/redact"
	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// The latency-mode backfill (owner ruling RV-19, "Backfill everything in the
// background"). The worker stores a YouTube broadcast's latency mode at the
// first extraction of a capture (latencyModeToStore), so recordings that
// finished before v30, and VOD downloads, have none, and the player cannot
// suggest a chat offset for them (jobstate.SuggestedChatOffset). This walks
// those rows slowly and asks YouTube once per recording:
//
//   - one row at a time, latencyBackfillPause between requests, the first
//     latencyBackfillStartDelay after start (past the boot's own burst);
//   - only while online, and never while a rate-limit hold stands: the
//     backlog scheduler's (Scheduler.RateLimitHeldUntil), or its own, placed for
//     rateLimitBackoff (the hour the scheduler holds) when its own request is
//     refused for a rate limit. It does not place the scheduler's hold: a
//     verdict on a background request is not a reason to hold the backlog.
//     Its own hold outlives a restart as the scheduler's does: saved in
//     open-alerts.json by cmd/moombox (RecordLatencyBackfillHold,
//     RestoreLatencyBackfillHold);
//   - never while a capture is starting (a row in Live: the step between the
//     go-live confirmation and the first segment, when the capture makes its
//     own requests);
//   - the request is the status probe (Service.ProbeVideoStatus: one
//     cookieless ANDROID_VR call, escalated once when it refuses with no
//     details), and the authenticated TV probe once more for a members-only
//     refusal when the jar is signed in, as the feed walk does. Every client
//     that returned videoDetails gave the same latencyClass, ended streams
//     included (the 2026-11-27 research);
//   - an answer is stored for good (classifyLatencyAnswer): the stated mode,
//     or LatencyModeUnstated when a playable answer's videoDetails carried
//     no class (an ordinary upload: never a stream) or when YouTube calls
//     the video private, gone or blocked here (isTerminalPlayability, from
//     the probe and its escalation alike);
//   - anything else leaves the row NULL: it is asked again on a later pass,
//     no sooner than latencyBackfillRetryAfter, and at most
//     latencyBackfillMaxTries times in one process, so one bad video cannot
//     loop. A failure met while going offline is not counted.
//
// The rows themselves are the state: a restart walks every row still NULL
// from the start. The one thing saved beside them is the walk's own
// rate-limit hold, so a restart inside its hour does not ask again into the
// limit.
//
// The switch is monitors.latency_backfill (owner ruling RV-20), default on,
// read before every request: off stops the walk before its next request (a
// request already sent finishes and stores its answer) and touches nothing
// else, so the saved hold stays and the rows keep what they hold; on resumes
// from the NULL rows. A Settings save wakes the walk
// (DownloadWorker.WakeLatencyBackfill) so it stops or resumes at once. Off
// stops only this walk: a new capture or download still stores its mode at
// its first extraction, and the player still applies suggested offsets.
// What the walk is doing is told twice (latency_backfill_status.go): an
// Info line each time it enters a state, and one derived status line beside
// the switch in Settings in both UIs. Once the archive is caught up it costs
// one request per newly finished row it has to ask about (a VOD download of
// an upload, which the worker leaves NULL) and a database read an hour.

const (
	// latencyBackfillStartDelay is how long after start the first request
	// waits.
	latencyBackfillStartDelay = 5 * time.Minute
	// latencyBackfillPause is the wait between two steps: two requests, or
	// two looks at a hold, an outage or a starting capture.
	latencyBackfillPause = time.Minute
	// latencyBackfillIdle is the wait once no row is left to ask about: a
	// recording that finishes meanwhile is picked up on the next look.
	latencyBackfillIdle = time.Hour
	// latencyBackfillRetryAfter is how long a row whose request failed waits
	// before it is asked again.
	latencyBackfillRetryAfter = time.Hour
	// latencyBackfillMaxTries is how many failed requests a row gets in one
	// process before the walk stops asking about it until the next start.
	latencyBackfillMaxTries = 3
	// latencyBackfillPage is how many rows one database read returns.
	latencyBackfillPage = 50
)

// latencyBackfill is the walk's state and what it reads through. Every
// field after logger is injectable for the tests; nil means the production
// default noted on it.
type latencyBackfill struct {
	db     *database.Database
	logger logger
	// enabled reports monitors.latency_backfill (production: the live
	// config, read before every request); nil is always on.
	enabled func() bool
	// wake is signalled when the switch may have moved (a Settings save in
	// either UI, DownloadWorker.WakeLatencyBackfill), so the walk stops or
	// resumes at once rather than at its next look; nil never wakes.
	wake <-chan struct{}
	// off is whether the last step found the walk switched off.
	off bool
	// probe asks YouTube about one video (production: latencyProbe). nil:
	// the walk never runs.
	probe func(ctx context.Context, videoID string) (*youtube.VideoInfo, error)
	// online reports the device online; nil always is.
	online func() bool
	// rateLimitHeld reports the backlog scheduler's rate-limit hold standing
	// at now, and when it runs out; nil never is.
	rateLimitHeld func(now time.Time) (until time.Time, held bool)
	// captureStarting reports a capture starting; nil never is.
	captureStarting func() bool
	// now is the clock; nil is time.Now.
	now func() time.Time

	// cursor is the job ID of the last row handed out: the walk goes on
	// after it, and from the start once it runs off the end.
	cursor string
	// heldUntil is the walk's own rate-limit hold; zero when none stands.
	heldUntil time.Time
	// recordHold is told when the walk's own hold now runs out (zero: none
	// stands), on every change; nil records nothing.
	recordHold func(until time.Time)
	// tries counts each row's failed requests in this process.
	tries map[string]latencyTry
	// current is the row being asked about, for the recover.
	current string

	// mu guards phase, which LatencyBackfillStatus reads from other
	// goroutines; every other field is the walk goroutine's own.
	mu sync.Mutex
	// phase is the state the walk last entered (enter); the zero value is
	// LatencyBackfillWaiting, before the first step.
	phase latencyPhase
	// changed is told after every phase change (production: the worker's
	// latencyChanged signal, which cmd/moombox pushes to both UIs); nil
	// tells nobody.
	changed func()
	// run is what the run in progress has done, for its log lines.
	run latencyRun
}

// latencyRun is one run of the walk: from the step that found a row to ask
// about, after start, after being switched on or after catching up, to the
// step that finds none left (or the switch off).
type latencyRun struct {
	// active is whether a run is in progress: its next running phase is a
	// resume, not a start.
	active bool
	// filled is how many rows the run stored a mode for; unstated is how
	// many of them YouTube would not say (LatencyModeUnstated); gaveUp is
	// how many rows it stopped asking about until the next start.
	filled, unstated, gaveUp int
}

// latencyTry is one row's failed requests and when it may be asked again.
type latencyTry struct {
	n    int
	next time.Time
}

// newLatencyBackfill is the worker's walk: its database, its YouTube
// service, its connectivity monitor, and its scheduler's rate-limit hold. It
// is also the walk LatencyBackfillStatus reads.
func (w *DownloadWorker) newLatencyBackfill() *latencyBackfill {
	b := &latencyBackfill{db: w.db, logger: w.logger, heldUntil: w.latencyHoldUntil, recordHold: w.latencyHoldRecord,
		wake: w.latencyWake}
	b.enabled = func() bool {
		on := true
		w.readConfig(func(c *config.MoomboxConfig) {
			if c != nil {
				on = c.Monitors.LatencyBackfillEnabled()
			}
		})
		return on
	}
	if w.yt != nil {
		b.probe = w.latencyProbe
	}
	if w.conn != nil {
		b.online = w.conn.IsOnline
	}
	if w.scheduler != nil {
		b.rateLimitHeld = w.scheduler.RateLimitHeldUntil
	}
	b.changed = w.signalLatencyChanged
	if w.db != nil {
		b.captureStarting = func() bool {
			has, err := w.db.HasJobWithStatus(database.StatusLive)
			return err != nil || has
		}
	}
	// The walk LatencyBackfillStatus reads its phase from.
	w.latencyWalk.Store(b)
	return b
}

// latencyProbe is the walk's one request: the status probe, and for a
// members-only refusal the authenticated TV probe when the jar is signed in
// (the feed walk's retry: a member's session sees the broadcast's details).
func (w *DownloadWorker) latencyProbe(ctx context.Context, videoID string) (*youtube.VideoInfo, error) {
	return latencyProbeVia(ctx, videoID, w.yt.ProbeVideoStatus, w.yt.ProbeVideoStatusAuthenticated, w.yt.HasAuthCookies)
}

// latencyProbeVia is latencyProbe over its three calls: probe, then authProbe
// for a members-only refusal that named no class (with or without
// videoDetails: a refusal often carries the title and channel) while
// signedIn.
func latencyProbeVia(ctx context.Context, videoID string,
	probe, authProbe func(context.Context, string) (*youtube.VideoInfo, error), signedIn func() bool) (*youtube.VideoInfo, error) {
	info, err := probe(ctx, videoID)
	if err == nil && info != nil && info.LatencyClass == "" &&
		info.PlayabilityError == youtube.PlayabilityMembersOnly && signedIn() {
		return authProbe(ctx, videoID)
	}
	return info, err
}

// runLatencyBackfill runs the walk until ctx ends. A panic in one step is
// recovered in stepRecovered; the recover here is the goroutine's own
// backstop.
func (w *DownloadWorker) runLatencyBackfill(ctx context.Context, b *latencyBackfill) {
	defer func() {
		if r := recover(); r != nil {
			w.logger.Error("panic in the latency-mode backfill", "panic", fmt.Sprint(r))
		}
	}()
	if b == nil || b.probe == nil || b.db == nil {
		return
	}
	if utils.Sleep(ctx, latencyBackfillStartDelay) != nil {
		return
	}
	for {
		d := b.stepRecovered(ctx)
		if ctx.Err() != nil || !b.sleep(ctx, d) {
			return
		}
	}
}

// sleep waits d before the next step, or less when a wake finds the switch
// moved since the last step (off to on, or on to off): the step then runs at
// once, to stop or resume now. A wake that finds it where it was goes on
// waiting out d, so a save that leaves the switch where it was never makes the
// walk ask sooner. Switching it on asks at once, so off and straight back on
// can send a request less than latencyBackfillPause after the last one (the
// rate-limit holds still stand). Reports false when ctx ends.
func (b *latencyBackfill) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		case <-b.wake:
			if b.isEnabled() == b.off {
				return true
			}
		}
	}
}

// isEnabled reports the switch; a nil enabled is on.
func (b *latencyBackfill) isEnabled() bool { return b.enabled == nil || b.enabled() }

// stepRecovered is step with a recover of its own: a row whose step
// panicked is counted as a failed request, so it cannot panic the walk on
// every pass.
func (b *latencyBackfill) stepRecovered(ctx context.Context) (wait time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			b.logger.Error("panic in a latency-mode backfill step", "jobID", b.current, "panic", fmt.Sprint(r))
			if b.current != "" {
				b.fail(b.current, b.clock())
			}
			b.current = ""
			wait = latencyBackfillPause
		}
	}()
	return b.step(ctx)
}

func (b *latencyBackfill) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *latencyBackfill) isOnline() bool { return b.online == nil || b.online() }

// step asks about at most one row and returns how long to wait before the
// next step.
func (b *latencyBackfill) step(ctx context.Context) time.Duration {
	now := b.clock()
	// Switched off (owner ruling RV-20): nothing is asked and nothing else
	// is touched. A request already sent has finished and stored its answer,
	// and the walk's own rate-limit hold stays as saved (only a step that
	// runs while on clears it), so switching on again inside the hour does
	// not ask into the limit.
	b.off = !b.isEnabled()
	if b.off {
		b.enter(latencyPhase{state: LatencyBackfillOff})
		return latencyBackfillPause
	}
	if !b.isOnline() {
		return b.pauseIfWork(now, latencyPhase{state: LatencyBackfillPaused, pause: LatencyPauseOffline})
	}
	if !b.heldUntil.IsZero() && !now.Before(b.heldUntil) {
		b.setHold(time.Time{})
	}
	if until, held := b.rateLimitHold(now); held {
		return b.pauseIfWork(now, latencyPhase{state: LatencyBackfillPaused, pause: LatencyPauseRateLimit, until: until})
	}
	if b.captureStarting != nil && b.captureStarting() {
		return b.pauseIfWork(now, latencyPhase{state: LatencyBackfillPaused, pause: LatencyPauseCaptureLive})
	}
	row, ok, err := b.next(now)
	if err != nil {
		b.logger.Warn("latency backfill: could not read the recordings to ask about", "err", err)
		return latencyBackfillPause
	}
	if !ok {
		b.enter(latencyPhase{state: LatencyBackfillCaughtUp})
		return latencyBackfillIdle
	}
	b.enter(latencyPhase{state: LatencyBackfillRunning})

	// Named for stepRecovered while the request runs: a panic leaves it set.
	b.current = row.JobID
	info, err := b.probe(ctx, row.VideoID)
	b.current = ""
	if ctx.Err() != nil {
		return 0
	}
	mode, verdict := classifyLatencyAnswer(info, err)
	switch verdict {
	case latencyAnswered:
		delete(b.tries, row.JobID)
		if b.db.SetLatencyModeIfUnset(row.JobID, mode) {
			b.run.filled++
			if mode == database.LatencyModeUnstated {
				b.run.unstated++
			}
			// The phase stays running, but the counts moved: both UIs read
			// the status again, so an open Settings line counts each row (at
			// most one push and one count a minute, only while rows fill).
			if b.changed != nil {
				b.changed()
			}
			b.logger.Debug("latency backfill: stored the broadcast's latency mode", "jobID", row.JobID, "mode", string(mode))
		}
	case latencyRateLimited:
		b.setHold(now.Add(rateLimitBackoff))
		b.logger.Debug("latency backfill: YouTube rate-limited the request", "jobID", row.JobID)
		until, _ := b.rateLimitHold(now)
		b.enter(latencyPhase{state: LatencyBackfillPaused, pause: LatencyPauseRateLimit, until: until})
	default:
		if !b.isOnline() {
			// Failed as the device went offline: not the video's failure.
			return latencyBackfillPause
		}
		why := latencyFailureOf(info, err)
		if n := b.fail(row.JobID, now); n >= latencyBackfillMaxTries {
			b.run.gaveUp++
			b.logger.Info("latency backfill: giving up on this recording until the next start",
				why.logArgs(row.JobID, n)...)
		} else {
			b.logger.Debug("latency backfill: the request failed; asking again on a later pass",
				why.logArgs(row.JobID, n)...)
		}
	}
	return latencyBackfillPause
}

// pauseIfWork enters the pause p only when a row is waiting to be asked
// about: a walk with nothing to ask (caught up, or a first step with no row)
// is caught up whatever holds it, so it logs no pause and a zero caught-up
// line after it at every hourly look. The look reads the database and sends
// no request, and leaves the cursor where it was, so the row it found is
// the one asked about once the pause ends. A failed read pauses: the step
// after it reads again.
func (b *latencyBackfill) pauseIfWork(now time.Time, p latencyPhase) time.Duration {
	cursor := b.cursor
	_, ok, err := b.next(now)
	b.cursor = cursor
	if err == nil && !ok {
		b.enter(latencyPhase{state: LatencyBackfillCaughtUp})
		return latencyBackfillIdle
	}
	b.enter(p)
	return latencyBackfillPause
}

// setHold sets the walk's own rate-limit hold to until (zero: none) and
// records it for the next process.
func (b *latencyBackfill) setHold(until time.Time) {
	b.heldUntil = until
	if b.recordHold != nil {
		b.recordHold(until)
	}
}

// RecordLatencyBackfillHold has the latency-mode backfill report its own
// rate-limit hold on every change (zero when it runs out), for cmd/moombox
// to save in open-alerts.json beside the scheduler's hold. In memory alone,
// a restart inside the hour sent the walk's next request, five minutes after
// start, into the limit it had just met. Call it before Start.
func (w *DownloadWorker) RecordLatencyBackfillHold(record func(until time.Time)) {
	w.latencyHoldRecord = record
}

// RestoreLatencyBackfillHold starts the backfill held until until, for a
// host whose previous process stopped with the walk's own hold standing, and
// reports whether it did: by Scheduler.RestoreRateLimitHold's rules, a hold
// that has run out restores nothing, and one ending more than
// rateLimitBackoff from now (a clock set back) is held for the hour only.
// Call it before Start.
func (w *DownloadWorker) RestoreLatencyBackfillHold(until time.Time) bool {
	now := time.Now()
	if !now.Before(until) {
		return false
	}
	if limit := now.Add(rateLimitBackoff); until.After(limit) {
		until = limit
	}
	w.latencyHoldUntil = until
	return true
}

// latencyFailure is what one failed request's answer said, for the walk's
// failure lines: a nil error with an answer YouTube refused or would not
// place, or the status probe's inconclusive refusal by both clients asked.
// It never holds a cookie, a token or visitor data: the kind, YouTube's
// reason text, whether videoDetails came back and the client's name are all
// the answer carries, and err's text goes through redact.MediaText.
type latencyFailure struct {
	err string
	// answered is whether YouTube answered at all: false for a network
	// failure, a timeout or an empty answer, whose lines carry err alone.
	answered bool
	kind     youtube.PlayabilityError
	reason   string
	details  bool
	// client names the client that answered, "" when the answer does not
	// say (latencyAnswerClient); escalation and escalationKind are the
	// status probe's second client and its own refusal, on an inconclusive
	// probe.
	client         string
	escalation     string
	escalationKind youtube.PlayabilityError
}

// latencyFailureOf reads one failed request's info and err.
func latencyFailureOf(info *youtube.VideoInfo, err error) latencyFailure {
	f := latencyFailure{err: redact.MediaText(errText(err))}
	if inc, ok := errors.AsType[*youtube.StatusProbeInconclusiveError](err); ok {
		f.answered = true
		f.kind, f.reason, f.client = inc.Playability, inc.Reason, "android_vr"
		f.escalation, f.escalationKind = inc.Escalation, inc.EscalationPlayability
		return f
	}
	if err == nil && info != nil {
		f.answered = true
		f.kind, f.reason, f.details = info.PlayabilityError, info.PlayabilityReason, info.LatencyClassAsked
		f.client = latencyAnswerClient(info)
	}
	return f
}

// latencyAnswerClient is the client an answer names: the status probe's
// escalation when it answered in ANDROID_VR's place, else the client
// stamped on its manifest; "" when it names none (an upload's answer often
// carries no manifest).
func latencyAnswerClient(info *youtube.VideoInfo) string {
	switch {
	case info.StatusProbeEscalation != "":
		return info.StatusProbeEscalation
	case info.DashManifestSource != "":
		return info.DashManifestSource
	default:
		return info.HlsManifestSource
	}
}

// logArgs are a failure line's key-value pairs for jobID's n-th try.
func (f latencyFailure) logArgs(jobID string, n int) []any {
	args := []any{"jobID", jobID, "tries", n, "err", f.err}
	if !f.answered {
		return args
	}
	args = append(args, "playability", string(f.kind), "reason", f.reason, "videoDetails", f.details)
	if f.client != "" {
		args = append(args, "client", f.client)
	}
	if f.escalation != "" {
		args = append(args, "escalation", f.escalation, "escalationPlayability", string(f.escalationKind))
	}
	return args
}

// errText is err's text, or "" for none.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// fail counts one failed request for jobID at now and returns the count.
func (b *latencyBackfill) fail(jobID string, now time.Time) int {
	if b.tries == nil {
		b.tries = make(map[string]latencyTry)
	}
	t := b.tries[jobID]
	t.n++
	t.next = now.Add(latencyBackfillRetryAfter)
	b.tries[jobID] = t
	return t.n
}

// eligible reports whether jobID may be asked about at now: not given up on,
// and past its retry wait.
func (b *latencyBackfill) eligible(jobID string, now time.Time) bool {
	t, ok := b.tries[jobID]
	return !ok || (t.n < latencyBackfillMaxTries && !now.Before(t.next))
}

// next is the next row to ask about after the cursor, wrapping to the start
// once: each row is looked at no more than twice per call, so a walk whose
// every row waits or was given up on ends with none.
func (b *latencyBackfill) next(now time.Time) (database.LatencyBackfillRow, bool, error) {
	wrapped := b.cursor == ""
	for {
		rows, err := b.db.LatencyBackfillRows(b.cursor, latencyBackfillPage)
		if err != nil {
			return database.LatencyBackfillRow{}, false, err
		}
		if len(rows) == 0 {
			b.cursor = ""
			if wrapped {
				return database.LatencyBackfillRow{}, false, nil
			}
			wrapped = true
			continue
		}
		for _, r := range rows {
			b.cursor = r.JobID
			if b.eligible(r.JobID, now) {
				return r, true, nil
			}
		}
	}
}

// latencyVerdict is what one request settled.
type latencyVerdict int

const (
	// latencyAnswered: the mode to store, for good.
	latencyAnswered latencyVerdict = iota
	// latencyRateLimited: the session is rate-limited; nothing settled, and
	// not the row's failure.
	latencyRateLimited
	// latencyTransient: nothing settled; the row is asked again later.
	latencyTransient
)

// classifyLatencyAnswer reads one request's answer (owner ruling RV-19):
//
//   - a class came back: the stated mode (LatencyModeUnstated for one this
//     build does not know);
//   - a playable answer with videoDetails and no class: LatencyModeUnstated
//     (an ordinary upload says none). A refusal's videoDetails without a
//     class settle nothing: the rules below read its playability;
//   - refused for a rate limit (the probe's own, its escalation's, or a 429
//     on the authenticated probe): latencyRateLimited;
//   - refused as private, unavailable or region-blocked by every client
//     asked (isTerminalPlayability): LatencyModeUnstated, for good;
//   - anything else (a network failure, a bot wall, a members-only refusal,
//     an age gate, an answer about another video): latencyTransient.
func classifyLatencyAnswer(info *youtube.VideoInfo, err error) (database.LatencyMode, latencyVerdict) {
	if err != nil {
		if _, ok := errors.AsType[*youtube.HTTPRateLimitError](err); ok {
			return "", latencyRateLimited
		}
		if inc, ok := errors.AsType[*youtube.StatusProbeInconclusiveError](err); ok {
			if inc.RateLimited() || inc.EscalationPlayability == youtube.PlayabilityRateLimited {
				return "", latencyRateLimited
			}
			if isTerminalPlayability(inc.Playability) && isTerminalPlayability(inc.EscalationPlayability) {
				return database.LatencyModeUnstated, latencyAnswered
			}
		}
		return "", latencyTransient
	}
	switch {
	case info == nil:
		return "", latencyTransient
	case info.LatencyClass != "":
		return database.ParseLatencyClass(info.LatencyClass), latencyAnswered
	case info.LatencyClassAsked && info.PlayabilityError.Playable():
		// A playable answer with videoDetails and no class: YouTube would
		// not say. A refusal's details without a class say nothing (a
		// members-only or age-gated archive often carries its title and
		// channel), so it falls to the playability rules below.
		return database.LatencyModeUnstated, latencyAnswered
	case info.PlayabilityError == youtube.PlayabilityRateLimited:
		return "", latencyRateLimited
	case isTerminalPlayability(info.PlayabilityError):
		return database.LatencyModeUnstated, latencyAnswered
	}
	return "", latencyTransient
}

// WakeLatencyBackfill tells the latency-mode backfill that
// monitors.latency_backfill may have changed (a Settings save in either UI),
// so a walk switched off stops before its next request and one switched on
// resumes now, rather than at its next look, which may be up to
// latencyBackfillIdle away. Non-blocking: a wake already pending covers it.
// The walk reads the key itself, so a wake that changed nothing does
// nothing. It also signals LatencyBackfillChanged: the status reads the
// switch from the config, so both UIs show the change at once, before the
// walk's next step (in the first latencyBackfillStartDelay after start the
// walk is not listening for a wake at all).
func (w *DownloadWorker) WakeLatencyBackfill() {
	select {
	case w.latencyWake <- struct{}{}:
	default:
	}
	w.signalLatencyChanged()
}
