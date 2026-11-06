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
	"testing/synctest"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestLiveTwitchChaptersVodFirstObservedFallback is owner decision D23
// follow-up 1: a live capture that began 90 s into the broadcast takes its
// archive VOD's chapters, shifted into its own timeline; with no VOD list, the
// categories it observed, each from the moment it saw it, the first one from
// the start; with no broadcast start, the observed list alone, from the
// capture's start.
//
// Mutants: the observed list used even when the VOD has chapters; the VOD
// list not shifted by the capture's offset; the first observation kept at its
// own time (an untitled sliver in front of it).
func TestLiveTwitchChaptersVodFirstObservedFallback(t *testing.T) {
	streamStart := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	rec := streamStart.Add(90 * time.Second).UnixMilli()
	tl := twitchTimeline{
		RecordingStartMs: rec,
		Categories: []twitchCategoryMark{
			{AtMs: rec + 4000, Title: "Just Chatting"},
			{AtMs: streamStart.Add(10 * time.Minute).UnixMilli(), Title: "Elden Ring"},
		},
	}
	vod := []utils.Chapter{
		{StartMs: 0, EndMs: 300_000, Title: "VOD: Chatting"},
		{StartMs: 300_000, EndMs: 900_000, Title: "VOD: Game"},
	}
	got := liveTwitchChapters(streamStart, tl, vod)
	want := []utils.Chapter{
		{StartMs: 0, EndMs: 210_000, Title: "VOD: Chatting"},
		{StartMs: 210_000, EndMs: 810_000, Title: "VOD: Game"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("with a VOD list = %+v, want %+v", got, want)
	}

	got = liveTwitchChapters(streamStart, tl, nil)
	want = []utils.Chapter{
		{StartMs: 0, Title: "Just Chatting"},
		{StartMs: 510_000, Title: "Elden Ring"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("observed fallback = %+v, want %+v", got, want)
	}

	got = liveTwitchChapters(time.Time{}, tl, vod)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("without a broadcast start = %+v, want the observed list %+v", got, want)
	}
}

// TestTwitchTimelineNotes: a category is recorded only when it changes, an
// empty one never, and the recording start is overwritten by each new file's
// first segment.
//
// Mutants: the repeat check removed (the same category twice); the empty
// check removed; the recording start written once only.
func TestTwitchTimelineNotes(t *testing.T) {
	dir := t.TempDir()
	at := time.Unix(1000, 0)
	noteTwitchCategory(dir, "A", at)
	noteTwitchCategory(dir, "A", at.Add(time.Minute))
	noteTwitchCategory(dir, "", at.Add(2*time.Minute))
	noteTwitchCategory(dir, "B", at.Add(3*time.Minute))
	noteTwitchRecordingStart(dir, at)
	noteTwitchRecordingStart(dir, at.Add(time.Hour))
	tl := readTwitchTimeline(dir)
	wantCats := []twitchCategoryMark{{AtMs: at.UnixMilli(), Title: "A"}, {AtMs: at.Add(3 * time.Minute).UnixMilli(), Title: "B"}}
	if !reflect.DeepEqual(tl.Categories, wantCats) {
		t.Errorf("categories = %+v, want %+v", tl.Categories, wantCats)
	}
	if tl.RecordingStartMs != at.Add(time.Hour).UnixMilli() {
		t.Errorf("recording start = %d, want the latest file's", tl.RecordingStartMs)
	}
}

// TestPollTwitchCategoryNotesEachMinute runs the category poll in a bubble:
// one read per twitchCategoryPollInterval, a change recorded at the minute it
// was read, a failed read skipped, and the poll gone when its context ends.
//
// Mutants: the poll reading once and stopping; a failed read ending the poll.
func TestPollTwitchCategoryNotesEachMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		start := time.Now()
		answers := []struct {
			cat string
			err error
		}{{"A", nil}, {"", context.DeadlineExceeded}, {"B", nil}}
		var mu sync.Mutex
		reads := 0
		read := func(context.Context) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			a := answers[min(reads, len(answers)-1)]
			reads++
			return a.cat, a.err
		}
		o := &DownloadOrchestrator{logger: discardLogger{}}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			o.pollTwitchCategory(ctx, dir, read)
		}()
		time.Sleep(3*twitchCategoryPollInterval + time.Second)
		synctest.Wait()
		cancel()
		<-done
		tl := readTwitchTimeline(dir)
		want := []twitchCategoryMark{
			{AtMs: start.Add(twitchCategoryPollInterval).UnixMilli(), Title: "A"},
			{AtMs: start.Add(3 * twitchCategoryPollInterval).UnixMilli(), Title: "B"},
		}
		if !reflect.DeepEqual(tl.Categories, want) {
			t.Errorf("categories = %+v, want %+v", tl.Categories, want)
		}
		if reads != 3 {
			t.Errorf("reads = %d, want 3", reads)
		}
	})
}

