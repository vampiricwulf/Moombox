package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

// infoLines records the Info lines whose message starts with prefix.
type infoLines struct {
	discardLogger
	prefix string
	mu     sync.Mutex
	n      int
}

func (l *infoLines) Info(msg string, _ ...any) {
	if strings.HasPrefix(msg, l.prefix) {
		l.mu.Lock()
		l.n++
		l.mu.Unlock()
	}
}

func (l *infoLines) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

// finishedWrites counts the job updates that carry the row to Finished.
func finishedWrites(db *database.Database, id string) *atomic.Int32 {
	var n atomic.Int32
	last := database.JobStatus("")
	var mu sync.Mutex
	db.OnJobUpdate(func(j *database.Job) {
		if j.ID != id {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if j.Status == database.StatusFinished && last != database.StatusFinished {
			n.Add(1)
		}
		last = j.Status
	})
	return &n
}

// TestACancelBetweenTheMuxAndItsMarkEndsFinished is owner ruling R-H2. A
// Cancel that lands after FFmpeg has written the archive and before the
// finalize marks itself finishing was acknowledged, and until this ruling it
// stood: the row stayed Cancelled, and the complete archive it had just
// written was left at the output path on no row. The mux completed, so
// Finished wins: the mark re-claims the Cancelled row (conditionally — only
// a row still Cancelled), the finalize records the archive as it records any
// other, exactly once, and the staging is reclaimed. The Cancel had already
// answered success; the row's flip to Finished is the ordinary job update,
// and one Info line names it.
//
// Mutants: beginFinishing refusing a Cancelled row as before — the row stays
// Cancelled with no output_file; the reclaim written unconditionally — see
// TestAResetInTheFinishWindowIsNotOverwritten.
func TestACancelBetweenTheMuxAndItsMarkEndsFinished(t *testing.T) {
	w, db := testWorkerSetup(t)
	staging, _ := muxFixtureJob(t, w, db, "j-window")
	ffmpegPath, _ := requireFFmpegTools(t)
	writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 3)
	db.UpdateJobFields("j-window", map[string]any{"status": database.StatusError})
	log := &infoLines{prefix: "a Cancel landed after the mux completed"}
	w.logger = log
	finished := finishedWrites(db, "j-window")

	mark := w.orchestrator.beginFinishing
	var cancelled bool
	var cancelErr error
	w.orchestrator.beginFinishing = func(id string, muxed bool) (func(), bool, bool) {
		cancelled, _, cancelErr = w.CancelJob(id)
		return mark(id, muxed)
	}
	if err := w.MuxJob("j-window"); err != nil {
		t.Fatal(err)
	}
	w.wg.Wait()

	if !cancelled || cancelErr != nil {
		t.Fatalf("the Cancel ahead of the mark = %v, %v; want it applied", cancelled, cancelErr)
	}
	j, _ := db.GetJob("j-window")
	if j.Status != database.StatusFinished || j.OutputFile == "" {
		t.Fatalf("row = %s (output %q), want Finished with its archive", j.Status, j.OutputFile)
	}
	if _, err := os.Stat(j.OutputFile); err != nil {
		t.Errorf("the row's archive: %v", err)
	}
	if n := finished.Load(); n != 1 {
		t.Errorf("the row was carried to Finished %d times, want once", n)
	}
	if HasSegmentFiles(filepath.Dir(staging), "j-window") {
		t.Error("staging was kept under the Finished row: the finish did not run its cleanup")
	}
	if n := log.count(); n != 1 {
		t.Errorf("%d Info lines named the Cancel the finish superseded, want 1", n)
	}
	if _, held := w.StagingClaimHolder("j-window"); held {
		t.Error("the staging claim outlived the Mux")
	}
	// The mark is gone with the finalize.
	if c, _, err := w.CancelJob("j-window"); c || err != nil {
		t.Errorf("Cancel of the Finished row = %v, %v; want not cancelled, no error", c, err)
	}
}

// onDebugLogger runs hook once, on the first Debug line whose message is
// msg.
type onDebugLogger struct {
	discardLogger
	msg  string
	once sync.Once
	hook func()
}

