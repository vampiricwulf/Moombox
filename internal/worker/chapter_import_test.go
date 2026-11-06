package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// TestRemuxCheckIsVerifyArchiveRemux pins the seam's production value.
func TestRemuxCheckIsVerifyArchiveRemux(t *testing.T) {
	if reflect.ValueOf(remuxCheck).Pointer() != reflect.ValueOf(verifyArchiveRemux).Pointer() {
		t.Fatal("remuxCheck is not verifyArchiveRemux")
	}
}

// TestVerifyArchiveRemux pins the re-mux's output checks (owner decision
// CI-1: "the same output checks the archive copy uses (streams and
// duration)", plus the chapter count the list asked for).
//
// Mutants: each comparison removed in turn — the stream count, a stream's
// shortfall, an extra stream kind, the file's shortfall, the chapter count —
// fails its row.
func TestVerifyArchiveRemux(t *testing.T) {
	orig := &archiveShape{durationSec: 3600, chapters: 3, streams: map[string][]float64{"video": {3600}, "audio": {3600}}}
	shape := func(mut func(*archiveShape)) *archiveShape {
		s := &archiveShape{durationSec: 3600, chapters: 2, streams: map[string][]float64{"video": {3600}, "audio": {3599}}}
		if mut != nil {
			mut(s)
		}
		return s
	}
	if err := verifyArchiveRemux(orig, shape(nil), 2); err != nil {
		t.Fatalf("a faithful copy was refused: %v", err)
	}
	for name, out := range map[string]*archiveShape{
		"audio stream lost":     shape(func(s *archiveShape) { delete(s.streams, "audio") }),
		"second video stream":   shape(func(s *archiveShape) { s.streams["video"] = []float64{3600, 3600} }),
		"video cut short":       shape(func(s *archiveShape) { s.streams["video"] = []float64{1200} }),
		"a new subtitle stream": shape(func(s *archiveShape) { s.streams["subtitle"] = []float64{3600} }),
		"file cut short":        shape(func(s *archiveShape) { s.durationSec = 1800 }),
		"chapters lost":         shape(func(s *archiveShape) { s.chapters = 0 }),
	} {
		if err := verifyArchiveRemux(orig, out, 2); err == nil {
			t.Errorf("%s: the copy was accepted", name)
		}
	}
}

// importRig is a worker over a real database with one Finished single-file
// job whose archive is a real MP4, for the re-mux tests.
type importRig struct {
	w       *DownloadWorker
	db      *database.Database
	ts      *TrimService
	archive string
	job     *database.Job
}

func newImportRig(t *testing.T) *importRig {
	t.Helper()
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found on PATH, skipping live-tool test")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not found on PATH, skipping live-tool test")
	}
	dir := t.TempDir()
	db := openTestDB(t)
	archive := filepath.Join(dir, "out", "archive.mp4")
	if err := os.MkdirAll(filepath.Dir(archive), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(ffmpegPath, "-nostdin", "-y", "-v", "error",
		"-f", "lavfi", "-i", "testsrc=size=32x32:rate=5:duration=4",
		"-f", "lavfi", "-i", "sine=duration=4",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", archive)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate the archive: %v\n%s", err, out)
	}
	one, size := 1, int64(1)
	job := &database.Job{ID: "yt_import", VideoID: "import", Platform: "youtube", URL: "u", Title: "t",
		Status: database.StatusFinished, OutputFile: archive, Filename: "archive.mp4", LengthSeconds: &one, FileSize: &size}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	cfg := &config.MoomboxConfig{}
	cfg.Paths.StagingDirectory = filepath.Join(dir, "staging")
	cfg.Paths.FfmpegPath = ffmpegPath
	ts := NewTrimService(db, ffmpegPath, discardLogger{})
	w := NewDownloadWorker(db, nil, cfg, &discardLogger{}, &DownloadWorkerDeps{TrimService: ts})
	return &importRig{w: w, db: db, ts: ts, archive: archive, job: job}
}

