package monitor

// BackfillState is a backfill scan's state as BackfillWorker.OnProgress
// reports it, and the "state" of the backfill_status WebSocket payload. The
// values are the wire bytes. Mirrored by tui.BackfillState* and the
// dashboard's utils.js BACKFILL_STATE (cmd/moombox
// TestBackfillStatesMatchAcrossLayers).
type BackfillState string

const (
	// BackfillScanning is one completed page of a scan in flight (it carries
	// the tab and the session's page count for it).
	BackfillScanning BackfillState = "scanning"
	// BackfillDone is a scan that completed: backfilled_at is written.
	BackfillDone BackfillState = "done"
	// BackfillError is a scan that ended incomplete: the cursor is saved and
	// the next sweep retries it.
	BackfillError BackfillState = "error"
	// BackfillIdle is no scan in flight: cancelled while queued, skipped
	// because the channel was disabled, paused, cancelled or pruned.
	BackfillIdle BackfillState = "idle"
)

// BackfillStates is every BackfillState, in declaration order.
var BackfillStates = []BackfillState{BackfillScanning, BackfillDone, BackfillError, BackfillIdle}

// Retained reports whether a channel's state is kept in the progress
// snapshot new clients are seeded from (runState.backfillProgress) and in
// the clients' own maps: a scan in flight, or one that failed and will be
// retried. Done and idle clear the channel's entry.
func (s BackfillState) Retained() bool {
	return s == BackfillScanning || s == BackfillError
}
