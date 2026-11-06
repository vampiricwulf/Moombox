package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// drmVideoMPD is potTestMPD with its video AdaptationSet DRM-protected and
// its audio in the clear: the DASH-manifest path finds no video it can record.
var drmVideoMPD = strings.Replace(potTestMPD, `<AdaptationSet mimeType="video/mp4">`,
	`<AdaptationSet mimeType="video/mp4"><ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc"/>`, 1)

// drmAudioMPD is potTestMPD the other way round: its audio AdaptationSet
// DRM-protected and its video in the clear.
var drmAudioMPD = strings.Replace(potTestMPD, `<AdaptationSet mimeType="audio/mp4">`,
	`<AdaptationSet mimeType="audio/mp4"><ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc"/>`, 1)

// TestDashManifestAllDRMFallsToHls pins owner decision FE-1: a DASH-manifest
// setup whose every Representation of a kind it needs is DRM-protected (here
// the whole video set) records the extraction's HLS manifest instead, as
// yt-dlp drops the has_drm formats and takes what remains; with no HLS
// manifest, or an HLS playlist refused as protected too, the job's error
// leads with "DRM-protected; Moombox cannot record it". An HLS fallback that
// fails for any other reason says so without that claim, and a manifest in
// the clear never asks HLS. A protected audio set beside clear video falls
// to HLS as a protected video set does, and an audio_only job beside a
// protected video set needs no video: it records the clear DASH audio and
// never asks HLS.
//
// Mutants (each run): DownloadDash not reporting the lost kind as
// ErrDRMProtected (every DRM row ends on "could not find suitable video
// stream", HLS never asked); drmHlsFallback dropped from setupOrFallback (the
// HLS row fails, HLS never asked); the no-HLS arm returning the bare error
// (its message is missing); every HLS failure labelled DRM (the
// transient-HLS row claims DRM); the audio check disabled (the protected-
// audio row ends on "could not find suitable audio stream", HLS never
// asked); the video check without its videoItag != -1 guard (the audio_only
// row falls to HLS).
func TestDashManifestAllDRMFallsToHls(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mpd        string
		hlsURL     string
		hlsErr     error
		audioOnly  bool
		wantHls    bool
		wantDash   bool
		wantDRMMsg bool
	}{
		{name: "protected video, HLS listed: HLS records", mpd: drmVideoMPD, hlsURL: "https://m.example/x.m3u8", wantHls: true},
		{name: "protected video, no HLS: cannot record", mpd: drmVideoMPD, wantDRMMsg: true},
		{name: "protected video, HLS protected too: cannot record", mpd: drmVideoMPD, hlsURL: "https://m.example/x.m3u8",
			hlsErr: fmt.Errorf("%w: the HLS playlist is DRM-protected", engine.ErrDRMProtected), wantHls: true, wantDRMMsg: true},
		{name: "protected video, HLS fails otherwise", mpd: drmVideoMPD, hlsURL: "https://m.example/x.m3u8",
			hlsErr: errors.New("fetch HLS manifest: connection reset"), wantHls: true},
		{name: "in the clear: DASH records", mpd: potTestMPD, hlsURL: "https://m.example/x.m3u8", wantDash: true},
		{name: "protected audio, HLS listed: HLS records", mpd: drmAudioMPD, hlsURL: "https://m.example/x.m3u8", wantHls: true},
		{name: "audio_only beside protected video: DASH audio records", mpd: drmVideoMPD, hlsURL: "https://m.example/x.m3u8",
			audioOnly: true, wantDash: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.mpd))
			}))
			t.Cleanup(srv.Close)
			hls := &fakeStrategy{kind: "hls", answers: []error{tc.hlsErr}}
			swapHls(t, hls)
			job, logs := vodPotJob(t)
			if tc.audioOnly {
				job.Job.QualityPreference = "audio_only"
			}
			info := &youtube.VideoInfo{
				StreamStatus:       youtube.StreamLive,
				DashManifestURL:    srv.URL + "/api/manifest/dash/id/abc",
				DashManifestSource: "tv_public",
				HlsManifestURL:     tc.hlsURL,
				HlsManifestSource:  "tv_public",
			}
			res, err := setupWithFallback(context.Background(), job, DashStrategy, false, info, &StrategyDeps{})
			if ran := len(hls.seen) == 1; ran != tc.wantHls {
				t.Fatalf("HLS ran %d times, want %v: %v", len(hls.seen), tc.wantHls, err)
			}
			if got := errors.Is(err, errDRMUnrecordable); got != tc.wantDRMMsg {
				t.Errorf("err = %v; DRM-unrecordable %v, want %v", err, got, tc.wantDRMMsg)
			}
			if tc.wantDRMMsg && !strings.HasPrefix(err.Error(), "DRM-protected; Moombox cannot record it") {
				t.Errorf("err = %q, want it to lead with the DRM message", err)
			}
			switch {
			case tc.wantDash && tc.audioOnly:
				if err != nil || res == nil || res.IsHls || res.AudioDownloader == nil || res.VideoDownloader != nil {
					t.Errorf("setup = %+v, %v; want the DASH audio alone", res, err)
				}
			case tc.wantDash:
				if err != nil || res == nil || res.IsHls || res.VideoDownloader == nil {
					t.Errorf("setup = %+v, %v; want the DASH capture", res, err)
				}
			case tc.wantHls && tc.hlsErr == nil:
				if err != nil || res == nil || !res.IsHls {
					t.Errorf("setup = %+v, %v; want the HLS fallback's result", res, err)
				}
				if logLine(logs, "[DRM] every DASH Representation of a kind the job needs is DRM-protected") == nil {
					t.Errorf("no fallback line: %v", logs.lines())
				}
			default:
				if err == nil || !errors.Is(err, engine.ErrDRMProtected) {
					t.Errorf("err = %v, want the DASH setup's DRM refusal in it", err)
				}
			}
		})
	}
}

