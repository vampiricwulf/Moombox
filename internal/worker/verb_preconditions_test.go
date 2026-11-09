package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// TestResetVerbsRefuseAStatusTheyAreNotOfferedOn: the worker's reset verbs
// check the same status predicate the routes and the TUI offer them on, on
// the row as it reads when the verb is accepted. The TUI calls them on the row
// it last drew, so a job that moved on since — resumed by the monitor,
// finished — was reset under its run or over its archive. Refused, nothing
// is written, deleted or queued, and the error is an ErrNotAllowed naming the
// status (the routes' 409, the TUI's red line).
//
// Mutant: a nil gate in ResumeJob/ReinitializeJob/AutoReinitializeJob — the
// verb is accepted and its reset lands on the row.
func TestResetVerbsRefuseAStatusTheyAreNotOfferedOn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		verb   func(w *DownloadWorker, id string) error
		status database.JobStatus
		want   string
	}{
		{"resume a download", (*DownloadWorker).ResumeJob, database.StatusDownloading, "Job cannot be resumed in current state (Downloading)"},
		{"resume a complete archive", (*DownloadWorker).ResumeJob, database.StatusFinished, "Job cannot be resumed in current state (Finished)"},
		{"reinitialize an archive", (*DownloadWorker).ReinitializeJob, database.StatusFinished, "Job cannot be reinitialized in current state (Finished)"},
		{"reinitialize a queued job", (*DownloadWorker).ReinitializeJob, database.StatusQueued, "Job cannot be reinitialized in current state (Queued)"},
		{"auto-reinitialize a live job", (*DownloadWorker).AutoReinitializeJob, database.StatusLive, "Job cannot be reinitialized in current state (Live)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			dir := ownershipJob(t, w, db, "j-gate", tc.status)
			err := tc.verb(w, "j-gate")
			var na *NotAllowedError
			if !errors.Is(err, ErrNotAllowed) || !errors.As(err, &na) || err.Error() != tc.want {
				t.Fatalf("verb = %v, want ErrNotAllowed %q", err, tc.want)
			}
			w.wg.Wait()
			row, _ := db.GetJob("j-gate")
			if row.Status != tc.status || row.AutoRetryCount != 0 {
				t.Errorf("row = %s (auto_retry_count %d), want it untouched", row.Status, row.AutoRetryCount)
			}
			if _, err := os.Stat(dir); err != nil {
				t.Errorf("staging removed by a refused verb: %v", err)
			}
			if w.queue.isPending("j-gate") {
				t.Error("a refused verb queued the job")
			}
			// The refusal accepted nothing: the next verb is not ErrResetPending.
			db.UpdateJobFields("j-gate", map[string]any{"status": database.StatusError})
			if err := w.ReinitializeJob("j-gate"); err != nil {
				t.Errorf("a verb after the refusal = %v, want accepted", err)
			}
			w.wg.Wait()
		})
	}
}

