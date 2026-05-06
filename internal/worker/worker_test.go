package worker

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cipher"
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

// TestNewDownloadWorker_AcceptsRoutedCipherSolver confirms that
// DownloadWorkerDeps accepts a non-nil cipher.Solver in RoutedCipherSolver
// and that NewDownloadWorker constructs without panic. This locks in the
// field-shape so any future Deps refactor that drops or renames
// RoutedCipherSolver surfaces here.
//
// Full wiring (solver → orchestrator → StrategyDeps) is exercised by the
// production code path; this smoke ensures the constructor round-trip is
// covered so CI catches accidental field removal.
//
// Raised in the v2.6.16-prep code review (I3).
func TestNewDownloadWorker_AcceptsRoutedCipherSolver(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := &config.MoomboxConfig{}
	cfg.Paths.StagingDirectory = filepath.Join(dir, "staging")

	var solver cipher.Solver = stubCipherSolver{}

	deps := &DownloadWorkerDeps{
		CipherSolver:       nil, // goja resolver not needed for this smoke
		RoutedCipherSolver: solver,
	}

	// Confirm the interface assignment compiles (type-shape lock).
	var _ cipher.Solver = deps.RoutedCipherSolver

	// Confirm construction doesn't panic with a non-nil RoutedCipherSolver.
	w := NewDownloadWorker(db, nil, cfg, &discardLogger{}, deps)
	if w == nil {
		t.Fatal("NewDownloadWorker returned nil")
	}
}
