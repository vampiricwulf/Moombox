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
// The trim service now stops ahead of the worker, and its wait for a trim
// whose FFmpeg will not die (worker.TrimStopWait) starts the worker's budget
// that much later, so the backstop must outlast the two together.
//
// Mutants: restore the flat 10-second backstop — it is shorter than the
// worker's in-flight wait plus mux-cancel grace; raise TrimStopWait to 3 s —
// the backstop fires before a worker that started that late reaches the end
// of its mux-cancel grace.
func TestForceExitOutlastsTheWorkerStop(t *testing.T) {
	if forceExitAfter <= worker.StopBudget {
		t.Fatalf("forceExitAfter = %v, want more than worker.StopBudget (%v)", forceExitAfter, worker.StopBudget)
	}
	if forceExitAfter <= worker.TrimStopWait+worker.StopBudget {
		t.Errorf("forceExitAfter = %v, want more than the trim stop's wait (%v) and worker.StopBudget (%v) together",
			forceExitAfter, worker.TrimStopWait, worker.StopBudget)
	}
}