// TestReinitResetFieldsAreTheSameForBothPaths: the operator's Reinitialize
// and the monitor's automatic one reset the same columns, to the same values,
// but for the retry budget. They were two ~35-field maps kept equal by a
// comment; now one (reinitResetFields), and this pins that both paths write
// it — by running both and comparing the rows they leave.
//
// Mutant: drop a column from one path's write (back to two maps) — the rows
// differ in it.
func TestReinitResetFieldsAreTheSameForBothPaths(t *testing.T) {
	dirty := map[string]any{
		"status": database.StatusError, "error": "boom", "park_reason": database.ParkReasonTwitchEndUnconfirmed,
		"park_identity": "x", "announce_found": true, "progress": "p", "percent": 50, "speed": "1x", "eta": "1m",
		"last_video_seq": 5, "last_audio_seq": 5, "total_video_seq": 9, "total_audio_seq": 9,
		"chat_status": "downloading", "total_chat_messages": 3, "download_started_at": "t", "stream_end_time": "t",
		"output_file": "o.mp4", "filename": "o.mp4", "file_size": 10, "chat_file": "c", "chat_filename": "c",
		"description_file": "d", "thumbnail_file": "t", "video_width": 1, "video_height": 1, "video_fps": 1.0,
		"length_seconds": 1, "join_offset_seconds": 1, "live_edge_reason": "r", "selected_video_itag": 1,
		"selected_audio_itag": 1, "incomplete_tail": true, "auto_retry_count": 1,
	}
	reset := func(verb func(w *DownloadWorker, id string) error) *database.Job {
		w, db := testWorkerSetup(t)
		ownershipJob(t, w, db, "j-reset", database.StatusError)
		db.UpdateJobFields("j-reset", dirty)
		if err := verb(w, "j-reset"); err != nil {
			t.Fatal(err)
		}
		w.wg.Wait()
		row, _ := db.GetJob("j-reset")
		return row
	}
	manual := reset((*DownloadWorker).ReinitializeJob)
	auto := reset((*DownloadWorker).AutoReinitializeJob)
	if manual.AutoRetryCount != 0 || auto.AutoRetryCount != 2 {
		t.Errorf("auto_retry_count: manual %d (want 0), auto %d (want the prior 1 + 1)", manual.AutoRetryCount, auto.AutoRetryCount)
	}
	if manual.Status != database.StatusUpcoming {
		t.Fatalf("manual reset left %s, want Upcoming", manual.Status)
	}
	auto.AutoRetryCount = manual.AutoRetryCount
	// Columns no reset touches.
	auto.UpdatedAt, auto.Version = manual.UpdatedAt, manual.Version
	auto.Segments = manual.Segments
	// Stamped by AddJob at one-second resolution in two separate setups,
	// which can fall on either side of a second boundary.
	auto.CreatedAt = manual.CreatedAt
	if a, m := jobColumns(auto), jobColumns(manual); a != m {
		t.Errorf("the two reinitialize paths leave different rows:\n manual %s\n auto   %s", m, a)
	}
	// And the reset reached every column the dirty row set.
	for col := range dirty {
		if _, ok := reinitResetFields(0)[col]; !ok {
			t.Errorf("the reset map leaves %s as the last run wrote it", col)
		}
	}
}

// jobColumns is a row as comparable text: its JSON, pointers followed.
func jobColumns(j *database.Job) string {
	b, err := json.Marshal(j)
	if err != nil {
		return err.Error()
	}
	return string(b)
}

// TestAReinitializeRefusedAtItsClaimDeletesNothing: the reset verbs claim the
// row (their conditional Upcoming write) before they delete anything. A
// status change landing after the verb's last read — an operator's Cancel
// between acceptedStatus and the write — refuses the claim, and the staging,
// the parts and the row stay exactly as they were. The deletion used to come
// first, so a reset that was then refused had already removed the staging.
//
// Mutant: RemoveAll ahead of the conditional write — the staging is gone
// while the row reads Cancelled.
func TestAReinitializeRefusedAtItsClaimDeletesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		verb func(w *DownloadWorker, id string) error
	}{
		{"reinitialize", (*DownloadWorker).ReinitializeJob},
		{"auto-reinitialize", (*DownloadWorker).AutoReinitializeJob},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			dir := ownershipJob(t, w, db, "j-claim", database.StatusError)
			if err := db.AddSegment(&database.Segment{JobID: "j-claim", SegmentIndex: 0, Filename: "p0.mp4"}); err != nil {
				t.Fatal(err)
			}
			orig := beforeResetClaim
			t.Cleanup(func() { beforeResetClaim = orig })
			beforeResetClaim = func(id string) {
				db.UpdateJobFields(id, map[string]any{"status": database.StatusCancelled})
			}
			if err := tc.verb(w, "j-claim"); err != nil {
				t.Fatal(err)
			}
			w.wg.Wait()
			row, _ := db.GetJob("j-claim")
			if row.Status != database.StatusCancelled || row.AutoRetryCount != 0 {
				t.Errorf("row = %s (auto_retry_count %d), want the Cancel standing", row.Status, row.AutoRetryCount)
			}
			if _, err := os.Stat(dir); err != nil {
				t.Errorf("staging deleted by a reset whose claim was refused: %v", err)
			}
			if segs, _ := db.GetSegments("j-claim"); len(segs) != 1 {
				t.Errorf("parts after the refused reset = %d, want the 1 kept", len(segs))
			}
			if w.queue.isPending("j-claim") {
				t.Error("a refused reset queued the job")
			}
		})
	}
}

