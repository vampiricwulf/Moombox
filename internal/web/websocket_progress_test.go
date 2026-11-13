package web

import (
	"encoding/json"
	"testing"
	"time"
)

// drainFrames empties a test client's write queue and returns the frames it
// held, in order.
func drainFrames(client *wsClient) [][]byte {
	var out [][]byte
	for {
		msg, ok := client.writes.pop()
		if !ok {
			return out
		}
		out = append(out, msg.data)
	}
}

// popFrame takes the next queued frame, or the zero frame when there is none.
func popFrame(client *wsClient) wsFrame {
	f, _ := client.writes.pop()
	return f
}

// attachTestClient registers a client with a queue deep enough that nothing
// this test sends can overflow it — an eviction would confuse "the hub
// coalesced a tick" with "the client was too slow", and only the first is
// under test.
func attachTestClient(hub *WebSocketHub, depth int) *wsClient {
	client := &wsClient{writes: newWSQueue(depth)}
	hub.mu.Lock()
	hub.clients[client] = struct{}{}
	hub.mu.Unlock()
	return client
}

// TestBroadcastJobProgressIsOneFramePerCallLikeJobUpdate is the hub half of
// the WEB-5 cadence differential (the protected ~60 Hz ruling: cheaper, never
// rarer). The old path put one job_update frame on the wire per progress tick.
// The new path must put exactly one job_progress frame on the wire per tick —
// same count, fewer bytes.
//
// THE MUTANT: give BroadcastJobProgress a per-job throttle or a coalescer (the
// kind BroadcastJobUpdate's own comment records being removed for an unrelated
// ordering race) — the counts diverge and this fails naming both.
func TestBroadcastJobProgressIsOneFramePerCallLikeJobUpdate(t *testing.T) {
	const ticks = 60

	// The old path: the whole row, once per tick.
	oldHub := NewWebSocketHub(testWSLogger{})
	oldClient := attachTestClient(oldHub, 4*ticks)
	row := map[string]any{
		"id": "dQw4w9WgXcQ", "status": "downloading", "progress": "V:12345 A:12345 C:67890",
		"percent": 37.5, "speed": "12.3 MB/s", "eta": "01:23:45", "updatedAt": "2026-09-17T11:23:45Z",
		"title": "a long archive title", "description": "a five-kilobyte YouTube description",
		"outputFile": `D:\media\output\file.mp4`, "thumbnailUrl": "https://i.ytimg.com/vi/x/max.jpg",
	}
	for range ticks {
		oldHub.BroadcastJobUpdate(row)
	}
	oldFrames := drainFrames(oldClient)

	// The new path: the slim frame, once per the SAME tick.
	newHub := NewWebSocketHub(testWSLogger{})
	newClient := attachTestClient(newHub, 4*ticks)
	frame := map[string]any{
		"id": "dQw4w9WgXcQ", "status": "downloading", "progress": "V:12345 A:12345 C:67890",
		"percent": 37.5, "speed": "12.3 MB/s", "eta": "01:23:45", "updatedAt": "2026-09-17T11:23:45Z",
	}
	for range ticks {
		newHub.BroadcastJobProgress(frame)
	}
	newFrames := drainFrames(newClient)

	if len(oldFrames) != ticks {
		t.Fatalf("the baseline itself is wrong: %d job_update frames for %d ticks", len(oldFrames), ticks)
	}
	if len(newFrames) != len(oldFrames) {
		t.Errorf("%d job_progress frames for the %d ticks that produced %d job_update frames — the "+
			"cadence is protected: this change makes each update cheaper, never rarer",
			len(newFrames), ticks, len(oldFrames))
	}

	oldBytes, newBytes := 0, 0
	for i, raw := range newFrames {
		var msg struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("frame %d is not JSON: %v", i, err)
		}
		if msg.Type != "job_progress" {
			t.Errorf("frame %d has type %q, want \"job_progress\" — the client routes on this string", i, msg.Type)
		}
		newBytes += len(raw)
	}
	for _, raw := range oldFrames {
		oldBytes += len(raw)
	}
	t.Logf("%d ticks: job_update %d B total -> job_progress %d B total", ticks, oldBytes, newBytes)
	if newBytes >= oldBytes {
		t.Errorf("job_progress moved %d B over %d ticks against job_update's %d B — the frame exists to "+
			"be smaller", newBytes, ticks, oldBytes)
	}
}

