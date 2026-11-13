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
// the file and its sidecars are exactly where they were, one Warn says so,
// and a marker under the archive's own name keeps them the job's: the Files
// sweep offers none of them until the job row is deleted. A job's only part
// under the archive's name is left in place the same way, not deleted as a
// part.
//
// Mutants: the set-aside marker left behind on a failed rename; the failure
// not logged; the sidecars renamed although the archive was not; the in-place
// marker not written, or ownerOf asking the marker only of set-aside names
// (the sweep offers the archive); clearJobParts not told what was kept (the
// one-part archive is deleted).
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
	if got := strings.Join(names, "|"); got != "Show.chat.json|Show.description|Show.mp4|Show.owner" {
		t.Errorf("the output directory holds %s; want the archive and its files as they were, and the marker", got)
	}
	if len(log.warns) != 1 || !strings.Contains(log.warns[0], "could not be set aside") {
		t.Errorf("warnings = %q, want one naming the archive that was not set aside", log.warns)
	}
	if got := offeredOutput(t, db, outputDir); len(got) != 0 {
		t.Errorf("the Files sweep offers %v while the job exists", got)
	}
	if err := db.DeleteJob("j-locked"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(offeredOutput(t, db, outputDir), "|"); got != "Show.chat.json|Show.description|Show.mp4|Show.owner" {
		t.Errorf("with the job row gone the sweep offers %s, want the files and their marker", got)
	}

	t.Run("one part under the archive's name", func(t *testing.T) {
		outputDir, archive := archivedErrorJob(t, w, db, "j-locked-part")
		chat := filepath.Join(outputDir, "Show.chat.json")
		if err := db.AddSegment(&database.Segment{JobID: "j-locked-part", SegmentIndex: 0, Filename: "Show.mp4", FilePath: archive, ChatFile: chat}); err != nil {
			t.Fatal(err)
		}
		w.reinitializeNow("j-locked-part", database.StatusError)
		if got := readTestFile(t, archive); got != "the earlier recording" {
			t.Errorf("the archive reads %q where it was", got)
		}
		if got := readTestFile(t, chat); got != "the earlier chat" {
			t.Errorf("its chat reads %q where it was", got)
		}
	})
}

// A job that ended with one part has it under the plain archive name, on a
// segment row (renameSinglePartToPlain). An operator's Reinitialize sets that
// archive aside with its chat like any single-file archive, and the confirm
// counts it as the existing archive, not as a part it deletes.
//
// Mutants: the one-part rule dropped from archiveSetAsideByReset (the reset
// deletes the archive as a part); the preview still counting it under
// RecordedParts.
func TestReinitializeSetsAOnePartArchiveAside(t *testing.T) {
	w, db := testWorkerSetup(t)
	outputDir, archive := archivedErrorJob(t, w, db, "j-one")
	if err := db.AddSegment(&database.Segment{JobID: "j-one", SegmentIndex: 0, Filename: "Show.mp4", FilePath: archive,
		ChatFile: filepath.Join(outputDir, "Show.chat.json")}); err != nil {
		t.Fatal(err)
	}
	job, _ := db.GetJob("j-one")
	if p := PreviewReinitialize(db, t.TempDir(), job); p.ExistingArchives != 1 || p.RecordedParts != 0 {
		t.Errorf("preview = %+v, want one existing archive and no recorded part", p)
	}

	w.reinitializeNow("j-one", database.StatusError)

	asides := asideArchives(t, outputDir)
	if len(asides) != 1 {
		t.Fatalf("set-aside archives = %v, want one", asides)
	}
	if got := readTestFile(t, asides[0]); got != "the earlier recording" {
		t.Errorf("the aside reads %q", got)
	}
	if got := readTestFile(t, strings.TrimSuffix(asides[0], ".mp4")+".chat.json"); got != "the earlier chat" {
		t.Errorf("the aside's chat reads %q", got)
	}
	if segs, _ := db.GetSegments("j-one"); len(segs) != 0 {
		t.Errorf("the reset left %d segment rows", len(segs))
	}
}

