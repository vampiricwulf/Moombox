package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// stubCipherSolver is a minimal cipher.Solver for worker package tests.
// The real staticSolver lives in package cipher (unexported); we define a
// local stub here so worker tests have an injectable non-nil Solver without
// pulling in the cipher test helpers.
type stubCipherSolver struct{}

func (stubCipherSolver) Sig(_ context.Context, _, _ string) (string, error) { return "", nil }
func (stubCipherSolver) N(_ context.Context, _, _ string) (string, error)   { return "", nil }
func (stubCipherSolver) Batch(_ context.Context, _ string, sigs, ns []string) (map[string]string, map[string]string, error) {
	return map[string]string{}, map[string]string{}, nil
}

// testWorkerSetup creates a DownloadWorker against a temp SQLite DB with a
// minimal config. Returned worker has no Twitch service, no notifier, no
// connectivity monitor — sufficient for testing pure DB-driven methods like
// ReinitializeJob and AutoReinitializeJob.
func testWorkerSetup(t *testing.T) (*DownloadWorker, *database.Database) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := &config.MoomboxConfig{}
	cfg.Paths.StagingDirectory = filepath.Join(dir, "staging")

	logger := &discardLogger{}
	w := NewDownloadWorker(db, nil, cfg, logger, nil)
	return w, db
}

// openTestDB opens a fresh database in a temp dir of its own, closed when
// the test ends.
func openTestDB(t *testing.T) *database.Database {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// discardLogger is the package's one no-op logger. It satisfies the anonymous
// logger interface every constructor here takes, and twitch.NewAuth's and
// cookies.NewRefreshService's too.
type discardLogger struct{}

func (discardLogger) Debug(msg string, args ...any) {}
func (discardLogger) Info(msg string, args ...any)  {}
func (discardLogger) Warn(msg string, args ...any)  {}
func (discardLogger) Error(msg string, args ...any) {}

func TestAutoReinitializeJobIncrementsCounter(t *testing.T) {
	w, db := testWorkerSetup(t)

	job := &database.Job{
		ID:             "tw_autoreinit",
		VideoID:        "autoreinit",
		URL:            "https://twitch.tv/x",
		Platform:       "twitch",
		Status:         database.StatusError,
		Error:          "twitch channel is offline",
		AutoRetryCount: 0,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}

	w.AutoReinitializeJob("tw_autoreinit")

	got, err := db.GetJob("tw_autoreinit")
	if err != nil {
		t.Fatal(err)
	}
	if got.AutoRetryCount != 1 {
		t.Errorf("AutoRetryCount: want 1, got %d", got.AutoRetryCount)
	}
	if got.Status != database.StatusUpcoming {
		t.Errorf("Status: want Upcoming, got %s", got.Status)
	}
	if got.Error != "" {
		t.Errorf("Error: want cleared, got %q", got.Error)
	}

	// The monitor offers it on an Error row only (the run that failed again).
	db.UpdateJobFields("tw_autoreinit", map[string]any{"status": database.StatusError})
	// Second call: counter goes to 2
	w.AutoReinitializeJob("tw_autoreinit")
	got, err = db.GetJob("tw_autoreinit")
	if err != nil {
		t.Fatal(err)
	}
	if got.AutoRetryCount != 2 {
		t.Errorf("after second call, AutoRetryCount: want 2, got %d", got.AutoRetryCount)
	}
}

// TestReinitializeDuringUnwindWaitsForTheRun: the cancel route writes
// Cancelled before the run stops, so Retry/Reinitialize show at once, and one
// clicked while the run unwound used to race it — staging deleted under the
// live run, Enqueue dropped as a duplicate, and handleCancellation's
// Cancelled written over the fresh Upcoming. The reset now waits for the run.
//
// Mutants: run ReinitializeJob's body immediately (staging is gone while the
// run is still in flight); write handleCancellation's Cancelled after
// Complete again (the row can end Cancelled instead of Upcoming).
func TestReinitializeDuringUnwindWaitsForTheRun(t *testing.T) {
	w, db := testWorkerSetup(t)
	const id = "j-unwind"
	if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, URL: "u", Platform: "youtube",
		Status: database.StatusDownloading}); err != nil {
		t.Fatal(err)
	}
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
	stagingDir := filepath.Join(stagingBase, id)
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagingDir, "chat.resume.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A run in flight, then the operator's Cancel.
	w.queue.Enqueue(id, database.StatusDownloading)
	if got, _, ok := w.queue.Dequeue(context.Background()); !ok || got != id {
		t.Fatalf("Dequeue = %q, %v", got, ok)
	}
	w.CancelJob(id)

	// Reinitialize, clicked while the run is still unwinding.
	w.ReinitializeJob(id)
	if _, err := os.Stat(stagingDir); err != nil {
		t.Fatalf("Reinitialize removed staging while the run was still in flight: %v", err)
	}

	// The run finishes unwinding. Its Cancelled must land while it still
	// holds its queue slot, i.e. before Complete closes the Done channel the
	// deferred reset waits on — otherwise it can land on the reset's Upcoming.
	var cancelledWhileProcessing []bool
	unsub := db.OnJobUpdate(func(j *database.Job) {
		if j.ID == id && j.Status == database.StatusCancelled {
			cancelledWhileProcessing = append(cancelledWhileProcessing, w.queue.IsProcessing(id))
		}
	})
	job, _ := db.GetJob(id)
	w.handleCancellation(job)
	unsub()
	if len(cancelledWhileProcessing) != 1 || !cancelledWhileProcessing[0] {
		t.Errorf("handleCancellation's Cancelled write saw processing=%v; want it written before Complete", cancelledWhileProcessing)
	}
	// handleCancellation gives back only the slots; the run ends where
	// processJob's deferred Complete ends it.
	if !w.queue.IsProcessing(id) {
		t.Error("handleCancellation unregistered the run; only processJob's deferred Complete may")
	}
	w.queue.Complete(id)

	deadline := time.Now().Add(5 * time.Second)
	for {
		j, _ := db.GetJob(id)
		_, statErr := os.Stat(stagingDir)
		if j != nil && j.Status == database.StatusUpcoming && os.IsNotExist(statErr) && w.queue.PendingCount() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the run exited: status=%v staging err=%v pending=%d; want Upcoming, staging gone, queued",
				statusOf(j), statErr, w.queue.PendingCount())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
