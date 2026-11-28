package main

import (
	"context"
	"time"

	"github.com/vampiricwulf/Moombox/internal/worker"
)

// runLatencyBackfillPush pushes the latency-mode backfill's status (owner
// ruling RV-20) to both UIs each time the walk signals a change: the
// dashboard as a latency_backfill frame carrying the structured status,
// which it words with its mirror of worker.LatencyBackfillLine, and the TUI
// through latencyBackfillTUI as the worded line. Signals coalesce (the
// channel holds one), and the status, counts included, is read once per
// signal. Returns when ctx is done, or at once with no signal to read.
func (s *runState) runLatencyBackfillPush(ctx context.Context, changed <-chan struct{},
	status func() worker.LatencyBackfillStatus, hub holdsBroadcaster) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("latency backfill push panic", "panic", r)
		}
	}()
	if changed == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
		st := status()
		if hub != nil {
			hub.Broadcast("latency_backfill", st)
		}
		if fn := s.latencyBackfillTUI.Load(); fn != nil {
			(*fn)(worker.LatencyBackfillLine(st, time.Local))
		}
	}
}

// latencyBackfillLine is the backfill's Settings line as the TUI shows it,
// with a rate-limit pause's end in the host's zone.
func (s *runState) latencyBackfillLine() string {
	return worker.LatencyBackfillLine(s.dlWorker.LatencyBackfillStatus(), time.Local)
}
