package web

import (
	"testing"
	"time"
)

// TestSnapshotWriteTimeoutScalesWithTheFrame: at a flat 10 s a 1.7 MB
// snapshot (500 Tasks rows) needed about 1.4 Mbit/s, and a slower link never
// got its job list. The bound grows with the frame so that a link at the
// floor rate finishes, and stops at the cap so a stalled peer is still
// dropped.
//
// Mutants: return wsWriteTimeout for every size: the 512 kbit/s row fails;
// drop the cap: the last row fails.
func TestSnapshotWriteTimeoutScalesWithTheFrame(t *testing.T) {
	if got := snapshotWriteTimeout(0); got != wsWriteTimeout {
		t.Errorf("an empty snapshot gets %v, want the plain %v", got, wsWriteTimeout)
	}
	// Bytes a link moves in a second, and a snapshot it must be able to take.
	for _, tc := range []struct {
		name          string
		bytes, perSec int
	}{
		{"500 rows over 512 kbit/s", 1_700_000, 64_000},
		{"500 rows at the floor rate", 1_700_000, wsSnapshotFloorRate},
		{"2,000 rows over 512 kbit/s", 7_000_000, 64_000},
	} {
		needs := time.Duration(tc.bytes) * time.Second / time.Duration(tc.perSec)
		if got := snapshotWriteTimeout(tc.bytes); got < needs {
			t.Errorf("%s: the write needs %v and is given %v", tc.name, needs, got)
		}
	}
	if got := snapshotWriteTimeout(1 << 30); got != wsSnapshotWriteCap {
		t.Errorf("a 1 GiB snapshot gets %v, want the %v cap", got, wsSnapshotWriteCap)
	}
}

// TestOnlyAResyncSnapshotIsQueuedAsASnapshot: writePump keys the longer
// deadline on wsFrame.snapshot, so a resync has to be marked on both of its
// ways into the queue, and nothing else may be: every other frame keeps
// wsWriteTimeout.
//
// Mutants: build the resync frame without snapshot: true in queueOrDrop or
// flushResync; mark every frame.
func TestOnlyAResyncSnapshotIsQueuedAsASnapshot(t *testing.T) {
	hub := NewWebSocketHub(testWSLogger{})
	hub.InitialState = func() map[string]any { return map[string]any{"jobs": []any{}} }
	client := attachTestClient(hub, wsWriteQueueSize)

	hub.Broadcast("job_deleted", map[string]any{"id": "x"})
	if f := popFrame(client); f.snapshot {
		t.Error("an ordinary frame was queued as a snapshot and would get the long write deadline")
	}

	client.needsResync.Store(true)
	hub.Broadcast("job_deleted", map[string]any{"id": "y"})
	if f := popFrame(client); !f.snapshot {
		t.Error("the resync that replaced a broadcast was not queued as a snapshot")
	}

	client.needsResync.Store(true)
	client.lastResync.Store(0)
	hub.flushResync(client)
	if f := popFrame(client); !f.snapshot {
		t.Error("the resync the ping tick flushed was not queued as a snapshot")
	}
}

// TestControlFramesWaitOutASnapshotWrite: one frame is written at a time, so
// a ping, or the pong that answers the dashboard's own ping, queues behind a
// snapshot write. At a flat 10 s the first to come due during a longer write
// closed the connection the snapshot was still going out on.
//
// Mutant: return wsWriteTimeout from controlTimeout whatever is in progress.
func TestControlFramesWaitOutASnapshotWrite(t *testing.T) {
	hub := NewWebSocketHub(testWSLogger{})
	client := &wsClient{}
	if got := hub.controlTimeout(client); got != wsWriteTimeout {
		t.Errorf("with no snapshot write in progress the timeout is %v, want %v", got, wsWriteTimeout)
	}

	client.slowWriteUntil.Store(time.Now().Add(time.Minute).UnixNano())
	if got := hub.controlTimeout(client); got < time.Minute {
		t.Errorf("a snapshot write has a minute left and a ping is given %v", got)
	}

	client.slowWriteUntil.Store(time.Now().Add(-time.Minute).UnixNano())
	if got := hub.controlTimeout(client); got != wsWriteTimeout {
		t.Errorf("a write whose deadline has passed still extends the timeout to %v", got)
	}
}