// The recovered set-aside recordings already beside the archive were the
// job's through the archive's stem, which no row names after the reset: the
// reset marks each as the job's, so the Files sweep goes on not offering
// them. An archive in another container keeps its extension when it is set
// aside, is listed in the job's details, and takes the next counter when its
// set-aside name is taken in that extension.
//
// Mutants: adoptStemAsides not called (the sweep offers the recovered
// recording and its chat); ScanSetAsideArchives matching a fixed list of
// media extensions (the .mov is not listed); freeAsideName handing back the
// taken name (the set-aside would be reported as failed).
func TestResetKeepsWhatTheStemRuleOwned(t *testing.T) {
	w, db := testWorkerSetup(t)
	_, outputDir := muxFixtureJob(t, w, db, "j-mov")
	archive := filepath.Join(outputDir, "Show.mov")
	writeTestFile(t, archive, "imported")
	writeTestFile(t, filepath.Join(outputDir, "Show.restart-1700000000.mp4"), "recovered")
	writeTestFile(t, filepath.Join(outputDir, "Show.restart-1700000000.chat.json"), "recovered chat")
	db.UpdateJobFields("j-mov", map[string]any{"status": database.StatusError, "output_file": archive, "filename": "Show.mov"})
	if got := offeredOutput(t, db, outputDir); len(got) != 0 {
		t.Fatalf("before the reset the sweep offers %v", got)
	}

	w.reinitializeNow("j-mov", database.StatusError)

	if got := offeredOutput(t, db, outputDir); len(got) != 0 {
		t.Errorf("after the reset the sweep offers %v; they are the job's", got)
	}
	job, _ := db.GetJob("j-mov")
	var mov []string
	for _, a := range ScanSetAsideArchives(job, "", "") {
		if strings.HasSuffix(a.Name, ".mov") {
			mov = append(mov, a.Name)
		}
		if strings.HasSuffix(a.Name, ".json") || strings.HasSuffix(a.Name, asideOwnerSuffix) {
			t.Errorf("job details list %s as an archive", a.Name)
		}
	}
	if len(mov) != 1 {
		t.Errorf("job details list the set-aside .mov %d times, want once", len(mov))
	}

	writeTestFile(t, filepath.Join(outputDir, "Other.restart-5.mkv"), "taken")
	if got, ok := freeAsideName(outputDir, "Other", ".mkv", "5", "j"); !ok || filepath.Base(got) != "Other.restart-5-2.mkv" {
		t.Errorf("freeAsideName = (%q, %v), want the next counter in the archive's own extension", got, ok)
	}
}

// failRenameArchive makes every set-aside rename fail for the rest of t.
func failRenameArchive(t *testing.T) {
	t.Helper()
	orig := renameArchive
	renameArchive = func(string, string) error { return errors.New("locked") }
	t.Cleanup(func() { renameArchive = orig })
}

// setAsideFiles returns the files in dir under a set-aside name whose name
// ends in suffix ("" for all of them).
func setAsideFiles(t *testing.T, dir, suffix string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".restart-") && strings.HasSuffix(e.Name(), suffix) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// soleSetAside is the one file setAsideFiles finds.
func soleSetAside(t *testing.T, dir, suffix string) string {
	t.Helper()
	got := setAsideFiles(t, dir, suffix)
	if len(got) != 1 {
		t.Fatalf("set-aside files ending %s = %v, want one", suffix, got)
	}
	return got[0]
}

