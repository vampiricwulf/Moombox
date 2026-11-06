package worker

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// Owner decision CJ-2: a re-finalize of the same recording keeps a chapter
// import — its list, refitted to the new archive, and the original it kept —
// where a new recording (a Reinitialize) still drops it.

// readImport reads path as an import's sidecar, failing the test when it is
// not one.
func readImport(t *testing.T, path string) *utils.ImportedChapters {
	t.Helper()
	imp, imported, err := utils.ReadImportedChaptersSidecar(path)
	if err != nil {
		t.Fatal(err)
	}
	if !imported {
		t.Fatalf("%s is not an import's list (missing, or its original dropped)", filepath.Base(path))
	}
	return imp
}

// TestMuxActionKeepsAnImportedChapterList is the Mux action over an Error row
// whose single-file archive was imported: the re-mux of its staging is the
// same recording, so the archive it writes embeds the imported list,
// refitted to the new length (the import was fitted to a 10 s file; the
// staging muxes to 4 s, so its last chapter is cut at 4 s), and the sidecar
// keeps the list and its original. Before CJ-2 the finalize dropped the
// sidecar and embedded the automatic list (none for this job).
//
// Mutants: MuxJob not marking its finalize a re-finalize (Refinalize unset:
// the sidecar is dropped and the archive carries no chapters);
// prepareArchiveChapters ignoring the kept import (the archive carries no
// chapters); the kept list written without its original (not an import's
// list); the list not refitted (Song still ends at 10 s).
func TestMuxActionKeepsAnImportedChapterList(t *testing.T) {
	ffmpegPath, ffprobePath := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)
	staging, outputDir := muxFixtureJob(t, w, db, "j-imported")
	writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 4)

	// The archive the import was saved beside, named on the row.
	oldArchive := filepath.Join(outputDir, "imported.mp4")
	if err := os.WriteFile(oldArchive, []byte("an earlier archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	sidecar := ArchiveSidecarPath(oldArchive, utils.ChaptersSidecarSuffix)
	imported := []utils.Chapter{{StartMs: 0, EndMs: 1000, Title: "Intro"}, {StartMs: 1000, EndMs: 10_000, Title: "Song"}}
	original := []utils.SidecarChapter{{Start: 0, End: 10, Title: "Automatic"}}
	if err := utils.WriteImportedChaptersSidecar(sidecar, imported, original); err != nil {
		t.Fatal(err)
	}
	db.UpdateJobFields("j-imported", map[string]any{"status": database.StatusError, "output_file": oldArchive})

	if err := w.MuxJob("j-imported"); err != nil {
		t.Fatalf("MuxJob: %v", err)
	}
	w.Stop()

	job, _ := db.GetJob("j-imported")
	if job == nil || job.Status != database.StatusFinished {
		t.Fatalf("job after the Mux action = %v, want Finished (error=%q)", statusOf(job), errorOf(job))
	}
	kept := readImport(t, ArchiveSidecarPath(job.OutputFile, utils.ChaptersSidecarSuffix))
	if len(kept.Chapters) != 2 || kept.Chapters[0].Title != "Intro" || kept.Chapters[1].Title != "Song" {
		t.Fatalf("kept list = %+v, want Intro and Song", kept.Chapters)
	}
	if end := kept.Chapters[1].End; end < 3 || end > 5 {
		t.Errorf("Song ends at %.3fs, want the new archive's ~4s end", end)
	}
	if len(kept.Original) != 1 || kept.Original[0].Title != "Automatic" {
		t.Errorf("kept original = %+v, want the import's original", kept.Original)
	}
	shape, err := probeArchiveShape(context.Background(), ffprobePath, job.OutputFile)
	if err != nil {
		t.Fatal(err)
	}
	if shape.chapters != 2 {
		t.Errorf("the re-muxed archive embeds %d chapters, want the import's 2", shape.chapters)
	}
	if job.OutputFile != oldArchive && fileExists(sidecar) {
		t.Errorf("the list under the archive's old name is still there")
	}
}

// TestReinitializeDropsAnImportedChapterList: a Reinitialize is a new
// recording whose timeline can differ, so the import beside the single-file
// archive goes when the row is reset — not only at the re-download's
// finalize, since a re-download Cancelled and then muxed by the Mux action is
// a re-finalize, which would otherwise carry the old list onto it.
//
// Mutant: reinitializeNow not dropping the import (the list is still there).
func TestReinitializeDropsAnImportedChapterList(t *testing.T) {
	w, db := testWorkerSetup(t)
	_, outputDir := muxFixtureJob(t, w, db, "j-reinit")
	archive := filepath.Join(outputDir, "reinit.mp4")
	if err := os.WriteFile(archive, []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	sidecar := ArchiveSidecarPath(archive, utils.ChaptersSidecarSuffix)
	if err := utils.WriteImportedChaptersSidecar(sidecar, []utils.Chapter{{StartMs: 0, EndMs: 5000, Title: "Intro"}}, nil); err != nil {
		t.Fatal(err)
	}
	db.UpdateJobFields("j-reinit", map[string]any{"status": database.StatusFinished, "output_file": archive})

	w.ReinitializeJob("j-reinit")

	if fileExists(sidecar) {
		t.Error("the imported chapter list survived a Reinitialize")
	}
	if job, _ := db.GetJob("j-reinit"); job == nil || job.OutputFile != "" {
		t.Errorf("row after Reinitialize = %+v, want output_file cleared", job)
	}
}

// TestSplitRefinalizeKeepsAnImportedChapterList: a split archive's
// re-finalize by MuxJob (Refinalize) keeps the import beside its parts —
// refitted to the parts' timeline as it now stands (the import was fitted to
// 500 s; the parts now run 600 s, so its last chapter runs to 600 s) and with
// its original — where it used to write the automatic list over it. A
// finalize that is not a re-finalize writes its own list, as before.
//
// Mutants: writeSplitArchiveChapters not keeping the import (Just Chatting
// and Elden Ring replace it); keptImportFor ignoring Refinalize (the
// download finalize keeps it too).
func TestSplitRefinalizeKeepsAnImportedChapterList(t *testing.T) {
	w, db := testWorkerSetup(t)
	o := w.orchestrator
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	job := &database.Job{ID: "tw_split_imp", VideoID: "tw_split_imp", URL: "https://twitch.tv/streamer",
		Platform: "twitch", Status: database.StatusMuxing, StreamStartTime: start.Format(time.RFC3339)}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	staging := t.TempDir()
	// An observed timeline, so the automatic list is not empty: it is what a
	// re-finalize that did not keep the import would write.
	tl := `{"recordingStartMs":` + strconv.FormatInt(start.UnixMilli(), 10) +
		`,"categories":[{"atMs":` + strconv.FormatInt(start.Add(2*time.Second).UnixMilli(), 10) + `,"title":"Just Chatting"},` +
		`{"atMs":` + strconv.FormatInt(start.Add(10*time.Minute).UnixMilli(), 10) + `,"title":"Elden Ring"}]}`
	if err := os.WriteFile(filepath.Join(staging, twitchTimelineFile), []byte(tl), 0o644); err != nil {
		t.Fatal(err)
	}
	var segments []database.Segment
	for i, sp := range []struct{ at, dur int64 }{{0, 300}, {600, 300}} {
		name := "Show - part" + strconv.Itoa(i+1) + ".mp4"
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte("part"), 0o644); err != nil {
			t.Fatal(err)
		}
		segments = append(segments, database.Segment{SegmentIndex: i, Filename: name, FilePath: p,
			UnixStart: start.Unix() + sp.at, DurationSeconds: float64(sp.dur)})
	}
	sidecar := filepath.Join(root, "Show"+utils.ChaptersSidecarSuffix)
	writeImport := func() {
		t.Helper()
		if err := utils.WriteImportedChaptersSidecar(sidecar,
			[]utils.Chapter{{StartMs: 0, EndMs: 120_000, Title: "Intro"}, {StartMs: 120_000, EndMs: 500_000, Title: "Raid"}},
			[]utils.SidecarChapter{{Start: 0, End: 500, Title: "Before"}}); err != nil {
			t.Fatal(err)
		}
	}
	writeImport()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // no network for copyAssets' thumbnail fallback

	jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, StagingDir: staging,
		OutputDir: root, Filename: "Show", Logger: &discardLogger{}, Refinalize: true}
	if err := o.finalizeMultiSegmentJob(ctx, jobCtx, segments); err != nil {
		t.Fatalf("re-finalize: %v", err)
	}
	kept := readImport(t, sidecar)
	wantList := []utils.SidecarChapter{{Start: 0, End: 120, Title: "Intro"}, {Start: 120, End: 600, Title: "Raid"}}
	if !reflect.DeepEqual(kept.Chapters, wantList) {
		t.Errorf("kept list = %+v, want %+v", kept.Chapters, wantList)
	}
	wantOrig := []utils.SidecarChapter{{Start: 0, End: 600, Title: "Before"}}
	if !reflect.DeepEqual(kept.Original, wantOrig) {
		t.Errorf("kept original = %+v, want %+v", kept.Original, wantOrig)
	}

	// The download pipeline's finalize is not a re-finalize: its own list.
	writeImport()
	db.UpdateJobFields(job.ID, map[string]any{"status": database.StatusMuxing})
	jobCtx.Refinalize = false
	if err := o.finalizeMultiSegmentJob(ctx, jobCtx, segments); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if _, imported, _ := utils.ReadImportedChaptersSidecar(sidecar); imported {
		t.Error("a finalize that is not a re-finalize kept the import")
	}
}
