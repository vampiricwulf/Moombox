package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// demuxedTestMaster is the shape of the visionos master a live-edge YouTube
// capture records (measured 2026-11-27 on three 24/7 streams): every variant
// is video-only and names AUDIO group 233 or 234, each an AAC rendition
// with a URI of its own. Relative URIs, so the test server serves them.
const demuxedTestMaster = "#EXTM3U\n" +
	`#EXT-X-MEDIA:URI="a/itag/233/index.m3u8",TYPE=AUDIO,GROUP-ID="233",NAME="Default",DEFAULT=YES,AUTOSELECT=YES` + "\n" +
	`#EXT-X-MEDIA:URI="a/itag/234/index.m3u8",TYPE=AUDIO,GROUP-ID="234",NAME="Default",DEFAULT=YES,AUTOSELECT=YES` + "\n" +
	`#EXT-X-STREAM-INF:BANDWIDTH=290000,CODECS="avc1.42C00B,mp4a.40.5",RESOLUTION=256x144,FRAME-RATE=15,AUDIO="233"` + "\n" +
	"v/itag/269/index.m3u8\n" +
	`#EXT-X-STREAM-INF:BANDWIDTH=400000,CODECS="avc1.4D4015,mp4a.40.5",RESOLUTION=426x240,FRAME-RATE=30,AUDIO="233"` + "\n" +
	"v/itag/229/index.m3u8\n" +
	`#EXT-X-STREAM-INF:BANDWIDTH=450000,CODECS="avc1.4D4015,mp4a.40.2",RESOLUTION=426x240,FRAME-RATE=30,AUDIO="234"` + "\n" +
	"v/itag/229b/index.m3u8\n" +
	`#EXT-X-STREAM-INF:BANDWIDTH=4500000,CODECS="avc1.640028,mp4a.40.2",RESOLUTION=1920x1080,FRAME-RATE=30,AUDIO="234"` + "\n" +
	"v/itag/270/index.m3u8\n"

// hlsAudioServer serves a master at /master/index.m3u8 and, for every media
// playlist under it, a two-segment ENDLIST playlist whose segments' bodies
// name the playlist they came from, so a staged file says which playlist it
// recorded.
type hlsAudioServer struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string
}

