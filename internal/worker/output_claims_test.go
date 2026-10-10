package worker

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// writeBlockingFFmpeg writes a stand-in FFmpeg that creates its output (the
// last argument), touches started, and then holds the "mux" open until
// release exists — the window a real multi-gigabyte copy-mux spends writing.
func writeBlockingFFmpeg(t *testing.T, started, release string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffmpeg")
	script := "#!/bin/sh\n" +
		"for a; do out=\"$a\"; done\n" +
		"printf partial > \"$out\"\n" +
		": > '" + started + "'\n" +
		"while [ ! -e '" + release + "' ]; do sleep 0.01; done\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A finalize names its output on the row only in the write that marks the job
// Finished, so for the whole length of a mux the file FFmpeg was writing was,
// to the orphan sweep, an unowned "output" entry — and DeleteOrphanedFile's
// active-job recheck found no column naming it either. A Files → Delete All
// during a long mux unlinked the archive mid-write; on Linux FFmpeg wrote on
// into the unlinked inode, the job finished clean, and the staging cleanup
// removed the only other copy.
//
// Mutant: dropping muxAndFinalize's claimOutputStem, or the sweep's or the
// recheck's outputClaimOwner check.
func TestOrphanSweepLeavesAnInProgressMuxAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in FFmpeg is a shell script")
	}
	w, db := testWorkerSetup(t)
	staging, outputDir := muxFixtureJob(t, w, db, "j-inflight")
	if err := os.WriteFile(filepath.Join(staging, "video.mp4"), []byte("staged"), 0o644); err != nil {
		t.Fatal(err)
	}
	gates := t.TempDir()
	started, release := filepath.Join(gates, "started"), filepath.Join(gates, "release")
	w.orchestrator.SetFfmpegPath(writeBlockingFFmpeg(t, started, release))

	done := make(chan error, 1)
	go func() {
		err := w.MuxJob("j-inflight")
		w.Stop()
		done <- err
	}()
	released := false
	defer func() {
		if !released {
			os.WriteFile(release, nil, 0o644)
			<-done
		}
	}()
	waitForFile(t, started)

	outs := mp4sIn(t, outputDir)
	if len(outs) != 1 {
		t.Fatalf("the stand-in mux should have written one output, found %v", outs)
	}
	out := filepath.Join(outputDir, outs[0])

	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
	cfg := &config.MoomboxConfig{Paths: config.PathsConfig{OutputDirectory: outputDir, StagingDirectory: stagingBase}}

	entries, err := ScanOrphanedFiles(db, cfg)
	if err != nil {
		t.Fatalf("ScanOrphanedFiles: %v", err)
	}
	for _, e := range entries {
		if normalizePath(e.Path) == normalizePath(out) {
			t.Errorf("the output of a mux still in progress was offered as an orphan: %+v", e)
		}
	}
	if err := DeleteOrphanedFile(out, db, cfg); err == nil {
		t.Error("DeleteOrphanedFile removed the output of a mux still in progress")
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("the in-progress output is gone: %v", err)
	}

	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	released = true
	if err := <-done; err != nil {
		t.Fatalf("MuxJob: %v", err)
	}
	fresh, _ := db.GetJob("j-inflight")
	if fresh == nil || fresh.Status != database.StatusFinished || normalizePath(fresh.OutputFile) != normalizePath(out) {
		t.Fatalf("the mux did not finish onto its output: %+v", fresh)
	}
	// The claim is the finalize's, not the file's: once the row names the
	// output the claim is gone, and ownership is the DB's again.
	if owner := outputClaimOwner(out); owner != "" {
		t.Errorf("the claim outlived the finalize (owner %q)", owner)
	}
}

// The part path has the same window: muxSegment records the part's segment
// row only after FFmpeg exits, so while a quality-split part muxes (the job
// still Downloading, the stream still running) the part file was an unowned
// orphan too.
//
// Mutant: dropping muxSegment's claimOutputStem.
func TestOrphanSweepLeavesAnInProgressPartMuxAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in FFmpeg is a shell script")
	}
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	staging, outputDir := muxFixtureJob(t, w, db, "j-inflight-part")
	media := filepath.Join(staging, "video.mp4")
	if err := os.WriteFile(media, []byte("staged"), 0o644); err != nil {
		t.Fatal(err)
	}
	gates := t.TempDir()
	started, release := filepath.Join(gates, "started"), filepath.Join(gates, "release")
	w.orchestrator.SetFfmpegPath(writeBlockingFFmpeg(t, started, release))

	job, _ := db.GetJob("j-inflight-part")
	jobCtx := w.buildJobContext(job)
	done := make(chan error, 1)
	go func() {
		_, err := w.orchestrator.muxSegment(context.Background(), jobCtx, 0, 0, time.Now().Unix(),
			QualityInfo{Label: "720p"}, &DownloadResult{HasVideo: true, VideoPath: media})
		done <- err
	}()
	released := false
	defer func() {
		if !released {
			os.WriteFile(release, nil, 0o644)
			<-done
		}
	}()
	waitForFile(t, started)

	outs := mp4sIn(t, outputDir)
	if len(outs) != 1 {
		t.Fatalf("the stand-in part mux should have written one output, found %v", outs)
	}
	part := filepath.Join(outputDir, outs[0])
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
	cfg := &config.MoomboxConfig{Paths: config.PathsConfig{OutputDirectory: outputDir, StagingDirectory: stagingBase}}

	entries, err := ScanOrphanedFiles(db, cfg)
	if err != nil {
		t.Fatalf("ScanOrphanedFiles: %v", err)
	}
	for _, e := range entries {
		if normalizePath(e.Path) == normalizePath(part) {
			t.Errorf("a part still muxing was offered as an orphan: %+v", e)
		}
	}
	if err := DeleteOrphanedFile(part, db, cfg); err == nil {
		t.Error("DeleteOrphanedFile removed a part still muxing")
	}

	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	released = true
	if err := <-done; err != nil {
		t.Fatalf("muxSegment: %v", err)
	}
	if owner := outputClaimOwner(part); owner != "" {
		t.Errorf("the part's claim outlived its mux (owner %q)", owner)
	}
}

