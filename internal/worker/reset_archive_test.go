package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

// offeredOutput is every path the Files sweep offers under outputDir, with
// the names it folds into an entry (OrphanedEntry.Asides), sorted.
func offeredOutput(t *testing.T, db *database.Database, outputDir string) []string {
	t.Helper()
	cfg := &config.MoomboxConfig{}
	cfg.Paths.OutputDirectory = outputDir
	entries, err := scanOutputOrphans(db, cfg)
	if err != nil {
		t.Fatalf("scanOutputOrphans: %v", err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[filepath.Base(e.Path)] = true
		for _, a := range e.Asides {
			seen[a] = true
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// archivedErrorJob adds an Error row that names a single-file archive with a
// chat and a description beside it: a Finished job with an incomplete tail
// whose resume failed. Returns the output directory and the archive.
func archivedErrorJob(t *testing.T, w *DownloadWorker, db *database.Database, jobID string) (outputDir, archive string) {
	t.Helper()
	_, outputDir = muxFixtureJob(t, w, db, jobID)
	archive = filepath.Join(outputDir, "Show.mp4")
	writeTestFile(t, archive, "the earlier recording")
	writeTestFile(t, filepath.Join(outputDir, "Show.chat.json"), "the earlier chat")
	writeTestFile(t, filepath.Join(outputDir, "Show.description"), "the earlier description")
	db.UpdateJobFields(jobID, map[string]any{
		"status": database.StatusError, "output_file": archive, "filename": "Show.mp4",
		"chat_file": filepath.Join(outputDir, "Show.chat.json"), "description_file": filepath.Join(outputDir, "Show.description"),
	})
	return outputDir, archive
}

// asideArchives is every set-aside archive ("<stem>.restart-<ts>[-N].mp4") in
// dir.
func asideArchives(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if _, ok := asideSiblingStem(p); ok && filepath.Ext(p) == ".mp4" {
			out = append(out, p)
		}
	}
	return out
}

// TestReinitializeSetsTheExistingArchiveAside is owner ruling R2-W1b: a
// Reinitialize of a job whose row names a single-file archive renames it
// aside at the reset, with its chat and description, and the job owns it from
// then on, whatever the new recording does.
//
//   - the preview names the archive, with its size;
//   - after the reset the archive's own name is free, the earlier recording,
//     chat and description are under one set-aside name beside it, and the
//     Files sweep offers none of them, though the row now names no file;
//   - a delete built by hand is refused for the aside and for its directory;
//   - once the job row is deleted the sweep lists them, marker included.
//
// Mutants: setArchiveAside not called (the archive is still at its name, and
// the sweep offers it); the sidecar loop dropped (the chat stays where the
// new recording writes its own); the marker not written, or asideOwner not
// asked by ownerOf (the sweep offers the aside while its job exists);
// asideOwnerUnder dropped (the directory delete is not refused); the marker
// left out of the walk (it is never listed once the job is gone).
func TestReinitializeSetsTheExistingArchiveAside(t *testing.T) {
	w, db := testWorkerSetup(t)
	outputDir, archive := archivedErrorJob(t, w, db, "j-aside")
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })

	job, _ := db.GetJob("j-aside")
	if p := PreviewReinitialize(db, stagingBase, job); p.ExistingArchives != 1 || p.ExistingArchiveBytes != int64(len("the earlier recording")) {
		t.Errorf("preview = %+v, want the existing archive and its size", p)
	}

	w.reinitializeNow("j-aside", database.StatusError)

	if job, _ = db.GetJob("j-aside"); job.Status != database.StatusUpcoming || job.OutputFile != "" {
		t.Fatalf("after the reset the row reads %s with output_file %q", job.Status, job.OutputFile)
	}
	if _, err := os.Lstat(archive); !os.IsNotExist(err) {
		t.Fatalf("the archive is still at its own name after the reset (%v): the new recording would replace it", err)
	}
	asides := asideArchives(t, outputDir)
	if len(asides) != 1 {
		t.Fatalf("set-aside archives beside the old name = %v, want one", asides)
	}
	if stem, _ := asideSiblingStem(asides[0]); stem != normalizePath(filepath.Join(outputDir, "Show")) {
		t.Errorf("the aside %s is not named after the archive", asides[0])
	}
	base := strings.TrimSuffix(asides[0], ".mp4")
	for name, want := range map[string]string{
		base + ".mp4":         "the earlier recording",
		base + ".chat.json":   "the earlier chat",
		base + ".description": "the earlier description",
	} {
		if got := readTestFile(t, name); got != want {
			t.Errorf("%s reads %q, want %q", filepath.Base(name), got, want)
		}
	}
	if got := offeredOutput(t, db, outputDir); len(got) != 0 {
		t.Errorf("the Files sweep offers %v after the reset; they are the job's", got)
	}
	// Job details list it for this job, which names no file now, and for no
	// other job writing to the same directory.
	if got := ScanSetAsideArchives(job, "", ""); len(got) != 1 || got[0].Path != asides[0] || got[0].Size != int64(len("the earlier recording")) {
		t.Errorf("ScanSetAsideArchives = %+v, want the aside %s and its size", got, asides[0])
	}
	other := &database.Job{ID: "j-other", VideoID: "j-other", URL: "u", Platform: "youtube", Status: database.StatusUpcoming, OutputDirectory: outputDir}
	if got := ScanSetAsideArchives(other, "", ""); len(got) != 0 {
		t.Errorf("another job lists %+v as its own", got)
	}

	cfg := &config.MoomboxConfig{}
	cfg.Paths.OutputDirectory = filepath.Dir(outputDir)
	cfg.Paths.StagingDirectory = stagingBase
	var notOrphan *NotOrphanError
	for _, p := range []string{asides[0], base + ".chat.json", outputDir} {
		if err := DeleteOrphanedFile(p, db, cfg); !errors.As(err, &notOrphan) {
			t.Errorf("DeleteOrphanedFile(%s) = %v, want it refused as the job's", filepath.Base(p), err)
		}
	}
	if got := readTestFile(t, asides[0]); got != "the earlier recording" {
		t.Fatalf("the aside reads %q after the refused deletes", got)
	}

	if err := db.DeleteJob("j-aside"); err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Base(base) + ".chat.json", filepath.Base(base) + ".description", filepath.Base(base) + ".mp4", filepath.Base(base) + asideOwnerSuffix}
	sort.Strings(want)
	if got := offeredOutput(t, db, outputDir); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("with the job row gone the sweep offers %v, want %v", got, want)
	}
}

