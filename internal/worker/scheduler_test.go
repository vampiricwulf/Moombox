package worker

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestSchedulerWakeIsSafeNoOp pins the Task 2 stub contract: creation sites
// call Scheduler().Wake() before the admission loop exists (Task 3 builds
// it), so Wake must be non-blocking with no reader — repeat calls coalesce
// into the buffered signal instead of blocking the monitor callback.
func TestSchedulerWakeIsSafeNoOp(t *testing.T) {
	w, _ := testWorkerSetup(t)

	s := w.Scheduler()
	if s == nil {
		t.Fatal("Scheduler() = nil — backlog creation calls Scheduler().Wake()")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 3 {
			s.Wake() // nothing drains the signal yet; must never block
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wake() blocked with no scheduler loop running")
	}
}

// TestCreationByDisposition_BacklogRestsOutsideQueue drives the worker-side
// half of spec §10's creator table with jobs shaped exactly as the host's
// OnVideoFound creation writes them. The backlog VOD (Queued, priority 1) is
// never handed to the JobQueue — its creation path only wakes the scheduler —
// while broadcast/new-VOD shapes are enqueued immediately. History rows exist
// for every disposition (HasProcessed means "a job was created").
func TestCreationByDisposition_BacklogRestsOutsideQueue(t *testing.T) {
	w, db := testWorkerSetup(t)

	chID := "UC_archive"
	shapes := []struct {
		id         string
		status     database.JobStatus
		priority   int
		enqueueNow bool
	}{
		{"disp_broadcast", database.StatusUpcoming, 0, true},
		{"disp_new_vod", database.StatusUpcoming, 0, true},
		{"disp_backlog_vod", database.StatusQueued, 1, false},
	}
	for _, s := range shapes {
		if _, err := db.AddJob(&database.Job{
			ID: s.id, VideoID: s.id, URL: "u",
			Status: s.status, ChannelID: &chID, QueuePriority: s.priority,
		}); err != nil {
			t.Fatal(err)
		}
		// AddToHistory fires for EVERY disposition, exactly as the host does.
		if err := db.AddToHistory(s.id); err != nil {
			t.Fatal(err)
		}
		if s.enqueueNow {
			w.EnqueueJob(s.id)
		} else {
			w.Scheduler().Wake()
		}
	}

	// The backlog job rests: not processing, no pending entry.
	if w.queue.IsProcessing("disp_backlog_vod") {
		t.Error("backlog VOD is processing — it must wait for the scheduler")
	}
	w.queue.mu.Lock()
	_, backlogPending := w.queue.pendingSet["disp_backlog_vod"]
	_, broadcastPending := w.queue.pendingSet["disp_broadcast"]
	_, newVODPending := w.queue.pendingSet["disp_new_vod"]
	w.queue.mu.Unlock()
	if backlogPending {
		t.Error("backlog VOD entered the pending queue — backlog creation must skip EnqueueJob")
	}
	if !broadcastPending || !newVODPending {
		t.Errorf("admit-now dispositions not enqueued (broadcast=%v newVOD=%v)", broadcastPending, newVODPending)
	}

	for _, s := range shapes {
		ok, err := db.HasProcessed(s.id)
		if err != nil || !ok {
			t.Errorf("HasProcessed(%s) = %v, %v — AddToHistory fires for every disposition", s.id, ok, err)
		}
	}
}
