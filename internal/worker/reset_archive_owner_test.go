package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// dirNames is the names in dir, sorted, joined with "|".
func dirNames(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return strings.Join(names, "|")
}

// Two jobs whose names resolve alike can overlap: the second starts later and
// finishes first, so its archive is NEWER than the first one's run. The first
// one's finalize must not take that for its own interrupted finalize's file:
// a file another row names is that row's whatever its age. The single-file
// finalize is refused, and a job's only part keeps its part name.
//
// Mutant: the other rows asked only about a file older than the run (the
// guard returns nil and the part is renamed over the other job's archive).
func TestFinalizeRefusesANewerFileAnotherRowNames(t *testing.T) {
	w, db := testWorkerSetup(t)
	root := t.TempDir()
	job := &database.Job{ID: "j-first", VideoID: "j-first", URL: "https://twitch.tv/s", Platform: "twitch", Status: database.StatusDownloading}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	db.UpdateJobFields(job.ID, map[string]any{"download_started_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)})
	archive := filepath.Join(root, "Show.mp4")
	writeTestFile(t, archive, "the second job's recording")
	if _, err := db.AddJob(&database.Job{ID: "j-second", VideoID: "j-second", URL: "u", Platform: "twitch",
		Status: database.StatusFinished, OutputFile: archive}); err != nil {
		t.Fatal(err)
	}

	err := w.orchestrator.setForeignFileAside("j-first", archive, archiveSidecarExts)
	if err == nil || !strings.Contains(err.Error(), "belongs to job j-second") {
		t.Errorf("the single-file finalize's check = %v, want a refusal naming job j-second", err)
	}

	seg := database.Segment{JobID: job.ID, SegmentIndex: 0, Filename: "Show - part1.mp4",
		FilePath: filepath.Join(root, "Show - part1.mp4"), Quality: "720p"}
	writeTestFile(t, seg.FilePath, "the first job's part")
	if err := db.AddSegment(&seg); err != nil {
		t.Fatal(err)
	}
	jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, StagingDir: t.TempDir(),
		OutputDir: root, Filename: "Show", Logger: &discardLogger{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.orchestrator.finalizeMultiSegmentJob(ctx, jobCtx, []database.Segment{seg}, nil); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if got := readTestFile(t, archive); got != "the second job's recording" {
		t.Errorf("the second job's archive now reads %q", got)
	}
	if got := readTestFile(t, seg.FilePath); got != "the first job's part" {
		t.Errorf("the first job's part reads %q under its part name", got)
	}
}

// An archive kept in place under one job's marker (its reset could not rename
// it) is that job's. Another job whose name resolves to it does not set it
// aside under its own marker: it is refused, and the file and the marker stay
// as they were.
//
// Mutant: the marker not asked about (the archive is renamed under a marker
// naming the finishing job, and is offered by Files once that job is deleted
// while the job that kept it still exists).
func TestFinalizeRefusesAFileAnotherJobKept(t *testing.T) {
	w, db := testWorkerSetup(t)
	outputDir, archive := archivedErrorJob(t, w, db, "j-keeper")
	failRenameArchive(t)
	w.reinitializeNow("j-keeper", database.StatusError)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(archive, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddJob(&database.Job{ID: "j-other", VideoID: "j-other", URL: "u", Platform: "youtube",
		Status: database.StatusMuxing, OutputDirectory: outputDir}); err != nil {
		t.Fatal(err)
	}
	db.UpdateJobFields("j-other", map[string]any{"download_started_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)})
	before := dirNames(t, outputDir)

	err := w.orchestrator.setForeignFileAside("j-other", archive, archiveSidecarExts)

	if err == nil || !strings.Contains(err.Error(), "belongs to job j-keeper") {
		t.Errorf("the check = %v, want a refusal naming job j-keeper", err)
	}
	if got := dirNames(t, outputDir); got != before {
		t.Errorf("the output directory holds %s, want it as it was: %s", got, before)
	}
	if got := asideOwnerID(archive); got != "j-keeper" {
		t.Errorf("the archive's marker names %q", got)
	}
}

// The marker a failed reset rename left under the archive's own name keeps
// the archive too, not only what a finalize happens to set aside. A new
// recording that ends as two parts with a whole-job chat sets the old chat
// aside and never looks at the old archive's name: the marker stays while the
// archive is there, so Files does not offer it.
//
// Mutant: the marker removed whenever a set-aside left nothing behind (the
// old archive is offered while its job exists and is Finished).
func TestInPlaceMarkerStaysWhileTheArchiveItKeepsIsThere(t *testing.T) {
	w, db := testWorkerSetup(t)
	root := t.TempDir()
	started := time.Now().Add(-time.Hour)
	job := &database.Job{ID: "j-split", URL: "u", Platform: "youtube", Status: database.StatusDownloading, OutputDirectory: root}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	db.UpdateJobFields(job.ID, map[string]any{"download_started_at": started.UTC().Format(time.RFC3339)})
	writeOldFile(t, filepath.Join(root, "Show.mp4"), "the earlier recording", started.Add(-time.Hour))
	writeOldFile(t, filepath.Join(root, "Show.chat.json"), "the earlier chat", started.Add(-time.Hour))
	if _, err := writeAsideOwner(filepath.Join(root, "Show"), job.ID); err != nil {
		t.Fatal(err)
	}
	var segs []database.Segment
	for i, n := range []string{"Show - part1.mp4", "Show - part2.mp4"} {
		s := database.Segment{JobID: job.ID, SegmentIndex: i, Filename: n, FilePath: filepath.Join(root, n), Quality: "720p"}
		writeTestFile(t, s.FilePath, n)
		if err := db.AddSegment(&s); err != nil {
			t.Fatal(err)
		}
		segs = append(segs, s)
	}
	staging := t.TempDir()
	writeTestFile(t, filepath.Join(staging, "chat.json"), "the new chat")
	jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, StagingDir: staging,
		OutputDir: root, Filename: "Show", Logger: &discardLogger{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := w.orchestrator.finalizeMultiSegmentJob(ctx, jobCtx, segs, nil); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	if got := readTestFile(t, soleSetAside(t, root, ".chat.json")); got != "the earlier chat" {
		t.Errorf("the set-aside chat reads %q", got)
	}
	if got := readTestFile(t, filepath.Join(root, "Show.mp4")); got != "the earlier recording" {
		t.Errorf("the kept archive reads %q", got)
	}
	if got := offeredOutput(t, db, root); len(got) != 0 {
		t.Errorf("the Files sweep offers %v while the job exists", got)
	}
}

// The chat copy does not write the capture's raw sidecar over one it did not
// write that has no chat beside it (a reset moved the chat, and the raw could
// not follow): the raw is set aside, and when it cannot be it is kept and the
// capture stays in staging, as when the copy fails.
//
// Mutants: the raw's name not asked about (the earlier raw is replaced); a
// failed rename ignored (replaced after all, with the chat read as complete).
func TestFinalizeSetsAsideARawSidecarWithNoChat(t *testing.T) {
	setup := func(t *testing.T, jobID string) (o *DownloadOrchestrator, jobCtx *JobContext, raw string) {
		w, db := testWorkerSetup(t)
		stagingDir, outputDir := muxFixtureJob(t, w, db, jobID)
		started := time.Now().Add(-time.Hour)
		raw = filepath.Join(outputDir, "Show.chat.raw.jsonl")
		writeOldFile(t, raw, "the earlier raw", started.Add(-time.Hour))
		if _, err := writeAsideOwner(filepath.Join(outputDir, "Show"), jobID); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(stagingDir, "chat.json"), "the new chat")
		writeTestFile(t, filepath.Join(stagingDir, "chat.raw.jsonl"), "the new raw")
		job := db.UpdateJobFields(jobID, map[string]any{"download_started_at": started.UTC().Format(time.RFC3339)})
		job.VideoID = ""
		return w.orchestrator, &JobContext{Job: job, DB: db, Config: &JobConfig{}, StagingDir: stagingDir,
			OutputDir: outputDir, Filename: "Show", Logger: &discardLogger{}}, raw
	}

	t.Run("set aside", func(t *testing.T) {
		o, jobCtx, raw := setup(t, "j-raw")
		updates := map[string]any{}
		o.copyAssets(context.Background(), jobCtx, jobCtx.OutputDir, "Show", "Show", updates, false)
		if got := readTestFile(t, soleSetAside(t, jobCtx.OutputDir, ".chat.raw.jsonl")); got != "the earlier raw" {
			t.Errorf("the set-aside raw reads %q", got)
		}
		if got := readTestFile(t, raw); got != "the new raw" {
			t.Errorf("the raw beside the archive reads %q, want the capture's", got)
		}
		if updates["chat_status"] == database.ChatIncomplete {
			t.Error("the chat reads incomplete though both files were copied")
		}
	})

	t.Run("cannot be set aside", func(t *testing.T) {
		o, jobCtx, raw := setup(t, "j-raw-locked")
		failRenameArchive(t)
		updates := map[string]any{}
		o.copyAssets(context.Background(), jobCtx, jobCtx.OutputDir, "Show", "Show", updates, false)
		if got := readTestFile(t, raw); got != "the earlier raw" {
			t.Errorf("the kept raw reads %q", got)
		}
		if updates["chat_status"] != database.ChatIncomplete {
			t.Errorf("chat_status = %v, want incomplete so the capture stays in staging", updates["chat_status"])
		}
	})
}

// A reset marks the recovered set-aside recordings the row owned by stem as
// the job's when there is no archive to set aside as well: a split job, whose
// parts the operator's reset deletes, and a row whose archive file is gone.
//
// Mutant: the adoption left inside setArchiveAside, which returns at once
// without an archive (Files offers the recovered footage while the job
// exists).
func TestResetKeepsRecoveredRecordingsWithNoArchiveToSetAside(t *testing.T) {
	w, db := testWorkerSetup(t)

	t.Run("split job", func(t *testing.T) {
		_, outputDir := muxFixtureJob(t, w, db, "j-parts")
		first := filepath.Join(outputDir, "Show - part1.mp4")
		for i, n := range []string{"Show - part1.mp4", "Show - part2.mp4"} {
			p := filepath.Join(outputDir, n)
			writeTestFile(t, p, n)
			if err := db.AddSegment(&database.Segment{JobID: "j-parts", SegmentIndex: i, Filename: n, FilePath: p}); err != nil {
				t.Fatal(err)
			}
		}
		writeTestFile(t, filepath.Join(outputDir, "Show.restart-1700000000.mp4"), "recovered footage")
		db.UpdateJobFields("j-parts", map[string]any{"status": database.StatusError, "output_file": first, "filename": "Show"})
		if got := offeredOutput(t, db, outputDir); len(got) != 0 {
			t.Fatalf("before the reset the sweep offers %v", got)
		}
		w.reinitializeNow("j-parts", database.StatusError)
		if got := dirNames(t, outputDir); got != "Show.restart-1700000000.mp4|Show.restart-1700000000.owner" {
			t.Errorf("the output directory holds %s, want the recovered recording and its marker", got)
		}
		if got := offeredOutput(t, db, outputDir); len(got) != 0 {
			t.Errorf("after the reset the sweep offers %v; it is the job's", got)
		}
	})

	t.Run("archive file gone", func(t *testing.T) {
		_, outputDir := muxFixtureJob(t, w, db, "j-gone")
		writeTestFile(t, filepath.Join(outputDir, "Show.restart-1700000000.mp4"), "recovered footage")
		db.UpdateJobFields("j-gone", map[string]any{"status": database.StatusError,
			"output_file": filepath.Join(outputDir, "Show.mp4"), "filename": "Show.mp4"})
		w.reinitializeNow("j-gone", database.StatusError)
		if got := offeredOutput(t, db, outputDir); len(got) != 0 {
			t.Errorf("after the reset the sweep offers %v; it is the job's", got)
		}
	})
}

// Resume keeps output_file, so a Finished archive that was resumed, recorded
// one part under a part name and then failed is on a row with both. The
// reset sets the archive aside with its chat and description and deletes the
// part; the confirm counts one of each.
//
// Mutant: any part row taken to mean "no single-file archive" (the archive
// stays at its name on no row, the confirm does not name it, and Files offers
// it with its chat and description).
func TestReinitializeSetsAsideAnArchiveWithAPartElsewhere(t *testing.T) {
	w, db := testWorkerSetup(t)
	outputDir, _ := archivedErrorJob(t, w, db, "j-both")
	part := filepath.Join(outputDir, "Show - part1.mp4")
	writeTestFile(t, part, "part")
	if err := db.AddSegment(&database.Segment{JobID: "j-both", SegmentIndex: 0, Filename: "Show - part1.mp4", FilePath: part}); err != nil {
		t.Fatal(err)
	}
	job, _ := db.GetJob("j-both")
	if p := PreviewReinitialize(db, t.TempDir(), job); p.ExistingArchives != 1 || p.RecordedParts != 1 {
		t.Errorf("preview = %+v, want one existing archive and one recorded part", p)
	}

	w.reinitializeNow("j-both", database.StatusError)

	if got := readTestFile(t, soleSetAside(t, outputDir, ".mp4")); got != "the earlier recording" {
		t.Errorf("the set-aside archive reads %q", got)
	}
	if fileExists(part) {
		t.Error("the part is still there")
	}
	if got := offeredOutput(t, db, outputDir); len(got) != 0 {
		t.Errorf("the Files sweep offers %v while the job exists", got)
	}
}
