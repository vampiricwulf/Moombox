package worker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestScanListsOrphansUnderALinkedOutputDirectory: paths.output_directory is
// a symlink (a junction, on Windows) to the tree it names. filepath.Walk
// Lstats its root, which reads the link as one entry that is not a
// directory, so the sweep listed nothing under it: every orphan in the
// archive tree was missing from the Files tab and the TUI's Files dialog.
// The walk now resolves the root, and what it lists keeps the configured
// spelling — the one the rows store and the one RelPath is taken against —
// so the ownership rule (a row naming the file through the link owns it) and
// the delete's containment (canonical on both sides) apply as they do to a
// plain directory.
//
// Mutant: scanOutputOrphans walking absOutputDir as configured, without
// the trailing separator — the orphan is not listed.
func TestScanListsOrphansUnderALinkedOutputDirectory(t *testing.T) {
	f := newOrphanFixture(t)
	realOut := f.outputDir + "-real"
	if err := os.MkdirAll(realOut, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realOut, f.outputDir); err != nil {
		t.Skipf("cannot create a directory link here: %v", err)
	}
	orphan := f.write(t, filepath.Join("Chan", "Stray [orpvid00001].mp4"))
	archive := f.write(t, filepath.Join("Chan", "Stream [lnkvid00002].mp4"))
	addJob(t, f, "lnkvid00002", database.StatusFinished, func(j *database.Job) { j.OutputFile = archive })

	entries, err := ScanOrphanedFiles(f.db, f.cfg)
	if err != nil {
		t.Fatalf("ScanOrphanedFiles: %v", err)
	}
	var found *OrphanedEntry
	for i, e := range entries {
		switch e.Path {
		case orphan:
			found = &entries[i]
		case archive, filepath.Join(realOut, "Chan", "Stream [lnkvid00002].mp4"):
			t.Errorf("the archive a row names through the link is offered as %q", e.Type)
		}
	}
	if found == nil {
		t.Fatalf("the orphan under the linked output directory is not listed (entries %+v)", entries)
	}
	if want := filepath.Join("Chan", "Stray [orpvid00001].mp4"); found.RelPath != want || found.Type != "output" {
		t.Errorf("listed as %q (%s), want %q (output)", found.RelPath, found.Type, want)
	}

	if err := DeleteOrphanedFile(found.Path, f.db, f.cfg); err != nil {
		t.Fatalf("the listed orphan cannot be deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(realOut, "Chan", "Stray [orpvid00001].mp4")); !os.IsNotExist(err) {
		t.Errorf("the orphan is still on disk: %v", err)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Errorf("the archive is gone: %v", err)
	}
}
