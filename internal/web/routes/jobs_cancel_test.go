package routes

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/web"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// TestCancelRouteReportsAJobThatEndedFirst: the route reads the row, checks
// it is cancellable and writes Cancelled — and a job that finishes between
// the read and the write was turned into a Cancelled one, answered 200 and
// announced as cancelled. The write leaves an outcome alone now, and the
// route says nothing was cancelled: a 409 naming the status the job reached,
// and no "Job Cancelled". With a worker (production) and without one (the
// route's own write).
//
// Mutants: the worker's CancelJob writing with UpdateJobFields, or the
// worker-less write likewise — 200 and the Finished row turns Cancelled;
// ignore cancelJob's answer in the route — 200 and a Job Cancelled for a job
// nothing cancelled.
func TestCancelRouteReportsAJobThatEndedFirst(t *testing.T) {
	for _, withWorker := range []bool{false, true} {
		name := "no worker"
		if withWorker {
			name = "worker"
		}
		t.Run(name, func(t *testing.T) {
			f := newJobsFixture(t)
			router := f.router
			if withWorker {
				w := worker.NewDownloadWorker(f.db, nil, config.Defaults(), slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
				t.Cleanup(w.Stop)
				rl := web.NewRateLimiter(1000, time.Minute)
				t.Cleanup(rl.Close)
				r := chi.NewRouter()
				JobRoutes(r, f.db, f.store, w, rl, f.tw, f.yt, f.notify)
				router = r
			}
			f.addJob(t, "ends", func(j *database.Job) { j.Status = database.StatusDownloading })

			orig := cancelJob
			t.Cleanup(func() { cancelJob = orig })
			cancelJob = func(db *database.Database, w *worker.DownloadWorker, id string) (bool, bool) {
				// The download finishes as the operator clicks Cancel.
				db.UpdateJobFields(id, map[string]any{"status": database.StatusFinished})
				return orig(db, w, id)
			}

			rec := doRequest(t, router, "POST", "/api/jobs/ends/cancel", nil)
			if rec.Code != http.StatusConflict {
				t.Errorf("POST cancel = %d (%s), want 409", rec.Code, rec.Body.String())
			}
			if body := rec.Body.String(); !strings.Contains(body, "Not cancelled") || !strings.Contains(body, "Finished") {
				t.Errorf("body = %s, want it to say the job is Finished and was not cancelled", body)
			}
			if row, _ := f.db.GetJob("ends"); row.Status != database.StatusFinished {
				t.Errorf("status = %s, want the Finished it reached", row.Status)
			}
			if n := len(f.notify.ByEvent("cancelled")); n != 0 {
				t.Errorf("sent %d Job Cancelled for a job nothing cancelled", n)
			}
		})
	}
}