// A reset whose claim is refused renames nothing (state-machine rule 3): the
// row a Cancel landed on still names its archive, where it was.
//
// Mutant: the rename done before the conditional write.
func TestRefusedResetLeavesTheArchiveInPlace(t *testing.T) {
	w, db := testWorkerSetup(t)
	outputDir, archive := archivedErrorJob(t, w, db, "j-refused")
	orig := beforeResetClaim
	beforeResetClaim = func(id string) { db.UpdateJobFields(id, map[string]any{"status": database.StatusCancelled}) }
	t.Cleanup(func() { beforeResetClaim = orig })

	w.reinitializeNow("j-refused", database.StatusError)

	if job, _ := db.GetJob("j-refused"); job.Status != database.StatusCancelled || job.OutputFile != archive {
		t.Fatalf("the refused reset left the row %s naming %q", job.Status, job.OutputFile)
	}
	if got := readTestFile(t, archive); got != "the earlier recording" {
		t.Errorf("the archive reads %q at its own name", got)
	}
	if asides := asideArchives(t, outputDir); len(asides) != 0 {
		t.Errorf("a refused reset set %v aside", asides)
	}
}

// The automatic reinitialize sets a single-file archive aside as the
// operator's does; a row with recorded parts has none to set aside (its
// output_file names a part, which the automatic reset keeps for the capture
// to continue from).
//
// Mutants: setArchiveAside left out of autoReinitializeNow; the parts check
// dropped from archiveSetAsideByReset (a kept part is renamed away from its
// segment row).
func TestAutoReinitializeSetsTheArchiveAside(t *testing.T) {
	t.Run("single file", func(t *testing.T) {
		w, db := testWorkerSetup(t)
		outputDir, archive := archivedErrorJob(t, w, db, "j-auto")
		w.autoReinitializeNow("j-auto", database.StatusError)
		if _, err := os.Lstat(archive); !os.IsNotExist(err) {
			t.Errorf("the archive is still at its own name (%v)", err)
		}
		if asides := asideArchives(t, outputDir); len(asides) != 1 {
			t.Errorf("set-aside archives = %v, want one", asides)
		}
	})
	t.Run("recorded parts", func(t *testing.T) {
		w, db := testWorkerSetup(t)
		outputDir, archive := archivedErrorJob(t, w, db, "j-auto-parts")
		if err := db.AddSegment(&database.Segment{JobID: "j-auto-parts", SegmentIndex: 0, Filename: "Show.mp4", FilePath: archive}); err != nil {
			t.Fatal(err)
		}
		w.autoReinitializeNow("j-auto-parts", database.StatusError)
		if got := readTestFile(t, archive); got != "the earlier recording" {
			t.Errorf("the recorded part reads %q at its own name", got)
		}
		if asides := asideArchives(t, outputDir); len(asides) != 0 {
			t.Errorf("a recorded part was set aside: %v", asides)
		}
	})
}

