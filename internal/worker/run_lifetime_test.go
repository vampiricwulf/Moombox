package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestReleaseSlotsKeepsTheRunRegistered: ReleaseSlots gives the slots back
// and nothing else — the run's context, its registration and its Done stay
// until Complete.
//
// Mutant: having ReleaseSlots call Complete's body — every check fails.
func TestReleaseSlotsKeepsTheRunRegistered(t *testing.T) {
	q := NewJobQueue(1)
	q.Enqueue("x", database.StatusUpcoming)
	_, run, ok := q.Dequeue(context.Background())
	if !ok || !q.AcquireLifecycleSlot(context.Background(), "x") || !q.AcquireDownloadSlot(context.Background(), "x") {
		t.Fatal("setup")
	}
	done := q.Done("x")

	q.ReleaseSlots("x")
	if q.LifecycleCount() != 0 || q.ActiveCount() != 0 {
		t.Errorf("slots still held: lifecycle=%d downloads=%d", q.LifecycleCount(), q.ActiveCount())
	}
	if run.Err() != nil || !q.IsProcessing("x") {
		t.Error("ReleaseSlots ended the run")
	}
	select {
	case <-done:
		t.Error("ReleaseSlots closed Done while the run's goroutine is still running")
	default:
	}
	// A re-enqueue meanwhile is a duplicate of the run in flight.
	q.Enqueue("x", database.StatusUpcoming)
	if q.isPending("x") {
		t.Error("a re-enqueue during the run's tail was accepted — it would start a second run beside this one")
	}

	q.Complete("x")
	if run.Err() == nil || q.IsProcessing("x") {
		t.Error("Complete did not end the run")
	}
	select {
	case <-done:
	default:
		t.Error("Complete did not close Done")
	}
}

// TestParkedRunsCookieResumeWaitsForItsExit drives setJobError inside a run
// whose automatic cookie refresh succeeds. setJobError used to Complete the
// run up front, so the resume's Enqueue started a SECOND run while the first
// was still in its tail — and the first run's deferred Complete then cancelled
// the second's context and closed its Done. Now the slots go up front, the run
// stays registered, and the resume is handed to the queue once it exits.
//
// Mutants: setJobError calling Complete again — the run is unregistered
// while its tail runs; the resume Enqueueing directly — it is dropped as a
// duplicate and never reaches the queue.
func TestParkedRunsCookieResumeWaitsForItsExit(t *testing.T) {
	w, db := testWorkerSetup(t)
	const id = "j-cookie-resume"
	job := &database.Job{ID: id, VideoID: id, URL: "u", Platform: "youtube", Status: database.StatusDownloading}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	w.queue.Enqueue(id, database.StatusDownloading)
	if got, _, ok := w.queue.Dequeue(context.Background()); !ok || got != id {
		t.Fatalf("Dequeue = %q, %v", got, ok)
	}
	if !w.queue.AcquireLifecycleSlot(context.Background(), id) {
		t.Fatal("lifecycle slot")
	}
	w.OnCookieRefreshNeeded = func(string) bool { return true }

	w.setJobError(job, fmt.Errorf("%w: sign in to confirm", ErrCookiesRequired))

	if !w.queue.IsProcessing(id) {
		t.Fatal("setJobError unregistered the run; only processJob's deferred Complete may")
	}
	if w.queue.LifecycleCount() != 0 {
		t.Error("setJobError kept the lifecycle slot through its tail")
	}
	if w.queue.isPending(id) {
		t.Fatal("the resume was queued while the parked run was still registered")
	}

	w.queue.Complete(id) // processJob returns
	deadline := time.Now().Add(5 * time.Second)
	for !w.queue.isPending(id) {
		if time.Now().After(deadline) {
			t.Fatal("the cookie-refresh resume never reached the queue after the run exited")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
