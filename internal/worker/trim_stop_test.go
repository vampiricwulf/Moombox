package worker

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestDeletingTheJobStopsItsTrim: a trim is of a Finished job, and no delete
// path cancels anything for a Finished row, so deleting the job mid-trim let
// FFmpeg encode to the end; the record then failed on the trims foreign key,
// the finished clip was removed, and Trim Failed went out for a job that no
// longer exists. The service now cancels the trim when its job is deleted:
// FFmpeg is killed at once, the partial file goes, and nothing is notified.
//
// Mutant: drop run's OnJobDeleted subscription — the gated encode is never
// stopped and no outcome arrives (the stand-in gives up after 30 s and fails,
// which sends trim_error).
func TestDeletingTheJobStopsItsTrim(t *testing.T) {
	ffmpeg, _ := gatedFFmpeg(t) // the gate never opens
	r := newTrimRig(t, ffmpeg)

	if _, err := r.ts.StartTrim(r.job, 60, 300); err != nil {
		t.Fatalf("StartTrim: %v", err)
	}
	r.events.wait(t, TrimStateRunning)
	waitForCond(t, 10*time.Second, "the encode to open its output", func() bool { return len(r.files(t)) > 0 })

	if err := r.db.DeleteJob(r.job.ID); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	ev := r.events.wait(t, TrimStateFailed)
	if ev.Error != trimJobDeletedReason || ev.StopReason != TrimStopJobDeleted {
		t.Errorf("the deleted job's trim event says %q (stopReason %q), want %q (%q)", ev.Error, ev.StopReason, trimJobDeletedReason, TrimStopJobDeleted)
	}
	if got := r.files(t); len(got) != 0 {
		t.Errorf("the deleted job's trim left %v in trim/", got)
	}
	if n := len(r.rec.ByEvent("trim_error")); n != 0 {
		t.Errorf("a trim whose job was deleted sent trim_error %d times", n)
	}
	if rs := r.ts.RunningTrims(); len(rs) != 0 {
		t.Errorf("RunningTrims = %+v after the job was deleted", rs)
	}
}

// TestTrimWhoseJobVanishedBeforeItsRecordSendsNothing: the row can go
// between the encode's end and the record — after the subscription could
// cancel anything. AddTrim then fails on the foreign key, and that is no
// failure of the trim's: no trim_error, the placed clip removed. The row is
// deleted through a second handle on the database here, whose delete the
// service's subscription never hears, to stand for that window.
//
// Mutant: drop the jobGone case in run's outcome — trim_error is sent.
func TestTrimWhoseJobVanishedBeforeItsRecordSendsNothing(t *testing.T) {
	ffmpeg, gate := gatedFFmpeg(t)
	r := newTrimRig(t, ffmpeg)
	other, err := database.Open(filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(r.trims))), "trim.db"))
	if err != nil {
		t.Fatalf("second database handle: %v", err)
	}
	t.Cleanup(func() { other.Close() })

	if _, err := r.ts.StartTrim(r.job, 60, 300); err != nil {
		t.Fatalf("StartTrim: %v", err)
	}
	waitForCond(t, 10*time.Second, "the encode to open its output", func() bool { return len(r.files(t)) > 0 })
	if err := other.DeleteJob(r.job.ID); err != nil {
		t.Fatalf("DeleteJob through the second handle: %v", err)
	}
	if j, _ := r.db.GetJob(r.job.ID); j != nil {
		t.Fatal("the job is still readable through the service's handle")
	}
	touch(t, gate, "gate")

	ev := r.events.wait(t, TrimStateFailed)
	if ev.Error != trimJobDeletedReason {
		t.Errorf("event says %q, want %q", ev.Error, trimJobDeletedReason)
	}
	if n := len(r.rec.ByEvent("trim_error")); n != 0 {
		t.Errorf("trim_error sent %d times for a trim whose job was deleted", n)
	}
	if got := r.files(t); len(got) != 0 {
		t.Errorf("the trim left %v in trim/", got)
	}
}

