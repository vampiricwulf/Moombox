package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// stagedFile writes n bytes of staged "recording" to a fresh temp file and
// returns its path. Every row below starts from a non-empty staged file with
// NO resume sidecar beside it — the exact shape ENGINE-1/ENGINE-5 destroy.
func stagedFile(t *testing.T, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "video_stream")
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatalf("write staged file: %v", err)
	}
	return path
}

func sizeOf(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}

// TestStartRefusesToTruncateStagedMedia pins the shared no-truncate guard:
// a segmented download that finds staged bytes it cannot resume must return
// ErrStagedMediaPresent with the file untouched, never open it O_TRUNC.
//
// Mutant: restoring the bare `flags |= os.O_TRUNC` else-branch in Start (i.e.
// deleting the guard) — Start returns nil and the staged file is 0 bytes.
func TestStartRefusesToTruncateStagedMedia(t *testing.T) {
	path := stagedFile(t, 1<<20)
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    "http://127.0.0.1:1/videoplayback?itag=140",
		OutputFile: path,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := d.Start(ctx)
	if !errors.Is(err, ErrStagedMediaPresent) {
		t.Fatalf("Start = %v, want ErrStagedMediaPresent", err)
	}
	if got := sizeOf(t, path); got != 1<<20 {
		t.Fatalf("staged file is %d bytes after Start, want 1048576 (the guard must not truncate)", got)
	}
}

// TestStartDiscardStagedMediaOptIn pins the escape hatch: a caller that has
// explicitly decided the staged bytes are disposable still gets the fresh
// O_TRUNC file it asked for.
//
// Mutant: dropping the `!d.opts.DiscardStaged` term from the guard — Start
// returns ErrStagedMediaPresent and the manifest-free post-live restart,
// which MUST begin at sq=0, can never run.
func TestStartDiscardStagedMediaOptIn(t *testing.T) {
	path := stagedFile(t, 4096)
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:       "http://127.0.0.1:1/videoplayback?itag=140",
		OutputFile:    path,
		DiscardStaged: true,
		MaxRetries:    1,
	})
	d.delays = fastDelays()

	// The open mode is decided before the first fetch, so the download's own
	// failure against a dead address is irrelevant — only the file is.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Start(ctx); errors.Is(err, ErrStagedMediaPresent) {
		t.Fatalf("Start = %v, want the guard to stand down for an explicit discard", err)
	}
	if got := sizeOf(t, path); got != 0 {
		t.Fatalf("staged file is %d bytes after an explicit discard, want 0", got)
	}
}

// TestStartDirectURLKeepsLegacyTruncate pins the guard's scope: whole-file
// direct downloads are NOT segmented staged media and keep their pre-arc
// restart-from-byte-0 behaviour (their partial loss is bounded by the
// 50 MB sidecar cadence, and Task 4 removes the truncation they actually hit).
//
// Mutant: widening the guard to IsDirectURL — a half-downloaded VOD whose
// sidecar was lost errors instead of restarting.
func TestStartDirectURLKeepsLegacyTruncate(t *testing.T) {
	path := stagedFile(t, 4096)
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:     "http://127.0.0.1:1/videoplayback?itag=140",
		OutputFile:  path,
		IsDirectURL: true,
		MaxRetries:  1,
	})
	d.delays = fastDelays()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Start(ctx); errors.Is(err, ErrStagedMediaPresent) {
		t.Fatalf("Start = %v, want the guard to ignore IsDirectURL downloads", err)
	}
}

// errSharingViolation stands in for the scanner/indexer refusal the ladder
// exists to ride out. The tests drive the classifier seam rather than a real
// OS errno so one fixture describes both platforms.
var errSharingViolation = errors.New("the process cannot access the file because it is being used by another process")

