package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// fakeChapterImporter stands in for the worker's half of the import: its claim and
// re-mux can be refused, its re-mux calls are counted, and with real set the
// re-mux is the worker's own.
type fakeChapterImporter struct {
	mu       sync.Mutex
	claimErr error
	remuxErr error
	remuxes  int
	real     ChapterImporter
}

func (f *fakeChapterImporter) ClaimChapterImport(jobID string) (func(), error) {
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	if f.real != nil {
		return f.real.ClaimChapterImport(jobID)
	}
	return func() {}, nil
}

func (f *fakeChapterImporter) RemuxArchiveChapters(jobID string, expect database.JobStatus, path string, chapters []utils.Chapter) (*worker.ArchiveRemuxResult, error) {
	f.mu.Lock()
	f.remuxes++
	f.mu.Unlock()
	if f.remuxErr != nil {
		return nil, f.remuxErr
	}
	if f.real != nil {
		return f.real.RemuxArchiveChapters(jobID, expect, path, chapters)
	}
	return &worker.ArchiveRemuxResult{}, nil
}

func (f *fakeChapterImporter) remuxCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.remuxes
}

// newChapterImportFixture is the chapter fixture with the import's routes wired over
// imp.
func newChapterImportFixture(t *testing.T, imp *fakeChapterImporter) *chapterFixture {
	t.Helper()
	f := &chapterFixture{jobsFixture: newJobsFixture(t), logger: &chapterLogger{}}
	f.cache = newChapterCache(chapterCacheSize)
	f.svc = chapterRoutes(f.router, &ChapterRouteDeps{DB: f.db, Store: f.store, Importer: imp, Logger: f.logger}, f.cache)
	return f
}

