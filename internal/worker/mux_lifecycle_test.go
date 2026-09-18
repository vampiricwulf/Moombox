package worker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
)

// TestCancelMuxesCutsTheMuxRoot pins owner decision O-E: every background and
// final mux descends from one cancellable root, so the worker's shutdown can
// kill FFmpeg instead of leaving it writing into a staging dir the respawned
// child is about to re-mux with -y.
//
// Mutant: restoring context.Background() in launchBackgroundSegmentMux —
// muxRoot() is never consulted, CancelMuxes cancels nothing, and the orphan
// keeps writing after the child exits.
func TestCancelMuxesCutsTheMuxRoot(t *testing.T) {
	o := NewDownloadOrchestrator(nil, nil, "ffmpeg", discardLogger{}, nil, nil, nil, nil, nil)

	root := o.muxRoot()
	if root.Err() != nil {
		t.Fatalf("muxRoot().Err() = %v before cancellation, want nil", root.Err())
	}
	o.CancelMuxes()
	select {
	case <-root.Done():
	default:
		t.Fatal("CancelMuxes did not cancel the mux root context")
	}
}

// TestMuxRootOnZeroValueOrchestrator pins the nil guard: tests and the
// standalone Mux action construct orchestrators without the constructor, and
// a nil root must degrade to Background rather than panic.
//
// Mutant: returning o.muxRootCtx unguarded — a struct-literal orchestrator
// hands a nil context to context.WithTimeout and panics.
func TestMuxRootOnZeroValueOrchestrator(t *testing.T) {
	o := &DownloadOrchestrator{}
	if got := o.muxRoot(); got == nil {
		t.Fatal("muxRoot() = nil on a zero-value orchestrator, want context.Background()")
	}
	o.CancelMuxes() // must not panic
}

// TestBackgroundSegmentMuxDescendsFromTheMuxRoot pins O-E at its main site:
// the part mux of a quality/gap split, which ran on context.Background() and
// so was unreachable from Stop. The goroutine's preMux hook sees the very
// context the FFmpeg run gets, so cancelling the root must close it.
//
// Mutant: restoring context.Background() in launchBackgroundSegmentMux — the
// captured context never closes, the part mux outlives the child, and on
// Windows it keeps writing inside the LAUNCHER's job object while the
// respawned child re-muxes the same part with -y.
func TestBackgroundSegmentMuxDescendsFromTheMuxRoot(t *testing.T) {
	w, db := testWorkerSetup(t)
	_, outputDir := muxFixtureJob(t, w, db, "j-bg")

	jobCtx := w.buildJobContext(&database.Job{ID: "j-bg", VideoID: "j-bg", OutputDirectory: outputDir})
	captured := make(chan context.Context, 1)
	// The hook holds the goroutine open across the assertion: let it return and
	// its own deferred muxCancel closes the context, which would pass under the
	// mutant too.
	release := make(chan struct{})
	var wg sync.WaitGroup
	w.orchestrator.launchBackgroundSegmentMux(jobCtx, &wg, 0, 0, 0, QualityInfo{Label: "720p"},
		&DownloadResult{}, "youtube", func(ctx context.Context) { captured <- ctx; <-release })

	muxCtx := <-captured
	if muxCtx.Err() != nil {
		t.Fatalf("part mux context = %v before cancellation, want live", muxCtx.Err())
	}
	w.orchestrator.CancelMuxes()
	select {
	case <-muxCtx.Done():
		close(release)
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("cancelling the mux root did not reach the background part mux")
	}
	wg.Wait()
}

// TestMuxedOutputIsShort pins ENGINE-9: `-c copy` stops at the first
// undemuxable fragment and exits 0, so a truncated .mp4 used to finish as a
// clean job. The check only fires when the INPUT probe produced a plausible
// duration, so a fragmented raw stream whose container metadata reports 0
// (or a couple of seconds) can never raise a false alarm.
//
// Mutants, one per row:
//   - dropping the tolerance: normal container jitter flags every archive.
//   - dropping the floor: a 30-second clip's rounding flags it.
//   - comparing the wrong way round: a LONGER output is flagged.
func TestMuxedOutputIsShort(t *testing.T) {
	for _, tc := range []struct {
		name          string
		input, output float64
		want          bool
	}{
		{"truncated at the first bad fragment", 7200, 300, true},
		{"container jitter within tolerance", 7200, 7195, false},
		{"input too short to judge", 30, 5, false},
		{"input duration unknown", 0, 300, false},
		{"output longer than input", 3600, 3605, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := muxedOutputIsShort(tc.input, tc.output); got != tc.want {
				t.Errorf("muxedOutputIsShort(in=%v, out=%v) = %v, want %v", tc.input, tc.output, got, tc.want)
			}
		})
	}
}

