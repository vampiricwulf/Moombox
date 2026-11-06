package worker

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
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
	if err := o.finalizeMultiSegmentJob(ctx, jobCtx, segments, nil); err != nil {
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
	if err := o.finalizeMultiSegmentJob(ctx, jobCtx, segments, nil); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if _, imported, _ := utils.ReadImportedChaptersSidecar(sidecar); imported {
		t.Error("a finalize that is not a re-finalize kept the import")
	}
}

// Owner decision CK-1: Resume and Retry continue the same recording, so the
// download run they start finalizes as a re-finalize (runJobContext,
// continuesRecording) and keeps an imported list as the Mux action does;
// only a Reinitialize — a new recording — drops it, and a fresh row's first
// download is never a re-finalize.

// TestProcessJobBuildsTheRunContext pins that processJob's download runs on
// runJobContext — the context that says whether the run continues the
// recording — and not on a bare buildJobContext, which would make every
// Resume's finalize a new recording's again. Driving processJob needs a
// platform to extract from, so this asserts on the source; the behaviour of
// the context it builds is pinned below.
//
// Mutant: processJob calling buildJobContext (the call is gone).
func TestProcessJobBuildsTheRunContext(t *testing.T) {
	src, err := os.ReadFile("worker.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func (w *DownloadWorker) processJob(")
	if start < 0 {
		t.Fatal("processJob not found")
	}
	body = body[start:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "jobCtx := w.runJobContext(job)") {
		t.Error("processJob does not build its JobContext with runJobContext: a Resume's finalize drops the import again")
	}
	if strings.Contains(body, "w.buildJobContext(") {
		t.Error("processJob builds a JobContext with buildJobContext, which never marks a re-finalize")
	}
}

// TestRunContextContinuesTheRecording is the verdict per way a run starts:
// a fresh row's first download is a new recording; a Resume (the operator's,
// or the monitor's auto-resume — both ResumeJob) of a row whose archive was
// finalized continues it, as does a retry that leaves the row as it was (a
// parked COOKIES? row re-admitted, a requeued backlog VOD: the row's status
// moves and nothing else) and a split row that has recorded parts but never
// finalized; a Reinitialize is a new recording; an auto-reinitialize keeps
// the same broadcast's parts and continues it.
//
// Mutants: continuesRecording ignoring output_file (the resumed and retried
// single files are new recordings); ignoring the parts (the split rows are);
// answering true always (the fresh row and the reinitialized one continue);
// runJobContext not setting Refinalize.
func TestRunContextContinuesTheRecording(t *testing.T) {
	w, db := testWorkerSetup(t)
	add := func(id string, fields map[string]any) {
		t.Helper()
		if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, URL: "u", Platform: "youtube", Status: database.StatusUpcoming}); err != nil {
			t.Fatal(err)
		}
		if len(fields) > 0 {
			db.UpdateJobFields(id, fields)
		}
	}
	addPart := func(id string) {
		t.Helper()
		if err := db.AddSegment(&database.Segment{JobID: id, SegmentIndex: 0, Filename: "x - part1.mp4", FilePath: filepath.Join(t.TempDir(), "x - part1.mp4"), DurationSeconds: 60}); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(t.TempDir(), "x.mp4")

	add("fresh", nil)
	add("resumed", map[string]any{"status": database.StatusError, "output_file": archive})
	w.ResumeJob("resumed")
	add("retried", map[string]any{"status": database.StatusCookies, "output_file": archive})
	db.UpdateJobFields("retried", map[string]any{"status": database.StatusUpcoming}) // re-admitted
	add("split-resumed", map[string]any{"status": database.StatusCancelled})
	addPart("split-resumed")
	w.ResumeJob("split-resumed")
	add("reinit", map[string]any{"status": database.StatusError, "output_file": archive})
	addPart("reinit")
	w.ReinitializeJob("reinit")
	add("auto-reinit", map[string]any{"status": database.StatusError, "output_file": archive})
	addPart("auto-reinit")
	w.AutoReinitializeJob("auto-reinit")

	for id, want := range map[string]bool{
		"fresh": false, "resumed": true, "retried": true, "split-resumed": true,
		"reinit": false, "auto-reinit": true,
	} {
		job, err := db.GetJob(id)
		if err != nil || job == nil {
			t.Fatalf("%s: %v", id, err)
		}
		if got := w.runJobContext(job).Refinalize; got != want {
			t.Errorf("%s: Refinalize = %v, want %v", id, got, want)
		}
	}
}