func probeChapterTitles(t *testing.T, path string) []string {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "quiet", "-print_format", "json", "-show_chapters", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Chapters []struct {
			Tags map[string]string `json:"tags"`
		} `json:"chapters"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, c := range p.Chapters {
		titles = append(titles, c.Tags["title"])
	}
	return titles
}

// TestRemuxArchiveChaptersEmbedsAndRecordsTheFile runs the real FFmpeg: the
// archive comes back with the chapters, and the row's file_size and
// length_seconds follow the replaced file — but only while the row still holds
// the status the caller read (UpdateJobFieldsIf).
//
// Mutants: the row write dropped (file_size stays 1); written unconditionally
// (the Cancelled row's size moves).
func TestRemuxArchiveChaptersEmbedsAndRecordsTheFile(t *testing.T) {
	r := newImportRig(t)
	list := utils.SanitizeChapters([]utils.Chapter{{StartMs: 0, Title: "one"}, {StartMs: 2000, Title: "two"}}, 4000)
	res, err := r.w.RemuxArchiveChapters(r.job.ID, database.StatusFinished, r.archive, list)
	if err != nil {
		t.Fatal(err)
	}
	if got := probeChapterTitles(t, r.archive); !reflect.DeepEqual(got, []string{"one", "two"}) {
		t.Errorf("archive chapters = %v", got)
	}
	info, _ := os.Stat(r.archive)
	row, _ := r.db.GetJob(r.job.ID)
	if row.FileSize == nil || *row.FileSize != info.Size() || res.SizeBytes != info.Size() {
		t.Errorf("file_size = %v, want %d", row.FileSize, info.Size())
	}
	if row.LengthSeconds == nil || *row.LengthSeconds != 4 {
		t.Errorf("length_seconds = %v, want 4", row.LengthSeconds)
	}

	// The row moved on (an operator's action) since the caller read it.
	r.db.UpdateJobFields(r.job.ID, map[string]any{"status": database.StatusCancelled, "file_size": int64(7)})
	if _, err := r.w.RemuxArchiveChapters(r.job.ID, database.StatusFinished, r.archive, list[:1]); err != nil {
		t.Fatal(err)
	}
	row, _ = r.db.GetJob(r.job.ID)
	if row.Status != database.StatusCancelled || row.FileSize == nil || *row.FileSize != 7 {
		t.Errorf("a row whose status moved was written: status %s, size %v", row.Status, row.FileSize)
	}
}

// TestRemuxArchiveChaptersRefusedLeavesTheArchive: a re-mux the check refuses
// leaves the archive byte-identical, no temporary file beside it, and the row
// untouched (owner decision CI-1: "On any failure the original stays
// untouched").
//
// Mutant: RemuxArchiveChapters passing no check to the muxer — the refused
// copy replaces the archive.
func TestRemuxArchiveChaptersRefusedLeavesTheArchive(t *testing.T) {
	r := newImportRig(t)
	refuse := errors.New("refused by the test")
	prev := remuxCheck
	remuxCheck = func(_, _ *archiveShape, _ int) error { return refuse }
	t.Cleanup(func() { remuxCheck = prev })

	before, err := os.ReadFile(r.archive)
	if err != nil {
		t.Fatal(err)
	}
	list := utils.SanitizeChapters([]utils.Chapter{{StartMs: 0, Title: "one"}}, 4000)
	if _, err := r.w.RemuxArchiveChapters(r.job.ID, database.StatusFinished, r.archive, list); !errors.Is(err, refuse) {
		t.Fatalf("want the check's refusal, got %v", err)
	}
	if after, _ := os.ReadFile(r.archive); !bytes.Equal(before, after) {
		t.Error("a refused re-mux changed the archive")
	}
	entries, _ := os.ReadDir(filepath.Dir(r.archive))
	for _, e := range entries {
		if e.Name() != "archive.mp4" {
			t.Errorf("left behind: %s", e.Name())
		}
	}
	if row, _ := r.db.GetJob(r.job.ID); *row.FileSize != 1 {
		t.Errorf("a refused re-mux wrote file_size %d", *row.FileSize)
	}
}

// TestChapterImportClaimExcludesTheOtherVerbs: the import's claim is the one
// the Mux action and a set-aside recovery take, and it and a trim exclude
// each other both ways; an active row and a run still stopping are refused
// (owner decision CI-1: the re-mux never overlaps the Mux action, Recover
// Set-aside or trim).
//
// Mutants: ClaimChapterImport not looking for a trim; TrimService.prepare not
// asking archiveRewriting; NewDownloadWorker not installing it; the active
// status check dropped; the IsProcessing refusal dropped.
func TestChapterImportClaimExcludesTheOtherVerbs(t *testing.T) {
	r := newTrimRig(t, "ffmpeg-unused")
	cfg := &config.MoomboxConfig{}
	cfg.Paths.StagingDirectory = filepath.Join(t.TempDir(), "staging")
	w := NewDownloadWorker(r.db, nil, cfg, &discardLogger{}, &DownloadWorkerDeps{TrimService: r.ts})
	id := r.job.ID

	release, err := w.ClaimChapterImport(id)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := w.claimJobOperation(id, opMux); !errors.Is(err, ErrStagingBusy) || !strings.Contains(err.Error(), "chapter import") {
		t.Errorf("the Mux action's claim during an import: %v", err)
	}
	if _, err := w.ClaimChapterImport(id); !errors.Is(err, ErrStagingBusy) {
		t.Errorf("a second import: %v", err)
	}
	if _, err := r.ts.StartTrim(r.job, 10, 20); err == nil || !strings.Contains(err.Error(), "chapter import") {
		t.Errorf("a trim during an import: %v", err)
	}
	release()

	// A trim's slot held: the import is refused.
	r.ts.activeMu.Lock()
	r.ts.activeOps[id] = &TrimTask{}
	r.ts.activeMu.Unlock()
	if _, err := w.ClaimChapterImport(id); !errors.Is(err, ErrStagingBusy) || !strings.Contains(err.Error(), "trim") {
		t.Errorf("an import during a trim: %v", err)
	}
	r.ts.activeMu.Lock()
	delete(r.ts.activeOps, id)
	r.ts.activeMu.Unlock()
	if w.chapterImportRunning(id) {
		t.Error("the refused import left its claim held")
	}

	// An active row.
	r.db.UpdateJobFields(id, map[string]any{"status": database.StatusMuxing})
	if _, err := w.ClaimChapterImport(id); !errors.Is(err, ErrArchiveJobActive) {
		t.Errorf("an import of a Muxing job: %v", err)
	}
	if w.chapterImportRunning(id) {
		t.Error("the refused import left its claim held")
	}

	// A run still registered (a Cancelled row whose run has not stopped is
	// about to be written again): refused until it exits.
	r.db.UpdateJobFields(id, map[string]any{"status": database.StatusCancelled})
	w.queue.Enqueue(id, database.StatusLive)
	if got, _, ok := w.queue.Dequeue(context.Background()); !ok || got != id {
		t.Fatalf("Dequeue = %q %v", got, ok)
	}
	if _, err := w.ClaimChapterImport(id); !errors.Is(err, ErrStagingBusy) || !strings.Contains(err.Error(), "still stopping") {
		t.Errorf("an import while the job's run is registered: %v", err)
	}
	if w.chapterImportRunning(id) {
		t.Error("the refused import left its claim held")
	}
	w.queue.Complete(id)
	release, err = w.ClaimChapterImport(id)
	if err != nil {
		t.Fatalf("an import once the run has exited: %v", err)
	}
	release()
}

// TestRemuxArchiveChaptersStopsWhenTheRowIsDeleted: a row deleted while the
// re-mux runs stops it before the copy replaces the archive — the archive is
// no longer anyone's, and the orphan sweep may be about to take it.
//
// Mutants: the OnJobDeleted listener dropped; the check not asking the
// context after the verdict.
func TestRemuxArchiveChaptersStopsWhenTheRowIsDeleted(t *testing.T) {
	r := newImportRig(t)
	prev := remuxCheck
	remuxCheck = func(orig, out *archiveShape, want int) error {
		if err := r.db.DeleteJob(r.job.ID); err != nil {
			t.Error(err)
		}
		return prev(orig, out, want)
	}
	t.Cleanup(func() { remuxCheck = prev })
	before, _ := os.ReadFile(r.archive)
	list := utils.SanitizeChapters([]utils.Chapter{{StartMs: 0, Title: "one"}}, 4000)
	if _, err := r.w.RemuxArchiveChapters(r.job.ID, database.StatusFinished, r.archive, list); err == nil {
		t.Fatal("the re-mux of a deleted job's archive went through")
	}
	if after, _ := os.ReadFile(r.archive); !bytes.Equal(before, after) {
		t.Error("the deleted job's archive was replaced")
	}
}

// TestSingleFileFinalizeDropsAnImportedChapterList: a finalize that writes a
// new single-file archive at a name an import saved a chapter list beside
// removes that list (owner decision CI-1) — it was fitted to the file just
// replaced, and GET /api/jobs/{id}/chapters reads an import's list ahead of
// the file's own, so it would go on overriding the new archive's chapters
// and Restore would put the old file's original onto it. The new archive
// carries its own finalize's chapters.
//
// Mutant: muxAndFinalize not calling dropImportedArchiveChapters (the list
// is still there).
func TestSingleFileFinalizeDropsAnImportedChapterList(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	o, jobCtx := muxTestOrchestrator(t, notificationtest.New(), 0)
	o.muxer = engine.NewMuxer(ffmpegPath, discardLogger{})
	jobCtx.Config = &JobConfig{}
	jobCtx.StagingDir = t.TempDir()
	jobCtx.OutputDir = t.TempDir()
	jobCtx.Filename = "a-job"
	video := filepath.Join(jobCtx.StagingDir, "video.mp4")
	writeMuxFixture(t, ffmpegPath, video, 4)
	// The previous archive's import, beside the name the finalize writes.
	sidecar := filepath.Join(jobCtx.OutputDir, "a-job"+utils.ChaptersSidecarSuffix)
	if err := utils.WriteImportedChaptersSidecar(sidecar, []utils.Chapter{{StartMs: 0, EndMs: 60_000, Title: "imported"}}, []utils.SidecarChapter{{Start: 0, End: 60, Title: "old original"}}); err != nil {
		t.Fatal(err)
	}
	if err := o.muxAndFinalize(context.Background(), jobCtx, &DownloadResult{HasVideo: true, VideoPath: video}); err != nil {
		t.Fatalf("muxAndFinalize: %v", err)
	}
	if _, err := os.Stat(filepath.Join(jobCtx.OutputDir, "a-job.mp4")); err != nil {
		t.Fatalf("no archive: %v", err)
	}
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Errorf("the imported chapter list outlived the archive it described (%v)", err)
	}
}

// TestRefinalizeOfANeverMuxedRowDropsAForeignImport: a re-finalize (the Mux
// action over an Error row whose download failed before its mux, a boot
// re-mux of a first download's interrupted Muxing row) of a row that names
// no archive has no import of its own — an import needs an archive to be
// saved beside — so a list already at the name it writes is another
// recording's, left at a reused name. The archive embeds none of it and the
// list goes, as before CJ-2.
//
// Mutant: keptImportFor also reading beside the name the finalize writes
// (the foreign list is embedded and kept).
func TestRefinalizeOfANeverMuxedRowDropsAForeignImport(t *testing.T) {
	ffmpegPath, ffprobePath := requireFFmpegTools(t)
	o, jobCtx := muxTestOrchestrator(t, notificationtest.New(), 0)
	o.muxer = engine.NewMuxer(ffmpegPath, discardLogger{})
	jobCtx.Config = &JobConfig{}
	jobCtx.Refinalize = true
	jobCtx.Job.OutputFile = ""
	jobCtx.StagingDir = t.TempDir()
	jobCtx.OutputDir = t.TempDir()
	jobCtx.Filename = "a-job"
	video := filepath.Join(jobCtx.StagingDir, "video.mp4")
	writeMuxFixture(t, ffmpegPath, video, 4)
	sidecar := filepath.Join(jobCtx.OutputDir, "a-job"+utils.ChaptersSidecarSuffix)
	foreign := []utils.Chapter{{StartMs: 0, EndMs: 2000, Title: "Foreign A"}, {StartMs: 2000, EndMs: 60_000, Title: "Foreign B"}}
	if err := utils.WriteImportedChaptersSidecar(sidecar, foreign, []utils.SidecarChapter{{Start: 0, End: 60, Title: "old original"}}); err != nil {
		t.Fatal(err)
	}
	if err := o.muxAndFinalize(context.Background(), jobCtx, &DownloadResult{HasVideo: true, VideoPath: video}); err != nil {
		t.Fatalf("muxAndFinalize: %v", err)
	}
	archive := filepath.Join(jobCtx.OutputDir, "a-job.mp4")
	shape, err := probeArchiveShape(context.Background(), ffprobePath, archive)
	if err != nil {
		t.Fatal(err)
	}
	if shape.chapters != 0 {
		t.Errorf("the archive embeds %d chapters, want none of the foreign list's", shape.chapters)
	}
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Errorf("the foreign imported chapter list was kept beside the new archive (%v)", err)
	}
}