// TestVerifyMuxedDurationNamesTheNumbers pins the reporting half of ENGINE-9
// against a REAL ffprobe: the shortfall verdict must carry both durations, so
// the Error row says what went wrong instead of "mux failed".
//
// Mutant: returning a bare error (or nil) when the output is short — the
// operator gets a row with no numbers and no way to tell a truncated copy
// from a missing binary.
func TestVerifyMuxedDurationNamesTheNumbers(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	input := filepath.Join(t.TempDir(), "video.mp4")
	writeMuxFixture(t, ffmpegPath, input, 90)

	o := NewDownloadOrchestrator(nil, nil, ffmpegPath, discardLogger{}, nil, nil, nil, nil, nil)

	err := o.verifyMuxedDuration(context.Background(), "j1", 5, input, "")
	if err == nil {
		t.Fatal("verifyMuxedDuration(output=5s, input=90s) = nil, want a shortfall error")
	}
	for _, want := range []string{"90", "5"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("shortfall error %q does not name %q", err, want)
		}
	}

	if err := o.verifyMuxedDuration(context.Background(), "j1", 89, input, ""); err != nil {
		t.Errorf("verifyMuxedDuration(output=89s, input=90s) = %v, want nil (within tolerance)", err)
	}
}

// TestRestartMuxFinishesAndClearsStaging pins Task 2's review finding 1: the
// off-queue restart/Mux path muxed the staged recording into the archive and
// then left the raw recording in staging forever, silently doubling the disk
// cost of every archive recovered that way.
//
// It doubles as ENGINE-9's false-positive control end to end: a healthy 90 s
// recording muxes to a 90 s output and must finish clean.
//
// Mutant: skipping cleanupStagingAfterMux in MuxJob — the staging dir (and
// its raw recording) survives a Finished row.
func TestRestartMuxFinishesAndClearsStaging(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)

	staging, _ := muxFixtureJob(t, w, db, "j-clean")
	writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 90)

	if err := w.MuxJob("j-clean"); err != nil {
		t.Fatalf("MuxJob: %v", err)
	}
	w.Stop()

	fresh, _ := db.GetJob("j-clean")
	if fresh == nil || fresh.Status != database.StatusFinished {
		t.Fatalf("job after a clean restart mux = %v, want Finished (error=%q)", statusOf(fresh), errorOf(fresh))
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("staging dir %s survived a successful restart mux (stat err = %v)", staging, err)
	}
}

// TestTruncatedMuxErrorsInsteadOfFinishing is ENGINE-9 end to end: a staged
// recording whose container is truncated mid-mdat still probes at its full
// declared length, `-c copy` stops at the first bad fragment and EXITS 0, and
// the job used to be written Finished over a third of a recording. It must
// land in Error with the numbers, and staging must survive for a re-mux.
//
// Mutant: dropping the verifyMuxedDuration call from muxAndFinalize — the job
// reads Finished and the staging dir is deleted under it.
func TestTruncatedMuxErrorsInsteadOfFinishing(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)

	staging, _ := muxFixtureJob(t, w, db, "j-trunc")
	media := filepath.Join(staging, "video.mp4")
	writeMuxFixture(t, ffmpegPath, media, 90)
	truncateFixture(t, media, 120000)

	if err := w.MuxJob("j-trunc"); err != nil {
		t.Fatalf("MuxJob: %v", err)
	}
	w.Stop()

	fresh, _ := db.GetJob("j-trunc")
	if fresh == nil || fresh.Status != database.StatusError {
		t.Fatalf("truncated mux left the job %v, want Error", statusOf(fresh))
	}
	if !strings.Contains(fresh.Error, "90") {
		t.Errorf("error %q does not name the input duration the copy fell short of", fresh.Error)
	}
	if _, err := os.Stat(media); err != nil {
		t.Errorf("staging media was removed after a short mux: %v", err)
	}
}

