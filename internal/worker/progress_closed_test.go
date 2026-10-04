package worker

import (
	"testing"
	"time"
)

// TestClosedTrackerKeepsTheFinalizePhaseLine pins the guarantee Finalize's
// doc makes for the progress path too: once the tracker is closed, a report
// must not rewrite progress, percent or speed. The case is a VOD — the
// orchestrator writes "V:100% A:100% C: n" (or the honest incomplete string)
// and finalizes, then resolveVodChatOutcome lets the replay chat keep paging,
// and every batch calls SetChatCount → maybeUpdate, which used to stamp the
// live-style line, its percent and "0 B/s" over the finalize-phase values.
// The chat count itself still has to land, so the details panels keep
// ticking through the wait.
//
// MUTANT: dropping the closed check from maybeUpdate — progress reads
// "V:100.0% C: 42", speed "0 B/s". MUTANT: returning early without the
// chat-count write — total_chat_messages stays at 5.
func TestClosedTrackerKeepsTheFinalizePhaseLine(t *testing.T) {
	pt, db := newDBProgressTracker(t)
	pt.mu.Lock()
	pt.vodTotalBytes = 1 << 20
	pt.vodPercent = 100
	pt.chatCount = 5
	pt.mu.Unlock()

	pt.Finalize()
	// The orchestrator's finalize-phase write (orchestrator.go, VOD branch),
	// with a speed the last live report left behind.
	db.UpdateJobFields("act-test", map[string]any{
		"progress":            "V:100% A:100% C: 5",
		"percent":             100.0,
		"speed":               "4.2 MB/s",
		"total_chat_messages": 5,
	})

	pt.mu.Lock()
	pt.lastUpdate = time.Now().Add(-time.Second) // pass the report gate
	pt.mu.Unlock()
	pt.SetChatCount(42) // the replay pager's next batch, after Finalize

	job, err := db.GetJob("act-test")
	if err != nil || job == nil {
		t.Fatalf("GetJob: %v (job=%v)", err, job)
	}
	if job.Progress != "V:100% A:100% C: 5" {
		t.Errorf("progress = %q, want the finalize-phase line untouched", job.Progress)
	}
	if job.Percent != 100 {
		t.Errorf("percent = %v, want 100 untouched", job.Percent)
	}
	if job.Speed != "4.2 MB/s" {
		t.Errorf("speed = %q, want the last value untouched (not reverted to 0 B/s)", job.Speed)
	}
	if job.TotalChatMessages == nil || *job.TotalChatMessages != 42 {
		t.Errorf("total_chat_messages = %v, want 42 — the count must still land after close", job.TotalChatMessages)
	}
}