// useRealWorker makes imp the worker itself, over the fixture's database,
// with the FFmpeg on PATH.
func useRealWorker(f *chapterFixture, imp *fakeChapterImporter, ffmpegPath string) {
	cfg := config.Defaults()
	cfg.Paths.FfmpegPath = ffmpegPath
	cfg.Paths.StagingDirectory = f.stagingDir
	cfg.Paths.OutputDirectory = f.outputDir
	imp.real = worker.NewDownloadWorker(f.db, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
}

func (f *chapterFixture) postImport(t *testing.T, path string, body any) (int, ChapterImportView, string) {
	t.Helper()
	rec := doRequest(t, f.router, "POST", path, body)
	var v ChapterImportView
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, v, rec.Body.String()
}

func titlesOf(list []Chapter) []string {
	var out []string
	for _, c := range list {
		out = append(out, c.Title)
	}
	return out
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestChapterImportPreviewDoesNotWrite: the preview parses the owner's
// example 3 against the archive and writes nothing — no sidecar, the archive
// byte-identical — and answers the job's current (embedded) chapters beside
// the list (owner decisions CI-3, CI-4). A paste with no timestamp answers
// 200 with the reason, for the dialog to show.
//
// Mutants: Preview calling apply (a sidecar appears); the current list left
// out of the view.
func TestChapterImportPreviewDoesNotWrite(t *testing.T) {
	ffmpegPath := requireFFmpeg(t)
	imp := &fakeChapterImporter{}
	f := newChapterImportFixture(t, imp)
	archive := filepath.Join(f.outputDir, "irys.mp4")
	// Two hours of timeline cannot be generated in a test; the archive is 6
	// s and the row's length stands in only if ffprobe fails, so the preview
	// keeps what falls in the file's 6 s.
	makeChapteredMP4(t, ffmpegPath, archive, 6, []utils.Chapter{{StartMs: 0, Title: "embedded"}})
	f.addJob(t, "yt_irys", func(j *database.Job) { j.Filename = "irys.mp4" })
	before := readFile(t, archive)

	text := string(readFile(t, filepath.Join("..", "..", "utils", "testdata", "chapter_import", "irys.txt")))
	code, v, body := f.postImport(t, "/api/jobs/yt_irys/chapters/preview", map[string]any{"text": text, "offsetSeconds": "-0:06:52"})
	if code != http.StatusOK {
		t.Fatalf("preview: %d %s", code, body)
	}
	// 0:06:55 and 0:08:26 shifted by -6:52 land at 3 s and past the end.
	if got := titlesOf(v.Chapters); strings.Join(got, "|") != "<Untitled Chapter 1>|irysHirys Start | スタート" {
		t.Errorf("previewed list = %q", got)
	}
	if v.OffsetSeconds != -412 || v.DurationSeconds < 5.9 {
		t.Errorf("offset %v, duration %v", v.OffsetSeconds, v.DurationSeconds)
	}
	if got := titlesOf(v.Current); strings.Join(got, "|") != "embedded" {
		t.Errorf("current = %q, want the embedded chapter", got)
	}
	if v.HasOriginal {
		t.Error("an archive never imported into offers a restore")
	}
	if _, err := os.Stat(filepath.Join(f.outputDir, "irys"+utils.ChaptersSidecarSuffix)); !os.IsNotExist(err) {
		t.Errorf("the preview wrote a sidecar (%v)", err)
	}
	if !bytes.Equal(before, readFile(t, archive)) {
		t.Error("the preview changed the archive")
	}
	if imp.remuxCount() != 0 {
		t.Error("the preview re-muxed")
	}

	code, v, _ = f.postImport(t, "/api/jobs/yt_irys/chapters/preview", map[string]any{"text": "⚔1.\n________"})
	if code != http.StatusOK || v.Error != "No timestamps found" || len(v.Chapters) != 0 {
		t.Errorf("an untimed paste: %d %+v", code, v)
	}
	// The save refuses it instead.
	if code, _, body := f.postImport(t, "/api/jobs/yt_irys/chapters", map[string]any{"text": "no times"}); code != http.StatusBadRequest || !strings.Contains(body, "No timestamps found") {
		t.Errorf("an untimed save: %d %s", code, body)
	}
	if code, _, _ := f.postImport(t, "/api/jobs/yt_irys/chapters/preview", map[string]any{"text": "0:01 a", "offsetSeconds": "soon"}); code != http.StatusOK {
		t.Errorf("a bad offset preview: %d", code)
	}
}

// TestChapterImportSaveRemuxesAndRestores runs the real FFmpeg and the real
// worker end to end (owner decisions CI-1, CI-3, CI-5): the first save writes
// the sidecar with the archive's embedded chapters as the original, re-muxes
// the archive so ffprobe shows the imported list, and GET answers it; the
// offset shifts it; a second save keeps the first original; restore brings
// the embedded chapters back into the file and the list.
//
// Mutants: the original taken from the sidecar on every save (the second
// save would make the first import the original); the re-mux skipped (the
// file keeps "embedded A"); GET not reading the import's sidecar first; the
// offset ignored.
func TestChapterImportSaveRemuxesAndRestores(t *testing.T) {
	ffmpegPath := requireFFmpeg(t)
	imp := &fakeChapterImporter{}
	f := newChapterImportFixture(t, imp)
	useRealWorker(f, imp, ffmpegPath)
	archive := filepath.Join(f.outputDir, "vod.mp4")
	makeChapteredMP4(t, ffmpegPath, archive, 6, []utils.Chapter{{StartMs: 0, Title: "embedded A"}, {StartMs: 3000, Title: "embedded B"}})
	f.addJob(t, "yt_vod", func(j *database.Job) { j.Filename = "vod.mp4"; j.OutputFile = archive })

	code, v, body := f.postImport(t, "/api/jobs/yt_vod/chapters", map[string]any{"text": "0:01 one\n├ 0:04 two", "offsetSeconds": 1})
	if code != http.StatusOK {
		t.Fatalf("save: %d %s", code, body)
	}
	if !v.Embedded || v.EmbedError != "" || !v.HasOriginal {
		t.Errorf("save view: %+v", v)
	}
	wantImported := []Chapter{{Start: 0, End: 2, Title: "<Untitled Chapter 1>"}, {Start: 2, End: 5, Title: "one"}, {Start: 5, End: 6, Title: "two"}}
	assertChapters(t, v.Chapters, wantImported)
	assertChapters(t, f.get(t, "yt_vod"), wantImported)
	probed, err := ffprobeChapters(t.Context(), "ffprobe", archive)
	if err != nil {
		t.Fatal(err)
	}
	assertChapters(t, probed, wantImported)
	imported, ok, err := utils.ReadImportedChaptersSidecar(filepath.Join(f.outputDir, "vod"+utils.ChaptersSidecarSuffix))
	if err != nil || !ok || len(imported.Original) != 2 || imported.Original[1].Title != "embedded B" {
		t.Fatalf("sidecar original = %+v %v %v", imported, ok, err)
	}
	info, _ := os.Stat(archive)
	if row, _ := f.db.GetJob("yt_vod"); row.FileSize == nil || *row.FileSize != info.Size() {
		t.Errorf("file_size = %v, want %d", row.FileSize, info.Size())
	}

	// A second import keeps the archive's own chapters as the original.
	if code, _, body := f.postImport(t, "/api/jobs/yt_vod/chapters", map[string]any{"text": "0:00 again"}); code != http.StatusOK {
		t.Fatalf("second save: %d %s", code, body)
	}
	imported, _, _ = utils.ReadImportedChaptersSidecar(filepath.Join(f.outputDir, "vod"+utils.ChaptersSidecarSuffix))
	if strings.Join(titlesOf(sidecarToChapters(imported.Original)), "|") != "embedded A|embedded B" {
		t.Errorf("the second save's original = %+v", imported.Original)
	}

	if !f.svc.HasOriginal("yt_vod") {
		t.Error("HasOriginal after an import of a single-file archive")
	}
	code, v, body = f.postImport(t, "/api/jobs/yt_vod/chapters/restore", nil)
	if code != http.StatusOK || !v.Embedded {
		t.Fatalf("restore: %d %s", code, body)
	}
	wantOriginal := []Chapter{{Start: 0, End: 3, Title: "embedded A"}, {Start: 3, End: 6, Title: "embedded B"}}
	assertChapters(t, f.get(t, "yt_vod"), wantOriginal)
	probed, _ = ffprobeChapters(t.Context(), "ffprobe", archive)
	assertChapters(t, probed, wantOriginal)
	if !v.HasOriginal {
		t.Error("the original is no longer offered after a restore")
	}
}

func sidecarToChapters(list []utils.SidecarChapter) []Chapter {
	out := make([]Chapter, 0, len(list))
	for _, c := range list {
		out = append(out, Chapter{Start: c.Start, End: c.End, Title: c.Title})
	}
	return out
}

// TestChapterImportFailedRemuxKeepsTheSidecar: a re-mux that fails leaves the
// archive byte-identical and the list saved beside it, which GET answers, and
// the save says why (owner decision CI-1).
//
// Mutant: apply returning the re-mux's error instead of saving the sidecar
// first (no sidecar; GET answers the embedded list).
func TestChapterImportFailedRemuxKeepsTheSidecar(t *testing.T) {
	ffmpegPath := requireFFmpeg(t)
	imp := &fakeChapterImporter{remuxErr: errors.New("the re-mux carries 0 chapter(s), 2 were asked for")}
	f := newChapterImportFixture(t, imp)
	archive := filepath.Join(f.outputDir, "vod.mp4")
	makeChapteredMP4(t, ffmpegPath, archive, 4, []utils.Chapter{{StartMs: 0, Title: "embedded"}})
	f.addJob(t, "yt_vod", func(j *database.Job) { j.Filename = "vod.mp4" })
	before := readFile(t, archive)

	code, v, body := f.postImport(t, "/api/jobs/yt_vod/chapters", map[string]any{"text": "0:00 a\n0:02 b"})
	if code != http.StatusOK {
		t.Fatalf("save: %d %s", code, body)
	}
	if v.Embedded || !strings.Contains(v.EmbedError, "chapter(s)") {
		t.Errorf("view = %+v, want the re-mux's failure", v)
	}
	if !bytes.Equal(before, readFile(t, archive)) {
		t.Error("the archive changed")
	}
	if got := titlesOf(f.get(t, "yt_vod")); strings.Join(got, "|") != "a|b" {
		t.Errorf("GET = %q, want the saved list", got)
	}
}

// TestChapterImportSplitArchiveSidecarOnly: a split archive gets the sidecar
// on the global timeline and no re-mux; its parts are byte-identical (owner
// decision CI-1: recorded parts are never rewritten). The list is fitted to
// the parts laid end to end.
//
// Mutants: apply re-muxing a split archive too (remuxes 1); GET not reading
// a split archive's imported sidecar ahead of its parts' own chapters.
func TestChapterImportSplitArchiveSidecarOnly(t *testing.T) {
	imp := &fakeChapterImporter{}
	f := newChapterImportFixture(t, imp)
	p1 := filepath.Join(f.outputDir, "Show - part1.mp4")
	p2 := filepath.Join(f.outputDir, "Show - part2.mp4")
	for _, p := range []string{p1, p2} {
		if err := os.WriteFile(p, []byte("recorded "+p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Part 1 carries a chapter of its own (a W14 gap-fill's original file):
	// the import's list answers ahead of it.
	f.cache.probe = func(_ context.Context, _, file string) ([]Chapter, error) {
		if file == p1 {
			return []Chapter{{Start: 0, End: 100, Title: "part 1's own"}}, nil
		}
		return nil, nil
	}
	f.addJob(t, "tw_split", func(j *database.Job) {
		j.Platform = "twitch"
		j.OutputFile = p1
		j.Segments = []database.Segment{
			{SegmentIndex: 0, Filename: filepath.Base(p1), FilePath: p1, DurationSeconds: 100},
			{SegmentIndex: 1, Filename: filepath.Base(p2), FilePath: p2, DurationSeconds: 60},
		}
	})
	if f.svc.HasOriginal("tw_split") {
		t.Error("HasOriginal before any import")
	}
	code, v, body := f.postImport(t, "/api/jobs/tw_split/chapters", map[string]any{"text": "0:00 a\n2:00 b\n3:00 past"})
	if code != http.StatusOK {
		t.Fatalf("save: %d %s", code, body)
	}
	if !v.Split || v.Embedded || imp.remuxCount() != 0 {
		t.Errorf("view %+v, remuxes %d", v, imp.remuxCount())
	}
	if !f.svc.HasOriginal("tw_split") {
		t.Error("HasOriginal after an import (the TUI's A U gate)")
	}
	assertChapters(t, f.get(t, "tw_split"), []Chapter{{Start: 0, End: 120, Title: "a"}, {Start: 120, End: 160, Title: "b"}})
	for _, p := range []string{p1, p2} {
		if string(readFile(t, p)) != "recorded "+p {
			t.Errorf("%s was rewritten", filepath.Base(p))
		}
	}
	imported, ok, _ := utils.ReadImportedChaptersSidecar(filepath.Join(f.outputDir, "Show"+utils.ChaptersSidecarSuffix))
	if !ok || len(imported.Original) != 1 || imported.Original[0].Title != "part 1's own" {
		t.Errorf("the split archive's original = %+v (want the parts' own list)", imported)
	}
}

// TestChapterImportRefusals: an active job is refused 409 by all three
// routes; a job the claim refuses (a Mux action or a trim holding it) is 409;
// a restore with nothing imported is 409; an unknown job is 404 (owner
// decision CI-1).
//
// Mutants: chapterTargetFor not checking the status; apply mapping the
// claim's refusal to 500.
func TestChapterImportRefusals(t *testing.T) {
	imp := &fakeChapterImporter{}
	f := newChapterImportFixture(t, imp)
	if err := os.WriteFile(filepath.Join(f.outputDir, "a.mp4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	length := 600
	f.addJob(t, "yt_live", func(j *database.Job) { j.Status = database.StatusDownloading; j.Filename = "a.mp4" })
	f.addJob(t, "yt_done", func(j *database.Job) { j.Filename = "a.mp4"; j.LengthSeconds = &length })
	body := map[string]any{"text": "0:01 a"}
	for _, path := range []string{"/api/jobs/yt_live/chapters", "/api/jobs/yt_live/chapters/preview", "/api/jobs/yt_live/chapters/restore"} {
		if code, _, resp := f.postImport(t, path, body); code != http.StatusConflict || !strings.Contains(resp, "once it has finished") {
			t.Errorf("%s on an active job: %d %s", path, code, resp)
		}
	}
	if code, _, resp := f.postImport(t, "/api/jobs/yt_done/chapters/restore", nil); code != http.StatusConflict {
		t.Errorf("restore with nothing imported: %d %s", code, resp)
	}
	if code, _, _ := f.postImport(t, "/api/jobs/nope/chapters", body); code != http.StatusNotFound {
		t.Errorf("unknown job: %d", code)
	}
	imp.claimErr = errors.Join(worker.ErrStagingBusy, errors.New("mux in progress"))
	if code, _, resp := f.postImport(t, "/api/jobs/yt_done/chapters", body); code != http.StatusConflict {
		t.Errorf("a claimed job: %d %s", code, resp)
	}
	if _, err := os.Stat(filepath.Join(f.outputDir, "a"+utils.ChaptersSidecarSuffix)); !os.IsNotExist(err) {
		t.Error("a refused save wrote the sidecar")
	}
}
