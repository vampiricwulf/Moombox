package worker

import (
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

func TestIsTerminalStatus(t *testing.T) {
	tests := []struct {
		status   database.JobStatus
		expected bool
	}{
		{database.StatusFinished, true},
		{database.StatusError, true},
		{database.StatusCancelled, true},
		{database.StatusUpcoming, false},
		{database.StatusLive, false},
		{database.StatusDownloading, false},
		{database.StatusMuxing, false},
		{database.StatusCookies, false},
	}

	for _, tt := range tests {
		result := isTerminalStatus(tt.status)
		if result != tt.expected {
			t.Errorf("isTerminalStatus(%q) = %v, want %v", tt.status, result, tt.expected)
		}
	}
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

func TestReinitializeJobResetsCounter(t *testing.T) {
	w, db := testWorkerSetup(t)

	job := &database.Job{
		ID:             "tw_userreinit",
		VideoID:        "userreinit",
		URL:            "https://twitch.tv/x",
		Platform:       "twitch",
		Status:         database.StatusError,
		Error:          "twitch channel is offline",
		AutoRetryCount: 2,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}

	w.ReinitializeJob("tw_userreinit")

	got, err := db.GetJob("tw_userreinit")
	if err != nil {
		t.Fatal(err)
	}
	if got.AutoRetryCount != 0 {
		t.Errorf("after user reinit, AutoRetryCount: want 0, got %d", got.AutoRetryCount)
	}
}
