package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/monitor"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/tui"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/worker"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// crossMonitorVouchWindow bounds how recently a sibling monitor must have
// succeeded for its success to suppress another monitor's "unhealthy" alert.
// Comfortably longer than DECAPI's seconds-scale cadence and a typical feed
// interval, but short enough that a silently-stalled sibling (frozen at a stale
// success) stops vouching so a genuine outage still surfaces.
const crossMonitorVouchWindow = 20 * time.Minute

// sweepShouldResume reports whether one job should be bounced out of
// StatusCookies back to Upcoming by a credential sweep for `platform`.
//
// currentIdentity is the opaque fingerprint of the account the platform's
// cookies belong to right now, or "" when the caller has none to offer. It is
// what separates the two sweeps:
//
//   - The auth-recovered sweep passes "". Dead cookies came back to life,
//     which fixes an auth park and cannot fix a membership one.
//   - The credential sweep passes the observed identity, letting a membership
//     park move if — and only if — the account is not the one that refused it.
//
// The status+platform gate is the pre-existing behavior. What the park reason
// adds is the membership case: a job parks at ParkReasonMembership only when
// the platform answered a session it had already confirmed was SIGNED IN, so
// the auth transition cannot be the event that fixes it — that session was
// authenticated when it failed. Resuming there bought a guaranteed-identical
// failure and a full extraction attempt every auth cycle, forever.
//
// The membership comparison is deliberately against the job's OWN recorded
// identity rather than any process-level "did it change since last time" edge.
// A durable per-job comparison cannot be missed: it survives restarts, it
// cannot be consumed by an intermediate observation, and re-evaluating it is
// free and idempotent. A resumed job that fails again re-parks under the
// current identity, so it settles at exactly one retry per real account
// change.
//
// Two permissive defaults, both chosen so an unknown resolves to one wasted
// retry rather than a permanent strand:
//
//   - ParkReasonNone (every COOKIES? row predating the park_reason column, and
//     any park recorded by a path that does not classify) is resumable —
//     nothing on such a row says retroactively whether it was a membership
//     problem, and stranding a genuinely dead-cookie job is the worse error.
//   - A membership park with no recorded identity ("" — a pre-v19 row, or a
//     park where the fingerprint could not be computed) is treated as parked
//     under an unknown account and resumes on the next observation.
func sweepShouldResume(job *database.Job, platform, currentIdentity string) bool {
	if job == nil || job.Status != database.StatusCookies || job.Platform != platform {
		return false
	}
	if job.ParkReason != database.ParkReasonMembership {
		return true
	}
	// No identity on offer means this caller cannot speak to the membership
	// question at all (the auth-recovered sweep), so it must not move these.
	return currentIdentity != "" && currentIdentity != job.ParkIdentity
}

// resumeCookieParkedJobs applies sweepShouldResume to every job and returns
// how many were resumed. Split out of the callback closures so the decision
// and the database loop it actually drives can both be tested directly.
func resumeCookieParkedJobs(db *database.Database, log interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}, platform, currentIdentity string) int {
	jobs, err := db.GetAllJobs()
	if err != nil {
		log.Warn("cookie-parked sweep: GetAllJobs failed", "platform", platform, "err", err)
		return 0
	}
	resumed := 0
	for _, job := range jobs {
		if !sweepShouldResume(job, platform, currentIdentity) {
			continue
		}
		db.UpdateJobFields(job.ID, map[string]any{
			"status":        database.StatusUpcoming,
			"error":         "",
			"park_reason":   database.ParkReasonNone,
			"park_identity": "",
		})
		resumed++
	}
	return resumed
}

// recheckAfterCookieWrite runs the in-process auth re-check that MUST follow
// any pass which may have rewritten cookies.txt, and reports whether a pass
// actually ran.
//
// Why every such gesture has to end here: refresh's status block is the only
// place the Twitch credential fingerprint is compared, the auth mark cleared
// and OnCredentialsChanged fired (Arc 10 R4), and that block runs only inside
// a refresh pass. A repaired cookie file that reaches no pass is invisible
// until the 30-minute ticker — which is precisely what "immediately apply the
// updated cookie" rules out. The full enumeration of sites, and which of them
// were missing this call before Arc 10, is in the plan's Task 3 table.
//
// CheckNow rather than something lighter: every caller has just waited on a
// headless browser or a whole setup wizard, so two validate round-trips are
// not the cost that matters, and a second entry point into the status block
// would be a second mechanism containing the first.
//
// The skipped case is Info, not the service's own Debug, and that split
// predates Arc 10: the caller has just rewritten the file, the in-flight pass
// read the OLD one, so the badge stays stale until the next tick and this is
// the only line that explains it. Nothing here retries or waits — the guard's
// contract is that a second caller does nothing.
//
// checkNow is the re-check as a func — runState.checkNowFn in production —
// taken as a func rather than as a *RefreshService so the polarity, the Info
// line and the context forwarding can all be driven by a fake.
//
// The nil guard here covers a caller that passes NO FUNC. It does NOT cover a
// nil *cookies.RefreshService: a bound method value taken off a nil pointer is
// itself non-nil, so `svc.CheckNow` would step straight over this and panic
// later inside refresh. checkNowFn is what collapses that case to nil, which is
// why every production site goes through it rather than reaching for the method
// value directly.
//
// gesture names what just wrote, and args are the caller's own structured
// fields.
func recheckAfterCookieWrite(ctx context.Context, checkNow func(context.Context) bool, log interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}, gesture string, args ...any) bool {
	if checkNow == nil {
		return false
	}
	if checkNow(ctx) {
		return true
	}
	log.Info("auth re-check after "+gesture+" was skipped, a cookie refresh was already in flight — status may lag until the next refresh", args...)
	return false
}

// checkNowFn returns the in-process auth re-check as a func, or nil when this
// process has no refresh service to run one.
//
// One line of guard, in one place, because the alternative is not "no guard" —
// it is the guard that LOOKS present and is not. Every site used to pass
// s.cookieRefresh.CheckNow, and a method value taken off a nil *RefreshService
// is non-nil, so recheckAfterCookieWrite's nil check was stepped over and the
// dereference landed inside refresh at rs.mu.Lock(). Returning the nil func
// instead makes that check mean what it says at all five sites, and lets
// runCookieRecovery be driven from a zero-value runState.
//
// A func rather than the concrete pointer so the helper keeps the seam its own
// tests are built on — polarity, the skipped line and context forwarding are
// all asserted with a fake CheckNow, and none of that survives a parameter that
// can only be a live RefreshService.
//
// Nil is not reachable in production at any of the five sites: initServices
// constructs and assigns cookieRefresh in §15, before the auto-cookie wiring,
// the worker callbacks and runTUI all of which follow it.
func (s *runState) checkNowFn() func(context.Context) bool {
	if s.cookieRefresh == nil {
		return nil
	}
	return s.cookieRefresh.CheckNow
}

// reauthenticateTwitchChats broadcasts a credential change to every live
// Twitch chat session and reports how many were told.
//
// Split out of the OnCredentialsChanged closure for the same reason
// sweepShouldResume was split out of it: the closure needs a whole runState to
// build, and the decision inside it — WHICH platform gets a broadcast — is
// worth driving directly.
//
// The gate is an equality test, not "anything but youtube". A third platform
// added later must not silently inherit Twitch's reconnect behaviour, and
// dropping the gate entirely would let a YouTube cookie rotation tear down
// every live Twitch chat session on its own cadence.
//
// broadcast is DownloadWorker.ReauthenticateTwitchChats in production, taken
// as a func so a nil worker degrades to "nothing to tell". It returns a COUNT
// and nothing else: the caller logs a number, never a channel or an account.
func reauthenticateTwitchChats(platform string, broadcast func() int) int {
	if platform != "twitch" || broadcast == nil {
		return 0
	}
	return broadcast()
}

