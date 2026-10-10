package worker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// TestSpanSecTakesTheStartOffAFragmentedMovOnly pins how an input's length is
// measured for the shortfall check. The mov demuxer reports a fragmented MP4's
// duration as the end timestamp of its last fragment — start included — so a
// DASH part that began 208 s into the broadcast probes as (start 208,
// duration 298) for 90 s of media. The MPEG-TS demuxer estimates duration as
// last minus first, so its duration already is the span and its start (hours,
// for a Twitch capture joined mid-broadcast) must stay out of the sum.
//
// Mutants, one per row:
//   - returning DurationSec unconditionally: the first row reads 298.
//   - subtracting for every container: the mpegts row reads negative, and
//     the check is silently disabled for every long Twitch recording.
//   - subtracting a negative start: AAC priming inflates the span.
func TestSpanSecTakesTheStartOffAFragmentedMovOnly(t *testing.T) {
	const mov = "mov,mp4,m4a,3gp,3g2,mj2"
	for _, tc := range []struct {
		name  string
		probe *ffprobeData
		want  float64
	}{
		{"fragmented mov part that began at 208 s", &ffprobeData{FormatName: mov, StartSec: 208, DurationSec: 298}, 90},
		{"mov from the start of the broadcast", &ffprobeData{FormatName: mov, StartSec: 0, DurationSec: 90}, 90},
		{"mpegts keeps its estimated span", &ffprobeData{FormatName: "mpegts", StartSec: 209.4, DurationSec: 90}, 90},
		{"negative start (priming) is left alone", &ffprobeData{FormatName: mov, StartSec: -0.046, DurationSec: 90}, 90},
		{"unknown container", &ffprobeData{FormatName: "", StartSec: 100, DurationSec: 300}, 300},
		{"nil probe", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.probe.spanSec(); got != tc.want {
				t.Errorf("spanSec() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestVerifyMuxedDurationMeasuresAnOffsetInputBySpan is the field failure
// against a REAL ffprobe: part 2 of a YouTube quality split began 208 s into
// the broadcast, its 7668 s probed as duration 7876, and a whole copy was
// rejected as "208s missing" — at finalize, again by segment recovery, and it
// would have been again by the Mux action the error recommended. A whole copy
// of such an input must pass, a genuinely short one must still fail naming
// the span, and an MPEG-TS input with the same offset must keep its full
// protection (its probed duration is already the span).
//
// Mutants:
//   - comparing probe.DurationSec: the fragmented 90 s input reads 298 and a
//     whole 90 s copy is rejected.
//   - subtracting start_time for every container: the mpegts input's span
//     goes negative, the floor skips it, and a 5 s copy of 90 s passes.
func TestVerifyMuxedDurationMeasuresAnOffsetInputBySpan(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	dir := t.TempDir()
	o := NewDownloadOrchestrator(nil, nil, ffmpegPath, discardLogger{}, nil, nil, nil, nil, nil)
	ctx := context.Background()

	frag := filepath.Join(dir, "video_stream")
	writeOffsetFragmentFixture(t, ffmpegPath, frag, 90, 208)
	if p := o.runFFprobe(ctx, frag); p == nil || p.StartSec < 200 || p.DurationSec < 290 {
		t.Fatalf("fragment fixture probed as %+v — the offset did not take, and the test would pass vacuously", p)
	}

	if err := o.verifyMuxedDuration(ctx, "j1", 90, frag, ""); err != nil {
		t.Errorf("a whole 90 s copy of a part that began at 208 s was rejected: %v", err)
	}
	err := o.verifyMuxedDuration(ctx, "j1", 5, frag, "")
	if err == nil {
		t.Fatal("a 5 s copy of a 90 s part passed — the shortfall check is gone, not corrected")
	}
	if !strings.Contains(err.Error(), "90") || strings.Contains(err.Error(), "298") {
		t.Errorf("shortfall error %q should name the 90 s span, not the 298 s end timestamp", err)
	}

	ts := filepath.Join(dir, "video.ts")
	writeOffsetTSFixture(t, ffmpegPath, ts, 90, 208)
	if p := o.runFFprobe(ctx, ts); p == nil || p.StartSec < 200 {
		t.Fatalf("mpegts fixture probed as %+v — the offset did not take", p)
	}
	if err := o.verifyMuxedDuration(ctx, "j1", 90, ts, ""); err != nil {
		t.Errorf("a whole 90 s copy of an mpegts input starting at 208 s was rejected: %v", err)
	}
	if err := o.verifyMuxedDuration(ctx, "j1", 5, ts, ""); err == nil {
		t.Error("a 5 s copy of a 90 s mpegts input passed — the start was taken off a span that never included it")
	}
}

// TestLaterPartOfASplitMuxes drives the field failure through the part path
// itself: a seg_1 recording that begins at 208 s of the broadcast must come
// out of muxSegment as a persisted part of its real length, not as a
// shortfall error that leaves it in staging.
//
// Mutant: measuring the input by its probed duration — muxSegment discards
// the part and returns "208s missing".
func TestLaterPartOfASplitMuxes(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)

	staging, outputDir := muxFixtureJob(t, w, db, "j-offset-part")
	segDir := filepath.Join(staging, "seg_1")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(segDir, "video_stream")
	writeOffsetFragmentFixture(t, ffmpegPath, media, 90, 208)

	job, _ := db.GetJob("j-offset-part")
	jobCtx := w.buildJobContext(job)
	seg, err := w.orchestrator.muxSegment(context.Background(), jobCtx, 1, 0, time.Now().Unix(),
		QualityInfo{Label: "1080p"}, &DownloadResult{HasVideo: true, VideoPath: media})
	if err != nil {
		t.Fatalf("muxSegment on a part that began at 208 s = %v, want a muxed part — the copy carried all of it", err)
	}
	if seg == nil {
		t.Fatal("muxSegment returned no segment for a whole copy")
	}
	if d := seg.DurationSeconds - 90; d < -2 || d > 2 {
		t.Errorf("part duration = %.1fs, want ~90s", seg.DurationSeconds)
	}
	if left := mp4sIn(t, outputDir); len(left) != 1 {
		t.Errorf("output dir holds %v, want the one part file", left)
	}
	if rows, _ := db.GetSegments("j-offset-part"); len(rows) != 1 || rows[0].SegmentIndex != 1 {
		t.Errorf("segment rows = %+v, want one row at index 1", rows)
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

	staging, outputDir := muxFixtureJob(t, w, db, "j-trunc")
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
	// Close-wave B3: the truncated copy must not survive under the archive's
	// own name. It was left there with output_file empty, so the operator
	// could neither play it nor find it in the UI, the orphan sweep offered
	// it as an unowned file, and a later Mux action wrote its retry beside a
	// bad file wearing the archive's name.
	//
	// Mutant: returning the shortfall error without removing the output — the
	// .mp4 below is still in the output dir.
	if left := mp4sIn(t, outputDir); len(left) != 0 {
		t.Errorf("a short mux left %v in the output dir — the archive name must be free for the re-mux", left)
	}
}

// TestTruncatedPartMuxLeavesNoPartFile is B3's twin on the PART path
// (muxSegment): the same `-c copy` shortfall, one part file. muxSegment
// already returns before AddSegment, so the row never appears — but the
// truncated "{name} - partN.mp4" stayed in the output dir, unreferenced by
// any segment row and indistinguishable from the real part a re-mux writes.
//
// Driven directly rather than through a split capture: the site's only input
// is a staged recording and a probe, and both are on disk here.
//
// Mutant: returning the shortfall error without removing the part file.
func TestTruncatedPartMuxLeavesNoPartFile(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)

	staging, outputDir := muxFixtureJob(t, w, db, "j-trunc-part")
	media := filepath.Join(staging, "video.mp4")
	writeMuxFixture(t, ffmpegPath, media, 90)
	truncateFixture(t, media, 120000)

	job, _ := db.GetJob("j-trunc-part")
	jobCtx := w.buildJobContext(job)
	seg, err := w.orchestrator.muxSegment(context.Background(), jobCtx, 0, 0, time.Now().Unix(),
		QualityInfo{Label: "720p"}, &DownloadResult{HasVideo: true, VideoPath: media})
	if err == nil {
		t.Fatalf("muxSegment on a truncated input = (%v, nil), want a shortfall error", seg)
	}
	if seg != nil {
		t.Errorf("muxSegment returned a segment %+v alongside its shortfall error", seg)
	}
	if left := mp4sIn(t, outputDir); len(left) != 0 {
		t.Errorf("a short part mux left %v in the output dir — an unreferenced truncated part "+
			"the operator cannot tell from the real one", left)
	}
	if _, err := os.Stat(media); err != nil {
		t.Errorf("staging media was removed after a short part mux: %v", err)
	}
}

// TestRestartMuxOutOfAChatWaitKeepsTheChatCapture is the close review's
// Important finding end to end: a Twitch VOD whose video finished while its
// chat pager still had pages left sits in Muxing for the length of the O-A
// chat wait (up to 6 h). A daemon restart in that window routes the row
// through MuxJob, which builds a JobContext with NO chat verdict on it — and
// the mux used to write chat_status = "finished" over the truncated capture,
// after which cleanupStagingAfterMux deleted the staging dir with the pager's
// chat.json.resume.json in it. The tail was unrecoverable and unbadged.
//
// The sidecar is the signal: every pager deletes it only on a clean
// completion. With it read as the verdict the row is badged incomplete and
// the existing preserveForChat branch prunes staging to the chat capture.
//
// Mutant: chatFileStatus without the sidecar term — the row reads "finished"
// and the whole staging dir (sidecar included) is gone.
func TestRestartMuxOutOfAChatWaitKeepsTheChatCapture(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)

	staging, _ := muxFixtureJob(t, w, db, "j-chatwait")
	media := filepath.Join(staging, "video.mp4")
	writeMuxFixture(t, ffmpegPath, media, 90)
	chat := filepath.Join(staging, "chat.json")
	if err := os.WriteFile(chat, []byte(`[{"message":"partial capture"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(staging, "chat.json.resume.json")
	if err := os.WriteFile(sidecar, []byte(`{"contentOffsetSeconds":12}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := w.MuxJob("j-chatwait"); err != nil {
		t.Fatalf("MuxJob: %v", err)
	}
	w.Stop()

	fresh, _ := db.GetJob("j-chatwait")
	if fresh == nil || fresh.Status != database.StatusFinished {
		t.Fatalf("job after a restart mux = %v, want Finished (error=%q)", statusOf(fresh), errorOf(fresh))
	}
	if fresh.ChatStatus != chatStatusIncomplete {
		t.Errorf("chat_status = %q after restart-muxing a job whose pager left its resume sidecar, "+
			"want %q — the capture was cut short and the row is the only place that says so",
			fresh.ChatStatus, chatStatusIncomplete)
	}
	if _, err := os.Stat(sidecar); err != nil {
		t.Errorf("the chat resume sidecar was deleted by the restart mux (stat err = %v) — it is "+
			"the only thing a re-run can page on from", err)
	}
	if _, err := os.Stat(chat); err != nil {
		t.Errorf("the partial chat.json was deleted by the restart mux (stat err = %v) — a resumed "+
			"pager APPENDS to it", err)
	}
	// Pruned to the chat capture, not kept whole: the media is already in the
	// archive, so keeping it would cost the archive's size again for a week.
	if _, err := os.Stat(media); !os.IsNotExist(err) {
		t.Errorf("the muxed staging media survived the chat-incomplete keep (stat err = %v) — only "+
			"the chat capture is kept", err)
	}
}

// TestCancelledAsideMuxLeavesNoPartialSibling pins close-review Minor 2. An
// aside mux cut off by a shutdown leaves FFmpeg's partial output beside the
// archive: cleanupFailedMux preserves a partial on ctx cancel (the right call
// for the MAIN mux, where the partial is all the operator has), but an
// aside's partial has no salvage value — the aside itself is still in
// staging, waiting for the next run. Worse, the leftover is INVISIBLE: the
// orphan sweep owns it by stem, so no UI offers it, and asideOutputPath's
// collision counter writes the retry to "-2" rather than over it.
//
// Mutant: dropping os.Remove(out) from the MuxCopy error arm — the moov-less
// sibling is still on disk.
func TestCancelledAsideMuxLeavesNoPartialSibling(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	staging, outputDir := t.TempDir(), t.TempDir()
	aside := filepath.Join(staging, "video.mp4"+engine.StagedRestartSuffix+"1700000000")
	writeSlowAsideFixture(t, ffmpegPath, aside)

	o := NewDownloadOrchestrator(nil, nil, ffmpegPath, discardLogger{}, nil, nil, nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sibling := filepath.Join(outputDir, "Title"+engine.StagedRestartSuffix+"1700000000.mp4")

	done := make(chan struct{})
	go func() {
		defer close(done)
		o.muxStagedAsides(ctx, &JobContext{
			Job: &database.Job{ID: "j-aside-cancel"}, StagingDir: staging,
		}, outputDir, "Title")
	}()

	// Cancel the moment FFmpeg has opened its output — that is the whole
	// window this finding lives in, and the fixture is sized so the copy is
	// still running hundreds of milliseconds later.
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(sibling); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("FFmpeg never created the aside's output file — nothing to cancel mid-copy")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	if _, err := os.Stat(aside); os.IsNotExist(err) {
		t.Fatal("the aside mux ran to completion before the cancellation landed — this test needs " +
			"to cut a copy in flight; the fixture is too small for this machine")
	}
	if _, err := os.Stat(sibling); !os.IsNotExist(err) {
		t.Errorf("a cancelled aside mux left %s behind (stat err = %v) — a moov-less file under the "+
			"archive's own stem that no UI can offer and the next finalize writes '-2' around", sibling, err)
	}
}

// TestStagedRestartAsidesSkipsTheLiveRecording pins the aside-only scan the
// orphan sweep runs (close review Minor 10 / Task 8 B6): jobNeedsStaging and
// scanStagingOrphans ask for asides on every pass and nothing else, so the
// scan must not pay for discoverStagingMedia's Stat of every candidate media
// name — and must not REPORT the live recording, which is ordinary staging
// content, not captured-and-never-muxed footage.
//
// Mutant: dropping this scan's own engine.IsStagedRestartPath filter (the
// shape it would take if it simply reported what ReadDir returned) — the live
// video.mp4 comes back as an aside, and through stagedAsideRecordings that
// shields every finished job's staging dir from the sweep forever. The
// end-to-end twin of that mutant is the control row in
// TestStagedAsideKeepsStagingFromCleanup.
func TestStagedRestartAsidesSkipsTheLiveRecording(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "video.mp4")
	newer := filepath.Join(dir, "video.mp4"+engine.StagedRestartSuffix+"1700000000")
	older := filepath.Join(dir, "video.mp4"+engine.StagedRestartSuffix+"1600000000")
	for _, p := range []string{live, newer, older, newer + ".resume.json"} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	got := stagedRestartAsides(dir)
	want := []string{older, newer}
	if len(got) != len(want) {
		t.Fatalf("stagedRestartAsides = %v, want %v (asides only, oldest first)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stagedRestartAsides = %v, want %v (asides only, oldest first — the live "+
				"recording and the sidecar twin are not asides)", got, want)
		}
	}
	if len(stagedRestartAsides(filepath.Join(dir, "does-not-exist"))) != 0 {
		t.Error("stagedRestartAsides on an unreadable dir returned entries, want none")
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

// TestOffQueueMuxHonoursTheOperatorsCancel: both UIs offer Cancel on a
// Muxing row and the route writes Cancelled, but queue.Cancel only reaches
// jobs the queue dequeued — the off-queue mux (/mux, A M, the boot re-mux)
// ran on regardless, wrote Finished over the Cancelled row and announced
// "Download Finished" after "Job Cancelled". The mux now listens for the
// row's Cancelled itself. Driven through the download-slot wait so the
// cancel lands deterministically before FFmpeg starts.
//
// Mutant: drop the OnJobUpdate listener and the row check from MuxJob —
// once the slot frees, the mux runs and the row reads Finished.
func TestOffQueueMuxHonoursTheOperatorsCancel(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)
	w.SetParallelDownloads(1)

	staging, _ := muxFixtureJob(t, w, db, "j-usercancel")
	writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 90)

	if !w.queue.AcquireDownloadSlot(context.Background(), "holder") {
		t.Fatal("could not take the only download slot")
	}
	if err := w.MuxJob("j-usercancel"); err != nil {
		t.Fatalf("MuxJob: %v", err)
	}
	// What the cancel route writes for a Muxing row the queue does not know.
	db.UpdateJobFields("j-usercancel", map[string]any{"status": database.StatusCancelled})
	w.queue.ReleaseDownloadSlot("holder")
	w.Stop()

	fresh, _ := db.GetJob("j-usercancel")
	if fresh == nil || fresh.Status != database.StatusCancelled {
		t.Fatalf("job after a cancelled off-queue mux = %v, want Cancelled (error=%q)", statusOf(fresh), errorOf(fresh))
	}
	if _, err := os.Stat(filepath.Join(staging, "video.mp4")); err != nil {
		t.Errorf("staging media was removed by a cancelled mux: %v", err)
	}
}

// TestOffQueueMuxCancelRestoresOnlyAStrandedMuxing: an operator's Cancel
// stops the off-queue mux's FFmpeg, and the mux then wrote Cancelled over
// whatever the row held by the time it looked — read, then written
// unconditionally — so the write meant for a row the mux's own Muxing write
// had stranded also turned a Finished archive a racing mux wrote back into
// Cancelled, and an operator's Resume of the cancelled row into a second
// Cancel. The write now applies only to the row it exists for: one still
// Muxing. The stand-in FFmpeg holds the mux open, and the row moves on ahead
// of the mux's own listener, so the mux reads it after the move.
//
// Mutants: write the Cancelled with UpdateJobFields — the Finished archive
// and the resumed row turn Cancelled; with UpdateJobFieldsUnlessTerminal —
// the resumed row does; drop the write — the stranded row is left Muxing
// with nothing running it.
func TestOffQueueMuxCancelRestoresOnlyAStrandedMuxing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in FFmpeg is a shell script")
	}
	for _, tc := range []struct {
		name   string
		landed database.JobStatus // what reaches the row after the route's Cancelled
		want   database.JobStatus
	}{
		{"the mux's own Muxing write", database.StatusMuxing, database.StatusCancelled},
		{"a racing mux's Finished", database.StatusFinished, database.StatusFinished},
		{"the operator's Resume", database.StatusUpcoming, database.StatusUpcoming},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			t.Cleanup(w.Stop)
			const id = "j-cancel-landed"
			staging, _ := muxFixtureJob(t, w, db, id)
			if err := os.WriteFile(filepath.Join(staging, "video.mp4"), []byte("staged"), 0o644); err != nil {
				t.Fatal(err)
			}
			gates := t.TempDir()
			started, release := filepath.Join(gates, "started"), filepath.Join(gates, "release")
			w.orchestrator.SetFfmpegPath(writeBlockingFFmpeg(t, started, release))
			defer os.WriteFile(release, nil, 0o644) // a mux the cancel missed ends anyway

			// Registered ahead of the mux's own listener, so it runs first:
			// the mux hears the Cancelled with the row already moved on. Once
			// only — the mux's own Cancelled must not set it off again.
			var once sync.Once
			unsubscribe := db.OnJobUpdate(func(j *database.Job) {
				if j.ID == id && j.Status == database.StatusCancelled {
					once.Do(func() { db.UpdateJobFields(id, map[string]any{"status": tc.landed}) })
				}
			})
			defer unsubscribe()

			if err := w.MuxJob(id); err != nil {
				t.Fatalf("MuxJob: %v", err)
			}
			waitForFile(t, started)
			db.UpdateJobFields(id, map[string]any{"status": database.StatusCancelled}) // the cancel route's write
			w.wg.Wait()                                                                // the mux has returned

			if row, _ := db.GetJob(id); statusOf(row) != tc.want {
				t.Errorf("status = %s after a cancelled mux found the row %s, want %s", statusOf(row), tc.landed, tc.want)
			}
		})
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

// TestRestartSiblingStem pins the OTHER half of the pair: the muxed sibling
// the worker writes beside a job's archive, where the timestamp is an infix
// before the container extension and may carry the collision counter. The
// orphan sweep keys on this to tell a recovered recording from an ordinary
// unreferenced output file.
//
// Mutants: dropping the counter arm (a `-2` sibling is offered for deletion);
// dropping the digits rule (any file whose name contains ".restart-" is
// treated as owned footage and can never be swept); matching the RAW staging
// name here (the two predicates collapse into one and a staging aside would
// be read as an output sibling).
func TestRestartSiblingStem(t *testing.T) {
	for _, tc := range []struct {
		name     string
		wantStem string
		wantOK   bool
	}{
		{"Stream Title.restart-1700000000.mp4", "Stream Title", true},
		{"Stream Title.restart-1700000000-2.mp4", "Stream Title", true},
		{"a.b.c.restart-1.mkv", "a.b.c", true},
		{"Stream Title.mp4", "", false},
		{"video.mp4.restart-1700000000", "", false}, // the RAW staging aside
		{"Stream Title.restart-.mp4", "", false},
		{"Stream Title.restart-notatimestamp.mp4", "", false},
		{"Stream Title.restart-1700000000-x.mp4", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stem, ok := engine.RestartSiblingStem(tc.name)
			if ok != tc.wantOK || stem != tc.wantStem {
				t.Errorf("RestartSiblingStem(%q) = (%q, %v), want (%q, %v)", tc.name, stem, ok, tc.wantStem, tc.wantOK)
			}
		})
	}
}

// TestStagedRestartAsidesInRecordingOrder pins extra item (d): the engine
// leaves a recording it could not resume beside the fresh one as
// <file>.restart-<ts>, and the asides a staging dir yields come in recording
// order — without the live recording, and with the sidecar twin excluded,
// since muxing a JSON file is not a recovery.
//
// Mutants: ignoring the asides (the set-aside footage is invisible to every
// consumer), or treating the .resume.json twin as one.
func TestStagedRestartAsidesInRecordingOrder(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "video.mp4")
	aside := filepath.Join(dir, "video.mp4"+engine.StagedRestartSuffix+"1700000000")
	older := filepath.Join(dir, "video.mp4"+engine.StagedRestartSuffix+"1600000000")
	for _, p := range []string{live, aside, older, aside + ".resume.json"} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	got := stagedRestartAsides(dir)
	want := []string{older, aside}
	if len(got) != len(want) {
		t.Fatalf("stagedRestartAsides = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stagedRestartAsides = %v, want %v (recording order: oldest first)", got, want)
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

// TestFinalizeMuxesEachAsideToItsOwnFile pins extra item (a): the recovery
// surface an aside never had. Task 8 taught the worker to PRESERVE a set-aside
// recording, which left it pinning its staging dir forever with nothing to do
// about it — the fresh capture restarts at sq=0 and OVERLAPS the aside, so it
// cannot simply be concatenated into the archive. Finalize therefore muxes
// each aside into its OWN file beside the archive: the data is preserved
// exactly once, the staging dir is free to go, and the operator is told the
// name.
//
// Three conditions, one assertion each:
//  1. the sibling exists (mutant: skipping the aside mux — the aside is
//     deleted with staging, which is data loss);
//  2. it is NOT a segment row (mutant: registering it as one — the part list
//     would replay the recording's opening);
//  3. staging goes, because nothing unmuxed is left in it (mutant: sweeping
//     the dir while the aside is still there — the sub-test below).
func TestFinalizeMuxesEachAsideToItsOwnFile(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)

	staging, _ := muxFixtureJob(t, w, db, "j-aside-mux")
	writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 30)
	aside := filepath.Join(staging, "video.mp4"+engine.StagedRestartSuffix+"1700000000")
	writeAsideFixture(t, ffmpegPath, aside, 10)
	twin := aside + ".resume.json"
	if err := os.WriteFile(twin, []byte(`{"stale":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := w.MuxJob("j-aside-mux"); err != nil {
		t.Fatalf("MuxJob: %v", err)
	}
	w.Stop()

	fresh, _ := db.GetJob("j-aside-mux")
	if fresh == nil || fresh.Status != database.StatusFinished {
		t.Fatalf("job after a mux with an aside beside it = %v, want Finished (error=%q)", statusOf(fresh), errorOf(fresh))
	}
	if _, err := os.Stat(fresh.OutputFile); err != nil {
		t.Fatalf("the main output is missing: %v", err)
	}
	sibling := strings.TrimSuffix(fresh.OutputFile, ".mp4") + engine.StagedRestartSuffix + "1700000000.mp4"
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("no %s beside the archive (stat err = %v) — the set-aside recording was never muxed, "+
			"so staging cleanup takes it with the dir", sibling, err)
	}
	// The twin goes with the recording it describes (fix round 1, Minor 3: the
	// removal shipped unpinned and the reviewer's mutant survived the package).
	// Mutant: dropping the engine.StagedRestartSidecar removal — a sidecar
	// outlives its media and a later Start could match its offsets.
	if _, err := os.Stat(twin); !os.IsNotExist(err) {
		t.Errorf("the aside's resume sidecar %s survived its recovery (stat err = %v) — a sidecar "+
			"beside nothing is a stale offset map", twin, err)
	}
	// And the sweep must not offer the sibling: it is the job's own footage,
	// unreferenced by construction (fix round 1, Important 1).
	outCfg := &config.MoomboxConfig{}
	outCfg.Paths.OutputDirectory = filepath.Dir(fresh.OutputFile)
	outEntries, err := scanOutputOrphans(db, outCfg)
	if err != nil {
		t.Fatalf("scanOutputOrphans: %v", err)
	}
	for _, e := range outEntries {
		if normalizePath(e.Path) == normalizePath(sibling) {
			t.Errorf("the recovered sibling %s is offered as a deletable output orphan right after "+
				"finalize — the Files tab's Delete All takes it", sibling)
		}
	}
	if segs, err := db.GetSegments("j-aside-mux"); err != nil {
		t.Fatalf("GetSegments: %v", err)
	} else if len(segs) != 0 {
		t.Errorf("the aside produced %d segment row(s) — it overlaps the opening of the main "+
			"recording, so listing it as a part would replay it", len(segs))
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("staging dir survived a finalize that consumed its aside (stat err = %v)", err)
	}

	t.Run("an aside that cannot be demuxed keeps its own staging shield", func(t *testing.T) {
		w2, db2 := testWorkerSetup(t)
		staging2, _ := muxFixtureJob(t, w2, db2, "j-aside-bad")
		writeMuxFixture(t, ffmpegPath, filepath.Join(staging2, "video.mp4"), 10)
		bad := filepath.Join(staging2, "video.mp4"+engine.StagedRestartSuffix+"1700000001")
		if err := os.WriteFile(bad, []byte("not a container"), 0o644); err != nil {
			t.Fatal(err)
		}

		if err := w2.MuxJob("j-aside-bad"); err != nil {
			t.Fatalf("MuxJob: %v", err)
		}
		w2.Stop()

		after, _ := db2.GetJob("j-aside-bad")
		if after == nil || after.Status != database.StatusFinished {
			t.Fatalf("an unreadable aside must not fail the job: %v (error=%q)", statusOf(after), errorOf(after))
		}
		if _, err := os.Stat(bad); err != nil {
			t.Errorf("the unreadable aside was deleted: %v — a mux that failed must not consume it", err)
		}
		if _, err := os.Stat(staging2); err != nil {
			t.Errorf("staging was swept while an unmuxed aside was still in it: %v", err)
		}
	})
}

// TestMuxStagedAsidesRemovesTheRecordingAndItsTwin pins fix round 1's Minor 3
// where it can actually be seen. The whole-finalize test cannot: a successful
// recovery leaves no aside, so cleanupStagingAfterMux sweeps the entire dir and
// the twin disappears with it whether or not the recovery removed it — which is
// why the reviewer's mutant survived the package. Driven directly, the dir is
// still standing when the assertions run.
//
// Mutant: dropping the engine.StagedRestartSidecar removal — the twin is still
// beside a recording that is gone, and a later Start could match its offsets
// against a fresh file.
func TestMuxStagedAsidesRemovesTheRecordingAndItsTwin(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	staging, outputDir := t.TempDir(), t.TempDir()
	aside := filepath.Join(staging, "video.mp4"+engine.StagedRestartSuffix+"1700000000")
	writeAsideFixture(t, ffmpegPath, aside, 5)
	twin := engine.StagedRestartSidecar(aside)
	if err := os.WriteFile(twin, []byte(`{"stale":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	o := NewDownloadOrchestrator(nil, nil, ffmpegPath, discardLogger{}, nil, nil, nil, nil, nil)
	o.muxStagedAsides(context.Background(), &JobContext{
		Job: &database.Job{ID: "j-twin"}, StagingDir: staging,
	}, outputDir, "Title")

	sibling := filepath.Join(outputDir, "Title"+engine.StagedRestartSuffix+"1700000000.mp4")
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("the aside was not recovered to %s: %v", sibling, err)
	}
	if _, err := os.Stat(aside); !os.IsNotExist(err) {
		t.Errorf("the recovered aside is still in staging (stat err = %v) — it would pin the dir forever", err)
	}
	if _, err := os.Stat(twin); !os.IsNotExist(err) {
		t.Errorf("the aside's resume sidecar %s outlived the recording it describes (stat err = %v)", twin, err)
	}
	if _, err := os.Stat(staging); err != nil {
		t.Errorf("muxStagedAsides removed the staging dir itself: %v — that is the cleanup's job, not this one's", err)
	}
}

// TestAsideOutputPathBoundsItsCollisionCounter pins fix round 1's Minor 4: the
// counter loop's guard let it fall out of the loop having BUILT the next
// candidate without ever testing it, so the last name could be returned over
// an existing file — the one outcome the counter exists to prevent. Past the
// bound there is no name to give, and the caller must be told so rather than
// handed one.
//
// Mutant: returning the final candidate unchecked (the exhaustion arm gets a
// path back and overwrites the file sitting at it).
func TestAsideOutputPathBoundsItsCollisionCounter(t *testing.T) {
	dir := t.TempDir()
	used := map[string]bool{}

	first, ok := asideOutputPath(dir, "Title", "1700000000", used)
	if !ok || first != filepath.Join(dir, "Title"+engine.StagedRestartSuffix+"1700000000.mp4") {
		t.Fatalf("asideOutputPath = (%q, %v), want the plain <stem>.restart-<ts>.mp4", first, ok)
	}
	second, ok := asideOutputPath(dir, "Title", "1700000000", used)
	if !ok || second == first {
		t.Fatalf("a second group with the same stamp got %q (ok=%v) — it would overwrite the first", second, ok)
	}

	// Every name the counter can produce is taken on disk.
	for n := 1; n <= asideOutputCollisionLimit; n++ {
		name := "Title" + engine.StagedRestartSuffix + "1700000000.mp4"
		if n > 1 {
			name = fmt.Sprintf("Title%s1700000000-%d.mp4", engine.StagedRestartSuffix, n)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := asideOutputPath(dir, "Title", "1700000000", map[string]bool{})
	if ok {
		if _, err := os.Stat(got); err == nil {
			t.Errorf("asideOutputPath returned %q, which already exists — the recovery would overwrite it", got)
		}
	}
	if ok || got != "" {
		t.Errorf("asideOutputPath past its bound = (%q, %v), want (\"\", false) so the caller keeps the aside in staging", got, ok)
	}
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

// writeOffsetFragmentFixture renders seconds of video as a FRAGMENTED MP4
// whose first fragment sits at offsetSec on its timeline — the shape of a
// DASH staging file for a part that began after a split or a restart. The
// mov demuxer probes it with the offset folded into its duration
// (start_time=offsetSec, duration=offsetSec+seconds). No extension is
// needed: the format is forced, as it is for a `video_stream`.
//
// frag_discont keeps the first tfdt at the packet's own timestamp instead of
// rebasing the track to zero, and avoid_negative_ts=disabled stops the CLI
// from shifting the offset back out on the way in.
func writeOffsetFragmentFixture(t *testing.T, ffmpegPath, path string, seconds, offsetSec int) {
	t.Helper()
	cmd := exec.Command(ffmpegPath, "-nostdin", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=64x64:rate=5:duration=%d", seconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-output_ts_offset", fmt.Sprint(offsetSec), "-avoid_negative_ts", "disabled",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof+frag_discont",
		"-f", "mp4", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate offset fragment fixture %s: %v\n%s", path, err, out)
	}
}

// writeOffsetTSFixture renders the same seconds of video as MPEG-TS starting
// at offsetSec — a Twitch or YouTube HLS staging file joined mid-broadcast.
// The mpegts demuxer probes it as start_time≈offsetSec, duration=seconds.
func writeOffsetTSFixture(t *testing.T, ffmpegPath, path string, seconds, offsetSec int) {
	t.Helper()
	cmd := exec.Command(ffmpegPath, "-nostdin", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=64x64:rate=5:duration=%d", seconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-output_ts_offset", fmt.Sprint(offsetSec),
		"-f", "mpegts", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate offset mpegts fixture %s: %v\n%s", path, err, out)
	}
}

// writeAsideFixture renders the same fixture under a name with no media
// extension — an aside is <file>.restart-<unix ts>, which FFmpeg cannot pick
// an output format for, so it is written as .mp4 and moved into place the way
// the engine's no-truncate guard renames it.
func writeAsideFixture(t *testing.T, ffmpegPath, path string, seconds int) {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "aside.mp4")
	writeMuxFixture(t, ffmpegPath, tmp, seconds)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("move the aside fixture into place: %v", err)
	}
}

// writeSlowAsideFixture renders an aside big enough that `-c copy` is still
// writing it hundreds of milliseconds after FFmpeg opens its output, which is
// what makes a mid-copy cancellation reachable from a test. PCM audio rather
// than video: 25 minutes of it is ~129 MB written in a fraction of a second,
// where the same size of encoded video would cost minutes to generate.
func writeSlowAsideFixture(t *testing.T, ffmpegPath, path string) {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "aside.mp4")
	cmd := exec.Command(ffmpegPath, "-nostdin", "-y",
		"-f", "lavfi", "-i", "sine=frequency=1000:duration=1500",
		"-c:a", "pcm_s16le", tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate the slow aside fixture: %v\n%s", err, out)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("move the slow aside fixture into place: %v", err)
	}
}

// mp4sIn lists the .mp4 files directly in dir. Used where the assertion is
// "the archive name is free", which must not depend on how the filename
// template resolved.
func mp4sIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read output dir %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".mp4") {
			out = append(out, e.Name())
		}
	}
	return out
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

