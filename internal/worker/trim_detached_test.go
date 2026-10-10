package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

// gatedFFmpeg is a stand-in FFmpeg for the trim service: it records the
// output it was given (its last argument) in dir/outputs, writes a few bytes
// there the way a real encode opens its output at once, reports 24 s encoded
// on stderr the way FFmpeg's stats line does, and then waits for dir/gate. With dir/fail present it then fails as FFmpeg does; otherwise it
// finishes the file. It gives up after 30 s, so a run nothing stops (a broken
// Stop) cannot outlive the test binary. Its sibling ffprobe does not exist,
// so the audio-bitrate probe falls back to its default without running
// anything.
func gatedFFmpeg(t *testing.T) (ffmpeg, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stand-in for FFmpeg")
	}
	dir = t.TempDir()
	ffmpeg = filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\n" +
		"for a; do out=\"$a\"; done\n" +
		"printf '%s\\n' \"$out\" >> '" + dir + "/outputs'\n" +
		"printf partial > \"$out\"\n" +
		"printf 'frame=120 fps=60 q=28.0 size=256kB time=00:00:24.00 bitrate=87.4kbits/s speed=12x\\r' >&2\n" +
		"i=0; while [ ! -e '" + dir + "/gate' ]; do sleep 0.02; i=$((i+1)); [ $i -gt 1500 ] && exit 1; done\n" +
		"if [ -e '" + dir + "/fail' ]; then echo 'Error writing trailer: No space left on device' >&2; echo 'Conversion failed!' >&2; exit 1; fi\n" +
		"printf whole > \"$out\"\n"
	if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return ffmpeg, dir
}