func (l *onDebugLogger) Debug(msg string, _ ...any) {
	if msg == l.msg {
		l.once.Do(l.hook)
	}
}

// TestACancelWhileFFmpegRunsStaysCancelled: the other side of R-H2. A Cancel
// that reaches the mux inside the copy — here as the muxer hands FFmpeg its
// arguments, after the finalize's Muxing write and its staged-input walk —
// stops it through MuxJob's own listener (the queue does not know an
// off-queue Mux), so no archive is completed, and the Cancel stands: the row
// stays Cancelled, nothing is at the output path, the staging is kept for a
// later Mux.
//
// Mutants: MuxJob's cancel listener not cancelling the mux — FFmpeg runs to
// its end, the mark re-claims the row and it ends Finished; the finalize
// going on after a failed copy — the same.
func TestACancelWhileFFmpegRunsStaysCancelled(t *testing.T) {
	w, db := testWorkerSetup(t)
	staging, outputDir := muxFixtureJob(t, w, db, "j-during")
	ffmpegPath, _ := requireFFmpegTools(t)
	writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 3)
	db.UpdateJobFields("j-during", map[string]any{"status": database.StatusError})

	var cancelled bool
	var before database.JobStatus
	w.orchestrator.muxer = engine.NewMuxer(ffmpegPath, &onDebugLogger{msg: "ffmpeg", hook: func() {
		if j, _ := db.GetJob("j-during"); j != nil {
			before = j.Status
		}
		cancelled, _, _ = w.CancelJob("j-during")
	}})
	if err := w.MuxJob("j-during"); err != nil {
		t.Fatal(err)
	}
	w.wg.Wait()

	if before != database.StatusMuxing {
		t.Fatalf("the row read %q as FFmpeg was started, want Muxing: the Cancel did not land inside the copy", before)
	}
	if !cancelled {
		t.Fatal("the Cancel during the mux was refused")
	}
	j, _ := db.GetJob("j-during")
	if j.Status != database.StatusCancelled || j.OutputFile != "" {
		t.Errorf("row = %s (output %q), want the Cancel standing with no archive", j.Status, j.OutputFile)
	}
	if !HasSegmentFiles(filepath.Dir(staging), "j-during") {
		t.Error("staging was reclaimed under a Cancelled row")
	}
	if mp4s, _ := filepath.Glob(filepath.Join(outputDir, "*.mp4")); len(mp4s) != 0 {
		t.Errorf("an archive was written for the cancelled mux: %v", mp4s)
	}
}

// TestACancelDuringTheOutputCheckStaysCancelled: the window R-H2 re-claims
// opens only once the copy is VERIFIED. Between FFmpeg's return and the mark
// the muxer still runs ENGINE-9's check on the copy (verifiedCopy: ffprobe
// of it and of the staged inputs), on the finalize's context. A Cancel that
// lands there ends that context, so the probes fail — and a copy ffprobe
// could not read used to pass: it was renamed into place, the mark re-claimed
// the Cancelled row, the row was written Finished and its staging deleted,
// with nothing having compared the copy to what was staged. The check now
// fails a copy whose context ended under it, so the Cancel stands: Cancelled,
// nothing at the output path, staging kept.
//
// Mutant: verifiedCopy passing a copy whose context ended — the row ends
// Finished and its staging is reclaimed.
func TestACancelDuringTheOutputCheckStaysCancelled(t *testing.T) {
	w, db := testWorkerSetup(t)
	staging, outputDir := muxFixtureJob(t, w, db, "j-check")
	ffmpegPath, _ := requireFFmpegTools(t)
	writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 3)
	db.UpdateJobFields("j-check", map[string]any{"status": database.StatusError})

	var cancelled bool
	w.orchestrator.logger = &onDebugLogger{msg: "checking the muxed copy against its staged inputs", hook: func() {
		cancelled, _, _ = w.CancelJob("j-check")
	}}
	if err := w.MuxJob("j-check"); err != nil {
		t.Fatal(err)
	}
	w.wg.Wait()

	if !cancelled {
		t.Fatal("the Cancel during the output check was refused (or the check never ran)")
	}
	j, _ := db.GetJob("j-check")
	if j.Status != database.StatusCancelled || j.OutputFile != "" {
		t.Errorf("row = %s (output %q), want the Cancel standing with no archive", j.Status, j.OutputFile)
	}
	if !HasSegmentFiles(filepath.Dir(staging), "j-check") {
		t.Error("staging was reclaimed though the copy was never checked against it")
	}
	if mp4s, _ := filepath.Glob(filepath.Join(outputDir, "*.mp4")); len(mp4s) != 0 {
		t.Errorf("an unchecked copy took the archive's name: %v", mp4s)
	}
}

