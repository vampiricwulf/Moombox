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

// TestQueueOverflowGivesUpAFollowedTickFirst walks four-slot queues through
// every overflow there is. L frames are ones nothing restates; A and B frames
// are progress ticks of two jobs.
//
// Mutants: evict the head whatever it is (the channel's behaviour: the first
// step loses L1); evict the oldest tick whatever its job (the second queue
// loses B1, the only copy of B's progress, with A1 followed by A2); evict the
// NEWEST followed tick; swap the victim with the tail instead of closing the
// gap (the order changes); report a tick with no newer one as restated (the
// results of the L3, L4 and B2 steps); queue the tick that finds no room over
// a frame nothing restates (the B2 step).
func TestQueueOverflowGivesUpAFollowedTickFirst(t *testing.T) {
	frame := func(name string) wsFrame {
		return wsFrame{data: []byte(name), restated: name[0] != 'L', job: name[:1]}
	}
	type step struct {
		push   string
		result wsPushResult
		want   []string
	}
	for _, tc := range []struct {
		start []string
		steps []step
	}{
		{[]string{"L1", "A1", "L2", "B1"}, []step{
			// A1 is followed by the incoming A2.
			{"A2", wsPushLostRestated, []string{"L1", "L2", "B1", "A2"}},
			// No tick is followed: the oldest goes, and it is a loss.
			{"L3", wsPushLostState, []string{"L1", "L2", "A2", "L3"}},
			{"A3", wsPushLostRestated, []string{"L1", "L2", "L3", "A3"}},
			{"L4", wsPushLostState, []string{"L1", "L2", "L3", "L4"}},
			// A tick that finds no tick to replace is refused, and is a loss.
			{"B2", wsPushLostState, []string{"L1", "L2", "L3", "L4"}},
			{"L5", wsPushLostState, []string{"L2", "L3", "L4", "L5"}},
		}},
		{[]string{"B1", "A1", "A2", "L1"}, []step{
			// A1 is followed in the queue; B1, older, is B's only tick.
			{"L2", wsPushLostRestated, []string{"B1", "A2", "L1", "L2"}},
		}},
	} {
		q := newWSQueue(4)
		for _, name := range tc.start {
			if got := q.push(frame(name)); got != wsPushQueued {
				t.Fatalf("push %s into a queue with room = %d, want queued", name, got)
			}
		}
		for _, s := range tc.steps {
			if got := q.push(frame(s.push)); got != s.result {
				t.Errorf("%v: push %s = %d, want %d", tc.start, s.push, got, s.result)
			}
			if got := queued(q); !slices.Equal(got, s.want) {
				t.Errorf("%v: after %s the queue holds %v, want %v", tc.start, s.push, got, s.want)
			}
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

// TestOnlyALossNothingRestatesArmsAResync, through the hub: on a full queue a
// job's tick replaces that job's older tick and costs nothing more; the tick
// of a job with none queued finds nothing to replace, is the frame given up,
// and arms, because that job may not tick again for minutes (it used to be
// written off against its next tick, and a job that stalled just then showed
// its old progress until it wrote again); and one more frame nothing restates
// arms too.
//
// Mutants: arm on every overflow (the first check); treat any tick as
// restated by a tick still to come (the second); let a tick evict the
// job_deleted (the queue's head); never arm (the last).
func TestOnlyALossNothingRestatesArmsAResync(t *testing.T) {
	hub := NewWebSocketHub(testWSLogger{})
	client := attachTestClient(hub, wsWriteQueueSize)
	// The build is not under test: keep the claim gated.
	client.lastResync.Store(time.Now().Add(time.Hour).UnixNano())

	hub.BroadcastJobDeleted("gone")
	for i := 2; i < wsWriteQueueSize; i++ {
		hub.BroadcastLog("line", uint64(i))
	}
	hub.BroadcastJobProgress(progressTick{ID: "ticking", Percent: 1, version: 1})
	if client.drops.Load() != 0 {
		t.Fatal("fixture: the queue overflowed while it was being filled")
	}

	hub.BroadcastJobProgress(progressTick{ID: "ticking", Percent: 2, version: 2})
	if client.drops.Load() != 1 {
		t.Fatalf("drops = %d, want 1: the replaced tick is still counted", client.drops.Load())
	}
	if client.needsResync.Load() {
		t.Error("a tick replaced by the same job's next tick armed a resync")
	}

	hub.BroadcastJobProgress(progressTick{ID: "stalling", Percent: 7, version: 1})
	if !client.needsResync.Load() {
		t.Error("the only tick of a job was given up and no resync was armed: if the job stalls now, the tab never learns its progress")
	}
	frames := queued(client.writes)
	if !strings.Contains(frames[0], "job_deleted") {
		t.Errorf("the queue's head is %s, want the job_deleted: a tick displaced it", frames[0])
	}
	if last := frames[len(frames)-1]; !strings.Contains(last, `"percent":2`) {
		t.Errorf("the queue's tail is %s, want the ticking job's newest tick", last)
	}

	hub.BroadcastLog("one too many", 99) // gives up the ticking job's only tick
	client.needsResync.Store(false)
	hub.BroadcastLog("and another", 100) // pushes the job_deleted out
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

// TestABurstReachesAHealthyClientWholeAndInOrder: a burst the queue has room
// for is written frame for frame with nothing dropped. The whole burst is
// queued before the write loop starts, so one wake-up token stands for all of
// it, whatever the machine's pace.
//
// Mutants: writePump taking one frame per wake-up (the second frame never
// arrives); push giving a frame up while there is room.
func TestABurstReachesAHealthyClientWholeAndInOrder(t *testing.T) {
	hub := NewWebSocketHub(testWSLogger{})
	const burst = wsWriteQueueSize
	drops := make(chan uint64, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.CloseNow()
		client := &wsClient{conn: conn, writes: newWSQueue(wsWriteQueueSize)}
		client.ctx, client.cancel = context.WithCancel(r.Context())
		defer client.cancel()
		for i := 1; i <= burst; i++ {
			hub.enqueue(client, wsFrame{data: fmt.Appendf(nil, `{"payload":%d}`, i), restated: true, job: "job"})
		}
		drops <- client.drops.Load()
		hub.writePump(client) // until the test hangs up
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
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
	if n := <-drops; n != 0 {
		t.Errorf("a queue with room dropped %d frames", n)
	}
}