// touch creates name in dir: the gate opens, or the next encode fails.
func touch(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// trimEvents records every TrimEvent the service hands its hook.
type trimEvents struct {
	mu  sync.Mutex
	evs []TrimEvent
}

func (e *trimEvents) add(ev TrimEvent) {
	e.mu.Lock()
	e.evs = append(e.evs, ev)
	e.mu.Unlock()
}

// wait returns the first event in state, failing the test if none arrives.
func (e *trimEvents) wait(t *testing.T, state string) TrimEvent {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		for _, ev := range e.evs {
			if ev.State == state {
				e.mu.Unlock()
				return ev
			}
		}
		e.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %q trim event in 10s", state)
	return TrimEvent{}
}

func (e *trimEvents) count(state string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, ev := range e.evs {
		if ev.State == state {
			n++
		}
	}
	return n
}

type trimRig struct {
	ts     *TrimService
	db     *database.Database
	job    *database.Job
	rec    *notificationtest.Recorder
	events *trimEvents
	trims  string // the job's trim/ directory
}

// newTrimRig is a trim service over a real database holding one Finished
// 600 s single-file job, with a notification recorder and an event log.
func newTrimRig(t *testing.T, ffmpeg string) *trimRig {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "trim.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	out := filepath.Join(dir, "out", "chan", "archive.mp4")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte("src"), 0o644); err != nil {
		t.Fatal(err)
	}
	length := 600
	job := &database.Job{
		ID: "yt_detached", VideoID: "detached", Platform: "youtube", URL: "u", Title: "A stream",
		Status: database.StatusFinished, OutputFile: out, Filename: filepath.Join("chan", "archive.mp4"),
		LengthSeconds: &length,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	r := &trimRig{
		ts: NewTrimService(db, ffmpeg, discardLogger{}), db: db, job: job,
		rec: notificationtest.New(), events: &trimEvents{},
		trims: filepath.Join(filepath.Dir(out), "trim"),
	}
	r.ts.SetNotifier(r.rec)
	r.ts.SetOnEvent(r.events.add)
	t.Cleanup(r.ts.Stop)
	return r
}

// files lists what the job's trim/ directory holds.
func (r *trimRig) files(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(r.trims)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// waitFor polls cond for up to 10 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestStartTrimRunsDetached: the dashboard's trim route answers at once and
// the encode runs as the service's own task. It used to run the whole encode
// under the request, so a page closed or reloaded mid-trim killed FFmpeg —
// and the partial output FFmpeg had written stayed in trim/ under the
// finished trim's name, with no row and no word to anyone. The encode now
// writes a .partial.mp4 that takes the finished name only when it is whole;
// the record carries the id the route answered with; and a second trim of the
// job is refused while the first runs.
//
// Mutants: run the encode inside StartTrim (drop the `go`) — StartTrim waits
// for the gated encode; partialTrimPath returns its argument — the finished
// name exists mid-encode; drop prepare's `ts.activeOps[job.ID] != nil`
// refusal — the second trim starts; give the record a fresh id in run — it is
// not the one StartTrim returned.
func TestStartTrimRunsDetached(t *testing.T) {
	ffmpeg, gate := gatedFFmpeg(t)
	r := newTrimRig(t, ffmpeg)

	started := make(chan TrimTask, 1)
	go func() {
		task, err := r.ts.StartTrim(r.job, 60, 300)
		if err != nil {
			t.Errorf("StartTrim: %v", err)
		}
		started <- task
	}()
	var task TrimTask
	select {
	case task = <-started:
	case <-time.After(5 * time.Second):
		touch(t, gate, "gate") // let the mutant's encode end before the test does
		t.Fatal("StartTrim waited for the encode")
	}
	if task.ID == "" || task.JobID != r.job.ID || task.StartTime != 60 || task.EndTime != 300 {
		t.Fatalf("StartTrim returned %+v", task)
	}
	if ev := r.events.wait(t, TrimStateRunning); ev.ID != task.ID {
		t.Errorf("running event names trim %q, want %q", ev.ID, task.ID)
	}

	finished := filepath.Join(r.trims, "detached [60s-300s].mp4")
	waitFor(t, "the encode to open its output", func() bool { return len(r.files(t)) > 0 })
	if _, err := os.Stat(finished); err == nil {
		t.Errorf("the finished trim's name exists while the encode runs: %v", r.files(t))
	}
	if got := r.files(t); len(got) != 1 || got[0] != "detached [60s-300s].partial.mp4" {
		t.Errorf("trim/ holds %v mid-encode, want only the .partial.mp4", got)
	}

	for _, rng := range [][2]float64{{60, 300}, {10, 20}} {
		_, err := r.ts.StartTrim(r.job, rng[0], rng[1])
		var refused *TrimRefusedError
		if !errors.As(err, &refused) || !refused.Conflict {
			t.Errorf("a second trim [%v, %v] while one runs: err = %v, want a conflict", rng[0], rng[1], err)
		}
	}

	touch(t, gate, "gate")
	ev := r.events.wait(t, TrimStateFinished)
	if ev.Trim == nil || ev.Trim.ID != task.ID {
		t.Fatalf("finished event carries %+v, want the record of %q", ev.Trim, task.ID)
	}
	rows, err := r.db.GetTrimsForJob(r.job.ID)
	if err != nil || len(rows) != 1 || rows[0].ID != task.ID {
		t.Fatalf("trim rows = %+v (err %v), want one with id %q", rows, err, task.ID)
	}
	if got := r.files(t); len(got) != 1 || got[0] != filepath.Base(finished) {
		t.Errorf("trim/ holds %v after the encode, want only the finished trim", got)
	}
	if b, _ := os.ReadFile(finished); string(b) != "whole" {
		t.Errorf("the finished trim holds %q, want the whole encode", b)
	}
	if n := len(r.rec.ByEvent("trim_created")); n != 1 {
		t.Errorf("trim_created sent %d times, want 1", n)
	}
	if n := len(r.rec.ByEvent("trim_error")); n != 0 {
		t.Errorf("trim_error sent %d times for a trim that worked", n)
	}
}

// TestStoppedTrimLeavesNothing: a trim cut short — Moombox stopping, the
// service's Stop — removes what it wrote. Mux keeps an archive mux's partial
// output when its context is cancelled (cleanupFailedMux, for a salvage);
// a trim's partial file is worth nothing, and under the finished name it
// read as a finished trim. A stop is not a failure: no trim_error, and the
// dashboard is told the trim was interrupted. Nothing starts after Stop.
//
// Mutants: drop run's `os.Remove(partial)` — the partial file stays; drop
// Stop's `ts.cancel()` — the encode runs on; send trim_error for a cancelled
// trim (drop the `ctx.Err() != nil` arm) — a stop is reported as a failure;
// drop Stop's `ts.stopped = true` — a trim starts after it.
func TestStoppedTrimLeavesNothing(t *testing.T) {
	ffmpeg, _ := gatedFFmpeg(t) // the gate never opens
	r := newTrimRig(t, ffmpeg)

	if _, err := r.ts.StartTrim(r.job, 60, 300); err != nil {
		t.Fatalf("StartTrim: %v", err)
	}
	r.events.wait(t, TrimStateRunning)
	waitFor(t, "the encode to open its output", func() bool { return len(r.files(t)) > 0 })

	r.ts.Stop()
	ev := r.events.wait(t, TrimStateFailed)
	if ev.Error != trimInterruptedReason {
		t.Errorf("stopped trim's event says %q, want %q", ev.Error, trimInterruptedReason)
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
	var refused *TrimRefusedError
	if _, err := r.ts.StartTrim(r.job, 10, 20); !errors.As(err, &refused) {
		t.Errorf("StartTrim after Stop: err = %v, want a refusal", err)
	}
}

// TestBrokenTrimSendsTrimError: trim_error is documented as "Trim operation
// failed", but only the post-download trim sent it — a Trim Video from either
// UI that failed (a full disk, an FFmpeg error, a database fault) sent
// nothing, so a target filtered on Trim Error never heard of it. The service
// now sends it for every trim that broke, whichever UI asked and whether it
// broke setting up or encoding; a refusal is answered to the requester, not
// notified.
//
// Mutants: drop run's sendTrimFailed (the encode failures send nothing);
// drop prepare's (the setup failure sends nothing); send for refusals too
// (drop prepare's `!refused`).
func TestBrokenTrimSendsTrimError(t *testing.T) {
	ffmpeg, gate := gatedFFmpeg(t)
	touch(t, gate, "gate")
	touch(t, gate, "fail")
	r := newTrimRig(t, ffmpeg)

	// The TUI's path: in process, waiting for the outcome.
	if _, err := r.ts.CreateTrim(t.Context(), r.job, 60, 300, nil); err == nil {
		t.Fatal("CreateTrim with a failing FFmpeg succeeded")
	}
	if ev := r.events.wait(t, TrimStateFailed); ev.Error != trimFailedReason {
		t.Errorf("failed event says %q, want %q", ev.Error, trimFailedReason)
	}
	if n := len(r.rec.ByEvent("trim_error")); n != 1 {
		t.Fatalf("a failed in-process trim sent trim_error %d times, want 1", n)
	}
	if call := r.rec.ByEvent("trim_error")[0]; !strings.Contains(call.Title, "Trim Failed") {
		t.Errorf("trim_error embed titled %q", call.Title)
	}

	// The dashboard's path: detached.
	if _, err := r.ts.StartTrim(r.job, 60, 300); err != nil {
		t.Fatalf("StartTrim: %v", err)
	}
	waitFor(t, "the detached trim to fail", func() bool { return r.events.count(TrimStateFailed) == 2 })
	if n := len(r.rec.ByEvent("trim_error")); n != 2 {
		t.Errorf("a failed detached trim: trim_error sent %d times in all, want 2", n)
	}
	if got := r.files(t); len(got) != 0 {
		t.Errorf("failed trims left %v in trim/", got)
	}

	// A refusal is the requester's to read, not a notification.
	if _, err := r.ts.CreateTrim(t.Context(), r.job, 30, 10, nil); err == nil {
		t.Fatal("a reversed range was not refused")
	}
	if n := len(r.rec.ByEvent("trim_error")); n != 2 {
		t.Errorf("a refusal sent trim_error (now %d)", n)
	}

	// A trim that breaks before its encode: the trims list cannot be read.
	r.db.Close()
	if _, err := r.ts.CreateTrim(t.Context(), r.job, 100, 200, nil); err == nil {
		t.Fatal("CreateTrim with the database closed succeeded")
	}
	if n := len(r.rec.ByEvent("trim_error")); n != 3 {
		t.Errorf("a trim that broke setting up: trim_error sent %d times in all, want 3", n)
	}
}

// TestPostDownloadTrimSendsOneTrimFailed: the post-download trim used to send
// Trim Failed for every error CreateTrim returned. The service now sends it
// itself for a trim that broke, so the orchestrator sends only what the
// service leaves to its caller — a refusal of the range the job asked for —
// and a failed post-download trim is still told exactly once.
//
// Mutants: send whatever the error (the old unconditional send) — a broken
// trim is told twice; never send from the orchestrator — a refused one is
// not told.
func TestPostDownloadTrimSendsOneTrimFailed(t *testing.T) {
	ffmpeg, gate := gatedFFmpeg(t)
	touch(t, gate, "gate")
	touch(t, gate, "fail")
	r := newTrimRig(t, ffmpeg)
	o := &DownloadOrchestrator{db: r.db, logger: discardLogger{}, notifier: r.rec, trims: r.ts}

	start := 60.0
	job := *r.job
	job.StartTime = &start
	o.postDownloadTrim(t.Context(), &job)
	if n := len(r.rec.ByEvent("trim_error")); n != 1 {
		t.Fatalf("a post-download trim whose encode failed: trim_error sent %d times, want 1", n)
	}

	end := 900.0 // past the 600 s recording
	job.EndTime = &end
	o.postDownloadTrim(t.Context(), &job)
	if n := len(r.rec.ByEvent("trim_error")); n != 2 {
		t.Errorf("a refused post-download trim: trim_error sent %d times in all, want 2", n)
	}
}

// TestTrimEventWireShape pins the trim_status payload app.js reads
// (handleTrimStatus): the task's fields flattened beside its state, then the
// record or the reason. A renamed or nested key is a trim whose result the
// dashboard silently drops.
//
// Mutant: rename a json tag (`jobId` to `job_id`), or name the embedded
// TrimTask field so it nests.
func TestTrimEventWireShape(t *testing.T) {
	ev := TrimEvent{
		TrimTask: TrimTask{ID: "trim_j_1", JobID: "j", StartTime: 1.5, EndTime: 9, Progress: 42},
		State:    TrimStateFinished,
		Trim:     &database.TrimRecord{ID: "trim_j_1", JobID: "j"},
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"id": "trim_j_1", "jobId": "j", "startTime": 1.5, "endTime": 9.0, "progress": 42.0, "state": "finished"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("trim_status[%q] = %#v, want %#v (payload %s)", k, got[k], v, b)
		}
	}
	if trim, ok := got["trim"].(map[string]any); !ok || trim["id"] != "trim_j_1" {
		t.Errorf("trim_status carries no record under \"trim\": %s", b)
	}
	if _, ok := got["error"]; ok {
		t.Errorf("a finished trim's frame carries an error: %s", b)
	}
}

// TestRunningTrimsReportProgress: the dashboard had no trim progress at all —
// only a spinning Create button — while the TUI showed a percentage. The
// service now keeps each running trim's FFmpeg percentage, the figure the
// TUI's dialog reads, in RunningTrims (what initial_state seeds a page with,
// so a reload mid-trim still shows it) and hands it on as "running" events,
// at most one per progressInterval; a finished trim leaves the list.
//
// Mutants: drop noteProgress's `plan.task.Progress = pct` — RunningTrims
// says 0; drop its emit — no event carries the figure; drop its interval
// check — a closed interval still sends every report; drop release's delete
// — the finished trim is still listed.
func TestRunningTrimsReportProgress(t *testing.T) {
	ffmpeg, gate := gatedFFmpeg(t)
	r := newTrimRig(t, ffmpeg)
	r.ts.progressInterval = 0 // every report

	task, err := r.ts.StartTrim(r.job, 60, 300) // 24 s of 240 s: 10%
	if err != nil {
		t.Fatalf("StartTrim: %v", err)
	}
	waitFor(t, "the running trim's progress", func() bool {
		rs := r.ts.RunningTrims()
		return len(rs) == 1 && rs[0].ID == task.ID && rs[0].Progress == 10
	})
	waitFor(t, "a running event carrying the progress", func() bool {
		r.events.mu.Lock()
		defer r.events.mu.Unlock()
		for _, ev := range r.events.evs {
			if ev.State == TrimStateRunning && ev.ID == task.ID && ev.Progress == 10 {
				return true
			}
		}
		return false
	})
	touch(t, gate, "gate")
	r.events.wait(t, TrimStateFinished)
	if rs := r.ts.RunningTrims(); len(rs) != 0 {
		t.Errorf("RunningTrims lists %+v after the trim finished", rs)
	}

	// The interval: with none elapsed, the start's event is the only one.
	ffmpeg2, gate2 := gatedFFmpeg(t)
	r2 := newTrimRig(t, ffmpeg2)
	r2.ts.progressInterval = time.Hour
	if _, err := r2.ts.StartTrim(r2.job, 60, 300); err != nil {
		t.Fatalf("StartTrim: %v", err)
	}
	waitFor(t, "the second trim's progress", func() bool {
		rs := r2.ts.RunningTrims()
		return len(rs) == 1 && rs[0].Progress == 10
	})
	if n := r2.events.count(TrimStateRunning); n != 1 {
		t.Errorf("%d running events inside one progress interval, want only the start's", n)
	}
	touch(t, gate2, "gate")
	r2.events.wait(t, TrimStateFinished)
}

// TestPostDownloadTrimHoldsTheJobsTrimSlot: the post-download trim built a
// TrimService of its own, whose one-trim-per-job slot the shared service —
// the dashboard's and the TUI's — could not see. While it encoded,
// RunningTrims (initial_state) listed nothing, and a dashboard trim of the
// very range it was encoding was accepted: both FFmpegs wrote the one
// .partial.mp4, the first to finish renamed it into place while the other
// still wrote it, and the clip stored and announced was unplayable. It now
// runs through the shared service, so it holds the job's slot — a second
// trim of the job, the same range or another, is refused (409) while it
// runs — and it is listed and reported like any other trim.
//
// Mutants: build a private NewTrimService in postDownloadTrim again — nothing
// is listed, the dashboard trim starts beside it, and FFmpeg runs twice for
// two rows; drop prepare's `ts.activeOps[job.ID] != nil` refusal — the
// dashboard trim starts.
func TestPostDownloadTrimHoldsTheJobsTrimSlot(t *testing.T) {
	ffmpeg, gate := gatedFFmpeg(t)
	r := newTrimRig(t, ffmpeg)
	o := &DownloadOrchestrator{db: r.db, logger: discardLogger{}, notifier: r.rec, trims: r.ts}

	start, end := 60.0, 300.0
	job := *r.job
	job.StartTime, job.EndTime = &start, &end
	done := make(chan struct{})
	go func() {
		defer close(done)
		o.postDownloadTrim(t.Context(), &job)
	}()
	// Whatever fails below, the gated encodes end before the test does.
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(gate, "gate"), nil, 0o644)
		<-done
	})
	waitFor(t, "the post-download encode to open its output", func() bool { return len(r.files(t)) > 0 })

	if rs := r.ts.RunningTrims(); len(rs) != 1 || rs[0].JobID != r.job.ID || rs[0].StartTime != start || rs[0].EndTime != end {
		t.Errorf("RunningTrims = %+v while the post-download trim encodes, want it listed", rs)
	}
	for _, rng := range [][2]float64{{start, end}, {10, 20}} {
		_, err := r.ts.StartTrim(r.job, rng[0], rng[1])
		var refused *TrimRefusedError
		if !errors.As(err, &refused) || !refused.Conflict {
			t.Errorf("a dashboard trim [%v, %v] while the post-download trim runs: err = %v, want a conflict", rng[0], rng[1], err)
		}
	}

	touch(t, gate, "gate")
	<-done
	waitFor(t, "every trim to end", func() bool { return len(r.ts.RunningTrims()) == 0 })
	if n := r.events.count(TrimStateRunning); n == 0 {
		t.Error("no running event for the post-download trim: the dashboard never sees it")
	}
	if ev := r.events.wait(t, TrimStateFinished); ev.JobID != r.job.ID || ev.Trim == nil {
		t.Errorf("finished event %+v, want the post-download trim's record", ev)
	}
	if b, _ := os.ReadFile(filepath.Join(gate, "outputs")); strings.Count(string(b), "\n") != 1 {
		t.Errorf("FFmpeg ran for:\n%s\nwant the one post-download encode", b)
	}
	if rows, _ := r.db.GetTrimsForJob(r.job.ID); len(rows) != 1 {
		t.Errorf("%d trim rows, want the post-download trim's one: %+v", len(rows), rows)
	}
	if n := len(r.rec.ByEvent("trim_created")); n != 1 {
		t.Errorf("trim_created sent %d times, want 1", n)
	}
	if n := len(r.rec.ByEvent("trim_error")); n != 0 {
		t.Errorf("trim_error sent %d times", n)
	}
}