// TestAResetInTheFinishWindowIsNotOverwritten: the re-claim is conditional on
// the row still reading Cancelled, and the mark is taken only on a row the
// finalize itself could hold. A verb that moved the row on in the window —
// here a Reinitialize's reset write, made the way reinitializeNow makes it,
// right after the Cancel — keeps its row: the finalize stops with nothing
// recorded, and Finished is not written over the reset.
//
// Mutants: the mark taken on any row short of an outcome, as it was — the
// reset Upcoming row is written Finished; the reclaim written with
// UpdateJobFields — the same.
func TestAResetInTheFinishWindowIsNotOverwritten(t *testing.T) {
	w, db := testWorkerSetup(t)
	staging, _ := muxFixtureJob(t, w, db, "j-reset")
	ffmpegPath, _ := requireFFmpegTools(t)
	writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 3)
	db.UpdateJobFields("j-reset", map[string]any{"status": database.StatusError})

	mark := w.orchestrator.beginFinishing
	w.orchestrator.beginFinishing = func(id string, muxed bool) (func(), bool, bool) {
		if c, _, err := w.CancelJob(id); !c || err != nil {
			t.Errorf("the Cancel ahead of the mark = %v, %v", c, err)
		}
		if !db.UpdateJobFieldsIf(id, database.StatusCancelled, reinitResetFields(0)) {
			t.Error("the reset's claim of the Cancelled row did not apply")
		}
		return mark(id, muxed)
	}
	if err := w.MuxJob("j-reset"); err != nil {
		t.Fatal(err)
	}
	w.wg.Wait()

	j, _ := db.GetJob("j-reset")
	if j.Status != database.StatusUpcoming || j.OutputFile != "" {
		t.Errorf("row = %s (output %q), want the reset's Upcoming standing", j.Status, j.OutputFile)
	}
}

// TestACancelBetweenASplitFinalizesMuxAndItsMarkEndsFinished is R-H2 in the
// split shape: the parts are muxed and on their rows, the finalize's own mux
// work returned with its context alive, and a Cancel acknowledged before the
// mark is re-claimed — the job ends Finished naming its parts.
//
// Mutant: finalizeMultiSegmentJob reporting its mux work incomplete whatever
// the context read — the row stays Cancelled.
func TestACancelBetweenASplitFinalizesMuxAndItsMarkEndsFinished(t *testing.T) {
	w, db := testWorkerSetup(t)
	jobCtx, segments := splitFinalizeFixture(t, w, db, "j-split-window")
	// The finalize's tail runs on the mux root once it has re-claimed the
	// row; cut, so copyAssets' thumbnail fallback makes no network call.
	w.orchestrator.muxRootCancel()
	mark := w.orchestrator.beginFinishing
	var cancelled bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.orchestrator.beginFinishing = func(id string, muxed bool) (func(), bool, bool) {
		cancelled, _, _ = w.CancelJob(id)
		cancel() // as the Cancel's listener cancels the run
		return mark(id, muxed)
	}
	if err := w.orchestrator.finalizeMultiSegmentJob(ctx, jobCtx, segments, nil); err != nil {
		t.Fatalf("finalizeMultiSegmentJob = %v, want the finish", err)
	}
	if !cancelled {
		t.Fatal("the Cancel ahead of the mark was refused")
	}
	j, _ := db.GetJob("j-split-window")
	if j.Status != database.StatusFinished || j.OutputFile != segments[0].FilePath {
		t.Errorf("row = %s (output %q), want Finished naming its first part", j.Status, j.OutputFile)
	}
}