// wireCredentialRepairCallbacks installs the two RefreshService callbacks that
// fire when a platform's credentials become usable again.
//
// TWO EDGES, and neither implies the other:
//
//   - OnAuthRecovered — validate went not-authenticated to authenticated. A
//     transient Twitch-side refusal, or an operator restoring the EXACT pair
//     they had before, recovers auth with the credential fingerprint
//     unchanged, so shouldObserveCredentials returns false and the other
//     callback never fires.
//   - OnCredentialsChanged — the fingerprint moved. A same-account rotation,
//     or a swap to an account that does not validate, moves it with no auth
//     transition at all.
//
// A live Twitch chat session that went anonymous on a refusal has to hear
// about BOTH, or a transient failure strands it in anonymous capture for the
// rest of the job (Arc 10 R5, "immediately"). Everything else about the two
// stays as it was: the recovery sweep passes no identity and so holds back
// membership parks, the credential sweep passes one and so can move them.
//
// broadcast is DownloadWorker.ReauthenticateTwitchChats in production, taken
// as a func so this method can be driven directly — wireMonitorCallbacks
// cannot be.
func (s *runState) wireCredentialRepairCallbacks(broadcast func() int) {
	// reauth is the half both edges share. reauthenticateTwitchChats filters
	// the platform and is nil-safe, so this is safe to call from either.
	//
	// Called before the sweep below, but the ORDER IS NOT LOAD-BEARING and no
	// test pins it: the sweep does UpdateJobFields calls and an asynchronous
	// notification, neither of which can meaningfully delay a broadcast
	// measured in microseconds. The broadcast is simply first because there is
	// no reason to make a running capture wait — a job the sweep resumes
	// starts a fresh downloader that reads the new credentials anyway, while a
	// job already CAPTURING has no other way to learn about them.
	//
	// Only the COUNT is logged. On the OnCredentialsChanged edge an identity is
	// in scope, and it is an opaque equality token (see
	// CookieJar.TwitchIdentity) that must never reach a log line.
	reauth := func(platform string) {
		if n := reauthenticateTwitchChats(platform, broadcast); n > 0 {
			s.log.Info("twitch credentials usable again — re-authenticating live chat sessions",
				"platform", platform, "sessions", n)
		}
	}

	// When a platform transitions from not-authenticated to authenticated,
	// sweep the jobs parked in StatusCookies on that platform back to Upcoming
	// so they get re-probed without manual intervention. Closes audit
	// decision #23 (worker.md Q3).
	//
	// "the jobs", not "every job": sweepShouldResume holds back the
	// membership-parked ones, whose session was already authenticated when
	// they failed and which this transition therefore cannot fix.
	s.cookieRefresh.OnAuthRecovered = func(platform string) {
		reauth(platform)
		resumed := resumeCookieParkedJobs(s.db, s.log, platform, "")
		if resumed > 0 {
			s.log.Info("auth recovered — resumed COOKIES? jobs", "platform", platform, "count", resumed)
			// Event "auth" pairs with the worker's "Authentication Required"
			// emit — an empty Event would bypass every target's allowlist
			// (unfilterable) since the filter only applies when Event != "".
			s.notifyMgr.Send("Authentication Recovered",
				fmt.Sprintf("Resumed %d job(s) waiting on %s cookies", resumed, platform),
				notifications.TypeInfo,
				[]notifications.Field{
					{Name: "Platform", Value: platform, Inline: true},
					{Name: "Jobs", Value: fmt.Sprintf("%d", resumed), Inline: true},
				},
				notifications.SendOptions{Event: "auth"},
			)
		}
	}

	// Whenever a platform's saved credentials are (re-)observed, re-evaluate
	// the parked jobs against them. The identity is a Google account on
	// YouTube and a bearer-token pair on Twitch (see the notification sentence
	// below, which was worded for the same reason), and only YouTube can park
	// a job on an account question. For a membership park this is the only
	// thing that can help — such a job parked while auth was perfectly
	// healthy, so it is invisible to OnAuthRecovered above — and it resumes
	// only if the account is genuinely a different one from the one that
	// refused it.
	//
	// Dead-cookie parks are eligible here as well. In the common case
	// OnAuthRecovered already took them (a swap that also restores auth fires
	// both), and resumeCookieParkedJobs is idempotent, so whichever runs
	// second simply finds nothing left. Being permissive costs nothing and
	// covers the swap-while-healthy case for them too.
	s.cookieRefresh.OnCredentialsChanged = func(platform, identity string) {
		reauth(platform)
		resumed := resumeCookieParkedJobs(s.db, s.log, platform, identity)
		if resumed > 0 {
			s.log.Info("account identity observed — resumed COOKIES? jobs", "platform", platform, "count", resumed)
			// States no cause, for the same reason the "Cookie Auto-Refresh
			// Ineffective" notification above states none. This fires on the
			// first authenticated observation of EVERY process, not only on a
			// real account change: an operator who fixed their cookies while
			// Moombox was stopped gets their jobs resumed here, and telling
			// them "a different account was supplied" would be flatly false.
			// A notification is more visible than a log line, so it should
			// assert less than the log line, not more — report what happened
			// (jobs resumed) and leave the cause to the log.
			//
			// "the saved credentials", not "the signed-in account": since Arc
			// 10 this fires for Twitch too, whose credential is a bearer token
			// and a login name rather than a Google account, and the old
			// wording would have been simply wrong there.
			//
			// Same "auth" event as the recovery notification above, for the
			// same reason: an empty Event bypasses every target's allowlist.
			s.notifyMgr.Send("Parked Jobs Re-evaluated",
				fmt.Sprintf("Resumed %d job(s) parked on %s credentials after re-checking the saved credentials", resumed, platform),
				notifications.TypeInfo,
				[]notifications.Field{
					{Name: "Platform", Value: platform, Inline: true},
					{Name: "Jobs", Value: fmt.Sprintf("%d", resumed), Inline: true},
				},
				notifications.SendOptions{Event: "auth"},
			)
		}
	}
}

// channelHealthReporter is the slice of a monitor's surface needed to
// cross-confirm a channel's reachability across sibling monitors.
type channelHealthReporter interface {
	Health() []monitor.ChannelHealth
}

// siblingReachable reports whether any sibling monitor RECENTLY reached the
// channel successfully. When true the channel is not "not responding" — another
// monitor is still seeing it, so its streams aren't being missed — and the
// failing monitor's unhealthy alert is a false positive to suppress. This is
// the guard against YouTube serving RSS 404/5xx during peak hours while the
// independent DECAPI monitor stays healthy. A sibling vouches only on a FRESH
// success (last check succeeded within crossMonitorVouchWindow); one that is
// itself failing, has never checked the channel, or has gone stale does not.
func siblingReachable(siblings []channelHealthReporter, channelID string, now time.Time) bool {
	for _, sib := range siblings {
		if sib == nil {
			continue
		}
		for _, h := range sib.Health() {
			if h.ChannelID != channelID {
				continue
			}
			if h.ConsecutiveErrors == 0 && h.LastCheckedAt != 0 &&
				now.Sub(time.UnixMilli(h.LastCheckedAt)) <= crossMonitorVouchWindow {
				return true
			}
		}
	}
	return false
}

// resumeOnRedetect decides what a live re-detection of an EXISTING job does.
// Only a Finished job with preserved resume data (incomplete_tail) AND
// staging files still on disk resumes; Cancelled is a human decision;
// everything else keeps today's silent drop. stagingExists mirrors the
// human-initiated /resume route's own gate (internal/web/routes/jobs.go,
// worker.HasStagingFiles against config.Paths.EffectiveStagingDir()) —
// staging can vanish between the Finished write and this re-detection
// (manual deletion, a reconfigured staging_dir), and resuming into an
// empty/missing staging dir would silently masquerade a fresh restart as
// an actual resume.
func resumeOnRedetect(existing *database.Job, disposition monitor.JobDisposition, stagingExists bool, lastAutoResume time.Time, now time.Time) bool {
	if existing == nil || disposition != monitor.DispositionBroadcast {
		return false
	}
	if existing.Status != database.StatusFinished || !existing.IncompleteTail {
		return false
	}
	if !stagingExists {
		return false
	}
	return now.Sub(lastAutoResume) >= 5*time.Minute
}

// jobCreationForDisposition maps a monitor.JobDisposition to the created
// job's initial state — spec §10's creator table:
//
//	Broadcast (live/upcoming) → Upcoming, queue_priority 0, enqueue now
//	NewVOD                    → Upcoming, queue_priority 0, enqueue now
//	BacklogVOD                → Queued,   queue_priority 1, NO enqueue (scheduler wake)
//
// Every creator writes queue_priority explicitly — the schema DEFAULT 1
// exists only for pre-v16 legacy rows and must never be relied on.
func jobCreationForDisposition(d monitor.JobDisposition) (status database.JobStatus, priority int, enqueueNow bool) {
	switch d {
	case monitor.DispositionBroadcast:
		return database.StatusUpcoming, 0, true
	case monitor.DispositionNewVOD:
		return database.StatusUpcoming, 0, true
	case monitor.DispositionBacklogVOD:
		return database.StatusQueued, 1, false
	default:
		// Unknown disposition: fail open to immediate admission — a wrongly
		// Queued job would rest until the scheduler noticed it; a wrongly
		// admitted job merely downloads early (today's behavior).
		return database.StatusUpcoming, 0, true
	}
}

// authFailureNotifier is wireMonitorCallbacks' cooldown-guarded notification
// sender. Threaded into runCookieRecovery as a parameter rather than reached
// through runState, both because the cooldown map is a local of the wiring
// function and so a test can observe exactly what would have been sent.
type authFailureNotifier func(platform, title, desc string, ntype notifications.NotificationType)

