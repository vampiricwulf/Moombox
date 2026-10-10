package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
)

// terseErr wraps an error under a message that does not repeat it, so only
// errors.Is can see what is inside.
type terseErr struct {
	msg string
	err error
}

func (e terseErr) Error() string { return e.msg }
func (e terseErr) Unwrap() error { return e.err }

// TestIsDiskFullRecognisesTheWriteForms pins the shapes a full disk reaches
// the worker in: the engine's writes (engine.ErrLocalWrite wrapping the
// *os.PathError with %w), its HLS init write (the os error flattened with
// %v), an error that carries the platform's code under a message of its own,
// and FFmpeg's stderr tail. A failure that is not about space is not one.
//
// Mutants: drop the diskFullErrnos loop — the terse wrap is missed; drop the
// diskFullTexts loop — the flattened and FFmpeg forms are missed.
func TestIsDiskFullRecognisesTheWriteForms(t *testing.T) {
	for _, errno := range diskFullErrnos {
		pathErr := &os.PathError{Op: "write", Path: filepath.Join("staging", "j", "video.mp4"), Err: errno}
		for _, tc := range []struct {
			name string
			err  error
		}{
			{"engine write", fmt.Errorf("setup download: %w", fmt.Errorf("%w: write chunk: %w", engine.ErrLocalWrite, pathErr))},
			{"engine HLS init write, flattened", fmt.Errorf("%w: %w: %v", engine.ErrLocalWrite, errors.New("write HLS init segment"), pathErr)},
			{"the code under a message of its own", terseErr{msg: "flush staged recording", err: errno}},
		} {
			if !isDiskFull(tc.err) {
				t.Errorf("%s (%v): isDiskFull = false, want true for %q", tc.name, errno, tc.err)
			}
		}
	}

	ffmpeg := fmt.Errorf("mux: %w", fmt.Errorf("ffmpeg: %w (stderr: %s)", errors.New("exit status 1"),
		"[out#0/mp4 @ 0x5609da0cf740] Error writing trailer: No space left on device\nConversion failed!"))
	if !isDiskFull(ffmpeg) {
		t.Errorf("FFmpeg's stderr form: isDiskFull = false, want true for %q", ffmpeg)
	}
	for _, text := range []string{"There is not enough space on the disk.", "The disk is full."} {
		if err := fmt.Errorf("%w: write: %s", engine.ErrLocalWrite, text); !isDiskFull(err) {
			t.Errorf("Windows text %q: isDiskFull = false, want true", text)
		}
	}

	for _, err := range []error{
		nil,
		fmt.Errorf("%w: open output file: %w", engine.ErrLocalWrite, &os.PathError{Op: "open", Path: "video.mp4", Err: os.ErrPermission}),
		dialRefused,
		errors.New("full fetch failed: web API error: HTTP 404"),
		fmt.Errorf("mux: %w", errors.New("ffmpeg: exit status 1 (stderr: Invalid data found when processing input)")),
	} {
		if isDiskFull(err) {
			t.Errorf("isDiskFull(%v) = true for a failure that is not about space", err)
		}
	}
}

// TestIsDiskFullOnFFmpegWritingToAFullDisk runs the real muxer onto a full
// device, so the form isDiskFull reads is FFmpeg's own, not a transcription.
// Linux only: /dev/full answers every write with ENOSPC.
//
// Mutant: drop the diskFullTexts loop — FFmpeg's failure reads as something
// else.
func TestIsDiskFullOnFFmpegWritingToAFullDisk(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /dev/full")
	}
	ffmpegPath, _ := requireFFmpegTools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	writeMuxFixture(t, ffmpegPath, src, 1)
	out := filepath.Join(dir, "out.mp4")
	if err := os.Symlink("/dev/full", out); err != nil {
		t.Skipf("cannot link to /dev/full: %v", err)
	}

	err := engine.NewMuxer(ffmpegPath, discardLogger{}).MuxCopy(context.Background(), src, "", out)
	if err == nil {
		t.Fatal("muxing onto /dev/full succeeded")
	}
	if !isDiskFull(fmt.Errorf("mux: %w", err)) {
		t.Errorf("isDiskFull = false for FFmpeg's failure on a full disk: %v", err)
	}
}