// onInfoLogger runs hook on the first Info line whose message starts with
// prefix: a way to land a write at a point the code logs, with no seam.
type onInfoLogger struct {
	discardLogger
	prefix string
	once   sync.Once
	hook   func()
}

func (l *onInfoLogger) Info(msg string, _ ...any) {
	if strings.HasPrefix(msg, l.prefix) {
		l.once.Do(l.hook)
	}
}

// TestAutoMuxNeverWritesErrorOverAReinitialize: an operator's Reinitialize
// landing between the automatic Twitch mux's re-read and its mux — its reset
// writes Upcoming and deletes the staging — left muxJob nothing staged, and
// the automatic mux's failure write then turned the reset Upcoming row back
// into Error over the run the Reinitialize queued. The write is conditional
// on Error now. With the staging still there, the Mux itself is refused
// (ErrNotAllowed: Upcoming is no Mux source), and nothing is written either.
//
// Mutant: autoMuxNow's failure write unconditional — the row reads Error
// with the automatic-mux message.
func TestAutoMuxNeverWritesErrorOverAReinitialize(t *testing.T) {
	for _, tc := range []struct {
		name    string
		staging bool
	}{{"staging deleted", false}, {"staging still there", true}} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			id := "tw_reinit_" + strings.ReplaceAll(tc.name, " ", "_")
			seconds := 0
			if tc.staging {
				seconds = 5
			}
			latchedJob(t, w, db, id, seconds, true)
			w.twitchLiveness = func(context.Context, string) (*twitch.TwitchStreamInfo, error) { return nil, nil }
			w.logger = &onInfoLogger{prefix: "Twitch broadcast confirmed over", hook: func() {
				db.UpdateJobFields(id, reinitResetFields(0))
			}}

			w.AutoMuxEndedBroadcast(id)
			w.wg.Wait()

			j, _ := db.GetJob(id)
			if j.Status != database.StatusUpcoming || j.Error != "" {
				t.Errorf("row = %s %q, want the Reinitialize's Upcoming standing", j.Status, j.Error)
			}
		})
	}
}

// TestMuxRefusesARowAResumeMovedOn: a Mux decided on a row that a Resume has
// since moved on — Downloading, its run queued but not yet registered (or no
// longer queued) — wrote Muxing over it. The Muxing write applies only while
// the row is one the Mux action is offered on; the refusal is ErrNotAllowed
// naming the status, and the row is left as it is.
//
// Mutant: the Muxing write unconditional and no pre-check — Mux answers nil
// and the row reads Muxing.
func TestMuxRefusesARowAResumeMovedOn(t *testing.T) {
	w, db := testWorkerSetup(t)
	staging, _ := muxFixtureJob(t, w, db, "j-mux-resume")
	ffmpegPath, _ := requireFFmpegTools(t)
	writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 3)
	db.UpdateJobFields("j-mux-resume", map[string]any{"status": database.StatusError})

	// A Resume: Downloading, queued.
	if err := w.ResumeJob("j-mux-resume"); err != nil {
		t.Fatal(err)
	}
	if err := w.MuxJob("j-mux-resume"); !errors.Is(err, ErrStagingBusy) {
		t.Errorf("Mux over a queued Resume = %v, want ErrStagingBusy", err)
	}
	// Its queue entry gone (dequeued elsewhere), the row still says Downloading.
	if got, _, ok := w.queue.Dequeue(context.Background()); !ok || got != "j-mux-resume" {
		t.Fatalf("Dequeue = %q, %v", got, ok)
	}
	w.queue.Complete("j-mux-resume")
	err := w.MuxJob("j-mux-resume")
	if !errors.Is(err, ErrNotAllowed) || err.Error() != "Job cannot be muxed in current state (Downloading)" {
		t.Errorf("Mux of a Downloading row = %v, want ErrNotAllowed naming Downloading", err)
	}
	w.wg.Wait()
	if j, _ := db.GetJob("j-mux-resume"); j.Status != database.StatusDownloading {
		t.Errorf("row after the refused Mux = %s, want Downloading", j.Status)
	}
	if _, held := w.StagingClaimHolder("j-mux-resume"); held {
		t.Error("the refused Mux kept the staging claim")
	}
}

