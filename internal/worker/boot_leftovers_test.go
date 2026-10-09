package worker

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
)

// lockedLogger records every line, safe for the goroutines Start spawns.
type lockedLogger struct {
	mu    sync.Mutex
	lines [][]any
}

func (l *lockedLogger) log(msg string, args []any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, append([]any{msg}, args...))
}
func (l *lockedLogger) Debug(msg string, args ...any) { l.log(msg, args) }
func (l *lockedLogger) Info(msg string, args ...any)  { l.log(msg, args) }
func (l *lockedLogger) Warn(msg string, args ...any)  { l.log(msg, args) }
func (l *lockedLogger) Error(msg string, args ...any) { l.log(msg, args) }

// deletionLogged reports whether an Info line names path under "path" and
// carries a non-empty "reason".
func (l *lockedLogger) deletionLogged(path string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		var gotPath, reason string
		for i := 1; i+1 < len(line); i += 2 {
			switch line[i] {
			case "path":
				gotPath, _ = line[i+1].(string)
			case "reason":
				reason, _ = line[i+1].(string)
			}
		}
		if gotPath == path && reason != "" {
			return true
		}
	}
	return false
}

// bootSweepWorker is testWorkerSetup with a logger the test can read.
func bootSweepWorker(t *testing.T) (*DownloadWorker, *database.Database, *lockedLogger, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := &config.MoomboxConfig{}
	cfg.Paths.StagingDirectory = filepath.Join(dir, "staging")
	log := &lockedLogger{}
	return NewDownloadWorker(db, nil, cfg, log, nil), db, log, cfg.Paths.StagingDirectory
}

func writeFixtureFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestBootSweepRemovesOnlyStagingTheCleanupWouldHave pins the boot sweep's
// staging rule: a Finished job's staging goes only when its archive is on
// disk and cleanupStagingAfterMux's own decision (decideStagingCleanup) says
// remove. Every shield that function has keeps the dir — and keeps ALL of it:
// a chat-incomplete dir is not even pruned, that is the orphan sweep's to
// offer — as do a missing archive, a non-Finished row, an active row and a dir
// with no row at all. A removal is logged with its path and a reason.
//
// Mutants: drop the missingArchiveFile check (the two missing-archive cases
// are deleted); drop the Finished check (the Error row's staging is deleted);
// act on every verdict but the chat prune (the tail, aside, unmuxed-part and
// unused-root cases are deleted); drop decideStagingCleanup's tail, chat,
// unmuxed-part or unused-root arm (that case is deleted — its aside arm is
// backed by hasUnmuxedPartsForJob's own aside term, so dropping it alone
// changes nothing); drop the reason from the Info line (the redundant cases
// fail their log assertion).
func TestBootSweepRemovesOnlyStagingTheCleanupWouldHave(t *testing.T) {
	type fixture struct {
		status       database.JobStatus
		noRow        bool
		tail         bool
		chat         string
		outputExists bool
		part         string // "" none, "present" or "missing": a recorded part 0's file
		root         []string
		seg1         bool // an unrecorded part 1 with media
	}
	for _, tc := range []struct {
		name     string
		f        fixture
		wantGone bool
	}{
		{"redundant: finished, archive on disk, nothing shielded",
			fixture{status: database.StatusFinished, outputExists: true, root: []string{"video.mp4", "audio.m4a"}}, true},
		{"redundant: finished as parts, part 0's own capture left in the root",
			fixture{status: database.StatusFinished, outputExists: true, part: "present", root: []string{"video_stream"}}, true},
		{"incomplete tail", fixture{status: database.StatusFinished, outputExists: true, tail: true, root: []string{"video.mp4"}}, false},
		{"chat incomplete", fixture{status: database.StatusFinished, outputExists: true, chat: chatStatusIncomplete, root: []string{"video.mp4"}}, false},
		{"a set-aside recording", fixture{status: database.StatusFinished, outputExists: true, root: []string{"video.mp4", "video.mp4.restart-1700000000"}}, false},
		{"an unmuxed part", fixture{status: database.StatusFinished, outputExists: true, root: []string{"video_stream"}, seg1: true}, false},
		{"an unused root recording", fixture{status: database.StatusFinished, outputExists: true, part: "present", root: []string{"video.mp4"}}, false},
		{"the archive is missing", fixture{status: database.StatusFinished, root: []string{"video.mp4"}}, false},
		{"a part file is missing", fixture{status: database.StatusFinished, outputExists: true, part: "missing", root: []string{"video_stream"}}, false},
		{"not finished", fixture{status: database.StatusError, outputExists: true, root: []string{"video.mp4"}}, false},
		{"active", fixture{status: database.StatusDownloading, outputExists: true, root: []string{"video.mp4"}}, false},
		{"no row", fixture{noRow: true, root: []string{"video.mp4"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db, log, stagingBase := bootSweepWorker(t)
			outputDir := t.TempDir()
			archive := filepath.Join(outputDir, "x.mp4")
			if tc.f.outputExists {
				writeFixtureFile(t, archive, "archive")
			}
			staging := filepath.Join(stagingBase, "j-boot")
			for _, n := range tc.f.root {
				writeFixtureFile(t, filepath.Join(staging, n), "\x00\x00\x00\x18ftypdash")
			}
			if tc.f.seg1 {
				writeFixtureFile(t, filepath.Join(staging, "seg_1", "video_stream"), "\x00\x00\x00\x18ftypdash")
			}
			if !tc.f.noRow {
				if _, err := db.AddJob(&database.Job{ID: "j-boot", VideoID: "j-boot", Status: tc.f.status,
					ChatStatus: tc.f.chat, OutputFile: archive}); err != nil {
					t.Fatal(err)
				}
				// incomplete_tail is not an INSERT column (AddJob's doc).
				if tc.f.tail {
					db.UpdateJobFields("j-boot", map[string]any{"incomplete_tail": true})
				}
			}
			if tc.f.part != "" {
				partFile := filepath.Join(outputDir, "x - part1.mp4")
				if tc.f.part == "present" {
					writeFixtureFile(t, partFile, "part")
				}
				if err := db.AddSegment(&database.Segment{JobID: "j-boot", SegmentIndex: 0, Filename: filepath.Base(partFile), FilePath: partFile}); err != nil {
					t.Fatal(err)
				}
			}

			w.reclaimBootLeftovers()

			_, err := os.Stat(staging)
			if gone := os.IsNotExist(err); gone != tc.wantGone {
				t.Fatalf("staging removed = %v, want %v", gone, tc.wantGone)
			}
			if tc.wantGone {
				if !log.deletionLogged(staging) {
					t.Errorf("the removal of %s was not logged with its path and a reason: %v", staging, log.lines)
				}
				return
			}
			for _, n := range tc.f.root {
				if !fileExists(filepath.Join(staging, n)) {
					t.Errorf("%s was deleted from a staging dir the sweep had to leave alone", n)
				}
			}
		})
	}
}

// TestBootSweepRemovesRecoveredAsidesWhoseSiblingExists pins the second
// leftover: a set-aside recording carrying a recovered marker goes — with its
// resume twin and the marker — only when the sibling the marker names is on
// disk, in the root and in a seg_N dir alike, and whatever the job's row (a
// Cancelled job keeps the rest of its staging; a dir whose row is gone is still
// cleared of the redundant aside). An unmarked aside, a marker naming a missing
// sibling, and an active job's staging are left alone. Each removal is logged
// with its path and a reason.
//
// Mutants: drop the sibling check (the missing-sibling aside is deleted); scan
// the root only (the seg_1 aside survives); move the call below the row
// check (the row-less dir keeps its aside); drop both active-row checks (the
// Downloading job's aside is deleted); drop the reason from the Info line.
func TestBootSweepRemovesRecoveredAsidesWhoseSiblingExists(t *testing.T) {
	w, db, log, stagingBase := bootSweepWorker(t)
	outputDir := t.TempDir()
	sibling := filepath.Join(outputDir, "x.restart-1700000000.mp4")
	writeFixtureFile(t, sibling, "recovered")

	// marked writes an aside, its resume twin and a marker naming target.
	marked := func(dir, name, target string) string {
		aside := filepath.Join(dir, name)
		writeFixtureFile(t, aside, "set aside")
		writeFixtureFile(t, engine.StagedRestartSidecar(aside), `{"lastSeq":1}`)
		writeFixtureFile(t, aside+asideRecoveredMarker, target)
		return aside
	}

	cancelled := filepath.Join(stagingBase, "j-cancelled")
	if _, err := db.AddJob(&database.Job{ID: "j-cancelled", VideoID: "j-cancelled", Status: database.StatusCancelled}); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(cancelled, "video.mp4"), "the main recording")
	rootAside := marked(cancelled, "video_stream.restart-1700000000", sibling)
	segAside := marked(filepath.Join(cancelled, "seg_1"), "audio_stream.restart-1700000001", sibling)
	noSibling := marked(cancelled, "video_stream.restart-1700000002", filepath.Join(outputDir, "gone.mp4"))
	unmarked := filepath.Join(cancelled, "video_stream.restart-1700000003")
	writeFixtureFile(t, unmarked, "never recovered")

	rowless := filepath.Join(stagingBase, "j-gone")
	rowlessAside := marked(rowless, "video_stream.restart-1700000004", sibling)

	active := filepath.Join(stagingBase, "j-active")
	if _, err := db.AddJob(&database.Job{ID: "j-active", VideoID: "j-active", Status: database.StatusDownloading}); err != nil {
		t.Fatal(err)
	}
	activeAside := marked(active, "video_stream.restart-1700000005", sibling)

	w.reclaimBootLeftovers()

	for _, aside := range []string{rootAside, segAside, rowlessAside} {
		for _, p := range []string{aside, engine.StagedRestartSidecar(aside), aside + asideRecoveredMarker} {
			if fileExists(p) {
				t.Errorf("%s survived: its aside was recovered to a sibling that is on disk", p)
			}
		}
		if !log.deletionLogged(aside) {
			t.Errorf("the removal of %s was not logged with its path and a reason", aside)
		}
	}
	for _, p := range []string{noSibling, noSibling + asideRecoveredMarker, unmarked, activeAside, activeAside + asideRecoveredMarker,
		filepath.Join(cancelled, "video.mp4")} {
		if !fileExists(p) {
			t.Errorf("%s was deleted; nothing proves it redundant", p)
		}
	}
	if !fileExists(sibling) {
		t.Error("the recovered sibling itself was deleted")
	}
	if strings.TrimSpace(readFileOr(t, noSibling+asideRecoveredMarker)) == "" {
		t.Error("the kept marker lost its content")
	}
}

