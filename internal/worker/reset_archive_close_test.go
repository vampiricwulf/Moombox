package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// guardJob adds a job whose run started an hour ago and returns its output
// directory and a time from before that run.
func guardJob(t *testing.T, w *DownloadWorker, db *database.Database, jobID string) (outputDir string, before time.Time) {
	t.Helper()
	started := time.Now().Add(-time.Hour)
	_, outputDir = muxFixtureJob(t, w, db, jobID)
	db.UpdateJobFields(jobID, map[string]any{"download_started_at": started.UTC().Format(time.RFC3339)})
	return outputDir, started.Add(-time.Hour)
}

// writeOldFile writes body at path with a modification time of mtime.
func writeOldFile(t *testing.T, path, body string, mtime time.Time) {
	t.Helper()
	writeTestFile(t, path, body)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// A job that ends as one part takes the plain names for the part and for its
// chat. A chat from before the recording at the plain chat name, with no
// archive beside it (a reset moved the archive and the chat could not follow;
// the operator deleted the video and kept the chat), is set aside before the
// part's chat is renamed there. When the part's files are no longer at their
// part names — a run that died between its rename and its row write — the
// files at the plain names are the part itself and are adopted, not set
// aside.
//
// Mutants: the chat not asked about in the one-part branch (the part's chat
// replaces the earlier one, which is gone); the asks made whatever is at the
// part names (the adopted part is set aside and the row names nothing).
func TestOnePartFinalizeSetsAsideAChatWithNoArchive(t *testing.T) {
	run := func(t *testing.T, partOnDisk bool, plain map[string]string) (root string, db *database.Database) {
		w, db := testWorkerSetup(t)
		root = t.TempDir()
		started := time.Now().Add(-time.Hour)
		job := &database.Job{ID: "j-part", VideoID: "j-part", URL: "https://twitch.tv/s", Platform: "twitch", Status: database.StatusDownloading}
		if _, err := db.AddJob(job); err != nil {
			t.Fatal(err)
		}
		db.UpdateJobFields(job.ID, map[string]any{"download_started_at": started.UTC().Format(time.RFC3339)})
		seg := database.Segment{JobID: job.ID, SegmentIndex: 0, Filename: "Show - part1.mp4",
			FilePath: filepath.Join(root, "Show - part1.mp4"), ChatFile: filepath.Join(root, "Show - part1.chat.json"), Quality: "720p"}
		if partOnDisk {
			writeTestFile(t, seg.FilePath, "part one")
			writeTestFile(t, seg.ChatFile, "part one's chat")
		}
		for name, body := range plain {
			writeOldFile(t, filepath.Join(root, name), body, started.Add(-time.Hour))
		}
		if err := db.AddSegment(&seg); err != nil {
			t.Fatal(err)
		}
		jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, StagingDir: t.TempDir(),
			OutputDir: root, Filename: "Show", Logger: &discardLogger{}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // no network for the thumbnail fallback inside copyAssets
		if err := w.orchestrator.finalizeMultiSegmentJob(ctx, jobCtx, []database.Segment{seg}, nil); err != nil {
			t.Fatalf("finalizeMultiSegmentJob: %v", err)
		}
		return root, db
	}

	t.Run("a chat alone at the plain name: set aside, the part's chat takes the name", func(t *testing.T) {
		root, db := run(t, true, map[string]string{"Show.chat.json": "the earlier chat"})
		aside := soleSetAside(t, root, ".chat.json")
		if got := readTestFile(t, aside); got != "the earlier chat" {
			t.Errorf("the set-aside chat reads %q", got)
		}
		if got := asideOwnerID(aside); got != "j-part" {
			t.Errorf("its marker names %q, want the finishing job", got)
		}
		if got := readTestFile(t, filepath.Join(root, "Show.chat.json")); got != "part one's chat" {
			t.Errorf("Show.chat.json reads %q, want the part's chat", got)
		}
		// The details block names it: no archive shares its set-aside name.
		job, _ := db.GetJob("j-part")
		job.OutputDirectory = root
		if got := ScanSetAsideArchives(job, "", "Show"); len(got) != 1 || got[0].Path != aside {
			t.Errorf("ScanSetAsideArchives = %+v, want the set-aside chat", got)
		}
	})

	t.Run("the part already moved by a run that died: adopted, nothing set aside", func(t *testing.T) {
		root, db := run(t, false, map[string]string{"Show.mp4": "part one", "Show.chat.json": "part one's chat"})
		if got := setAsideFiles(t, root, ""); len(got) != 0 {
			t.Errorf("the job's own part was set aside: %v", got)
		}
		segs, err := db.GetSegments("j-part")
		if err != nil || len(segs) != 1 || segs[0].FilePath != filepath.Join(root, "Show.mp4") || segs[0].ChatFile != filepath.Join(root, "Show.chat.json") {
			t.Errorf("part row = %+v (%v), want it repaired to the plain names", segs, err)
		}
	})
}

// A file another existing row names is never set aside by a finalize: the row
// would then name the finishing job's recording, and its own would sit under
// the other job's marker. The finalize is refused for it, naming the row; a
// sidecar another row names stays where it is while the rest goes aside.
//
// Mutants: the other-rows check dropped (the other job's archive is renamed
// and marked as the finishing job's); a sidecar's row not asked (the other
// job's chat goes aside with a file that is nobody's).
func TestFinalizeRefusesAFileAnotherRowNames(t *testing.T) {
	w, db := testWorkerSetup(t)
	outputDir, before := guardJob(t, w, db, "j-new")
	archive := filepath.Join(outputDir, "Show.mp4")
	chat := filepath.Join(outputDir, "Other.chat.json")
	writeOldFile(t, archive, "job A's recording", before)
	writeOldFile(t, chat, "job A's chat", before)
	writeOldFile(t, filepath.Join(outputDir, "Other.mp4"), "nobody's", before)
	other := &database.Job{ID: "j-A", VideoID: "j-A", URL: "u", Platform: "youtube", Status: database.StatusFinished,
		OutputFile: archive, ChatFile: chat}
	if _, err := db.AddJob(other); err != nil {
		t.Fatal(err)
	}

	err := w.orchestrator.setForeignFileAside("j-new", archive, archiveSidecarExts)
	if err == nil || !strings.Contains(err.Error(), "job j-A") {
		t.Fatalf("setForeignFileAside = %v, want it refused naming job j-A", err)
	}
	if got := readTestFile(t, archive); got != "job A's recording" {
		t.Errorf("job A's archive reads %q at its name", got)
	}
	if got := setAsideFiles(t, outputDir, ""); len(got) != 0 {
		t.Errorf("files were set aside: %v", got)
	}

	if err := w.orchestrator.setForeignFileAside("j-new", filepath.Join(outputDir, "Other.mp4"), archiveSidecarExts); err != nil {
		t.Fatalf("a file no row names: %v", err)
	}
	if got := readTestFile(t, chat); got != "job A's chat" {
		t.Errorf("job A's chat reads %q at its name; it went aside with a file that is not its archive", got)
	}
	if got := readTestFile(t, soleSetAside(t, outputDir, ".mp4")); got != "nobody's" {
		t.Errorf("the set-aside file reads %q", got)
	}
}

// A marker is created, never written over. A set-aside whose rename fails
// takes back only a marker it created: one already there for the same job
// speaks for a file set aside before it. A set-aside name under another job's
// marker is passed over.
//
// Mutants: the marker written unconditionally (another job's marker now names
// this one); the marker removed on every failed rename (the earlier aside
// loses its marker, and Files offers it while its job exists); freeAsideName
// not asking about the marker (the file lands under another job's marker).
func TestAsideMarkerIsNeverOverwrittenOrTakenFromAnotherFile(t *testing.T) {
	// The stamp is the second the rename runs in: hold every name it can take
	// for the next few.
	markers := func(dir, jobID string) []string {
		var out []string
		for ts := time.Now().Unix(); len(out) < 5; ts++ {
			p := filepath.Join(dir, fmt.Sprintf("Show.restart-%d.owner", ts))
			writeTestFile(t, p, asideMarkerPrefix+jobID+"\n")
			out = append(out, p)
		}
		return out
	}

	t.Run("a failed rename leaves the marker that was there", func(t *testing.T) {
		dir := t.TempDir()
		held := markers(dir, "j-me")
		chat := filepath.Join(dir, "Show.chat.json")
		writeTestFile(t, chat, "chat")
		failRenameArchive(t)
		if mv := renameAside("j-me", chat, nil); mv.err == nil {
			t.Fatal("the rename did not fail")
		}
		for _, m := range held {
			if id, ok := readAsideMarker(m); !ok || id != "j-me" {
				t.Errorf("%s is gone or changed after a failed set-aside under its name", filepath.Base(m))
			}
		}
	})

	t.Run("a name under another job's marker is passed over", func(t *testing.T) {
		dir := t.TempDir()
		held := markers(dir, "j-other")
		archive := filepath.Join(dir, "Show.mp4")
		writeTestFile(t, archive, "recording")
		mv := renameAside("j-me", archive, nil)
		if mv.err != nil || mv.markerErr != nil {
			t.Fatalf("renameAside = %+v", mv)
		}
		if got := asideOwnerID(mv.dst); got != "j-me" {
			t.Errorf("%s is under a marker naming %q, want j-me", filepath.Base(mv.dst), got)
		}
		for _, m := range held {
			if id, _ := readAsideMarker(m); id != "j-other" {
				t.Errorf("%s now names %q", filepath.Base(m), id)
			}
		}
	})

	t.Run("an in-place marker does not replace the operator's file", func(t *testing.T) {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, "Show.owner"), "this folder is mine\n")
		if _, err := writeAsideOwner(filepath.Join(dir, "Show"), "j-me"); err == nil {
			t.Error("writeAsideOwner reported no error over a file that is not a marker")
		}
		if got := readTestFile(t, filepath.Join(dir, "Show.owner")); got != "this folder is mine\n" {
			t.Errorf("the operator's file now reads %q", got)
		}
	})
}