// TestStoppedPostDownloadTrimSendsTrimFailed: at shutdown TrimService.Stop
// runs ahead of the worker, so it cuts a post-download trim short while the
// job's own context is still live. The service sends nothing for a stopped
// trim — nothing failed, and a UI that asked has its answer — but nobody
// asked for this one from a dialog, so the post-download trim tells the user
// itself. Its context cannot say the trim was stopped; the error does.
//
// Mutants: test the job context again (`ctx.Err() != nil`) in place of
// errTrimInterrupted — nothing is sent; drop run's errTrimInterrupted wrap —
// likewise.
func TestStoppedPostDownloadTrimSendsTrimFailed(t *testing.T) {
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
	waitFor(t, "the post-download encode to open its output", func() bool { return len(r.files(t)) > 0 })

	r.ts.Stop()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the post-download trim outlived the trim service's Stop")
	}
	if n := len(r.rec.ByEvent("trim_error")); n != 1 {
		t.Errorf("a post-download trim the stop cut short: trim_error sent %d times, want 1", n)
	}
	if got := r.files(t); len(got) != 0 {
		t.Errorf("the stopped post-download trim left %v in trim/", got)
	}
	if rows, _ := r.db.GetTrimsForJob(r.job.ID); len(rows) != 0 {
		t.Errorf("the stopped post-download trim stored %d rows", len(rows))
	}
}