// TestACancelDuringASplitFinalizesMuxStands: the split finalize's mux work —
// the same-format merge, the set-aside recovery — swallows its failures, so
// a Cancel that stopped it leaves no error to stop on. What says so is the
// finalize's context, read when that work returns: cancelled, the work may
// have been cut short, and the Cancel stands — the finalize stops with
// ErrCancelled, the row stays Cancelled, the parts stay on their rows and on
// disk.
//
// Mutant: the split mark re-claiming whatever its context read — the row
// ends Finished over a Cancel that stopped its mux.
func TestACancelDuringASplitFinalizesMuxStands(t *testing.T) {
	w, db := testWorkerSetup(t)
	jobCtx, segments := splitFinalizeFixture(t, w, db, "j-split-during")
	ctx, cancel := context.WithCancel(context.Background())
	if c, _, err := w.CancelJob("j-split-during"); !c || err != nil {
		t.Fatalf("Cancel of the Muxing row = %v, %v", c, err)
	}
	cancel() // the Cancel's listener, heard while the mux work ran
	if err := w.orchestrator.finalizeMultiSegmentJob(ctx, jobCtx, segments, nil); !errors.Is(err, ErrCancelled) {
		t.Fatalf("finalizeMultiSegmentJob = %v, want ErrCancelled", err)
	}
	if j, _ := db.GetJob("j-split-during"); j.Status != database.StatusCancelled {
		t.Errorf("row = %s, want the Cancel standing", j.Status)
	}
	if segs, _ := db.GetSegments("j-split-during"); len(segs) != len(segments) {
		t.Errorf("%d part rows left, want %d", len(segs), len(segments))
	}
	for _, s := range segments {
		if _, err := os.Stat(s.FilePath); err != nil {
			t.Errorf("part %s: %v", s.Filename, err)
		}
	}
}

// TestASupersededCancelOfAnOffQueueMuxIsNotAnnounced: a Mux action, a boot
// re-mux and the automatic Twitch mux run off the queue, which therefore
// flags nothing for a Cancel of one, and the Cancel's caller (the cancel
// route, the TUI) sent "Job Cancelled" itself. When the finishing mark then
// re-claimed the row (R-H2) a "Download Finished" followed it — two terminal
// notifications, and in webhook edit mode a second message. A Cancel applied
// while an off-queue mux runs is the mux's to announce now (flagged true, as
// for a queue run), and it announces it at its exit only if the row still
// reads Cancelled: superseded, the one notification is "Download Finished".
//
// Mutant: CancelJob not handing the announcement to the mux run — flagged
// is false and the caller sends "Job Cancelled" ahead of the finish.
func TestASupersededCancelOfAnOffQueueMuxIsNotAnnounced(t *testing.T) {
	w, db := testWorkerSetup(t)
	staging, _ := muxFixtureJob(t, w, db, "j-notify")
	ffmpegPath, _ := requireFFmpegTools(t)
	writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 3)
	db.UpdateJobFields("j-notify", map[string]any{"status": database.StatusError})
	rec := notificationtest.New()
	w.notifier = rec
	w.orchestrator.notifier = rec

	mark := w.orchestrator.beginFinishing
	var cancelled, flagged bool
	w.orchestrator.beginFinishing = func(id string, muxed bool) (func(), bool, bool) {
		cancelled, flagged, _ = w.CancelJob(id)
		return mark(id, muxed)
	}
	if err := w.MuxJob("j-notify"); err != nil {
		t.Fatal(err)
	}
	w.wg.Wait()

	if !cancelled {
		t.Fatal("the Cancel ahead of the mark was refused")
	}
	if !flagged {
		t.Error("the Cancel of a running off-queue mux left its announcement to the caller, who sends it ahead of the finish")
	}
	if j, _ := db.GetJob("j-notify"); j.Status != database.StatusFinished {
		t.Fatalf("row = %s, want Finished", j.Status)
	}
	if n := len(rec.ByEvent("cancelled")); n != 0 {
		t.Errorf("%d Job Cancelled sent for a Cancel the finish superseded, want 0", n)
	}
	if n := len(rec.ByEvent("finished")); n != 1 {
		t.Errorf("%d Download Finished sent, want 1", n)
	}
	// With the mux gone, a Cancel is its caller's to announce again.
	db.UpdateJobFields("j-notify", map[string]any{"status": database.StatusUpcoming})
	if c, f, err := w.CancelJob("j-notify"); !c || f || err != nil {
		t.Errorf("Cancel after the mux = cancelled %v, flagged %v, %v; want cancelled, not flagged", c, f, err)
	}
}

