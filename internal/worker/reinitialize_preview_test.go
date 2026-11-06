package worker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// TestPreviewReinitializeCountsWhatTheResetDeletes pins owner decision PR-M1:
// the Reinitialize confirm in both UIs names what the reset deletes, and the
// preview counts exactly that — the set-aside recordings in staging (one per
// restart, as A S recovers them), every staged file and its bytes (the
// set-asides included, the display-aspect record not), each split part's
// file still on disk and its bytes (a row whose file is gone counts nothing),
// and the chapters of an imported list beside the parts. A finalize's own
// list, which carries no original, is not an import and counts nothing.
//
// Then the reset itself runs and the same preview reads all zeroes: what was
// counted is what reinitializeNow removed.
//
// Mutants: the aside count dropped (SetAsideRecordings 0); the walk counting
// the display-aspect record (StagedFiles 4); the parts loop not checking the
// file on disk (RecordedParts 3); the import check dropped so a finalize's
// list counts (the finalize list case reads 1).
func TestPreviewReinitializeCountsWhatTheResetDeletes(t *testing.T) {
	w, db := testWorkerSetup(t)
	stagingDir, outputDir := muxFixtureJob(t, w, db, "j-pre")
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })

	writeAsidePair(t, stagingDir, "1600000000", 100, false, true) // one restart: video + audio halves
	if err := os.WriteFile(filepath.Join(stagingDir, "video.mp4"), make([]byte, 30), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagingDir, displayAspectFile), []byte("16:9"), 0o644); err != nil {
		t.Fatal(err)
	}

	part1 := filepath.Join(outputDir, "show - part1.mp4")
	part2 := filepath.Join(outputDir, "show - part2.mp4")
	for _, p := range []string{part1, part2} {
		if err := os.WriteFile(p, make([]byte, 7), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i, p := range []string{part1, part2, filepath.Join(outputDir, "show - part3.mp4")} {
		if err := db.AddSegment(&database.Segment{JobID: "j-pre", SegmentIndex: i, Filename: filepath.Base(p), FilePath: p}); err != nil {
			t.Fatal(err)
		}
	}
	list := ArchiveSidecarPath(part1, utils.ChaptersSidecarSuffix)
	if err := utils.WriteImportedChaptersSidecar(list, []utils.Chapter{
		{StartMs: 0, EndMs: 5000, Title: "Intro"}, {StartMs: 5000, EndMs: 9000, Title: "Game"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	db.UpdateJobFields("j-pre", map[string]any{"status": database.StatusError, "output_file": part1})

	job, _ := db.GetJob("j-pre")
	got := PreviewReinitialize(db, stagingBase, job)
	want := ReinitializePreview{
		SetAsideRecordings: 1,
		RecordedParts:      2, RecordedPartBytes: 14,
		StagedFiles: 3, StagedBytes: 230,
		ImportedChapters: 2,
	}
	if got != want {
		t.Errorf("preview = %+v, want %+v", got, want)
	}

	// A finalize's own list is no import.
	if err := utils.WriteChaptersSidecar(list, []utils.Chapter{{StartMs: 0, EndMs: 9000, Title: "Auto"}}); err != nil {
		t.Fatal(err)
	}
	if got := PreviewReinitialize(db, stagingBase, job); got.ImportedChapters != 0 {
		t.Errorf("a finalize's chapter list counted as %d imported chapters, want 0", got.ImportedChapters)
	}
	if err := utils.WriteImportedChaptersSidecar(list, []utils.Chapter{{StartMs: 0, EndMs: 9000, Title: "Intro"}}, nil); err != nil {
		t.Fatal(err)
	}

	w.reinitializeNow("j-pre")
	job, _ = db.GetJob("j-pre")
	if after := PreviewReinitialize(db, stagingBase, job); after != (ReinitializePreview{}) {
		t.Errorf("preview after the reset = %+v, want nothing left to delete", after)
	}
}

// TestReinitializePreviewSummary pins the confirm clause both UIs show
// (the dashboard's reinitializeDeletesText is its twin, pinned in
// web/tests/reinitialize-confirm.test.mjs): each kind named only when there
// is some, singular for one, sizes beside the files, the last joined by
// "and", and a reset with nothing to lose says so. Add sums a batch.
//
// Mutants: the plural helper always plural ("1 set-aside recordings"); the
// empty case dropped ("deletes "); Add skipping a field.
func TestReinitializePreviewSummary(t *testing.T) {
	for _, tc := range []struct {
		p    ReinitializePreview
		want string
	}{
		{ReinitializePreview{}, "deletes nothing recorded"},
		{ReinitializePreview{SetAsideRecordings: 1}, "deletes 1 set-aside recording"},
		{ReinitializePreview{SetAsideRecordings: 2, RecordedParts: 3, RecordedPartBytes: 1536, StagedFiles: 1, StagedBytes: 100, ImportedChapters: 5},
			"deletes 2 set-aside recordings, 3 recorded parts (1.5KB), 1 staged file (100B) and 5 imported chapters"},
		{ReinitializePreview{StagedFiles: 4, StagedBytes: 2048, ImportedChapters: 1}, "deletes 4 staged files (2.0KB) and 1 imported chapter"},
	} {
		if got := tc.p.Summary(); got != tc.want {
			t.Errorf("Summary(%+v) = %q, want %q", tc.p, got, tc.want)
		}
	}
	var sum ReinitializePreview
	sum.Add(ReinitializePreview{1, 2, 3, 4, 5, 6})
	sum.Add(ReinitializePreview{1, 1, 1, 1, 1, 1})
	if sum != (ReinitializePreview{2, 3, 4, 5, 6, 7}) {
		t.Errorf("Add = %+v", sum)
	}
}
