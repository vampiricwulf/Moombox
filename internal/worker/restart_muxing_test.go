package worker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// stageMediaFor drops a recognised media file into <stagingBase>/<jobID> so
// HasSegmentFiles answers true.
func stageMediaFor(t *testing.T, stagingBase, jobID string) {
	t.Helper()
	dir := filepath.Join(stagingBase, jobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "video.ts"), []byte("staged"), 0o644); err != nil {
		t.Fatalf("write staged media: %v", err)
	}
}

// TestMuxOnRestart pins owner decision O-B's predicate: an interrupted Muxing
// row whose staging still holds recognised media is re-muxed from what is
// there, EXCEPT when incomplete_tail is set — that row still needs the
// post-live VOD-refresh loop, so it goes back through Downloading.
//
// Mutants, one per row:
//   - dropping the IncompleteTail term: an incomplete-tail row is muxed short
//     and its missing tail is never refreshed.
//   - dropping the HasSegmentFiles term: a Muxing row with empty staging is
//     handed to muxFromStaging, which fails with "no segment files found".
//   - dropping the status term: every job on disk is muxed at boot.
func TestMuxOnRestart(t *testing.T) {
	base := t.TempDir()
	stageMediaFor(t, base, "staged")

	for _, tc := range []struct {
		name string
		job  *database.Job
		want bool
	}{
		{"muxing with staged media", &database.Job{ID: "staged", Status: database.StatusMuxing}, true},
		{"muxing with incomplete tail", &database.Job{ID: "staged", Status: database.StatusMuxing, IncompleteTail: true}, false},
		{"muxing with empty staging", &database.Job{ID: "empty", Status: database.StatusMuxing}, false},
		{"downloading with staged media", &database.Job{ID: "staged", Status: database.StatusDownloading}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := muxOnRestart(tc.job, base); got != tc.want {
				t.Errorf("muxOnRestart(%s/%s) = %v, want %v", tc.job.Status, tc.job.ID, got, tc.want)
			}
		})
	}
}

// TestEnqueueExistingJobsRoutesMuxingRow pins the routing itself: a Muxing row
// with staged media is NOT reset to Downloading and NOT put on the queue (the
// mux runs off-queue via MuxJob), while an incomplete-tail Muxing row is. The
// staged row is a Twitch job on purpose: the historical reset handed it to the
// stream processor, which lands a post-live Twitch job in Error "channel is
// offline" — the O-B route never touches the network.
//
// Mutant: restoring the unconditional "reset interrupted mux job" block — the
// staged row reappears as Downloading in the pending set and re-downloads
// from sq=0 over the complete recording.
func TestEnqueueExistingJobsRoutesMuxingRow(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)

	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })

	// Keep the off-queue mux's output inside the test's temp tree; the config
	// default would write ./output relative to the package directory.
	outDir := t.TempDir()
	staged := &database.Job{ID: "j-staged", VideoID: "v1", Platform: "twitch", Status: database.StatusMuxing, OutputDirectory: outDir}
	tail := &database.Job{ID: "j-tail", VideoID: "v2", Platform: "youtube", Status: database.StatusMuxing, IncompleteTail: true, OutputDirectory: outDir}
	for _, j := range []*database.Job{staged, tail} {
		if _, err := db.AddJob(j); err != nil {
			t.Fatalf("AddJob %s: %v", j.ID, err)
		}
		stageMediaFor(t, stagingBase, j.ID)
	}
	// insertJobExec does not carry incomplete_tail (it is only ever set on a
	// live row), so the flag has to be written the way the download path
	// writes it — otherwise the tail row reads back as a plain Muxing row.
	if db.UpdateJobFields(tail.ID, map[string]any{"incomplete_tail": true}) == nil {
		t.Fatalf("UpdateJobFields(incomplete_tail) returned nil for %s", tail.ID)
	}

	w.enqueueExistingJobs()

	if fresh, _ := db.GetJob("j-tail"); fresh == nil || fresh.Status != database.StatusDownloading {
		t.Fatalf("incomplete-tail Muxing row = %v, want Downloading (the VOD-refresh loop must still run)", fresh)
	}
	if !w.queue.isPending("j-tail") {
		t.Error("incomplete-tail row was not enqueued")
	}
	if w.queue.isPending("j-staged") {
		t.Error("a Muxing row with staged media must not be enqueued for re-download — it muxes off-queue")
	}
}
