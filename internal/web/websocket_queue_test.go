package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// queued lists a queue's frames by their data, oldest first, without taking
// them.
func queued(q *wsQueue) []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]string, 0, len(q.frames))
	for _, f := range q.frames {
		out = append(out, string(f.data))
	}
	return out
}

// TestQueueOverflowGivesUpARestatedFrameFirst walks one four-slot queue
// through every overflow there is. L frames are ones nothing restates, T
// frames are progress ticks.
//
// Mutants: evict the head whatever it is (the channel's behaviour: step 1
// loses L1); evict the NEWEST restated frame (step 1 keeps T1 over T2);
// swap the victim with the tail instead of closing the gap (the order after
// step 1 changes); evict an unrestated frame for an incoming tick (step 4).
func TestQueueOverflowGivesUpARestatedFrameFirst(t *testing.T) {
	q := newWSQueue(4)
	frame := func(name string) wsFrame {
		return wsFrame{data: []byte(name), restated: name[0] == 'T'}
	}
	for _, name := range []string{"L1", "T1", "L2", "T2"} {
		if got := q.push(frame(name)); got != wsPushQueued {
			t.Fatalf("push %s into a queue with room = %d, want queued", name, got)
		}
	}
	steps := []struct {
		push   string
		result wsPushResult
		want   []string
	}{
		{"T3", wsPushLostRestated, []string{"L1", "L2", "T2", "T3"}},
		{"L3", wsPushLostRestated, []string{"L1", "L2", "T3", "L3"}},
		{"L4", wsPushLostRestated, []string{"L1", "L2", "L3", "L4"}},
		{"T4", wsPushLostRestated, []string{"L1", "L2", "L3", "L4"}},
		{"L5", wsPushLostState, []string{"L2", "L3", "L4", "L5"}},
	}
	for i, step := range steps {
		if got := q.push(frame(step.push)); got != step.result {
			t.Errorf("step %d: push %s = %d, want %d", i, step.push, got, step.result)
		}
		if got := queued(q); !slices.Equal(got, step.want) {
			t.Errorf("step %d: after %s the queue holds %v, want %v", i, step.push, got, step.want)
		}
	}
}

// TestASlowSnapshotWriteDoesNotArmTheNextSnapshot is the loop this queue
// closes. A client is sent a resync snapshot over a slow link; while the write
// loop is blocked on it, a download ticks 30 times between each of ten log
// lines, and one row changes. The ticks used to push the log lines and the
// row out of the queue, each loss armed the next snapshot, and its write lost
// the next ones: snapshots back to back for as long as the download ran.
//
// Mutant: evict the oldest frame whatever it is: the resync is armed, and
// once the write loop is free the next frame is a second snapshot.
func TestASlowSnapshotWriteDoesNotArmTheNextSnapshot(t *testing.T) {
	hub := NewWebSocketHub(testWSLogger{})
	builds := 0
	hub.InitialState = func() map[string]any {
		builds++
		return map[string]any{"jobs": []any{}}
	}
	client := attachTestClient(hub, wsWriteQueueSize)

	// The snapshot is queued, and the write loop takes it and blocks.
	client.needsResync.Store(true)
	hub.BroadcastJobDeleted("gone")
	if f := popFrame(client); !f.snapshot || builds != 1 {
		t.Fatalf("fixture: the first frame should be the one resync snapshot (builds = %d)", builds)
	}

	var want []string
	version := uint64(0)
	for line := 1; line <= 10; line++ {
		hub.BroadcastLog(fmt.Sprintf("line %d", line), uint64(line))
		want = append(want, fmt.Sprintf("log:line %d", line))
		if line == 5 {
			version++
			hub.BroadcastJobUpdate(progressTick{ID: "job", Percent: -1, version: version})
			want = append(want, "job_update")
		}
		for range 30 {
			version++
			hub.BroadcastJobProgress(progressTick{ID: "job", Percent: int(version), version: version})
		}
	}
	if client.drops.Load() == 0 {
		t.Fatal("fixture: the queue never overflowed")
	}
	if client.needsResync.Load() {
		t.Error("frames queued during the snapshot's write armed the next snapshot")
	}

	// The write finishes, well past the rate limit, and the queue drains.
	client.lastResync.Store(time.Now().Add(-2 * wsResyncMinInterval).UnixNano())
	hub.BroadcastLog("after", 11)
	want = append(want, "log:after")

	var got []string
	lastTick := 0
	for _, raw := range drainFrames(client) {
		var msg struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatal(err)
		}
		switch msg.Type {
		case "initial_state":
			t.Error("the client was sent a second snapshot")
		case "log":
			var line string
			if err := json.Unmarshal(msg.Payload, &line); err != nil {
				t.Fatal(err)
			}
			got = append(got, "log:"+line)
		case "job_progress":
			var tick progressTick
			if err := json.Unmarshal(msg.Payload, &tick); err != nil {
				t.Fatal(err)
			}
			if tick.Percent <= lastTick {
				t.Errorf("tick %d follows tick %d", tick.Percent, lastTick)
			}
			lastTick = tick.Percent
		default:
			got = append(got, msg.Type)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("frames nothing restates:\n got %v\nwant %v", got, want)
	}
	if lastTick != int(version) {
		t.Errorf("the newest tick queued is %d, want %d: the client must end on the latest progress", lastTick, version)
	}
	if builds != 1 {
		t.Errorf("InitialState built %d times, want 1", builds)
	}
}

