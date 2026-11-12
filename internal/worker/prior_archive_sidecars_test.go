package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// offeredOutput is every path the Files sweep offers under outputDir.
func offeredOutput(t *testing.T, db *database.Database, outputDir string) []string {
	t.Helper()
	cfg := &config.MoomboxConfig{}
	cfg.Paths.OutputDirectory = outputDir
	entries, err := scanOutputOrphans(db, cfg)
	if err != nil {
		t.Fatalf("scanOutputOrphans: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.RelPath)
	}
	return names
}

// The earlier archive's chat and description go aside with it (R2-W1): the
// reset clears their columns, and the new recording's finalize writes its own
// chat at the same name. A live chat cannot be fetched again.
//
// Mutants: priorArchiveSidecars left out of jobFileLocations (the sweep
// offers the chat and description after the reset); moveSidecarsAside not
// called (the old chat is replaced by the new one, and no chat is beside the
// set-aside archive).
func TestReinitializeKeepsTheArchivesChatAndDescription(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)
	o := w.orchestrator
	stagingDir, outputDir := muxFixtureJob(t, w, db, "j-side")
	finalize := func(chat string) *database.Job {
		t.Helper()
		writeTestFile(t, filepath.Join(stagingDir, "chat.json"), chat)
		writeMuxFixture(t, ffmpegPath, filepath.Join(stagingDir, "video.mp4"), 2)
		job, _ := db.GetJob("j-side")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := o.muxFromStaging(ctx, w.buildJobContext(job)); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		job, _ = db.GetJob("j-side")
		return job
	}

	first := finalize(`{"messages":["old"]}`)
	archive := first.OutputFile
	stem := strings.TrimSuffix(archive, ".mp4")
	if first.ChatFile != stem+".chat.json" {
		t.Fatalf("precondition: chat_file = %q, want it beside the archive", first.ChatFile)
	}
	writeTestFile(t, stem+".description", "old description")

	db.UpdateJobFields("j-side", map[string]any{"status": database.StatusError})
	w.reinitializeNow("j-side", database.StatusError)
	if job, _ := db.GetJob("j-side"); job.ChatFile != "" || job.PriorOutputFile != archive {
		t.Fatalf("after the reset chat_file = %q, prior_output_file = %q", job.ChatFile, job.PriorOutputFile)
	}
	if got := offeredOutput(t, db, outputDir); len(got) != 0 {
		t.Errorf("the Files sweep offers %v after the reset; they belong to the archive the row keeps", got)
	}

	second := finalize(`{"messages":["new"]}`)
	if second.OutputFile != archive {
		t.Fatalf("the new recording resolved to %q, not %q: the test no longer covers the collision", second.OutputFile, archive)
	}
	asideStem := strings.TrimSuffix(second.PriorOutputFile, ".mp4")
	if got := readTestFile(t, asideStem+".chat.json"); !strings.Contains(got, "old") {
		t.Errorf("the chat beside the set-aside archive reads %q, want the earlier recording's", got)
	}
	if got := readTestFile(t, stem+".chat.json"); !strings.Contains(got, "new") {
		t.Errorf("the new archive's chat reads %q, want the new recording's", got)
	}
	if got := readTestFile(t, asideStem+".description"); got != "old description" {
		t.Errorf("the description beside the set-aside archive reads %q", got)
	}
	if got := offeredOutput(t, db, outputDir); len(got) != 0 {
		t.Errorf("the Files sweep offers %v after the new recording finished", got)
	}
}