func newHlsAudioServer(t *testing.T, master string) *hlsAudioServer {
	t.Helper()
	s := &hlsAudioServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.paths = append(s.paths, r.URL.Path)
		s.mu.Unlock()
		p := r.URL.Path
		if i := strings.Index(p, "/pot/"); i >= 0 {
			p = p[:i]
		}
		switch {
		case strings.HasPrefix(p, "/master/") && strings.HasSuffix(p, "/seg"):
			_, _ = w.Write([]byte("[" + strings.TrimSuffix(strings.TrimPrefix(p, "/master/"), "/seg") + "]"))
		case p == "/master/index.m3u8":
			_, _ = w.Write([]byte(master))
		case strings.HasPrefix(p, "/master/"):
			dir := strings.TrimSuffix(p, "index.m3u8")
			_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:5\n#EXT-X-MEDIA-SEQUENCE:40\n#EXTINF:5.0,\n" +
				dir + "sq/40/seg\n#EXTINF:5.0,\n" + dir + "sq/41/seg\n#EXT-X-ENDLIST\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func hlsAudioInfo(srv *hlsAudioServer, source string) *youtube.VideoInfo {
	return &youtube.VideoInfo{
		StreamStatus:      youtube.StreamLive,
		HlsManifestURL:    srv.URL + "/master/index.m3u8",
		HlsManifestSource: source,
	}
}

// runHlsResult runs result's downloaders to the end of their ENDLIST
// playlists.
func runHlsResult(t *testing.T, res *DownloadResult) {
	t.Helper()
	for _, d := range []*engine.SegmentDownloader{res.VideoDownloader, res.AudioDownloader} {
		if d == nil {
			continue
		}
		if err := d.Start(context.Background()); err != nil {
			t.Fatalf("downloader: %v", err)
		}
	}
}

func parseTestMaster(t *testing.T, m string) *engine.HlsParseResult {
	t.Helper()
	p := engine.ParseHls(m, "https://e/x/m.m3u8")
	if p == nil || !p.IsMaster {
		t.Fatalf("not a master: %q", m)
	}
	return p
}

func readStaged(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// TestDownloadHlsRecordsTheAudioRenditionOfADemuxedMaster: on a demuxed
// master the picked video-only variant is recorded into video.ts and its
// AUDIO group's rendition into audio.ts, by a second downloader, so the
// finalize's two-input mux joins them. Before, the variant was recorded
// alone and the archive had no sound (the 2026-11-27 measurement).
//
// Mutants: no second downloader (HasAudio false, no audio.ts); the rendition
// taken from another group than the variant's (audio.ts holds 233's
// segments); the log line not saying where the audio is.
func TestDownloadHlsRecordsTheAudioRenditionOfADemuxedMaster(t *testing.T) {
	fastEngine(t)
	srv := newHlsAudioServer(t, demuxedTestMaster)
	job, logs := vodPotJob(t)
	job.Config.MaxVideoResolution = 1080
	res, err := DownloadHls(context.Background(), job, hlsAudioInfo(srv, "visionos"), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("DownloadHls: %v", err)
	}
	if !res.IsHls || !res.HasVideo || !res.HasAudio || res.AudioDownloader == nil || !res.AdvertisedAudio {
		t.Fatalf("result = %+v, want an HLS pair with audio advertised", res)
	}
	if filepath.Base(res.VideoPath) != "video.ts" || filepath.Base(res.AudioPath) != "audio.ts" {
		t.Errorf("paths = %s, %s; want video.ts and audio.ts", res.VideoPath, res.AudioPath)
	}
	if res.AudioItag != 234 {
		t.Errorf("AudioItag = %d, want 234 (the 1080p variant's group)", res.AudioItag)
	}
	runHlsResult(t, res)
	if got := readStaged(t, res.VideoPath); got != "[v/itag/270/sq/40][v/itag/270/sq/41]" {
		t.Errorf("video.ts = %q", got)
	}
	if got := readStaged(t, res.AudioPath); got != "[a/itag/234/sq/40][a/itag/234/sq/41]" {
		t.Errorf("audio.ts = %q", got)
	}
	lines := potLines(logs, "selected HLS variant")
	if len(lines) != 1 || lines[0]["audio"] != "separate rendition (group 234)" {
		t.Errorf("selected HLS variant lines = %v, want audio=separate rendition (group 234)", lines)
	}
}

// TestDownloadHlsMuxedMasterRecordsOneStream: a master whose variants carry
// their audio (no AUDIO group: the old itags 91-96, every Twitch master) is
// recorded as it always was, video.ts alone. AdvertisedAudio follows the
// variant's CODECS.
func TestDownloadHlsMuxedMasterRecordsOneStream(t *testing.T) {
	for _, tc := range []struct {
		name, master string
		advertised   bool
		audio        string
	}{
		{"no codecs", potTestMaster, false, "none advertised"},
		{"muxed codecs", "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=2500000,RESOLUTION=1280x720,CODECS=\"avc1.4d401f,mp4a.40.2\"\nhttps://example.com/itag/95/index.m3u8\n", true, "in the variant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newManifestServer(t, tc.master)
			job, logs := vodPotJob(t)
			info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, HlsManifestURL: srv.URL + "/m/index.m3u8", HlsManifestSource: "visionos"}
			res, err := DownloadHls(context.Background(), job, info, nil, nil, nil, nil)
			if err != nil {
				t.Fatalf("DownloadHls: %v", err)
			}
			if res.HasAudio || res.AudioDownloader != nil || res.AudioPath != "" || res.AudioItag != 0 {
				t.Errorf("result = %+v, want video.ts alone", res)
			}
			if res.AdvertisedAudio != tc.advertised {
				t.Errorf("AdvertisedAudio = %v, want %v", res.AdvertisedAudio, tc.advertised)
			}
			if lines := potLines(logs, "selected HLS variant"); len(lines) != 1 || lines[0]["audio"] != tc.audio {
				t.Errorf("selected HLS variant lines = %v, want audio=%s", lines, tc.audio)
			}
		})
	}
}

