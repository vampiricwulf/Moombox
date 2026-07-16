package worker

// Scheduler admits backlog (Queued) jobs M-at-a-time per channel — the
// archive-slots pacing of spec §10. PLAN4-TASK3 fleshes this out: the
// single-goroutine Run loop, the M-count and admission-order queries, and
// the per-channel archive_slots resolver. Until then it is only a wake
// target so creation sites can signal "backlog changed" without enqueueing
// anything themselves.
type Scheduler struct {
	// wake coalesces signals: capacity 1, non-blocking send. A wake that
	// arrives while one is already pending is absorbed — the run loop
	// drains at most one signal per admission sweep.
	wake chan struct{}
}

// newScheduler creates the worker-owned scheduler.
func newScheduler() *Scheduler {
	return &Scheduler{wake: make(chan struct{}, 1)}
}

// Wake signals the scheduler that backlog state changed (a Queued job was
// created, or a slot may have freed). Non-blocking and safe from any
// goroutine — with no Run loop draining yet, repeat calls coalesce into the
// single buffered signal.
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