// TestTruncatedAsideStaysInStaging is ENGINE-9 on the set-aside recovery: an
// aside whose container is cut mid-mdat still probes at its full length,
// `-c copy` stops at the first bad fragment and exits 0 — and muxStagedAsides
// then deleted the aside, the only copy of that footage, behind a sibling
// holding a third of it. A short copy is now discarded and the aside kept.
//
// Mutant: dropping the verifyMuxedDuration check from muxStagedAsides — the
// aside is gone and a short sibling is reported recovered.
func TestTruncatedAsideStaysInStaging(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)

	staging, outputDir := muxFixtureJob(t, w, db, "j-aside-trunc")
	full := filepath.Join(staging, "video.mp4")
	writeMuxFixture(t, ffmpegPath, full, 90)
	truncateFixture(t, full, 120000)
	aside := full + engine.StagedRestartSuffix + "1700000000"
	if err := os.Rename(full, aside); err != nil {
		t.Fatal(err)
	}

	job, _ := db.GetJob("j-aside-trunc")
	recovered := w.orchestrator.muxStagedAsides(context.Background(), w.buildJobContext(job), outputDir, "base")
	if len(recovered) != 0 {
		t.Errorf("a short copy was reported recovered: %v", recovered)
	}
	if _, err := os.Stat(aside); err != nil {
		t.Errorf("the aside — the only copy of its footage — is gone after a short copy: %v", err)
	}
	if left := mp4sIn(t, outputDir); len(left) != 0 {
		t.Errorf("the short sibling was left in the output dir: %v", left)
	}
}