// TestBootSweepNeverClaimsAnActiveJob: the sweep runs beside
// enqueueExistingJobs, whose restart mux of a Muxing row takes the same
// per-job claim — and a refused restart mux falls back to resetting the row to
// Downloading, the re-download that truncated a complete recording. So an
// active row is skipped before the claim is taken, never under it; a row that
// is not active is claimed.
//
// Mutant: take the claim before the active check (the Muxing row is claimed).
func TestBootSweepNeverClaimsAnActiveJob(t *testing.T) {
	w, db, _, stagingBase := bootSweepWorker(t)
	for id, status := range map[string]database.JobStatus{
		"j-muxing":   database.StatusMuxing,
		"j-finished": database.StatusFinished,
	} {
		if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, Status: status}); err != nil {
			t.Fatal(err)
		}
		writeFixtureFile(t, filepath.Join(stagingBase, id, "video_stream"), "\x00\x00\x00\x18ftypdash")
	}
	var claimed []string
	bootSweepClaimed = func(id string) { claimed = append(claimed, id) }
	t.Cleanup(func() { bootSweepClaimed = nil })

	w.reclaimBootLeftovers()

	if slices.Contains(claimed, "j-muxing") {
		t.Error("the sweep took the staging claim of a Muxing row — a restart mux asking for it then falls back to a re-download")
	}
	if !slices.Contains(claimed, "j-finished") {
		t.Errorf("claimed = %v; the Finished row was never considered", claimed)
	}
}

// TestBootSweepRereadsTheRowUnderTheClaim: a row revived between the sweep's
// first look and its claim (a Reinitialize, a Resume) is active by the time
// anything is deleted, and its staging — now the new run's — is left alone.
//
// Mutant: drop the active check from the read under the claim (the revived
// job's staging, and the recovered aside in it, are deleted).
func TestBootSweepRereadsTheRowUnderTheClaim(t *testing.T) {
	w, db, _, stagingBase := bootSweepWorker(t)
	archive := filepath.Join(t.TempDir(), "x.mp4")
	writeFixtureFile(t, archive, "archive")
	sibling := filepath.Join(t.TempDir(), "x.restart-1700000000.mp4")
	writeFixtureFile(t, sibling, "recovered")
	staging := filepath.Join(stagingBase, "j-revived")
	writeFixtureFile(t, filepath.Join(staging, "video.mp4"), "\x00\x00\x00\x18ftypdash")
	aside := filepath.Join(staging, "video_stream.restart-1700000000")
	writeFixtureFile(t, aside, "set aside")
	writeFixtureFile(t, aside+asideRecoveredMarker, sibling)
	if _, err := db.AddJob(&database.Job{ID: "j-revived", VideoID: "j-revived", Status: database.StatusFinished, OutputFile: archive}); err != nil {
		t.Fatal(err)
	}
	bootSweepClaimed = func(id string) {
		db.UpdateJobFields(id, map[string]any{"status": database.StatusDownloading})
	}
	t.Cleanup(func() { bootSweepClaimed = nil })

	w.reclaimBootLeftovers()

	if !fileExists(filepath.Join(staging, "video.mp4")) || !fileExists(aside) {
		t.Error("the sweep deleted from a staging dir whose row was revived before it acted")
	}
}

func readFileOr(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// TestStartRunsTheBootSweep pins the wiring: Start reclaims a redundant
// finished job's staging without anything else asking it to.
//
// Mutant: drop the reclaimBootLeftovers goroutine from Start — the staging
// survives.
func TestStartRunsTheBootSweep(t *testing.T) {
	w, db, _, stagingBase := bootSweepWorker(t)
	archive := filepath.Join(t.TempDir(), "x.mp4")
	writeFixtureFile(t, archive, "archive")
	staging := filepath.Join(stagingBase, "j-start")
	writeFixtureFile(t, filepath.Join(staging, "video.mp4"), "\x00\x00\x00\x18ftypdash")
	if _, err := db.AddJob(&database.Job{ID: "j-start", VideoID: "j-start", Status: database.StatusFinished, OutputFile: archive}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Start(ctx)
	w.wg.Wait()

	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("staging survived Start (%v); the boot sweep did not run", err)
	}
}
