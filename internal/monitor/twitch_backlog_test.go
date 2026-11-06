package monitor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// TestTwitchBacklogQueuesNewVods is owner decision D18's monitor backlog: an
// opted-in, enabled Twitch channel's videos are queued as Queued backlog VODs
// — except one already in history, and the archive of a broadcast a live job
// of the channel captured — and the scheduler is woken once; a channel that
// did not opt in, or is disabled, is not listed at all.
//
// Mutants: the opt-in check dropped ("plain" listed); the enabled check
// dropped ("off" listed); SkipHistory not set (tw_v2, whose job was deleted,
// queued again); the captured-broadcast skip dropped (tw_v3 queued);
// OnQueued not called.
func TestTwitchBacklogQueuesNewVods(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	off := false
	cfg := config.Defaults()
	cfg.Channels = []config.ChannelConfig{
		{ID: "streamer", Platform: "twitch", TwitchVodBacklog: true},
		{ID: "plain", Platform: "twitch"},
		{ID: "off", Platform: "twitch", Enabled: &off, TwitchVodBacklog: true},
	}
	store := config.NewStore(cfg, filepath.Join(t.TempDir(), "config.toml"))

	// The broadcast captured live started at 12:00; its archive (3) was
	// published four seconds later.
	if _, err := db.AddJob(&database.Job{ID: "tw_live", VideoID: "live", URL: "https://twitch.tv/streamer",
		Platform: "twitch", Status: database.StatusFinished, StreamStartTime: "2026-10-03T12:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	db.AddToHistory("tw_v2") // a job was created once, and since deleted

	listed := map[string]bool{}
	b := &TwitchBacklog{configStore: store, db: db, logger: &testMonitorLogger{}}
	b.ListVideos = func(_ context.Context, login string) ([]twitch.TwitchVideo, error) {
		listed[login] = true
		return []twitch.TwitchVideo{
			{ID: "3", Title: "the captured one", PublishedAt: "2026-10-03T12:00:04Z"},
			{ID: "2", Title: "seen before", PublishedAt: "2026-10-02T12:00:00Z"},
			{ID: "1", Title: "missed", PublishedAt: "2026-10-01T12:00:00Z"},
		}, nil
	}
	woken := 0
	b.OnQueued = func(n int) { woken += n }

	b.RunCycle(context.Background(), time.Now())

	if !listed["streamer"] || listed["plain"] || listed["off"] {
		t.Errorf("listed %v, want only the opted-in, enabled channel", listed)
	}
	if j, _ := db.GetJob("tw_v1"); j == nil || j.Status != database.StatusQueued || j.QueuePriority != 1 || j.ManuallyAdded {
		t.Errorf("tw_v1 = %+v, want a Queued backlog row", j)
	}
	for _, id := range []string{"tw_v2", "tw_v3"} {
		if db.JobExists(id) {
			t.Errorf("%s queued, want it skipped", id)
		}
	}
	if woken != 1 {
		t.Errorf("OnQueued told %d, want 1", woken)
	}

	// A second cycle finds nothing new and wakes nothing.
	woken = 0
	b.RunCycle(context.Background(), time.Now())
	if woken != 0 {
		t.Errorf("second cycle told %d, want nothing", woken)
	}
}

// TestTwitchBacklogProductionLister pins NewTwitchBacklog's ListVideos: one
// page of FilterableVideoTower_Videos for the channel, every type (null),
// newest first — a newest-videos check, not a catalogue walk — and the
// hourly cadence.
//
// Mutants: the page bound raised (a second page is asked for); a type filter
// passed (broadcastType not null).
func TestTwitchBacklogProductionLister(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		io.WriteString(rw, `{"data":{"user":{"id":"1","videos":{"edges":[{"__typename":"VideoEdge","cursor":"c1","node":{"__typename":"Video","id":"1","publishedAt":"2026-10-01T00:00:00Z"}}]}}}}`)
	}))
	t.Cleanup(srv.Close)
	old := constants.TwitchURLs.GQL
	constants.TwitchURLs.GQL = srv.URL
	t.Cleanup(func() { constants.TwitchURLs.GQL = old })

	b := NewTwitchBacklog(nil, nil, twitch.NewService(nil, &testMonitorLogger{}), &testMonitorLogger{})
	vids, err := b.ListVideos(context.Background(), "streamer")
	if err != nil || len(vids) != 1 {
		t.Fatalf("ListVideos = %+v, %v", vids, err)
	}
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"broadcastType":null`) || !strings.Contains(bodies[0], `"videoSort":"TIME"`) {
		t.Errorf("requests %q, want one page, every type, newest first", bodies)
	}
	if twitchBacklogInterval != time.Hour {
		t.Errorf("cadence %v, want an hour", twitchBacklogInterval)
	}
}