// TestPostDownloadTrimWithoutServiceSaysSo: a worker built without a trim
// service (DownloadWorkerDeps.TrimService unset) cannot run the trim the job
// asked for; it says so rather than skipping it in silence.
//
// Mutant: return without the send.
func TestPostDownloadTrimWithoutServiceSaysSo(t *testing.T) {
	r := newTrimRig(t, "ffmpeg-unused")
	o := &DownloadOrchestrator{db: r.db, logger: discardLogger{}, notifier: r.rec}

	start := 60.0
	job := *r.job
	job.StartTime = &start
	o.postDownloadTrim(t.Context(), &job)
	if n := len(r.rec.ByEvent("trim_error")); n != 1 {
		t.Errorf("trim_error sent %d times, want 1", n)
	}
}

// TestNewDownloadWorkerSharesTheTrimService: the trim service handed in
// DownloadWorkerDeps is the one the orchestrator's post-download trim runs
// through — not a service of the worker's own, whose slot the dashboard's
// cannot see (TestPostDownloadTrimHoldsTheJobsTrimSlot).
//
// Mutant: drop NewDownloadWorker's `orchestrator.trims = trims`.
func TestNewDownloadWorkerSharesTheTrimService(t *testing.T) {
	r := newTrimRig(t, "ffmpeg-unused")
	cfg := &config.MoomboxConfig{}
	cfg.Paths.StagingDirectory = filepath.Join(t.TempDir(), "staging")
	w := NewDownloadWorker(r.db, nil, cfg, &discardLogger{}, &DownloadWorkerDeps{TrimService: r.ts})
	if w.orchestrator.trims != r.ts {
		t.Errorf("the orchestrator's trim service is %p, want the one handed in (%p)", w.orchestrator.trims, r.ts)
	}
}

