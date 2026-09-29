package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/bgutils"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// ptrInt is a shared test helper for the *int format/seq fields. Kept as a
// named helper (not Go 1.26 new(expr)) for readability and gopls support.
func ptrInt(v int) *int { return &v }

func TestManifestlessSq0URL(t *testing.T) {
	if got := manifestlessSq0URL("https://x/videoplayback?a=1&b=2"); got != "https://x/videoplayback?a=1&b=2&sq=0" {
		t.Errorf("query-style url: got %q", got)
	}
	if got := manifestlessSq0URL("https://x/videoplayback"); got != "https://x/videoplayback?sq=0" {
		t.Errorf("no-query url: got %q", got)
	}
}

func TestHasManifestlessDashFormats(t *testing.T) {
	cases := []struct {
		name    string
		formats []youtube.Format
		want    bool
	}{
		{
			name:    "empty pool",
			formats: nil,
			want:    false,
		},
		{
			name: "only HLS muxed video formats — no audio-only entries",
			formats: []youtube.Format{
				{Itag: 300, MimeType: `video/mp4; codecs="avc1.4d4020,mp4a.40.2"`, URL: "https://x/", Width: ptrInt(1280), Height: ptrInt(720)},
				{Itag: 301, MimeType: `video/mp4; codecs="avc1.64002a,mp4a.40.2"`, URL: "https://x/", Width: ptrInt(1920), Height: ptrInt(1080)},
			},
			want: false,
		},
		{
			name: "split video + audio adaptive (the manifest-free DASH case)",
			formats: []youtube.Format{
				{Itag: 299, MimeType: `video/mp4; codecs="avc1.64002a"`, URL: "https://x/", Width: ptrInt(1920), Height: ptrInt(1080)},
				{Itag: 140, MimeType: `audio/mp4; codecs="mp4a.40.2"`, URL: "https://x/"},
			},
			want: true,
		},
		{
			// Complete-file adaptive formats (contentLength set) are whole files
			// downloaded directly, NOT &sq segments — a premiere/regular VOD, not
			// the manifest-free live DASH case. Feeding these to the &sq loop
			// re-downloads the whole file forever.
			name: "complete-file adaptive formats (contentLength set) are not segment-addressable",
			formats: []youtube.Format{
				{Itag: 248, MimeType: `video/webm; codecs="vp9"`, URL: "https://x/", Width: ptrInt(1920), Height: ptrInt(1080), ContentLength: "58382400"},
				{Itag: 251, MimeType: `audio/webm; codecs="opus"`, URL: "https://x/", ContentLength: "3211436"},
			},
			want: false,
		},
		{
			name: "audio-only without any video — incomplete (no DASH possible)",
			formats: []youtube.Format{
				{Itag: 140, MimeType: `audio/mp4; codecs="mp4a.40.2"`, URL: "https://x/"},
			},
			want: false,
		},
		{
			name: "video-only without audio — incomplete",
			formats: []youtube.Format{
				{Itag: 299, MimeType: `video/mp4; codecs="avc1.64002a"`, URL: "https://x/", Width: ptrInt(1920), Height: ptrInt(1080)},
			},
			want: false,
		},
		{
			name: "URL absent on a format — that format must not count",
			formats: []youtube.Format{
				{Itag: 299, MimeType: `video/mp4`, URL: "", Width: ptrInt(1920), Height: ptrInt(1080)},
				{Itag: 140, MimeType: `audio/mp4`, URL: "https://x/"},
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := HasManifestlessDashFormats(tc.formats)
			if got != tc.want {
				t.Errorf("HasManifestlessDashFormats() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPartitionManifestlessFormatsExcludesContentLength pins the fix for the
// mixed-pool runaway: HasManifestlessDashFormats excludes contentLength formats
// at the strategy gate, and the partition inside DownloadManifestlessDash MUST
// do the same. A whole-file itag that reaches the pool is contentLength-blind
// to SelectBestDashStream and, if higher-res, gets picked as "best" and fed to
// the &sq loop — which returns the entire file for every sequence forever.
// Excluding it from the pool is the strongest guarantee it is never selected.
func TestPartitionManifestlessFormatsExcludesContentLength(t *testing.T) {
	// A MIXED pool: OTF video+audio (segment-addressable, no contentLength) plus
	// a HIGHER-res whole-file video (contentLength set) and a URL-less format.
	formats := []youtube.Format{
		{Itag: 299, MimeType: `video/mp4; codecs="avc1.64002a"`, URL: "https://x/", Width: ptrInt(1920), Height: ptrInt(1080)},
		{Itag: 140, MimeType: `audio/mp4; codecs="mp4a.40.2"`, URL: "https://x/"},
		{Itag: 401, MimeType: `video/mp4; codecs="av01.0.12M.08"`, URL: "https://x/", Width: ptrInt(3840), Height: ptrInt(2160), ContentLength: "1073741824"},
		{Itag: 137, MimeType: `video/mp4; codecs="avc1.640028"`, URL: "", Width: ptrInt(1920), Height: ptrInt(1080)},
	}

	video, audio := partitionManifestlessFormats(formats)

	for _, s := range video {
		if s.Itag == 401 {
			t.Fatal("contentLength (whole-file) itag 401 must be excluded — it would feed the &sq runaway")
		}
		if s.Itag == 137 {
			t.Fatal("URL-less itag 137 must be excluded from the video pool")
		}
	}
	if len(video) != 1 || video[0].Itag != 299 {
		t.Fatalf("video pool = %+v, want exactly the OTF itag 299", video)
	}
	if len(audio) != 1 || audio[0].Itag != 140 {
		t.Fatalf("audio pool = %+v, want exactly the OTF itag 140", audio)
	}
}

// TestManifestlessDiscardStagedOnlyForNonLiveRestart pins the ONE caller that
// opts into engine.DownloaderOptions.DiscardStaged: a manifest-free post-live
// capture whose segments carry ftyp+moov only at sq=0 must be allowed to
// restart from 0 over staged bytes; a LIVE capture, and any part that
// force-starts at an orchestrator-provided seq, must not.
//
// This is the whole of the worker's part. What DiscardStaged then does to the
// bytes on disk — resume from a usable sidecar, set a headed recording aside,
// or discard unrecognisable bytes — belongs to the engine and is pinned by
// TestStartDiscardStaged* in internal/engine (fix round 2, R1).
//
// Mutant: setting DiscardStaged unconditionally — a live manifest-free
// recording is restarted from sq=0 on every re-entry.
func TestManifestlessDiscardStagedOnlyForNonLiveRestart(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status youtube.StreamStatus
		forced bool
		want   bool
	}{
		{"post-live restart discards", youtube.StreamPostLive, false, true},
		{"live restart never discards", youtube.StreamLive, false, false},
		{"forced start seq never discards", youtube.StreamPostLive, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := manifestlessDiscardStaged(tc.status, tc.forced); got != tc.want {
				t.Errorf("manifestlessDiscardStaged(%v, %v) = %v, want %v", tc.status, tc.forced, got, tc.want)
			}
		})
	}
}

// manifestlessPotInfo is a live VideoInfo with one open-ended (no
// contentLength) video and audio adaptive format from the given clients —
// the split shape the manifest-free path segments with &sq=N. Plain URLs
// with no sig and no n, so the routed stub solver passes them through.
func manifestlessPotInfo(videoSource, audioSource string) *youtube.VideoInfo {
	w, h, fps := 1920, 1080, 30
	return &youtube.VideoInfo{
		StreamStatus: youtube.StreamLive,
		PlayerURL:    "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js",
		Formats: []youtube.Format{
			{Itag: 299, URL: "http://127.0.0.1:1/videoplayback?itag=299", MimeType: `video/mp4; codecs="avc1.64002a"`, Bitrate: 9_000_000, Width: &w, Height: &h, Fps: &fps, Source: videoSource},
			{Itag: 140, URL: "http://127.0.0.1:1/videoplayback?itag=140", MimeType: `audio/mp4; codecs="mp4a.40.2"`, Bitrate: 128_000, Source: audioSource},
		},
	}
}

// TestDownloadManifestlessDashAttachesWebPOPerStream is the manifest-free
// half of the 2026-09-29 live-path fix. Here there is no manifest: each
// chosen format carries its own client, and video and audio can come from
// different ones (dedup keeps one copy per itag, whichever client ranked
// best). So the strategy mints once if EITHER stream is from a WebPO client
// and hands the token only to the downloader whose own format is; a
// visionos / android_vr stream rides bare and says so.
//
// Mutants: dropping the per-stream gate on the downloader options fails the
// mixed row (the visionos audio carries tok123); minting without the
// IsWebPOSource gate fails the visionos row's mint count; dropping the skip
// log fails the visionos and mixed rows.
func TestDownloadManifestlessDashAttachesWebPOPerStream(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		videoSource, audioSource string
		wantMints                int
		wantVideo, wantAudio     string
		wantSkips                []string // "stream=source" in log order
	}{
		{name: "tv_auth pair", videoSource: "tv_auth", audioSource: "tv_auth", wantMints: 1, wantVideo: "tok123", wantAudio: "tok123"},
		{name: "visionos pair", videoSource: "visionos", audioSource: "visionos", wantSkips: []string{"video=visionos", "audio=visionos"}},
		{name: "tv_auth video beside visionos audio", videoSource: "tv_auth", audioSource: "visionos", wantMints: 1, wantVideo: "tok123", wantSkips: []string{"audio=visionos"}},
		{name: "android_vr video beside web_safari audio", videoSource: "android_vr", audioSource: "web_safari", wantMints: 1, wantAudio: "tok123", wantSkips: []string{"video=android_vr"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := fakeVodMint(t, "tok123", nil)
			job, logs := vodPotJob(t)
			res, err := DownloadManifestlessDash(context.Background(), job, manifestlessPotInfo(tc.videoSource, tc.audioSource), stubCipherSolver{}, nil, &bgutils.PotProvider{}, nil)
			if err != nil {
				t.Fatalf("DownloadManifestlessDash: %v", err)
			}
			if res.VideoDownloader == nil || res.AudioDownloader == nil {
				t.Fatalf("want both downloaders, got video=%v audio=%v", res.VideoDownloader, res.AudioDownloader)
			}
			if n := int(calls.Load()); n != tc.wantMints {
				t.Errorf("mint ran %d times, want %d", n, tc.wantMints)
			}
			if got := res.VideoDownloader.PoToken(); got != tc.wantVideo {
				t.Errorf("video (%s) downloader token = %q, want %q", tc.videoSource, got, tc.wantVideo)
			}
			if got := res.AudioDownloader.PoToken(); got != tc.wantAudio {
				t.Errorf("audio (%s) downloader token = %q, want %q", tc.audioSource, got, tc.wantAudio)
			}
			var want []map[string]any
			for _, s := range tc.wantSkips {
				stream, source, _ := strings.Cut(s, "=")
				want = append(want, skipLine(job, source, "stream", stream))
			}
			assertSkipLines(t, logs, want...)
			if tc.wantMints == 1 {
				lines := potLines(logs, "[POT] GVS mint")
				if len(lines) != 1 || lines[0]["videoSource"] != tc.videoSource || lines[0]["audioSource"] != tc.audioSource {
					t.Errorf("[POT] GVS mint lines = %v, want one naming videoSource=%s audioSource=%s", lines, tc.videoSource, tc.audioSource)
				}
			}
		})
	}
}
