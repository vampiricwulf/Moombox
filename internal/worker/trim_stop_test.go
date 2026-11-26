package worker

import (
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
	if ev.Error != trimJobDeletedReason {
		t.Errorf("the deleted job's trim event says %q, want %q", ev.Error, trimJobDeletedReason)
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