// TestDownloadHlsAudioOnlyTakesTheRenditionAlone: audio_only on a demuxed
// master records the audio rendition alone — yt-dlp's best audio there, the
// group of the better video (234) — where it used to pick the lowest
// bandwidth variant, whose segments hold no audio.
//
// Mutant: the audio_only branch removed (a video variant is recorded).
func TestDownloadHlsAudioOnlyTakesTheRenditionAlone(t *testing.T) {
	fastEngine(t)
	srv := newHlsAudioServer(t, demuxedTestMaster)
	job, _ := vodPotJob(t)
	job.Job.QualityPreference = "audio_only"
	res, err := DownloadHls(context.Background(), job, hlsAudioInfo(srv, "visionos"), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("DownloadHls: %v", err)
	}
	if res.HasVideo || res.VideoDownloader != nil || res.VideoPath != "" || !res.HasAudio || !res.IsHls {
		t.Fatalf("result = %+v, want the audio rendition alone", res)
	}
	runHlsResult(t, res)
	if got := readStaged(t, res.AudioPath); got != "[a/itag/234/sq/40][a/itag/234/sq/41]" {
		t.Errorf("audio.ts = %q, want group 234's segments", got)
	}
}

// TestDownloadHlsAudioRenditionCarriesTokenAndPosition: the rendition's
// downloader takes what the variant's does — the GVS token on its playlist
// path and as its PoToken, and the audio position a split or a refresh
// hands the setup (JobContext.AudioStartSeq), as the variant takes the
// video one.
//
// Mutants: the /pot/ suffix or PoToken left off the rendition; the audio
// downloader seeded from VideoStartSeq or not forced.
func TestDownloadHlsAudioRenditionCarriesTokenAndPosition(t *testing.T) {
	fakeVodMint(t, "tok123", nil)
	built := recordDownloaders(t)
	srv := newHlsAudioServer(t, demuxedTestMaster)
	job, _ := vodPotJob(t)
	job.VideoStartSeq, job.AudioStartSeq = 900, 899
	res, err := DownloadHls(context.Background(), job, hlsAudioInfo(srv, "web"), nil, nil, &bgutils.PotProvider{}, nil)
	if err != nil {
		t.Fatalf("DownloadHls: %v", err)
	}
	if res.AudioDownloader == nil || res.AudioDownloader.PoToken() != "tok123" {
		t.Fatalf("audio downloader token missing: %+v", res)
	}
	if len(*built) != 2 {
		t.Fatalf("built %d downloaders, want 2", len(*built))
	}
	v, a := (*built)[0], (*built)[1]
	if !strings.HasSuffix(a.BaseURL, "/a/itag/234/index.m3u8/pot/tok123") || !strings.HasSuffix(v.BaseURL, "/v/itag/270/index.m3u8/pot/tok123") {
		t.Errorf("playlist URLs = %s | %s, want each with /pot/tok123", v.BaseURL, a.BaseURL)
	}
	if v.StartSeq != 900 || !v.ForceStartSeq || a.StartSeq != 899 || !a.ForceStartSeq {
		t.Errorf("starts: video %d/%v audio %d/%v, want 900 and 899, forced", v.StartSeq, v.ForceStartSeq, a.StartSeq, a.ForceStartSeq)
	}
	if !strings.HasSuffix(a.OutputFile, "audio.ts") {
		t.Errorf("audio output = %s", a.OutputFile)
	}
}

// TestSelectHlsAudioRendition: within a group the DEFAULT rendition wins,
// an alternate (DEFAULT=NO, AUTOSELECT=NO) ranks below every other, and the
// original language above both, as yt-dlp's format sort ranks them.
func TestSelectHlsAudioRendition(t *testing.T) {
	master := "#EXTM3U\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="g",NAME="Described",DEFAULT=NO,LANGUAGE="en",URI="https://e/x/d.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="g",NAME="Other",LANGUAGE="fr",URI="https://e/x/o.m3u8"` + "\n" +
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="g",NAME="Main",DEFAULT=YES,LANGUAGE="fr",URI="https://e/x/m.m3u8"` + "\n" +
		`#EXT-X-STREAM-INF:BANDWIDTH=1,CODECS="avc1.4d401f,mp4a.40.2",AUDIO="g"` + "\n" + "https://e/x/v.m3u8\n"
	p := parseTestMaster(t, master)
	if got := selectHlsAudioRendition(p.AudioRenditions("g"), youtube.AudioLanguages{}); got == nil || got.Name != "Main" {
		t.Errorf("no language map: picked %+v, want Main", got)
	}
	if got := selectHlsAudioRendition(p.AudioRenditions("g"), youtube.AudioLanguages{Original: "en"}); got == nil || got.Name != "Described" {
		t.Errorf("original en: picked %+v, want the English one", got)
	}
}

