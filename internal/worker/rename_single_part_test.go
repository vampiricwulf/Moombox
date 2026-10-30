package worker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

func newRenameTestOrch(t *testing.T) (*DownloadOrchestrator, *database.Database) {
	t.Helper()
	db := openTestDB(t)
	return &DownloadOrchestrator{db: db, logger: discardLogger{}}, db
}

func TestRenameSinglePartToPlain(t *testing.T) {
	cases := []struct {
		name       string
		writeSrc   bool // the suffixed part file is on disk
		writePlain bool // the plain name already holds the media
		wantPlain  bool // the returned seg and the row name the plain file
	}{
		// HappyPath: the normal case renames the suffixed part file to the
		// plain name and repairs the row.
		{name: "HappyPath", writeSrc: true, wantPlain: true},
		// RecoversRelocatedFile: recovery re-entry after a crash between a
		// prior run's rename and its DB commit — the file is already at the
		// plain name, the suffixed source is gone. Must adopt the plain path
		// and repair the row rather than leaving output_file pointing at a
		// missing file (which the orphan scanner would then offer for
		// deletion).
		{name: "RecoversRelocatedFile", writePlain: true, wantPlain: true},
		// GenuineFailureKeepsSuffixed: source missing AND plain missing (a
		// real error, not recovery) leaves the segment unchanged.
		{name: "GenuineFailureKeepsSuffixed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, db := newRenameTestOrch(t)
			out := t.TempDir()
			base := "My Stream"
			suffixed := filepath.Join(out, base+" - part1.mp4")
			plain := filepath.Join(out, base+".mp4")
			if tc.writeSrc {
				if err := os.WriteFile(suffixed, []byte("media"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.writePlain {
				if err := os.WriteFile(plain, []byte("real media"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			db.AddJob(&database.Job{ID: "j1", VideoID: "j1", URL: "u"})
			db.AddSegment(&database.Segment{JobID: "j1", SegmentIndex: 0, Filename: base + " - part1.mp4", FilePath: suffixed})
			segs, _ := db.GetSegments("j1")

			got := o.renameSinglePartToPlain(segs[0], out, base)
			if !tc.wantPlain {
				if got.FilePath != suffixed {
					t.Errorf("genuine failure should keep suffixed path, got %q", got.FilePath)
				}
				return
			}
			if got.FilePath != plain || got.Filename != base+".mp4" {
				t.Errorf("returned seg = %q/%q, want plain %q", got.FilePath, got.Filename, plain)
			}
			if _, err := os.Stat(plain); err != nil {
				t.Errorf("plain file not present: %v", err)
			}
			if segs2, _ := db.GetSegments("j1"); segs2[0].FilePath != plain {
				t.Errorf("row not repaired to plain path, got %q", segs2[0].FilePath)
			}
		})
	}
}
