package web

import (
	"encoding/json"
	"testing"
)

// versionedFrame is a stand-in for *database.Job / the progress frame: the
// hub only needs JobVersion.
type versionedFrame struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	version uint64
}

func (f versionedFrame) JobVersion() (string, uint64) { return f.ID, f.version }

func drainTypes(t *testing.T, c *wsClient) []string {
	t.Helper()
	var out []string
	for {
		select {
		case b := <-c.writes:
			var m struct {
				Type    string         `json:"type"`
				Payload versionedFrame `json:"payload"`
			}
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			out = append(out, m.Type+":"+m.Payload.Status)
		default:
			return out
		}
	}
}

// TestHubDropsAJobFrameOlderThanOneSent: the database notifies after
// releasing its lock, so a progress tick read back before a Muxing write can
// reach the hub after it — and the hub sent it, putting Downloading back on
// every tab for the whole mux. A frame older than one already sent is dropped,
// except the job_update that first introduces a job's row (clients add rows
// only from job_update, never from job_progress).
//
// Mutants: dropping the version check — the stale progress frame is sent;
// dropping the introduces exception — the overtaken JobAdded row is lost.
func TestHubDropsAJobFrameOlderThanOneSent(t *testing.T) {
	hub := NewWebSocketHub(nil)
	c := attachTestClient(hub, 32)

	hub.BroadcastJobUpdate(versionedFrame{ID: "j", Status: "Downloading", version: 5})
	hub.BroadcastJobUpdate(versionedFrame{ID: "j", Status: "Muxing", version: 7})
	hub.BroadcastJobProgress(versionedFrame{ID: "j", Status: "Downloading", version: 6})
	hub.BroadcastJobUpdate(versionedFrame{ID: "j", Status: "Downloading", version: 6})
	got := drainTypes(t, c)
	if len(got) != 2 || got[1] != "job_update:Muxing" {
		t.Errorf("frames = %v, want the two in-order updates and neither stale one", got)
	}

	// A job whose first progress tick overtook its JobAdded: the add still
	// goes out, because it is the row the clients will hold.
	hub.BroadcastJobProgress(versionedFrame{ID: "k", Status: "Downloading", version: 9})
	hub.BroadcastJobUpdate(versionedFrame{ID: "k", Status: "Upcoming", version: 8})
	got = drainTypes(t, c)
	if len(got) != 2 || got[1] != "job_update:Upcoming" {
		t.Errorf("frames = %v, want the introducing job_update sent despite its age", got)
	}

	// After a delete the job starts over (a re-added video reuses its ID).
	hub.BroadcastJobDeleted("j")
	drainTypes(t, c)
	hub.BroadcastJobUpdate(versionedFrame{ID: "j", Status: "Upcoming", version: 1})
	if got := drainTypes(t, c); len(got) != 1 {
		t.Errorf("a re-added job's first frame was dropped: %v", got)
	}
}
