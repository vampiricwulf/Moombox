package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestReinitializeKeepsTheExistingArchive is owner ruling R2-W1 end to end. A
// job whose row still names a single-file archive (a Finished job with an
// incomplete tail, resumed, the resume failed) is reinitialized and recorded
// again, shorter:
//
//   - the preview names the archive, with its size;
//   - after the reset the row still owns the file (prior_output_file), so the
//     Files sweep does not offer it;
//   - the new recording's finalize resolves to the same name and does not
//     replace it: the 5 s archive is beside the new 2 s one under a set-aside
//     name, the row names it there, and the sweep offers neither.
//
// Mutants: resetFieldsKeepingArchive not adding the column (the sweep lists
// the archive after the reset, and the finalize writes over it: no 5 s file
// is left); setAsidePriorArchive not called by the finalize (the same loss);
// the column not updated after the rename (the row names a missing file);
// prior_output_file left out of jobFileLocations (the sweep lists it).
func TestReinitializeKeepsTheExistingArchive(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)
	o := w.orchestrator
	stagingDir, outputDir := muxFixtureJob(t, w, db, "j-keep")
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
	cfg := &config.MoomboxConfig{}
	cfg.Paths.OutputDirectory = outputDir
	offered := func() []string {
		t.Helper()
		entries, err := scanOutputOrphans(db, cfg)
		if err != nil {
			t.Fatalf("scanOutputOrphans: %v", err)
		}
		var names []string
		for _, e := range entries {
			if strings.HasSuffix(e.Path, ".mp4") {
				names = append(names, e.RelPath)
			}
		}
		return names
	}
	finalize := func(seconds int) *database.Job {
		t.Helper()
		if err := os.MkdirAll(stagingDir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeMuxFixture(t, ffmpegPath, filepath.Join(stagingDir, "video.mp4"), seconds)
		job, _ := db.GetJob("j-keep")
		if err := o.muxFromStaging(context.Background(), w.buildJobContext(job)); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		job, _ = db.GetJob("j-keep")
		return job
	}

	first := finalize(5)
	archive := first.OutputFile
	if archive == "" || !fileExists(archive) {
		t.Fatalf("precondition: the first finalize left no archive (%q)", archive)
	}
	// The failed resume: an Error row that still names the archive.
	db.UpdateJobFields("j-keep", map[string]any{"status": database.StatusError})
	job, _ := db.GetJob("j-keep")
	info, _ := os.Stat(archive)
	if p := PreviewReinitialize(db, stagingBase, job); p.ExistingArchives != 1 || p.ExistingArchiveBytes != info.Size() {
		t.Errorf("preview = %+v, want the existing archive and its %d bytes", p, info.Size())
	}

	w.reinitializeNow("j-keep", database.StatusError)
	job, _ = db.GetJob("j-keep")
	if job.OutputFile != "" || job.PriorOutputFile != archive {
		t.Fatalf("after the reset output_file = %q, prior_output_file = %q; want the archive moved to the second", job.OutputFile, job.PriorOutputFile)
	}
	if got := offered(); len(got) != 0 {
		t.Errorf("the Files sweep offers %v after the reset; the row still owns the archive", got)
	}

	second := finalize(2)
	if second.OutputFile != archive {
		t.Fatalf("the new recording resolved to %q, not the old archive's name %q: the test no longer covers the collision", second.OutputFile, archive)
	}
	if p := o.runFFprobe(context.Background(), archive); p == nil || p.DurationSec > 3 {
		t.Errorf("%s probes %+v, want the new 2 s recording", archive, p)
	}
	aside := second.PriorOutputFile
	if !priorArchiveSetAside(aside) || filepath.Dir(aside) != filepath.Dir(archive) {
		t.Fatalf("prior_output_file = %q, want a set-aside name beside %s", aside, archive)
	}
	if p := o.runFFprobe(context.Background(), aside); p == nil || p.DurationSec < 4 {
		t.Errorf("the set-aside archive %s probes %+v, want the earlier 5 s recording", aside, p)
	}
	if got := offered(); len(got) != 0 {
		t.Errorf("the Files sweep offers %v after the new recording finished", got)
	}

	// A second finalize of the same recording (a Mux after a failed one)
	// leaves the set-aside archive where it is.
	db.UpdateJobFields("j-keep", map[string]any{"status": database.StatusDownloading})
	if third := finalize(2); third.PriorOutputFile != aside || !fileExists(aside) {
		t.Errorf("a second finalize moved the set-aside archive: prior_output_file = %q, want %q", third.PriorOutputFile, aside)
	}
}

// TestArchiveKeptByReset pins which archive a reset keeps: the single file
// output_file names, never a split job's part (the reset deletes those, and
// the preview counts them as parts), never a file that is gone; and for a row
// reset once already, the archive that reset recorded until a finalize has
// set it aside.
//
// Mutants: the parts check dropped (a part is reported as kept while
// clearJobParts deletes it); the set-aside check dropped (a second reset
// re-arms an archive that is already aside).
func TestArchiveKeptByReset(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "show.mp4")
	aside := filepath.Join(dir, "show.restart-1700000000.mp4")
	for _, p := range []string{archive, aside} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gone := filepath.Join(dir, "gone.mp4")
	for _, tc := range []struct {
		name     string
		job      *database.Job
		hasParts bool
		want     string
	}{
		{"single file", &database.Job{OutputFile: archive}, false, archive},
		{"a part", &database.Job{OutputFile: archive}, true, ""},
		{"file gone", &database.Job{OutputFile: gone}, false, ""},
		{"reset once, not yet set aside", &database.Job{PriorOutputFile: archive}, false, archive},
		{"already set aside", &database.Job{PriorOutputFile: aside}, false, ""},
		{"no archive", &database.Job{}, false, ""},
	} {
		if got := archiveKeptByReset(tc.job, tc.hasParts); got != tc.want {
			t.Errorf("%s: archiveKeptByReset = %q, want %q", tc.name, got, tc.want)
		}
	}
}
