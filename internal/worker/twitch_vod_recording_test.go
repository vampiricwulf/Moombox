package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// recordingVodServer serves a Twitch VOD playlist that never carries ENDLIST
// until grow() is called, after which it lists two more segments and ENDLIST.
// served counts every segment request by name.
func recordingVodServer(t *testing.T) (srv *httptest.Server, grow func(), served func(string) int) {
	t.Helper()
	ts := oneSecondTS(t)
	var grown atomic.Bool
	var mu sync.Mutex
	counts := map[string]int{}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			var b strings.Builder
			b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n")
			n := 2
			if grown.Load() {
				n = 4
			}
			for i := range n {
				fmt.Fprintf(&b, "#EXTINF:1.000,\nseg%d.ts\n", i)
			}
			if grown.Load() {
				b.WriteString("#EXT-X-ENDLIST\n")
			}
			_, _ = w.Write([]byte(b.String()))
			return
		}
		mu.Lock()
		counts[strings.TrimPrefix(r.URL.Path, "/")]++
		mu.Unlock()
		w.Header().Set("Content-Type", "video/mp2t")
		_, _ = w.Write(ts)
	}))
	t.Cleanup(srv.Close)
	return srv, func() { grown.Store(true) }, func(name string) int {
		mu.Lock()
		defer mu.Unlock()
		return counts[name]
	}
}

// TestTwitchVodNoLongerRecordingFinalizes is G34/G58: a VOD whose playlist
// stops growing without ever gaining ENDLIST used to reload forever and hold
// its download slot, because a VOD's CheckStreamStatus was a flat "not
// ended". It now asks VideoMetadata (twitchVodEnded): a video that is no
// longer recording ends the download with what it has, the stale branch
// finalizes it, and — the job having started on a recording VOD — the fresh
// length and thumbnail are written to the row (refreshRecordedVodInfo).
//
// Mutants: twitchVodEnded answering "not ended" again (the run never ends
// and the backstop context fails it); dropping the refresh call (the row
// keeps no thumbnail and the archive gets none).
func TestTwitchVodNoLongerRecordingFinalizes(t *testing.T) {
	h := newEndVerdictHarness(t, "tw_v2000000001")
	srv, _, served := recordingVodServer(t)
	thumbURL := srv.URL + "/thumb-640x360.jpg"
	var asks atomic.Int32
	h.variant.URL = srv.URL + "/vod.m3u8"
	h.variant.VodRecording = true
	h.variant.VodInfoFn = func(context.Context) (*twitch.TwitchVodInfo, error) {
		asks.Add(1)
		return &twitch.TwitchVodInfo{IsRecording: false, Duration: 7200, ThumbnailURL: thumbURL}, nil
	}
	h.jobCtx.OutputDir = t.TempDir()
	h.jobCtx.Filename = "vod"

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := h.o.ExecuteTwitch(ctx, h.jobCtx, h.variant, true, nil); err != nil {
		t.Fatalf("ExecuteTwitch = %v, want the VOD finalized once VideoMetadata said it is no longer recording", err)
	}
	for _, seg := range []string{"seg0.ts", "seg1.ts"} {
		if n := served(seg); n != 1 {
			t.Errorf("%s served %d times, want once", seg, n)
		}
	}
	if n := asks.Load(); n < 2 {
		t.Errorf("VideoMetadata asked %d times, want the end check and the post-download refresh", n)
	}
	row, err := h.db.GetJob(h.job.ID)
	if err != nil || row == nil {
		t.Fatalf("GetJob: %v", err)
	}
	// length_seconds is not asserted on the row: the finalize overwrites it
	// with the muxed file's probed duration. The thumbnail is: the row
	// carries the re-read URL, and the finalize's fallback fetched it into
	// the archive.
	if row.ThumbnailURL != thumbURL {
		t.Errorf("thumbnail_url = %q, want the re-read %q", row.ThumbnailURL, thumbURL)
	}
	if row.ThumbnailFile == "" {
		t.Error("thumbnail_file is empty: the finalize never fetched the re-read thumbnail")
	}
	// The finalize asks for the thumbnail's original size first
	// (downloadTwitchThumbnail, G45); this server serves it, so the sized
	// URL is never needed.
	if n := served("thumb-0x0.jpg"); n != 1 {
		t.Errorf("re-read thumbnail's original size fetched %d times, want once", n)
	}
	if n := served("thumb-640x360.jpg"); n != 0 {
		t.Errorf("re-read thumbnail's sized URL fetched %d times, want none once the original size answered", n)
	}
	if row.Status != database.StatusFinished {
		t.Errorf("status = %s, want Finished", row.Status)
	}
}