// Claims nest and release once: a stem claimed twice stays claimed until both
// holders let go, and a second call of one release cannot take the other's
// claim with it.
//
// Mutant: deleting the stem on the first release regardless of the count, or
// dropping the sync.Once.
func TestOutputClaimsNestAndReleaseOnce(t *testing.T) {
	stem := filepath.Join(t.TempDir(), "Show [abc]")
	file := stem + ".chat.json"
	r1 := claimOutputStem("a", stem)
	r2 := claimOutputStem("a", stem)
	r1()
	r1()
	if outputClaimOwner(file) != "a" {
		t.Fatal("releasing one of two claims (twice) dropped the other")
	}
	r2()
	if owner := outputClaimOwner(file); owner != "" {
		t.Errorf("both claims released, still owned by %q", owner)
	}
	if outputClaimOwner(filepath.Join(filepath.Dir(stem), "Other.mp4")) != "" {
		t.Error("a path outside the stem was claimed")
	}
}

// DeleteOrphanedFile's containment check compares canonical spellings, but
// the active-job recheck used to look the path up against the CONFIGURED
// staging spelling only: through a symlinked (Docker volume, NAS link) or
// junctioned staging_directory, a request naming the real directory passed
// containment, found no job, and RemoveAll'd an active job's staging.
//
// Mutant: dropping findActiveJobForPath's canonical pass.
func TestDeleteOrphanedFileRefusesAnActiveJobByItsCanonicalSpelling(t *testing.T) {
	dir := t.TempDir()
	realStaging := filepath.Join(dir, "real-staging")
	if err := os.MkdirAll(realStaging, 0o755); err != nil {
		t.Fatal(err)
	}
	linkStaging := filepath.Join(dir, "staging")
	if err := os.Symlink(realStaging, linkStaging); err != nil {
		t.Skipf("symlink: %v", err)
	}
	cfg := &config.MoomboxConfig{Paths: config.PathsConfig{StagingDirectory: linkStaging, OutputDirectory: filepath.Join(dir, "output")}}

	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const jobID = "activeSymlinkJob"
	if _, err := db.AddJob(&database.Job{ID: jobID, VideoID: jobID, URL: "u", Status: database.StatusDownloading}); err != nil {
		t.Fatal(err)
	}
	jobStaging := filepath.Join(realStaging, jobID)
	if err := os.MkdirAll(jobStaging, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobStaging, "video.ts"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := DeleteOrphanedFile(filepath.Join(linkStaging, jobID), db, cfg); err == nil {
		t.Fatal("the configured spelling of an active job's staging was deleted")
	}
	if err := DeleteOrphanedFile(jobStaging, db, cfg); err == nil {
		t.Error("the canonical spelling of an active job's staging was deleted")
	}
	if _, err := os.Stat(jobStaging); err != nil {
		t.Errorf("the active job's staging is gone: %v", err)
	}
}

// A claim made through a symlinked (or junctioned) output directory also
// covers the file named through the real directory behind it — the spelling
// DeleteOrphanedFile's containment check accepts.
//
// Mutant: outputClaimKeys returning the configured spelling alone.
func TestOutputClaimCoversTheCanonicalSpelling(t *testing.T) {
	dir := t.TempDir()
	realOut := filepath.Join(dir, "real-output")
	if err := os.MkdirAll(realOut, 0o755); err != nil {
		t.Fatal(err)
	}
	linkOut := filepath.Join(dir, "output")
	if err := os.Symlink(realOut, linkOut); err != nil {
		t.Skipf("symlink: %v", err)
	}
	release := claimOutputStem("j", filepath.Join(linkOut, "Show [abc]"))
	defer release()
	if owner := outputClaimOwner(filepath.Join(realOut, "Show [abc].mp4")); owner != "j" {
		t.Errorf("the real directory's spelling of a claimed output is unowned (owner %q)", owner)
	}
}
