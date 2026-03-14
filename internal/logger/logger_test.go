package logger

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewLogger(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	l, err := New(logPath, "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	l.Info("test message")
	l.Debug("debug message", "key", "value")

	// Verify file contains the actual messages we logged
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "test message") {
		t.Errorf("expected log file to contain 'test message', got:\n%s", content)
	}
	if !strings.Contains(content, "debug message") {
		t.Errorf("expected log file to contain 'debug message', got:\n%s", content)
	}
	if !strings.Contains(content, "key=value") {
		t.Errorf("expected log file to contain 'key=value', got:\n%s", content)
	}
}

func TestRingBuffer(t *testing.T) {
	l, err := New("", "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Fill ring buffer past capacity
	for i := 0; i < 250; i++ {
		l.Info("message", "i", i)
	}

	lines := l.GetRecentLines()
	if len(lines) != defaultRingSize {
		t.Errorf("expected %d lines, got %d", defaultRingSize, len(lines))
	}
}

func TestRingBufferPartialFill(t *testing.T) {
	l, err := New("", "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Log fewer messages than ring size
	for i := 0; i < 5; i++ {
		l.Info("partial", "i", i)
	}

	lines := l.GetRecentLines()
	if len(lines) != 5 {
		t.Errorf("expected 5 lines, got %d", len(lines))
	}
}

func TestRingBufferOrder(t *testing.T) {
	l, err := New("", "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Overfill and check that we get the most recent entries in order
	for i := 0; i < 210; i++ {
		l.Info(fmt.Sprintf("msg-%d", i))
	}

	lines := l.GetRecentLines()
	if len(lines) != defaultRingSize {
		t.Errorf("expected %d lines, got %d", defaultRingSize, len(lines))
	}

	// First line should be msg-10 (oldest retained), last should be msg-209
	if !strings.Contains(lines[0], "msg-10") {
		t.Errorf("expected first line to contain 'msg-10', got %q", lines[0])
	}
	if !strings.Contains(lines[len(lines)-1], "msg-209") {
		t.Errorf("expected last line to contain 'msg-209', got %q", lines[len(lines)-1])
	}
}

func TestSubscribe(t *testing.T) {
	l, err := New("", "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	ch := l.Subscribe()

	go func() {
		time.Sleep(10 * time.Millisecond)
		l.Info("hello subscriber")
	}()

	select {
	case line := <-ch:
		if !strings.Contains(line, "hello subscriber") {
			t.Errorf("expected message to contain 'hello subscriber', got %s", line)
		}
	case <-time.After(time.Second):
		t.Error("timeout waiting for log message")
	}

	l.Unsubscribe(ch)
}

func TestUnsubscribeNonExistent(t *testing.T) {
	l, err := New("", "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Should not panic when unsubscribing a channel that was never subscribed
	ch := make(chan string, 1)
	l.Unsubscribe(ch) // no-op, should not panic
}

func TestJobLogs(t *testing.T) {
	l, err := New("", "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	l.LogForJob("job-1", slog.LevelInfo, "downloading segment", "seq", 1)
	l.LogForJob("job-1", slog.LevelInfo, "downloading segment", "seq", 2)

	logs := l.GetJobLogs("job-1")
	if len(logs) != 2 {
		t.Errorf("expected 2 job log entries, got %d", len(logs))
	}

	l.ClearJobLogs("job-1")
	logs = l.GetJobLogs("job-1")
	if logs != nil {
		t.Error("expected nil after clear")
	}
}

func TestJobLogsPruning(t *testing.T) {
	l, err := New("", "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Fill past maxJobLogLines (500)
	for i := 0; i < 550; i++ {
		l.LogForJob("job-prune", slog.LevelInfo, "msg", "i", i)
	}

	logs := l.GetJobLogs("job-prune")
	if len(logs) > maxJobLogLines {
		t.Errorf("expected at most %d job log entries, got %d", maxJobLogLines, len(logs))
	}
	// After pruning 20% (100 lines) from 500, then adding remaining 50:
	// 500 -> prune to 400 -> add 50 -> 450
	// Then no more pruning needed for the rest
	if len(logs) < 400 {
		t.Errorf("expected at least 400 job log entries after pruning, got %d", len(logs))
	}
}

func TestJobLogsPruneByActiveIDs(t *testing.T) {
	l, err := New("", "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	l.LogForJob("keep-1", slog.LevelInfo, "msg")
	l.LogForJob("keep-2", slog.LevelInfo, "msg")
	l.LogForJob("remove-1", slog.LevelInfo, "msg")

	activeIDs := map[string]struct{}{
		"keep-1": {},
		"keep-2": {},
	}
	l.PruneJobLogs(activeIDs)

	if l.GetJobLogs("keep-1") == nil {
		t.Error("expected keep-1 logs to be retained")
	}
	if l.GetJobLogs("keep-2") == nil {
		t.Error("expected keep-2 logs to be retained")
	}
	if l.GetJobLogs("remove-1") != nil {
		t.Error("expected remove-1 logs to be pruned")
	}
}

func TestSetLevel(t *testing.T) {
	l, err := New("", "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Log a debug message at DEBUG level — should reach the ring buffer
	l.Debug("visible debug message")
	lines := l.GetRecentLines()
	foundVisible := false
	for _, line := range lines {
		if strings.Contains(line, "visible debug message") {
			foundVisible = true
			break
		}
	}
	if !foundVisible {
		t.Error("expected 'visible debug message' in ring buffer at DEBUG level")
	}

	// Raise level to ERROR — debug messages should be filtered before ring buffer
	l.SetLevel("ERROR")
	countBefore := len(l.GetRecentLines())
	l.Debug("should not appear")
	l.Info("also filtered")
	l.Warn("also filtered")
	countAfter := len(l.GetRecentLines())
	if countAfter != countBefore {
		t.Errorf("expected ring buffer count unchanged after filtered messages, got %d -> %d",
			countBefore, countAfter)
	}

	// ERROR messages should still reach the ring buffer
	l.Error("visible error message")
	lines = l.GetRecentLines()
	foundError := false
	for _, line := range lines {
		if strings.Contains(line, "visible error message") {
			foundError = true
			break
		}
	}
	if !foundError {
		t.Error("expected 'visible error message' in ring buffer at ERROR level")
	}
}

func TestSetLevelValidLevels(t *testing.T) {
	l, err := New("", "INFO", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	tests := []struct {
		level string
		valid bool
	}{
		{"DEBUG", true},
		{"INFO", true},
		{"WARN", true},
		{"WARNING", true},
		{"ERROR", true},
		{"debug", true},  // case insensitive
		{"Info", true},   // case insensitive
		{"INVALID", false},
		{"", false},
		{"TRACE", false},
	}

	for _, tt := range tests {
		ok := l.SetLevel(tt.level)
		if ok != tt.valid {
			t.Errorf("SetLevel(%q) returned %v, expected %v", tt.level, ok, tt.valid)
		}
	}
}

func TestLogRotation(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "rotate.log")

	// Use a very small max size to trigger rotation quickly
	l, err := New(logPath, "DEBUG", 100, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Write enough to trigger rotation
	for i := 0; i < 20; i++ {
		l.Info("rotation test message that is long enough to exceed the limit", "i", i)
	}

	// Check that rotated files exist
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		t.Error("expected current log file to exist")
	}
	if _, err := os.Stat(logPath + ".1"); os.IsNotExist(err) {
		t.Error("expected rotated log file .1 to exist")
	}
}

func TestSuppressRestoreStdout(t *testing.T) {
	l, err := New("", "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if !l.stdout.enabled.Load() {
		t.Error("expected stdout enabled by default")
	}

	l.SuppressStdout()
	if l.stdout.enabled.Load() {
		t.Error("expected stdout disabled after SuppressStdout")
	}

	l.RestoreStdout()
	if !l.stdout.enabled.Load() {
		t.Error("expected stdout enabled after RestoreStdout")
	}
}

func TestCloseClosesSubscribers(t *testing.T) {
	l, err := New("", "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}

	ch := l.Subscribe()
	l.Close()

	// Channel should be closed after Close
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected channel to be closed")
		}
	case <-time.After(time.Second):
		t.Error("timeout waiting for channel close")
	}
}

func TestFormatLogLineMissingValue(t *testing.T) {
	// Odd number of args should produce "key=!MISSING"
	line := formatLogLine(slog.LevelInfo, "test", "orphan_key")
	if !strings.Contains(line, "orphan_key=!MISSING") {
		t.Errorf("expected '!MISSING' marker for unpaired key, got %q", line)
	}
}

func TestFormatLogLineSlogAttr(t *testing.T) {
	// slog.Attr values should be handled as self-contained key=value pairs
	line := formatLogLine(slog.LevelInfo, "test", slog.String("key1", "val1"), slog.Int("key2", 42))
	if !strings.Contains(line, "key1=val1") {
		t.Errorf("expected 'key1=val1', got %q", line)
	}
	if !strings.Contains(line, "key2=42") {
		t.Errorf("expected 'key2=42', got %q", line)
	}
	if strings.Contains(line, "!MISSING") {
		t.Errorf("unexpected '!MISSING' marker for slog.Attr args, got %q", line)
	}

	// Mixed: slog.Attr + plain key-value pairs
	line2 := formatLogLine(slog.LevelWarn, "mixed", slog.String("attr", "val"), "plain_key", "plain_val")
	if !strings.Contains(line2, "attr=val") {
		t.Errorf("expected 'attr=val', got %q", line2)
	}
	if !strings.Contains(line2, "plain_key=plain_val") {
		t.Errorf("expected 'plain_key=plain_val', got %q", line2)
	}
	if strings.Contains(line2, "!MISSING") {
		t.Errorf("unexpected '!MISSING' in mixed args, got %q", line2)
	}
}

func TestLogAfterClose(t *testing.T) {
	l, err := New("", "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}

	l.Close()

	// Should not panic when logging after close
	l.Info("after close")
	l.Debug("after close debug")
	l.Warn("after close warn")
	l.Error("after close error")
}

func TestNewLoggerNoFile(t *testing.T) {
	// Empty path means no file output
	l, err := New("", "INFO", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if l.file != nil {
		t.Error("expected nil file when path is empty")
	}

	// Should still work for ring buffer and subscribers
	l.Info("no file test")
	lines := l.GetRecentLines()
	if len(lines) != 1 {
		t.Errorf("expected 1 line in ring buffer, got %d", len(lines))
	}
}