// TestResumeKeepsAnImportedChapterList is CK-1 end to end for a single
// file: a Finished archive was imported, the operator resumed the row (its
// download had stopped short), and the resumed run's finalize — the
// download pipeline's muxAndFinalize, on the context processJob builds —
// embeds the import refitted to the new 4 s archive and keeps it with its
// original, where before CK-1 it dropped it. A Reinitialize of the same row
// drops it, and the re-download's finalize embeds none of it.
//
// Mutants: runJobContext not marking the resumed run (the sidecar is
// dropped and the archive carries no chapters); reinitializeNow not
// dropping the import (the list is still there after the reset).
func TestResumeKeepsAnImportedChapterList(t *testing.T) {
	ffmpegPath, ffprobePath := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)
	staging, outputDir := muxFixtureJob(t, w, db, "j-resume")
	writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 4)

	oldArchive := filepath.Join(outputDir, "resumed.mp4")
	if err := os.WriteFile(oldArchive, []byte("an earlier archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	sidecar := ArchiveSidecarPath(oldArchive, utils.ChaptersSidecarSuffix)
	imported := []utils.Chapter{{StartMs: 0, EndMs: 1000, Title: "Intro"}, {StartMs: 1000, EndMs: 10_000, Title: "Song"}}
	if err := utils.WriteImportedChaptersSidecar(sidecar, imported, []utils.SidecarChapter{{Start: 0, End: 10, Title: "Automatic"}}); err != nil {
		t.Fatal(err)
	}
	db.UpdateJobFields("j-resume", map[string]any{"status": database.StatusFinished, "incomplete_tail": true, "output_file": oldArchive})

	w.ResumeJob("j-resume")
	job, _ := db.GetJob("j-resume")
	jobCtx := w.runJobContext(job)
	if err := w.orchestrator.muxAndFinalize(context.Background(), jobCtx, &DownloadResult{HasVideo: true, VideoPath: filepath.Join(staging, "video.mp4")}); err != nil {
		t.Fatalf("the resumed run's finalize: %v", err)
	}
	job, _ = db.GetJob("j-resume")
	if job == nil || job.Status != database.StatusFinished {
		t.Fatalf("job after the resumed run = %v, want Finished", statusOf(job))
	}
	kept := readImport(t, ArchiveSidecarPath(job.OutputFile, utils.ChaptersSidecarSuffix))
	if len(kept.Chapters) != 2 || kept.Chapters[1].Title != "Song" || kept.Chapters[1].End < 3 || kept.Chapters[1].End > 5 {
		t.Errorf("kept list = %+v, want Intro and Song, Song refitted to the ~4s archive", kept.Chapters)
	}
	if len(kept.Original) != 1 || kept.Original[0].Title != "Automatic" {
		t.Errorf("kept original = %+v, want the import's original", kept.Original)
	}
	shape, err := probeArchiveShape(context.Background(), ffprobePath, job.OutputFile)
	if err != nil {
		t.Fatal(err)
	}
	if shape.chapters != 2 {
		t.Errorf("the resumed run's archive embeds %d chapters, want the import's 2", shape.chapters)
	}

	// A Reinitialize is a new recording: the import goes with the reset.
	keptPath := ArchiveSidecarPath(job.OutputFile, utils.ChaptersSidecarSuffix)
	db.UpdateJobFields("j-resume", map[string]any{"status": database.StatusError})
	w.ReinitializeJob("j-resume")
	if fileExists(keptPath) {
		t.Error("the imported chapter list survived a Reinitialize")
	}
	job, _ = db.GetJob("j-resume")
	if w.runJobContext(job).Refinalize {
		t.Error("the re-download after a Reinitialize finalizes as a re-finalize")
	}
}

// TestSplitResumeKeepsTheImportBesideTheRowsArchive: a split archive's
// import kept by a Resume's finalize is read only from beside the archive
// the row named when the run began. Here a Finished single file ("Old.mp4",
// no parts) was resumed, and the resumed run split into parts under a name
// the template now resolves afresh ("New"), where a list another recording
// left sits; the row's own import, beside Old.mp4, is the one kept —
// refitted to the parts and moved beside them — and the foreign list is not
// mistaken for it.
//
// Mutants: writeSplitArchiveChapters reading beside the parts rather than
// the row's archive (the foreign list is kept); continuesRecording ignoring
// output_file (this split run, whose row has no parts yet, writes the
// automatic list over everything).
func TestSplitResumeKeepsTheImportBesideTheRowsArchive(t *testing.T) {
	w, db := testWorkerSetup(t)
	o := w.orchestrator
	root := t.TempDir()
	oldPart := filepath.Join(root, "Old.mp4")
	job := &database.Job{ID: "yt_split_resume", VideoID: "yt_split_resume", URL: "u", Platform: "youtube", Status: database.StatusError}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	db.UpdateJobFields(job.ID, map[string]any{"output_file": oldPart})
	ownSidecar := ArchiveSidecarPath(oldPart, utils.ChaptersSidecarSuffix)
	if err := utils.WriteImportedChaptersSidecar(ownSidecar,
		[]utils.Chapter{{StartMs: 0, EndMs: 60_000, Title: "Own A"}, {StartMs: 60_000, EndMs: 300_000, Title: "Own B"}},
		[]utils.SidecarChapter{{Start: 0, End: 300, Title: "Own original"}}); err != nil {
		t.Fatal(err)
	}
	foreignSidecar := filepath.Join(root, "New"+utils.ChaptersSidecarSuffix)
	if err := utils.WriteImportedChaptersSidecar(foreignSidecar,
		[]utils.Chapter{{StartMs: 0, EndMs: 600_000, Title: "Foreign"}}, []utils.SidecarChapter{}); err != nil {
		t.Fatal(err)
	}

	w.ResumeJob(job.ID)
	fresh, _ := db.GetJob(job.ID)
	jobCtx := w.runJobContext(fresh)
	jobCtx.StagingDir, jobCtx.OutputDir, jobCtx.Filename = t.TempDir(), root, "New"
	var segments []database.Segment
	for i := range 2 {
		name := "New - part" + strconv.Itoa(i+1) + ".mp4"
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte("part"), 0o644); err != nil {
			t.Fatal(err)
		}
		segments = append(segments, database.Segment{SegmentIndex: i, Filename: name, FilePath: p, DurationSeconds: 200})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // no network for copyAssets' thumbnail fallback
	o.writeSplitArchiveChapters(ctx, jobCtx, segments)

	kept := readImport(t, foreignSidecar)
	want := []utils.SidecarChapter{{Start: 0, End: 60, Title: "Own A"}, {Start: 60, End: 400, Title: "Own B"}}
	if !reflect.DeepEqual(kept.Chapters, want) {
		t.Errorf("list beside the parts = %+v, want the row's own import refitted to 400 s: %+v", kept.Chapters, want)
	}
	if len(kept.Original) != 1 || kept.Original[0].Title != "Own original" {
		t.Errorf("kept original = %+v, want the row's own", kept.Original)
	}
	if fileExists(ownSidecar) {
		t.Error("the import is still under the row's old name as well")
	}
}

// TestSplitRefinalizeReadsTheImportWhereTheRouteSavedIt: a resumed single
// file ("Old.mp4") whose resumed run split into parts under a name the
// template now resolves afresh ("New - partN") and then stopped short keeps
// output_file at Old.mp4 with those parts recorded. The import route saves
// such a row's import beside its first part (routes.splitSidecarPath), so
// the next re-finalize — the Mux action's or a Resume's — reads it there,
// not beside output_file, where only an older list (the one the import
// replaced) may sit.
//
// Mutants: writeSplitArchiveChapters reading beside output_file ahead of
// the row's recorded parts (the operator's import is written over);
// buildJobContext not recording the first part (the same).
func TestSplitRefinalizeReadsTheImportWhereTheRouteSavedIt(t *testing.T) {
	w, db := testWorkerSetup(t)
	o := w.orchestrator
	root := t.TempDir()
	old := filepath.Join(root, "Old.mp4")
	job := &database.Job{ID: "yt_split_route", VideoID: "yt_split_route", URL: "u", Platform: "youtube", Status: database.StatusCancelled}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	db.UpdateJobFields(job.ID, map[string]any{"output_file": old})
	var segments []database.Segment
	for i := range 2 {
		name := "New - part" + strconv.Itoa(i+1) + ".mp4"
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte("part"), 0o644); err != nil {
			t.Fatal(err)
		}
		seg := database.Segment{JobID: job.ID, SegmentIndex: i, Filename: name, FilePath: p, DurationSeconds: 200}
		if err := db.AddSegment(&seg); err != nil {
			t.Fatal(err)
		}
		segments = append(segments, seg)
	}
	stale := ArchiveSidecarPath(old, utils.ChaptersSidecarSuffix)
	if err := utils.WriteImportedChaptersSidecar(stale,
		[]utils.Chapter{{StartMs: 0, EndMs: 300_000, Title: "Stale"}}, []utils.SidecarChapter{}); err != nil {
		t.Fatal(err)
	}
	// Where the route saves a split row's import: beside its first part.
	routes := ArchiveSidecarPath(segments[0].FilePath, utils.ChaptersSidecarSuffix)
	if err := utils.WriteImportedChaptersSidecar(routes,
		[]utils.Chapter{{StartMs: 0, EndMs: 100_000, Title: "Imported A"}, {StartMs: 100_000, EndMs: 400_000, Title: "Imported B"}},
		[]utils.SidecarChapter{{Start: 0, End: 400, Title: "Automatic"}}); err != nil {
		t.Fatal(err)
	}

	fresh, _ := db.GetJob(job.ID)
	jobCtx := w.runJobContext(fresh)
	if !jobCtx.Refinalize {
		t.Fatal("a row with recorded parts does not continue its recording")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // no network for copyAssets' thumbnail fallback
	o.writeSplitArchiveChapters(ctx, jobCtx, segments)

	kept := readImport(t, routes) // fails when the automatic list replaced the import
	want := []utils.SidecarChapter{{Start: 0, End: 100, Title: "Imported A"}, {Start: 100, End: 400, Title: "Imported B"}}
	if !reflect.DeepEqual(kept.Chapters, want) {
		t.Errorf("list beside the parts = %+v, want the operator's import: %+v", kept.Chapters, want)
	}
}