// diskFullVodJob adds an admitted VOD of channel UC_full with its output
// directory, and its feed_items partner when backlog.
func diskFullVodJob(t *testing.T, db *database.Database, id string, priority int) {
	t.Helper()
	ch := "UC_full"
	if _, err := db.AddJob(&database.Job{
		ID: id, VideoID: id, URL: "u", Platform: "youtube", Status: database.StatusUpcoming,
		ChannelID: &ch, QueuePriority: priority, OutputDirectory: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	if priority == 1 {
		addFeedItemRow(t, db, ch, id, "2026-07-10T00:00:00Z")
	}
}

// TestBacklogDownloadOutOfDiskRequeuesThenErrors is D-disk's second half
// through processJob and the real engine: a backlog VOD whose download fails
// because the disk is full goes back to Queued, held for a backoff, and only
// once its retries are spent does it end in Error, saying so. Each of those
// runs fetches successfully before its download fails, so the count must
// outlive a successful fetch. A VOD that is not backlog ends in Error at
// once. Linux only: the staged file is a link to /dev/full.
//
// Mutants: drop the requeue from processJob's download failure — the first
// failure is an Error; reset the retry count after a successful fetch (where
// it was reset before) — the job never reaches Error.
func TestBacklogDownloadOutOfDiskRequeuesThenErrors(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /dev/full")
	}
	body := make([]byte, 64<<10)
	srv := serveWholeFile(t, body)

	run := func(t *testing.T, w *DownloadWorker, id string) *database.Job {
		t.Helper()
		var stagingBase string
		w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
		staged := filepath.Join(stagingBase, id, "video.mp4")
		if err := os.MkdirAll(filepath.Dir(staged), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(staged); err != nil {
			if err := os.Symlink("/dev/full", staged); err != nil {
				t.Skipf("cannot link to /dev/full: %v", err)
			}
		}
		w.processJob(context.Background(), id)
		row, _ := w.db.GetJob(id)
		return row
	}

	t.Run("backlog", func(t *testing.T) {
		w, db := testWorkerSetup(t)
		w.orchestrator.routedCipher = stubCipherSolver{}
		w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
			return &StreamProcessResult{ShouldDownload: true, IsVod: true,
				VideoInfo: vodInfoAt(srv.URL, time.Now().Add(6*time.Hour).Unix(), len(body))}, nil
		}
		diskFullVodJob(t, db, "full_vod", 1)

		for attempt := 1; attempt <= backlogRetryLimit; attempt++ {
			row := run(t, w, "full_vod")
			if row.Status != database.StatusQueued {
				t.Fatalf("attempt %d: status = %s (%q), want Queued", attempt, row.Status, row.Error)
			}
			if !w.scheduler.held("full_vod", time.Now()) {
				t.Fatalf("attempt %d: the requeued job is not held from re-admission", attempt)
			}
			if n := w.queue.ActiveCount(); n != 0 {
				t.Fatalf("attempt %d: slots still held after the requeue: %d", attempt, n)
			}
			// The scheduler's re-admission, once the hold is over.
			db.UpdateJobFields("full_vod", map[string]any{"status": database.StatusUpcoming})
		}
		row := run(t, w, "full_vod")
		if row.Status != database.StatusError || !strings.Contains(row.Error, "gave up after") {
			t.Errorf("after the budget: status = %s (%q), want Error naming the spent retries", row.Status, row.Error)
		}
	})

	t.Run("not backlog", func(t *testing.T) {
		w, db := testWorkerSetup(t)
		w.orchestrator.routedCipher = stubCipherSolver{}
		w.processStreamFn = func(context.Context, *database.Job) (*StreamProcessResult, error) {
			return &StreamProcessResult{ShouldDownload: true, IsVod: true,
				VideoInfo: vodInfoAt(srv.URL, time.Now().Add(6*time.Hour).Unix(), len(body))}, nil
		}
		diskFullVodJob(t, db, "full_manual", 0)
		row := run(t, w, "full_manual")
		if row.Status != database.StatusError {
			t.Errorf("status = %s (%q), want Error for a VOD that is not backlog", row.Status, row.Error)
		}
		// And it failed for the disk, which is what the backlog half's
		// requeues were requeueing.
		if !isDiskFull(errors.New(row.Error)) {
			t.Errorf("the download failed with %q, not for want of space", row.Error)
		}
	})
}