// withAuthFailureCooldown wraps `send` in a per-platform 30-minute cooldown.
// wireMonitorCallbacks hands the wrapped notifier to every path that can
// report an auth problem, so this is the single place a repeat is dropped.
//
// A package-level function rather than a closure inside wireMonitorCallbacks
// because the cooldown is the thing that decides whether the operator hears
// the ACCURATE message or a vague one that arrived first, and that decision
// needs a test — see TestDeclinedRecoveryDoesNotSpendTheCooldown. The `send`
// parameter is the seam: production passes notifyMgr.Send, a test passes a
// recorder.
//
// HOW OFTEN THE WRAPPED NOTIFIER CAN BE REACHED, read off the producers rather
// than assumed. This comment previously said OnRecoveryNeeded "can re-fire on
// every periodic auth check while cookies stay dead". Tier-1 cannot do that:
//
//   - Today there is exactly one producer, shouldFireRecovery
//     (internal/cookies/refresh.go), and for a platform whose cookies stay
//     dead it fires ONCE PER PROCESS. Its first-conclusive-check arm answers
//     the first check; every later check falls to the witnessed-transition
//     arm, which returns prevAuth — and prevAuth was just set to that same
//     dead answer, so it stays false until the platform genuinely
//     re-authenticates. A platform that flaps dead → alive → dead fires again
//     on each real transition, which is the case this window still coalesces.
//   - Arming cookies.livenessRecoveryArmed adds a SECOND producer that is
//     periodic by design: a signed-out liveness verdict may clear its own
//     dedupe once per livenessRefireWindow (30 minutes), on every cycle a dead
//     session persists.
//
// So the per-poll alarm this was written to bound does not exist yet; what the
// window really covers is the armed case and the flap.
//
// What must NOT reach here is a recovery pass that DECLINED to run. Stamping
// this map for a pass that learned nothing suppresses the accurate verdict
// that follows it inside the window — the failure runCookieRecovery's Unknown
// branch splits on RefreshResult.Ran to prevent.
//
// Guarded by a mutex: recovery attempts run on their own goroutines, so two
// platforms can arrive here concurrently. The lock covers the read-and-stamp
// only and is released before `send`, so one target's dispatch cannot hold up
// the other platform's decision.
func withAuthFailureCooldown(send authFailureNotifier) authFailureNotifier {
	var mu sync.Mutex
	last := make(map[string]time.Time)
	return func(platform, title, desc string, ntype notifications.NotificationType) {
		mu.Lock()
		if time.Since(last[platform]) < 30*time.Minute {
			mu.Unlock()
			return
		}
		last[platform] = time.Now()
		mu.Unlock()
		send(platform, title, desc, ntype)
	}
}

// cookieRefresher is the single outward call runCookieRecovery makes on the
// auto-cookie service.
//
// It is a parameter rather than a reach through runState because the branch
// it feeds is the entire subject of this code, and driving a real
// AutoCookieService to a chosen pair of per-platform verdicts needs a browser
// profile, a browser, and the network. Passing the refresh in lets each
// verdict be exercised directly; the mapping from a real refresh onto those
// verdicts is pinned inside the cookies package, where the profile fixtures
// live.
type cookieRefresher func(context.Context) (cookies.RefreshResult, error)

// cookieReplacementGuidance is the tail shared by the failure notifications.
//
// It leads with the DASHBOARD IMPORT on purpose, and the ordering has changed
// once: it used to lead with the cookie file on the volume, because the only
// other remedy it named was the interactive browser login, which opens a headed
// browser ON THE HOST and so is no use to a container (the image ships none) or
// to anyone reading the dashboard from another machine — exactly where this
// notification is most likely to be read. The
// import (Arc 11) is reachable from anywhere the dashboard is, needs no browser
// and no volume access, and is therefore the first thing to say. The file
// remains named because it still works and is the faster path for anyone with a
// shell. %s is the path to the configured cookie file.
const cookieReplacementGuidance = "Export a fresh Netscape cookies.txt from a browser signed in to the account, " +
	"then paste or upload it in Settings -> Cookies on the dashboard — no shell or volume access needed. " +
	"You can also overwrite the file at %s directly. (Export from a private window and close it: browsing on " +
	"in the source profile rotates the session and invalidates the export.) The interactive browser login in " +
	"Settings is an alternative only on the machine hosting Moombox."

