package web

import (
	"encoding/json"
	"testing"
)

// drainFrames empties a test client's write queue and returns the frames it
// held, in order.
func drainFrames(client *wsClient) [][]byte {
	var out [][]byte
	for {
		select {
		case msg := <-client.writes:
			out = append(out, msg)
		default:
			return out
		}
	}
}

// attachTestClient registers a client with a queue deep enough that nothing
// this test sends can overflow it — an eviction would confuse "the hub
// coalesced a tick" with "the client was too slow", and only the first is
// under test.
func attachTestClient(hub *WebSocketHub, depth int) *wsClient {
	client := &wsClient{writes: make(chan []byte, depth)}
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
