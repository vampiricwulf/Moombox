package jobstate

import "github.com/vampiricwulf/Moombox/internal/database"

// AwaitingProcessing reports whether an Upcoming row is a VOD the scheduler
// or a creator handed to stream processing, not a broadcast being waited
// for: a row stream processing has called a VOD (IsVod), or a backlog row
// (queue_priority 1 of a channel — worker.CookieResumeStatus's rule) the
// archive-slots scheduler admitted, whose IsVod is written only once its
// processing starts. Such a row has no stream to wait for, and both UIs say
// "Awaiting processing" for it unless it is in a wait (IsWaitLine) — owner
// ruling R-D1. No writer stores the text; each UI derives it.
//
// utils.js isAwaitingProcessing is this rule; cmd/moombox's job-state parity
// test runs both over web/tests/fixtures/job-state-parity.json. Like the
// dashboard's, it does not read the status: callers ask it of Upcoming rows.
func AwaitingProcessing(job *database.Job) bool {
	if job == nil {
		return false
	}
	return job.IsVod || (job.QueuePriority == 1 && job.ChannelID != nil && *job.ChannelID != "")
}

// AwaitingProcessingText is the line both UIs show for such a row.
const AwaitingProcessingText = "Awaiting processing"