// TestLiveLoopStopsOnADRMRefusal: a live capture whose downloader the engine
// stopped on an encrypted playlist (engine.ErrDRMProtected — the HLS the DRM
// fallback fell to) ends on errDRMUnrecordable at once, as a staging write
// failure does, instead of being read as a possible stream end, re-verified
// and finished.
//
// Mutant: the ErrDRMProtected arm removed from runLiveStreamDownload (the
// loop re-verifies the stream, runs the downloaders again, and returns
// without the error).
func TestLiveLoopStopsOnADRMRefusal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w, db := testWorkerSetup(t)
		pin := 299
		job := &database.Job{ID: "drm1", VideoID: "drm1", URL: "u", Platform: "youtube", Status: database.StatusDownloading,
			SelectedVideoItag: &pin} // a pin: no quality monitor
		if _, err := db.AddJob(job); err != nil {
			t.Fatal(err)
		}
		o := w.orchestrator
		o.SetFfmpegPath(filepath.Join(t.TempDir(), "no-ffmpeg"))
		root := t.TempDir()
		jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, YT: youtube.NewService(nil, &discardLogger{}),
			StagingDir: root, OutputDir: t.TempDir(), Filename: "drm1", Logger: &captureLogger{},
			Interruption: &interruptionSignal{}, Run: &runRecord{}}
		result := &DownloadResult{HasVideo: true, IsHls: true, VideoPath: filepath.Join(root, "video.ts"),
			VideoDownloader: engine.NewSegmentDownloader(engine.DownloaderOptions{OutputFile: filepath.Join(root, "video.ts"), IsHls: true, BaseURL: "https://m.example/v.m3u8"})}
		live := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, HlsManifestURL: "https://m.example/x.m3u8"}

		var mu sync.Mutex
		runs := 0
		origInfo, origRun := liveVideoInfo, runLiveDownloaders
		t.Cleanup(func() { liveVideoInfo, runLiveDownloaders = origInfo, origRun })
		liveVideoInfo = func(*youtube.Service, context.Context, string) (*youtube.VideoInfo, error) {
			return &youtube.VideoInfo{StreamStatus: youtube.StreamPostLive}, nil
		}
		runLiveDownloaders = func(*DownloadOrchestrator, context.Context, *DownloadResult) error {
			mu.Lock()
			defer mu.Unlock()
			runs++
			if runs == 1 {
				return fmt.Errorf("%w: the HLS playlist is DRM-protected (#EXT-X-KEY METHOD=SAMPLE-AES)", engine.ErrDRMProtected)
			}
			return nil
		}

		tracker := NewProgressTracker(db, job.ID, discardLogger{}, 0)
		var lastSeg atomicTimeValue
		_, _, err := o.runLiveStreamDownload(context.Background(), jobCtx, jobCtx, 0, false, live, result, tracker,
			func() bool { return false }, &lastSeg)
		tracker.Close()
		time.Sleep(2 * activityUpdateInterval)
		if !errors.Is(err, errDRMUnrecordable) || !errors.Is(err, engine.ErrDRMProtected) {
			t.Errorf("runLiveStreamDownload = %v, want the DRM-unrecordable error", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if runs != 1 {
			t.Errorf("downloaders ran %d times, want once — a DRM refusal is not a stream end to verify", runs)
		}
	})
}

// TestDrmUnrecordable: the lead goes on once, and only on a DRM refusal.
//
// Mutant: the already-wrapped test dropped (the message doubles).
func TestDrmUnrecordable(t *testing.T) {
	refusal := fmt.Errorf("parse: %w", engine.ErrDRMProtected)
	once := drmUnrecordable(refusal)
	if !errors.Is(once, errDRMUnrecordable) || strings.Count(drmUnrecordable(once).Error(), "Moombox cannot record it") != 1 {
		t.Errorf("drmUnrecordable(drmUnrecordable(x)) = %q, want the lead once", drmUnrecordable(once))
	}
	other := errors.New("dial tcp: i/o timeout")
	if got := drmUnrecordable(other); got != other {
		t.Errorf("drmUnrecordable(other) = %v, want it unchanged", got)
	}
	if drmUnrecordable(nil) != nil {
		t.Error("drmUnrecordable(nil) != nil")
	}
}