// A set-aside rename that fails does not undo the reset: the row is reset,
// the file is exactly where it was with no marker beside it, and one Warn
// says so.
//
// Mutants: the marker left behind on a failed rename; the failure not logged;
// the sidecars renamed although the archive was not.
func TestFailedSetAsideLeavesTheArchiveWhereItIs(t *testing.T) {
	w, db := testWorkerSetup(t)
	outputDir, _ := archivedErrorJob(t, w, db, "j-locked")
	log := &warnCaptureLogger{}
	w.logger = log
	orig := renameArchive
	renameArchive = func(string, string) error { return errors.New("locked") }
	t.Cleanup(func() { renameArchive = orig })

	w.reinitializeNow("j-locked", database.StatusError)

	if job, _ := db.GetJob("j-locked"); job.Status != database.StatusUpcoming || job.OutputFile != "" {
		t.Fatalf("the reset did not stand: %s, output_file %q", job.Status, job.OutputFile)
	}
	entries, _ := os.ReadDir(outputDir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if got := strings.Join(names, "|"); got != "Show.chat.json|Show.description|Show.mp4" {
		t.Errorf("the output directory holds %s; want the archive and its files as they were, and nothing else", got)
	}
	if len(log.warns) != 1 || !strings.Contains(log.warns[0], "could not be set aside") {
		t.Errorf("warnings = %q, want one naming the archive that was not set aside", log.warns)
	}
}

// TestFinalizeDoesNotReplaceAFileItDidNotWrite is the guard behind a failed
// set-aside (owner ruling R2-W1b): a finalize whose archive name is held by a
// file from before its recording stops with the staging kept, and a job's
// only part keeps its part name. A file the finalizing run wrote itself — a
// finalize interrupted between its copy and its Finished write — is replaced
// as before.
//
// Mutants: the single-file check dropped (the mux replaces the earlier
// archive); the single-part check dropped (the part is renamed onto it); the
// run-start test dropped (the interrupted finalize's own file stops the
// re-mux for good).
func TestFinalizeDoesNotReplaceAFileItDidNotWrite(t *testing.T) {
	started := time.Now().Add(-time.Hour)
	before, after := started.Add(-time.Hour), started.Add(time.Minute)

	single := func(t *testing.T, mtime time.Time) (existing, staged string, err error) {
		ffmpegPath, _ := requireFFmpegTools(t)
		w, db := testWorkerSetup(t)
		stagingDir, _ := muxFixtureJob(t, w, db, "j-guard")
		db.UpdateJobFields("j-guard", map[string]any{"download_started_at": started.UTC().Format(time.RFC3339)})
		job, _ := db.GetJob("j-guard")
		jobCtx := w.buildJobContext(job)
		existing = filepath.Join(jobCtx.OutputDir, jobCtx.Filename+".mp4")
		writeTestFile(t, existing, "already here")
		if err := os.Chtimes(existing, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		staged = filepath.Join(stagingDir, "video.mp4")
		writeMuxFixture(t, ffmpegPath, staged, 2)
		return existing, staged, w.orchestrator.muxFromStaging(context.Background(), jobCtx)
	}

	t.Run("single file from before the recording: refused, staging kept", func(t *testing.T) {
		existing, staged, err := single(t, before)
		if err == nil || !strings.Contains(err.Error(), "is not this job's recording") {
			t.Fatalf("finalize = %v, want it refused for the file already there", err)
		}
		if got := readTestFile(t, existing); got != "already here" {
			t.Errorf("the file already there was replaced (%d bytes now)", len(got))
		}
		if !fileExists(staged) {
			t.Error("the staged recording is gone")
		}
	})

	t.Run("single file this run wrote: replaced", func(t *testing.T) {
		existing, _, err := single(t, after)
		if err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if got := readTestFile(t, existing); got == "already here" {
			t.Error("the interrupted finalize's file was not replaced by the re-mux")
		}
	})

	t.Run("one part: it keeps its part name, the finalize goes on", func(t *testing.T) {
		w, db := testWorkerSetup(t)
		root := t.TempDir()
		existing := filepath.Join(root, "Show.mp4")
		writeTestFile(t, existing, "already here")
		if err := os.Chtimes(existing, before, before); err != nil {
			t.Fatal(err)
		}
		job := &database.Job{ID: "j-part", VideoID: "j-part", URL: "https://twitch.tv/s", Platform: "twitch", Status: database.StatusDownloading}
		if _, err := db.AddJob(job); err != nil {
			t.Fatal(err)
		}
		db.UpdateJobFields(job.ID, map[string]any{"download_started_at": started.UTC().Format(time.RFC3339)})
		part := filepath.Join(root, "Show - part1.mp4")
		writeTestFile(t, part, "part one")
		seg := database.Segment{JobID: job.ID, SegmentIndex: 0, Filename: "Show - part1.mp4", FilePath: part, Quality: "720p"}
		jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, StagingDir: t.TempDir(),
			OutputDir: root, Filename: "Show", Logger: &discardLogger{}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // no network for the thumbnail fallback inside copyAssets
		if err := w.orchestrator.finalizeMultiSegmentJob(ctx, jobCtx, []database.Segment{seg}, nil); err != nil {
			t.Fatalf("finalizeMultiSegmentJob: %v", err)
		}
		if got := readTestFile(t, existing); got != "already here" {
			t.Errorf("the file already there now reads %q", got)
		}
		if got := readTestFile(t, part); got != "part one" {
			t.Errorf("the part reads %q at its part name", got)
		}
	})
}