// TestStopTrimStopsARunningTrim: a running trim had no stop but a restart of
// Moombox. StopTrim — what the dashboard's Stop and the TUI dialog's key call
// — kills FFmpeg, removes the partial file, stores nothing, sends no
// trim_error, and marks the outcome stopped so neither UI calls it a failure.
//
// Mutants: drop run's stopRequested case — the event reads as an interrupted
// trim, not a stopped one; drop StopTrim's id check — a stale id stops the
// job's current trim.
func TestStopTrimStopsARunningTrim(t *testing.T) {
	ffmpeg, _ := gatedFFmpeg(t) // the gate never opens
	r := newTrimRig(t, ffmpeg)

	task, err := r.ts.StartTrim(r.job, 60, 300)
	if err != nil {
		t.Fatalf("StartTrim: %v", err)
	}
	waitForCond(t, 10*time.Second, "the encode to open its output", func() bool { return len(r.files(t)) > 0 })

	if err := r.ts.StopTrim(r.job.ID, "trim_"+r.job.ID+"_1"); !errors.Is(err, ErrTrimNotFound) {
		t.Errorf("StopTrim with another id: err = %v, want ErrTrimNotFound", err)
	}
	if err := r.ts.StopTrim(r.job.ID, task.ID); err != nil {
		t.Fatalf("StopTrim: %v", err)
	}
	ev := r.events.wait(t, TrimStateFailed)
	if !ev.Stopped || ev.Error != trimStoppedReason || ev.StopReason != TrimStopOperator || ev.ID != task.ID {
		t.Errorf("stopped trim's event = %+v, want trim %q stopped with %q", ev, task.ID, trimStoppedReason)
	}
	if got := r.files(t); len(got) != 0 {
		t.Errorf("a stopped trim left %v in trim/", got)
	}
	if n := len(r.rec.ByEvent("trim_error")); n != 0 {
		t.Errorf("a stopped trim sent trim_error %d times", n)
	}
	if rows, _ := r.db.GetTrimsForJob(r.job.ID); len(rows) != 0 {
		t.Errorf("a stopped trim stored %d rows", len(rows))
	}
	if err := r.ts.StopTrim(r.job.ID, task.ID); !errors.Is(err, ErrTrimNotFound) {
		t.Errorf("StopTrim of a trim that has ended: err = %v, want ErrTrimNotFound", err)
	}
}

// TestStopTrimIsRefusedOnceTheClipIsBeingSaved: once the encode is whole
// the clip is moved into place and recorded without a stop point, so a stop
// then is refused as a conflict rather than answered "stopped" for a trim
// that is stored anyway; and a stop that came first ends the trim there.
//
// Mutants: drop StopTrim's saving check — the late stop is accepted; drop
// beginSaving's requested check — the stopped trim goes on to be saved.
func TestStopTrimIsRefusedOnceTheClipIsBeingSaved(t *testing.T) {
	r := newTrimRig(t, "ffmpeg-not-run")

	plan, err := r.ts.prepare(r.job, 60, 300)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !r.ts.beginSaving(plan) {
		t.Fatal("beginSaving refused a trim nobody stopped")
	}
	err = r.ts.StopTrim(r.job.ID, plan.task.ID)
	if refused, ok := errors.AsType[*TrimRefusedError](err); !ok || !refused.Conflict {
		t.Errorf("StopTrim while the clip is saved: err = %v, want a conflict", err)
	}
	r.ts.release(r.job.ID)

	plan, err = r.ts.prepare(r.job, 60, 300)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := r.ts.StopTrim(r.job.ID, plan.task.ID); err != nil {
		t.Fatalf("StopTrim: %v", err)
	}
	if r.ts.beginSaving(plan) {
		t.Error("beginSaving let a stopped trim be saved")
	}
	r.ts.release(r.job.ID)
}

