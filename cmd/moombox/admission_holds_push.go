package main

import (
	"context"

	"github.com/vampiricwulf/Moombox/internal/jobstate"
)

// holdsBroadcaster is the WebSocket hub's Broadcast, narrowed for the test.
type holdsBroadcaster interface {
	Broadcast(msgType string, payload any)
}

// runAdmissionHoldsPush pushes the backlog scheduler's hold snapshot
// (DownloadWorker.AdmissionHolds, owner ruling R-D3) to both UIs each time
// the scheduler signals a change: the dashboard as an admission_holds frame,
// the TUI through admissionHoldsTUI. Signals coalesce (the channel holds
// one), so a burst of changes is one snapshot. A signal on
// admissionHoldsResync (the TUI's Init, resyncAdmissionHoldsTUI) sends the
// snapshot to the TUI alone: one goroutine sends every snapshot, so the
// resync's is never delivered after a newer change's. Returns when ctx is
// done, or at once when the worker has no scheduler.
func (s *runState) runAdmissionHoldsPush(ctx context.Context, changed <-chan struct{}, snapshot func() jobstate.AdmissionHolds, hub holdsBroadcaster) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("admission holds push panic", "panic", r)
		}
	}()
	if changed == nil {
		return
	}
	for {
		toHub := true
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-s.admissionHoldsResync:
			toHub = false
		}
		h := snapshot()
		if hub != nil && toHub {
			hub.Broadcast("admission_holds", h)
		}
		if fn := s.admissionHoldsTUI.Load(); fn != nil {
			(*fn)(h)
		}
	}
}

// resyncAdmissionHoldsTUI asks runAdmissionHoldsPush to send the TUI the
// current snapshot again. Non-blocking: a pending resync already covers it.
func (s *runState) resyncAdmissionHoldsTUI() {
	select {
	case s.admissionHoldsResync <- struct{}{}:
	default:
	}
}