// TestRestartMuxWaitsForADownloadSlot pins extra item (b): N interrupted muxes
// at boot must not spawn N FFmpegs. The off-queue restart mux takes the same
// download slot a queued job takes, so num_parallel_downloads serialises them.
//
// Mutant: muxing without acquiring the slot — the job finishes while the only
// slot is held elsewhere.
func TestRestartMuxWaitsForADownloadSlot(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)
	w.SetParallelDownloads(1)

	staging, _ := muxFixtureJob(t, w, db, "j-slot")
	writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 90)

	if !w.queue.AcquireDownloadSlot(context.Background(), "holder") {
		t.Fatal("could not take the only download slot")
	}
	if err := w.MuxJob("j-slot"); err != nil {
		t.Fatalf("MuxJob: %v", err)
	}
	if reachedStatus(db, "j-slot", database.StatusFinished, 500*time.Millisecond) {
		t.Fatal("the restart mux ran while the only download slot was held elsewhere")
	}

	w.queue.ReleaseDownloadSlot("holder")
	if !reachedStatus(db, "j-slot", database.StatusFinished, 60*time.Second) {
		t.Fatal("the restart mux never finished after the download slot was freed")
	}
	w.Stop()
}

// TestRestartMuxCancelledByStopLeavesTheRowResumable pins extra item (c) —
// O-E on the off-queue path. A Stop cuts the mux root; the mux dies with the
// child and the row must be left exactly as the restarted child expects to
// find it: Muxing, with staging intact, never Error and never Finished.
//
// Mutants: giving MuxJob context.Background() (the mux runs anyway and the row
// leaves Muxing), or writing StatusError on a cancelled mux (the restarted
// child no longer re-muxes it — muxOnRestart only routes a Muxing row).
func TestRestartMuxCancelledByStopLeavesTheRowResumable(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)

	staging, _ := muxFixtureJob(t, w, db, "j-cancel")
	writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 90)

	// Stop's own cancellation, taken before the mux starts: the mux root is
	// already cut, so the FFmpeg the goroutine would launch never runs.
	w.orchestrator.CancelMuxes()
	if err := w.MuxJob("j-cancel"); err != nil {
		t.Fatalf("MuxJob: %v", err)
	}
	w.Stop()

	fresh, _ := db.GetJob("j-cancel")
	if fresh == nil || fresh.Status != database.StatusMuxing {
		t.Fatalf("job after a cancelled restart mux = %v, want Muxing (error=%q)", statusOf(fresh), errorOf(fresh))
	}
	if fresh.Error != "" {
		t.Errorf("cancelled mux wrote an error %q — the restarted child re-muxes this row, it is not a failure", fresh.Error)
	}
	if _, err := os.Stat(filepath.Join(staging, "video.mp4")); err != nil {
		t.Errorf("staging media was removed by a cancelled mux: %v", err)
	}
}