// TestPostDownloadTrimThatCannotRunSaysSo: trim_error is documented as sent
// for a post-download trim that did not produce its file for any reason, but
// postDownloadTrim returned in silence when the trim could not run at all —
// a start at or past the end of the recording with no end given, a length
// it could not know, a row no longer Finished, a row it could not read — and
// the trim the job asked for simply never appeared. Each now sends Trim
// Failed with a reason the user can act on; only a job deleted first sends
// nothing, since whoever deleted it wants none of it.
//
// Mutants: return without failPostDownloadTrim at the past-the-end check, the
// unknown-length case or the read failure — that case sends nothing; restore
// the `freshJob.Status != database.StatusFinished` early return — the row no
// longer Finished sends nothing; drop failPostDownloadTrim's deleted-job
// check — the job deleted while its trim ran is told; send at the
// deleted-row return — the job deleted before its trim is told.
func TestPostDownloadTrimThatCannotRunSaysSo(t *testing.T) {
	ffmpeg, gate := gatedFFmpeg(t)
	touch(t, gate, "gate")
	for _, tc := range []struct {
		name     string
		start    float64
		alter    func(t *testing.T, r *trimRig)
		sends    bool
		inReason string
	}{
		{name: "start past the end", start: 700, sends: true, inReason: "700s"},
		{name: "start at the end", start: 600, sends: true, inReason: "600s"},
		{name: "length unknown", start: 60, sends: true, inReason: "length is unknown",
			alter: func(t *testing.T, r *trimRig) {
				r.db.UpdateJobFields(r.job.ID, map[string]any{"length_seconds": nil})
			}},
		{name: "row no longer Finished", start: 60, sends: true, inReason: "finished",
			alter: func(t *testing.T, r *trimRig) {
				r.db.UpdateJobFields(r.job.ID, map[string]any{"status": database.StatusError})
			}},
		{name: "row unreadable", start: 60, sends: true, inReason: "read the job",
			alter: func(t *testing.T, r *trimRig) { r.db.Close() }},
		{name: "job deleted", start: 60, sends: false,
			alter: func(t *testing.T, r *trimRig) {
				if err := r.db.DeleteJob(r.job.ID); err != nil {
					t.Fatal(err)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTrimRig(t, ffmpeg)
			o := &DownloadOrchestrator{db: r.db, logger: discardLogger{}, notifier: r.rec, trims: r.ts}
			if tc.alter != nil {
				tc.alter(t, r)
			}
			job := *r.job
			job.StartTime = &tc.start
			o.postDownloadTrim(t.Context(), &job)

			calls := r.rec.ByEvent("trim_error")
			if !tc.sends {
				if len(calls) != 0 {
					t.Errorf("trim_error sent %d times for a job deleted before its trim", len(calls))
				}
				return
			}
			if len(calls) != 1 {
				t.Fatalf("a post-download trim that could not run: trim_error sent %d times, want 1", len(calls))
			}
			reason := ""
			for _, f := range calls[0].Fields {
				if f.Name == "Error" {
					reason = f.Value
				}
			}
			if !strings.Contains(reason, tc.inReason) {
				t.Errorf("Trim Failed says %q, want it to mention %q", reason, tc.inReason)
			}
			if got := r.files(t); len(got) != 0 {
				t.Errorf("trim/ holds %v", got)
			}
		})
	}

	// Deleted while its trim ran: processJob cancels a job's context when its
	// row is deleted (OnJobDeleted), and the trim that stops is not told.
	t.Run("job deleted while its trim ran", func(t *testing.T) {
		ffmpeg, _ := gatedFFmpeg(t) // the gate never opens
		r := newTrimRig(t, ffmpeg)
		o := &DownloadOrchestrator{db: r.db, logger: discardLogger{}, notifier: r.rec, trims: r.ts}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		start := 60.0
		job := *r.job
		job.StartTime = &start
		done := make(chan struct{})
		go func() {
			defer close(done)
			o.postDownloadTrim(ctx, &job)
		}()
		waitFor(t, "the post-download encode to open its output", func() bool { return len(r.files(t)) > 0 })
		if err := r.db.DeleteJob(r.job.ID); err != nil {
			t.Fatal(err)
		}
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the post-download trim outlived its job's context")
		}
		if n := len(r.rec.ByEvent("trim_error")); n != 0 {
			t.Errorf("trim_error sent %d times for a job deleted while its trim ran", n)
		}
	})
}
