package main

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/worker"
)

// TestForceExitOutlastsTheWorkerStop pins the ordering owner decision O-E
// depends on. The backstop's clock starts before the worker's Stop does, so
// a backstop no longer than the worker's own budget exits before Stop
// reaches CancelMuxes, leaving FFmpeg writing into staging the restarted
// child re-muxes with -y.
//
// Mutant: restore the flat 10-second backstop — it is shorter than the
// worker's in-flight wait plus mux-cancel grace.
func TestForceExitOutlastsTheWorkerStop(t *testing.T) {
	if forceExitAfter <= worker.StopBudget {
		t.Fatalf("forceExitAfter = %v, want more than worker.StopBudget (%v)", forceExitAfter, worker.StopBudget)
	}
}