// installTruncateSeams points the ladder at a scripted truncate, a no-op pause
// and a classifier that treats errSharingViolation (and nothing else) as
// transient. Restores everything at the end of the test.
func installTruncateSeams(t *testing.T, trunc func(string, int64) error) {
	t.Helper()
	prevTrunc, prevSleep, prevClass := truncateFile, truncateRetrySleep, isTransientTruncateError
	t.Cleanup(func() {
		truncateFile, truncateRetrySleep, isTransientTruncateError = prevTrunc, prevSleep, prevClass
	})
	truncateFile = trunc
	truncateRetrySleep = func(context.Context, time.Duration) error { return nil }
	isTransientTruncateError = func(err error) bool { return errors.Is(err, errSharingViolation) }
}

// TestTruncateForResumeRetriesThenFails pins the retry ladder that replaces
// ENGINE-5's silent `starting fresh` fallback: the truncate is re-attempted
// through the Windows AV/indexer sharing-violation window, and the last error
// is RETURNED rather than swallowed into an O_TRUNC.
//
// Mutant: making truncateForResume a single os.Truncate call — attempts is 1
// and a window that clears on the third try is never seen.
func TestTruncateForResumeRetriesThenFails(t *testing.T) {
	path := stagedFile(t, 1024)

	attempts := 0
	installTruncateSeams(t, func(name string, size int64) error {
		attempts++
		if attempts < 3 {
			return errSharingViolation
		}
		return os.Truncate(name, size)
	})

	if err := truncateForResume(context.Background(), path, 512); err != nil {
		t.Fatalf("truncateForResume = %v, want nil once the window clears", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (the ladder must retry, not give up on the first refusal)", attempts)
	}
	if got := sizeOf(t, path); got != 512 {
		t.Fatalf("file is %d bytes, want 512", got)
	}
}

// TestTruncateForResumeReturnsPermanentErrorImmediately pins the classifier
// the ladder claims to share with utils.ReplaceFile: only the scanner/indexer
// refusal is waited out. A missing file, a directory in its place or a
// read-only volume is permanent, and sleeping the full 1270 ms before saying
// so delays every caller — including the Twitch split path — for nothing.
//
// Mutant: dropping the `|| !isTransientTruncateError(err)` term — attempts
// climbs to the ladder's full 8.
func TestTruncateForResumeReturnsPermanentErrorImmediately(t *testing.T) {
	permanent := errors.New("read-only file system")

	attempts := 0
	installTruncateSeams(t, func(string, int64) error {
		attempts++
		return permanent
	})

	err := truncateForResume(context.Background(), stagedFile(t, 16), 8)
	if !errors.Is(err, permanent) {
		t.Fatalf("truncateForResume = %v, want the permanent error", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (a permanent error must not be retried)", attempts)
	}
}

// TestTruncateForResumeStopsOnContextCancel pins the shutdown behaviour: a
// cancelled context ends the ladder at once and surfaces the last refusal,
// instead of sleeping out the remaining seven pauses inside Start while the
// process is trying to exit.
//
// Mutant: ignoring truncateRetrySleep's error — attempts runs to 8.
func TestTruncateForResumeStopsOnContextCancel(t *testing.T) {
	attempts := 0
	prevTrunc, prevSleep, prevClass := truncateFile, truncateRetrySleep, isTransientTruncateError
	t.Cleanup(func() {
		truncateFile, truncateRetrySleep, isTransientTruncateError = prevTrunc, prevSleep, prevClass
	})
	// The REAL pause, so the cancelled context is what stops the ladder.
	truncateRetrySleep = utils.Sleep
	isTransientTruncateError = func(err error) bool { return errors.Is(err, errSharingViolation) }
	truncateFile = func(string, int64) error {
		attempts++
		return errSharingViolation
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := truncateForResume(ctx, stagedFile(t, 16), 8); !errors.Is(err, errSharingViolation) {
		t.Fatalf("truncateForResume = %v, want the last refusal surfaced", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (a dead context must end the ladder, not wait it out)", attempts)
	}
}

// TestStartResumeTruncateFailureKeepsStagedMedia closes door (b) at the
// behavioural level: when the resume truncate cannot be performed even after
// the retry ladder, Start RETURNS the error. The staged recording and its
// sidecar are both left exactly as they were, so the job stays resumable.
//
// Mutant: restoring ENGINE-5's fallthrough (`resuming = false; flags =
// os.O_CREATE|os.O_WRONLY|os.O_TRUNC; state = nil; …`) after the failed
// truncate — Start proceeds, the 1 MiB staged file is reopened O_TRUNC and
// this test reports 0 bytes on disk.
func TestStartResumeTruncateFailureKeepsStagedMedia(t *testing.T) {
	const streamURL = "http://127.0.0.1:1/videoplayback?id=abcdefghijk.1&itag=140"
	path := stagedFile(t, 1<<20)
	writeResumeSidecar(t, path, streamURL, 512<<10)

	// A sharing violation that never clears — AV holding the handle. The
	// ladder exhausts and the error must surface.
	installTruncateSeams(t, func(string, int64) error { return errSharingViolation })

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    streamURL,
		OutputFile: path,
		MaxRetries: 1,
	})
	d.delays = fastDelays()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	startErr := d.Start(ctx)
	if !errors.Is(startErr, ErrTruncateBlocked) {
		t.Fatalf("Start = %v, want ErrTruncateBlocked", startErr)
	}
	if !strings.Contains(startErr.Error(), "truncate for resume") {
		t.Fatalf("Start = %v, want the message to name the truncate", startErr)
	}
	if got := sizeOf(t, path); got != 1<<20 {
		t.Fatalf("staged file is %d bytes, want 1048576 (a failed truncate must never fall through to O_TRUNC)", got)
	}
	if _, statErr := os.Stat(path + ".resume.json"); statErr != nil {
		t.Fatalf("resume sidecar gone (%v) — the job must stay resumable", statErr)
	}
}

// writeResumeSidecar drops a valid-looking sidecar beside path so Start takes
// the resume branch and reaches the truncate.
func writeResumeSidecar(t *testing.T, path, baseURL string, bytesWritten int64) {
	t.Helper()
	blob, err := json.Marshal(ResumeState{
		LastSeq:      41,
		BytesWritten: bytesWritten,
		Timestamp:    time.Now().Unix(),
		BaseURL:      baseURL,
	})
	if err != nil {
		t.Fatalf("marshal resume state: %v", err)
	}
	if err := os.WriteFile(path+".resume.json", blob, 0o644); err != nil {
		t.Fatalf("write resume sidecar: %v", err)
	}
}

// TestStartResumeTruncateFailureSplitsWithoutClaimingDataLoss pins the Twitch
// half of door (b): a StopOnGap caller still splits (ErrGapDetected, so the
// current file is muxed as a finished part), but the error ALSO carries
// ErrTruncateBlocked so the orchestrator can tell the operator the truth —
// nothing expired from the CDN, a truncate was simply refused.
//
// Mutant: returning a bare ErrGapDetected — the Twitch loop sends a
// "segments were lost" notification for a split that lost nothing.
func TestStartResumeTruncateFailureSplitsWithoutClaimingDataLoss(t *testing.T) {
	const streamURL = "http://127.0.0.1:1/videoplayback?id=abcdefghijk.1&itag=140"
	path := stagedFile(t, 1<<20)
	writeResumeSidecar(t, path, streamURL, 512<<10)
	installTruncateSeams(t, func(string, int64) error { return errSharingViolation })

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    streamURL,
		OutputFile: path,
		StopOnGap:  true,
		MaxRetries: 1,
	})
	d.delays = fastDelays()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := d.Start(ctx)
	if !errors.Is(err, ErrGapDetected) {
		t.Fatalf("Start = %v, want ErrGapDetected so the part is muxed and a fresh one begins", err)
	}
	if !errors.Is(err, ErrTruncateBlocked) {
		t.Fatalf("Start = %v, want ErrTruncateBlocked alongside the split so no data-loss notification is sent", err)
	}
	if got := sizeOf(t, path); got != 1<<20 {
		t.Fatalf("staged file is %d bytes, want 1048576 (the split must not truncate either)", got)
	}
}
