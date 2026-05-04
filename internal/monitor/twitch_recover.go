package monitor

import (
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// isRecoverableTwitchError reports whether an errored Twitch job is in the
// narrow shape that auto-recovery is designed for: a transient flap during
// the initial probe (no segments downloaded yet), captured in the database
// with the exact worker.TwitchOfflineErrMsg message, and still within the
// retry budget. Any deviation — different error string, partial download
// already on disk, exhausted budget — defers to user action.
//
// Imports worker.TwitchOfflineErrMsg so producer (stream_processor_twitch.go)
// and consumer (this predicate) can never drift on the literal.
func isRecoverableTwitchError(job *database.Job, maxRetries int) bool {
	if job == nil {
		return false
	}
	if job.Status != database.StatusError {
		return false
	}
	if job.Error != worker.TwitchOfflineErrMsg {
		return false
	}
	if job.LastVideoSeq != nil {
		return false
	}
	if job.AutoRetryCount >= maxRetries {
		return false
	}
	return true
}
