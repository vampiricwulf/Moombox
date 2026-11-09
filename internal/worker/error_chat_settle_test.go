package worker

import (
	"errors"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestYouTubeErrorSettlesAnInFlightChat (owner ruling R-E1): the
// orchestrator's error arm stops the chat downloader without recording a
// verdict, so a failed YouTube download left "downloading" (or "pending") on
// its Error row and the Web badge showed a capture in progress. setJobError
// now settles an in-flight value to "incomplete"; a recorded verdict, a
// Twitch row and a COOKIES? park are left as they were.
//
// Mutant: dropping the settleErrorChat call — the first two rows keep their
// in-flight value.
func TestYouTubeErrorSettlesAnInFlightChat(t *testing.T) {
	for _, tc := range []struct {
		name     string
		platform string
		chat     database.ChatStatus
		err      error
		want     database.ChatStatus
	}{
		{"downloading", "youtube", database.ChatDownloading, errors.New("download: reset"), database.ChatIncomplete},
		{"pending", "youtube", database.ChatPending, errors.New("download: reset"), database.ChatIncomplete},
		{"finished verdict", "youtube", database.ChatFinished, errors.New("mux: failed"), database.ChatFinished},
		{"unavailable verdict", "youtube", database.ChatUnavailable, errors.New("download: reset"), database.ChatUnavailable},
		{"no chat", "youtube", database.ChatNone, errors.New("download: reset"), database.ChatNone},
		{"twitch is not in scope", "twitch", database.ChatDownloading, errors.New("download: reset"), database.ChatDownloading},
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
