package worker

import (
	"context"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

// notifyField is the fatal-on-missing wrapper around N1's Call.Field
// (notificationtest/recorder.go:35), which already answers
// `value, ok := call.Field(name)`. This adds only the t.Fatalf, so a missing
// field names the embed it was missing from instead of failing three
// assertions later on an empty string.
func notifyField(t *testing.T, c notificationtest.Call, name string) string {
	t.Helper()
	v, ok := c.Field(name)
	if !ok {
		t.Fatalf("field %q missing from %q; got %+v", name, c.Title, c.Fields)
	}
	return v
}

// TestUserCancelSendsTheOneCancelEmbed is audit C5's worker half: the
// mid-job cancel said "Download Cancelled" while the route's cancel of the
// same job a second earlier would have said "Job Cancelled".
//
// Mutant: leaving the old title in place — a subscriber filtering on the
// title (not the event) sees two different cancels for one action.
func TestUserCancelSendsTheOneCancelEmbed(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	rec := notificationtest.New()
	w.notifier = rec

	job := &database.Job{
		ID: "tw_c1", VideoID: "c1", Platform: "twitch", Title: "Cancel Me",
		ChannelName: "Streamer", URL: "https://twitch.tv/streamer",
		Status: database.StatusDownloading,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	// Cancel flags ONLY a job the queue is already PROCESSING
	// (internal/worker/queue.go:387 — "Only flag jobs that are actually
	// processing"), so the row has to be enqueued and dequeued first.
	// Without that, nothing is flagged, handleCancellation takes the SHUTDOWN
	// branch and sends nothing — the test would be red before the change and
	// red after it.
	w.queue.Enqueue(job.ID, database.StatusDownloading)
	if _, _, ok := w.queue.Dequeue(context.Background()); !ok {
		t.Fatal("Dequeue returned no job — the cancel path needs a processing run")
	}
	if !w.queue.Cancel(job.ID) {
		t.Fatal("Cancel did not flag a user cancel")
	}
	w.handleCancellation(job)

	calls := rec.ByEvent("cancelled")
	if len(calls) != 1 {
		t.Fatalf("recorded %d cancelled calls, want 1", len(calls))
	}
	if calls[0].Title != "Job Cancelled" {
		t.Errorf("title = %q, want %q", calls[0].Title, "Job Cancelled")
	}
	if got := notifyField(t, calls[0], "Stream ID"); got != "c1" {
		t.Errorf("Stream ID = %q", got)
	}
}
