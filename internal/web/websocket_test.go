package web

import (
	"bytes"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
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

func (l *countingWSLogger) errorCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.errors
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

// TestQueueOrDropDoesNotRetryAnUnbuildableSnapshot: initialStateBytes reports a
// marshal failure through the APP logger, and that logger's subscriber
// broadcasts the line straight back to this same client. Re-arming needsResync
// on a failed build therefore means claim → build → fail → Error → broadcast →
// claim → … forever, with a full GetAllJobs on every turn. It is reachable, not
// theoretical: a NaN or ±Inf in any of a job's float64 columns makes
// json.Marshal fail permanently.
//
// THE MUTANT: put `client.needsResync.Store(true)` back on the nil-snapshot
// path — the flag is still armed below and builds goes 1 → 2.
func TestQueueOrDropDoesNotRetryAnUnbuildableSnapshot(t *testing.T) {
	log := &countingWSLogger{}
	hub := NewWebSocketHub(log)
	builds := 0
	hub.InitialState = func() map[string]any {
		builds++
		return map[string]any{"jobs": math.NaN()} // json: unsupported value
	}
	client := &wsClient{writes: make(chan []byte, wsWriteQueueSize)}
	client.needsResync.Store(true)

	hub.queueOrDrop(client, []byte(`{"type":"log","payload":"first"}`))
	if builds != 1 {
		t.Fatalf("fixture: InitialState called %d times, want 1", builds)
	}
	if got := <-client.writes; !bytes.Contains(got, []byte("first")) {
		t.Fatalf("frame = %s; want the original incremental frame — a snapshot that cannot be built "+
			"must not also cost the caller its frame", got)
	}
	if client.needsResync.Load() {
		t.Error("needsResync is still armed after a failed build — the retry rebuilds, fails and logs, " +
			"and that log line is broadcast right back here: an unbounded Error→broadcast→Error loop")
	}

	// Past the rate limit, so a re-armed flag — and only a re-armed flag —
	// would build a second time.
	client.lastResync.Store(time.Now().Add(-2 * wsResyncMinInterval).UnixNano())
	hub.queueOrDrop(client, []byte(`{"type":"log","payload":"second"}`))
	if builds != 1 {
		t.Errorf("InitialState called %d times, want 1 — a failed snapshot must not be retried", builds)
	}
}

// TestQueueOrDropSurvivesAPanickingInitialState: the snapshot build runs
// hub.InitialState, an application closure that reads the database. It must not
// run under the enqueue's catch-all recover, which is silent by design (it
// exists for the send-on-closed-channel race). There, a provider panic would be
// swallowed with no log line and the frame would vanish before either select
// without even counting a drop — one bad provider and every broadcast to every
// client disappears, forever, in silence.
//
// THE MUTANT: fold the claim+build back under the enqueue's single silent
// recover — errors drops to 0 and nothing reaches the queue.
func TestQueueOrDropSurvivesAPanickingInitialState(t *testing.T) {
	log := &countingWSLogger{}
	hub := NewWebSocketHub(log)
	hub.InitialState = func() map[string]any { panic("provider exploded") }
	client := &wsClient{writes: make(chan []byte, wsWriteQueueSize)}
	client.needsResync.Store(true)

	hub.queueOrDrop(client, []byte(`{"type":"log","payload":"survivor"}`))

	if got := log.errorCount(); got != 1 {
		t.Errorf("Error called %d times, want exactly 1 — a panicking initial-state provider must be "+
			"LOUD, not absorbed by a recover written for a different race", got)
	}
	select {
	case got := <-client.writes:
		if !bytes.Contains(got, []byte("survivor")) {
			t.Errorf("frame = %s; want the original incremental frame", got)
		}
	default:
		t.Error("nothing was enqueued — the provider panic swallowed the frame the caller was sending")
	}
	if client.needsResync.Load() {
		t.Error("needsResync re-armed after a panic — the next frame panics again, and the next")
	}
}

// TestQueueOrDropRateLimitsResyncSnapshots: a client that never drains drops on
// EVERY frame, and every drop re-arms the flag. A snapshot costs a GetAllJobs
// plus a whole-state marshal, so without a floor between claims one slow tab
// provokes one of those per broadcast — tens per second while a download runs.
//
// THE MUTANTS: (1) delete the interval check — builds goes 1 → ~25;
// (2) stamp lastResync anywhere but the claim (e.g. only where the frame goes
// straight into a queue with room) — this client's queue is never not-full, so
// the stamp never advances and builds goes ~25 again; (3) CompareAndSwap before
// the interval check — a gated call clears the flag it is not allowed to act
// on, and the "still armed" assertion below fails.
func TestQueueOrDropRateLimitsResyncSnapshots(t *testing.T) {
	log := &countingWSLogger{}
	hub := NewWebSocketHub(log)
	builds := 0
	hub.InitialState = func() map[string]any {
		builds++
		return map[string]any{"jobs": []any{"snapshot-marker"}}
	}
	client := &wsClient{writes: make(chan []byte, wsWriteQueueSize)}

	// Never drained: the first wsWriteQueueSize frames fill the queue, and every
	// frame after that evicts one and re-arms — all inside one interval.
	for range wsWriteQueueSize + 50 {
		hub.queueOrDrop(client, []byte(`{"type":"job_update"}`))
	}
	if builds != 1 {
		t.Errorf("InitialState called %d times inside one %v window, want exactly 1", builds, wsResyncMinInterval)
	}

	// Now a gated frame on a queue with ROOM: nothing is evicted, so nothing
	// re-arms the flag behind the assertion. This is where claim-then-check
	// ordering shows up — it would have consumed the flag and lost the resync.
	for len(client.writes) > 0 {
		<-client.writes
	}
	client.lastResync.Store(time.Now().UnixNano()) // squarely inside the window
	hub.queueOrDrop(client, []byte(`{"type":"job_update"}`))
	if builds != 1 {
		t.Errorf("InitialState called %d times, want 1 — a gated frame must not build a snapshot", builds)
	}
	if !client.needsResync.Load() {
		t.Error("a gated claim cleared needsResync — the rate limit must DELAY the resync, not cancel " +
			"it; the ghost row would then survive until the client reconnects")
	}

	client.lastResync.Store(time.Now().Add(-2 * wsResyncMinInterval).UnixNano())
	hub.queueOrDrop(client, []byte(`{"type":"job_update"}`))
	if builds != 2 {
		t.Errorf("InitialState called %d times once the interval elapsed, want 2 — the delayed resync "+
			"must actually arrive", builds)
	}
}

// TestPingTickFlushesAPendingResync: the flag is otherwise only ever consumed by
// an enqueue, so a hub that goes quiet right after a drop (job_deleted, the next
// frame gated by wsResyncMinInterval, then nothing for minutes) would leave the
// ghost row on screen — the exact thing T3-26 is about. The ping tick flushes it
// instead, bounding the ghost's life to one wsPingInterval.
//
// The tick's CALL SITE needs a live connection and is not covered here; what is
// covered is flushResync itself.
//
// THE MUTANT: the shortcut — flushResync just enqueues initialStateBytes()
// every tick without going through the claim. The first assertion then fires a
// full snapshot at an idle client, and the last one sees a second snapshot on
// the very next tick, forever.
func TestPingTickFlushesAPendingResync(t *testing.T) {
	log := &countingWSLogger{}
	hub := NewWebSocketHub(log)
	hub.InitialState = func() map[string]any {
		return map[string]any{"jobs": []any{"snapshot-marker"}}
	}
	client := &wsClient{writes: make(chan []byte, wsWriteQueueSize)}

	hub.flushResync(client) // nothing armed
	if len(client.writes) != 0 {
		t.Fatalf("an idle client was sent %d frames; a ping tick must cost a load, not a snapshot",
			len(client.writes))
	}

	client.needsResync.Store(true)
	client.lastResync.Store(time.Now().Add(-2 * wsResyncMinInterval).UnixNano())
	hub.flushResync(client)

	got := <-client.writes
	if !bytes.Contains(got, []byte(`"initial_state"`)) || !bytes.Contains(got, []byte("snapshot-marker")) {
		t.Fatalf("flushed frame = %s; want the initial_state snapshot", got)
	}

	client.lastResync.Store(time.Now().Add(-2 * wsResyncMinInterval).UnixNano())
	hub.flushResync(client)
	if len(client.writes) != 0 {
		t.Errorf("a second flush sent %d more frames; the claim must consume the flag, or every ping "+
			"for the rest of the connection carries a full snapshot", len(client.writes))
	}
}