// TestOnlyALossNothingRestatesArmsAResync: with a full queue of frames
// nothing restates, a tick is the frame given up and costs nothing more; one
// more frame nothing restates pushes the oldest out, and that arms.
//
// Mutants: arm on every overflow (the tick arms); never arm, or let the tick
// evict the job_deleted (the last check, or the queue's head).
func TestOnlyALossNothingRestatesArmsAResync(t *testing.T) {
	hub := NewWebSocketHub(testWSLogger{})
	client := attachTestClient(hub, wsWriteQueueSize)
	// The build is not under test: keep the claim gated.
	client.lastResync.Store(time.Now().Add(time.Hour).UnixNano())

	hub.BroadcastJobDeleted("gone")
	for i := 1; i < wsWriteQueueSize; i++ {
		hub.BroadcastLog("line", uint64(i))
	}
	hub.BroadcastJobProgress(progressTick{ID: "job", Percent: 1, version: 1})
	if client.drops.Load() != 1 {
		t.Fatalf("drops = %d, want 1: the refused tick is still counted", client.drops.Load())
	}
	if client.needsResync.Load() {
		t.Error("a tick that found no room armed a resync; the next tick restates it")
	}
	if head := queued(client.writes)[0]; !strings.Contains(head, "job_deleted") {
		t.Errorf("the queue's head is %s, want the job_deleted: a tick displaced it", head)
	}

	hub.BroadcastLog("one too many", 99)
	if !client.needsResync.Load() {
		t.Error("the job_deleted was pushed out by a log line and no resync was armed")
	}
}

// TestWritePumpStopsWhenItsClientIsDetached: the write loop parks on the
// queue's wake-up channel, which nothing closes; the client's ctx is what
// ends it. After the detach a broadcast that had already listed the client is
// refused and holds nothing.
//
// Mutants: writePump waiting on the wake-up alone (the goroutine never
// returns); close leaving the queue open (the push is accepted).
func TestWritePumpStopsWhenItsClientIsDetached(t *testing.T) {
	hub := NewWebSocketHub(testWSLogger{})
	client := attachTestClient(hub, wsWriteQueueSize)
	client.ctx, client.cancel = context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.writePump(client)
	}()
	if !hub.detachClient(client) {
		t.Fatal("fixture: the client was not in the hub")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("writePump is still running after its client was detached")
	}
	if hub.enqueue(client, wsFrame{data: []byte("late")}) {
		t.Error("a frame was accepted for a detached client")
	}
	if n := client.writes.size(); n != 0 {
		t.Errorf("a detached client's queue holds %d frames", n)
	}
}

// TestABurstReachesAHealthyClientWholeAndInOrder: over a real socket, a burst
// of ticks the queue has room for is delivered frame for frame with nothing
// dropped, whatever the write loop's pace. Sixteen pushes take far less time
// than one socket write, so most of the burst waits behind a single wake-up
// token.
//
// Mutants: writePump taking one frame per wake-up (the frames behind the
// second token never arrive); push giving a frame up while there is room.
func TestABurstReachesAHealthyClientWholeAndInOrder(t *testing.T) {
	hub := NewWebSocketHub(testWSLogger{})
	hub.InitialState = func() map[string]any { return map[string]any{} }
	srv := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	t.Cleanup(func() { srv.Close(); hub.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatalf("read initial_state: %v", err)
	}
	hub.mu.RLock()
	var client *wsClient
	for c := range hub.clients {
		client = c
	}
	hub.mu.RUnlock()
	if client == nil {
		t.Fatal("fixture: no client registered")
	}

	const burst = wsWriteQueueSize
	for i := 1; i <= burst; i++ {
		hub.Broadcast("job_progress", i)
	}
	for want := 1; want <= burst; want++ {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("frame %d of %d never arrived: %v", want, burst, err)
		}
		var msg struct {
			Payload int `json:"payload"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Payload != want {
			t.Fatalf("frame %d carries tick %d", want, msg.Payload)
		}
	}
	if n := client.drops.Load(); n != 0 {
		t.Errorf("a client that kept up dropped %d frames", n)
	}
}
