package routes

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
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

// TestVerbsRefusedWhileAChapterImportRuns: Resume, Retry and Reinitialize
// answer 409 while a chapter import holds the job's claim, and change
// nothing (owner decision PR-M5): each starts what the import's re-mux must
// not race — a run whose finalize writes the archive the re-mux is about to
// rename its copy over, or a reset that deletes the parts and staging beside
// it. Once the import is done they are accepted again.
//
// Mutant: either handler not calling refuseWhileStagingClaimed (200, and the
// worker then holds the verb until the import ends).
func TestVerbsRefusedWhileAChapterImportRuns(t *testing.T) {
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

	f.addJob(t, "yt_importing", func(j *database.Job) { j.Status = database.StatusError })
	f.seedStaging(t, "yt_importing")
	release, err := w.ClaimChapterImport("yt_importing")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, verb := range []string{"resume", "retry", "reinitialize"} {
		if rec := doRequest(t, r, "POST", "/api/jobs/yt_importing/"+verb, nil); rec.Code != http.StatusConflict {
			t.Errorf("/%s during a chapter import = %d (%s), want 409", verb, rec.Code, rec.Body.String())
		}
	}
	if j, _ := f.db.GetJob("yt_importing"); j.Status != database.StatusError {
		t.Errorf("a refused verb changed the row: %s", j.Status)
	}
	release()
	if rec := doRequest(t, r, "POST", "/api/jobs/yt_importing/resume", nil); rec.Code != http.StatusOK {
		t.Errorf("/resume once the import is done = %d (%s), want 200", rec.Code, rec.Body.String())
	}
}

// TestVerbsRefusedWhileASetAsideRecoveryRuns: the same refusal for ANY
// operation holding the job's staging claim (owner ruling R-A2), naming it.
// /reinitialize during a recovery deleted the staging the recovery's FFmpeg
// was reading; /resume started a run that took its download slot.
//
// The recovery is held in place by a download pool of zero: it takes its
// claim and parks waiting for a slot, which is exactly the state a running
// recovery leaves.
//
// Mutant: refuseWhileStagingClaimed asking only for a chapter import (200,
// and the worker then holds the verb until the recovery ends).
func TestVerbsRefusedWhileASetAsideRecoveryRuns(t *testing.T) {
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

	f.addJob(t, "yt_recovering", func(j *database.Job) { j.Status = database.StatusError })
	stage := f.seedStaging(t, "yt_recovering")
	if err := os.WriteFile(filepath.Join(stage, "video.mp4"+engine.StagedRestartSuffix+"1700000000"), make([]byte, 64), 0o644); err != nil {
		t.Fatal(err)
	}
	w.SetParallelDownloads(0)
	// Registered after w.Stop, so it runs first: the parked recovery gets its
	// slot and ends, rather than Stop waiting its 10 s out on it.
	t.Cleanup(func() { w.SetParallelDownloads(1) })
	if err := w.RecoverAsides("yt_recovering"); err != nil {
		t.Fatalf("RecoverAsides: %v", err)
	}
	for _, verb := range []string{"resume", "retry", "reinitialize"} {
		rec := doRequest(t, r, "POST", "/api/jobs/yt_recovering/"+verb, nil)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "set-aside recovery") {
			t.Errorf("/%s during a set-aside recovery = %d (%s), want 409 naming the recovery", verb, rec.Code, rec.Body.String())
		}
	}
	if j, _ := f.db.GetJob("yt_recovering"); j.Status != database.StatusError {
		t.Errorf("a refused verb changed the row: %s", j.Status)
	}
	if _, err := os.Stat(stage); err != nil {
		t.Errorf("DATA LOSS: staging removed during the recovery: %v", err)
	}
}

