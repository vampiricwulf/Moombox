package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestYouTubeErrorSettlesAnInFlightChat (owner ruling R-E1): the
// orchestrator's error arm stops the chat downloader without recording a
// verdict, so a failed YouTube download left "downloading" (or "pending") on
// its Error row and the Web badge showed a capture in progress. setJobError
// now settles an in-flight value as a Cancel does (owner ruling R-E1a): a
// running capture becomes "incomplete", a pending one is cleared; a recorded
// verdict and a COOKIES? park are left as they were. A Twitch row settles the
// same way (owner ruling R-H1; TestNoErrorWriteLeavesATwitchChatInFlight).
//
// Mutant: dropping the settleErrorChat call — the first two rows keep their
// in-flight value; settling pending to "incomplete" (R-E1 as first written)
// fails the second.
func TestYouTubeErrorSettlesAnInFlightChat(t *testing.T) {
	for _, tc := range []struct {
		name     string
		platform string
		chat     database.ChatStatus
		err      error
		want     database.ChatStatus
	}{
		{"downloading", "youtube", database.ChatDownloading, errors.New("download: reset"), database.ChatIncomplete},
		{"pending", "youtube", database.ChatPending, errors.New("download: reset"), database.ChatNone},
		{"finished verdict", "youtube", database.ChatFinished, errors.New("mux: failed"), database.ChatFinished},
		{"unavailable verdict", "youtube", database.ChatUnavailable, errors.New("download: reset"), database.ChatUnavailable},
		{"no chat", "youtube", database.ChatNone, errors.New("download: reset"), database.ChatNone},
		{"twitch settles as youtube does (R-H1)", "twitch", database.ChatDownloading, errors.New("download: reset"), database.ChatIncomplete},
		{"a COOKIES? park is not an Error", "youtube", database.ChatDownloading, ErrCookiesRequired, database.ChatDownloading},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			job := &database.Job{ID: "j", VideoID: "v", URL: "u", Platform: tc.platform,
				Status: database.StatusDownloading, ChatStatus: tc.chat}
			if _, err := db.AddJob(job); err != nil {
				t.Fatal(err)
			}
			w.setJobError(job, tc.err)
			row, _ := db.GetJob(job.ID)
			if row.ChatStatus != tc.want {
				t.Errorf("chat_status = %q (status %s), want %q", row.ChatStatus, row.Status, tc.want)
			}
		})
	}
}

// TestAResumeAheadOfTheErrorChatSettleWins: the settle is conditional on the
// row still reading Error, so a Resume that moved the row on before it — here
// a subscriber to the Error write, the earliest a Resume could land — keeps
// its own chat value, which its run re-arms. And a Resume of a settled row is
// accepted: "incomplete" does not stand in its way.
func TestAResumeAheadOfTheErrorChatSettleWins(t *testing.T) {
	w, db := testWorkerSetup(t)
	job := &database.Job{ID: "r", VideoID: "v", URL: "u", Platform: "youtube",
		Status: database.StatusDownloading, ChatStatus: database.ChatDownloading}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	resumed := false
	db.OnJobUpdate(func(j *database.Job) {
		if j.ID == job.ID && j.Status == database.StatusError && !resumed {
			resumed = true
			db.UpdateJobFieldsIf(j.ID, database.StatusError, map[string]any{
				"status": database.StatusDownloading, "chat_status": database.ChatPending,
			})
		}
	})
	w.setJobError(job, errors.New("download: reset"))
	row, _ := db.GetJob(job.ID)
	if !resumed || row.Status != database.StatusDownloading || row.ChatStatus != database.ChatPending {
		t.Errorf("row = %s %q, want the Resume's Downloading %q to stand", row.Status, row.ChatStatus, database.ChatPending)
	}
}

// TestResumeAcceptsAnErrorRowWithAnIncompleteChat: the settled value is no
// gate on Resume. The row goes back to Downloading and is enqueued; its run's
// chat setup writes "pending" over the value (setupChatDownloader).
func TestResumeAcceptsAnErrorRowWithAnIncompleteChat(t *testing.T) {
	w, db := testWorkerSetup(t)
	job := &database.Job{ID: "ri", VideoID: "v", URL: "u", Platform: "youtube",
		Status: database.StatusDownloading, ChatStatus: database.ChatDownloading}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	w.setJobError(job, errors.New("download: reset"))
	if row, _ := db.GetJob(job.ID); row.Status != database.StatusError || row.ChatStatus != database.ChatIncomplete {
		t.Fatalf("after the failure row = %s %q, want Error %q", row.Status, row.ChatStatus, database.ChatIncomplete)
	}
	if err := w.ResumeJob(job.ID); err != nil {
		t.Fatalf("ResumeJob = %v", err)
	}
	if row, _ := db.GetJob(job.ID); row.Status != database.StatusDownloading {
		t.Errorf("after Resume row = %s, want Downloading", row.Status)
	}
}