// TestStageTwitchChapters: a VOD stages its own chapters; a live capture
// stages its archive VOD's, looked up at finalize, in its own timeline.
//
// Mutant: the live arm not asking ArchiveChaptersFn (the observed list is
// staged instead).
func TestStageTwitchChapters(t *testing.T) {
	o := &DownloadOrchestrator{logger: discardLogger{}}
	vodDir := t.TempDir()
	vodList := []utils.Chapter{{StartMs: 0, EndMs: 5000, Title: "Game"}}
	o.stageTwitchChapters(context.Background(), twitchTestJobCtx(vodDir, true, ""), &TwitchVariantInfo{VodChapters: vodList}, true)
	if got, _ := utils.ReadStagedChapters(vodDir); !reflect.DeepEqual(got, vodList) {
		t.Errorf("VOD staged %+v, want %+v", got, vodList)
	}

	liveDir := t.TempDir()
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	noteTwitchRecordingStart(liveDir, start.Add(time.Minute))
	noteTwitchCategory(liveDir, "Observed", start.Add(time.Minute))
	asked := false
	variant := &TwitchVariantInfo{ArchiveChaptersFn: func(context.Context) []utils.Chapter {
		asked = true
		return []utils.Chapter{{StartMs: 0, EndMs: 180_000, Title: "From the VOD"}}
	}}
	o.stageTwitchChapters(context.Background(), twitchTestJobCtx(liveDir, false, start.Format(time.RFC3339)), variant, false)
	want := []utils.Chapter{{StartMs: 0, EndMs: 120_000, Title: "From the VOD"}}
	if got, _ := utils.ReadStagedChapters(liveDir); !asked || !reflect.DeepEqual(got, want) {
		t.Errorf("live staged %+v (VOD asked: %v), want %+v", got, asked, want)
	}
}

// TestYouTubeChaptersSeamIsTheService pins the production value of the
// YouTube chapter seam.
func TestYouTubeChaptersSeamIsTheService(t *testing.T) {
	if reflect.ValueOf(youtubeChapters).Pointer() != reflect.ValueOf(fetchYouTubeChapters).Pointer() {
		t.Fatal("youtubeChapters is not fetchYouTubeChapters")
	}
}

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

// twitchTestJobCtx is a JobContext for the chapter staging tests.
func twitchTestJobCtx(dir string, isVod bool, streamStart string) *JobContext {
	return &JobContext{StagingDir: dir, Job: &database.Job{
		ID: "tw_chapters", Platform: "twitch", IsVod: isVod, StreamStartTime: streamStart,
	}}
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

// TestTwitchChapterHooksAreWired pins, by source, the call sites a unit test
// cannot reach without a real live capture: ExecuteTwitch notes the starting
// category, runs the category poll, records the root part's start and stages
// the chapters before the single-part finalize; processJob hands the VOD's
// chapters, the archive lookup and the category read to the variant.
//
// Mutant: any one of those lines removed.
func TestTwitchChapterHooksAreWired(t *testing.T) {
	for file, needles := range map[string][]string{
		"orchestrator_twitch.go": {
			"noteTwitchCategory(jobCtx.StagingDir, jobCtx.Job.TwitchCategory, time.Now())",
			"go o.pollTwitchCategory(pollCtx, jobCtx.StagingDir, variant.CategoryFn)",
			"noteTwitchRecordingStart(stagingDir, pdt)",
			"o.stageTwitchChapters(muxCtx, jobCtx, variant, isVod)",
		},
		"worker.go": {
			"variant.VodChapters = result.TwitchVodChapters",
			"variant.CategoryFn = func(innerCtx context.Context) (string, error) {",
			"return w.twitchArchiveChapters(innerCtx, login, startedAt)",
		},
	} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range needles {
			if !strings.Contains(string(src), n) {
				t.Errorf("%s no longer carries %q", file, n)
			}
		}
	}
}

// TestCopyAssetsCopiesTheChatRawSidecar: a capture's chat.raw.jsonl (owner
// decision D16) goes beside the archive with its chat, as
// <name>.chat.raw.jsonl.
//
// Mutant: the raw copy removed.
func TestCopyAssetsCopiesTheChatRawSidecar(t *testing.T) {
	staging, outDir := t.TempDir(), t.TempDir()
	for name, body := range map[string]string{"chat.json": `{"messages":[]}`, "chat.raw.jsonl": "{\"a\":1}\n"} {
		if err := os.WriteFile(filepath.Join(staging, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	o := &DownloadOrchestrator{logger: discardLogger{}}
	jobCtx := &JobContext{Job: &database.Job{ID: "yt_raw"}, StagingDir: staging}
	o.copyAssets(context.Background(), jobCtx, outDir, "A Job", "A Job", map[string]any{}, false)
	got, err := os.ReadFile(filepath.Join(outDir, "A Job.chat.raw.jsonl"))
	if err != nil || string(got) != "{\"a\":1}\n" {
		t.Errorf("raw sidecar beside the archive: %q, %v", got, err)
	}
}
