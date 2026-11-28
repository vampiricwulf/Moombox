package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
