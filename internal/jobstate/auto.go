package jobstate

import "github.com/vampiricwulf/Moombox/internal/database"

// MaxTwitchAutoRetries caps how many times the Twitch monitor's auto-recovery
// will re-initialise an errored Twitch live job before giving up. 2 retries
// (3 attempts including the original) rides out GQL flaps measured in seconds
// without looping on a persistent fault. A user-driven Reinitialize resets the
// count. worker.MaxTwitchAutoRetries is this constant.
const MaxTwitchAutoRetries = 2

// TwitchOfflineErrMsg is the error a non-manual Twitch live job takes when its
// channel is offline at stream processing (the offline flap). The worker
// writes it (worker.TwitchOfflineErrMsg is this constant) beside
// database.ParkReasonTwitchOffline, and the reason is what
// TwitchAutoRetryPending keys on (owner ruling R-I1): the text is what the
// operator reads and routes nothing in this binary. It is still the frozen
// database.LegacyTwitchOfflineError byte for byte, because the release before
// the park reason keys on the text — reword it only once no such binary can
// run on a database this one wrote (TestLegacyOfflineTextIsTheWritersText).
const TwitchOfflineErrMsg = "twitch channel is offline"

// legacyOfflineFlapRow reports whether job is an offline-flap row as releases
// before database.ParkReasonTwitchOffline wrote it: the frozen error text and
// no park reason at all. database.Open backfills the reason onto such rows,
// so this is read only for one a boot has not converted — written by an older
// binary still running against this database, or one whose backfill failed.
//
// ONE-RELEASE FALLBACK (owner ruling R-I1): delete this function and its one
// call, in TwitchAutoRetryPending, in the release after the one that
// introduces ParkReasonTwitchOffline. The dashboard's twin is
// legacyOfflineFlapRow in web/public/modules/utils.js.
func legacyOfflineFlapRow(job *database.Job) bool {
	return job.ParkReason == database.ParkReasonNone && job.Error == database.LegacyTwitchOfflineError
}

// TwitchAutoRetryPending reports whether job is an errored Twitch live job the
// Twitch monitor will re-initialise (AutoReinitializeJob) when it finds the
// same broadcast live again: a failure before any segment was downloaded
// (LastVideoSeq nil), still within MaxTwitchAutoRetries, and one of two kinds,
// each a park reason setJobError records — the offline flap,
// ParkReasonTwitchOffline, or a probe that never reached Twitch,
// ParkReasonTwitchUnreachable. Anything else waits for the operator. The
// error text routes nothing, but for one release: a flap row from before its
// reason existed is still read by its text (legacyOfflineFlapRow).
//
// The one predicate for every caller (it replaced two copies kept in sync by
// comment):
//   - the Twitch monitor's recovery (monitor.processStreamInfo) and its VOD
//     backlog's capturedLiveJob, on the row as read;
//   - worker.setJobError's notification suppression, on a copy of the job
//     carrying the status, error and park reason the failure is about to
//     write — a retry still possible under these conditions is not worth a
//     Job Failed embed;
//   - PendingAutoAction, for both UIs.
//
// A row holds one park reason, so the two retry reasons exclude every other:
// a row the unconfirmed-end latch marked (ParkReasonTwitchEndUnconfirmed) is
// the automatic mux's, never a re-initialisation's, which would delete the
// staging the mux exists to archive; ParkReasonTwitchEndedOffline and
// ParkReasonMuxAfterCancel are the operator's Mux's. The legacy text is read
// only on a row no reason claimed, which keeps the same exclusions for it.
// Every row a writer from before ParkReasonTwitchOffline could produce is
// answered as the text-keyed rule answered it
// (TestOfflineParkReasonsKeepTheRecoveryTruthTable).
func TwitchAutoRetryPending(job *database.Job) bool {
	if job == nil || job.Status != database.StatusError {
		return false
	}
	switch job.ParkReason {
	case database.ParkReasonTwitchOffline, database.ParkReasonTwitchUnreachable:
	default:
		if !legacyOfflineFlapRow(job) {
			return false
		}
	}
	return job.LastVideoSeq == nil && job.AutoRetryCount < MaxTwitchAutoRetries
}

// AutoAction names the automatic action a job's row is waiting on. The wire
// values are the job JSON's pendingAutoAction (routes enrichJob) and the
// dashboard's pendingAutoAction (utils.js); "" is none.
type AutoAction string

const (
	// AutoActionNone: nothing automatic will happen; the row waits for the
	// operator.
	AutoActionNone AutoAction = ""
	// AutoActionRetry: the Twitch monitor re-initialises the job when it
	// finds the same broadcast live again (TwitchAutoRetryPending).
	AutoActionRetry AutoAction = "auto_retry"
	// AutoActionMux: the Twitch monitor muxes the job's staging once it
	// confirms the broadcast over (ParkReasonTwitchEndUnconfirmed, owner
	// decision D-T4), exactly as the Mux action would, once.
	AutoActionMux AutoAction = "auto_mux"
)

// AutoMuxPending is the automatic mux's precondition: a Twitch row in Error
// the unconfirmed-end latch marked. worker.AutoMuxEndedBroadcast re-checks it
// on the row before it claims it, and db.TwitchEndUnconfirmedJobs selects by
// the same three columns.
func AutoMuxPending(job *database.Job) bool {
	return job != nil && job.Platform == "twitch" && job.Status == database.StatusError &&
		job.ParkReason == database.ParkReasonTwitchEndUnconfirmed
}

// PendingAutoAction is the automatic action the Twitch monitor will take for
// job, if any: AutoActionMux for a row the unconfirmed-end latch marked,
// AutoActionRetry for a Twitch row TwitchAutoRetryPending accepts that the
// recovery can reach, else AutoActionNone. Derived from the row, never
// persisted.
//
// Reach: the recovery (monitor.processStreamInfo) acts only on a row it finds
// HasProcessed under twitch.BuildJobID's "tw_<stream id>", and a manual add
// writes no history row (and a manual add of an offline channel is
// "tw_manual_<login>_<n>"), so a ManuallyAdded row is never offered one —
// TwitchAutoRetryPending, which the monitor and setJobError read, keeps
// answering for it as before; only this derived answer drops it. The
// automatic mux does reach a manual row: dispatchEndedBroadcasts reads the
// login from either id shape.
//
// The row's side of it only: the monitor acts only for a channel it still
// polls, and only once the broadcast answers (live again for a retry, over
// for a mux). A row cannot tell whether its channel is still configured, so
// Text states that condition rather than promising the action.
//
// Both UIs show it (the dashboard's job row and details, the TUI's details
// pane and row tag), and both Reinitialize confirms warn that the reset
// cancels it (worker.ReinitializePreview) — for a pending mux, by deleting
// the very staging the mux would archive.
func PendingAutoAction(job *database.Job) AutoAction {
	switch {
	case AutoMuxPending(job):
		return AutoActionMux
	case job != nil && job.Platform == "twitch" && !job.ManuallyAdded && TwitchAutoRetryPending(job):
		return AutoActionRetry
	}
	return AutoActionNone
}

// Text is the sentence both UIs show for a pending action: the TUI's details
// pane, the dashboard's twin pendingAutoActionText (utils.js). "" for none.
func (a AutoAction) Text() string {
	switch a {
	case AutoActionMux:
		return "Muxes automatically once Twitch confirms the broadcast over, while the channel is monitored"
	case AutoActionRetry:
		return "Retries automatically if the broadcast is live again, while the channel is monitored"
	}
	return ""
}
