package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// The chapter and thumbnail tests that reach the network stack: an HTTP
// fetch through the shared transport leaves a keep-alive connection whose
// read loop calls time.Now, and a real twitch.API starts a GQL rate-limiter
// ticker that lives for the rest of the test binary. Both read time.Local,
// which TestFilenamesUseTheHostsLocalDate assigns, so they sit in a file that
// sorts after filename_localtime_test.go, as twitch_vod_queue_status_test.go
// and orchestrator_mux_test.go do; the race detector reports the pair
// otherwise.

// TestMuxAndFinalizeEmbedsTheChapters runs the real finalize over a real
// six-second capture: a Twitch VOD's staged chapters, and a YouTube VOD's
// from the chapter source, come out of the mux as the archive's chapters,
// fitted to the media (the last one ends at six seconds, one past the end is
// gone). A YouTube job that is not a VOD is not asked.
//
// Mutants: muxAndFinalize passing "" for the chapters; the staged list not
// read; the IsVod gate removed (the live YouTube job is asked).
func TestMuxAndFinalizeEmbedsTheChapters(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	for _, tc := range []struct {
		name      string
		platform  string
		isVod     bool
		staged    []utils.Chapter
		wantTitle []string
	}{
		{"twitch VOD", "twitch", true, []utils.Chapter{{StartMs: 0, EndMs: 2000, Title: "Chatting"}, {StartMs: 2000, EndMs: 60_000, Title: "Game"}, {StartMs: 90_000, Title: "Past"}}, []string{"Chatting", "Game"}},
		{"youtube VOD", "youtube", true, nil, []string{"Intro", "Main"}},
		{"youtube live capture", "youtube", false, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked := false
			orig := youtubeChapters
			youtubeChapters = func(_ context.Context, _ *youtube.Service, videoID, desc string, durationMs int64) []utils.Chapter {
				asked = true
				if durationMs < 5500 || durationMs > 6500 {
					t.Errorf("the chapter source was given %d ms, want the staged media's six seconds", durationMs)
				}
				return []utils.Chapter{{StartMs: 0, Title: "Intro"}, {StartMs: 3000, Title: "Main"}}
			}
			t.Cleanup(func() { youtubeChapters = orig })

			o, jobCtx := muxTestOrchestrator(t, notificationtest.New(), 0)
			o.muxer = engine.NewMuxer(ffmpegPath, discardLogger{})
			o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{"platform": tc.platform, "is_vod": tc.isVod})
			jobCtx.Job.Platform, jobCtx.Job.IsVod = tc.platform, tc.isVod
			jobCtx.Config = &JobConfig{}
			jobCtx.YT = &youtube.Service{}
			jobCtx.StagingDir = t.TempDir()
			jobCtx.OutputDir = t.TempDir()
			jobCtx.Filename = "a-job"
			video := filepath.Join(jobCtx.StagingDir, "video.mp4")
			writeMuxFixture(t, ffmpegPath, video, 6)
			if err := utils.WriteStagedChapters(jobCtx.StagingDir, tc.staged); err != nil {
				t.Fatal(err)
			}
			if err := o.muxAndFinalize(context.Background(), jobCtx, &DownloadResult{HasVideo: true, VideoPath: video}); err != nil {
				t.Fatalf("muxAndFinalize: %v", err)
			}
			if asked != (tc.platform == "youtube" && tc.isVod) {
				t.Errorf("the YouTube chapter source asked: %v", asked)
			}
			titles, lastEnd := probeChapters(t, filepath.Join(jobCtx.OutputDir, "a-job.mp4"))
			if !reflect.DeepEqual(titles, tc.wantTitle) {
				t.Errorf("chapter titles = %q, want %q", titles, tc.wantTitle)
			}
			if len(titles) > 0 && (lastEnd < 5.5 || lastEnd > 6.5) {
				t.Errorf("the last chapter ends at %.3f s, want the archive's six seconds", lastEnd)
			}
		})
	}
}