// runCookieRecovery performs one auto-cookie recovery pass on behalf of
// `platform` and reports what it concluded ABOUT THAT PLATFORM.
//
// The per-platform verdict is the entire point. This used to branch on
// RefreshCookies' whole-service bool, which answers "did ANY platform end up
// authenticated" — so a healthy Twitch made a conclusively dead YouTube take
// the success branch. The field log for 2026-08-20 03:40:01 has all three
// lines within a second of each other:
//
//	YouTube auth verification failed after refresh — manual re-login required
//	cookie refresh succeeded                       verified=Twitch
//	auto-cookie recovery succeeded                 platform=youtube
//
// No notification was sent, and the operator found out days later when a
// recording failed.
//
// The three verdicts map onto the three branches that were already here; only
// the question being asked changed. The Unknown branch then splits once more,
// on RefreshResult.Ran, because a pass that DECLINED to run and a pass that
// ran without reaching an answer are the same verdict and not the same event —
// see that branch.
func (s *runState) runCookieRecovery(ctx context.Context, platform string, refresh cookieRefresher, notify authFailureNotifier) {
	result, err := refresh(ctx)

	// The re-check, on EVERY exit below rather than only the successful one.
	//
	// It used to sit inside the RefreshOK arm, on the reasoning that a
	// successful refresh is the one whose result the UI needs. That was half
	// the truth twice over. A pass that ran and produced a conclusively DEAD
	// pair still rewrote cookies.txt — a browser that hands back a
	// new-but-refused credential moves the fingerprint exactly as a working one
	// does — so the Twitch auth mark taken under the OLD pair would stand until
	// the ticker, naming a login problem the file no longer has. And a pass
	// that ran and then ERRORED is the same story: three of the EIGHT
	// refreshAborted() exits in refreshCookiesDetailed happen AFTER the write
	// — the jar reload that failed over a cookies.txt just replaced, and the
	// two rollback exits, which fail over a file the import had already
	// rewritten. That first one is where a re-check is worth most, because
	// refresh's own jar.Reload repairs the stale in-memory jar the abort left
	// behind. Both services share one *CookieJar.
	//
	// Deferred rather than copied into each exit: the gate has to be evaluated
	// independently of the error return, and there are three returns below plus
	// the switch. A defer also puts it after the notifications, so a 30-second
	// re-check cannot delay the line that tells the operator what happened.
	//
	// Gated on Ran, which is an OVER-approximation and deliberately so. Ran is
	// false at all seven refreshDeclined() exits — setup in progress, a refresh
	// already in flight, nothing configured, the service stopped — where
	// nothing was written and there is nothing to re-read. It is true at the
	// FIVE aborts that failed before the write as well as at the three that
	// failed after it: an empty profile import, a browser refresh that errored,
	// a failed MkdirAll, the S9 read abort, and the write itself failing. Each
	// of those five costs one wasted validate pass on a rare error path.
	// Getting the declines wrong would instead print a staleness warning about
	// a file nobody touched, on every ordinary tick; getting the three
	// post-write aborts wrong costs half an hour of a stale mark. Ran buys the
	// second and third at the price of the first.
	defer func() {
		if result.Ran {
			recheckAfterCookieWrite(context.Background(), s.checkNowFn(), s.log, "recovery", "platform", platform)
		}
	}()

	if err != nil {
		// Narrowed IN FRONT of the generic branch below, which must stay
		// generic — it also carries write/reload/restore failures that make
		// the same false claim today, and fixing those is a different task
		// with a different blast radius.
		//
		// This one gets its own message because the generic copy is not
		// merely imprecise for it, it is BACKWARDS: the S9 abort in
		// autocookies.go refused to write to cookies.txt BECAUSE it could
		// not read it — the file may hold a working credential for a
		// platform this pass never touched — and the generic text ("...
		// recordings will fail until the cookies are replaced") tells the
		// operator to overwrite the one file Moombox just went out of its
		// way not to destroy. The code destroys nothing; that message would
		// solicit the destruction. Same species as 76f6d79 ("stop the sweep
		// notification asserting a cause it cannot know"): say only what is
		// actually known — a read failed, nothing was written — and name the
		// remedy that is actually true (fix the permission/mount, then it
		// retries on its own).
		if errors.Is(err, cookies.ErrCookieFileUnreadable) {
			s.log.Error("auto-cookie recovery could not read the existing cookie file — nothing was written",
				"platform", platform, "err", err)
			notify(platform, "Cookie File Unreadable",
				fmt.Sprintf("Moombox could not read the existing cookies.txt at %s while refreshing %s cookies, and deliberately did NOT "+
					"write to it — that file may hold a working credential for another platform, so it was left exactly as it was rather than "+
					"replaced with an incomplete refresh. This is a filesystem or permissions problem, not a credential one: confirm the path is "+
					"readable by the account Moombox runs as (in a container, confirm the volume actually mounted). Do not replace cookies.txt "+
					"over this — Moombox will retry automatically once it can read the file again.",
					s.cookieFilePath(), platform),
				notifications.TypeError)
			return
		}
		s.log.Error("auto-cookie recovery failed", "platform", platform, "err", err)
		// The recovery was fired by a CONCLUSIVE signed-out verdict and the
		// automatic remedy for it failed, so a human has to sign in again. On an
		// install with the browser path ON but no browser and no profile, this is
		// the only line that ever says so: RefreshCookiesDetailed's own
		// verify-failed arm — the other producer of this flag — needs a pass that
		// got as far as checking, and there the pass returns ErrNoBrowserFound
		// before it verifies anything. (With a mounted profile it DOES get that
		// far and raises the flag itself; with the flag off, the disabled branch
		// above raises it.)
		//
		// Deliberately BELOW the ErrCookieFileUnreadable arm above, which
		// returns before reaching this. That abort read nothing, wrote nothing
		// and checked nothing; the credentials may be perfectly good, and
		// telling the operator to sign in again over a permissions or mount
		// problem is the unearned cause that arm's own message exists to avoid.
		//
		// An over-approximation for the rest — a locked cookie DB lands here too
		// — and deliberately so: the flag is process-local and the next
		// successful refresh, setup or import clears it, so a spurious raise
		// costs one badge, while a missing raise costs a container operator any
		// indication at all that the session is dead.
		if s.autoCookieSvc != nil {
			s.autoCookieSvc.FlagManualRelogin(platform)
		}
		// Previously log-only: the operator learned cookies were dead
		// only when a recording actually failed. 30-min per-platform
		// cooldown via notifyAuthFailure.
		notify(platform, "Cookie Auto-Refresh Failed",
			fmt.Sprintf("Automatic cookie refresh for %s failed — recordings will fail until the cookies are replaced. "+
				cookieReplacementGuidance, platform, s.cookieFilePath()),
			notifications.TypeError)
		return
	}

	switch result.Verdict(platform) {
	case cookies.RefreshOK:
		s.log.Info("auto-cookie recovery succeeded", "platform", platform)

	case cookies.RefreshFailed:
		// The case that was silently swallowed whenever the sibling platform
		// verified. It is conclusive — the refresh ran, the credentials were
		// verified, and the answer was no — so unlike the branch below it may
		// name the cause.
		s.log.Warn("auto-cookie recovery ran and this platform is still not authenticated", "platform", platform)
		if !result.HasCredentials(platform) {
			// Reachable, and the trigger is a total credential EXPIRY.
			//
			// The two conditions are sampled at different moments and read
			// different things. shouldFireRecovery's cookiesPresent comes from
			// the jar, which ignores expiry; HasCredentials comes from the
			// post-merge jar, and mergeCookieFiles prunes ON expiry — the
			// disagreement RefreshCookiesDetailed documents where it computes
			// `lost`. So a platform whose every stored row has lapsed fires
			// recovery (the jar still sees rows), merges to nothing, and
			// arrives here with a conclusive failure and no credentials left.
			//
			// The wording therefore has to hold for BOTH shapes — a platform
			// that just lost everything and one that never had anything —
			// because nothing here can tell them apart. It names the two
			// possibilities and asserts neither. Deliberately NOT
			// cookiesLostMessage, which is the same information but asserts
			// the loss outright, and would be simply false on an install that
			// never held credentials for this platform.
			notify(platform, "Cookie Auto-Refresh Failed",
				fmt.Sprintf("Automatic cookie refresh ran, and Moombox now holds no %s cookies at all — either every "+
					"stored credential had expired and was dropped, or none were ever supplied. Recordings that need "+
					"an account will fail until some are. "+cookieReplacementGuidance, platform, s.cookieFilePath()),
				notifications.TypeError)
			return
		}
		notify(platform, "Cookie Auto-Refresh Failed",
			fmt.Sprintf("Automatic cookie refresh ran, and %s is still not authenticated — the stored cookies are dead "+
				"and recordings will fail until they are replaced. "+cookieReplacementGuidance, platform, s.cookieFilePath()),
			notifications.TypeError)

	default: // cookies.RefreshUnknown
		// Two different nothings arrive here, and only one of them is the
		// operator's problem. RefreshResult.Ran draws exactly that line, and
		// this is its first production consumer; services.go's
		// cookieRefreshReport already splits the operator-facing wording on it
		// for the same reason.
		if !result.Ran {
			// The pass DECLINED before doing any work — setup in progress, a
			// refresh already in flight, nothing configured to refresh, or the
			// service already stopped (refreshDeclined() is RefreshResult{}, so
			// Ran is false and both verdicts are the zero value). No browser
			// ran, nothing was checked, and nothing about these cookies changed.
			//
			// The first three are the ones cookies.RefreshDeclinedCauses names
			// for operators; see its doc for why the stopped latch is
			// deliberately absent from that copy but belongs in this list, which
			// is reasoning about control flow rather than wording for a reader.
			//
			// Notifying here is worse than useless, because it is REACHABLE BY
			// RACING OURSELVES and it costs the accurate message. Both
			// platforms losing auth in one pass makes refresh.go fire
			// OnRecoveryNeeded twice; AutoCookieService.RefreshCookiesDetailed
			// single-flights, so the second call is declined immediately, and
			// an "Ineffective" sent for that decline stamps the platform's
			// 30-minute cooldown (withAuthFailureCooldown). When the one real
			// attempt finishes ~2 minutes later and genuinely fails, its
			// actionable "Cookie Auto-Refresh Failed" is inside that window and
			// is never sent — the operator is left with a vague warning about a
			// condition Moombox created for itself.
			//
			// So: a log line, no notification, and — the load-bearing half —
			// no cooldown stamp. TestDeclinedRecoveryDoesNotSpendTheCooldown
			// drives that exact two-platform sequence.
			s.log.Info("auto-cookie recovery declined to run — no verdict for this platform, and nothing reported",
				"platform", platform)
			return
		}
		s.log.Warn("auto-cookie recovery ran and did not establish whether this platform is authenticated", "platform", platform)
		// States no cause about the CREDENTIALS, because with Ran true and no
		// error the session may be perfectly healthy throughout. A notification
		// is more visible than a log line, so an unearned assertion here is
		// worse, not better.
		//
		// Two possibilities are named and neither is asserted, and the list is
		// exhaustive rather than illustrative — traced from verdictOf back
		// through checkPlatformAuth (internal/cookies/autocookies_profile.go):
		// a completed pass reports RefreshUnknown for a platform only when its
		// verify callback returned an error, which splits in two.
		//
		//   - The site could not answer: a 429, a dropped connection, a
		//     response that failed the provenance check.
		//   - The question could not be formed at all: ErrAuthCheckNotAttempted,
		//     raised when no cookie header can be built or no SAPISIDHASH can
		//     be generated. Reachable — HasAnyYouTubeAuthCookie counts
		//     LOGIN_INFO, so a jar holding it with the whole SAPISID family
		//     gone is "configured" and still cannot sign a request.
		//
		// Deliberately NOT offered: "it stopped before verifying". All EIGHT
		// refreshAborted() returns in refreshCookiesDetailed carry a non-nil
		// error — stated as a count rather than a line list, which had drifted
		// by hundreds of lines and by one exit — so an aborted pass takes the
		// err != nil branch above and cannot reach this line. Nor
		// "it declined to run" — the branch above takes every declined pass.
		// Both would be causes this code cannot have.
		//
		// One case is not in the copy because it is not a production cause: an
		// unrecognised platform key resolves to Unknown through Verdict's
		// default. That is defence in depth against a wiring mistake or a
		// future third platform (TestRecoveryUnrecognisedPlatformDoesNotAssertFailure),
		// and the wording holds for it because it asserts nothing.
		notify(platform, "Cookie Auto-Refresh Ineffective",
			fmt.Sprintf("Automatic cookie refresh ran and did not restore %s authentication, but could not establish why — the check either could not reach the service or could not be made at all, so nothing has been concluded about the cookies (the log at debug level says which). If they have in fact expired, paste or upload a fresh Netscape export in Settings -> Cookies on the dashboard, or replace %s directly; the interactive browser login in Settings is an alternative only on the machine hosting Moombox.", platform, s.cookieFilePath()),
			notifications.TypeWarning)
	}
}