// A finalize does not copy its chat capture over a chat it did not write (the
// one a Reinitialize could not set aside, a deleted row's): that chat is set
// aside under a set-aside name with its raw sidecar and a marker naming the
// job, and the capture is then copied to the name. Only when the rename fails
// is the chat already there kept and the capture left in staging, as when the
// copy fails. The chat the row names is replaced as before.
//
// Mutants: the check dropped from copyAssets (the earlier chat is replaced);
// the raw sidecar left behind (the new raw replaces the earlier one); no
// marker (Files offers the earlier chat while the job exists); a failed
// rename ignored (the earlier chat is replaced after all); chat_file not
// counted as the row's own (every re-finalize sets its own chat aside).
func TestFinalizeSetsAsideAChatItDidNotWrite(t *testing.T) {
	started := time.Now().Add(-time.Hour)
	run := func(t *testing.T, named bool) (outputDir string, updates map[string]any) {
		w, db := testWorkerSetup(t)
		stagingDir, outputDir := muxFixtureJob(t, w, db, "j-chat")
		chat := filepath.Join(outputDir, "Show.chat.json")
		writeTestFile(t, chat, "the earlier chat")
		writeTestFile(t, filepath.Join(outputDir, "Show.chat.raw.jsonl"), "the earlier raw")
		for _, p := range []string{chat, filepath.Join(outputDir, "Show.chat.raw.jsonl")} {
			if err := os.Chtimes(p, started.Add(-time.Hour), started.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		writeTestFile(t, filepath.Join(stagingDir, "chat.json"), "the new chat")
		writeTestFile(t, filepath.Join(stagingDir, "chat.raw.jsonl"), "the new raw")
		fields := map[string]any{"download_started_at": started.UTC().Format(time.RFC3339)}
		if named {
			fields["chat_file"] = chat
		}
		job := db.UpdateJobFields("j-chat", fields)
		job.VideoID = "" // no thumbnail fetch
		jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, StagingDir: stagingDir, OutputDir: outputDir, Filename: "Show", Logger: &discardLogger{}}
		updates = map[string]any{}
		w.orchestrator.copyAssets(context.Background(), jobCtx, outputDir, "Show", "Show", updates, false)
		return outputDir, updates
	}
	t.Run("from before the recording: set aside, the capture copied", func(t *testing.T) {
		outputDir, updates := run(t, false)
		aside := soleSetAside(t, outputDir, ".chat.json")
		if got := readTestFile(t, aside); got != "the earlier chat" {
			t.Errorf("the set-aside chat reads %q", got)
		}
		if got := readTestFile(t, asideBase(aside)+".chat.raw.jsonl"); got != "the earlier raw" {
			t.Errorf("its raw sidecar reads %q beside it", got)
		}
		if got := asideOwnerID(aside); got != "j-chat" {
			t.Errorf("the set-aside chat's marker names %q, want the finishing job", got)
		}
		chat := filepath.Join(outputDir, "Show.chat.json")
		if got := readTestFile(t, chat); got != "the new chat" {
			t.Errorf("the chat at the name reads %q, want the capture", got)
		}
		if got := readTestFile(t, filepath.Join(outputDir, "Show.chat.raw.jsonl")); got != "the new raw" {
			t.Errorf("the raw sidecar at the name reads %q, want the capture's", got)
		}
		if updates["chat_file"] != chat {
			t.Errorf("chat_file = %v, want the copied capture", updates["chat_file"])
		}
	})
	t.Run("it cannot be renamed: kept, the capture stays in staging", func(t *testing.T) {
		failRenameArchive(t)
		outputDir, updates := run(t, false)
		if got := readTestFile(t, filepath.Join(outputDir, "Show.chat.json")); got != "the earlier chat" {
			t.Errorf("the chat already there now reads %q", got)
		}
		if got := readTestFile(t, filepath.Join(outputDir, "Show.chat.raw.jsonl")); got != "the earlier raw" {
			t.Errorf("its raw sidecar now reads %q", got)
		}
		if updates["chat_status"] != database.ChatIncomplete {
			t.Errorf("chat_status = %v, want incomplete so the capture is kept in staging", updates["chat_status"])
		}
	})
	t.Run("the row's own: replaced", func(t *testing.T) {
		outputDir, updates := run(t, true)
		chat := filepath.Join(outputDir, "Show.chat.json")
		if got := readTestFile(t, chat); got != "the new chat" {
			t.Errorf("the row's chat reads %q after the finalize", got)
		}
		if updates["chat_file"] != chat {
			t.Errorf("chat_file = %v", updates["chat_file"])
		}
		if got := setAsideFiles(t, outputDir, ""); len(got) != 0 {
			t.Errorf("the row's own chat was set aside: %v", got)
		}
	})
}

// TestFinalizeSetsAsideAFileItDidNotWrite: a finalize whose archive name is
// held by a file from before its recording that the row does not name (the
// archive a reset could not rename, a deleted row's kept file under a video
// added again) sets that file aside with its sidecars, under a marker naming
// the finishing job, says so in one Warn, and writes its own archive. Only
// when the rename fails does it stop with the staging kept, and a job's only
// part keeps its part name. A file the finalizing run wrote itself — a
// finalize interrupted between its copy and its Finished write — is replaced
// as before, and so is the file a part row names.
//
// Mutants: the single-file check dropped (the mux replaces the earlier
// archive); the set-aside's error ignored (the same, when the rename fails);
// refusing instead of setting aside (a whole download ends in Error); the
// sidecars not taken (the earlier chat is replaced by the new one); no marker
// (Files offers the set-aside archive under Delete All while the job exists);
// the single-part check dropped (the part is renamed onto the file); the
// run-start test dropped (the interrupted finalize's own file is set aside,
// one more copy per re-mux); part rows not counted as the job's own (a
// re-finalize sets the job's only part aside).
func TestFinalizeSetsAsideAFileItDidNotWrite(t *testing.T) {
	started := time.Now().Add(-time.Hour)
	before, after := started.Add(-time.Hour), started.Add(time.Minute)

	single := func(t *testing.T, mtime time.Time) (db *database.Database, log *warnCaptureLogger, existing, staged string, err error) {
		ffmpegPath, _ := requireFFmpegTools(t)
		w, db := testWorkerSetup(t)
		log = &warnCaptureLogger{}
		w.orchestrator.logger = log
		stagingDir, _ := muxFixtureJob(t, w, db, "j-guard")
		db.UpdateJobFields("j-guard", map[string]any{"download_started_at": started.UTC().Format(time.RFC3339)})
		job, _ := db.GetJob("j-guard")
		jobCtx := w.buildJobContext(job)
		existing = filepath.Join(jobCtx.OutputDir, jobCtx.Filename+".mp4")
		writeTestFile(t, existing, "already here")
		writeTestFile(t, asideBase(existing)+".chat.json", "the earlier chat")
		for _, p := range []string{existing, asideBase(existing) + ".chat.json"} {
			if err := os.Chtimes(p, mtime, mtime); err != nil {
				t.Fatal(err)
			}
		}
		staged = filepath.Join(stagingDir, "video.mp4")
		writeMuxFixture(t, ffmpegPath, staged, 2)
		return db, log, existing, staged, w.orchestrator.muxFromStaging(context.Background(), jobCtx)
	}

	t.Run("single file from before the recording: set aside, the archive written", func(t *testing.T) {
		db, log, existing, _, err := single(t, before)
		if err != nil {
			t.Fatalf("finalize: %v", err)
		}
		aside := soleSetAside(t, filepath.Dir(existing), ".mp4")
		if got := readTestFile(t, aside); got != "already here" {
			t.Errorf("the set-aside file reads %d bytes, want the file that was there", len(got))
		}
		if got := readTestFile(t, asideBase(aside)+".chat.json"); got != "the earlier chat" {
			t.Errorf("the set-aside file's chat reads %q beside it", got)
		}
		if got := asideOwnerID(aside); got != "j-guard" {
			t.Errorf("the marker names %q, want the finishing job", got)
		}
		if got := readTestFile(t, existing); got == "already here" || got == "" {
			t.Error("the finalize did not write its own archive at the name")
		}
		var said int
		for _, w := range log.warns {
			if strings.Contains(w, "was set aside") {
				said++
			}
		}
		if said != 1 {
			t.Errorf("warnings = %q, want one saying the file was set aside", log.warns)
		}
		if got := offeredOutput(t, db, filepath.Dir(existing)); len(got) != 0 {
			t.Errorf("the Files sweep offers %v while the job exists", got)
		}
	})

	t.Run("it cannot be renamed: refused, staging kept", func(t *testing.T) {
		failRenameArchive(t)
		_, _, existing, staged, err := single(t, before)
		if err == nil || !strings.Contains(err.Error(), "is not this job's") {
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
		_, _, existing, _, err := single(t, after)
		if err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if got := readTestFile(t, existing); got == "already here" {
			t.Error("the interrupted finalize's file was not replaced by the re-mux")
		}
		if got := setAsideFiles(t, filepath.Dir(existing), ""); len(got) != 0 {
			t.Errorf("the run's own file was set aside: %v", got)
		}
	})

	// onePart finalizes a job whose only part is partName, with a file from
	// before the recording at Show.mp4.
	onePart := func(t *testing.T, partName string) (root string) {
		w, db := testWorkerSetup(t)
		root = t.TempDir()
		job := &database.Job{ID: "j-part", VideoID: "j-part", URL: "https://twitch.tv/s", Platform: "twitch", Status: database.StatusDownloading}
		if _, err := db.AddJob(job); err != nil {
			t.Fatal(err)
		}
		db.UpdateJobFields(job.ID, map[string]any{"download_started_at": started.UTC().Format(time.RFC3339)})
		part := filepath.Join(root, partName)
		writeTestFile(t, part, "part one")
		existing := filepath.Join(root, "Show.mp4")
		if part != existing {
			writeTestFile(t, existing, "already here")
		}
		if err := os.Chtimes(existing, before, before); err != nil {
			t.Fatal(err)
		}
		seg := database.Segment{JobID: job.ID, SegmentIndex: 0, Filename: partName, FilePath: part, Quality: "720p"}
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
		return root
	}

	t.Run("one part: the file is set aside, the part takes the name", func(t *testing.T) {
		root := onePart(t, "Show - part1.mp4")
		if got := readTestFile(t, soleSetAside(t, root, ".mp4")); got != "already here" {
			t.Errorf("the set-aside file reads %q", got)
		}
		if got := readTestFile(t, filepath.Join(root, "Show.mp4")); got != "part one" {
			t.Errorf("Show.mp4 reads %q, want the job's only part", got)
		}
	})

	t.Run("one part, the file cannot be renamed: the part keeps its part name", func(t *testing.T) {
		failRenameArchive(t)
		root := onePart(t, "Show - part1.mp4")
		if got := readTestFile(t, filepath.Join(root, "Show.mp4")); got != "already here" {
			t.Errorf("the file already there now reads %q", got)
		}
		if got := readTestFile(t, filepath.Join(root, "Show - part1.mp4")); got != "part one" {
			t.Errorf("the part reads %q at its part name", got)
		}
	})

	t.Run("one part already at the name: it is the job's, and stays", func(t *testing.T) {
		root := onePart(t, "Show.mp4")
		if got := setAsideFiles(t, root, ""); len(got) != 0 {
			t.Errorf("the job's own part was set aside: %v", got)
		}
		if got := readTestFile(t, filepath.Join(root, "Show.mp4")); got != "part one" {
			t.Errorf("Show.mp4 reads %q, want the job's only part", got)
		}
	})
}

// The Files sweep offers a marker whose job is gone, and only a marker: a
// *.owner file whose whole content is not the marker line is the operator's,
// whichever of the two marker names it has and wherever it is, a trims
// directory included, and is never listed, so never deleted by Delete All.
//
// Mutants: every *.owner file listed (the operator's are offered); the
// content rule dropped for the set-aside name alone; a marker with a CRLF
// line end not read as one (it is never offered, and owns nothing); a bare
// job ID read as a marker (a one-word file of the operator's is offered);
// the trims directory's any-extension rule left ahead of the marker rule.
func TestFilesSweepOffersOnlyMarkersMoomboxWrote(t *testing.T) {
	_, db := testWorkerSetup(t)
	outputDir := t.TempDir()
	// A job whose archive makes outputDir/trim a trims directory, where the
	// sweep offers a file of any extension.
	kept := &database.Job{ID: "j-kept", VideoID: "j-kept", URL: "u", Platform: "youtube", Status: database.StatusFinished,
		OutputFile: filepath.Join(outputDir, "Kept.mp4")}
	if _, err := db.AddJob(kept); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"Kept.mp4":                      "kept",
		"Gone.restart-1700000000.owner": asideMarkerPrefix + "j-gone\n",   // a set-aside archive's marker, its job deleted
		"Old.owner":                     asideMarkerPrefix + "j-gone\r\n", // an in-place marker, saved by an editor
		"notes.owner":                   "the owner of this folder is Wulf\n",
		"bare.owner":                    "j-gone\n",
		"Show.restart-1700000001.owner": asideMarkerPrefix + "line one\n" + asideMarkerPrefix + "line two\n",
		"empty.owner":                   "",
		"long.owner":                    asideMarkerPrefix + strings.Repeat("x", asideMarkerMaxID+1) + "\n",
		"trim/clip.owner":               "mine\n",
		"trim/left.bin":                 "an encode's leftover",
	} {
		writeTestFile(t, filepath.Join(outputDir, name), content)
	}
	if got := strings.Join(offeredOutput(t, db, outputDir), "|"); got != "Gone.restart-1700000000.owner|Old.owner|left.bin" {
		t.Errorf("the sweep offers %s, want the two markers, the trim leftover and none of the operator's files", got)
	}
}