// probeChapters reads an output's chapter titles and the last chapter's end.
func probeChapters(t *testing.T, path string) ([]string, float64) {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "quiet", "-print_format", "json", "-show_chapters", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var got struct {
		Chapters []struct {
			EndTime string            `json:"end_time"`
			Tags    map[string]string `json:"tags"`
		} `json:"chapters"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	var titles []string
	var end float64
	for _, c := range got.Chapters {
		titles = append(titles, c.Tags["title"])
		end = parseFloatOr0(c.EndTime)
	}
	return titles, end
}
func parseFloatOr0(s string) float64 {
	var f float64
	_ = json.Unmarshal([]byte(s), &f)
	return f
}

// TestDownloadTwitchThumbnailTriesTheOriginalSizeFirst is G45: the 0x0 form
// first, and the sized URL when the original is refused or too small to be an
// image.
//
// Mutants: the 0x0 attempt removed (the sized bytes win the first case); the
// fallback removed (the second and third cases save nothing).
func TestDownloadTwitchThumbnailTriesTheOriginalSizeFirst(t *testing.T) {
	for _, tc := range []struct {
		name     string
		original func(http.ResponseWriter)
		want     string
	}{
		{"original served", func(w http.ResponseWriter) { w.Write(bytes.Repeat([]byte("O"), 3000)) }, "O"},
		{"original refused", func(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) }, "S"},
		{"original too small", func(w http.ResponseWriter) { w.Write([]byte("tiny")) }, "S"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "-0x0.jpg") {
					tc.original(w)
					return
				}
				w.Write(bytes.Repeat([]byte("S"), 2000))
			}))
			t.Cleanup(srv.Close)
			dst := filepath.Join(t.TempDir(), "thumb.jpg")
			if err := downloadTwitchThumbnail(context.Background(), srv.URL+"/live_user_x-640x360.jpg", dst, discardLogger{}); err != nil {
				t.Fatal(err)
			}
			body, _ := os.ReadFile(dst)
			if len(body) == 0 || string(body[:1]) != tc.want {
				t.Errorf("saved %d bytes starting %q, want the %s image", len(body), body[:min(1, len(body))], tc.want)
			}
		})
	}
}

// TestCopyAssetsAsksForTheTwitchThumbnailsOriginalSize: a Twitch job's own
// thumbnail URL is fetched at 0x0 first (G45); its avatar fallback and a
// YouTube job's URL are fetched as they are.
//
// Mutant: copyAssets calling DownloadFileMinSize for the Twitch URL.
func TestCopyAssetsAsksForTheTwitchThumbnailsOriginalSize(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Path)
		mu.Unlock()
		w.Write(bytes.Repeat([]byte("x"), 2000))
	}))
	t.Cleanup(srv.Close)
	o := &DownloadOrchestrator{logger: discardLogger{}}
	jobCtx := &JobContext{Job: &database.Job{ID: "tw_thumb", Platform: "twitch", ThumbnailURL: srv.URL + "/thumb0-640x360.jpg"}, StagingDir: t.TempDir()}
	o.copyAssets(context.Background(), jobCtx, t.TempDir(), "A Job", "A Job", map[string]any{}, false)
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || asked[0] != "/thumb0-0x0.jpg" {
		t.Errorf("asked %v, want the original size alone", asked)
	}
}

// TestProcessTwitchVodStoresTheDescriptionAndChapters is G68: the VOD's
// description goes to the row (and so beside the archive as .description),
// and its chapters to the result ExecuteTwitch stages them from.
//
// Mutants: the description not written; TwitchVodChapters not set.
func TestProcessTwitchVodStoresTheDescriptionAndChapters(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/gql":
			b, _ := io.ReadAll(r.Body)
			if strings.Contains(string(b), "videoPlaybackAccessToken") {
				io.WriteString(rw, `{"data":{"videoPlaybackAccessToken":{"value":"v","signature":"s"}}}`)
				return
			}
			io.WriteString(rw, `[{"data":{"video":{"id":"123","title":"A VOD","description":"Uploader's notes.","lengthSeconds":60,"createdAt":"2026-10-01T00:00:00Z","owner":{"id":"1","login":"streamer","displayName":"Streamer"}}}},`+
				`{"data":{"video":{"moments":{"edges":[{"node":{"positionMilliseconds":0,"durationMilliseconds":60000,"description":"Art"}}]}}}}]`)
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
	sp := NewStreamProcessor(nil, twitch.NewService(nil, discardLogger{}), cfg, db, discardLogger{})
	res, err := sp.processTwitchVod(context.Background(), job)
	if err != nil || res == nil || !res.ShouldDownload {
		t.Fatalf("processTwitchVod = %+v, %v", res, err)
	}
	row, _ := db.GetJob(job.ID)
	if row.Description != "Uploader's notes." || job.Description != row.Description {
		t.Errorf("description: row %q, job %q; want the VOD's", row.Description, job.Description)
	}
	want := []utils.Chapter{{StartMs: 0, EndMs: 60000, Title: "Art"}}
	if !reflect.DeepEqual(res.TwitchVodChapters, want) {
		t.Errorf("TwitchVodChapters = %+v, want %+v", res.TwitchVodChapters, want)
	}
}

// TestTwitchArchiveChaptersLooksUpTheVod: the archive VOD is found by the
// broadcast's start (FilterableVideoTower_Videos) and its chapters read
// (GetVodInfo); a channel with no VOD of the broadcast has none.
//
// Mutant: twitchArchiveChapters returning the VOD info's chapters without
// the lookup's id (asking GetVodInfo for "").
func TestTwitchArchiveChaptersLooksUpTheVod(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		asked = append(asked, body)
		if strings.Contains(body, "FilterableVideoTower_Videos") {
			io.WriteString(rw, `{"data":{"user":{"videos":{"edges":[{"__typename":"VideoEdge","node":{"__typename":"Video","id":"777","publishedAt":"2026-10-01T12:00:03Z"}}]}}}}`)
			return
		}
		if !strings.Contains(body, `"videoID":"777"`) {
			io.WriteString(rw, `[{"data":{"video":null}},{"data":{}}]`)
			return
		}
		io.WriteString(rw, `[{"data":{"video":{"id":"777","title":"t","lengthSeconds":600,"owner":{"login":"s"}}}},{"data":{"video":{"moments":{"edges":[{"node":{"positionMilliseconds":0,"durationMilliseconds":600000,"description":"Music"}}]}}}}]`)
	}))
	t.Cleanup(srv.Close)
	old := constants.TwitchURLs.GQL
	constants.TwitchURLs.GQL = srv.URL
	t.Cleanup(func() { constants.TwitchURLs.GQL = old })

	w := &DownloadWorker{tw: twitch.NewService(nil, discardLogger{}), logger: discardLogger{}}
	got := w.twitchArchiveChapters(context.Background(), "s", "2026-10-01T12:00:00Z")
	want := []utils.Chapter{{StartMs: 0, EndMs: 600000, Title: "Music"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("twitchArchiveChapters = %+v, want %+v (requests %q)", got, want, asked)
	}
	if got := w.twitchArchiveChapters(context.Background(), "s", "2026-10-02T12:00:00Z"); got != nil {
		t.Errorf("a broadcast newer than every VOD = %+v, want none", got)
	}
}