// TestStoppedPostDownloadTrimSendsNothing: a post-download trim shows in the
// running list like any other, so it can be stopped. One cut short by the
// shutdown is told as Trim Failed (nobody asked for it from a dialog); one
// the operator stopped is an answer they gave, and sends nothing.
//
// Mutant: drop postDownloadTrim's ErrTrimStopped return — Trim Failed is sent.
func TestStoppedPostDownloadTrimSendsNothing(t *testing.T) {
	ffmpeg, _ := gatedFFmpeg(t) // the gate never opens
	r := newTrimRig(t, ffmpeg)
	o := &DownloadOrchestrator{db: r.db, logger: discardLogger{}, notifier: r.rec, trims: r.ts}

	start := 60.0
	job := *r.job
	job.StartTime = &start
	done := make(chan struct{})
	go func() {
		defer close(done)
		o.postDownloadTrim(t.Context(), &job)
	}()
	waitForCond(t, 10*time.Second, "the post-download encode to open its output", func() bool { return len(r.files(t)) > 0 })
	rs := r.ts.RunningTrims()
	if len(rs) != 1 {
		t.Fatalf("RunningTrims = %+v, want the post-download trim", rs)
	}
	if err := r.ts.StopTrim(r.job.ID, rs[0].ID); err != nil {
		t.Fatalf("StopTrim: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the post-download trim outlived StopTrim")
	}
	if n := len(r.rec.ByEvent("trim_error")); n != 0 {
		t.Errorf("a post-download trim the operator stopped: trim_error sent %d times", n)
	}
	if got := r.files(t); len(got) != 0 {
		t.Errorf("the stopped post-download trim left %v in trim/", got)
	}
}

// TestCreateTrimStartedNamesTheTrimBeforeItEncodes: the TUI's dialog stops
// its trim by id, so it must learn the id while the encode runs, not when it
// returns.
//
// Mutant: call started after run — the id arrives only once the trim is over.
func TestCreateTrimStartedNamesTheTrimBeforeItEncodes(t *testing.T) {
	ffmpeg, _ := gatedFFmpeg(t) // the gate never opens
	r := newTrimRig(t, ffmpeg)

	ids := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		_, err := r.ts.CreateTrimStarted(t.Context(), r.job, 60, 300, func(task TrimTask) { ids <- task.ID }, nil)
		done <- err
	}()
	var id string
	select {
	case id = <-ids:
	case <-time.After(10 * time.Second):
		r.ts.Stop()
		t.Fatal("no trim id while the encode runs")
	}
	if err := r.ts.StopTrim(r.job.ID, id); err != nil {
		t.Fatalf("StopTrim(%q): %v", id, err)
	}
	if err := <-done; !errors.Is(err, ErrTrimStopped) {
		t.Errorf("CreateTrimStarted of a stopped trim: err = %v, want ErrTrimStopped", err)
	}
}

// TestATrimFailingDuringShutdownIsInterrupted: systemd's default KillMode
// signals a trim's FFmpeg directly, so it fails before Stop cancels the
// service. Once the stop has begun that is the shutdown, not a broken trim:
// it is reported as interrupted and sends no trim_error (owner ruling R3-8).
//
// Mutant: drop shutdownBegun from run's outcome — trim_error is sent.
func TestATrimFailingDuringShutdownIsInterrupted(t *testing.T) {
	r := newTrimRig(t, filepath.Join(t.TempDir(), "no-ffmpeg-here"))
	r.ts.setShuttingDown(func() bool { return true })

	if _, err := r.ts.StartTrim(r.job, 60, 300); err != nil {
		t.Fatalf("StartTrim: %v", err)
	}
	ev := r.events.wait(t, TrimStateFailed)
	if ev.Error != trimInterruptedReason || ev.Stopped {
		t.Errorf("a trim that failed during shutdown: event = %+v, want %q", ev, trimInterruptedReason)
	}
	if n := len(r.rec.ByEvent("trim_error")); n != 0 {
		t.Errorf("a trim that failed during shutdown sent trim_error %d times", n)
	}
}
