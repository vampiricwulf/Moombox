package routes

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/web"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// TestRetryContinuesWhileReinitializeStartsOver drives both verbs through a
// real worker: /retry continues the recording — the row goes to Downloading
// and its staging stays — while /reinitialize starts over and deletes it.
//
// Mutant: /retry's handler calling w.ReinitializeJob instead of w.ResumeJob
// (staging deleted, the row not Downloading).
func TestRetryContinuesWhileReinitializeStartsOver(t *testing.T) {
	f := newJobsFixture(t)
	cfg := config.Defaults()
	cfg.Paths.StagingDirectory = f.stagingDir
	cfg.Paths.OutputDirectory = f.outputDir
	w := worker.NewDownloadWorker(f.db, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	t.Cleanup(w.Stop)
	rl := web.NewRateLimiter(1000, time.Minute)
	t.Cleanup(rl.Close)
	r := chi.NewRouter()
	JobRoutes(r, f.db, f.store, w, rl, f.tw, f.yt, f.notify)

	f.addJob(t, "yt_retry", func(j *database.Job) { j.Status = database.StatusError })
	retryStage := f.seedStaging(t, "yt_retry")
	f.addJob(t, "yt_reinit", func(j *database.Job) { j.Status = database.StatusError })
	reinitStage := f.seedStaging(t, "yt_reinit")

	if rec := doRequest(t, r, "POST", "/api/jobs/yt_retry/retry", nil); rec.Code != http.StatusOK {
		t.Fatalf("retry = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if rec := doRequest(t, r, "POST", "/api/jobs/yt_reinit/reinitialize", nil); rec.Code != http.StatusOK {
		t.Fatalf("reinitialize = %d (%s), want 200", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		rj, _ := f.db.GetJob("yt_retry")
		ij, _ := f.db.GetJob("yt_reinit")
		_, reinitErr := os.Stat(reinitStage)
		if rj.Status != database.StatusError && ij.Status != database.StatusError && os.IsNotExist(reinitErr) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("verbs did not apply: retry %s, reinitialize %s (staging err %v)", rj.Status, ij.Status, reinitErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rj, _ := f.db.GetJob("yt_retry"); rj.Status != database.StatusDownloading {
		t.Errorf("after /retry the row is %s, want Downloading (resumed)", rj.Status)
	}
	if _, err := os.Stat(retryStage); err != nil {
		t.Errorf("/retry removed the staging it should continue from: %v", err)
	}
}
