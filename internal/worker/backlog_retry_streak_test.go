package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// backlogRetryCount is jobID's current requeue streak.
func (w *DownloadWorker) backlogRetryCount(jobID string) int {
	w.backlogRetryMu.Lock()
	defer w.backlogRetryMu.Unlock()
	return w.backlogRetries[jobID]
}

// TestBacklogRetryStreakEndsWithTheRun: the retry budget counts a job's runs
// that ended back in Queued, and a run that ends any other way — in Error,
// cancelled, or finished — starts the next streak from nothing. (A fetch that
// succeeds no longer ends one: a download that then runs out of disk follows
// one every time, and the budget would never be spent.)
//
// Mutants: drop the forgetBacklogRetries from setJobError — the Error run
// leaves the streak at 1; from handleCancellation — the cancelled run does;
// from processJob's finished path — the finished run does.
func TestBacklogRetryStreakEndsWithTheRun(t *testing.T) {
	startStreak := func(t *testing.T, w *DownloadWorker, db *database.Database, id string) {
		t.Helper()
		w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
			return nil, dialRefused
		}
		w.processJob(context.Background(), id)
		if n := w.backlogRetryCount(id); n != 1 {
			t.Fatalf("streak = %d after one requeue, want 1", n)
		}
		db.UpdateJobFields(id, map[string]any{"status": database.StatusUpcoming})
	}

	t.Run("an Error", func(t *testing.T) {
		w, db := testWorkerSetup(t)
		backlogRetryJob(t, db, "streak_err", 1, true)
		startStreak(t, w, db, "streak_err")
		w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
			return nil, errors.New("full fetch failed: web API error: HTTP 404")
		}
		w.processJob(context.Background(), "streak_err")
		if n := w.backlogRetryCount("streak_err"); n != 0 {
			t.Errorf("streak = %d after a run that ended in Error, want 0", n)
		}
	})

	t.Run("a cancel", func(t *testing.T) {
		w, db := testWorkerSetup(t)
		backlogRetryJob(t, db, "streak_cancel", 1, true)
		startStreak(t, w, db, "streak_cancel")
		ctx, cancel := context.WithCancel(context.Background())
		w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
			cancel()
			return nil, context.Canceled
		}
		w.processJob(ctx, "streak_cancel")
		if n := w.backlogRetryCount("streak_cancel"); n != 0 {
			t.Errorf("streak = %d after a cancelled run, want 0", n)
		}
	})

	t.Run("a finish", func(t *testing.T) {
		ffmpegPath, _ := requireFFmpegTools(t)
		w, db := testWorkerSetup(t)
		w.orchestrator.SetFfmpegPath(ffmpegPath)
		w.orchestrator.routedCipher = stubCipherSolver{}
		diskFullVodJob(t, db, "streak_done", 1)
		startStreak(t, w, db, "streak_done")

		full := filepath.Join(t.TempDir(), "full.mp4")
		writeMuxFixture(t, ffmpegPath, full, 2)
		body, err := os.ReadFile(full)
		if err != nil {
			t.Fatal(err)
		}
		srv := serveWholeFile(t, body)
		w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
			return &StreamProcessResult{ShouldDownload: true, IsVod: true,
				VideoInfo: vodInfoAt(srv.URL, time.Now().Add(6*time.Hour).Unix(), len(body))}, nil
		}
		w.processJob(context.Background(), "streak_done")
		if row, _ := db.GetJob("streak_done"); row.Status != database.StatusFinished {
			t.Fatalf("status = %s (%q), want Finished", row.Status, row.Error)
		}
		if n := w.backlogRetryCount("streak_done"); n != 0 {
			t.Errorf("streak = %d after a run that finished, want 0", n)
		}
	})
}