// TestAPanickedYouTubeRunSettlesItsChat: R-E1's settle on the panic Error
// paths. A YouTube run that panics is written to Error by recordRunPanic (or
// Start's recover, which has the job ID alone), not by setJobError, and so
// left "downloading" on the Error row — the symptom R-E1 removes. Both write
// through writePanicError, which settles an in-flight chat once its Error
// write applies; a row whose Error write did not apply (a Finished row) keeps
// its chat as it is.
//
// Mutant: writePanicError without the settle — the first two cases keep
// "downloading"/"pending".
func TestAPanickedYouTubeRunSettlesItsChat(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status database.JobStatus
		chat   database.ChatStatus
		viaID  bool // Start's recover: the job ID alone
		want   database.ChatStatus
	}{
		{"recordRunPanic, downloading", database.StatusDownloading, database.ChatDownloading, false, database.ChatIncomplete},
		{"Start's recover, pending", database.StatusDownloading, database.ChatPending, true, database.ChatNone},
		{"a row already Finished", database.StatusFinished, database.ChatDownloading, false, database.ChatDownloading},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			job := &database.Job{ID: "p", VideoID: "v", URL: "u", Platform: "youtube",
				Status: tc.status, ChatStatus: tc.chat}
			if _, err := db.AddJob(job); err != nil {
				t.Fatal(err)
			}
			if tc.viaID {
				w.writePanicError(job.ID, "boom")
			} else {
				w.recordRunPanic(job.ID, job, "boom")
			}
			row, _ := db.GetJob(job.ID)
			if row.ChatStatus != tc.want {
				t.Errorf("chat_status = %q (status %s), want %q", row.ChatStatus, row.Status, tc.want)
			}
		})
	}
}

// TestAPendingChatSettledOnErrorReachesAMuxAsNoChat pins the pending half of
// the Error settle (owner ruling R-E1a, the Cancel path's split): a YouTube
// download that failed before its chat received a batch reads "" — there is
// no capture to report — and an operator's Mux of that Error row carries ""
// to Finished (finalize writes chat_status only when staging holds a
// chat.json, which a pending capture does not), so the Finished embed has no
// chat line and the staging cleanup removes the dir rather than keeping it
// chat-only. Under R-E1 as first written (pending → incomplete) the same row
// reached Finished as "Chat: incomplete" with its staging kept chat-only.
//
// Mutant: settling pending to "incomplete" — every assertion here fails.
func TestAPendingChatSettledOnErrorReachesAMuxAsNoChat(t *testing.T) {
	w, db := testWorkerSetup(t)
	staging, _ := muxFixtureJob(t, w, db, "pm")
	db.UpdateJobFields("pm", map[string]any{"status": database.StatusDownloading, "chat_status": database.ChatPending})
	job, _ := db.GetJob("pm")
	w.setJobError(job, errors.New("download: reset"))
	if row, _ := db.GetJob("pm"); row.Status != database.StatusError || row.ChatStatus != database.ChatNone {
		t.Fatalf("after the failure row = %s %q, want Error %q", row.Status, row.ChatStatus, database.ChatNone)
	}
	// The Mux's finalize, with no chat.json in staging: the status moves,
	// chat_status is not written.
	db.UpdateJobFields("pm", map[string]any{"status": database.StatusFinished})
	fresh, _ := db.GetJob("pm")
	if fresh.ChatStatus != database.ChatNone {
		t.Errorf("Finished chat_status = %q, want %q", fresh.ChatStatus, database.ChatNone)
	}
	if v := decideStagingCleanup(db, fresh, "pm", staging); v.keep != removeStaging {
		t.Errorf("staging verdict = %v, want removeStaging", v.keep)
	}
}