// TestTwitchVodStillRecordingKeepsFollowing is the other half: while
// VideoMetadata still says recording, a stale window is no end — the
// download keeps reloading and takes the segments the growing VOD adds until
// ENDLIST. A VOD that was not recording when its job was set up gets no
// post-download re-read (VodRecording false), so VideoMetadata is asked once.
//
// Mutants: twitchVodEnded answering "ended" whatever IsRecording says (the
// download stops at the first two segments); refreshing without the
// VodRecording gate (a second ask).
func TestTwitchVodStillRecordingKeepsFollowing(t *testing.T) {
	h := newEndVerdictHarness(t, "tw_v2000000002")
	srv, grow, served := recordingVodServer(t)
	var asks atomic.Int32
	h.variant.URL = srv.URL + "/vod.m3u8"
	h.variant.VodInfoFn = func(context.Context) (*twitch.TwitchVodInfo, error) {
		if asks.Add(1) == 1 {
			// Still recording: the broadcast adds two more segments and
			// ends, as Twitch finalizes the VOD.
			grow()
			return &twitch.TwitchVodInfo{IsRecording: true}, nil
		}
		return nil, errors.New("asked again")
	}
	h.jobCtx.OutputDir = t.TempDir()
	h.jobCtx.Filename = "vod"

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := h.o.ExecuteTwitch(ctx, h.jobCtx, h.variant, true, nil); err != nil {
		t.Fatalf("ExecuteTwitch = %v", err)
	}
	for i := range 4 {
		seg := fmt.Sprintf("seg%d.ts", i)
		if n := served(seg); n != 1 {
			t.Errorf("%s served %d times, want once — a recording VOD must be followed to ENDLIST", seg, n)
		}
	}
	if n := asks.Load(); n != 1 {
		t.Errorf("VideoMetadata asked %d times, want 1 (the one stale-window end check)", n)
	}
}

// TestTwitchVodEndedAnswers pins twitchVodEnded's table: no VodInfoFn is no
// verdict ("not ended", the old answer), a failed read is no verdict, and
// only a read that says not recording is an end.
func TestTwitchVodEndedAnswers(t *testing.T) {
	readErr := errors.New("gql down")
	cases := []struct {
		name    string
		fn      func(context.Context) (*twitch.TwitchVodInfo, error)
		ended   bool
		wantErr error
	}{
		{"unwired", nil, false, nil},
		{"recording", func(context.Context) (*twitch.TwitchVodInfo, error) {
			return &twitch.TwitchVodInfo{IsRecording: true}, nil
		}, false, nil},
		{"not recording", func(context.Context) (*twitch.TwitchVodInfo, error) {
			return &twitch.TwitchVodInfo{}, nil
		}, true, nil},
		{"read failed", func(context.Context) (*twitch.TwitchVodInfo, error) { return nil, readErr }, false, readErr},
		{"nil info", func(context.Context) (*twitch.TwitchVodInfo, error) { return nil, nil }, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ended, err := twitchVodEnded(context.Background(), &TwitchVariantInfo{VodInfoFn: tc.fn})
			if ended != tc.ended || !errors.Is(err, tc.wantErr) {
				t.Errorf("twitchVodEnded = (%v, %v), want (%v, %v)", ended, err, tc.ended, tc.wantErr)
			}
		})
	}
}

// TestWireTwitchVodInfo pins the worker's wiring: a VOD result gets a
// VodInfoFn keyed by the job's own VOD ID (tw_v stripped) and carries the
// stream processor's recording flag; a live result is left alone.
func TestWireTwitchVodInfo(t *testing.T) {
	var gotID string
	get := func(_ context.Context, id string) (*twitch.TwitchVodInfo, error) {
		gotID = id
		return &twitch.TwitchVodInfo{}, nil
	}
	job := &database.Job{ID: "tw_v123456789", VideoID: "tw_v123456789"}

	v := &TwitchVariantInfo{}
	wireTwitchVodInfo(v, job, &StreamProcessResult{IsVod: true, TwitchVodRecording: true}, get)
	if v.VodInfoFn == nil || !v.VodRecording {
		t.Fatalf("VOD wiring = (fn %v, recording %v), want both set", v.VodInfoFn != nil, v.VodRecording)
	}
	if _, err := v.VodInfoFn(context.Background()); err != nil || gotID != "123456789" {
		t.Errorf("VodInfoFn asked for %q (err %v), want 123456789", gotID, err)
	}

	live := &TwitchVariantInfo{}
	wireTwitchVodInfo(live, job, &StreamProcessResult{IsVod: false, TwitchVodRecording: true}, get)
	if live.VodInfoFn != nil || live.VodRecording {
		t.Error("a live result must not get a VOD metadata re-read")
	}
}

