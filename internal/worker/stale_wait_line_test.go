package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestBootClearsAStaleWaitLine: a wait writer clears its line on the way out
// (a defer), but a process that dies mid-wait never runs it, and YouTube's
// waitForLive writes no progress of its own — so an Upcoming row kept a
// frozen "Rate-limited by YouTube; fetching again... (24m 0s left)" for the
// whole re-run wait once the dashboard began showing an Upcoming row's wait
// line (owner ruling R-D1). The boot pass clears a wait line on the Upcoming
// rows it re-enqueues, and nothing else: an ordinary progress line, and a row
// in another status, keep theirs. A Downloading row is re-enqueued too, but
// its run writes progress of its own (the replacing writer Upcoming lacks),
// and the dashboard and TUI read its line as the last thing it did.
//
// A Muxing row with nothing staged is reset to Downloading; a mux-slot wait
// line a dead process left on it (acquireMuxSlot) goes with the reset, and
// any other line stays.
//
// Mutants: enqueueExistingJobs not clearing — the stale line survives; the
// clear applied to every re-enqueued status — the Downloading line is wiped;
// the Muxing reset not clearing — the mux-slot wait stays on the Downloading
// row; the Muxing reset clearing every line — the counters are wiped.
func TestBootClearsAStaleWaitLine(t *testing.T) {
	w, db := testWorkerSetup(t)
	add := func(id string, st database.JobStatus, progress string) {
		t.Helper()
		if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, URL: "u", Status: st}); err != nil {
			t.Fatal(err)
		}
		db.UpdateJobFields(id, map[string]any{"progress": progress})
	}
	add("stale", database.StatusUpcoming, waitLine("Rate-limited by YouTube; fetching again", "24m 0s left"))
	add("twitch_wait", database.StatusUpcoming, "Waiting for stream...")
	add("queued_wait", database.StatusQueued, waitLine("Waiting for a download slot", "3m"))
	add("downloading_wait", database.StatusDownloading, waitLine("Network unavailable; waiting", "2m"))
	add("mux_slot_wait", database.StatusMuxing, muxSlotWaitLine(0))
	add("mux_counters", database.StatusMuxing, "(V: 10/10 A: 10/10)")

	w.enqueueExistingJobs()

	for id, want := range map[string]string{
		"stale":            "",
		"twitch_wait":      "Waiting for stream...",
		"queued_wait":      waitLine("Waiting for a download slot", "3m"),
		"downloading_wait": waitLine("Network unavailable; waiting", "2m"),
		"mux_slot_wait":    "",
		"mux_counters":     "(V: 10/10 A: 10/10)",
	} {
		j, _ := db.GetJob(id)
		if j.Progress != want {
			t.Errorf("%s: progress = %q after boot, want %q", id, j.Progress, want)
		}
	}
}