// handleRecoveryNeeded is the whole body of the OnRecoveryNeeded callback,
// with the two things a test cannot supply passed in: the config answer
// (autoEnabled) and the auto-cookie service's refresh entry point.
//
// autoEnabled splits two genuinely different situations, not one situation
// and a silence:
//
//   - Automatic refresh is ON: there is something to attempt, so attempt it
//     on its own goroutine (RefreshCookiesDetailed drives a headless browser
//     and is bounded here by a 2-minute timeout; the refresh service's loop
//     must not block on it) and report whatever it concluded.
//   - Automatic refresh is OFF: there is nothing to attempt. This used to
//     Debug-log and send nothing, on the implicit reasoning that no
//     configured recovery means nothing worth saying. That is inverted. A
//     user with auto-recovery on has an automated attempt that may quietly
//     fix the problem before they ever see it; a user with it off has none,
//     so this notification is not redundant for them — it is the only thing
//     that will tell them their credentials need replacing by hand. The
//     whole point of this work is that Moombox could hold dead cookies and
//     never say so, and this gate was the last place it still stayed silent.
//
// The disabled path runs SYNCHRONOUSLY and deliberately does not call
// `refresh`: launching a headless browser the operator explicitly turned off,
// and paying its timeout, is precisely what "disabled" forbids.
// notifyMgr.Send hands every target off to its own bounded goroutine
// (internal/notifications.Manager.Send), so sending inline cannot stall the
// refresh loop — the same reason OnAuthRecovered and OnCredentialsChanged
// below send inline.
func (s *runState) handleRecoveryNeeded(platform string, autoEnabled bool, refresh cookieRefresher, notify authFailureNotifier) {
	if !autoEnabled {
		// Warn, not Debug. Debug was right for "we did nothing"; it is not
		// right for "we are telling the operator their recordings will fail".
		s.log.Warn("auth lost and automatic cookie refresh is disabled — manual re-authentication required",
			"platform", platform)
		// Claims nothing about a refresh, because none ran: no attempt, no
		// finding, no failure. What IS known is exactly two things — this
		// platform answered a conclusive "not authenticated", and the config
		// flag this branch just read is off. Note it does NOT say cookies are
		// present:
		// the witnessed-transition arm of shouldFireRecovery never consults
		// cookiesPresent, so the file may have been deleted outright.
		//
		// WHY "conclusive" holds, and what to re-check before it stops. Today
		// there is exactly one producer: shouldFireRecovery, which fires only on
		// checkErr == nil && !nowAuth. cookies.livenessRecoveryArmed is false, so
		// the liveness probes cannot reach here at all. ARMING IT ADDS A SECOND
		// PRODUCER — and this copy survives it, because ObserveLiveness is
		// documented to take only conclusive verdicts (SessionAuthUnknown is
		// dropped upstream in routeLivenessVerdict), so a liveness LoggedOut is
		// conclusive in the same sense. Nothing here fails loudly if that stops
		// being true, so any THIRD producer has to be checked against this
		// sentence by hand: a caller that could pass an inconclusive result would
		// make this notification assert a dead session it does not have.
		//
		// "on its own" scopes the claim to AUTOMATIC attempts, which is all
		// this branch knows about. POST /api/cookies/auto-refresh
		// (internal/web/routes/cookies.go) is not gated on AutoEnabled, so an
		// operator can still trigger a refresh by hand — a flat "nothing will
		// attempt to restore it" would be a shade stronger than the code.
		//
		// Automatic refresh is off, so nothing here will attempt a repair, and
		// shouldFireRecovery has already concluded this platform is not
		// authenticated. That is exactly "a human has to sign in again", and on
		// this install — the container's documented shape — nothing else ever
		// says so: runCookieRecovery is not called on this path at all, and
		// RefreshCookiesDetailed's verify-failed arm needs a pass that got as far
		// as checking.
		if s.autoCookieSvc != nil {
			s.autoCookieSvc.FlagManualRelogin(platform)
		}
		// Guidance leads with the DASHBOARD IMPORT — see
		// cookieReplacementGuidance, whose ordering changed with Arc 11. This is
		// the notification most likely to be read somewhere the host's own
		// browser cannot be reached, and the import is the remedy that works
		// from there.
		notify(platform, "Cookie Re-Authentication Required",
			fmt.Sprintf("Moombox is not authenticated to %s, and automatic cookie refresh is turned off — nothing will "+
				"attempt to restore it on its own, so recordings that need an account will fail until the cookies are "+
				"replaced by hand. "+cookieReplacementGuidance, platform, s.cookieFilePath()),
			notifications.TypeError)
		return
	}
	// The pass itself, and the per-platform branch it takes, live in
	// runCookieRecovery — see cookieReplacementGuidance there for why the
	// notification copy leads with the dashboard import rather than the host's
	// own browser.
	s.log.Warn("Auth lost, attempting auto-cookie recovery", "platform", platform)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("auto-cookie recovery panic", "panic", r)
			}
		}()
		refreshCtx, refreshCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer refreshCancel()
		s.runCookieRecovery(refreshCtx, platform, refresh, notify)
	}()
}

// routeLivenessVerdict hands one YouTube login verdict to the cookie health
// signal, and drops the one that is not a verdict.
//
// Split out of the FetchMembership closure it is called from because the
// mapping is the whole of the decision and the closure around it needs a
// monitor, the service graph and the network to exist. `observe` is
// (*cookies.RefreshService).ObserveLiveness in production.
//
// SessionAuthUnknown is absent from the switch deliberately, not by omission.
// A consent wall, a rate limit, an off-host redirect and a jar that was never
// configured all arrive as Unknown; none of them is evidence about the
// session, and reporting any of them as a dead one would send an operator off
// to re-export credentials that were never wrong — a remedy that, in a
// container, they may not even be able to reach.
func routeLivenessVerdict(observe func(platform string, loggedIn bool), verdict youtube.SessionAuthState) {
	switch verdict {
	case youtube.SessionAuthLoggedIn:
		observe("youtube", true)
	case youtube.SessionAuthLoggedOut:
		observe("youtube", false)
	}
}