// TestACancelThatStopsAnOffQueueMuxIsAnnouncedByTheMux: the other half. The
// Cancel stopped FFmpeg, the row stays Cancelled, and the mux run, which
// took the announcement over, sends the one "Job Cancelled" at its exit.
//
// Mutant: the mux run's exit not sending what it took over — no Job
// Cancelled at all.
func TestACancelThatStopsAnOffQueueMuxIsAnnouncedByTheMux(t *testing.T) {
	w, db := testWorkerSetup(t)
	staging, _ := muxFixtureJob(t, w, db, "j-notify-stop")
	ffmpegPath, _ := requireFFmpegTools(t)
	writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 3)
	db.UpdateJobFields("j-notify-stop", map[string]any{"status": database.StatusError})
	rec := notificationtest.New()
	w.notifier = rec
	w.orchestrator.notifier = rec

	var cancelled, flagged bool
	w.orchestrator.muxer = engine.NewMuxer(ffmpegPath, &onDebugLogger{msg: "ffmpeg", hook: func() {
		cancelled, flagged, _ = w.CancelJob("j-notify-stop")
	}})
	if err := w.MuxJob("j-notify-stop"); err != nil {
		t.Fatal(err)
	}
	w.wg.Wait()

	if !cancelled || !flagged {
		t.Fatalf("Cancel during the mux = cancelled %v, flagged %v; want both", cancelled, flagged)
	}
	if j, _ := db.GetJob("j-notify-stop"); j.Status != database.StatusCancelled {
		t.Fatalf("row = %s, want Cancelled", j.Status)
	}
	if n := len(rec.ByEvent("cancelled")); n != 1 {
		t.Errorf("%d Job Cancelled sent, want the mux run's one", n)
	}
	if n := len(rec.ByEvent("finished")); n != 0 {
		t.Errorf("%d Download Finished sent for a cancelled mux, want 0", n)
	}
}

// TestAReclaimedFinishStillRunsTheTrimTheJobAskedFor: the post-download trim
// (StartTime/EndTime) runs at the end of ExecuteWithChat, on the run's
// context — which the superseded Cancel of a re-claimed finish (R-H2) had
// ended, so the row ended Finished with no trim and no "Trim Failed" either.
// A finalize that re-claimed its row says so on the JobContext, and the trim
// then runs on the mux root. (No trim service here, so the attempt shows as
// the "Trim Failed" postDownloadTrim sends for one.)
//
// Mutants: trimAfterFinalize reading the run's context whatever the finalize
// re-claimed — no trim is attempted; muxAndFinalize not recording the
// re-claim — the second half fails.
func TestAReclaimedFinishStillRunsTheTrimTheJobAskedFor(t *testing.T) {
	start := 1.0
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name      string
		reclaimed bool
		want      int
	}{
		{"a cancelled run's trim is skipped", false, 0},
		{"a re-claimed finish's trim runs", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := notificationtest.New()
			w, db := testWorkerSetup(t)
			o := w.orchestrator
			o.notifier, o.trims = rec, nil
			job := &database.Job{ID: "j-trim", VideoID: "j-trim", URL: "u", Platform: "youtube", Title: "T", Status: database.StatusFinished, StartTime: &start}
			if _, err := db.AddJob(job); err != nil {
				t.Fatal(err)
			}
			jobCtx := &JobContext{Job: job, FinishReclaimed: tc.reclaimed}
			o.trimAfterFinalize(ended, jobCtx)
			if n := len(rec.Calls()); n != tc.want {
				t.Errorf("%d trim attempts reported, want %d", n, tc.want)
			}
		})
	}

	// And the single-file finalize records its re-claim.
	w, db := testWorkerSetup(t)
	staging, _ := muxFixtureJob(t, w, db, "j-trim-mark")
	ffmpegPath, _ := requireFFmpegTools(t)
	writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 3)
	job, _ := db.GetJob("j-trim-mark")
	jobCtx := w.buildJobContext(job)
	mark := w.orchestrator.beginFinishing
	w.orchestrator.beginFinishing = func(id string, muxed bool) (func(), bool, bool) {
		if c, _, err := w.CancelJob(id); !c || err != nil {
			t.Errorf("the Cancel ahead of the mark = %v, %v", c, err)
		}
		return mark(id, muxed)
	}
	if err := w.orchestrator.muxAndFinalize(context.Background(), jobCtx, &DownloadResult{HasVideo: true, VideoPath: filepath.Join(staging, "video.mp4")}); err != nil {
		t.Fatal(err)
	}
	if !jobCtx.FinishReclaimed {
		t.Error("the finalize re-claimed its row and did not say so: the run's trim would be skipped")
	}
}