// TestMuxOfAFinishedRowNeedsAnUnmuxedPart: Mux is offered on a Finished job
// only while a part its finalize could not mux is still in staging
// (HasUnmuxedParts). The REST route and the TUI checked that on their own
// earlier read; the worker checked only the status, so a caller whose row was
// stale (read as Error, since Resumed and Finished with its staging kept)
// wrote Muxing over the archive and re-finalized it. Refused now, the row and
// the staging as they were.
//
// Mutant: muxJob without the unmuxed-part check — Mux answers nil and the
// row reads Muxing (then Finished again, re-finalized).
func TestMuxOfAFinishedRowNeedsAnUnmuxedPart(t *testing.T) {
	w, db := testWorkerSetup(t)
	staging, _ := muxFixtureJob(t, w, db, "j-mux-fin")
	if err := os.WriteFile(filepath.Join(staging, "video.mp4"), []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	db.UpdateJobFields("j-mux-fin", map[string]any{"status": database.StatusFinished, "output_file": "kept.mp4"})

	err := w.MuxJob("j-mux-fin")
	if !errors.Is(err, ErrNotAllowed) || err.Error() != "Job cannot be muxed in current state (Finished)" {
		t.Fatalf("Mux of a Finished row with no unmuxed part = %v, want ErrNotAllowed naming Finished", err)
	}
	w.wg.Wait()
	if j, _ := db.GetJob("j-mux-fin"); j.Status != database.StatusFinished || j.OutputFile != "kept.mp4" {
		t.Errorf("row after the refused Mux = %s (%q), want Finished with its archive", j.Status, j.OutputFile)
	}
	if !HasSegmentFiles(filepath.Dir(staging), "j-mux-fin") {
		t.Error("the refused Mux touched the staging")
	}
	if _, held := w.StagingClaimHolder("j-mux-fin"); held {
		t.Error("the refused Mux kept the staging claim")
	}
}

// TestMuxOfAJobWithNoRowIsNotFound: a Mux whose job's row is gone (deleted
// after the caller's read) answered ErrNotAllowed with an empty status — a
// 409 "Job cannot be muxed in current state ()". It is ErrJobNotFound, as the
// reset verbs answer, and the route's 404.
//
// Mutant: the NotAllowedError on a missing row — errors.Is(ErrJobNotFound)
// is false.
func TestMuxOfAJobWithNoRowIsNotFound(t *testing.T) {
	w, db := testWorkerSetup(t)
	staging, _ := muxFixtureJob(t, w, db, "j-mux-gone")
	if err := os.WriteFile(filepath.Join(staging, "video.mp4"), []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteJob("j-mux-gone"); err != nil {
		t.Fatal(err)
	}
	if err := w.MuxJob("j-mux-gone"); !errors.Is(err, ErrJobNotFound) || errors.Is(err, ErrNotAllowed) {
		t.Fatalf("Mux of a job with no row = %v, want ErrJobNotFound", err)
	}
	if _, held := w.StagingClaimHolder("j-mux-gone"); held {
		t.Error("the refused Mux kept the staging claim")
	}
}

// splitFinalizeFixture is a Muxing job with two recorded parts beside its
// output, for driving finalizeMultiSegmentJob directly (the split shape of
// the finalize, which muxAndFinalize's single-file tests do not reach).
func splitFinalizeFixture(t *testing.T, w *DownloadWorker, db *database.Database, id string) (*JobContext, []database.Segment) {
	t.Helper()
	job := &database.Job{ID: id, VideoID: id, URL: "u", Platform: "twitch", Status: database.StatusMuxing}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	var segments []database.Segment
	for i := range 2 {
		name := "Show - part" + string(rune('1'+i)) + ".mp4"
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte("part"), 0o644); err != nil {
			t.Fatal(err)
		}
		seg := database.Segment{JobID: id, SegmentIndex: i, Filename: name, FilePath: p,
			UnixStart: 1_700_000_000 + int64(i)*600, DurationSeconds: 300}
		if err := db.AddSegment(&seg); err != nil {
			t.Fatal(err)
		}
		segments = append(segments, seg)
	}
	jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, StagingDir: t.TempDir(),
		OutputDir: root, Filename: "Show", Logger: &discardLogger{}}
	return jobCtx, segments
}

