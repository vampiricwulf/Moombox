package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
)

// writeAsidePair drops one restart's worth of set-aside files into dir: the
// video half at `<stem>.restart-<stamp>`, optionally its resume twin, and
// optionally the audio half a DASH restart sets aside under the same second.
// Bytes, not media — every assertion in this file is about the SCAN, which
// never opens a file. The tests that actually mux use writeAsideFixture
// (mux_lifecycle_test.go), which needs FFmpeg.
func writeAsidePair(t *testing.T, dir, stamp string, size int, withSidecar, withAudio bool) {
	t.Helper()
	video := filepath.Join(dir, "video.mp4"+engine.StagedRestartSuffix+stamp)
	if err := os.WriteFile(video, make([]byte, size), 0o644); err != nil {
		t.Fatalf("write aside %s: %v", video, err)
	}
	if withSidecar {
		if err := os.WriteFile(engine.StagedRestartSidecar(video), []byte(`{"seq":1}`), 0o644); err != nil {
			t.Fatalf("write aside sidecar: %v", err)
		}
	}
	if withAudio {
		audio := filepath.Join(dir, "audio_stream"+engine.StagedRestartSuffix+stamp)
		if err := os.WriteFile(audio, make([]byte, size), 0o644); err != nil {
			t.Fatalf("write audio aside: %v", err)
		}
	}
}

// TestAsideReportGroupsEachRestartOnce is the shape the whole arc is built on:
// one Aside per RESTART, not per file. A DASH restart sets the video and audio
// halves aside under the same second, and both halves' bytes belong to the one
// recording an operator will get back.
//
// Mutants this kills:
//   - reporting one Aside per FILE (dropping groupStagedAsides): the count is
//     3, not 2, and the first group's Size is half what it should be.
//   - summing only the video half: Size is 100, not 200.
//   - ordering newest-first within a directory: the 1600000000 group is not first.
func TestAsideReportGroupsEachRestartOnce(t *testing.T) {
	dir := t.TempDir()
	writeAsidePair(t, dir, "1600000000", 100, true, true) // video + audio + sidecar
	writeAsidePair(t, dir, "1700000000", 50, false, false)

	got := asideReport(dir)

	if len(got.Groups) != 2 {
		t.Fatalf("asideReport returned %d groups, want 2 (one per restart, not one per file): %+v", len(got.Groups), got.Groups)
	}
	// Oldest first WITHIN one directory. Across directories the order is the
	// scan's — root, then each seg_N — which is what stagedAsideRecordings
	// produces and what the doc comment promises.
	if got.Groups[0].Timestamp != "2020-09-13T12:26:40Z" {
		t.Errorf("first group Timestamp = %q, want the 1600000000 stamp rendered as RFC 3339 — asides in one directory come back oldest first", got.Groups[0].Timestamp)
	}
	if got.Groups[0].Size != 200 {
		t.Errorf("first group Size = %d, want 200 (both halves of the restart)", got.Groups[0].Size)
	}
	if !got.Groups[0].HasResumeSidecar {
		t.Error("first group HasResumeSidecar = false, want true — its .resume.json is on disk")
	}
	if !strings.HasSuffix(got.Groups[0].Path, engine.StagedRestartSuffix+"1600000000") {
		t.Errorf("first group Path = %q, want the video half of the 1600000000 restart", got.Groups[0].Path)
	}
	if got.Groups[1].Size != 50 || got.Groups[1].HasResumeSidecar {
		t.Errorf("second group = %+v, want Size 50 and no sidecar", got.Groups[1])
	}
	if got.KeptChatSidecar {
		t.Error("KeptChatSidecar = true with no chat.json anywhere under the dir")
	}
}