// thumbnailStub points copyAssets' YouTube thumbnail fetch at a server that
// answers every size with a JPEG-sized body, and counts the requests it got.
func thumbnailStub(t *testing.T) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = rw.Write(make([]byte, 2048))
	}))
	orig := ytThumbnailBase
	ytThumbnailBase = srv.URL
	t.Cleanup(func() {
		ytThumbnailBase = orig
		srv.Close()
	})
	return &hits
}

// TestAReclaimedFinishRunsItsTailOnALiveContext: the Cancel a re-claimed
// finish superseded (R-H2) cancelled the finalize's own context. What the
// finalize still does after the mark — the join offset's probe, the assets —
// runs on the mux root instead; left on the cancelled context, the Finished
// row lost its thumbnail (the fetch fails at once) and its join offset.
// Pinned on the thumbnail, the one asset fetched over the context.
//
// Mutant: muxAndFinalize keeping its own context after a re-claim — the
// Finished row has no thumbnail_file, and the stub is never asked.
func TestAReclaimedFinishRunsItsTailOnALiveContext(t *testing.T) {
	hits := thumbnailStub(t)
	w, db := testWorkerSetup(t)
	staging, _ := muxFixtureJob(t, w, db, "j-tail")
	ffmpegPath, _ := requireFFmpegTools(t)
	writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 3)
	db.UpdateJobFields("j-tail", map[string]any{"status": database.StatusError})

	mark := w.orchestrator.beginFinishing
	var cancelled bool
	w.orchestrator.beginFinishing = func(id string, muxed bool) (func(), bool, bool) {
		cancelled, _, _ = w.CancelJob(id) // its row write cancels the mux's context
		return mark(id, muxed)
	}
	if err := w.MuxJob("j-tail"); err != nil {
		t.Fatal(err)
	}
	w.wg.Wait()

	if !cancelled {
		t.Fatal("the Cancel ahead of the mark was refused")
	}
	j, _ := db.GetJob("j-tail")
	if j.Status != database.StatusFinished {
		t.Fatalf("row = %s, want Finished", j.Status)
	}
	if j.ThumbnailFile == "" {
		t.Errorf("the re-claimed finish has no thumbnail (%d fetches reached the stub): its tail ran on the context the Cancel ended", hits.Load())
	} else if _, err := os.Stat(j.ThumbnailFile); err != nil {
		t.Errorf("the row's thumbnail: %v", err)
	}
}

