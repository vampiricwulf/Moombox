package worker

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

// TestTwitchVodQueueingKeepsItsStatus is W20-25: a Twitch VOD that has to
// queue for a download slot keeps the status it came in with — Upcoming —
// under the slot-wait progress line, as a YouTube VOD does. processTwitchVod
// wrote Downloading before the wait, so a Twitch VOD behind a busy pool read
// Downloading, "Waiting for a download slot...", with no slot held.
//
// Mutant: put `"status": database.StatusDownloading` back in
// processTwitchVod's vodUpdates — the queueing row reads Downloading.
func TestTwitchVodQueueingKeepsItsStatus(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/gql":
			b, _ := io.ReadAll(r.Body)
			if strings.Contains(string(b), "videoPlaybackAccessToken") {
				io.WriteString(rw, `{"data":{"videoPlaybackAccessToken":{"value":"v","signature":"s"}}}`)
				return
			}
			io.WriteString(rw, `{"data":{"video":{"id":"123","title":"A VOD","lengthSeconds":60,"createdAt":"2026-10-01T00:00:00Z","owner":{"id":"1","login":"streamer","displayName":"Streamer"}}}}`)
		case strings.HasPrefix(r.URL.Path, "/vod/"):
			io.WriteString(rw, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000000,RESOLUTION=1280x720,FRAME-RATE=30.000,VIDEO=\"720p30\"\n"+srv.URL+"/720p30/index-dvr.m3u8\n")
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(srv.Close)
	oldGQL, oldUsher := constants.TwitchURLs.GQL, constants.TwitchURLs.UsherVOD
	constants.TwitchURLs.GQL = srv.URL + "/gql"
	constants.TwitchURLs.UsherVOD = srv.URL + "/vod"
	t.Cleanup(func() { constants.TwitchURLs.GQL, constants.TwitchURLs.UsherVOD = oldGQL, oldUsher })

	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := &config.MoomboxConfig{}
	cfg.Paths.StagingDirectory = filepath.Join(dir, "staging")
	cfg.Downloader.NumParallelDownloads = 1
	w := NewDownloadWorker(db, nil, cfg, &discardLogger{}, &DownloadWorkerDeps{TwitchService: twitch.NewService(nil, &discardLogger{})})

	if _, err := db.AddJob(&database.Job{ID: "tw_v123", VideoID: "tw_v123", URL: "https://www.twitch.tv/videos/123", Platform: "twitch", Status: database.StatusUpcoming}); err != nil {
		t.Fatal(err)
	}
	if !w.queue.TryAcquireDownloadSlot("busy") { // the pool of one is full
		t.Fatal("could not fill the download pool")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("processJob panicked: %v", r)
			}
		}()
		w.processJob(ctx, "tw_v123")
	}()
	defer func() {
		cancel()
		<-done
	}()

	deadline := time.Now().Add(20 * time.Second)
	for {
		row, _ := db.GetJob("tw_v123")
		if row != nil && row.Progress == vodSlotWaitProgress {
			if row.Status != database.StatusUpcoming {
				t.Errorf("the queueing Twitch VOD reads %s, want the Upcoming it came in with", row.Status)
			}
			if !row.IsVod || row.Title == "" {
				t.Errorf("the VOD's metadata was not written before the wait: is_vod %v, title %q", row.IsVod, row.Title)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never reached the slot wait: status %s, progress %q, error %q", row.Status, row.Progress, row.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
