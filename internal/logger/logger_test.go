package logger

import (
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

func TestJobLogs(t *testing.T) {
	l, err := New("", "DEBUG", 1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	l.LogForJob("job-1", 0, "downloading segment", "seq", 1)
	l.LogForJob("job-1", 0, "downloading segment", "seq", 2)

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
