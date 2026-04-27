package logger

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestConcurrentWriteAndRotate hammers the Write path with concurrent
// goroutines while rotation is forced via a tiny maxSize. The race
// detector should not flag any access to l.file / l.currentSize, and
// no goroutine should panic. Audit reports/small-packages.md logger
// concurrent Write+rotate.
func TestConcurrentWriteAndRotate(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "stress.log")

	l, err := New(logPath, "DEBUG", minLogRotationSize, 5)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })

	const writers = 8
	const perWriter = 200

	var wg sync.WaitGroup
	wg.Add(writers)
	for w := range writers {
		go func(id int) {
			defer wg.Done()
			for i := range perWriter {
				l.Info("stress write", "writer", id, "i", i, "filler", "padding text to grow line size")
			}
		}(w)
	}
	wg.Wait()

	// At least the current log file should exist after the storm.
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		t.Error("expected current log file to exist after concurrent writes")
	}
}

// TestRotateRenameFailureKeepsLoggerUsable simulates the rare path
// where os.Rename fails (e.g. file-lock contention on Windows). The
// logger MUST still successfully open a fresh file and accept further
// writes — failing silently here would lose every subsequent log
// line. Audit reports/small-packages.md logger rotate Rename failure.
func TestRotateRenameFailureKeepsLoggerUsable(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows handles open-file rename differently; on POSIX the
		// rename always succeeds even if a reader holds the file. The
		// assertion below ("post-rotate writes still land") is the
		// invariant we care about regardless of which branch fires.
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "rename-fail.log")

	l, err := New(logPath, "DEBUG", minLogRotationSize, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })

	// Create a directory at the rotation target name so os.Rename will
	// fail with "is a directory" / "file exists" — the rotate path's
	// non-IsNotExist branch.
	if err := os.MkdirAll(logPath+".1", 0o755); err != nil {
		t.Fatal(err)
	}

	// Force enough writes to trigger rotation.
	for i := range 60 {
		l.Info("write that will exceed maxSize and trigger rotation", "i", i, "padding", "filler")
	}

	// Post-rotate writes should still land. Use a fresh marker.
	const marker = "post-rotate-marker-9876"
	l.Info(marker)

	// Read current log file and verify the marker landed.
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !contains(body, []byte(marker)) {
		t.Errorf("post-rotate marker not found in log file (rename failure recovered?)")
	}
}

// TestRotateOpenFileFailureRecoversOnNextWrite covers the Write path's
// retry branch (logger.go:172-186): if rotate's reopen fails, the
// next Write should retry openFile rather than silently dropping. We
// trigger the reopen failure by making the parent dir read-only after
// rotation kicks off.
func TestRotateOpenFileFailureRecoversOnNextWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file-permissions semantics differ; tested implicitly via 786487b regression")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "openfail.log")

	l, err := New(logPath, "DEBUG", minLogRotationSize, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })

	// Step 1: write a few lines so the file exists.
	l.Info("seed line")

	// Step 2: chmod the dir to read-only so the next openFile inside
	// rotate fails, then trigger rotation via a flood of writes.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	for i := range 60 {
		l.Info("write that triggers rotation but reopen fails", "i", i, "padding", "filler")
	}

	// Step 3: restore permissions and write again. The retry-on-write
	// path should successfully reopen the file and the write should land.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const recovered = "recovered-after-reopen-failure-1234"
	l.Info(recovered)

	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log file after recovery: %v", err)
	}
	if !contains(body, []byte(recovered)) {
		t.Errorf("recovered marker not found in file — retry-on-write branch did not fire")
	}
}

// TestSubscriberDropDoesNotBlockBroadcast verifies that a wedged
// subscriber (one whose buffer fills because nobody reads) doesn't
// block broadcast(): the select default branch must drop the line
// and other subscribers must still receive subsequent lines.
//
// Subscribe returns a 100-buffer channel, so the test sends 200 lines
// to guarantee the wedged buffer fills mid-stream. Audit
// reports/small-packages.md logger subscriber drop.
func TestSubscriberDropDoesNotBlockBroadcast(t *testing.T) {
	l, err := New("", "INFO", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })

	wedged := l.Subscribe() // 100-buffer, never drained
	t.Cleanup(func() { l.Unsubscribe(wedged) })

	fast := l.Subscribe()
	t.Cleanup(func() { l.Unsubscribe(fast) })

	const lines = 200
	var received int
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		for line := range fast {
			_ = line
			mu.Lock()
			received++
			n := received
			mu.Unlock()
			if n >= lines {
				return
			}
		}
	}()

	for i := range lines {
		l.Info("broadcast line", "i", i)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		mu.Lock()
		got := received
		mu.Unlock()
		t.Fatalf("fast subscriber received %d/%d lines within 3s — wedged subscriber blocked broadcast",
			got, lines)
	}

	mu.Lock()
	got := received
	mu.Unlock()
	if got < lines {
		t.Errorf("fast subscriber received only %d lines, want %d", got, lines)
	}
}

// TestLogForJobConcurrentSameJobID hammers LogForJob with concurrent
// goroutines all targeting the same jobID. The double-checked-lock
// pattern in jobLogs initialisation must not lose lines (no zero-init
// race) and must not exceed maxJobLogLines (the prune trigger).
// Audit reports/small-packages.md logger LogForJob concurrent.
func TestLogForJobConcurrentSameJobID(t *testing.T) {
	l, err := New("", "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })

	const jobID = "job-stress"
	const writers = 8
	const perWriter = 100

	var wg sync.WaitGroup
	wg.Add(writers)
	for w := range writers {
		go func(id int) {
			defer wg.Done()
			for i := range perWriter {
				l.LogForJob(jobID, slog.LevelInfo, "stress", "writer", id, "i", i)
			}
		}(w)
	}
	wg.Wait()

	logs := l.GetJobLogs(jobID)
	// Expected exactly writers*perWriter = 800 lines, but the prune
	// trigger fires at maxJobLogLines (500) and removes 20% (100) at a
	// time. So the final count is somewhere in [500-100, maxJobLogLines]
	// = [400, 500]. Anything outside that range signals a race.
	if len(logs) > maxJobLogLines {
		t.Errorf("got %d job log lines; prune trigger should cap at %d", len(logs), maxJobLogLines)
	}
	if len(logs) < maxJobLogLines-maxJobLogLines/5 {
		t.Errorf("got %d job log lines; expected ≥ %d (signal of lost-line race)",
			len(logs), maxJobLogLines-maxJobLogLines/5)
	}
}

// contains is a tiny helper to avoid pulling in bytes for one search.
func contains(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
