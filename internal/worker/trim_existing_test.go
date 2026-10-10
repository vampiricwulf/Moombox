package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestCreateTrimFailsWhenExistingTrimsCannotBeListed pins the duplicate
// check's error path: CreateTrim used to discard GetTrimsForJob's error, so
// a DB read failure read as "no trims yet" — the duplicate check passed,
// uniqueTrimBasename picked the base name, and ffmpeg (-y) overwrote the
// trim already on disk while a second record was stored for the same path.
// The read failure is a closed database; the job is otherwise trim-ready, so
// the list read is the first thing that can fail.
//
// MUTANT: `existing, _ := ts.db.GetTrimsForJob(job.ID)` — CreateTrim reaches
// ffmpeg (and fails there, with no "list existing trims" in the error).
func TestCreateTrimFailsWhenExistingTrimsCannotBeListed(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "trim.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	ts := NewTrimService(db, "ffmpeg", discardLogger{})
	db.Close() // every read from here on fails

	output := filepath.Join(dir, "archive.mp4")
	if err := os.WriteFile(output, []byte("mp4"), 0o644); err != nil {
		t.Fatalf("seed output: %v", err)
	}
	job := &database.Job{
		ID: "yt_trimerr", VideoID: "trimerr", Platform: "youtube",
		Status: database.StatusFinished, OutputFile: output, Filename: "archive.mp4",
	}

	_, err = ts.CreateTrim(t.Context(), job, 0, 10, nil)
	if err == nil {
		t.Fatal("CreateTrim = nil error with the database closed, want the list failure")
	}
	if !strings.Contains(err.Error(), "list existing trims") {
		t.Errorf("CreateTrim error = %q, want it to name the existing-trims read", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "trim")); statErr == nil {
		// The trim dir is created before the check; what must NOT exist is
		// any trim file in it.
		entries, _ := os.ReadDir(filepath.Join(dir, "trim"))
		if len(entries) != 0 {
			t.Errorf("trim dir holds %d entries after the refused trim, want none", len(entries))
		}
	}
}