// An earlier archive that cannot be renamed aside is never written over, and
// stops only the finalize that would land on it.
//
// Mutants: the single-file check dropped (the new mux replaces the archive);
// the single-part check dropped (the part is renamed onto it).
func TestStuckPriorArchiveIsNotReplaced(t *testing.T) {
	failRename := func(t *testing.T) {
		orig := renameArchive
		renameArchive = func(string, string) error { return errors.New("locked") }
		t.Cleanup(func() { renameArchive = orig })
	}

	t.Run("single file: the finalize stops, staging kept", func(t *testing.T) {
		ffmpegPath, _ := requireFFmpegTools(t)
		w, db := testWorkerSetup(t)
		stagingDir, outputDir := muxFixtureJob(t, w, db, "j-stuck")
		job, _ := db.GetJob("j-stuck")
		jobCtx := w.buildJobContext(job)
		prior := filepath.Join(jobCtx.OutputDir, jobCtx.Filename+".mp4")
		if filepath.Dir(prior) != outputDir {
			t.Logf("archive directory: %s", filepath.Dir(prior))
		}
		writeTestFile(t, prior, "the earlier archive")
		db.UpdateJobFields("j-stuck", map[string]any{"prior_output_file": prior})
		writeMuxFixture(t, ffmpegPath, filepath.Join(stagingDir, "video.mp4"), 2)
		failRename(t)

		err := w.orchestrator.muxFromStaging(context.Background(), jobCtx)
		if err == nil || !strings.Contains(err.Error(), "set the earlier archive aside") {
			t.Fatalf("finalize = %v, want it refused for the earlier archive", err)
		}
		if got := readTestFile(t, prior); got != "the earlier archive" {
			t.Errorf("the earlier archive now reads %d bytes of something else", len(got))
		}
		if !fileExists(filepath.Join(stagingDir, "video.mp4")) {
			t.Error("the staged recording is gone")
		}
	})

	t.Run("one part: it keeps its part name, the finalize goes on", func(t *testing.T) {
		w, db := testWorkerSetup(t)
		o := w.orchestrator
		root := t.TempDir()
		prior := filepath.Join(root, "Show.mp4")
		writeTestFile(t, prior, "the earlier archive")
		job := &database.Job{ID: "j-part", VideoID: "j-part", URL: "https://twitch.tv/s", Platform: "twitch",
			Status: database.StatusDownloading, PriorOutputFile: prior}
		if _, err := db.AddJob(job); err != nil {
			t.Fatal(err)
		}
		db.UpdateJobFields(job.ID, map[string]any{"prior_output_file": prior})
		part := filepath.Join(root, "Show - part1.mp4")
		writeTestFile(t, part, "part one")
		seg := database.Segment{JobID: job.ID, SegmentIndex: 0, Filename: "Show - part1.mp4", FilePath: part, Quality: "720p"}
		jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, StagingDir: t.TempDir(),
			OutputDir: root, Filename: "Show", Logger: &discardLogger{}}
		failRename(t)

		o.setAsidePriorArchive(jobCtx)
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // no network for the thumbnail fallback inside copyAssets
		if err := o.finalizeMultiSegmentJob(ctx, jobCtx, []database.Segment{seg}, nil); err != nil {
			t.Fatalf("finalizeMultiSegmentJob: %v", err)
		}
		if got := readTestFile(t, prior); got != "the earlier archive" {
			t.Errorf("the earlier archive now reads %q", got)
		}
		if got := readTestFile(t, part); got != "part one" {
			t.Errorf("the part reads %q at its part name", got)
		}
	})
}

// A reset whose claim is refused renames nothing; one that applies sets the
// older archive aside under the stem of the archive the row now keeps, where
// the row still owns it (state-machine rule 3).
//
// Mutants: the rename done while the fields are built (the refused reset has
// already moved the file); the older archive set aside under its own stem
// (the sweep offers it).
func TestResetSetsTheOlderArchiveAsideOnlyAfterItsClaim(t *testing.T) {
	setup := func(t *testing.T) (w *DownloadWorker, db *database.Database, dir, newer, older string) {
		w, db = testWorkerSetup(t)
		_, dir = muxFixtureJob(t, w, db, "j-two")
		newer, older = filepath.Join(dir, "New.mp4"), filepath.Join(dir, "Old.mp4")
		writeTestFile(t, newer, "newer")
		writeTestFile(t, older, "older")
		writeTestFile(t, filepath.Join(dir, "Old.chat.json"), "older chat")
		db.UpdateJobFields("j-two", map[string]any{
			"status": database.StatusError, "output_file": newer, "prior_output_file": older})
		return
	}

	t.Run("refused", func(t *testing.T) {
		w, db, _, _, older := setup(t)
		orig := beforeResetClaim
		beforeResetClaim = func(id string) { db.UpdateJobFields(id, map[string]any{"status": database.StatusCancelled}) }
		t.Cleanup(func() { beforeResetClaim = orig })
		w.reinitializeNow("j-two", database.StatusError)
		job, _ := db.GetJob("j-two")
		if job.PriorOutputFile != older || !fileExists(older) {
			t.Errorf("a refused reset left prior_output_file = %q, file there = %v; want both untouched", job.PriorOutputFile, fileExists(older))
		}
	})

	t.Run("applied", func(t *testing.T) {
		w, db, dir, newer, older := setup(t)
		w.reinitializeNow("j-two", database.StatusError)
		job, _ := db.GetJob("j-two")
		if job.PriorOutputFile != newer {
			t.Fatalf("prior_output_file = %q, want the newer archive %q", job.PriorOutputFile, newer)
		}
		if fileExists(older) {
			t.Fatal("the older archive is still at its own name")
		}
		asides, _ := filepath.Glob(filepath.Join(dir, "New.restart-*.mp4"))
		if len(asides) != 1 || readTestFile(t, asides[0]) != "older" {
			t.Fatalf("set-aside files under the newer stem = %v, want the older archive", asides)
		}
		if got := readTestFile(t, strings.TrimSuffix(asides[0], ".mp4")+".chat.json"); got != "older chat" {
			t.Errorf("the older archive's chat beside it reads %q", got)
		}
		if got := offeredOutput(t, db, dir); len(got) != 0 {
			t.Errorf("the Files sweep offers %v; the row still owns both archives", got)
		}
		// Once the newer archive is set aside too, the older one is still the
		// row's: it carries the stem the row's archive was set aside from.
		newerAside := filepath.Join(dir, "New.restart-1.mp4")
		if err := os.Rename(newer, newerAside); err != nil {
			t.Fatal(err)
		}
		db.UpdateJobFields("j-two", map[string]any{"prior_output_file": newerAside})
		if got := offeredOutput(t, db, dir); len(got) != 0 {
			t.Errorf("the Files sweep offers %v once the newer archive is set aside", got)
		}
	})
}