// TestALateCancelOfASplitFinalizeLeavesItFinished is R-C1 in the split
// shape: a Cancel after finalizeMultiSegmentJob has muxed its parts and
// marked itself finishing is refused with ErrAlreadyFinishing, and the job
// ends Finished.
//
// Mutant: no finishing() call in finalizeMultiSegmentJob — the Cancel lands
// (Cancelled, answered success) and the finalize writes Finished over it.
func TestALateCancelOfASplitFinalizeLeavesItFinished(t *testing.T) {
	w, db := testWorkerSetup(t)
	jobCtx, segments := splitFinalizeFixture(t, w, db, "j-split-late")
	var cancelled bool
	var cancelErr error
	w.orchestrator.logger = &onInfoLogger{prefix: "multi-segment download complete", hook: func() {
		cancelled, _, cancelErr = w.CancelJob("j-split-late")
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // no network for copyAssets' thumbnail fallback
	if err := w.orchestrator.finalizeMultiSegmentJob(ctx, jobCtx, segments, nil); err != nil {
		t.Fatalf("finalizeMultiSegmentJob: %v", err)
	}
	if cancelled || !errors.Is(cancelErr, ErrAlreadyFinishing) {
		t.Errorf("late Cancel = %v, %v; want refused with ErrAlreadyFinishing", cancelled, cancelErr)
	}
	if j, _ := db.GetJob("j-split-late"); j.Status != database.StatusFinished {
		t.Errorf("row = %s, want Finished", j.Status)
	}
}

// TestACancelBeforeASplitFinalizeIsMarkedStands: the other side, split
// shape. A Cancel acknowledged before the finalize marks itself finishing
// stands: the finalize stops with ErrCancelled, the row stays Cancelled, and
// the parts stay on their rows and on disk.
//
// Mutant: the !ok branch after finishing() dropped — the finalize writes
// Finished over the acknowledged Cancel.
func TestACancelBeforeASplitFinalizeIsMarkedStands(t *testing.T) {
	w, db := testWorkerSetup(t)
	jobCtx, segments := splitFinalizeFixture(t, w, db, "j-split-early")
	mark := w.orchestrator.beginFinishing
	var cancelled bool
	w.orchestrator.beginFinishing = func(id string) (func(), bool) {
		cancelled, _, _ = w.CancelJob(id)
		return mark(id)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.orchestrator.finalizeMultiSegmentJob(ctx, jobCtx, segments, nil); !errors.Is(err, ErrCancelled) {
		t.Fatalf("finalizeMultiSegmentJob = %v, want ErrCancelled", err)
	}
	if !cancelled {
		t.Fatal("the Cancel ahead of the mark was refused")
	}
	if j, _ := db.GetJob("j-split-early"); j.Status != database.StatusCancelled {
		t.Errorf("row = %s, want the acknowledged Cancel standing", j.Status)
	}
	rows, _ := db.GetSegments("j-split-early")
	if len(rows) != 2 {
		t.Errorf("segment rows = %d, want both parts kept", len(rows))
	}
	for _, s := range segments {
		if _, err := os.Stat(s.FilePath); err != nil {
			t.Errorf("part %s gone: %v", s.Filename, err)
		}
	}
}

// TestALateCancelLeavesTheArchiveFinished is owner ruling R-C1: a Cancel that
// reaches a job whose finalize has already written its archive — after
// FFmpeg, before the Finished write — used to be written Cancelled and
// answered success, and the finalize then wrote Finished over it and sent a
// "Download Finished" after the "Job Cancelled". The job ends Finished now,
// and the Cancel answers ErrAlreadyFinishing with nothing written.
//
// Mutant: CancelJob without its finishing check — the Cancel reports
// success (and the row is written Cancelled under the finalize).
func TestALateCancelLeavesTheArchiveFinished(t *testing.T) {
	w, db := testWorkerSetup(t)
	staging, _ := muxFixtureJob(t, w, db, "j-late")
	ffmpegPath, _ := requireFFmpegTools(t)
	writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 3)
	db.UpdateJobFields("j-late", map[string]any{"status": database.StatusError})

	var cancelled bool
	var cancelErr error
	w.orchestrator.logger = &onInfoLogger{prefix: "download complete", hook: func() {
		cancelled, _, cancelErr = w.CancelJob("j-late")
	}}
	if err := w.MuxJob("j-late"); err != nil {
		t.Fatal(err)
	}
	w.wg.Wait()

	if cancelled || !errors.Is(cancelErr, ErrAlreadyFinishing) {
		t.Errorf("late Cancel = %v, %v; want refused with ErrAlreadyFinishing", cancelled, cancelErr)
	}
	j, _ := db.GetJob("j-late")
	if j.Status != database.StatusFinished || j.OutputFile == "" {
		t.Errorf("row = %s (output %q), want Finished with its archive", j.Status, j.OutputFile)
	}
	// Once finished, the mark is gone: a Cancel is the ordinary "ended" answer.
	if c, _, err := w.CancelJob("j-late"); c || err != nil {
		t.Errorf("Cancel of the Finished row = %v, %v; want not cancelled, no error", c, err)
	}
}

// TestACancelBeforeTheArchiveIsMarkedStands: the other side of R-C1. A
// Cancel that lands while FFmpeg writes the archive, before the finalize
// marks itself finishing, was acknowledged; the finalize then finds the row's
// outcome when it tries to mark, stops, and the row stays Cancelled with its
// staging kept. The file written is left where it is (logged).
//
// Mutant: beginFinishing marking without reading the row — the finalize
// writes Finished over the acknowledged Cancel.
func TestACancelBeforeTheArchiveIsMarkedStands(t *testing.T) {
	w, db := testWorkerSetup(t)
	staging, _ := muxFixtureJob(t, w, db, "j-early")
	ffmpegPath, _ := requireFFmpegTools(t)
	writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 3)
	db.UpdateJobFields("j-early", map[string]any{"status": database.StatusError})

	mark := w.orchestrator.beginFinishing
	var cancelled bool
	w.orchestrator.beginFinishing = func(id string) (func(), bool) {
		cancelled, _, _ = w.CancelJob(id)
		return mark(id)
	}
	if err := w.MuxJob("j-early"); err != nil {
		t.Fatal(err)
	}
	w.wg.Wait()

	if !cancelled {
		t.Fatal("the Cancel ahead of the mark was refused")
	}
	if j, _ := db.GetJob("j-early"); j.Status != database.StatusCancelled {
		t.Errorf("row = %s, want the acknowledged Cancel standing", j.Status)
	}
	if !HasSegmentFiles(filepath.Dir(staging), "j-early") {
		t.Error("staging was reclaimed under a Cancelled row")
	}
}
