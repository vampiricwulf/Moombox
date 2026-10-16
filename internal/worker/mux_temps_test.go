package worker

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
)

// goneRunTemp names the temporary file a crashed run (token 0badc0de) left
// for final.
func goneRunTemp(final string) string {
	return final[:len(final)-len(filepath.Ext(final))] + ".moombox-tmp-0badc0de-1" + filepath.Ext(final)
}

// TestTheBootReMuxAfterACrashLeavesOneGoodArchive is W28-A01's start-up half,
// end to end with a real FFmpeg: a Muxing row whose child died mid-mux is
// re-muxed from staging by the next start (enqueueExistingJobs → muxOnRestart
// → MuxJob), and what the crash left in the output directory is one of:
//
//   - the temporary file the dead run's FFmpeg was writing, which is what a
//     crash leaves now — the boot sweep removes it;
//   - a half-written file under the archive's own name, which is what a crash
//     of a release that let FFmpeg write that name left — the re-mux writes
//     its own temporary file and replaces it only with a copy that passed;
//   - both.
//
// Either way the row ends Finished over a whole archive, alone in the
// directory. The sweep runs ahead of the re-mux here; in production the two
// run side by side, which is safe because the sweep never takes this run's
// own temporary files (TestRemoveStaleMuxTempsTakesOnlyAGoneRunsTemporaryFiles,
// internal/engine).
//
// Mutants: muxTempRoots not walking each job's output_directory (the
// leftover outside ./output stays); RemoveStaleMuxTemps handing the engine
// sweep no roots (the leftover stays).
func TestTheBootReMuxAfterACrashLeavesOneGoodArchive(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	for _, tc := range []struct {
		name            string
		leftover, final bool
	}{
		{"beside a leftover temporary file", true, false},
		{"over a half-written archive", false, true},
		{"beside both", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			t.Cleanup(w.Stop)
			staging, outputDir := muxFixtureJob(t, w, db, "j-crashed")
			writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 3)
			job, _ := db.GetJob("j-crashed")
			final := filepath.Join(outputDir, w.buildJobContext(job).Filename+".mp4")

			if tc.leftover {
				if err := os.WriteFile(goneRunTemp(final), []byte("half a copy"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.final {
				if err := os.WriteFile(final, []byte("half a copy"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			RemoveStaleMuxTemps(db, "", discardLogger{})
			w.enqueueExistingJobs()
			w.Stop()

			fresh, _ := db.GetJob("j-crashed")
			if fresh == nil {
				t.Fatal("the row is gone after the boot re-mux")
			}
			if fresh.Status != database.StatusFinished || normalizePath(fresh.OutputFile) != normalizePath(final) {
				t.Fatalf("the boot re-mux left the row %v (error %q, output %q), want Finished over %s",
					fresh.Status, fresh.Error, fresh.OutputFile, final)
			}
			if probe := w.orchestrator.runFFprobe(context.Background(), final); probe == nil || probe.DurationSec < 2 {
				t.Errorf("the archive probes as %+v, want the whole 3s recording", probe)
			}
			if got := mp4sIn(t, outputDir); !slices.Equal(got, []string{filepath.Base(final)}) {
				t.Errorf("the output directory holds %v after the boot re-mux, want the archive alone", got)
			}
		})
	}
}

// TestMuxTempRootsCoverEveryDirectoryAnArchiveIsWrittenTo: the sweep walks
// the output directory and every job's own output_directory, once each, and
// reads the directory of every recorded archive and part file that lies
// outside those — a part stays beside the first part of its job wherever that
// was written, which a changed output_directory leaves behind — and the
// "trim" directory beside each, where a trim of that archive or part writes
// its encode. A directory is read one level, so a listed one does not stand
// for the trim directory inside it.
//
// Mutants: dropping the job output_directory trees (the override is not
// walked); dropping the recorded files' directories (the moved-away part
// directory is not read); dropping trimDirsOf (neither trim directory is
// read); deduplicating dirs by containment instead of equality (the trim
// directory inside a listed part directory is dropped).
func TestMuxTempRootsCoverEveryDirectoryAnArchiveIsWrittenTo(t *testing.T) {
	_, db := testWorkerSetup(t)
	base := t.TempDir()
	global := filepath.Join(base, "output")
	override := filepath.Join(base, "elsewhere")
	nested := filepath.Join(global, "channel")
	oldParts := filepath.Join(base, "old-output", "channel")
	oldArchive := filepath.Join(base, "older-output", "channel")
	for _, j := range []*database.Job{
		{ID: "a", VideoID: "a", URL: "u", Status: database.StatusFinished, OutputDirectory: override,
			OutputFile: filepath.Join(override, "A.mp4")},
		{ID: "b", VideoID: "b", URL: "u", Status: database.StatusFinished, OutputDirectory: nested,
			OutputFile: filepath.Join(nested, "B.mp4")},
		{ID: "c", VideoID: "c", URL: "u", Status: database.StatusMuxing, OutputDirectory: global},
		{ID: "d", VideoID: "d", URL: "u", Status: database.StatusFinished,
			OutputFile: filepath.Join(oldArchive, "D.mp4")},
	} {
		if _, err := db.AddJob(j); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AddSegment(&database.Segment{JobID: "c", SegmentIndex: 0, Filename: "C - part1.mp4",
		FilePath: filepath.Join(oldParts, "C - part1.mp4")}); err != nil {
		t.Fatal(err)
	}

	trees, dirs := muxTempRoots(db, global)
	slices.Sort(trees)
	if want := []string{override, global}; !slices.Equal(trees, sorted(want)) {
		t.Errorf("trees = %v, want %v (the global directory and the override, the nested one inside the global)", trees, sorted(want))
	}
	want := sorted([]string{oldParts, filepath.Join(oldParts, "trim"), oldArchive, filepath.Join(oldArchive, "trim")})
	if got := sorted(dirs); !slices.Equal(got, want) {
		t.Errorf("dirs = %v, want the part and archive directories outside every tree and the trim directory in each, %v", got, want)
	}
}

// TestRemoveStaleMuxTempsSweepsACrashedTrimBesideAMovedArchive is the W30
// review's trim finding: paths.output_directory moved to a new disk, a
// Finished archive left where the old one put it, and a trim of it — whose
// encode writes "<trim>.partial.moombox-tmp-<run>-<n>.mp4" in the trim
// directory beside the archive — killed mid-encode. The sweep walked the
// current output tree and read the archive's own directory one level, never
// its trim directory, so the partial re-encode (gigabytes for a long range)
// stayed on disk for good.
//
// Mutant: muxTempRoots dropping trimDirsOf (the leftover survives).
func TestRemoveStaleMuxTempsSweepsACrashedTrimBesideAMovedArchive(t *testing.T) {
	_, db := testWorkerSetup(t)
	base := t.TempDir()
	oldChan := filepath.Join(base, "old-output", "Chan")
	trimDir := filepath.Join(oldChan, "trim")
	if err := os.MkdirAll(trimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(oldChan, "Title [vid].mp4")
	if err := os.WriteFile(archive, []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddJob(&database.Job{ID: "vid", VideoID: "vid", URL: "u", Status: database.StatusFinished,
		OutputFile: archive, Filename: filepath.Join("Chan", "Title [vid].mp4")}); err != nil {
		t.Fatal(err)
	}
	leftover := goneRunTemp(filepath.Join(trimDir, "vid_00-00-10_00-00-20.partial.mp4"))
	if err := os.WriteFile(leftover, []byte("half a trim"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := RemoveStaleMuxTemps(db, filepath.Join(base, "new-output"), discardLogger{}); n != 1 {
		t.Errorf("RemoveStaleMuxTemps removed %d files, want the crashed trim's temporary file", n)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Errorf("the crashed trim's temporary file %s survived the boot sweep", leftover)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Errorf("the archive the trim was cut from is gone: %v", err)
	}
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

// TestOutermostDirsWalksEachTreeOnce: a tree is dropped when another contains
// it, whichever comes first, and a repeat is dropped too.
//
// Mutant: outermostDirs not looking ahead for a tree that contains d (the
// nested tree listed before its parent is kept, and walked twice).
func TestOutermostDirsWalksEachTreeOnce(t *testing.T) {
	base := t.TempDir()
	a, ab, c := filepath.Join(base, "a"), filepath.Join(base, "a", "b"), filepath.Join(base, "c")
	if got := outermostDirs([]string{ab, a, ab, c, c}); !slices.Equal(got, []string{a, c}) {
		t.Errorf("outermostDirs = %v, want %v", got, []string{a, c})
	}
}

// TestRemoveStaleMuxTempsSweepsAJobsOwnOutputDirectory is the wiring between
// the two halves: a leftover under a job's own output_directory, outside the
// configured one, is found and removed, and the archive beside it is not.
//
// Mutants: muxTempRoots not walking each job's output_directory;
// RemoveStaleMuxTemps handing the engine sweep no roots.
func TestRemoveStaleMuxTempsSweepsAJobsOwnOutputDirectory(t *testing.T) {
	_, db := testWorkerSetup(t)
	override := filepath.Join(t.TempDir(), "elsewhere", "channel")
	if err := os.MkdirAll(override, 0o755); err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(override, "Title [x].mp4")
	if _, err := db.AddJob(&database.Job{ID: "x", VideoID: "x", URL: "u", Status: database.StatusMuxing,
		OutputDirectory: filepath.Dir(override)}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{final, goneRunTemp(final)} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if n := RemoveStaleMuxTemps(db, filepath.Join(t.TempDir(), "output"), discardLogger{}); n != 1 {
		t.Errorf("RemoveStaleMuxTemps removed %d files, want the one leftover", n)
	}
	if _, err := os.Stat(goneRunTemp(final)); !os.IsNotExist(err) {
		t.Error("the leftover under the job's own output directory survived")
	}
	if _, err := os.Stat(final); err != nil {
		t.Errorf("the archive beside the leftover is gone: %v", err)
	}
	if !engine.IsMuxTempPath(goneRunTemp(final)) {
		t.Fatal("premise: the fixture's leftover is not named as a temporary output")
	}
}