// An earlier archive that is not at the path the row names: adopted when a
// set-aside file with its stem is beside it (a crash between the rename and
// the column write), dropped only when its directory is readable and it is
// not there, and kept when the directory cannot be read (an unmounted
// volume).
//
// Mutants: no adoption (the crash case clears the column); the directory
// check dropped (the unmounted case clears it).
func TestSetAsidePriorArchiveWhenTheFileIsNotThere(t *testing.T) {
	w, db := testWorkerSetup(t)
	dir := t.TempDir()
	aside := filepath.Join(dir, "crashed.restart-1700000000.mp4")
	writeTestFile(t, aside, "x")
	writeTestFile(t, filepath.Join(dir, "other.restart-1700000000.mp4"), "x")
	for _, tc := range []struct{ name, prior, want string }{
		{"renamed, column not written", filepath.Join(dir, "crashed.mp4"), aside},
		{"removed by hand", filepath.Join(dir, "removed.mp4"), ""},
		{"volume not mounted", filepath.Join(dir, "unmounted", "show.mp4"), filepath.Join(dir, "unmounted", "show.mp4")},
	} {
		id := "j-" + strings.ReplaceAll(tc.name, " ", "-")
		job := &database.Job{ID: id, VideoID: id, URL: "u", Platform: "youtube", Status: database.StatusMuxing}
		if _, err := db.AddJob(job); err != nil {
			t.Fatal(err)
		}
		db.UpdateJobFields(id, map[string]any{"prior_output_file": tc.prior})
		w.orchestrator.setAsidePriorArchive(&JobContext{Job: job})
		if got, _ := db.GetJob(id); got.PriorOutputFile != tc.want {
			t.Errorf("%s: prior_output_file = %q, want %q", tc.name, got.PriorOutputFile, tc.want)
		}
	}
}

// The Files sweep drops a set-aside archive the operator removed by hand from
// a settled row, and from no other.
//
// Mutants: the status guard dropped (the waiting row loses its column); the
// set-aside check dropped (a pending archive is the finalize's to settle).
func TestSweepForgetsARemovedSetAsideArchive(t *testing.T) {
	_, db := testWorkerSetup(t)
	dir := t.TempDir()
	gone := filepath.Join(dir, "show.restart-1700000000.mp4")
	there := filepath.Join(dir, "kept.restart-1700000000.mp4")
	writeTestFile(t, there, "x")
	cases := []struct {
		id     string
		status database.JobStatus
		prior  string
		want   string
	}{
		{"finished-gone", database.StatusFinished, gone, ""},
		{"finished-there", database.StatusFinished, there, there},
		{"upcoming-gone", database.StatusUpcoming, gone, gone},
		{"finished-pending", database.StatusFinished, filepath.Join(dir, "pending.mp4"), filepath.Join(dir, "pending.mp4")},
	}
	for _, tc := range cases {
		if _, err := db.AddJob(&database.Job{ID: tc.id, VideoID: tc.id, URL: "u", Platform: "youtube", Status: tc.status}); err != nil {
			t.Fatal(err)
		}
		db.UpdateJobFields(tc.id, map[string]any{"prior_output_file": tc.prior})
	}
	offeredOutput(t, db, dir)
	for _, tc := range cases {
		if got, _ := db.GetJob(tc.id); got.PriorOutputFile != tc.want {
			t.Errorf("%s: prior_output_file = %q, want %q", tc.id, got.PriorOutputFile, tc.want)
		}
	}
}

