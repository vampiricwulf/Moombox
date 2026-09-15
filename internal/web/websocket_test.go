package web

import (
	"bytes"
	"strings"
	"sync"
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

// countingWSLogger counts calls per level so a test can assert on the VOLUME of
// logging, not only its content.
type countingWSLogger struct {
	mu                        sync.Mutex
	debug, info, warn, errors int
}

func (l *countingWSLogger) Debug(msg string, args ...any) { l.mu.Lock(); l.debug++; l.mu.Unlock() }
func (l *countingWSLogger) Info(msg string, args ...any)  { l.mu.Lock(); l.info++; l.mu.Unlock() }
func (l *countingWSLogger) Warn(msg string, args ...any)  { l.mu.Lock(); l.warn++; l.mu.Unlock() }
func (l *countingWSLogger) Error(msg string, args ...any) { l.mu.Lock(); l.errors++; l.mu.Unlock() }

func (l *countingWSLogger) counts() (debug, warn int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.debug, l.warn
}

// TestQueueOrDropLogsAtMostOncePerLaggingClient pins the fix for the drop-
// warning feedback loop: queueOrDrop Warned on every drop, that line reached
// the app logger, the logger's subscriber rebroadcast it to the same full
// queue, which dropped and Warned again (sweep T1-9).
//
// THE MUTANT: put `hub.logger.Warn("dropped oldest WS frame; client lagging")`
// back in queueOrDrop — warn goes 0 → 100 and the loop is back.
func TestQueueOrDropLogsAtMostOncePerLaggingClient(t *testing.T) {
	log := &countingWSLogger{}
	hub := NewWebSocketHub(log)
	client := &wsClient{writes: make(chan []byte, wsWriteQueueSize)}

	// The first wsWriteQueueSize frames fit; every one after that drops one.
	for range wsWriteQueueSize + 100 {
		hub.queueOrDrop(client, []byte(`{"type":"log"}`))
	}

	debug, warn := log.counts()
	if warn != 0 {
		t.Errorf("Warn called %d times; a dropped frame must not reach the app logger at Warn — "+
			"that line is broadcast straight back into the same full queue", warn)
	}
	if debug != 1 {
		t.Errorf("Debug called %d times, want exactly 1 in one 30s window", debug)
	}
	if got := client.drops.Load(); got != 100 {
		t.Errorf("drops = %d, want 100 — every dropped frame must still be COUNTED even though "+
			"only one of them is logged", got)
	}
}

// TestLogDropTotalReportsOnceAtTeardown: the lifetime total is the number an
// operator needs, and it is reported exactly once, from the one teardown point
// every removal path reaches.
//
// THE MUTANT: drop the `n > 0` guard — the first assertion fails and every
// healthy disconnect logs a zero.
func TestLogDropTotalReportsOnceAtTeardown(t *testing.T) {
	log := &countingWSLogger{}
	hub := NewWebSocketHub(log)
	client := &wsClient{writes: make(chan []byte, 1)}

	hub.logDropTotal(client)
	if debug, _ := log.counts(); debug != 0 {
		t.Errorf("Debug called %d times for a client that dropped nothing, want 0", debug)
	}

	client.drops.Store(7)
	hub.logDropTotal(client)
	if debug, _ := log.counts(); debug != 1 {
		t.Errorf("Debug called %d times after a lagging client went away, want exactly 1", debug)
	}
}

// TestQueueOrDropResyncsAfterADrop: drop-oldest throws away frames that nothing
// later supersedes — job_deleted, jobs_update, config_update — so a lagging tab
// keeps a ghost row until it reconnects. The first frame after a drop is
// therefore the full initial-state snapshot instead (sweep T3-26).
//
// THE MUTANT: delete the needsResync claim at the top of queueOrDrop — the
// first assertion sees the incremental frame and the ghost row survives.
func TestQueueOrDropResyncsAfterADrop(t *testing.T) {
	log := &countingWSLogger{}
	hub := NewWebSocketHub(log)
	hub.InitialState = func() map[string]any {
		return map[string]any{"jobs": []any{"snapshot-marker"}}
	}
	client := &wsClient{writes: make(chan []byte, wsWriteQueueSize)}

	// One frame more than fits: exactly one drop.
	for range wsWriteQueueSize + 1 {
		hub.queueOrDrop(client, []byte(`{"type":"job_update"}`))
	}
	if client.drops.Load() != 1 {
		t.Fatalf("fixture: drops = %d, want exactly 1", client.drops.Load())
	}
	for len(client.writes) > 0 { // the client catches up
		<-client.writes
	}

	hub.queueOrDrop(client, []byte(`{"type":"log","payload":"after"}`))
	got := <-client.writes
	if !bytes.Contains(got, []byte(`"initial_state"`)) || !bytes.Contains(got, []byte("snapshot-marker")) {
		t.Fatalf("first frame after a drop = %s; want the initial_state snapshot — without it the "+
			"client keeps whatever row the dropped job_deleted was retiring", got)
	}

	hub.queueOrDrop(client, []byte(`{"type":"log","payload":"later"}`))
	next := <-client.writes
	if !bytes.Contains(next, []byte("later")) {
		t.Fatalf("second frame = %s; want the frame itself — the resync flag must clear, or every "+
			"later frame is replaced by a full snapshot for the rest of the connection", next)
	}
}