// TestIsStagedRestartPath pins the engine predicate package worker keys on: a
// recording the no-truncate guard set aside is <file>.restart-<unix ts>, and
// its resume sidecar shares that stem.
//
// Mutants: dropping the .resume.json exclusion (the sidecar is muxed as if it
// were media), or dropping the digits rule (any .restart- name counts).
func TestIsStagedRestartPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"video.mp4.restart-1700000000", true},
		{"video_stream.restart-1", true},
		{"video.mp4.restart-1700000000.resume.json", false},
		{"video.mp4.resume.json", false},
		{"video.mp4", false},
		{"video.mp4.restart-", false},
		{"video.mp4.restart-notatimestamp", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := engine.IsStagedRestartPath(tc.name); got != tc.want {
				t.Errorf("IsStagedRestartPath(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestStagedRecordingPartsOrdersAsidesFirst pins extra item (d): the engine
// leaves a recording it could not resume beside the fresh one as
// <file>.restart-<ts>, and the part list a staging dir yields is those asides
// in recording order followed by the live recording — with the sidecar twin
// excluded, since muxing a JSON file is not a recovery.
//
// Mutants: ignoring the aside (the part list is just the live recording, and
// the set-aside footage is invisible to every consumer), or treating the
// .resume.json twin as a part.
func TestStagedRecordingPartsOrdersAsidesFirst(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "video.mp4")
	aside := filepath.Join(dir, "video.mp4"+engine.StagedRestartSuffix+"1700000000")
	older := filepath.Join(dir, "video.mp4"+engine.StagedRestartSuffix+"1600000000")
	for _, p := range []string{live, aside, older, aside + ".resume.json"} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	got := stagedRecordingParts(dir)
	want := []string{older, aside, live}
	if len(got) != len(want) {
		t.Fatalf("stagedRecordingParts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stagedRecordingParts = %v, want %v (recording order: asides oldest first, then the live recording)", got, want)
		}
	}
}

// TestStagedAsideKeepsStagingFromCleanup is the other half of extra item (d):
// finalize must never delete an aside that has not been merged into the
// archive. The aside is captured footage no mux has consumed, so the staging
// dir it sits in is preserved exactly the way an unmuxed part's is.
//
// Mutant: dropping the aside term from hasUnmuxedPartsForJob — the successful
// mux of the fresh recording takes the set-aside one with it via RemoveAll.
func TestStagedAsideKeepsStagingFromCleanup(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)

	staging, _ := muxFixtureJob(t, w, db, "j-aside")
	if err := os.WriteFile(filepath.Join(staging, "video.mp4"), []byte("fresh"), 0o644); err != nil {
		t.Fatal(err)
	}
	aside := filepath.Join(staging, "video.mp4"+engine.StagedRestartSuffix+"1700000000")
	if err := os.WriteFile(aside, []byte("set aside"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !hasUnmuxedPartsForJob(db, "j-aside", staging) {
		t.Error("hasUnmuxedPartsForJob = false with a set-aside recording in staging, want true")
	}

	w.cleanupStagingAfterMux("j-aside", staging)
	if _, err := os.Stat(aside); err != nil {
		t.Errorf("the set-aside recording was deleted by staging cleanup: %v", err)
	}

	t.Run("control: the same dir without an aside is cleaned", func(t *testing.T) {
		if err := os.Remove(aside); err != nil {
			t.Fatal(err)
		}
		w.cleanupStagingAfterMux("j-aside", staging)
		if _, err := os.Stat(staging); !os.IsNotExist(err) {
			t.Errorf("staging survived cleanup with no aside present (stat err = %v) — proves the aside, not something else, held it above", err)
		}
	})
}

// --- helpers ---------------------------------------------------------------

// muxFixtureJob inserts a Muxing row whose output lands in the test's own temp
// tree and returns its (created) staging dir and output dir.
func muxFixtureJob(t *testing.T, w *DownloadWorker, db *database.Database, jobID string) (stagingDir, outputDir string) {
	t.Helper()
	outputDir = t.TempDir()
	job := &database.Job{ID: jobID, VideoID: jobID, URL: "u", Platform: "youtube", Status: database.StatusMuxing, OutputDirectory: outputDir}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob %s: %v", jobID, err)
	}
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
	stagingDir = filepath.Join(stagingBase, jobID)
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}
	return stagingDir, outputDir
}

// writeMuxFixture renders a real seconds-long MP4 with its moov up front, so a
// byte-truncated copy still probes at its full declared length (which is what
// makes the ENGINE-9 scenario reproducible).
func writeMuxFixture(t *testing.T, ffmpegPath, path string, seconds int) {
	t.Helper()
	cmd := exec.Command(ffmpegPath, "-nostdin", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=64x64:rate=5:duration=%d", seconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-movflags", "+faststart", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture %s: %v\n%s", path, err, out)
	}
}

// truncateFixture cuts a fixture off mid-mdat: ffprobe still reports the full
// duration from the leading moov, while `-c copy` stops at the first bad
// fragment and exits 0.
func truncateFixture(t *testing.T, path string, keep int64) {
	t.Helper()
	if err := os.Truncate(path, keep); err != nil {
		t.Fatalf("truncate %s: %v", path, err)
	}
}

// reachedStatus polls for a status within the window. Used for both the
// positive and the negative assertion on the slot gate.
func reachedStatus(db *database.Database, jobID string, want database.JobStatus, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if job, _ := db.GetJob(jobID); job != nil && job.Status == want {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func statusOf(job *database.Job) database.JobStatus {
	if job == nil {
		return "<missing>"
	}
	return job.Status
}

func errorOf(job *database.Job) string {
	if job == nil {
		return ""
	}
	return job.Error
}
