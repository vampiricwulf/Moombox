package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
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

// TestACancelWhileFFmpegRunsStaysCancelled: the other side of R-H2. A Cancel
// that reaches the mux while it runs stops it — here from the "muxing"
// announcement, ahead of the copy — so no archive is completed, and the
// Cancel stands: the row stays Cancelled, nothing is at the output path, the
// staging is kept for a later Mux.
//
// Mutant: the finalize going on after a failed copy — the row ends Finished.
func TestACancelWhileFFmpegRunsStaysCancelled(t *testing.T) {
	w, db := testWorkerSetup(t)
	staging, outputDir := muxFixtureJob(t, w, db, "j-during")
	ffmpegPath, _ := requireFFmpegTools(t)
	writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 3)
	db.UpdateJobFields("j-during", map[string]any{"status": database.StatusError})

	var cancelled bool
	w.orchestrator.logger = &onInfoLogger{prefix: "muxing", hook: func() {
		cancelled, _, _ = w.CancelJob("j-during")
	}}
	if err := w.MuxJob("j-during"); err != nil {
		t.Fatal(err)
	}
	w.wg.Wait()

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