// What a finalize's set-aside leaves alone and what it clears: a directory at
// the archive's name is not renamed (the details block would never show it;
// the write fails on it as it always did), and the marker a failed reset
// rename left under the archive's own name goes once the archive is set aside
// after all.
//
// Mutants: the file-type check dropped (the directory is renamed under a
// marker); the in-place marker kept (Files offers Show.owner once the job is
// deleted).
func TestFinalizeSetAsideOfADirectoryAndAStaleInPlaceMarker(t *testing.T) {
	w, db := testWorkerSetup(t)
	outputDir, before := guardJob(t, w, db, "j-new")

	dir := filepath.Join(outputDir, "Dir.mp4")
	writeTestFile(t, filepath.Join(dir, "inside"), "x")
	if err := os.Chtimes(dir, before, before); err != nil {
		t.Fatal(err)
	}
	if err := w.orchestrator.setForeignFileAside("j-new", dir, archiveSidecarExts); err != nil {
		t.Fatalf("a directory at the name: %v", err)
	}
	if got := setAsideFiles(t, outputDir, ""); len(got) != 0 {
		t.Errorf("the directory was set aside: %v", got)
	}

	archive := filepath.Join(outputDir, "Show.mp4")
	writeOldFile(t, archive, "the earlier recording", before)
	if _, err := writeAsideOwner(asideBase(archive), "j-new"); err != nil {
		t.Fatal(err)
	}
	if err := w.orchestrator.setForeignFileAside("j-new", archive, archiveSidecarExts); err != nil {
		t.Fatalf("setForeignFileAside: %v", err)
	}
	if got := asideOwnerID(soleSetAside(t, outputDir, ".mp4")); got != "j-new" {
		t.Errorf("the set-aside archive's marker names %q", got)
	}
	if _, err := os.Lstat(filepath.Join(outputDir, "Show.owner")); !os.IsNotExist(err) {
		t.Errorf("the in-place marker is still there after the archive was set aside (%v)", err)
	}
}

// The delete behind the Files tab refuses an *.owner file that is not a
// marker, which only a request built by hand can name, and still deletes a
// marker whose job is gone.
//
// Mutant: the recheck dropped (the operator's file is deleted).
func TestDeleteOrphanedFileRefusesAnOwnerFileThatIsNoMarker(t *testing.T) {
	_, db := testWorkerSetup(t)
	outputDir := t.TempDir()
	cfg := &config.MoomboxConfig{}
	cfg.Paths.OutputDirectory = outputDir
	cfg.Paths.StagingDirectory = t.TempDir()
	notes := filepath.Join(outputDir, "notes.owner")
	marker := filepath.Join(outputDir, "Gone.restart-1700000000.owner")
	writeTestFile(t, notes, "j-gone\n")
	writeTestFile(t, marker, asideMarkerPrefix+"j-gone\n")

	if err := DeleteOrphanedFile(notes, db, cfg); err == nil {
		t.Error("the operator's .owner file was deleted without an error")
	}
	if !fileExists(notes) {
		t.Error("the operator's .owner file is gone")
	}
	if err := DeleteOrphanedFile(marker, db, cfg); err != nil || fileExists(marker) {
		t.Errorf("a marker whose job is gone was not deleted: %v", err)
	}
}