// TestAReclaimedSplitFinishRunsItsTailOnALiveContext: the same, in the split
// shape.
//
// Mutant: finalizeMultiSegmentJob keeping its own context after a re-claim.
func TestAReclaimedSplitFinishRunsItsTailOnALiveContext(t *testing.T) {
	hits := thumbnailStub(t)
	w, db := testWorkerSetup(t)
	jobCtx, segments := splitFinalizeFixture(t, w, db, "j-split-tail")
	mark := w.orchestrator.beginFinishing
	var cancelled bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.orchestrator.beginFinishing = func(id string, muxed bool) (func(), bool, bool) {
		cancelled, _, _ = w.CancelJob(id)
		cancel() // as the Cancel's listener cancels the run
		return mark(id, muxed)
	}
	if err := w.orchestrator.finalizeMultiSegmentJob(ctx, jobCtx, segments, nil); err != nil {
		t.Fatalf("finalizeMultiSegmentJob = %v, want the finish", err)
	}
	if !cancelled {
		t.Fatal("the Cancel ahead of the mark was refused")
	}
	j, _ := db.GetJob("j-split-tail")
	if j.Status != database.StatusFinished {
		t.Fatalf("row = %s, want Finished", j.Status)
	}
	if j.ThumbnailFile == "" {
		t.Errorf("the re-claimed split finish has no thumbnail (%d fetches reached the stub): its tail ran on the context the Cancel ended", hits.Load())
	}
}

// TestACancelInAQueueRunsFinishWindowEndsFinished is R-H2 on the path most
// captures take: a queue run (processJob) whose download ended and whose mux
// completed, cancelled between the mux's return and the mark. CancelJob
// flags the run and cancels its context; the mark re-claims the row, the
// finalize returns nil, and the run takes its success tail — one "Download
// Finished", no "Job Cancelled" (handleCancellation is not reached), and the
// staging reclaimed as after any finish.
//
// Mutants: processJob reading its context ahead of the orchestrator's nil
// (handleCancellation runs: a "Job Cancelled" beside the finish, the staging
// kept); the re-claim refused — the row stays Cancelled.
func TestACancelInAQueueRunsFinishWindowEndsFinished(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)
	w.orchestrator.SetFfmpegPath(ffmpegPath)
	w.orchestrator.routedCipher = stubCipherSolver{}
	rec := notificationtest.New()
	w.notifier = rec
	w.orchestrator.notifier = rec
	const id = "j-queue-window"
	diskFullVodJob(t, db, id, 0)
	finished := finishedWrites(db, id)

	srv, size := serveMuxFixture(t, ffmpegPath, 2)
	w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
		return &StreamProcessResult{ShouldDownload: true, IsVod: true,
			VideoInfo: vodInfoAt(srv.URL, time.Now().Add(6*time.Hour).Unix(), size)}, nil
	}
	mark := w.orchestrator.beginFinishing
	var cancelled, flagged bool
	var cancelErr error
	w.orchestrator.beginFinishing = func(jobID string, muxed bool) (func(), bool, bool) {
		cancelled, flagged, cancelErr = w.CancelJob(jobID)
		return mark(jobID, muxed)
	}

	w.queue.Enqueue(id, database.StatusUpcoming)
	_, ctx, ok := w.queue.Dequeue(context.Background())
	if !ok {
		t.Fatal("Dequeue returned no job")
	}
	w.processJob(ctx, id)

	if !cancelled || !flagged || cancelErr != nil {
		t.Fatalf("the Cancel ahead of the mark = cancelled %v, flagged %v, %v; want it applied and left to the run", cancelled, flagged, cancelErr)
	}
	if ctx.Err() == nil {
		t.Fatal("the Cancel did not cancel the run's context: the test is not in the window")
	}
	j, _ := db.GetJob(id)
	if j.Status != database.StatusFinished || j.OutputFile == "" {
		t.Fatalf("row = %s %q (output %q), want Finished with its archive", j.Status, j.Error, j.OutputFile)
	}
	if _, err := os.Stat(j.OutputFile); err != nil {
		t.Errorf("the row's archive: %v", err)
	}
	if n := finished.Load(); n != 1 {
		t.Errorf("the row was carried to Finished %d times, want once", n)
	}
	if n := len(rec.ByEvent("finished")); n != 1 {
		t.Errorf("%d Download Finished sent, want 1", n)
	}
	if n := len(rec.ByEvent("cancelled")); n != 0 {
		t.Errorf("%d Job Cancelled sent for a Cancel the finish superseded, want 0", n)
	}
	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
	if _, err := os.Stat(filepath.Join(stagingBase, id)); !os.IsNotExist(err) {
		t.Errorf("staging was kept under the Finished row (stat: %v): the run did not take its success tail", err)
	}
}

