package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestReinitializePreviewRoute pins the preflight both UIs read before they
// confirm a Reinitialize (owner decision PR-M1): GET
// /api/jobs/{id}/reinitialize/preview answers worker.PreviewReinitialize's
// counts flat beside allowed — the status gate POST /reinitialize applies —
// and 404 for a job that is not there.
//
// Mutants: the route not registered (404 for j1); allowed not following the
// status (true for the Finished row); the counts left out (no
// setAsideRecordings).
func TestReinitializePreviewRoute(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "j1", func(j *database.Job) { j.Status = database.StatusCancelled })
	f.addJob(t, "j2", func(j *database.Job) { j.Status = database.StatusFinished })
	stageAside(t, f, "j1", "1700000000")

	var got struct {
		Allowed            bool  `json:"allowed"`
		SetAsideRecordings *int  `json:"setAsideRecordings"`
		StagedFiles        int   `json:"stagedFiles"`
		StagedBytes        int64 `json:"stagedBytes"`
	}
	body := f.getJSON(t, "/api/jobs/j1/reinitialize/preview", &got)
	if !got.Allowed || got.SetAsideRecordings == nil || *got.SetAsideRecordings != 1 || got.StagedFiles != 1 || got.StagedBytes != 64 {
		t.Errorf("preview of a Cancelled job with one set-aside = %s, want allowed with 1 set-aside, 1 file of 64 bytes", body)
	}

	got.Allowed = true
	f.getJSON(t, "/api/jobs/j2/reinitialize/preview", &got)
	if got.Allowed {
		t.Error("a Finished job's preview says allowed; POST /reinitialize refuses it")
	}

	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/jobs/nope/reinitialize/preview", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("preview of a missing job = %d, want 404", rec.Code)
	}
}

// TestReinitializePreviewAllowedMatchesThePost pins that the preview's allowed
// is the POST's own gate on every status: a confirm that the preview allows
// is never refused by the POST, and one it refuses would have been refused.
//
// Mutants: COOKIES? left out of allowed (the COOKIES? row disagrees);
// Finished let through by the POST only (the Finished row disagrees).
func TestReinitializePreviewAllowedMatchesThePost(t *testing.T) {
	f := newJobsFixture(t)
	for _, st := range []database.JobStatus{
		database.StatusQueued, database.StatusUpcoming, database.StatusLive, database.StatusDownloading,
		database.StatusMuxing, database.StatusFinished, database.StatusError, database.StatusCancelled, database.StatusCookies,
	} {
		id := "s-" + strings.ReplaceAll(string(st), "?", "")
		f.addJob(t, id, func(j *database.Job) { j.Status = st })
		var got struct {
			Allowed bool `json:"allowed"`
		}
		f.getJSON(t, "/api/jobs/"+id+"/reinitialize/preview", &got)
		rec := f.post(t, "/api/jobs/"+id+"/reinitialize", "")
		if posted := rec.Code == http.StatusOK; posted != got.Allowed {
			t.Errorf("%s: preview allowed = %v, POST /reinitialize answered %d", st, got.Allowed, rec.Code)
		}
		want := st == database.StatusError || st == database.StatusCancelled || st == database.StatusCookies
		if got.Allowed != want {
			t.Errorf("%s: allowed = %v, want %v", st, got.Allowed, want)
		}
	}
}

// TestPendingAutoActionReachesTheDetailsAndThePreview pins owner ruling
// R-D2 on the wire: GET /api/jobs/{id} carries the row's pending automatic
// action (jobstate.PendingAutoAction) as pendingAutoAction, absent when there
// is none, and the Reinitialize preview counts it, so the dashboard's confirm
// says the reset cancels it.
//
// Mutants: enrichJob not setting PendingAutoAction; PreviewReinitialize not
// counting it.
func TestPendingAutoActionReachesTheDetailsAndThePreview(t *testing.T) {
	f := newJobsFixture(t)
	f.addJob(t, "tw_mux", func(j *database.Job) {
		j.Platform, j.Status, j.Error = "twitch", database.StatusError, "twitch HLS: 500"
	})
	// AddJob does not write park_reason; the latch's failure write does.
	f.db.UpdateJobFields("tw_mux", map[string]any{"park_reason": database.ParkReasonTwitchEndUnconfirmed})
	f.addJob(t, "plain", func(j *database.Job) { j.Status, j.Error = database.StatusError, "boom" })

	var details struct {
		PendingAutoAction *string `json:"pendingAutoAction"`
	}
	body := f.getJSON(t, "/api/jobs/tw_mux", &details)
	if details.PendingAutoAction == nil || *details.PendingAutoAction != "auto_mux" {
		t.Errorf("details of an end-unconfirmed row = %s, want pendingAutoAction auto_mux", body)
	}
	details.PendingAutoAction = nil
	body = f.getJSON(t, "/api/jobs/plain", &details)
	if details.PendingAutoAction != nil || strings.Contains(body, "pendingAutoAction") {
		t.Errorf("details of a plain Error row carry pendingAutoAction: %s", body)
	}

	var prev struct {
		PendingAutoMux   int `json:"pendingAutoMux"`
		PendingAutoRetry int `json:"pendingAutoRetry"`
	}
	body = f.getJSON(t, "/api/jobs/tw_mux/reinitialize/preview", &prev)
	if prev.PendingAutoMux != 1 || prev.PendingAutoRetry != 0 {
		t.Errorf("preview of an end-unconfirmed row = %s, want pendingAutoMux 1", body)
	}
}