// TestAResumedRunReArmsAnIncompleteChat: R-E1's "a later Resume re-arms it",
// past the Resume's own accept. After a failed YouTube download settled its
// chat to "incomplete", Resume moves the row on, and the resumed run's chat
// setup (setupChatDownloader — the call the orchestrator makes when
// download_chat is on) writes "pending" over the settled value, from which the
// capture's own writes go on to its verdict. With download_chat off no setup
// runs and the value stays "incomplete" to Finished, and the staging
// cleanup keeps the capture chat-only (the downloading half of R-E1a; the
// pending half reaches Finished as no chat,
// TestAPendingChatSettledOnErrorReachesAMuxAsNoChat).
//
// Mutant: setupChatDownloader not writing "pending" (or guarding the write
// on the settled value) — the resumed row keeps "incomplete".
func TestAResumedRunReArmsAnIncompleteChat(t *testing.T) {
	w, db := testWorkerSetup(t)
	job := &database.Job{ID: "rearm", VideoID: "vid", URL: "u", Platform: "youtube",
		Status: database.StatusDownloading, ChatStatus: database.ChatDownloading}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	w.setJobError(job, errors.New("download: reset"))
	if err := w.ResumeJob(job.ID); err != nil {
		t.Fatalf("ResumeJob = %v", err)
	}
	row, _ := db.GetJob(job.ID)
	if row.ChatStatus != database.ChatIncomplete {
		t.Fatalf("resumed row chat_status = %q before its run, want %q", row.ChatStatus, database.ChatIncomplete)
	}

	o := &DownloadOrchestrator{db: db, logger: discardLogger{}}
	jobCtx := &JobContext{Job: row, StagingDir: t.TempDir()}
	info := &youtube.VideoInfo{Chat: youtube.ChatSource{Continuation: "cont", FetchedAt: time.Now()}}
	dl := o.setupChatDownloader(context.Background(), jobCtx, info)
	if dl == nil {
		t.Fatal("setupChatDownloader set up no downloader")
	}
	if row, _ := db.GetJob(job.ID); row.ChatStatus != database.ChatPending {
		t.Errorf("after the resumed run's chat setup chat_status = %q, want %q", row.ChatStatus, database.ChatPending)
	}
}

// TestAFailedMuxSettlesAnInFlightChat: MuxJob writes its own Error (failed),
// not through setJobError, so a YouTube row that reached a Mux still showing
// an in-flight chat — an Error row from before the settle existed, or one
// whose settle a stray downloader's OnStart wrote over (the lost-Stop window
// in ChatDownloader.Start) — went on showing it on the Error row a failed Mux
// leaves. The Mux's failure write settles it by the same rule (R-E1a): a
// running capture becomes "incomplete", a pending one is cleared. A Twitch
// row settles the same way (owner ruling R-H1).
//
// Mutant: the Mux's failure write without the settle — the first two cases
// keep their in-flight value.
func TestAFailedMuxSettlesAnInFlightChat(t *testing.T) {
	for _, tc := range []struct {
		name     string
		platform string
		chat     database.ChatStatus
		want     database.ChatStatus
	}{
		{"downloading", "youtube", database.ChatDownloading, database.ChatIncomplete},
		{"pending", "youtube", database.ChatPending, database.ChatNone},
		{"a recorded verdict", "youtube", database.ChatFinished, database.ChatFinished},
		{"twitch settles as youtube does (R-H1)", "twitch", database.ChatDownloading, database.ChatIncomplete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			job := &database.Job{ID: "fm", VideoID: "fm", URL: "u", Platform: tc.platform,
				Status: database.StatusError, ChatStatus: tc.chat, OutputDirectory: t.TempDir()}
			if _, err := db.AddJob(job); err != nil {
				t.Fatal(err)
			}
			var stagingBase string
			w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
			staging := filepath.Join(stagingBase, "fm")
			if err := os.MkdirAll(staging, 0o755); err != nil {
				t.Fatal(err)
			}
			// A recording FFmpeg cannot read: the Mux starts and fails.
			if err := os.WriteFile(filepath.Join(staging, "video.mp4"), []byte("not a recording"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := w.MuxJob("fm"); err != nil {
				t.Fatalf("MuxJob = %v", err)
			}
			w.wg.Wait()
			row, _ := db.GetJob("fm")
			if row.Status != database.StatusError {
				t.Fatalf("after the failed Mux status = %s (error %q), want Error", row.Status, row.Error)
			}
			if row.ChatStatus != tc.want {
				t.Errorf("chat_status = %q, want %q", row.ChatStatus, tc.want)
			}
		})
	}
}

// TestStoppedChatStatusSettlesExactlyTheInFlightValues: the one settle rule
// (R-E1a) maps exactly the values database.ChatStatus.InFlight names, which
// is the from-set settleErrorChat iterates — running to "incomplete", pending
// to "" — and leaves every verdict alone.
func TestStoppedChatStatusSettlesExactlyTheInFlightValues(t *testing.T) {
	want := map[database.ChatStatus]database.ChatStatus{
		database.ChatDownloading: database.ChatIncomplete,
		database.ChatPending:     database.ChatNone,
	}
	for _, s := range database.ChatStatuses {
		to, ok := stoppedChatStatus(s)
		if ok != s.InFlight() {
			t.Errorf("stoppedChatStatus(%q) ok = %v, InFlight = %v", s, ok, s.InFlight())
		}
		if w, settles := want[s]; ok && (!settles || to != w) {
			t.Errorf("stoppedChatStatus(%q) = %q, want %q", s, to, w)
		}
	}
}