// TestAPanicInAReclaimedFinishesTailEndsInError: a queue run whose finish
// re-claimed its row (R-H2) is still flagged by the Cancel the mark
// superseded. A panic in what is left of the finalize — between the mark and
// the Finished write — was recorded as that cancel's end: the Cancelled
// write was refused (the row reads Muxing), "Job Cancelled" was sent, and the
// row stayed Muxing with nothing running until a restart. A flagged run's
// row reads Muxing only through that re-claim (CancelJob wrote Cancelled
// ahead of the flag, and a Mux is refused while the run is registered), so
// it is recorded as any other panicked finalize: Error, which Mux recovers.
//
// Mutant: recordRunPanic handing every flagged, non-terminal row to
// handleCancellation — the row stays Muxing and Job Cancelled is sent.
func TestAPanicInAReclaimedFinishesTailEndsInError(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	rec := notificationtest.New()
	w.notifier = rec
	job := runningJob(t, w, db, "reclaim_panic")
	if c, flagged, err := w.CancelJob(job.ID); !c || !flagged || err != nil {
		t.Fatalf("CancelJob = %v, %v, %v; want the run cancelled and flagged", c, flagged, err)
	}
	release, reclaimed, ok := w.beginFinishing(job.ID, true)
	if !ok || !reclaimed {
		t.Fatalf("the mark = reclaimed %v, ok %v; want the Cancelled row re-claimed", reclaimed, ok)
	}
	release() // the finalize's deferred release, as the panic unwinds it

	w.recordRunPanic(job.ID, job, "boom")

	row, _ := db.GetJob(job.ID)
	if row.Status != database.StatusError || !strings.Contains(row.Error, "internal panic") {
		t.Errorf("row = %s %q, want Error naming the panic", row.Status, row.Error)
	}
	if n := len(rec.ByEvent("cancelled")); n != 0 {
		t.Errorf("%d Job Cancelled sent for a job that is not cancelled", n)
	}
}

// TestAMuxRunAnnouncesItsCancelWhenTheRowCannotBeRead: CancelJob told its
// caller the off-queue mux would announce the Cancel (flagged), so nobody
// else sends "Job Cancelled". When the row cannot be read at the mux run's
// exit, the run goes by what it knows: a mux that did not finish left the
// Cancel standing, and it is announced from the row the Mux read at its
// start; one that finished announced itself ("Download Finished"), and a
// Cancel nobody noted is not the run's.
//
// Mutant: endMuxRun returning on a read error, as it did — no Job Cancelled.
func TestAMuxRunAnnouncesItsCancelWhenTheRowCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name     string
		noted    bool
		finished bool
		want     int
	}{
		{"a cancelled mux", true, false, 1},
		{"a mux that finished", true, true, 0},
		{"no Cancel noted", false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			rec := notificationtest.New()
			w.notifier = rec
			prior := &database.Job{ID: "j-unread", VideoID: "j-unread", URL: "u", Platform: "youtube", Title: "T", Status: database.StatusCancelled}
			if _, err := db.AddJob(prior); err != nil {
				t.Fatal(err)
			}
			w.beginMuxRun(prior.ID)
			if tc.noted && !w.noteMuxRunCancel(prior.ID) {
				t.Fatal("the Cancel was not noted on the running mux")
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := db.GetJob(prior.ID); err == nil {
				t.Fatal("the closed database still reads: the test is not exercising the read error")
			}
			w.endMuxRun(prior.ID, prior, tc.finished)
			if n := len(rec.ByEvent("cancelled")); n != tc.want {
				t.Errorf("%d Job Cancelled sent, want %d", n, tc.want)
			}
		})
	}
}
