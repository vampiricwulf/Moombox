package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
)

// On-demand recovery for a job that already has an archive puts the sibling
// beside that archive, under its stem — where the finalize put the ones it
// recovered. It used to re-resolve the name from the current title and
// template, so a title edited after the finalize sent the sibling to a stem no
// archive has, and the output sweep offered it as a deletable stray.
//
// Mutant: the recordedArchiveLocation pin removed — the sibling takes the
// fresh name.
func TestRecoverAsidesPutsTheSiblingBesideTheRecordedArchive(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)

	staging, outputDir := muxFixtureJob(t, w, db, "j-pinned")
	archive := filepath.Join(outputDir, "Old Title [j-pinned].mp4")
	if err := os.WriteFile(archive, []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	if db.UpdateJobFields("j-pinned", map[string]any{
		"status":      database.StatusFinished,
		"title":       "A Title Edited Since",
		"output_file": archive,
	}) == nil {
		t.Fatal("UpdateJobFields returned nil — the fixture row is not there")
	}
	writeAsideFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"+engine.StagedRestartSuffix+"1700000000"), 3)

	if err := w.RecoverAsides("j-pinned"); err != nil {
		t.Fatalf("RecoverAsides: %v", err)
	}
	w.Stop()

	want := filepath.Join(outputDir, "Old Title [j-pinned]"+engine.StagedRestartSuffix+"1700000000.mp4")
	if !fileExists(want) {
		var found []string
		_ = filepath.Walk(outputDir, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				found = append(found, strings.TrimPrefix(p, outputDir))
			}
			return nil
		})
		t.Errorf("no %s beside the archive; the output dir holds %v", filepath.Base(want), found)
	}
}

// A split job's recovered sibling is named after the base its parts share
// ("X.restart-<ts>.mp4" beside "X - part1.mp4"), and nothing else in the row
// carries that stem when the job has no thumbnail or description — so the
// output sweep offered the sibling as a stray.
//
// Mutant: the part-base stem not added in scanOutputOrphans.
func TestASplitJobsRecoveredSiblingIsNotAnOrphan(t *testing.T) {
	_, db := testWorkerSetup(t)
	outputDir := t.TempDir()
	part1 := filepath.Join(outputDir, "Stream - part1.mp4")
	part2 := filepath.Join(outputDir, "Stream - part2.mp4")
	sibling := filepath.Join(outputDir, "Stream"+engine.StagedRestartSuffix+"1700000000.mp4")
	if _, err := db.AddJob(&database.Job{
		ID: "j-split", VideoID: "j-split", URL: "u", Platform: "youtube",
		Status: database.StatusFinished, OutputDirectory: outputDir, OutputFile: part1,
	}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if err := db.ReplaceJobSegments("j-split", []database.Segment{
		{JobID: "j-split", SegmentIndex: 0, Filename: filepath.Base(part1), FilePath: part1},
		{JobID: "j-split", SegmentIndex: 1, Filename: filepath.Base(part2), FilePath: part2},
	}); err != nil {
		t.Fatalf("ReplaceJobSegments: %v", err)
	}
	for _, p := range []string{part1, part2, sibling} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.MoomboxConfig{}
	cfg.Paths.OutputDirectory = outputDir
	entries, err := scanOutputOrphans(db, cfg)
	if err != nil {
		t.Fatalf("scanOutputOrphans: %v", err)
	}
	for _, e := range entries {
		t.Errorf("%s is offered as a deletable output orphan", e.RelPath)
	}
}
