package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
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
//     backlog scheduler's (Scheduler.RateLimitHeld), or its own, placed for
//     rateLimitBackoff (the hour the scheduler holds) when its own request is
//     refused for a rate limit. It does not place the scheduler's hold: a
//     verdict on a background request is not a reason to hold the backlog;
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
//     or LatencyModeUnstated when videoDetails carried no class (an ordinary
//     upload: never a stream) or when YouTube calls the video private, gone
//     or blocked here (isTerminalPlayability, from the probe and its
//     escalation alike);
//   - anything else leaves the row NULL: it is asked again on a later pass,
//     no sooner than latencyBackfillRetryAfter, and at most
//     latencyBackfillMaxTries times in one process, so one bad video cannot
//     loop. A failure met while going offline is not counted.
//
// The rows themselves are the state: a restart walks every row still NULL
// from the start, and nothing else is saved. No config key: like the Twitch
// gap-fill sweep and the session keep-alive, it is a background job of the
// archive's own with no switch. Once the archive is caught up it costs one
// request per newly finished row it has to ask about (a VOD download of an
// upload, which the worker leaves NULL) and a database read an hour.

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
	// probe asks YouTube about one video (production: latencyProbe). nil:
	// the walk never runs.
	probe func(ctx context.Context, videoID string) (*youtube.VideoInfo, error)
	// online reports the device online; nil always is.
	online func() bool
	// rateLimitHeld reports the backlog scheduler's rate-limit hold standing
	// at now; nil never is.
	rateLimitHeld func(now time.Time) bool
	// captureStarting reports a capture starting; nil never is.
	captureStarting func() bool
	// now is the clock; nil is time.Now.
	now func() time.Time

	// cursor is the job ID of the last row handed out: the walk goes on
	// after it, and from the start once it runs off the end.
	cursor string
	// heldUntil is the walk's own rate-limit hold.
	heldUntil time.Time
	// tries counts each row's failed requests in this process.
	tries map[string]latencyTry
	// current is the row being asked about, for the recover.
	current string
}

// latencyTry is one row's failed requests and when it may be asked again.
type latencyTry struct {
	n    int
	next time.Time
}

// newLatencyBackfill is the worker's walk: its database, its YouTube
// service, its connectivity monitor, and its scheduler's rate-limit hold.
func (w *DownloadWorker) newLatencyBackfill() *latencyBackfill {
	b := &latencyBackfill{db: w.db, logger: w.logger}
	if w.yt != nil {
		b.probe = w.latencyProbe
	}
	if w.conn != nil {
		b.online = w.conn.IsOnline
	}
	if w.scheduler != nil {
		b.rateLimitHeld = w.scheduler.RateLimitHeld
	}
	if w.db != nil {
		b.captureStarting = func() bool {
			has, err := w.db.HasJobWithStatus(database.StatusLive)
			return err != nil || has
		}
	}
	return b
}

// latencyProbe is the walk's one request: the status probe, and for a
// members-only refusal the authenticated TV probe when the jar is signed in
// (the feed walk's retry: a member's session sees the broadcast's details).
func (w *DownloadWorker) latencyProbe(ctx context.Context, videoID string) (*youtube.VideoInfo, error) {
	return latencyProbeVia(ctx, videoID, w.yt.ProbeVideoStatus, w.yt.ProbeVideoStatusAuthenticated, w.yt.HasAuthCookies)
}

// latencyProbeVia is latencyProbe over its three calls: probe, then authProbe
// for a members-only refusal with no videoDetails while signedIn.
func latencyProbeVia(ctx context.Context, videoID string,
	probe, authProbe func(context.Context, string) (*youtube.VideoInfo, error), signedIn func() bool) (*youtube.VideoInfo, error) {
	info, err := probe(ctx, videoID)
	if err == nil && info != nil && !info.LatencyClassAsked &&
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
		if ctx.Err() != nil || utils.Sleep(ctx, d) != nil {
			return
		}
	}
}

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
	if !b.isOnline() {
		return latencyBackfillPause
	}
	if now.Before(b.heldUntil) || (b.rateLimitHeld != nil && b.rateLimitHeld(now)) {
		return latencyBackfillPause
	}
	if b.captureStarting != nil && b.captureStarting() {
		return latencyBackfillPause
	}
	row, ok, err := b.next(now)
	if err != nil {
		b.logger.Warn("latency backfill: could not read the recordings to ask about", "err", err)
		return latencyBackfillPause
	}
	if !ok {
		return latencyBackfillIdle
	}

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
			b.logger.Debug("latency backfill: stored the broadcast's latency mode", "jobID", row.JobID, "mode", string(mode))
		}
	case latencyRateLimited:
		b.heldUntil = now.Add(rateLimitBackoff)
		b.logger.Info("latency backfill: YouTube rate-limited the request; asking again after the hold",
			"jobID", row.JobID, "until", b.heldUntil.UTC().Format(time.RFC3339))
	default:
		if !b.isOnline() {
			// Failed as the device went offline: not the video's failure.
			return latencyBackfillPause
		}
		if n := b.fail(row.JobID, now); n >= latencyBackfillMaxTries {
			b.logger.Info("latency backfill: giving up on this recording until the next start",
				"jobID", row.JobID, "tries", n, "err", errText(err))
		} else {
			b.logger.Debug("latency backfill: the request failed; asking again on a later pass",
				"jobID", row.JobID, "tries", n, "err", errText(err))
		}
	}
	return latencyBackfillPause
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
//   - videoDetails came back: the stated mode, LatencyModeUnstated without a
//     class it knows (an ordinary upload says none);
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
	case info.LatencyClassAsked:
		return database.ParseLatencyClass(info.LatencyClass), latencyAnswered
	case info.PlayabilityError == youtube.PlayabilityRateLimited:
		return "", latencyRateLimited
	case isTerminalPlayability(info.PlayabilityError):
		return database.LatencyModeUnstated, latencyAnswered
	}
	return "", latencyTransient
}
