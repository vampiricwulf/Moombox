package worker

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
)

// assertSweepSpares checks that neither the orphan scan nor a delete touches
// any of paths.
func assertSweepSpares(t *testing.T, db *database.Database, cfg *config.MoomboxConfig, paths ...string) {
	t.Helper()
	entries, err := ScanOrphanedFiles(db, cfg)
	if err != nil {
		t.Fatalf("ScanOrphanedFiles: %v", err)
	}
	for _, p := range paths {
		for _, e := range entries {
			if normalizePath(e.Path) == normalizePath(p) {
				t.Errorf("%s was offered as an orphan while it is in use: %+v", filepath.Base(p), e)
			}
		}
		if err := DeleteOrphanedFile(p, db, cfg); err == nil {
			t.Errorf("DeleteOrphanedFile removed %s while it is in use", filepath.Base(p))
		}
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s is gone: %v", filepath.Base(p), err)
		}
	}
}

// A trim encode writes into trim/ for as long as the range takes to encode,
// and no trim row names the file until it is done — so the Files sweep listed
// the half-written trim, and Delete All unlinked it under FFmpeg.
//
// Mutant: dropping CreateTrim's claimOutputStem, or scanTrimOrphans' claim
// check.
func TestOrphanSweepLeavesAnInProgressTrimAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in FFmpeg is a shell script")
	}
	_, db := testWorkerSetup(t)
	outputDir := t.TempDir()
	src := filepath.Join(outputDir, "src.mp4")
	if err := os.WriteFile(src, []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	length := 60
	job := &database.Job{ID: "j-trim", VideoID: "j-trim", URL: "u", Platform: "youtube",
		Status: database.StatusFinished, OutputFile: src, Filename: "src.mp4", LengthSeconds: &length}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	gates := t.TempDir()
	started, release := filepath.Join(gates, "started"), filepath.Join(gates, "release")
	ts := NewTrimService(db, writeBlockingFFmpeg(t, started, release), &discardLogger{})

	done := make(chan error, 1)
	go func() {
		_, err := ts.CreateTrim(context.Background(), job, 0, 10, nil)
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

	trims := mp4sIn(t, filepath.Join(outputDir, "trim"))
	if len(trims) != 1 {
		t.Fatalf("the stand-in encode should have written one trim, found %v", trims)
	}
	trim := filepath.Join(outputDir, "trim", trims[0])
	cfg := &config.MoomboxConfig{Paths: config.PathsConfig{OutputDirectory: outputDir, StagingDirectory: t.TempDir()}}
	assertSweepSpares(t, db, cfg, trim)

	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	released = true
	<-done
	if owner := outputClaimOwner(trim); owner != "" {
		t.Errorf("the trim's claim outlived its encode (owner %q)", owner)
	}
}

// RecoverAsides keeps the row's status, so for a Cancelled job the sweep saw
// an orphaned staging dir and an orphaned sibling archive while FFmpeg read
// the one and wrote the other — and a delete of either during the copy lost
// the footage once the recovery removed the aside.
//
// Mutants: dropping recoverAsides' claimOutputStem (the sibling is offered),
// RecoverAsides' staging claim, or scanStagingOrphans' claim check (the
// staging dir is offered).
func TestOrphanSweepLeavesAnInProgressAsideRecoveryAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in FFmpeg is a shell script")
	}
	w, db := testWorkerSetup(t)
	staging, outputDir := muxFixtureJob(t, w, db, "j-recover-claim")
	db.UpdateJobFields("j-recover-claim", map[string]any{"status": database.StatusCancelled})
	aside := filepath.Join(staging, "video.mp4"+engine.StagedRestartSuffix+"1700000000")
	if err := os.WriteFile(aside, []byte("set aside"), 0o644); err != nil {
		t.Fatal(err)
	}
	gates := t.TempDir()
	started, release := filepath.Join(gates, "started"), filepath.Join(gates, "release")
	w.orchestrator.SetFfmpegPath(writeBlockingFFmpeg(t, started, release))

	done := make(chan error, 1)
	go func() {
		err := w.RecoverAsides("j-recover-claim")
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
		t.Fatalf("the stand-in recovery should have written one sibling, found %v", outs)
	}
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
	cfg := &config.MoomboxConfig{Paths: config.PathsConfig{OutputDirectory: outputDir, StagingDirectory: stagingBase}}
	assertSweepSpares(t, db, cfg, filepath.Join(outputDir, outs[0]), staging)

	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	released = true
	<-done
	if owner := outputClaimOwner(staging); owner != "" {
		t.Errorf("the staging claim outlived the recovery (owner %q)", owner)
	}
}
