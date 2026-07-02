package worker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

func newRenameTestOrch(t *testing.T) (*DownloadOrchestrator, *database.Database) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &DownloadOrchestrator{db: db, logger: discardLogger{}}, db
}

// TestRenameSinglePartToPlain_HappyPath: the normal case renames the suffixed
// part file to the plain name and repairs the row.
func TestRenameSinglePartToPlain_HappyPath(t *testing.T) {
	o, db := newRenameTestOrch(t)
	out := t.TempDir()
	base := "My Stream"
	src := filepath.Join(out, base+" - part1.mp4")
	if err := os.WriteFile(src, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	db.AddJob(&database.Job{ID: "j1", VideoID: "j1", URL: "u"})
	db.AddSegment(&database.Segment{JobID: "j1", SegmentIndex: 0, Filename: base + " - part1.mp4", FilePath: src})
	segs, _ := db.GetSegments("j1")

	got := o.renameSinglePartToPlain(segs[0], out, base)
	plain := filepath.Join(out, base+".mp4")
	if got.FilePath != plain || got.Filename != base+".mp4" {
		t.Errorf("returned seg = %q/%q, want plain %q", got.FilePath, got.Filename, plain)
	}
	if _, err := os.Stat(plain); err != nil {
		t.Errorf("plain file not present: %v", err)
	}
	if segs2, _ := db.GetSegments("j1"); segs2[0].FilePath != plain {
		t.Errorf("row not repaired: %q", segs2[0].FilePath)
	}
}

// TestRenameSinglePartToPlain_RecoversRelocatedFile: recovery re-entry after a
// crash between a prior run's rename and its DB commit — the file is already at
// the plain name, the suffixed source is gone. Must adopt the plain path and
// repair the row rather than leaving output_file pointing at a missing file
// (which the orphan scanner would then offer for deletion).
func TestRenameSinglePartToPlain_RecoversRelocatedFile(t *testing.T) {
	o, db := newRenameTestOrch(t)
	out := t.TempDir()
	base := "My Stream"
	plain := filepath.Join(out, base+".mp4")
	if err := os.WriteFile(plain, []byte("real media"), 0o644); err != nil { // already relocated
		t.Fatal(err)
	}
	suffixed := filepath.Join(out, base+" - part1.mp4") // gone on disk
	db.AddJob(&database.Job{ID: "j1", VideoID: "j1", URL: "u"})
	db.AddSegment(&database.Segment{JobID: "j1", SegmentIndex: 0, Filename: base + " - part1.mp4", FilePath: suffixed})
	segs, _ := db.GetSegments("j1")

	got := o.renameSinglePartToPlain(segs[0], out, base)
	if got.FilePath != plain || got.Filename != base+".mp4" {
		t.Errorf("recovery: returned seg = %q/%q, want plain %q", got.FilePath, got.Filename, plain)
	}
	if segs2, _ := db.GetSegments("j1"); segs2[0].FilePath != plain {
		t.Errorf("recovery: row not repaired to plain path, got %q", segs2[0].FilePath)
	}
}

// TestRenameSinglePartToPlain_GenuineFailureKeepsSuffixed: source missing AND
// plain missing (a real error, not recovery) leaves the segment unchanged.
func TestRenameSinglePartToPlain_GenuineFailureKeepsSuffixed(t *testing.T) {
	o, db := newRenameTestOrch(t)
	out := t.TempDir()
	base := "My Stream"
	suffixed := filepath.Join(out, base+" - part1.mp4") // neither exists
	db.AddJob(&database.Job{ID: "j1", VideoID: "j1", URL: "u"})
	db.AddSegment(&database.Segment{JobID: "j1", SegmentIndex: 0, Filename: base + " - part1.mp4", FilePath: suffixed})
	segs, _ := db.GetSegments("j1")

	got := o.renameSinglePartToPlain(segs[0], out, base)
	if got.FilePath != suffixed {
		t.Errorf("genuine failure should keep suffixed path, got %q", got.FilePath)
	}
}