// encryptedHlsStrategy is an HLS setup whose playlist the engine will refuse
// as encrypted: a real downloader on a media playlist under #EXT-X-KEY.
type encryptedHlsStrategy struct {
	playlistURL string
	seen        int
}

func (s *encryptedHlsStrategy) Kind() string { return "hls" }

func (s *encryptedHlsStrategy) Download(_ context.Context, job *JobContext, _ *youtube.VideoInfo, _ *StrategyDeps) (*DownloadResult, error) {
	s.seen++
	out := filepath.Join(job.StagingDir, "video.ts")
	return &DownloadResult{HasVideo: true, IsHls: true, VideoPath: out,
		VideoDownloader: engine.NewSegmentDownloader(engine.DownloaderOptions{BaseURL: s.playlistURL, OutputFile: out, IsHls: true})}, nil
}

// TestVodDRMEverywhereFailsWithTheMessage runs the whole VOD setup and
// download: the DASH manifest is DRM-protected (the DashStrategy setup's
// ErrDRMProtected), the job falls to the HLS manifest, and the engine refuses
// that playlist as encrypted before writing anything — so the job's error
// leads with "DRM-protected; Moombox cannot record it".
//
// Mutant: the VOD branch's drmUnrecordable dropped (the error is the
// engine's alone).
func TestVodDRMEverywhereFailsWithTheMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n" +
			"#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"skd://key\",KEYFORMAT=\"com.apple.streamingkeydelivery\"\n" +
			"#EXTINF:2.000,\nseg0.ts\n#EXT-X-ENDLIST\n"))
	}))
	t.Cleanup(srv.Close)
	dash := &fakeStrategy{kind: DashStrategy.Kind(), answers: []error{fmt.Errorf("DASH: %w: every video Representation in the manifest carries ContentProtection", engine.ErrDRMProtected)}}
	hls := &encryptedHlsStrategy{playlistURL: srv.URL + "/v.m3u8"}
	origDash, origHls := DashStrategy, HlsStrategy
	DashStrategy, HlsStrategy = dash, hls
	t.Cleanup(func() { DashStrategy, HlsStrategy = origDash, origHls })

	w, db := testWorkerSetup(t)
	job := &database.Job{ID: "yt_drmvod", VideoID: "drmvod", URL: "https://www.youtube.com/watch?v=drmvod", Platform: "youtube", Status: database.StatusDownloading, IsVod: true}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, StagingDir: filepath.Join(t.TempDir(), "staging"), Logger: &captureLogger{}}
	info := &youtube.VideoInfo{StreamStatus: youtube.StreamVOD, DashManifestURL: "https://m.example/dash.mpd", HlsManifestURL: "https://m.example/x.m3u8"}
	// No deadline: the engine refuses the encrypted playlist on its first
	// fetch, so nothing here waits on a clock.
	err := w.orchestrator.ExecuteWithChat(context.Background(), jobCtx, info, true, nil)
	if len(dash.seen) != 1 || hls.seen != 1 {
		t.Fatalf("DASH ran %d, HLS %d times; want once each", len(dash.seen), hls.seen)
	}
	if !errors.Is(err, errDRMUnrecordable) || !errors.Is(err, engine.ErrDRMProtected) {
		t.Errorf("ExecuteWithChat = %v, want the DRM-unrecordable error over the engine's refusal", err)
	}
}

// TestDrmHlsFallbackAnswersOnlyADashSetup: the HLS fallback answers a DRM
// refusal from the DASH-manifest setup alone. One from any other setup — the
// HLS setup itself, the manifest-free path — is not handed HLS again (the
// playlist that was just refused, set up a second time as its own fallback);
// it ends on errDRMUnrecordable in setupOrFallback. Nor is a DASH failure
// that is not DRM, or one whose extraction lists no HLS manifest.
//
// Mutant: the Kind clause dropped from drmHlsFallback (the HLS and
// manifest-free rows are handed HLS).
func TestDrmHlsFallbackAnswersOnlyADashSetup(t *testing.T) {
	drm := fmt.Errorf("setup: %w", engine.ErrDRMProtected)
	withHls := &youtube.VideoInfo{HlsManifestURL: "https://m.example/x.m3u8"}
	for _, tc := range []struct {
		name     string
		strategy DownloadStrategy
		info     *youtube.VideoInfo
		err      error
		want     bool
	}{
		{"DASH manifest, DRM, HLS listed", DashStrategy, withHls, drm, true},
		{"HLS setup, DRM", HlsStrategy, withHls, drm, false},
		{"manifest-free, DRM", ManifestlessDashStrategy, withHls, drm, false},
		{"DASH manifest, not DRM", DashStrategy, withHls, errors.New("could not find suitable video stream"), false},
		{"DASH manifest, DRM, no HLS", DashStrategy, &youtube.VideoInfo{}, drm, false},
		{"DASH manifest, DRM, no extraction", DashStrategy, nil, drm, false},
	} {
		if got := drmHlsFallback(tc.strategy, tc.info, tc.err) != nil; got != tc.want {
			t.Errorf("%s: fallback %v, want %v", tc.name, got, tc.want)
		}
	}
}