// TestDownloadHlsResumesTheAudioRendition: a demuxed capture stopped
// mid-stream (a restart, a refresh) and set up again in the same part dir
// continues BOTH files from their resume sidecars — audio.ts as video.ts —
// rather than starting the rendition over or refusing the staged audio.
//
// Mutant: the rendition recorded under another name than the one the
// discovery and the sidecar know (audio.ts holds the second run only).
func TestDownloadHlsResumesTheAudioRendition(t *testing.T) {
	fastEngine(t)
	var mu sync.Mutex
	ended := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		mu.Lock()
		end := ended
		mu.Unlock()
		switch {
		case strings.HasSuffix(p, "/seg"):
			_, _ = w.Write([]byte("[" + strings.TrimSuffix(strings.TrimPrefix(p, "/master/"), "/seg") + "]"))
		case p == "/master/index.m3u8":
			_, _ = w.Write([]byte(demuxedTestMaster))
		default:
			dir := strings.TrimSuffix(p, "index.m3u8")
			body := "#EXTM3U\n#EXT-X-TARGETDURATION:5\n#EXT-X-MEDIA-SEQUENCE:40\n"
			n := 2
			if end {
				n = 4
			}
			for i := 0; i < n; i++ {
				body += "#EXTINF:5.0,\n" + dir + "sq/" + strconv.Itoa(40+i) + "/seg\n"
			}
			if end {
				body += "#EXT-X-ENDLIST\n"
			}
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(srv.Close)
	job, _ := vodPotJob(t)
	job.Config.MaxVideoResolution = 1080
	info := &youtube.VideoInfo{StreamStatus: youtube.StreamLive, HlsManifestURL: srv.URL + "/master/index.m3u8", HlsManifestSource: "visionos"}

	first, err := DownloadHls(context.Background(), job, info, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("first DownloadHls: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, d := range []*engine.SegmentDownloader{first.VideoDownloader, first.AudioDownloader} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = d.Start(ctx)
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		v, _ := os.ReadFile(first.VideoPath)
		a, _ := os.ReadFile(first.AudioPath)
		if strings.Count(string(v), "[") == 2 && strings.Count(string(a), "[") == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	wg.Wait()

	mu.Lock()
	ended = true
	mu.Unlock()
	second, err := DownloadHls(context.Background(), job, info, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("second DownloadHls: %v", err)
	}
	runHlsResult(t, second)
	if got := readStaged(t, second.AudioPath); got != "[a/itag/234/sq/40][a/itag/234/sq/41][a/itag/234/sq/42][a/itag/234/sq/43]" {
		t.Errorf("audio.ts = %q, want sq 40-43 once each", got)
	}
	if got := readStaged(t, second.VideoPath); got != "[v/itag/270/sq/40][v/itag/270/sq/41][v/itag/270/sq/42][v/itag/270/sq/43]" {
		t.Errorf("video.ts = %q, want sq 40-43 once each", got)
	}
}

// TestDownloadHlsPairsALoneStagedHalf: a demuxed capture's setup in a part
// dir that holds video.ts and no audio.ts — a job in flight under v2.9.8 to
// v2.9.10, which recorded the video-only variant alone, or a crash before
// the rendition wrote its first segment — sets the lone video.ts aside with
// its sidecar and starts both halves together where it stopped (its
// sidecar's next sequence, else the restart's seed), instead of resuming
// video.ts and starting the rendition at the playlist window an hour back.
// A staged pair, and a refresh into the running part (not a setup), leave
// the part's files alone.
//
// Mutants: pairLoneHlsHalf not called (the lone half stays, audio unforced);
// the setup gate dropped (the refresh sets the part's video.ts aside); the
// seed ignored when there is no sidecar; a pair only one half of which
// resumes left as it is (the audio would start at the window).
func TestDownloadHlsPairsALoneStagedHalf(t *testing.T) {
	for _, tc := range []struct {
		name              string
		setup, sidecar    bool
		dated, withAudio  bool
		audioSidecar      bool
		seedV, seedA      int
		wantStart         int // both forced at it; 0: as the seeds say
		wantVideoSetAside bool
	}{
		{name: "older version's lone video", setup: true, sidecar: true, wantStart: 42, wantVideoSetAside: true},
		{name: "dated lone video", setup: true, sidecar: true, dated: true, wantStart: 42, wantVideoSetAside: true},
		{name: "seeded restart, sidecar wins", setup: true, sidecar: true, seedV: 40, seedA: 40, wantStart: 42, wantVideoSetAside: true},
		{name: "seeded restart, no sidecar", setup: true, seedV: 900, seedA: 900, wantStart: 900, wantVideoSetAside: true},
		{name: "no sidecar, no seed", setup: true, wantVideoSetAside: true},
		{name: "a staged pair", setup: true, sidecar: true, withAudio: true, audioSidecar: true},
		{name: "a staged pair only the video of which resumes", setup: true, sidecar: true, withAudio: true, wantStart: 42, wantVideoSetAside: true},
		{name: "a staged pair neither half of which resumes", setup: true, withAudio: true},
		{name: "a refresh into the running part", sidecar: true, seedV: 42},
	} {
		t.Run(tc.name, func(t *testing.T) {
			built := recordDownloaders(t)
			srv := newHlsAudioServer(t, demuxedTestMaster)
			job, _ := vodPotJob(t)
			job.Config.MaxVideoResolution = 1080
			job.PairStagedHalves = tc.setup
			job.VideoStartSeq, job.AudioStartSeq = tc.seedV, tc.seedA
			video := filepath.Join(job.StagingDir, "video.ts")
			body := []byte("[v/itag/270/sq/40][v/itag/270/sq/41]")
			if err := os.WriteFile(video, body, 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.sidecar {
				state, _ := json.Marshal(engine.ResumeState{LastSeq: 41, BytesWritten: int64(len(body)), Timestamp: time.Now().Unix(), BaseURL: srv.URL + "/master/v/itag/270/index.m3u8"})
				if err := os.WriteFile(video+".resume.json", state, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.dated {
				noteMediaStart(video, time.Date(2026, 11, 28, 1, 14, 5, 0, time.UTC))
			}
			if tc.withAudio {
				audio := filepath.Join(job.StagingDir, hlsAudioFile)
				if err := os.WriteFile(audio, []byte("[a]"), 0o644); err != nil {
					t.Fatal(err)
				}
				if tc.audioSidecar {
					state, _ := json.Marshal(engine.ResumeState{LastSeq: 41, BytesWritten: 3, Timestamp: time.Now().Unix(), BaseURL: srv.URL + "/master/a/itag/234/index.m3u8"})
					if err := os.WriteFile(audio+".resume.json", state, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				asideAudio, _ := filepath.Glob(audio + engine.StagedRestartSuffix + "*")
				defer func() {
					got, _ := filepath.Glob(audio + engine.StagedRestartSuffix + "*")
					if (len(got) > len(asideAudio)) != tc.wantVideoSetAside {
						t.Errorf("audio.ts set aside = %v, want %v", len(got) > len(asideAudio), tc.wantVideoSetAside)
					}
				}()
			}
			if _, err := DownloadHls(context.Background(), job, hlsAudioInfo(srv, "visionos"), nil, nil, nil, nil); err != nil {
				t.Fatalf("DownloadHls: %v", err)
			}
			if len(*built) != 2 {
				t.Fatalf("built %d downloaders, want 2", len(*built))
			}
			v, a := (*built)[0], (*built)[1]
			wantV, wantA := tc.wantStart, tc.wantStart
			if tc.wantStart == 0 {
				wantV, wantA = tc.seedV, tc.seedA
			}
			if v.ForceStartSeq != (wantV > 0) || (wantV > 0 && v.StartSeq != wantV) || a.ForceStartSeq != (wantA > 0) || (wantA > 0 && a.StartSeq != wantA) {
				t.Errorf("starts: video %d/%v audio %d/%v, want %d and %d (forced when > 0)", v.StartSeq, v.ForceStartSeq, a.StartSeq, a.ForceStartSeq, wantV, wantA)
			}
			if fileExists(video) == tc.wantVideoSetAside {
				t.Errorf("video.ts still staged = %v, want %v", fileExists(video), !tc.wantVideoSetAside)
			}
			asides, _ := filepath.Glob(video + engine.StagedRestartSuffix + "*")
			var media []string
			for _, p := range asides {
				if engine.IsStagedRestartPath(p) {
					media = append(media, p)
				}
			}
			if (len(media) == 1) != tc.wantVideoSetAside {
				t.Errorf("asides = %v, want set aside %v", asides, tc.wantVideoSetAside)
			}
			if tc.wantVideoSetAside && tc.sidecar && len(media) == 1 && !fileExists(engine.StagedRestartSidecar(media[0])) {
				t.Errorf("the aside's sidecar did not go with it: %v", asides)
			}
		})
	}
}

// TestSetupPairsALoneStagedHalf is the lone-half rule through the job's real
// setup (setupWithFallback): the mark it sets reaches DownloadHls on the job
// it passes down, so the lone video.ts is set aside and both halves start
// together at its sidecar's next sequence, and the mark is cleared again for
// the refreshes that follow.
//
// Mutant: setupWithFallback not setting the mark (the lone half resumes,
// the audio starts unforced at the window).
func TestSetupPairsALoneStagedHalf(t *testing.T) {
	built := recordDownloaders(t)
	srv := newHlsAudioServer(t, demuxedTestMaster)
	job, _ := vodPotJob(t)
	job.Config.MaxVideoResolution = 1080
	video := filepath.Join(job.StagingDir, "video.ts")
	body := []byte("[v/itag/270/sq/40][v/itag/270/sq/41]")
	if err := os.WriteFile(video, body, 0o644); err != nil {
		t.Fatal(err)
	}
	state, _ := json.Marshal(engine.ResumeState{LastSeq: 41, BytesWritten: int64(len(body)), Timestamp: time.Now().Unix(), BaseURL: srv.URL + "/master/v/itag/270/index.m3u8"})
	if err := os.WriteFile(video+".resume.json", state, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := setupWithFallback(context.Background(), job, HlsStrategy, false, hlsAudioInfo(srv, "visionos"), &StrategyDeps{}); err != nil {
		t.Fatalf("setupWithFallback: %v", err)
	}
	if job.PairStagedHalves {
		t.Error("the setup left its mark on the job for the refreshes that follow")
	}
	if len(*built) != 2 {
		t.Fatalf("built %d downloaders, want 2", len(*built))
	}
	v, a := (*built)[0], (*built)[1]
	if v.StartSeq != 42 || !v.ForceStartSeq || a.StartSeq != 42 || !a.ForceStartSeq {
		t.Errorf("starts: video %d/%v audio %d/%v, want both 42, forced", v.StartSeq, v.ForceStartSeq, a.StartSeq, a.ForceStartSeq)
	}
	if fileExists(video) {
		t.Error("the lone video.ts is still staged")
	}
}

// TestDownloadHlsLevelsAFreshPart: the two halves of a demuxed HLS capture
// do not advance together while they catch up, so a part split
// mid-catch-up (carriedStartSeqs carries each half's own position: video
// 500, audio 560) or a restart seeded from last_video_seq and last_audio_seq
// can name two starts minutes apart. A fresh part starts both at the
// earlier one; a part whose halves are staged continues each file where it
// stands, since moving one back would append segments it already holds.
//
// Mutants: the levelling dropped (500 and 560); the max taken instead of
// the min; the staged-file check dropped (the staged audio is moved back).
func TestDownloadHlsLevelsAFreshPart(t *testing.T) {
	for _, tc := range []struct {
		name         string
		staged       bool
		v, a         int
		wantV, wantA int
	}{
		{"fresh part, audio ahead", false, 500, 560, 500, 500},
		{"fresh part, video ahead", false, 560, 500, 500, 500},
		{"fresh part, level", false, 500, 500, 500, 500},
		{"staged part continues each file", true, 500, 560, 500, 560},
	} {
		t.Run(tc.name, func(t *testing.T) {
			built := recordDownloaders(t)
			srv := newHlsAudioServer(t, demuxedTestMaster)
			job, _ := vodPotJob(t)
			job.Config.MaxVideoResolution = 1080
			job.VideoStartSeq, job.AudioStartSeq = tc.v, tc.a
			if tc.staged {
				for _, name := range []string{"video.ts", hlsAudioFile} {
					if err := os.WriteFile(filepath.Join(job.StagingDir, name), []byte("[x]"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := DownloadHls(context.Background(), job, hlsAudioInfo(srv, "visionos"), nil, nil, nil, nil); err != nil {
				t.Fatalf("DownloadHls: %v", err)
			}
			if len(*built) != 2 {
				t.Fatalf("built %d downloaders, want 2", len(*built))
			}
			v, a := (*built)[0], (*built)[1]
			if v.StartSeq != tc.wantV || !v.ForceStartSeq || a.StartSeq != tc.wantA || !a.ForceStartSeq {
				t.Errorf("starts: video %d/%v audio %d/%v, want %d and %d, forced", v.StartSeq, v.ForceStartSeq, a.StartSeq, a.ForceStartSeq, tc.wantV, tc.wantA)
			}
		})
	}
}