// TestAsideReportFindsAKeptChatCaptureAtAnyDepth pins the rule the copy in
// Task 2 depends on: keepOnlyChatCapture preserves chat.json AT ANY DEPTH,
// because a quality- or gap-split job keeps each part's chat beside that
// part's media in seg_N/. Each case stages an aside too, because asideReport
// only looks for the chat capture once it has something to recover.
//
// Mutants this kill:
//   - looking only in the staging root: the seg_1 case reports false.
//   - accepting any chat.json.* member as the capture: the sidecar-only case
//     reports true, and Task 2 would then try to copy a file it never found.
func TestAsideReportFindsAKeptChatCaptureAtAnyDepth(t *testing.T) {
	t.Run("chat.json in a seg_N part dir", func(t *testing.T) {
		dir := t.TempDir()
		writeAsidePair(t, dir, "1700000000", 8, false, false)
		seg := filepath.Join(dir, "seg_1")
		if err := os.MkdirAll(seg, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(seg, "chat.json"), []byte(`{"messages":[]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if !asideReport(dir).KeptChatSidecar {
			t.Error("KeptChatSidecar = false with a chat.json under seg_1 — a split job's chat lives beside its part")
		}
		if got := findKeptChatCapture(dir); got != filepath.Join(seg, "chat.json") {
			t.Errorf("findKeptChatCapture = %q, want the seg_1 capture", got)
		}
	})

	t.Run("only the resume sidecar, no chat.json", func(t *testing.T) {
		dir := t.TempDir()
		writeAsidePair(t, dir, "1700000000", 8, false, false)
		if err := os.WriteFile(filepath.Join(dir, "chat.json.resume.json"), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if asideReport(dir).KeptChatSidecar {
			t.Error("KeptChatSidecar = true with no chat.json — there is nothing for a recovery to copy")
		}
		if got := findKeptChatCapture(dir); got != "" {
			t.Errorf("findKeptChatCapture = %q, want \"\"", got)
		}
	})

	t.Run("the walk is skipped when nothing was set aside", func(t *testing.T) {
		// Mutant: evaluating findKeptChatCapture before the group count —
		// every GET /api/jobs/{id} in the fleet walks a staging tree to
		// populate a flag neither UI renders without a recording to recover.
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "chat.json"), []byte(`{"messages":[]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := asideReport(dir); got.KeptChatSidecar {
			t.Error("asideReport reported a chat capture for a dir with no asides — the tree walk must be short-circuited")
		}
	})
}

// TestAsideReportGroupsIsNeverNil keeps the JSON payload an array. The
// dashboard reads `enriched.asides` straight into a .length, and the TUI
// wiring ranges over it.
//
// Mutant: `var out []Aside` instead of make(...,0,n) — the field marshals as
// null and the assertion below fails.
func TestAsideReportGroupsIsNeverNil(t *testing.T) {
	if got := asideReport(t.TempDir()).Groups; got == nil {
		t.Error("asideReport(empty dir).Groups is nil — it must marshal as [], not null")
	}
	if got := asideReport(filepath.Join(t.TempDir(), "does-not-exist")).Groups; got == nil {
		t.Error("asideReport(missing dir).Groups is nil — a job that never staged anything must still answer []")
	}
}

// TestScanAsidesJoinsTheStagingBaseLikeHasSegmentFiles pins the exported
// entry point's shape: (stagingBase, jobID), the same two arguments
// HasSegmentFiles takes, so the route can call it with the base it already
// reads out of the config store and no worker instance.
//
// Mutant: taking a full staging DIR instead of the base — the join is done
// twice (or not at all) and the scan reads the wrong directory, so the report
// comes back empty.
func TestScanAsidesJoinsTheStagingBaseLikeHasSegmentFiles(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "job-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAsidePair(t, dir, "1700000000", 8, false, false)

	if got := ScanAsides(base, "job-1"); len(got.Groups) != 1 {
		t.Errorf("ScanAsides(base, %q) found %d groups, want 1", "job-1", len(got.Groups))
	}
	if got := ScanAsides(base, "job-2"); len(got.Groups) != 0 {
		t.Errorf("ScanAsides(base, %q) found %d groups for a job with no staging dir, want 0", "job-2", len(got.Groups))
	}
}

// TestWorkerAsidesReadsTheConfiguredStagingBase is the method the TUI wiring
// calls: it resolves the staging base itself and refuses a job that is not in
// the database, so a caller cannot be handed a report for a row that no
// longer exists.
//
// Mutants this kills:
//   - dropping the job lookup: the unknown-job call returns a nil error.
//   - reading Paths.OutputDirectory instead of EffectiveStagingDir(): the
//     known job reports no asides.
func TestWorkerAsidesReadsTheConfiguredStagingBase(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)

	if _, err := db.AddJob(&database.Job{
		ID: "j-report", VideoID: "j-report", URL: "u", Platform: "youtube", Status: database.StatusCancelled,
	}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	var base string
	w.readConfig(func(c *config.MoomboxConfig) { base = c.Paths.EffectiveStagingDir() })
	dir := filepath.Join(base, "j-report")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAsidePair(t, dir, "1700000000", 16, true, false)
	if err := os.WriteFile(filepath.Join(dir, "chat.json"), []byte(`{"messages":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := w.Asides("j-report")
	if err != nil {
		t.Fatalf("Asides: %v", err)
	}
	if len(report.Groups) != 1 || !report.KeptChatSidecar {
		t.Errorf("Asides = %+v, want one group and KeptChatSidecar true", report)
	}

	if _, err := w.Asides("no-such-job"); err == nil {
		t.Error("Asides on an unknown job returned a nil error — the caller cannot tell an empty report from a missing row")
	}
}

// TestRecoverAsidesMuxesEveryGroupAndCarriesTheChatSidecar is the whole verb,
// end to end, on the realistic shape: a Finished job whose chat capture ended
// incomplete, so keepOnlyChatCapture kept chat.json in staging, and whose
// engine set two recordings aside across two restarts.
//
// Mutants this kills:
//   - the chat copy dropped: no <first sibling>.chat.json beside the output,
//     and the only copy of those comments stays in a staging dir the operator
//     is being told to let go.
//   - the chat copied beside EVERY sibling: the second assertion fails — one
//     chat file per stream, beside the first recovered recording.
//   - the aside not deleted after a successful mux: stagedAsideRecordings is
//     non-empty, so cleanupStagingAfterMux's aside shield holds the dir
//     forever and the verb has achieved nothing.
//   - cleanupStagingAfterMux not called: the raw media survives beside the
//     chat capture, roughly doubling the archive's disk cost.
func TestRecoverAsidesMuxesEveryGroupAndCarriesTheChatSidecar(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)

	staging, outputDir := muxFixtureJob(t, w, db, "j-recover")
	if db.UpdateJobFields("j-recover", map[string]any{
		"status":      database.StatusFinished,
		"chat_status": chatStatusIncomplete,
	}) == nil {
		t.Fatal("UpdateJobFields returned nil — the fixture row is not there")
	}
	writeAsideFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"+engine.StagedRestartSuffix+"1700000000"), 5)
	writeAsideFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"+engine.StagedRestartSuffix+"1700000100"), 5)
	chatSrc := filepath.Join(staging, "chat.json")
	if err := os.WriteFile(chatSrc, []byte(`{"messages":[{"id":"m1"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := w.RecoverAsides("j-recover"); err != nil {
		t.Fatalf("RecoverAsides: %v", err)
	}
	w.Stop()

	first := mp4Named(t, outputDir, engine.StagedRestartSuffix+"1700000000.mp4")
	second := mp4Named(t, outputDir, engine.StagedRestartSuffix+"1700000100.mp4")
	if first == "" || second == "" {
		t.Fatalf("recovery produced %v, want one sibling per restart", mp4sIn(t, outputDir))
	}

	chatDst := strings.TrimSuffix(first, ".mp4") + ".chat.json"
	if _, err := os.Stat(chatDst); err != nil {
		t.Errorf("no %s beside the first recovered recording (stat err = %v) — the kept chat capture never left staging", filepath.Base(chatDst), err)
	}
	if secondChat := strings.TrimSuffix(second, ".mp4") + ".chat.json"; fileExists(secondChat) {
		t.Errorf("%s exists — one stream has one chat archive, and it goes beside the FIRST recovered recording", filepath.Base(secondChat))
	}

	if left := stagedAsideRecordings(staging); len(left) != 0 {
		t.Errorf("%d set-aside recording(s) still in staging after a successful recovery: %v", len(left), left)
	}
	// chat_status is still incomplete, so cleanupStagingAfterMux prunes the
	// dir down to the chat capture rather than deleting it: the media goes,
	// the chat stays for a later re-run.
	if _, err := os.Stat(chatSrc); err != nil {
		t.Errorf("the staged chat capture was deleted (stat err = %v) — the chat-incomplete shield must survive a recovery", err)
	}
	if discoverStagingMedia(staging) != nil {
		t.Error("recognised media survived in staging — cleanupStagingAfterMux did not run, or its chat carve-out kept more than the chat")
	}
}

// TestRecoverAsidesNeverDeletesAnUnmuxedRecording is the data-loss pin.
//
// The commonest shape this arc exists for is a Cancelled job whose staging
// holds BOTH the fresh recording the restart began and the aside it replaced.
// cleanupStagingAfterMux's four shields do not cover it: the asides are gone
// (the recovery just consumed them), hasUnmuxedSegmentParts is false for a
// single-file job with no seg_N dirs, the tail is not flagged and the chat is
// not incomplete — so an unguarded cleanup calls os.RemoveAll and destroys the
// main recording that /mux and A M exist to rescue.
//
// Mutants this kills:
//   - the media guard dropped (the unconditional cleanupStagingAfterMux the
//     first draft of this plan specified): video.mp4 and the whole staging
//     directory are gone, and the Mux at the end of this test has nothing to
//     work with.
//   - the guard inverted so staging is NEVER cleaned: the aside-only test
//     above fails instead.
func TestRecoverAsidesNeverDeletesAnUnmuxedRecording(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)

	staging, outputDir := muxFixtureJob(t, w, db, "j-both")
	db.UpdateJobFields("j-both", map[string]any{"status": database.StatusCancelled})
	main := filepath.Join(staging, "video.mp4")
	writeMuxFixture(t, ffmpegPath, main, 10)
	writeAsideFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"+engine.StagedRestartSuffix+"1700000000"), 5)

	if err := w.RecoverAsides("j-both"); err != nil {
		t.Fatalf("RecoverAsides: %v", err)
	}
	// Drain the recovery without ending the worker: Stop waits on the same
	// WaitGroup the Mux below needs, so it is called once at the end.
	waitForStagingClaim(t, w, "j-both")

	if _, err := os.Stat(main); err != nil {
		t.Fatalf("DATA LOSS: the never-muxed recording %s was deleted by the recovery (stat err = %v)", main, err)
	}
	if _, err := os.Stat(staging); err != nil {
		t.Fatalf("DATA LOSS: the staging dir was removed after recovering only the asides (stat err = %v)", err)
	}
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
	if !HasSegmentFiles(stagingBase, "j-both") {
		t.Fatal("HasSegmentFiles is false after the recovery — /mux and A M can no longer reach the recording")
	}

	// …and the Mux action still does its job on what is left.
	if err := w.MuxJob("j-both"); err != nil {
		t.Fatalf("MuxJob after a recovery: %v", err)
	}
	w.Stop()

	fresh, _ := db.GetJob("j-both")
	if fresh == nil || fresh.Status != database.StatusFinished {
		t.Fatalf("job after the follow-up mux = %v, want Finished (error=%q)", statusOf(fresh), errorOf(fresh))
	}
	if _, err := os.Stat(fresh.OutputFile); err != nil {
		t.Errorf("the main recording never reached the archive: %v", err)
	}
	if mp4Named(t, outputDir, engine.StagedRestartSuffix+"1700000000.mp4") == "" {
		t.Errorf("the recovered sibling is missing: %v", mp4sIn(t, outputDir))
	}
}

// waitForStagingClaim blocks until a job's staging claim is free again.
//
// Deterministic rather than a sleep: every defer in the recovery goroutine —
// the claim release included — runs after the body, so being able to TAKE the
// claim proves the mux, the chat copy and the cleanup decision have all
// happened. Used where a test needs the recovery finished but the worker still
// alive, because w.Stop drains the WaitGroup a follow-up MuxJob needs.
func waitForStagingClaim(t *testing.T, w *DownloadWorker, jobID string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if release, err := w.claimJobOperation(jobID, "test probe"); err == nil {
			release()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the set-aside recovery for %s did not finish within 60s", jobID)
}

// TestRecoverAsidesCleansStagingWhenNothingIsKept is the same verb on the
// plain shape: no chat carve-out, no incomplete tail, so once the last aside
// is recovered there is nothing left to shield and the dir goes.
//
// Mutant: skipping cleanupStagingAfterMux — the dir survives, which is the
// exact leak sweep-2 Task 2 found on the off-queue Mux path.
func TestRecoverAsidesCleansStagingWhenNothingIsKept(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)

	staging, outputDir := muxFixtureJob(t, w, db, "j-recover-clean")
	db.UpdateJobFields("j-recover-clean", map[string]any{"status": database.StatusCancelled})
	writeAsideFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"+engine.StagedRestartSuffix+"1700000000"), 5)

	if err := w.RecoverAsides("j-recover-clean"); err != nil {
		t.Fatalf("RecoverAsides: %v", err)
	}
	w.Stop()

	if mp4Named(t, outputDir, engine.StagedRestartSuffix+"1700000000.mp4") == "" {
		t.Fatalf("recovery produced %v, want the sibling", mp4sIn(t, outputDir))
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("the staging dir survived a complete recovery (stat err = %v) — nothing in it is shielded any more", err)
	}
}

// TestRecoverAsidesRefusesAnActiveJob: an active job's staging is being
// written. Reading a half-written aside would be pointless and muxing over
// the output directory while the download runs is worse.
//
// Mutant: dropping the IsActiveJobStatus gate — the call is accepted and a
// second FFmpeg starts against a live staging dir.
func TestRecoverAsidesRefusesAnActiveJob(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)

	staging, _ := muxFixtureJob(t, w, db, "j-active") // muxFixtureJob inserts Muxing
	writeAsidePair(t, staging, "1700000000", 8, false, false)

	for _, st := range []database.JobStatus{
		database.StatusDownloading, database.StatusMuxing, database.StatusLive, database.StatusUpcoming,
	} {
		db.UpdateJobFields("j-active", map[string]any{"status": st})
		err := w.RecoverAsides("j-active")
		if !errors.Is(err, ErrRecoveryJobActive) {
			t.Errorf("RecoverAsides on a %s job = %v, want ErrRecoveryJobActive", st, err)
		}
	}
}

// TestRecoverAsidesRefusesWhenStagingHoldsNoAsides keeps the verb honest: it
// is not a second Mux button, and a job with an ordinary staging dir must be
// told so rather than have a no-op goroutine started for it.
//
// Mutant: dropping the ScanAsides precondition — a nil error comes back and
// the UI reports a recovery that never had anything to recover.
func TestRecoverAsidesRefusesWhenStagingHoldsNoAsides(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)

	staging, _ := muxFixtureJob(t, w, db, "j-none")
	db.UpdateJobFields("j-none", map[string]any{"status": database.StatusCancelled})
	if err := os.WriteFile(filepath.Join(staging, "video.mp4"), []byte("ordinary media"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := w.RecoverAsides("j-none"); !errors.Is(err, ErrNoAsides) {
		t.Errorf("RecoverAsides with no asides in staging = %v, want ErrNoAsides", err)
	}
	if err := w.RecoverAsides("no-such-job"); err == nil {
		t.Error("RecoverAsides on an unknown job returned nil")
	}
}

// TestStagingClaimRefusesBothVerbsInBothOrders pins the claim's SYMMETRY, and
// does it without racing two real muxes: the claim is taken synchronously
// before either verb spawns its goroutine, so holding it by hand is exactly
// the state a running operation leaves behind.
//
// Both verbs write into one staging directory and one output directory, and
// both call muxStagedAsides — each with its OWN asideOutputPath `used` map, so
// two of them running together both pick the plain <stem>.restart-<ts>.mp4 and
// write over each other. Recover→Recover and Mux→Recover were covered by
// status alone; Recover→Mux was not, because MuxJob has no status
// precondition of its own.
//
// Mutants this kills:
//   - MuxJob not taking the claim (the first draft): the Recover→Mux row is
//     accepted, which is the collision above.
//   - RecoverAsides not taking it: the Recover→Recover row is accepted.
//   - a global rather than per-job claim: the "a second job is unaffected"
//     row fails and one recovery freezes every other job's Mux button.
func TestStagingClaimRefusesBothVerbsInBothOrders(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)

	staging, _ := muxFixtureJob(t, w, db, "j-claim")
	db.UpdateJobFields("j-claim", map[string]any{"status": database.StatusCancelled})
	writeAsidePair(t, staging, "1700000000", 8, false, false)
	// Real media too, so MuxJob's HasSegmentFiles precondition passes and the
	// claim is demonstrably what refuses it.
	writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 2)

	release, err := w.claimJobOperation("j-claim", opRecoverAsides)
	if err != nil {
		t.Fatalf("claimJobOperation on an idle job: %v", err)
	}
	if err := w.RecoverAsides("j-claim"); !errors.Is(err, ErrStagingBusy) {
		t.Errorf("RecoverAsides while a recovery holds the claim = %v, want ErrStagingBusy", err)
	}
	if err := w.MuxJob("j-claim"); !errors.Is(err, ErrStagingBusy) {
		t.Errorf("MuxJob while a recovery holds the claim = %v, want ErrStagingBusy — this is the Recover-then-Mux collision", err)
	}
	if fresh, _ := db.GetJob("j-claim"); fresh != nil && fresh.Status == database.StatusMuxing {
		t.Error("the refused MuxJob still wrote status=Muxing — the claim must be taken before the status write")
	}

	// A different job is unaffected: the claim is per job, not global.
	otherRelease, err := w.claimJobOperation("j-other", opMux)
	if err != nil {
		t.Errorf("claimJobOperation on a second job = %v, want it granted — the guard must not be global", err)
	} else {
		otherRelease()
	}

	release()
	if err := w.RecoverAsides("j-claim"); err != nil {
		t.Errorf("RecoverAsides after the claim was released = %v, want it accepted — the job must not be stuck for the process's life", err)
	}
}

// TestStagingClaimIsRaceFree: eight goroutines racing one job must produce
// exactly one winner. The claim is the only thing standing between them.
//
// Mutant: reading the map outside recoverMu (or checking then locking) —
// `-race` reports it and accepted climbs above 1.
func TestStagingClaimIsRaceFree(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	muxFixtureJob(t, w, db, "j-race")

	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { _ = recover() }()
			if release, err := w.claimJobOperation("j-race", opRecoverAsides); err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
				_ = release
			}
		}()
	}
	wg.Wait()
	if accepted != 1 {
		t.Errorf("%d goroutines took the same job's staging claim, want exactly 1", accepted)
	}
}

// TestRecoverAsidesRoutesItsLogLinesToTheJob: both UIs promise that a
// recovery's progress shows up in the job's log lines, and neither can deliver
// on its own. Per-job routing comes from db.logRouted, which
// SyncJobLogTracking empties of every terminal job — and recovery runs ONLY on
// terminal jobs and deliberately never writes a status. Without the bracket,
// the dashboard's disabled Recover button is followed by silence.
//
// The logger here is the one internal/logger installs in the real binary,
// reduced to the one thing that matters: it feeds RouteLogToJobs.
//
// Mutants this kills:
//   - TrackJobForLogs dropped: the buffer is empty.
//   - the untrack at the end dropped: the "after" line still lands, i.e. a
//     terminal job stays in the routed set forever, which is CORE-12.
func TestRecoverAsidesRoutesItsLogLinesToTheJob(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)

	rl := routingLogger{db: db}
	w.logger = rl
	w.orchestrator.logger = rl

	staging, _ := muxFixtureJob(t, w, db, "j-logs")
	db.UpdateJobFields("j-logs", map[string]any{"status": database.StatusFinished})
	writeAsideFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"+engine.StagedRestartSuffix+"1700000000"), 5)

	// Baseline: a terminal job is NOT routed to.
	db.RouteLogToJobs("before the recovery, job j-logs")
	if got := db.GetJobLogs("j-logs"); len(got) != 0 {
		t.Fatalf("a terminal job was already routed to before the recovery: %v", got)
	}

	if err := w.RecoverAsides("j-logs"); err != nil {
		t.Fatalf("RecoverAsides: %v", err)
	}
	w.Stop()

	if got := db.GetJobLogs("j-logs"); len(got) == 0 {
		t.Error("the recovery produced no routed log lines — both UIs point the operator at the job's log for its progress")
	}

	db.RouteLogToJobs("after the recovery, job j-logs")
	after := db.GetJobLogs("j-logs")
	if len(after) > 0 && strings.Contains(after[len(after)-1], "after the recovery") {
		t.Error("the job is still tracked for log routing after the recovery — a terminal ID left in the routed set is CORE-12")
	}
}

// routingLogger is internal/logger's routing behaviour, reduced to the one
// thing these tests need: every line it is given is offered to
// db.RouteLogToJobs, so a tracked job collects it.
type routingLogger struct{ db *database.Database }

func (l routingLogger) line(msg string, args []any) {
	var b strings.Builder
	b.WriteString(msg)
	for _, a := range args {
		fmt.Fprintf(&b, " %v", a)
	}
	l.db.RouteLogToJobs(b.String())
}

func (l routingLogger) Debug(msg string, args ...any) { l.line(msg, args) }
func (l routingLogger) Info(msg string, args ...any)  { l.line(msg, args) }
func (l routingLogger) Warn(msg string, args ...any)  { l.line(msg, args) }
func (l routingLogger) Error(msg string, args ...any) { l.line(msg, args) }

// TestCopyKeptChatSidecarNeverDuplicatesAnArchive: one stream has one chat
// archive. A chat-incomplete job that DID finalize already wrote it as
// <filenameBase>.chat.json, and a recovery that copied the staged capture
// again would leave two copies of the same comments in the output directory
// under different names.
//
// Mutants this kills:
//   - checking only the sibling destination (the first draft, whose comment
//     claimed a finalize could own that name — it cannot; finalize writes
//     <filenameBase>.chat.json, never <filenameBase>.restart-<ts>.chat.json):
//     the third row copies a duplicate.
//   - dropping the destination check: the second row overwrites a previous
//     recovery's archive.
func TestCopyKeptChatSidecarNeverDuplicatesAnArchive(t *testing.T) {
	newCase := func(t *testing.T) (staging, out, sibling string) {
		t.Helper()
		staging, out = t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(staging, "chat.json"), []byte(`{"messages":[{"id":"m1"}]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		sibling = filepath.Join(out, "Title"+engine.StagedRestartSuffix+"1700000000.mp4")
		if err := os.WriteFile(sibling, []byte("recovered"), 0o644); err != nil {
			t.Fatal(err)
		}
		return staging, out, sibling
	}

	t.Run("nothing there yet", func(t *testing.T) {
		staging, out, sibling := newCase(t)
		dst, err := copyKeptChatSidecar(staging, sibling, filepath.Join(out, "Title.chat.json"))
		if err != nil {
			t.Fatalf("copyKeptChatSidecar: %v", err)
		}
		want := filepath.Join(out, "Title"+engine.StagedRestartSuffix+"1700000000.chat.json")
		if dst != want {
			t.Errorf("dst = %q, want %q", dst, want)
		}
		if !fileExists(want) {
			t.Error("the chat capture was not copied")
		}
	})

	t.Run("the sibling already has one", func(t *testing.T) {
		staging, out, sibling := newCase(t)
		existing := filepath.Join(out, "Title"+engine.StagedRestartSuffix+"1700000000.chat.json")
		if err := os.WriteFile(existing, []byte(`{"messages":[{"id":"older"}]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if dst, err := copyKeptChatSidecar(staging, sibling, filepath.Join(out, "Title.chat.json")); err != nil || dst != "" {
			t.Errorf("copyKeptChatSidecar = (%q, %v), want (\"\", nil) — never overwrite an archive", dst, err)
		}
		body, _ := os.ReadFile(existing)
		if !strings.Contains(string(body), "older") {
			t.Error("the existing archive was overwritten")
		}
	})

	t.Run("the job's own finalize already wrote one", func(t *testing.T) {
		staging, out, sibling := newCase(t)
		jobChat := filepath.Join(out, "Title.chat.json")
		if err := os.WriteFile(jobChat, []byte(`{"messages":[{"id":"m1"}]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if dst, err := copyKeptChatSidecar(staging, sibling, jobChat); err != nil || dst != "" {
			t.Errorf("copyKeptChatSidecar = (%q, %v), want (\"\", nil) — the same comments are already in the output dir", dst, err)
		}
		if fileExists(filepath.Join(out, "Title"+engine.StagedRestartSuffix+"1700000000.chat.json")) {
			t.Error("a second copy of the same chat archive was written")
		}
	})
}

// TestRecoverAsidesReportsTheGroupItCouldNotMux: muxStagedAsides is
// best-effort per group, so a partial success must be REPORTED rather than
// swallowed — the unrecovered aside stays in staging (where its shield keeps
// it) and the operator has to know the recovery was incomplete.
//
// Driven through the orchestrator method rather than the worker's goroutine
// so the error is returned rather than logged.
//
// Mutants this kills:
//   - returning nil regardless of leftovers: a partial recovery reports
//     success, and the operator is never told footage is still stuck.
//   - deleting an aside whose mux failed: the second assertion fails and the
//     only copy of that footage is gone.
func TestRecoverAsidesReportsTheGroupItCouldNotMux(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)

	staging, outputDir := muxFixtureJob(t, w, db, "j-partial")
	db.UpdateJobFields("j-partial", map[string]any{"status": database.StatusCancelled})
	writeAsideFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"+engine.StagedRestartSuffix+"1700000000"), 5)
	bad := filepath.Join(staging, "video.mp4"+engine.StagedRestartSuffix+"1700000100")
	if err := os.WriteFile(bad, []byte("this is not a media file"), 0o644); err != nil {
		t.Fatal(err)
	}

	job, err := db.GetJob("j-partial")
	if err != nil || job == nil {
		t.Fatalf("GetJob: %v", err)
	}
	err = w.orchestrator.recoverAsides(context.Background(), w.buildJobContext(job))
	if err == nil {
		t.Fatal("recoverAsides returned nil with one group left in staging — a partial recovery must report")
	}
	if !strings.Contains(err.Error(), "1 of 2") {
		t.Errorf("recoverAsides error = %q, want it to name how many of how many were recovered", err)
	}
	if _, statErr := os.Stat(bad); statErr != nil {
		t.Errorf("the aside whose mux failed was deleted (stat err = %v) — it is the only copy of that footage", statErr)
	}
	if mp4Named(t, outputDir, engine.StagedRestartSuffix+"1700000000.mp4") == "" {
		t.Errorf("the readable group was not recovered: %v", mp4sIn(t, outputDir))
	}
}

// TestRecoveredChatSidecarIsNotOfferedAsAnOrphan closes the hole the chat copy
// opens. A recovered sibling is owned by its archive's stem
// (engine.RestartSiblingStem), but the chat file beside it is named
// <stem>.restart-<ts>.chat.json, whose ".json" extension leaves ".chat" in the
// stamp — so the sibling predicate rejects it and the sweep offers it as a
// deletable output orphan the moment recovery writes it.
//
// Mutant: dropping the .chat.json fold in scanOutputOrphans — the copied chat
// archive appears in the Files tab, one Delete All from being destroyed.
func TestRecoveredChatSidecarIsNotOfferedAsAnOrphan(t *testing.T) {
	_, db := testWorkerSetup(t)
	outputDir := t.TempDir()

	if _, err := db.AddJob(&database.Job{
		ID: "j-owned", VideoID: "j-owned", URL: "u", Platform: "youtube",
		Status: database.StatusFinished, OutputDirectory: outputDir,
		OutputFile: filepath.Join(outputDir, "Title.mp4"),
	}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	for _, name := range []string{
		"Title.mp4",
		"Title" + engine.StagedRestartSuffix + "1700000000.mp4",
		"Title" + engine.StagedRestartSuffix + "1700000000.chat.json",
	} {
		if err := os.WriteFile(filepath.Join(outputDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.MoomboxConfig{}
	cfg.Paths.OutputDirectory = outputDir
	entries, err := scanOutputOrphans(db, cfg)
	if err != nil {
		t.Fatalf("scanOutputOrphans: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Path, ".chat.json") {
			t.Errorf("the recovered recording's chat archive %s is offered as a deletable output orphan", e.RelPath)
		}
	}
}

// mp4Named returns the absolute path of the one .mp4 in dir whose name ends
// with suffix, or "" — the archive's own name comes from the filename
// template, so assertions name the SUFFIX and never the whole file.
func mp4Named(t *testing.T, dir, suffix string) string {
	t.Helper()
	for _, name := range mp4sIn(t, dir) {
		if strings.HasSuffix(name, suffix) {
			return filepath.Join(dir, name)
		}
	}
	return ""
}
