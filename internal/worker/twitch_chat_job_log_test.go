package worker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// Per-job log routing (database.RouteLogToJobs) matches the job ID as a
// substring of the formatted line. The Twitch chat downloaders log
// channel=<login> or vodID=<digits> and know nothing of jobs, so the stream
// processor hands each a logger that adds jobID=<id> to every line; without
// it their reconnects, auth downgrades and fetch errors never reached the
// job's own log.

// linesWithoutJobID returns the captured lines that do not carry
// jobID=<jobID>, and how many lines there were.
func linesWithoutJobID(logs *captureLogger, jobID string) (missing [][]any, total int) {
	for _, line := range logs.lines() {
		total++
		found := false
		for i := 1; i+1 < len(line); i += 2 {
			if line[i] == "jobID" && line[i+1] == jobID {
				found = true
			}
		}
		if !found {
			missing = append(missing, line)
		}
	}
	return missing, total
}

// Mutant: the VOD site passing sp.logger again.
func TestTwitchVodChatDownloaderLogsWithTheJobID(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/gql":
			b, _ := io.ReadAll(r.Body)
			if strings.Contains(string(b), "videoPlaybackAccessToken") {
				io.WriteString(rw, `{"data":{"videoPlaybackAccessToken":{"value":"v","signature":"s"}}}`)
				return
			}
			io.WriteString(rw, `[{"data":{"video":{"id":"123","title":"A VOD","lengthSeconds":60,"createdAt":"2026-10-01T00:00:00Z","owner":{"id":"1","login":"streamer","displayName":"Streamer"}}}},`+
				`{"data":{"video":{"moments":{"edges":[]}}}}]`)
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

	db := openTestDB(t)
	job := &database.Job{ID: "tw_v123", VideoID: "tw_v123", Platform: "twitch", Status: database.StatusUpcoming}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	cfg := &config.MoomboxConfig{}
	cfg.Paths.StagingDirectory = t.TempDir()
	cfg.Downloader.DownloadChat = true
	logs := &captureLogger{}
	sp := NewStreamProcessor(nil, twitch.NewService(nil, discardLogger{}), cfg, db, logs)
	res, err := sp.processTwitchVod(context.Background(), job)
	if err != nil || res == nil || res.TwitchVodChatDl == nil {
		t.Fatalf("processTwitchVod = %+v, %v; want a VOD chat downloader", res, err)
	}

	// A run that is cancelled before its first page still logs its start
	// and its stop, and fetches nothing.
	logs.reset()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = res.TwitchVodChatDl.Start(ctx)

	missing, total := linesWithoutJobID(logs, job.ID)
	if total == 0 {
		t.Fatal("the VOD chat downloader logged nothing; the test has no line to check")
	}
	if len(missing) > 0 {
		t.Errorf("%d of %d VOD chat lines carry no jobID=%s, so they never reach the job's log: %v",
			len(missing), total, job.ID, missing)
	}
}

// Mutant: the live site passing sp.logger again.
func TestTwitchLiveChatDownloaderLogsWithTheJobID(t *testing.T) {
	db := openTestDB(t)
	job := &database.Job{ID: "tw_555", VideoID: "tw_555", URL: "https://twitch.tv/streamer",
		Platform: "twitch", Status: database.StatusUpcoming}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Paths.StagingDirectory = t.TempDir()
	cfg.Downloader.DownloadChat = true
	logs := &captureLogger{}
	sp := NewStreamProcessor(nil, twitch.NewService(nil, discardLogger{}), cfg, db, logs)
	sp.hlsMasterFn = func(context.Context, string) ([]twitch.TwitchHLSVariant, bool, error) {
		return []twitch.TwitchHLSVariant{{Name: "720p30", Width: 1280, Height: 720,
			URL: "http://127.0.0.1:1/720p30.m3u8"}}, false, nil
	}
	sp.StashTwitchStreamInfo(&twitch.TwitchStreamInfo{IsLive: true, ChannelLogin: "streamer",
		ChannelDisplayName: "Streamer", StreamID: "555", StartedAt: "2026-10-02T18:00:00Z"})

	res, err := sp.processTwitchLive(context.Background(), job, "streamer")
	if err != nil || res == nil || res.TwitchChatDownloader == nil {
		t.Fatalf("processTwitchLive = %+v, %v; want a chat downloader", res, err)
	}

	// Reauthenticate on a downloader that is not running logs one line and
	// opens no connection.
	logs.reset()
	res.TwitchChatDownloader.Reauthenticate()

	missing, total := linesWithoutJobID(logs, job.ID)
	if total == 0 {
		t.Fatal("the chat downloader logged nothing; the test has no line to check")
	}
	if len(missing) > 0 {
		t.Errorf("%d of %d Twitch chat lines carry no jobID=%s, so they never reach the job's log: %v",
			len(missing), total, job.ID, missing)
	}
}
