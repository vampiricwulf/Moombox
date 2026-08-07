package worker

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

func TestNormalizePath(t *testing.T) {
	// Basic test: relative paths resolve to absolute
	result := normalizePath("test.txt")
	if result == "" {
		t.Error("normalizePath should return non-empty result")
	}

	// Case normalization on Windows
	if runtime.GOOS == "windows" {
		a := normalizePath("C:\\Users\\Test\\file.txt")
		b := normalizePath("c:\\users\\test\\file.txt")
		if a != b {
			t.Errorf("normalizePath should be case-insensitive on Windows: %q != %q", a, b)
		}
	}
}

func TestIsUnderDirectory(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		dir      string
		expected bool
	}{
		{
			name:     "path under directory",
			path:     "./output/subdir/file.mp4",
			dir:      "./output",
			expected: true,
		},
		{
			name:     "path is the directory itself",
			path:     "./output",
			dir:      "./output",
			expected: false, // Correctly rejects deleting the root directory
		},
		{
			name:     "path escapes via ..",
			path:     "./output/../secret/file.txt",
			dir:      "./output",
			expected: false,
		},
		{
			name:     "completely unrelated path",
			path:     "./other/file.txt",
			dir:      "./output",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isUnderDirectory(tt.path, tt.dir)
			if result != tt.expected {
				t.Errorf("isUnderDirectory(%q, %q) = %v, want %v",
					tt.path, tt.dir, result, tt.expected)
			}
		})
	}
}

// TestDeleteOrphanedFileRefusesActiveJob verifies that DeleteOrphanedFile refuses to
// remove a staging directory whose owning job has become active between scan and
// delete. This is the race that reports/worker.md Finding 18 flagged.
func TestDeleteOrphanedFileRefusesActiveJob(t *testing.T) {
	dir := t.TempDir()
	stagingDir := filepath.Join(dir, "staging")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.MoomboxConfig{Paths: config.PathsConfig{StagingDirectory: stagingDir}}

	dbPath := filepath.Join(dir, "test.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	jobID := "yt_activeOrphanRaceTest"
	job := &database.Job{
		ID:      jobID,
		VideoID: "activeOrphanRaceTest",
		URL:     "https://youtube.com/watch?v=activeOrphanRaceTest",
		Status:  database.StatusDownloading, // active — must not be deletable
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}

	jobStagingPath := filepath.Join(stagingDir, jobID)
	if err := os.MkdirAll(jobStagingPath, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := DeleteOrphanedFile(jobStagingPath, db, cfg); err == nil {
		t.Fatal("expected DeleteOrphanedFile to refuse active job's staging dir, got nil error")
	}

	if _, err := os.Stat(jobStagingPath); err != nil {
		t.Errorf("staging dir should still exist after refused delete: %v", err)
	}

	// Now transition to a terminal state — delete should succeed.
	db.UpdateJobFields(jobID, map[string]any{"status": database.StatusCancelled})
	if err := DeleteOrphanedFile(jobStagingPath, db, cfg); err != nil {
		t.Errorf("expected delete to succeed after job transitioned to Cancelled, got %v", err)
	}
	if _, err := os.Stat(jobStagingPath); !os.IsNotExist(err) {
		t.Errorf("staging dir should be gone after successful delete, got err=%v", err)
	}
}

// TestDeleteOrphanedFileRefusesFinishedIncompleteTail verifies that a Finished
// job flagged IncompleteTail is protected the same way an active job is: its
// staging directory + resume sidecar are deliberately preserved (see
// Job.IncompleteTail) so Resume can append the missing tail, and the orphan
// scanner's pre-delete recheck must not let that preserved state be deleted.
// This pins the fix for the data-loss path where findActiveJobForPath's
// activeJobStatuses set omitted StatusFinished entirely.
func TestDeleteOrphanedFileRefusesFinishedIncompleteTail(t *testing.T) {
	dir := t.TempDir()
	stagingDir := filepath.Join(dir, "staging")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.MoomboxConfig{Paths: config.PathsConfig{StagingDirectory: stagingDir}}

	dbPath := filepath.Join(dir, "test.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	jobID := "yt_incompleteTailOrphanTest"
	job := &database.Job{
		ID:      jobID,
		VideoID: "incompleteTailOrphanTest",
		URL:     "https://youtube.com/watch?v=incompleteTailOrphanTest",
		Status:  database.StatusFinished,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	// insertJobExec doesn't carry incomplete_tail — only UpdateJobFields
	// flips it (see database.TestMigrateV17IncompleteTail).
	if updated := db.UpdateJobFields(jobID, map[string]any{"incomplete_tail": true}); updated == nil {
		t.Fatal("UpdateJobFields(incomplete_tail) returned nil")
	}

	jobStagingPath := filepath.Join(stagingDir, jobID)
	if err := os.MkdirAll(jobStagingPath, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := DeleteOrphanedFile(jobStagingPath, db, cfg); err == nil {
		t.Fatal("expected DeleteOrphanedFile to refuse Finished+IncompleteTail job's staging dir, got nil error")
	}

	if _, err := os.Stat(jobStagingPath); err != nil {
		t.Errorf("staging dir should still exist after refused delete: %v", err)
	}
}