// progressTick is a job_progress payload with a write version, as
// cmd/moombox's jobProgressFrame is.
type progressTick struct {
	ID      string `json:"id"`
	Percent int    `json:"percent"`
	version uint64
}

func (p progressTick) JobVersion() (string, uint64) { return p.ID, p.version }

// TestDroppedProgressFramesDoNotArmAResync: a client that does not drain
// through 100 progress ticks (a snapshot still being written to a slow link)
// loses ticks, and the next tick restates every one of them. It used to be
// handed a full snapshot for it, whose own write lost the next 16 ticks and
// armed the next snapshot, for as long as the download ran.
//
// Mutant: arm needsResync on every eviction again: the flag is set and the
// job_update below is replaced by an initial_state.
func TestDroppedProgressFramesDoNotArmAResync(t *testing.T) {
	hub := NewWebSocketHub(testWSLogger{})
	builds := 0
	hub.InitialState = func() map[string]any {
		builds++
		return map[string]any{"jobs": []any{}}
	}
	client := attachTestClient(hub, wsWriteQueueSize)
	// lastResync is zero: nothing but the flag stands between a drop and a
	// snapshot.

	const ticks = wsWriteQueueSize + 100
	for i := 1; i <= ticks; i++ {
		hub.BroadcastJobProgress(progressTick{ID: "job", Percent: i, version: uint64(i)})
	}
	if got := client.drops.Load(); got != 100 {
		t.Fatalf("drops = %d, want 100: a dropped tick is still counted", got)
	}
	if client.needsResync.Load() {
		t.Error("dropped progress ticks armed a resync; the next tick restates them")
	}

	// The download ends: the last tick the client was sent may be one it
	// never got, and the row that follows carries every column.
	hub.BroadcastJobUpdate(progressTick{ID: "job", Percent: 100, version: ticks + 1})
	frames := drainFrames(client)
	var last struct {
		Type    string       `json:"type"`
		Payload progressTick `json:"payload"`
	}
	if err := json.Unmarshal(frames[len(frames)-1], &last); err != nil {
		t.Fatal(err)
	}
	if last.Type != "job_update" || last.Payload.Percent != 100 {
		t.Errorf("last frame = %s, want the job_update itself", frames[len(frames)-1])
	}
	if builds != 0 {
		t.Errorf("InitialState built %d times for a client that lost only progress ticks, want 0", builds)
	}
}

// TestAReplayedTickIsNotTreatedAsRestated: the tick the hub sends again after
// a row older than it is the one frame that puts the newer progress columns
// back, and a stalled job may not tick again for minutes. Losing it arms a
// resync like any other loss.
//
// Mutant: send the replay through Broadcast("job_progress", …) so it is
// marked restated: the flag stays clear.
func TestAReplayedTickIsNotTreatedAsRestated(t *testing.T) {
	hub := NewWebSocketHub(testWSLogger{})
	client := attachTestClient(hub, wsWriteQueueSize)
	hub.BroadcastJobProgress(progressTick{ID: "job", Percent: 50, version: 5})
	drainFrames(client)

	// A one-slot client: the older row fills the queue and the replayed tick
	// evicts it, then is itself evicted by a later frame nothing restates (a
	// tick would be given up in its place).
	slow := attachTestClient(hub, 1)
	hub.BroadcastJobUpdate(progressTick{ID: "job", Percent: 40, version: 4})
	slow.needsResync.Store(false) // the evicted row armed it; not under test
	slow.lastResync.Store(time.Now().UnixNano())
	hub.BroadcastJobDeleted("other")
	if !slow.needsResync.Load() {
		t.Error("the replayed tick was evicted and no resync was armed")
	}
}
