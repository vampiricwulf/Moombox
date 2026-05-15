package web

import (
	"strings"
	"testing"
)

type testWSLogger struct{}

func (l testWSLogger) Debug(msg string, args ...any) {}
func (l testWSLogger) Info(msg string, args ...any)  {}
func (l testWSLogger) Warn(msg string, args ...any)  {}
func (l testWSLogger) Error(msg string, args ...any) {}

func TestWebSocketHubClose(t *testing.T) {
	hub := NewWebSocketHub(testWSLogger{})

	hub.Close()

	hub.mu.Lock()
	if !hub.closed {
		t.Error("expected hub to be marked closed")
	}
	hub.mu.Unlock()
}

func TestWebSocketHubBroadcastNoClients(t *testing.T) {
	hub := NewWebSocketHub(testWSLogger{})

	// Should not panic with no clients
	hub.Broadcast("test", map[string]string{"key": "value"})
	hub.BroadcastJobUpdate(map[string]string{"key": "value"})
	hub.BroadcastJobsUpdate([]string{})
	hub.BroadcastCheckTimers(map[string]any{})
}

func TestWebSocketHubLogBuffer(t *testing.T) {
	hub := NewWebSocketHub(testWSLogger{})

	// Add some log lines
	for range 10 {
		hub.BroadcastLog("line")
	}

	buf := hub.GetLogBuffer()
	if len(buf) != 10 {
		t.Errorf("expected 10 log lines, got %d", len(buf))
	}
}

func TestWebSocketHubLogBufferTruncation(t *testing.T) {
	hub := NewWebSocketHub(testWSLogger{})

	// The buffer trims when it exceeds 400 entries, keeping the last 200.
	// Adding 500 lines: at 401 it trims to 200, then 99 more are added = 299.
	for range 500 {
		hub.BroadcastLog("line")
	}

	buf := hub.GetLogBuffer()
	if len(buf) > 400 {
		t.Errorf("expected log buffer to be bounded, got %d lines", len(buf))
	}

	// Adding enough to trigger a second trim
	for range 200 {
		hub.BroadcastLog("line")
	}

	buf = hub.GetLogBuffer()
	// After 700 total: first trim at 401 -> 200, continues to 400+99=499 -> second trim at 500 -> 200, then 200 more = never exceeds 400
	if len(buf) > 400 {
		t.Errorf("expected log buffer to stay bounded after multiple trims, got %d lines", len(buf))
	}
}

func TestWebSocketHubLogLineTruncation(t *testing.T) {
	hub := NewWebSocketHub(testWSLogger{})

	// Add a very long log line (over 4096 chars)
	var longLine strings.Builder
	for range 5000 {
		longLine.WriteString("x")
	}
	hub.BroadcastLog(longLine.String())

	buf := hub.GetLogBuffer()
	if len(buf) != 1 {
		t.Fatalf("expected 1 log line, got %d", len(buf))
	}
	// Should be truncated with marker
	if len(buf[0]) > 4200 {
		t.Errorf("expected log line to be truncated, got length %d", len(buf[0]))
	}
}

func TestWebSocketHubClientCount(t *testing.T) {
	hub := NewWebSocketHub(testWSLogger{})
	if hub.ClientCount() != 0 {
		t.Errorf("expected 0 clients, got %d", hub.ClientCount())
	}
}