// TestProcessTwitchVodRecordingAndUntitled drives processTwitchVod against a
// VideoMetadata answer for a broadcast still being recorded, whose streamer
// set no title: the row stores no thumbnail (Twitch serves only its
// processing placeholder), the result carries the recording flag the
// orchestrator's post-download re-read is gated on, and the title names the
// broadcast "Untitled Broadcast" (yt-dlp twitch.py 526) rather than ending on
// "Streamer — ".
//
// Mutant: leaving TwitchVodRecording unset in processTwitchVod's result.
func TestProcessTwitchVodRecordingAndUntitled(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/gql":
			b, _ := io.ReadAll(r.Body)
			if strings.Contains(string(b), "videoPlaybackAccessToken") {
				io.WriteString(rw, `{"data":{"videoPlaybackAccessToken":{"value":"v","signature":"s"}}}`)
				return
			}
			io.WriteString(rw, `{"data":{"video":{"id":"123","title":"","lengthSeconds":600,"broadcastType":"ARCHIVE",`+
				`"previewThumbnailURL":"https://vod-secure.twitch.tv/_404/404_processing_320x180.png",`+
				`"createdAt":"2026-10-01T00:00:00Z","owner":{"id":"1","login":"streamer","displayName":"Streamer"}}}}`)
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
	sp := NewStreamProcessor(nil, twitch.NewService(nil, &discardLogger{}), cfg, db, &discardLogger{})
	job := &database.Job{ID: "tw_v123", VideoID: "tw_v123", URL: "https://www.twitch.tv/videos/123",
		Platform: "twitch", Status: database.StatusUpcoming, ThumbnailURL: "https://example.test/stale.jpg"}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}

	result, err := sp.processTwitchVod(context.Background(), job)
	if err != nil || result == nil || !result.ShouldDownload {
		t.Fatalf("processTwitchVod = (%+v, %v), want a download", result, err)
	}
	if !result.TwitchVodRecording {
		t.Error("TwitchVodRecording = false for a recording ARCHIVE")
	}
	row, _ := db.GetJob(job.ID)
	if row.ThumbnailURL != "" {
		t.Errorf("thumbnail_url = %q, want none for a recording VOD's placeholder", row.ThumbnailURL)
	}
	if want := "Streamer — Untitled Broadcast"; row.Title != want {
		t.Errorf("title = %q, want %q", row.Title, want)
	}
}

// TestTwitchVodFailureWithMetadataWiredLandsInError is the W18 review's
// regression: twitchVodEnded was first wired as a VOD's whole
// CheckStreamStatus, so every engine site that consults it — the playlist
// 404/410 branch, the fetch- and parse-failure escalations, the stuck-skip
// exit and the init-failure escalation — read a finished VOD's "no longer
// recording" as a confirmed end. A VOD whose playlist failed then muxed a
// truncated capture and was written Finished, and processJob deleted its
// staging with the resume sidecar, where TestTwitchVodDownloadFailureLandsInError
// (whose harness wires no VodInfoFn) pins Error with staging kept. The
// VideoMetadata answer now reaches only the stale-window site (CheckStaleEnd);
// with it wired, a failed VOD still lands in Error, whether the playlist
// is dead from the first fetch or starts failing mid-download.
//
// Mutant: ExecuteTwitch's VOD CheckStreamStatus returning twitchVodEnded
// again (both rows return nil, or a mux error after Muxing).
func TestTwitchVodFailureWithMetadataWiredLandsInError(t *testing.T) {
	ts := oneSecondTS(t)
	cases := []struct {
		name   string
		failAt int // segment requests served before the server answers 500 to everything; -1: playlist 404 at once
	}{
		{"playlist dead from the start", -1},
		{"5xx mid-download", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newEndVerdictHarness(t, "tw_v2000000009")
			statuses := h.watchStatuses()
			var segs atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.failAt < 0 {
					http.NotFound(w, r)
					return
				}
				if strings.HasSuffix(r.URL.Path, ".m3u8") {
					if int(segs.Load()) >= tc.failAt {
						http.Error(w, "cdn down", http.StatusInternalServerError)
						return
					}
					var b strings.Builder
					b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n")
					for i := range 6 {
						fmt.Fprintf(&b, "#EXTINF:1.000,\nseg%d.ts\n", i)
					}
					_, _ = w.Write([]byte(b.String()))
					return
				}
				if int(segs.Add(1)) > tc.failAt {
					http.Error(w, "cdn down", http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "video/mp2t")
				_, _ = w.Write(ts)
			}))
			t.Cleanup(srv.Close)
			h.variant.URL = srv.URL + "/vod.m3u8"
			h.variant.VodInfoFn = func(context.Context) (*twitch.TwitchVodInfo, error) {
				return &twitch.TwitchVodInfo{IsRecording: false}, nil
			}
			h.jobCtx.OutputDir = t.TempDir()
			h.jobCtx.Filename = "vod"

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			err := h.o.ExecuteTwitch(ctx, h.jobCtx, h.variant, true, nil)
			if err == nil {
				t.Fatal("ExecuteTwitch = nil for a VOD whose download failed — its truncated capture was finalized")
			}
			if seq := statuses(); sawMuxing(seq) {
				t.Errorf("the failed VOD was advertised Muxing (sequence %v), err %v", seq, err)
			}
			if row, _ := h.db.GetJob(h.job.ID); row != nil && row.Status == database.StatusFinished {
				t.Error("the failed VOD was written Finished")
			}
		})
	}
}
