package web

import (
	"encoding/json"
	"slices"
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
// only from job_update, never from job_progress), which is followed by the
// newer tick again (TestHubSendsAWriteATickOvertook).
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
	// goes out, because it is the row the clients will hold, and the tick
	// follows it again, so the row they hold ends on the newer status.
	hub.BroadcastJobProgress(versionedFrame{ID: "k", Status: "Downloading", version: 9})
	hub.BroadcastJobUpdate(versionedFrame{ID: "k", Status: "Upcoming", version: 8})
	got = drainTypes(t, c)
	if len(got) != 3 || got[1] != "job_update:Upcoming" || got[2] != "job_progress:Downloading" {
		t.Errorf("frames = %v, want the introducing job_update sent despite its age, then the newer tick again", got)
	}

	// After a delete the job starts over (a re-added video reuses its ID).
	hub.BroadcastJobDeleted("j")
	drainTypes(t, c)
	hub.BroadcastJobUpdate(versionedFrame{ID: "j", Status: "Upcoming", version: 1})
	if got := drainTypes(t, c); len(got) != 1 {
		t.Errorf("a re-added job's first frame was dropped: %v", got)
	}
}

// titledFrame is versionedFrame with a column a job_progress does not carry.
type titledFrame struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Title   string `json:"title,omitempty"`
	version uint64
}

func (f titledFrame) JobVersion() (string, uint64) { return f.ID, f.version }

// TestHubSendsAWriteATickOvertook: a title or twitch_quality write whose
// job_update the next progress tick overtook. The tick's frame carries the
// progress columns and the status, not the title, so dropping the older
// job_update behind it, as the hub did behind any newer frame, lost the write
// on every tab until a resync. It is sent, and the tick after it again, so the
// clients' progress columns and status end on the newest write. A job_update
// is still dropped behind a newer job_update, and a job_progress behind any
// newer frame.
//
// Mutants: a job_update dropped behind any newer frame again (the title write
// is not sent); the tick not restated (the clients end on the write's older
// progress); the tick restated after a job_update newer than it (an extra
// frame); a job_progress judged against the newest job_update alone (the
// stale tick after Muxing is sent); the cached tick kept once a newer
// job_update or the job's deletion supersedes it (the map check).
func TestHubSendsAWriteATickOvertook(t *testing.T) {
	hub := NewWebSocketHub(nil)
	c := attachTestClient(hub, 32)
	frames := func() []string {
		var out []string
		for {
			select {
			case b := <-c.writes:
				var m struct {
					Type    string      `json:"type"`
					Payload titledFrame `json:"payload"`
				}
				if err := json.Unmarshal(b, &m); err != nil {
					t.Fatal(err)
				}
				out = append(out, m.Type+":"+m.Payload.Title)
			default:
				return out
			}
		}
	}

	hub.BroadcastJobUpdate(titledFrame{ID: "j", Status: "Downloading", Title: "old", version: 1})
	frames()
	hub.BroadcastJobProgress(titledFrame{ID: "j", Status: "Downloading", version: 3})             // the tick, delivered first
	hub.BroadcastJobUpdate(titledFrame{ID: "j", Status: "Downloading", Title: "new", version: 2}) // the write it overtook
	want := []string{"job_progress:", "job_update:new", "job_progress:"}
	if got := frames(); !slices.Equal(got, want) {
		t.Errorf("frames %v, want %v: the write, then the newer tick restated", got, want)
	}

	// A duplicate of the write, and the tick again, are both stale now.
	hub.BroadcastJobUpdate(titledFrame{ID: "j", Status: "Downloading", Title: "new", version: 2})
	hub.BroadcastJobProgress(titledFrame{ID: "j", Status: "Downloading", version: 3})
	if got := frames(); len(got) != 0 {
		t.Errorf("frames %v for a write and a tick already sent", got)
	}

	// A job_update newer than every tick is sent once, and supersedes the
	// cached tick: a later, older tick is dropped and nothing is restated.
	hub.BroadcastJobUpdate(titledFrame{ID: "j", Status: "Muxing", Title: "new", version: 5})
	hub.BroadcastJobProgress(titledFrame{ID: "j", Status: "Downloading", version: 4})
	if got := frames(); !slices.Equal(got, []string{"job_update:new"}) {
		t.Errorf("frames %v, want the Muxing row alone", got)
	}
	hub.jobVerMu.Lock()
	_, cached := hub.jobLastTick["j"]
	hub.jobVerMu.Unlock()
	if cached {
		t.Error("the hub still holds a tick for j after a newer job_update superseded it")
	}

	hub.BroadcastJobProgress(titledFrame{ID: "j", Status: "Muxing", version: 6})
	hub.BroadcastJobDeleted("j")
	frames()
	hub.jobVerMu.Lock()
	_, cached = hub.jobLastTick["j"]
	hub.jobVerMu.Unlock()
	if cached {
		t.Error("the hub still holds a tick for j after its job_deleted")
	}
}
