package monitor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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
// The channel's title terms apply, as to its live broadcasts: "picky" asks
// for karaoke, and only its karaoke VOD is queued.
//
// Mutants: the opt-in check dropped ("plain" listed); the enabled check
// dropped ("off" listed); SkipHistory not set (tw_v2, whose job was deleted,
// queued again); the captured-broadcast skip dropped (tw_v3 queued); the
// window's side before the stream start dropped (tw_v4, published five
// minutes ahead of it, queued); the terms not applied (tw_v7 queued);
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
		{ID: "picky", Platform: "twitch", TwitchVodBacklog: true, Terms: config.ChannelTerms{Simple: "karaoke"}},
	}
	store := config.NewStore(cfg, filepath.Join(t.TempDir(), "config.toml"))

	// The broadcast captured live started at 12:00; its archive (3) was
	// published four seconds later.
	if _, err := db.AddJob(&database.Job{ID: "tw_live", VideoID: "live", URL: "https://twitch.tv/streamer",
		Platform: "twitch", Status: database.StatusFinished, StreamStartTime: "2026-10-03T12:00:00Z",
		OutputFile: "/out/streamer.mp4"}); err != nil {
		t.Fatal(err)
	}
	db.AddToHistory("tw_v2") // a job was created once, and since deleted

	listed := map[string]bool{}
	b := &TwitchBacklog{configStore: store, db: db, logger: &testMonitorLogger{}}
	b.ListVideos = func(_ context.Context, login string) ([]twitch.TwitchVideo, error) {
		listed[login] = true
		if login == "picky" {
			return []twitch.TwitchVideo{
				{ID: "8", Title: "karaoke night", PublishedAt: "2026-10-01T20:00:00Z"},
				{ID: "7", Title: "minecraft all night", PublishedAt: "2026-10-01T10:00:00Z"},
			}, nil
		}
		return []twitch.TwitchVideo{
			{ID: "4", Title: "published a little early", PublishedAt: "2026-10-03T11:55:00Z"},
			{ID: "3", Title: "the captured one", PublishedAt: "2026-10-03T12:00:04Z"},
			{ID: "2", Title: "seen before", PublishedAt: "2026-10-02T12:00:00Z"},
			{ID: "1", Title: "missed", PublishedAt: "2026-10-01T12:00:00Z"},
		}, nil
	}
	woken := 0
	b.OnQueued = func(n int) { woken += n }

	b.RunCycle(context.Background(), time.Now())

	if !listed["streamer"] || !listed["picky"] || listed["plain"] || listed["off"] {
		t.Errorf("listed %v, want only the opted-in, enabled channel", listed)
	}
	if j, _ := db.GetJob("tw_v1"); j == nil || j.Status != database.StatusQueued || j.QueuePriority != 1 || j.ManuallyAdded {
		t.Errorf("tw_v1 = %+v, want a Queued backlog row", j)
	}
	for _, id := range []string{"tw_v2", "tw_v3", "tw_v4", "tw_v7"} {
		if db.JobExists(id) {
			t.Errorf("%s queued, want it skipped", id)
		}
	}
	if !db.JobExists("tw_v8") {
		t.Error("picky's karaoke VOD was not queued")
	}
	if woken != 2 {
		t.Errorf("OnQueued told %d, want 2", woken)
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
	// The window is the gap-fill lookup's (owner decision PR-M3): the two
	// ask whether a video is one broadcast's archive.
	if twitchBacklogCapturedWindow != 10*time.Minute {
		t.Errorf("captured window %v, want 10 minutes, the archive lookup's", twitchBacklogCapturedWindow)
	}
}

// backlogLogLines records the Info lines a backlog cycle logs.
type backlogLogLines struct {
	testMonitorLogger
	info []string
}

func (l *backlogLogLines) Info(msg string, args ...any) {
	l.info = append(l.info, fmt.Sprint(append([]any{msg}, args...)...))
}