// TestVerbAnswersConflictWhileAnotherIsPending: a verb the worker refuses
// because a different one is already waiting on the job (ErrResetPending)
// answers 409 with the worker's text, where it used to answer success for a
// click the worker had dropped (owner ruling R-A3).
func TestVerbAnswersConflictWhileAnotherIsPending(t *testing.T) {
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
	f.addJob(t, "yt_pending", func(j *database.Job) { j.Status = database.StatusError })
	f.seedStaging(t, "yt_pending")

	pending := fmt.Errorf("auto-reinitialize %w", worker.ErrResetPending)
	origResume, origReinit := resumeJob, reinitializeJob
	t.Cleanup(func() { resumeJob, reinitializeJob = origResume, origReinit })
	resumeJob = func(*worker.DownloadWorker, string) error { return pending }
	reinitializeJob = func(*worker.DownloadWorker, string) error { return pending }

	for _, verb := range []string{"resume", "retry", "reinitialize"} {
		rec := doRequest(t, r, "POST", "/api/jobs/yt_pending/"+verb, nil)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "auto-reinitialize already pending") {
			t.Errorf("/%s while another verb is pending = %d (%s), want 409 with the worker's text", verb, rec.Code, rec.Body.String())
		}
	}
}

// TestVerbAnswersByTheWorkersReason: the worker answers a repeat of the verb
// already on its way with ErrAlreadyPending, which is no refusal (owner
// ruling R-A3: a double-click is a quiet success), and a verb whose row went
// between the route's read and the worker's with ErrJobNotFound, which is
// the 404 the route gives a missing row itself — not the 500 of a failure.
//
// Mutant: refuseVerbError without either case (500 for both).
func TestVerbAnswersByTheWorkersReason(t *testing.T) {
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
	f.addJob(t, "yt_reason", func(j *database.Job) { j.Status = database.StatusError })
	f.seedStaging(t, "yt_reason")

	origResume, origReinit := resumeJob, reinitializeJob
	t.Cleanup(func() { resumeJob, reinitializeJob = origResume, origReinit })
	for _, tc := range []struct {
		name string
		err  error
		code int
	}{
		{"the same verb already on its way", fmt.Errorf("resume %w", worker.ErrAlreadyPending), http.StatusOK},
		{"the row gone", fmt.Errorf("resume: %w", worker.ErrJobNotFound), http.StatusNotFound},
		// The row moved on between the route's read and the worker's (a
		// monitor's resume, a finish): a conflict, not a fault.
		{"the row moved on", &worker.NotAllowedError{Verb: "resumed", Status: database.StatusDownloading}, http.StatusConflict},
	} {
		resumeJob = func(*worker.DownloadWorker, string) error { return tc.err }
		reinitializeJob = func(*worker.DownloadWorker, string) error { return tc.err }
		for _, verb := range []string{"resume", "retry", "reinitialize"} {
			if rec := doRequest(t, r, "POST", "/api/jobs/yt_reason/"+verb, nil); rec.Code != tc.code {
				t.Errorf("%s: /%s = %d (%s), want %d", tc.name, verb, rec.Code, rec.Body.String(), tc.code)
			}
		}
	}
}

// TestMuxAnswersByTheWorkersReason: the Mux route maps the worker's
// refusals — a staging another operation holds, a row that moved on since the
// route read it (ErrNotAllowed) — to 409 with the worker's text, and anything
// else to 500.
//
// Mutant: ErrNotAllowed left to the 500 default.
func TestMuxAnswersByTheWorkersReason(t *testing.T) {
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
	f.addJob(t, "yt_mux_reason", func(j *database.Job) { j.Status = database.StatusError })
	stage := filepath.Join(f.stagingDir, "yt_mux_reason")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "video.mp4"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	orig := muxJob
	t.Cleanup(func() { muxJob = orig })
	moved := &worker.NotAllowedError{Verb: "muxed", Status: database.StatusDownloading}
	for _, tc := range []struct {
		name string
		err  error
		code int
	}{
		{"the row moved on", moved, http.StatusConflict},
		{"the staging is busy", fmt.Errorf("%w (set-aside recovery in progress)", worker.ErrStagingBusy), http.StatusConflict},
		{"the row is gone", fmt.Errorf("mux: %w", worker.ErrJobNotFound), http.StatusNotFound},
		{"a fault", errors.New("boom"), http.StatusInternalServerError},
	} {
		muxJob = func(*worker.DownloadWorker, string) error { return tc.err }
		rec := doRequest(t, r, "POST", "/api/jobs/yt_mux_reason/mux", nil)
		if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.err.Error()) {
			t.Errorf("%s: /mux = %d (%s), want %d with the worker's text", tc.name, rec.Code, rec.Body.String(), tc.code)
		}
	}
}
