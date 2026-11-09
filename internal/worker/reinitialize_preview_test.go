package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	w.reinitializeNow("j-pre", database.StatusError)
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
		// What the reset cancels (owner ruling R-D2).
		{ReinitializePreview{StagedFiles: 1, StagedBytes: 10, PendingAutoMux: 1}, "cancels the pending automatic mux, and deletes 1 staged file (10B)"},
		{ReinitializePreview{PendingAutoRetry: 1}, "cancels the pending automatic retry, and deletes nothing recorded"},
		{ReinitializePreview{PendingAutoMux: 2, PendingAutoRetry: 3}, "cancels 2 pending automatic muxes and 3 pending automatic retries, and deletes nothing recorded"},
	} {
		if got := tc.p.Summary(); got != tc.want {
			t.Errorf("Summary(%+v) = %q, want %q", tc.p, got, tc.want)
		}
	}
	var sum ReinitializePreview
	sum.Add(ReinitializePreview{1, 2, 3, 4, 5, 6, 7, 8})
	sum.Add(ReinitializePreview{1, 1, 1, 1, 1, 1, 1, 1})
	if sum != (ReinitializePreview{2, 3, 4, 5, 6, 7, 8, 9}) {
		t.Errorf("Add = %+v", sum)
	}
}

// TestPreviewReinitializeFindsEachImportedList pins the two places the
// preview looks for an imported chapter list, each on its own: beside the
// single archive a row with no part rows names (dropImportedArchiveChapters'
// case, the common one), and beside part 1 of a split archive whose row
// names no output_file — a live job that errored after its parts were
// recorded, before a finalize wrote one (removeSplitArchiveChapters' case).
// Every part of one archive resolves to the same list
// (recordedArchiveLocation strips " - partN"), so only a row naming no file
// tells the two arms apart.
//
// Mutants: the output_file arm dropped (the single-file case reads 0); the
// part-1 arm dropped (the split case reads 0).
func TestPreviewReinitializeFindsEachImportedList(t *testing.T) {
	w, db := testWorkerSetup(t)
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
	imported := func(t *testing.T, archive string, n int) {
		t.Helper()
		chs := make([]utils.Chapter, n)
		for i := range chs {
			chs[i] = utils.Chapter{StartMs: int64(i) * 1000, EndMs: int64(i+1) * 1000, Title: fmt.Sprintf("C%d", i)}
		}
		if err := utils.WriteImportedChaptersSidecar(ArchiveSidecarPath(archive, utils.ChaptersSidecarSuffix), chs, nil); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("single file", func(t *testing.T) {
		_, outputDir := muxFixtureJob(t, w, db, "j-pre-one")
		archive := filepath.Join(outputDir, "show.mp4")
		if err := os.WriteFile(archive, make([]byte, 5), 0o644); err != nil {
			t.Fatal(err)
		}
		imported(t, archive, 3)
		db.UpdateJobFields("j-pre-one", map[string]any{"status": database.StatusError, "output_file": archive})
		job, _ := db.GetJob("j-pre-one")
		if got := PreviewReinitialize(db, stagingBase, job).ImportedChapters; got != 3 {
			t.Errorf("single-file archive: ImportedChapters = %d, want 3", got)
		}
	})

	t.Run("split, no output_file", func(t *testing.T) {
		_, outputDir := muxFixtureJob(t, w, db, "j-pre-split")
		part1 := filepath.Join(outputDir, "show - part1.mp4")
		part2 := filepath.Join(outputDir, "show - part2.mp4")
		for i, p := range []string{part1, part2} {
			if err := os.WriteFile(p, make([]byte, 5), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := db.AddSegment(&database.Segment{JobID: "j-pre-split", SegmentIndex: i, Filename: filepath.Base(p), FilePath: p}); err != nil {
				t.Fatal(err)
			}
		}
		imported(t, part1, 4)
		db.UpdateJobFields("j-pre-split", map[string]any{"status": database.StatusError, "output_file": ""})
		job, _ := db.GetJob("j-pre-split")
		if got := PreviewReinitialize(db, stagingBase, job).ImportedChapters; got != 4 {
			t.Errorf("split archive: ImportedChapters = %d, want 4", got)
		}
	})
}

// TestPreviewReinitializeNamesThePendingAutomaticAction: a row waiting on the
// Twitch monitor's automatic mux or retry is counted, so both confirms say
// the reset cancels it — for the mux, by deleting the staging it would
// archive. A row with nothing pending counts neither.
//
// Mutant: PreviewReinitialize not reading jobstate.PendingAutoAction.
func TestPreviewReinitializeNamesThePendingAutomaticAction(t *testing.T) {
	base := t.TempDir()
	mux := &database.Job{ID: "tw_m", Platform: "twitch", Status: database.StatusError, Error: "twitch HLS: 500",
		ParkReason: database.ParkReasonTwitchEndUnconfirmed}
	retry := &database.Job{ID: "tw_r", Platform: "twitch", Status: database.StatusError, Error: TwitchOfflineErrMsg}
	plain := &database.Job{ID: "yt_p", Platform: "youtube", Status: database.StatusError, Error: "boom"}
	if p := PreviewReinitialize(nil, base, mux); p.PendingAutoMux != 1 || p.PendingAutoRetry != 0 ||
		!strings.HasPrefix(p.Summary(), "cancels the pending automatic mux, and ") {
		t.Errorf("mux row: %+v %q", p, p.Summary())
	}
	if p := PreviewReinitialize(nil, base, retry); p.PendingAutoRetry != 1 || p.PendingAutoMux != 0 {
		t.Errorf("retry row: %+v", p)
	}
	if p := PreviewReinitialize(nil, base, plain); p.PendingAutoMux != 0 || p.PendingAutoRetry != 0 ||
		strings.Contains(p.Summary(), "cancels") {
		t.Errorf("plain row: %+v %q", p, p.Summary())
	}
	var total ReinitializePreview
	total.Add(PreviewReinitialize(nil, base, mux))
	total.Add(PreviewReinitialize(nil, base, retry))
	if total.PendingAutoMux != 1 || total.PendingAutoRetry != 1 {
		t.Errorf("Add dropped the pending counts: %+v", total)
	}
}