// TestTwitchBacklogCountsOnlyRealCaptures is owner decision PR-M4: a live
// job stands for its broadcast's archive — and the backlog skips the VOD —
// only when it holds footage (an archive file, recorded parts, staged media),
// was Cancelled, already has a gap-fill plan, or is still capturing. A
// capture that failed or parked in COOKIES? before its first segment holds
// nothing, and the VOD, the broadcast's only copy, is queued. A skip logs the
// job it deferred to, once.
//
// Mutants: every live row counted as before (tw_v10 and tw_v11 skipped); the
// staging check dropped (tw_v13 queued beside the staged capture); the
// segments check dropped (tw_v14 queued); the gap-fill check dropped (tw_v15
// queued); the in-progress statuses not counted (tw_v17 queued beside the
// running capture); the skip's log line dropped, or logged every cycle.
func TestTwitchBacklogCountsOnlyRealCaptures(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	staging := t.TempDir()
	cfg := config.Defaults()
	cfg.Paths.StagingDirectory = staging
	cfg.Channels = []config.ChannelConfig{{ID: "streamer", Platform: "twitch", TwitchVodBacklog: true}}
	store := config.NewStore(cfg, filepath.Join(t.TempDir(), "config.toml"))

	live := func(id string, hour int, status database.JobStatus, mut func(*database.Job)) {
		j := &database.Job{ID: id, VideoID: id, URL: "https://twitch.tv/streamer", Platform: "twitch",
			Status: status, StreamStartTime: fmt.Sprintf("2026-10-03T%02d:00:00Z", hour)}
		if mut != nil {
			mut(j)
		}
		if _, err := db.AddJob(j); err != nil {
			t.Fatal(err)
		}
	}
	live("tw_err_empty", 10, database.StatusError, nil)
	live("tw_cookies_empty", 11, database.StatusCookies, nil)
	live("tw_err_staged", 13, database.StatusError, nil)
	if err := os.MkdirAll(filepath.Join(staging, "tw_err_staged"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "tw_err_staged", "video.ts"), []byte("ts"), 0o644); err != nil {
		t.Fatal(err)
	}
	live("tw_err_parts", 14, database.StatusError, nil)
	if err := db.AddSegment(&database.Segment{JobID: "tw_err_parts", Filename: "p0.mp4", FilePath: "/out/p0.mp4"}); err != nil {
		t.Fatal(err)
	}
	live("tw_err_plan", 15, database.StatusError, nil)
	if err := db.SaveGapFill(database.GapFill{JobID: "tw_err_plan", Plan: "{}", NextCheckAt: "2026-10-03T16:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	live("tw_cancelled", 16, database.StatusCancelled, nil)
	live("tw_running", 17, database.StatusDownloading, nil)
	live("tw_finished", 18, database.StatusFinished, func(j *database.Job) { j.OutputFile = "/out/x.mp4" })

	lg := &backlogLogLines{}
	b := &TwitchBacklog{configStore: store, db: db, logger: lg}
	b.ListVideos = func(context.Context, string) ([]twitch.TwitchVideo, error) {
		var out []twitch.TwitchVideo
		for _, h := range []int{18, 17, 16, 15, 14, 13, 11, 10} {
			out = append(out, twitch.TwitchVideo{ID: strconv.Itoa(h), Title: "b", PublishedAt: fmt.Sprintf("2026-10-03T%02d:00:03Z", h)})
		}
		return out, nil
	}
	b.RunCycle(context.Background(), time.Now())

	for _, id := range []string{"tw_v10", "tw_v11"} {
		if !db.JobExists(id) {
			t.Errorf("%s not queued; its live capture recorded nothing", id)
		}
	}
	for _, id := range []string{"tw_v13", "tw_v14", "tw_v15", "tw_v16", "tw_v17", "tw_v18"} {
		if db.JobExists(id) {
			t.Errorf("%s queued; its live capture holds the broadcast", id)
		}
	}
	deferred := func() int {
		n := 0
		for _, l := range lg.info {
			if strings.Contains(l, "its live capture holds it") && strings.Contains(l, "tw_err_staged") {
				n++
			}
		}
		return n
	}
	if n := deferred(); n != 1 {
		t.Errorf("the skip for tw_v13 logged %d times naming tw_err_staged, want once; lines %q", n, lg.info)
	}
	b.RunCycle(context.Background(), time.Now())
	if n := deferred(); n != 1 {
		t.Errorf("after a second cycle the skip logged %d times, want once", n)
	}
}
