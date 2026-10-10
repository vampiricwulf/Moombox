package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// The broadcast-identity guard used to apply only to a Downloading row. A
// manually-added job interrupted mid-broadcast and restarted while the channel
// was offline waits as Upcoming, so a second restart once the channel came
// back with a NEW broadcast skipped the guard and appended that broadcast to
// the old one's staged footage, under the old stream_start_time. Any job that
// holds a capture is guarded now.
//
// The stashed monitor hint stands in for the stream lookup; nothing past the
// guard is wired, so attaching shows up as a panic the test reports.
//
// Mutant: the guard keyed on Downloading again.
func TestTwitchGuardRefusesANewBroadcastForAnUpcomingJobWithACapture(t *testing.T) {
	staging := t.TempDir()
	cfg := config.Defaults()
	cfg.Paths.StagingDirectory = staging
	db, err := database.Open(filepath.Join(t.TempDir(), "guard.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	sp := &StreamProcessor{db: db, cfg: cfg, twitchHints: newTwitchHintCache(), logger: nopWorkerLogger{}}

	job := &database.Job{ID: "tw_guard", VideoID: "tw_guard", URL: "https://twitch.tv/streamer",
		Platform: "twitch", Status: database.StatusUpcoming, ManuallyAdded: true,
		StreamStartTime: "2026-10-01T10:00:00Z"}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(staging, job.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, job.ID, "video_stream"), []byte("old broadcast"), 0o644); err != nil {
		t.Fatal(err)
	}
	sp.StashTwitchStreamInfo(&twitch.TwitchStreamInfo{IsLive: true, ChannelLogin: "streamer",
		StreamID: "2", StartedAt: "2026-10-02T18:00:00Z"})

	var res *StreamProcessResult
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("processTwitchLive went on to attach the new broadcast (%v)", r)
			}
		}()
		var err error
		res, err = sp.processTwitchLive(context.Background(), job, "streamer")
		if err != nil {
			t.Fatalf("processTwitchLive: %v", err)
		}
	}()
	if res == nil || res.ShouldDownload || !strings.Contains(res.Error, "new broadcast") {
		t.Errorf("result = %+v, want the new-broadcast refusal", res)
	}
}