// An archive tree that was moved, with paths.output_directory repointed at it
// while the old directory still exists: the row's earlier archive is at the
// re-rooted path, where the Files sweep counts it as the row's
// (rowAbsoluteLocations). The sweep must not drop the column for it, and a
// finalize sets it aside where it is.
//
// Mutants: either reader deciding from the stored spelling alone (the sweep
// clears the column; the finalize clears it and leaves the file at the name
// the new recording may take).
func TestPriorArchiveInAMovedArchiveTree(t *testing.T) {
	w, db := testWorkerSetup(t)
	oldDir, newDir := t.TempDir(), t.TempDir()
	add := func(id string, status database.JobStatus, prior string) *database.Job {
		job := &database.Job{ID: id, VideoID: id, URL: "u", Platform: "youtube", Status: status, OutputDirectory: oldDir}
		if _, err := db.AddJob(job); err != nil {
			t.Fatal(err)
		}
		db.UpdateJobFields(id, map[string]any{"prior_output_file": prior})
		return job
	}

	writeTestFile(t, filepath.Join(newDir, "show.restart-1700000000.mp4"), "x")
	stored := filepath.Join(oldDir, "show.restart-1700000000.mp4")
	add("j-swept", database.StatusFinished, stored)
	if got := offeredOutput(t, db, newDir); len(got) != 0 {
		t.Errorf("the Files sweep offers %v; the row owns the moved file", got)
	}
	if got, _ := db.GetJob("j-swept"); got.PriorOutputFile != stored {
		t.Errorf("the sweep changed prior_output_file to %q, want %q kept", got.PriorOutputFile, stored)
	}

	moved := filepath.Join(newDir, "pending.mp4")
	writeTestFile(t, moved, "earlier")
	job := add("j-finalized", database.StatusMuxing, filepath.Join(oldDir, "pending.mp4"))
	w.orchestrator.setAsidePriorArchive(&JobContext{Job: job, GlobalOutputDir: newDir})
	got, _ := db.GetJob("j-finalized")
	if filepath.Dir(got.PriorOutputFile) != newDir || !priorArchiveSetAside(got.PriorOutputFile) ||
		readTestFile(t, got.PriorOutputFile) != "earlier" || fileExists(moved) {
		t.Errorf("prior_output_file = %q, file still at %s = %v; want it set aside in the moved tree", got.PriorOutputFile, moved, fileExists(moved))
	}
}

// A second reset of a row whose earlier archive was already set aside, after a
// recording that took another name (the title changed): the set-aside file
// shares no stem with the archive the column moves on to, so it is renamed
// under that archive's stem and stays the row's.
//
// Mutant: a set-aside older archive left as it is (the Files sweep offers it
// under Delete All, with nothing logged).
func TestResetKeepsAnAlreadySetAsideArchiveOwned(t *testing.T) {
	w, db := testWorkerSetup(t)
	_, dir := muxFixtureJob(t, w, db, "j-twice")
	newer, older := filepath.Join(dir, "New title.mp4"), filepath.Join(dir, "Old title.restart-1700000000.mp4")
	writeTestFile(t, newer, "newer")
	writeTestFile(t, older, "older")
	db.UpdateJobFields("j-twice", map[string]any{
		"status": database.StatusError, "output_file": newer, "prior_output_file": older})
	w.reinitializeNow("j-twice", database.StatusError)
	if job, _ := db.GetJob("j-twice"); job.PriorOutputFile != newer {
		t.Fatalf("prior_output_file = %q, want %q", job.PriorOutputFile, newer)
	}
	asides, _ := filepath.Glob(filepath.Join(dir, "New title.restart-*.mp4"))
	if len(asides) != 1 || readTestFile(t, asides[0]) != "older" || fileExists(older) {
		t.Fatalf("set-aside files under the newer stem = %v, want the older archive moved there", asides)
	}
	if got := offeredOutput(t, db, dir); len(got) != 0 {
		t.Errorf("the Files sweep offers %v; the row still owns both archives", got)
	}
}