// wireMonitorCallbacks installs every post-service-startup callback that
// connects the construction graph: cookie recovery / auth-recovered sweep,
// monitor ProbeVideo + OnVideoFound / OnStreamFound job-creation closures,
// monitor OnSchedule (broadcast-all-timers + TUI atomic dispatch), initial
// per-job log tracking, database OnJobUpdate / OnJobsChange subscribers
// (persisted on runState so shutdown can unsubscribe), the log-forwarder
// goroutine, and the connectivity OnStateChange wiring.
//
// Called once between wireRoutes() and the "start services" phase in run().
func (s *runState) wireMonitorCallbacks() {
	notifyAuthFailure := withAuthFailureCooldown(func(platform, title, desc string, ntype notifications.NotificationType) {
		s.notifyMgr.Send(title, desc, ntype,
			[]notifications.Field{{Name: "Platform", Value: platform, Inline: true}},
			notifications.SendOptions{Event: "auth"},
		)
	})

	// Cooldown for auto-resume on broadcast re-detection: a restarted
	// broadcast can be re-detected on every monitor cycle (as often as
	// every 15s per the field case that motivated this) while a previous
	// resume is still spinning up — coalesce to at most one auto-resume
	// attempt per window per job. Guarded by a mutex — createYouTubeJob
	// runs on the feed/DECAPI monitor goroutines. Same pattern as
	// lastAuthFailNotify above.
	var resumeMu sync.Mutex
	lastAutoResume := make(map[string]time.Time)

	s.cookieRefresh.OnRecoveryNeeded = func(platform string) {
		var autoEnabled bool
		s.configStore.Read(func(c *config.MoomboxConfig) {
			autoEnabled = c.Cookies.AutoEnabled
		})
		// Both branches — attempt a recovery, or report that no attempt will
		// be made — live in handleRecoveryNeeded so each can be driven
		// directly by a test. The method value is taken unconditionally and
		// is not called on the disabled path.
		//
		// THIS IMPORT DELIBERATELY BYPASSES cookies.automaticImportGuard, and
		// it is the only automatic import that does. The guard's rule — never
		// import over an existing cookies.txt — protects credentials that MIGHT
		// BE WORKING. This path runs only when they are known not to be:
		// shouldFireRecovery fires on a conclusive not-authenticated for this
		// platform (see the "WHY conclusive holds" note in
		// handleRecoveryNeeded). Applying the rule here would refuse the one
		// automatic import most likely to fix the problem, because a file
		// exists that has just been proven dead.
		//
		// Do not "fix" this by adding the guard. A surviving second platform is
		// protected by RefreshCookiesDetailed's own pre/post verification and
		// platformsToRestore, not by the guard. The full derivation is at
		// automaticImportGuard.
		s.handleRecoveryNeeded(platform, autoEnabled, s.autoCookieSvc.RefreshCookiesDetailed, notifyAuthFailure)
	}

	// Both credential-repair edges, wired together because they mean the same
	// thing to a live chat session and different things to everything else.
	// The broadcast is injected rather than read off s.dlWorker inside, so a
	// test can count it; the method value is safe on a nil worker
	// (ReauthenticateTwitchChats is nil-receiver-guarded — Task 5).
	s.wireCredentialRepairCallbacks(s.dlWorker.ReauthenticateTwitchChats)

	// ProbeVideo callback for monitors (metadata check before job creation).
	// Uses the caller-supplied ctx so monitor shutdown cancels in-flight
	// probes (per audit reports/cross-cutting.md C4).
	probeVideoFunc := func(ctx context.Context, videoID string) (*monitor.VideoProbeResult, error) {
		meta, err := s.ytService.ProbeVideoStatus(ctx, videoID)
		if err != nil {
			return nil, err
		}
		return &monitor.VideoProbeResult{
			StreamStatus:       string(meta.StreamStatus),
			Title:              meta.Title,
			ChannelName:        meta.ChannelName,
			PublishedAt:        meta.PublishedAt,
			PublishedPrecision: meta.PublishedPrecision,
			PlayabilityError:   string(meta.PlayabilityError),
		}, nil
	}
	s.feedMon.ProbeVideo = probeVideoFunc
	s.decapiMon.ProbeVideo = probeVideoFunc

	// Date-completing fetch for the two-phase probe (§9): the ANDROID_VR/TV
	// status probes carry no microformat, so vod-family results arrive
	// dateless; both monitors call this (one anonymous WEB player fetch)
	// when a date is actually needed for a window decision.
	probeDateFunc := func(ctx context.Context, videoID string) (string, string, error) {
		return s.ytService.ProbeVideoDate(ctx, videoID)
	}
	s.feedMon.ProbeDate = probeDateFunc
	s.decapiMon.ProbeDate = probeDateFunc

	// Authenticated probe for members-only videos: an anonymous probe can't see
	// members-only content, gets no formats, and misclassifies it as "upcoming"
	// (which bypasses include_non_live_content). The TV_DOWNGRADED+cookies probe
	// classifies it correctly (vod/live/upcoming). Only the feed monitor's
	// membership path uses it; RSS/DECAPI stay on the anonymous probe.
	s.feedMon.ProbeVideoAuth = func(ctx context.Context, videoID string) (*monitor.VideoProbeResult, error) {
		meta, err := s.ytService.ProbeVideoStatusAuthenticated(ctx, videoID)
		if err != nil {
			return nil, err
		}
		return &monitor.VideoProbeResult{
			StreamStatus:       string(meta.StreamStatus),
			Title:              meta.Title,
			ChannelName:        meta.ChannelName,
			PublishedAt:        meta.PublishedAt,
			PublishedPrecision: meta.PublishedPrecision,
			PlayabilityError:   string(meta.PlayabilityError),
		}, nil
	}

	// Membership discovery: authenticated /membership tab scan for members-only
	// videos the RSS feed never lists. Wired on the feed monitor only. The
	// closure adapts youtube.MembershipVideo -> monitor.MembershipVideo (keeping
	// the monitor package decoupled from youtube, like probeVideoFunc does for
	// VideoInfo). MembershipEnabled re-reads the config flag AND cookie state
	// live each cycle, so toggling the setting or acquiring cookies takes effect
	// on the next cycle with no restart.
	s.feedMon.FetchMembership = func(ctx context.Context, channelID string) ([]monitor.MembershipVideo, error) {
		vids, verdict, err := s.ytService.FetchMembershipVideos(ctx, channelID)
		// The login verdict is a credential-health signal, not a discovery
		// result, so MembershipFetchFunc keeps its two-value shape and the
		// adapter absorbs the third here.
		//
		// Routed BEFORE the error return on purpose. Whether the tab scan
		// produced videos is a different question from whether YouTube
		// recognised the session, and this placement does not depend on the
		// two answers being packaged together. It costs nothing today —
		// FetchMembershipVideos returns SessionAuthUnknown on every failure
		// path — and it means a conclusive verdict reported alongside a failed
		// fetch would still reach the health signal rather than being dropped
		// by an early return nobody re-read.
		//
		// This closure runs once per configured channel per feed cycle, so a
		// dead session arrives as N identical verdicts. ObserveLiveness owns
		// the de-duplication — see livenessRefireWindow in internal/cookies.
		routeLivenessVerdict(s.cookieRefresh.ObserveLiveness, verdict)
		if err != nil {
			return nil, err
		}
		out := make([]monitor.MembershipVideo, len(vids))
		for i, v := range vids {
			out[i] = monitor.MembershipVideo{VideoID: v.VideoID, Title: v.Title, Age: v.Age}
		}
		return out, nil
	}
	s.feedMon.MembershipEnabled = func() bool {
		enabled := true
		s.configStore.Read(func(c *config.MoomboxConfig) {
			enabled = c.Monitors.MembershipDiscoveryEnabled()
		})
		// HasAnyAuthCookie, not HasAuthCookies: this gate asks "should we
		// even look", and the complete-set predicate answers no for exactly
		// the half-cleared session the probe exists to detect — SAPISID
		// present with LOGIN_INFO gone reads as never-configured. Left
		// strict, FetchMembershipVideos is never called in that state and
		// the verdict it now returns is unreachable.
		//
		// This value reaches FOUR consumers via membershipActive()
		// (internal/monitor/feed.go:645). Widening was checked against all
		// four, not just the first:
		//
		//	feed.go:513     the discovery arm — upserts only videos it finds
		//	walk.go:90      skips membership-source rows when inactive
		//	walk.go:247     same-cycle escalation to the authed probe
		//	archive.go:131  skips membership-source rows when inactive
		//
		// None writes durable state for a dead session: a refusal is
		// OutcomeDenied, applyProbe runs only on OutcomeProbed, and archive's
		// denied arm just counts a retry. No job, no classification, no
		// completion flag — unlike the backfill sweep's own gate
		// (services.go), which persists backfilled_with_membership and must
		// stay strict.
		//
		// There IS a real cost, in two parts, and the second is the larger.
		//
		// Per membership ROW: with a half-cleared session those rows are no
		// longer parked at walk.go:90 / archive.go:131, so each burns one
		// refused authenticated probe per cycle, and walk.go:247's same-cycle
		// escalation fires too.
		//
		// Per membership CHANNEL: the discovery arm at feed.go:513 now also runs,
		// so every feed cycle pays a full authenticated /channel/<id>/membership
		// page fetch and parse — the ~1MB payload FetchMembershipVideos
		// describes, capped by utils.MaxFetchBodySize at 50MB
		// (internal/utils/http.go: 50 << 20) — where the strict gate skipped it
		// outright. Indefinitely, for as long as the session stays
		// half-cleared.
		//
		// Both are bounded and neither is a regression: this is the same work a
		// HEALTHY install already does every cycle, and it is exactly the fetch
		// the liveness verdict is read off — refusing to pay it is refusing to
		// observe the session at all. It stops the moment the cookies are fixed
		// or the operator clears them.
		return enabled && s.ytService.HasAnyAuthCookie()
	}

	// createYouTubeJob creates a YouTube job per the disposition's creation
	// semantics (spec §10's creator table, via jobCreationForDisposition).
	// Stream-status classification is handled by the monitors via
	// ProcessYouTubeVideo.
	createYouTubeJob := func(videoID, title, videoURL string, ch *config.ChannelConfig, source string, d monitor.JobDisposition) {
		s.log.Info("Video found", slog.String("source", source), slog.String("videoID", videoID),
			slog.String("title", title), slog.String("disposition", d.String()))

		includeNonLive := ch.IncludeNonLiveContent
		outputDir := resolveOutputDir(ch, s.configStore)
		thumbnailURL := youtubeThumbnailURL(videoID)

		status, priority, enqueueNow := jobCreationForDisposition(d)
		// The feed affiliation the scheduler groups by. Copied so the row
		// never aliases live config memory; an empty ID (defensive — feed
		// channels always carry one) stays nil and therefore stores NULL.
		var channelID *string
		if ch.ID != "" {
			id := ch.ID
			channelID = &id
		}

		now := time.Now().UTC().Format(time.RFC3339)
		job := &database.Job{
			ID:                videoID,
			VideoID:           videoID,
			URL:               videoURL,
			Title:             title,
			ChannelName:       ch.Name,
			Platform:          "youtube",
			Status:            status,
			ThumbnailURL:      thumbnailURL,
			OutputDirectory:   outputDir,
			AllowNonStream:    includeNonLive,
			QualityPreference: ch.QualityPreference,
			ChannelID:         channelID,
			QueuePriority:     priority,
			CreatedAt:         now,
			UpdatedAt:         now,
		}

		added, err := s.db.AddJob(job)
		if err != nil {
			s.log.Error("Failed to add YouTube job", slog.String("error", err.Error()))
			return
		}
		if !added {
			// Duplicate job — but a broadcast that dropped and came back
			// under the SAME video ID (interrupted stream, re-detected
			// live) may be sitting Finished with preserved staging
			// (incomplete_tail). Re-arm exactly that case; everything
			// else (still active, Cancelled, no preserved tail, VOD
			// re-detection, or within the cooldown of a prior auto-resume)
			// stays a silent drop, unchanged from today.
			existing, _ := s.db.GetJob(videoID)

			// The cheap in-memory half of resumeOnRedetect's guard,
			// duplicated only to decide whether the staging-existence
			// disk check below is worth paying for. This branch runs on
			// EVERY re-detection of an already-known job — including a
			// still-live job re-polled every monitor cycle — so gating
			// the stat avoids hitting disk on that hot path. This can't
			// loosen the real decision: resumeOnRedetect independently
			// re-checks every one of these conditions itself.
			eligible := existing != nil && d == monitor.DispositionBroadcast &&
				existing.Status == database.StatusFinished && existing.IncompleteTail

			var stagingExists bool
			if eligible {
				// Same gate + same stagingBase resolution as the human
				// /resume route (internal/web/routes/jobs.go) — a job
				// whose preserved staging vanished (manual deletion, a
				// reconfigured staging_dir) must not masquerade a fresh
				// empty-staging restart as a resume.
				var stagingBase string
				s.configStore.Read(func(c *config.MoomboxConfig) {
					stagingBase = c.Paths.EffectiveStagingDir()
				})
				stagingExists = worker.HasStagingFiles(stagingBase, videoID)
			}

			resumeMu.Lock()
			shouldResume := resumeOnRedetect(existing, d, stagingExists, lastAutoResume[videoID], time.Now())
			if shouldResume {
				lastAutoResume[videoID] = time.Now()
			}
			resumeMu.Unlock()
			if !shouldResume {
				if eligible && !stagingExists {
					s.log.Debug("broadcast re-detected live but preserved staging is gone — skipping auto-resume, use Reinitialize",
						slog.String("videoID", videoID))
				}
				return
			}
			if title != "" && title != existing.Title {
				s.db.UpdateJobFields(videoID, map[string]any{"title": title})
			}
			s.dlWorker.ResumeJob(videoID)
			// No notification here: orchestrator.go sends "YouTube Download
			// Starting" unconditionally on every ExecuteWithChat entry —
			// including this resume — so a second send here would just be
			// a duplicate back-to-back message for the same event. This
			// INFO log is the auto-resume-specific record for the operator.
			s.log.Info("broadcast re-detected live — auto-resuming preserved job",
				slog.String("source", source), slog.String("videoID", videoID), slog.String("title", title))
			return
		}
		// History fires for EVERY disposition — it is what makes
		// HasProcessed mean "a job was created" (spec §10/§15).
		s.db.AddToHistory(videoID)
		if enqueueNow {
			s.dlWorker.EnqueueJob(videoID)
		} else {
			// Backlog VOD: rests in Queued until the archive-slots
			// scheduler admits it (M per channel). Wake so a running
			// scheduler sweeps now instead of on its next heartbeat.
			s.dlWorker.Scheduler().Wake()
		}
		// AddJob's OnJobAdded handler (wired below) handles the WS
		// broadcast for the new job; no explicit BroadcastJobsUpdate
		// needed here. DECISIONS #21 consumer migration.
		if s.notifyMgr.HasTargets() {
			s.notifyMgr.Send("Stream Found",
				fmt.Sprintf("Found matching stream: %s", title),
				notifications.TypeInfo,
				[]notifications.Field{
					{Name: "Channel", Value: ch.Name, Inline: true},
					{Name: "Video ID", Value: videoID, Inline: true},
				},
				notifications.SendOptions{
					Event:     "found",
					URL:       videoURL,
					Thumbnail: youtubeThumbnailURL(videoID),
				})
		}
	}

	// Monitor -> Worker: create jobs for found videos. Panic recovery
	// prevents a single bad callback from killing the monitor goroutine.
	// The JobDisposition drives the creation semantics — see
	// jobCreationForDisposition for spec §10's creator table.
	s.feedMon.OnVideoFound = func(videoID, title, url string, ch *config.ChannelConfig, d monitor.JobDisposition) {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("Panic in OnVideoFound (feed)", slog.Any("panic", r))
			}
		}()
		createYouTubeJob(videoID, title, url, ch, "feed", d)
	}
	s.decapiMon.OnVideoFound = func(videoID, title, url string, ch *config.ChannelConfig, d monitor.JobDisposition) {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("Panic in OnVideoFound (decapi)", slog.Any("panic", r))
			}
		}()
		createYouTubeJob(videoID, title, url, ch, "decapi", d)
	}
	s.twitchMon.OnStreamFound = func(info *twitch.TwitchStreamInfo, ch *config.ChannelConfig) {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("Panic in OnStreamFound (twitch)", slog.Any("panic", r))
			}
		}()
		jobID := twitch.BuildJobID(info.StreamID, false)
		s.log.Info("Stream found by Twitch monitor", slog.String("jobID", jobID), slog.String("title", info.Title))

		outputDir := resolveOutputDir(ch, s.configStore)

		now := time.Now().UTC().Format(time.RFC3339)
		title := info.ChannelDisplayName + " — " + info.Title
		if info.Title == "" {
			title = info.ChannelDisplayName + " — " + time.Now().UTC().Format(time.RFC3339)
		}

		job := &database.Job{
			ID:                jobID,
			VideoID:           info.StreamID,
			URL:               "https://twitch.tv/" + info.ChannelLogin,
			Title:             title,
			ChannelName:       info.ChannelDisplayName,
			Platform:          "twitch",
			Status:            database.StatusLive, // Twitch: immediately Live (confirmed by GQL)
			ThumbnailURL:      info.ThumbnailURL,
			ChannelAvatarURL:  info.ProfileImageURL,
			TwitchCategory:    info.GameCategory,
			TwitchQuality:     ch.QualityPreference,
			QualityPreference: ch.QualityPreference,
			StreamStartTime:   info.StartedAt,
			OutputDirectory:   outputDir,
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		added, err := s.db.AddJob(job)
		if err != nil {
			s.log.Error("Failed to add Twitch job", slog.String("error", err.Error()))
			return
		}
		if !added {
			return // Duplicate job
		}
		// Stash the monitor's fresh streamInfo for processTwitchLive to consume,
		// so it doesn't immediately re-query Twitch GQL (which has been observed
		// to return Stream=nil for ~1s after StreamMetadata reports a stream as
		// live, manifesting as a false "twitch channel is offline" error).
		s.dlWorker.StashTwitchStreamInfo(jobID, info)
		s.db.AddToHistory(jobID)
		s.dlWorker.EnqueueJob(jobID)
		// Same as the YouTube path — AddJob's OnJobAdded handler
		// broadcasts the new job; no explicit BroadcastJobsUpdate
		// needed. DECISIONS #21 consumer migration.
		if s.notifyMgr.HasTargets() {
			twitchFields := []notifications.Field{
				{Name: "Channel", Value: info.ChannelDisplayName, Inline: true},
				{Name: "Stream ID", Value: info.StreamID, Inline: true},
			}
			if info.GameCategory != "" {
				twitchFields = append(twitchFields, notifications.Field{
					Name: "Category", Value: info.GameCategory, Inline: true,
				})
			}
			s.notifyMgr.Send("Twitch Stream Found",
				fmt.Sprintf("Live: %s", title),
				notifications.TypeInfo,
				twitchFields,
				notifications.SendOptions{
					Event:     "found",
					URL:       "https://twitch.tv/" + info.ChannelLogin,
					Thumbnail: info.ThumbnailURL,
				})
		}
	}

	s.twitchMon.OnStreamRecover = func(info *twitch.TwitchStreamInfo, ch *config.ChannelConfig, jobID string) {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("Panic in OnStreamRecover (twitch)", slog.Any("panic", r))
			}
		}()

		// Stash the fresh streamInfo so processTwitchLive consumes it instead
		// of re-querying GQL — same flap-prevention as the OnStreamFound path.
		s.dlWorker.StashTwitchStreamInfo(jobID, info)

		// AutoReinitializeJob increments auto_retry_count, clears state, and
		// re-enqueues. The cap (worker.MaxTwitchAutoRetries) is enforced by
		// the monitor's predicate before we even get here.
		s.dlWorker.AutoReinitializeJob(jobID)

		s.log.Info("auto-recovered twitch job",
			slog.String("jobID", jobID),
			slog.String("channel", info.ChannelDisplayName),
			slog.String("streamID", info.StreamID))
	}

	// Backfill worker -> UIs: progress surfacing (spec §11), modeled on the
	// disk_status pipeline in main.go — generic hub.Broadcast for web
	// clients, non-blocking channel push for the TUI — plus the snapshot
	// write InitialState and the TUI seed read (disk_status keeps its
	// snapshot in routes.SharedDiskStatus; backfill's lives on runState).
	// The worker never imports web/tui; this closure is the seam.
	s.backfillWorker.OnProgress = func(chID, tab string, pages int, state string) {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("Panic in backfill OnProgress", slog.Any("panic", r))
			}
		}()
		// Snapshot first, broadcast second, so a client connecting between
		// the two sees this state in InitialState rather than missing it.
		// Active states are stored; "done" and "idle" clear the entry (see
		// the backfillProgress field doc).
		s.backfillMu.Lock()
		switch state {
		case "scanning", "error":
			s.backfillProgress[chID] = backfillProgressState{Tab: tab, Pages: pages, State: state}
		default: // "done", "idle"
			delete(s.backfillProgress, chID)
		}
		s.backfillMu.Unlock()

		// Broadcast to web clients — the exact Task 5 payload shape.
		s.wsHub.Broadcast("backfill_status", map[string]any{
			"channel": chID,
			"tab":     tab,
			"pages":   pages,
			"state":   state,
		})

		// Push to TUI (drop-on-full like tuiDiskStatusCh — a dropped page
		// tick is superseded by the next one within a second).
		select {
		case s.tuiBackfillCh <- tui.BackfillStatusMsg{Channel: chID, Tab: tab, Pages: pages, State: state}:
		default:
		}
	}

	// Monitor -> WebSocket: broadcast timer updates. TypeScript broadcasts
	// ALL three monitor times on each schedule event so we do the same —
	// read all three monitors' next check times.
	broadcastAllTimers := func() {
		s.wsHub.BroadcastCheckTimers(map[string]any{
			"nextFeedCheck":   s.feedMon.GetNextCheckAt(),
			"nextDecapiCheck": s.decapiMon.GetNextCheckAt(),
			"nextTwitchCheck": s.twitchMon.GetNextCheckAt(),
		})
	}

	// OnSchedule subscriber slots live on runState (atomic.Pointer fields).
	// Set the dispatchers once before monitor.Start(); the TUI wiring later
	// Store()s concrete funcs into the atomic slots. Reassigning the
	// monitor's plain func field while its goroutine is running would race
	// with scheduleNext() reading it; atomic.Pointer keeps the read-side
	// lock-free and the write-side safe.
	s.feedMon.OnSchedule = func(next int64) {
		broadcastAllTimers()
		if fn := s.feedTUISchedule.Load(); fn != nil {
			(*fn)(next)
		}
	}
	s.decapiMon.OnSchedule = func(next int64) {
		broadcastAllTimers()
		if fn := s.decapiTUISchedule.Load(); fn != nil {
			(*fn)(next)
		}
	}
	s.twitchMon.OnSchedule = func(next int64) {
		broadcastAllTimers()
		if fn := s.twitchTUISchedule.Load(); fn != nil {
			(*fn)(next)
		}
	}

	// Channel-health notifications: a channel that fails every check for a
	// sustained streak (renamed/banned Twitch login, dead YouTube channel,
	// 404 RSS) previously rotted at Debug level until a stream was missed.
	// One notification per streak, per monitor; the /api/status
	// channelHealth surface shows the live state. platform label is set per
	// monitor so the operator knows which source flagged it.
	unhealthyNotify := func(platform string, siblings ...channelHealthReporter) func(channelID string, consecutive int, lastErr string) {
		return func(channelID string, consecutive int, lastErr string) {
			// Cross-monitor confirmation: a channel is only "not responding" if
			// EVERY monitor covering it has lost it. YouTube serves RSS 404/5xx
			// during peak hours while the independent DECAPI monitor keeps
			// working, so a lone feed-monitor failure is a false positive — its
			// streams are still being seen. Suppress unless no sibling vouches.
			if siblingReachable(siblings, channelID, time.Now()) {
				s.log.Info("channel unhealthy on one monitor but still reachable via another — suppressing alert",
					"platform", platform, "channel", channelID, "consecutive", consecutive, "err", lastErr)
				return
			}
			s.log.Warn("channel failing monitor checks — verify it still exists",
				"platform", platform, "channel", channelID, "consecutive", consecutive, "err", lastErr)
			s.notifyMgr.Send("Channel Not Responding",
				fmt.Sprintf("A %s channel has failed %d consecutive monitor checks — it may be renamed, banned, or misconfigured, and its streams are being missed", platform, consecutive),
				notifications.TypeWarning,
				[]notifications.Field{
					{Name: "Channel", Value: channelID, Inline: true},
					{Name: "Platform", Value: platform, Inline: true},
					{Name: "Last Error", Value: lastErr},
				},
				notifications.SendOptions{Event: "channel_unhealthy"},
			)
		}
	}
	// YouTube channels are covered by both the RSS feed and DECAPI monitors, so
	// each cross-confirms against the other before alerting. Twitch has a single
	// (reliable GQL) monitor with no sibling to confirm against.
	s.feedMon.SetOnChannelUnhealthy(unhealthyNotify("youtube", s.decapiMon))
	s.decapiMon.SetOnChannelUnhealthy(unhealthyNotify("youtube", s.feedMon))
	s.twitchMon.SetOnChannelUnhealthy(unhealthyNotify("twitch"))

	// Initialize per-job log tracking with existing jobs (matches TS knownJobIds)
	if existingJobs, err := s.db.GetAllJobs(); err == nil {
		for _, j := range existingJobs {
			s.db.TrackJobForLogs(j.ID)
		}
	}

	// Database -> WebSocket: broadcast job updates. Uses the
	// fine-grained OnJobChange API. silentColumns (resume_position,
	// chat_offset) bypass OnJobChange entirely at the writer side,
	// so player-state scrubs don't reach this subscriber.
	//
	// No per-job throttle here: progress writes are already capped to
	// ~60Hz/job upstream by ProgressTracker.maybeUpdate (16ms gate in
	// internal/worker/progress.go), and every other UpdateJobFields
	// caller is event-driven (state transitions, not loops).
	s.unsubWSJobUpdate = s.db.OnJobChange(func(ev *database.JobChange) {
		job := ev.Job
		// Skip broadcasting updates for archived (old finished) jobs — same
		// classification as the list filter, via the shared jobArchivedAt
		// predicate so the two can never disagree about which jobs are
		// archived.
		if job.Status == database.StatusFinished && job.UpdatedAt != "" {
			var hideAgeDays float64
			s.configStore.Read(func(c *config.MoomboxConfig) {
				hideAgeDays = c.Monitors.HideFinishedAgeDays.Value
			})
			if hideAgeDays >= 0 {
				cutoff := time.Now().Add(-time.Duration(hideAgeDays*24) * time.Hour)
				if jobArchivedAt(job, cutoff) {
					return
				}
			}
		}
		s.wsHub.BroadcastJobUpdate(job)
	})

	// OnJobAdded subscriber: AddJob no longer fires OnJobsChange (the
	// writer-side dispatch was dropped as part of DECISIONS #21
	// consumer migration). The new-job broadcast goes through
	// BroadcastJobUpdate — frontend's "job_update" handler already
	// has a "job not in array yet — add it and re-render" branch
	// (web/public/app.js around line 1021), so a singular update
	// for an unknown ID is the right wire shape. We also do the
	// per-job log tracking here that the OnJobsChange handler used
	// to do for ALL jobs on every fan-out.
	s.unsubWSJobAdded = s.db.OnJobAdded(func(ev *database.JobAdded) {
		job := ev.Job
		s.db.TrackJobForLogs(job.ID)
		s.wsHub.BroadcastJobUpdate(job)
	})

	// OnTrimsChanged subscriber: AddTrim/DeleteTrim no longer fire
	// OnJobsChange (writer-side dispatch dropped per DECISIONS #21).
	// Re-fetch the affected job (so its Trims field reflects the
	// current SQLite state) and broadcast it through BroadcastJobUpdate
	// — frontend's existing job_update handler replaces the in-memory
	// job and re-renders, picking up the new trim list naturally.
	s.unsubWSTrimsChanged = s.db.OnTrimsChanged(func(ev *database.TrimsChanged) {
		job, err := s.db.GetJob(ev.JobID)
		if err != nil || job == nil {
			return
		}
		s.wsHub.BroadcastJobUpdate(job)
	})

	// OnJobDeleted subscriber: send a targeted job_deleted WS event so the
	// frontend drops the row immediately. This replaces the prior full-list
	// rebroadcast (jobs_update) which raced against the preceding
	// status=Cancelled job_update and left stale rows visible in the UI.
	// Per-job log buffers are pruned via the active-IDs set derived from the
	// post-delete snapshot (the deleted ID drops out naturally).
	s.unsubWSJobDeleted = s.db.OnJobDeleted(func(ev *database.JobDeleted) {
		jobs := getAllJobsSafe(s.db)
		activeIDs := make(map[string]struct{}, len(jobs))
		for _, j := range jobs {
			activeIDs[j.ID] = struct{}{}
		}
		// Only the DATABASE per-job log pipeline is live (RouteLogToJobs);
		// the logger's parallel buffers are unwired and permanently empty.
		s.db.PruneJobLogs(activeIDs)
		s.wsHub.BroadcastJobDeleted(ev.JobID)
	})

	s.unsubWSJobsChange = s.db.OnJobsChange(func(jobs []*database.Job) {
		// Keep per-job log tracking in sync (matches TS knownJobIds update)
		activeIDs := make(map[string]struct{}, len(jobs))
		for _, j := range jobs {
			activeIDs[j.ID] = struct{}{}
			s.db.TrackJobForLogs(j.ID)
		}
		s.db.PruneJobLogs(activeIDs)
		s.wsHub.BroadcastJobsUpdate(filterJobsByAge(jobs, s.configStore))
	})

	// Logger -> WebSocket: broadcast log lines + route to per-job buffers
	s.logSub = s.log.Subscribe()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("log forwarder panic", "panic", r)
			}
		}()
		for line := range s.logSub {
			s.wsHub.BroadcastLog(line)
			s.db.RouteLogToJobs(line) // Route to per-job buffer (matches TS knownJobIds log routing)
		}
	}()

	// Connectivity -> monitors + WebSocket: kick monitors on reconnect,
	// broadcast state — and notify. The web/TUI broadcasts are ephemeral;
	// an operator who was away during an outage previously had NO trace
	// that monitoring was down (streams silently missed). Transitions are
	// already debounced by the connectivity monitor, so this can't flap-spam.
	//
	// ONE notification, on restore only: a "lost" webhook has, by
	// definition, no connectivity to ride — it either dies in the sender's
	// bounded retries or lands late and out of order, so the offline
	// transition just stamps outageStart and the restore sends the whole
	// story (start, end, duration) as a single Outage Alert.
	// outageStart is guarded by a mutex — OnStateChange serializes today,
	// but this closure must not silently depend on that.
	var connNotifyMu sync.Mutex
	var outageStart time.Time
	s.connMon.OnStateChange(func(online bool) {
		if online {
			s.feedMon.CheckNow()
			s.decapiMon.CheckNow()
			s.twitchMon.CheckNow()
		}
		s.wsHub.BroadcastConnectivity(online)

		connNotifyMu.Lock()
		defer connNotifyMu.Unlock()
		if !online {
			outageStart = time.Now()
			return
		}
		if outageStart.IsZero() {
			return // startup/initial online state — nothing to report
		}
		title, desc, fields := outageAlert(outageStart, time.Now())
		outageStart = time.Time{}
		// Event key stays "connectivity_restored": the alert still fires on
		// the restore transition, and existing target allowlists must keep
		// working (the UIs relabel the toggle "Outage alert").
		s.notifyMgr.Send(title, desc, notifications.TypeWarning, fields,
			notifications.SendOptions{Event: "connectivity_restored"},
		)
	})
}

// outageAlert builds the Outage Alert notification for a connectivity
// outage spanning [start, end]: title, description, and the three embed
// fields. Start/end render as Discord dynamic timestamps (<t:unix:f> —
// Discord shows each viewer's local timezone; Discord webhooks are the only
// notification target type, so the markup never reaches a renderer that
// can't display it). Duration is a plain second-rounded string.
func outageAlert(start, end time.Time) (title, description string, fields []notifications.Field) {
	return "Outage Alert",
		"Internet connectivity was lost — channel monitoring paused and downloads waited; monitors are re-checking now",
		[]notifications.Field{
			{Name: "Started", Value: fmt.Sprintf("<t:%d:f>", start.Unix()), Inline: true},
			{Name: "Ended", Value: fmt.Sprintf("<t:%d:f>", end.Unix()), Inline: true},
			{Name: "Duration", Value: end.Sub(start).Round(time.Second).String(), Inline: true},
		}
}
