package worker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestStagingSweepLeavesWhatAJobStillHolds: staging of a job that is still on
// its way (Queued with its resume checkpoint, parked in COOKIES? with its
// segments) or that the Twitch monitor will mux automatically is not an
// orphan — the sweep does not list it and a delete of it is refused — while a
// plain Error or Cancelled job's staging is still listed and deletable. The
// list and the delete recheck share one rule (jobHoldsStaging), so each row
// asserts both.
func TestStagingSweepLeavesWhatAJobStillHolds(t *testing.T) {
	cases := []struct {
		name     string
		platform string
		status   database.JobStatus
		park     database.ParkReason
		held     bool
	}{
		{name: "Queued", platform: "youtube", status: database.StatusQueued, held: true},
		{name: "Cookies", platform: "youtube", status: database.StatusCookies, held: true},
		{name: "TwitchEndUnconfirmed", platform: "twitch", status: database.StatusError, park: database.ParkReasonTwitchEndUnconfirmed, held: true},
		// The automatic retry starts over from nothing: it holds no staging.
		{name: "TwitchOffline", platform: "twitch", status: database.StatusError, park: database.ParkReasonTwitchOffline},
		{name: "Error", platform: "youtube", status: database.StatusError},
		{name: "Cancelled", platform: "twitch", status: database.StatusCancelled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stagingDir := filepath.Join(dir, "staging")
			cfg := &config.MoomboxConfig{Paths: config.PathsConfig{StagingDirectory: stagingDir}}
			db, err := database.Open(filepath.Join(dir, "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			jobID := "job_heldStaging"
			if _, err := db.AddJob(&database.Job{
				ID: jobID, VideoID: "heldStaging", URL: "https://example.invalid/heldStaging",
				Platform: tc.platform, Status: tc.status,
			}); err != nil {
				t.Fatal(err)
			}
			if tc.park != "" {
				if db.UpdateJobFields(jobID, map[string]any{"park_reason": tc.park}) == nil {
					t.Fatal("UpdateJobFields(park_reason) returned nil")
				}
			}
			jobStaging := filepath.Join(stagingDir, jobID)
			if err := os.MkdirAll(jobStaging, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(jobStaging, "video.ts"), []byte("media"), 0o644); err != nil {
				t.Fatal(err)
			}

			entries, err := scanStagingOrphans(db, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if listed := len(entries) == 1; listed == tc.held {
				t.Errorf("listed as an orphan = %v, want %v (entries %+v)", listed, !tc.held, entries)
			}

			err = DeleteOrphanedFile(jobStaging, db, cfg)
			_, statErr := os.Stat(jobStaging)
			if tc.held {
				var notOrphan *NotOrphanError
				if !errors.As(err, &notOrphan) {
					t.Errorf("delete of held staging: err = %v, want a NotOrphanError", err)
				}
				if statErr != nil {
					t.Errorf("held staging should survive the refused delete: %v", statErr)
				}
				return
			}
			if err != nil {
				t.Errorf("delete of orphaned staging: %v", err)
			}
			if !os.IsNotExist(statErr) {
				t.Errorf("orphaned staging should be gone, stat err = %v", statErr)
			}
		})
	}
}
