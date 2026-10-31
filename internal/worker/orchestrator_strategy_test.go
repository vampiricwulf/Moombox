package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// adaptiveVideo / adaptiveAudio build the split video-only + audio-only
// adaptive entries that HasManifestlessDashFormats keys on.
func adaptiveVideo() youtube.Format {
	return youtube.Format{Itag: 137, URL: "https://v.example/videoplayback?x=1", MimeType: "video/mp4", Width: new(1920), Height: new(1080)}
}
func adaptiveAudio() youtube.Format {
	return youtube.Format{Itag: 140, URL: "https://a.example/videoplayback?x=2", MimeType: "audio/mp4"}
}
func progressive() youtube.Format {
	return youtube.Format{Itag: 18, URL: "https://p.example/videoplayback?x=3", MimeType: "video/mp4", Width: new(640), Height: new(360)}
}

// TestSelectDownloadStrategy pins the routing table.
//
// Mutants (G6, G24; each run): the live adaptive arm dropped (both "live
// adaptive split" rows go to vod); the OTF-with-a-manifest arm dropped (the
// not-a-stream OTF row goes to vod); that arm taken without
// hasSegmentedFormats (the not-a-stream whole-file row goes to dash).
func TestSelectDownloadStrategy(t *testing.T) {
	manifestlessFormats := []youtube.Format{adaptiveVideo(), adaptiveAudio()}
	wholeFileFormats := []youtube.Format{adaptiveVideo(), adaptiveAudio()}
	wholeFileFormats[0].ContentLength, wholeFileFormats[1].ContentLength = "1048576000", "117000000"
	liveAdaptiveFormats := []youtube.Format{adaptiveVideo(), adaptiveAudio()}
	liveAdaptiveFormats[0].TargetDurationSec, liveAdaptiveFormats[1].TargetDurationSec = 2, 2
	otfFormats := []youtube.Format{adaptiveVideo(), adaptiveAudio()}
	otfFormats[0].IsOTF, otfFormats[1].IsOTF = true, true

	tests := []struct {
		name  string
		isVod bool
		info  *youtube.VideoInfo
		want  string // DownloadStrategy.Kind()
	}{
		{
			// The bug: a was-live stream that just ended is StreamPostLive
			// with segment-only adaptive URLs and no dashManifestUrl. It must
			// use the manifestless (&sq) path, not the whole-file VodStrategy.
			name:  "post-live manifestless routes to manifestless DASH",
			isVod: true,
			info: &youtube.VideoInfo{
				StreamStatus:    youtube.StreamPostLive,
				DashManifestURL: "",
				Formats:         manifestlessFormats,
			},
			want: "manifestless_dash",
		},
		{
			// Regression (hundreds-of-GB runaway): a premiere is classified
			// StreamPostLive, but its adaptive URLs are COMPLETE FILES
			// (contentLength set) served whole — not &sq segments. It MUST use
			// VodStrategy; the &sq loop makes YouTube return the whole file for
			// every seq (no past-end 403), an infinite re-download.
			name:  "premiere post-live with complete-file formats uses VodStrategy",
			isVod: true,
			info: &youtube.VideoInfo{
				StreamStatus:    youtube.StreamPostLive,
				DashManifestURL: "",
				Formats: []youtube.Format{
					{Itag: 248, URL: "https://v.example/videoplayback?x=1", MimeType: "video/webm", Width: new(1920), Height: new(1080), ContentLength: "58382400"},
					{Itag: 251, URL: "https://a.example/videoplayback?x=2", MimeType: "audio/webm", ContentLength: "3211436"},
				},
			},
			want: "vod",
		},
		{
			// Non-regression: a genuine finished VOD (StreamVOD) whose
			// adaptive formats are whole files keeps using VodStrategy.
			name:  "genuine VOD with whole-file formats stays on VodStrategy",
			isVod: true,
			info: &youtube.VideoInfo{
				StreamStatus: youtube.StreamVOD,
				Formats:      wholeFileFormats,
			},
			want: "vod",
		},
		{
			// Updated deliberately with G6: the shape alone — open-ended,
			// no contentLength — is still no reason to leave the whole-file
			// path for a VOD. yt-dlp downloads such a format over https too;
			// only its two marks of a segmented format (targetDurationSec,
			// and OTF, below) are.
			name:  "VOD with open-ended formats that carry neither mark stays on VodStrategy",
			isVod: true,
			info: &youtube.VideoInfo{
				StreamStatus:    youtube.StreamVOD,
				DashManifestURL: "",
				Formats:         manifestlessFormats,
			},
			want: "vod",
		},
		{
			// G6's belt and braces: TV's post-live DVR answer as it read
			// before the extraction promoted it — StreamVOD, live adaptive
			// formats, no manifest. The whole-file GET of these fetched one
			// fragment.
			name:  "VOD with a live adaptive split routes to manifestless DASH",
			isVod: true,
			info: &youtube.VideoInfo{
				StreamStatus: youtube.StreamVOD,
				Formats:      liveAdaptiveFormats,
			},
			want: "manifestless_dash",
		},
		{
			name:  "VOD with a live adaptive split prefers manifestless DASH over a manifest",
			isVod: true,
			info: &youtube.VideoInfo{
				StreamStatus:    youtube.StreamVOD,
				DashManifestURL: "https://manifest.example/dash.mpd",
				Formats:         liveAdaptiveFormats,
			},
			want: "manifestless_dash",
		},
		{
			// G24: an OTF upload goes to its DASH manifest, as yt-dlp's does;
			// only not_a_stream reaches the whole-file path with a manifest.
			name:  "not-a-stream OTF with a manifest uses DashStrategy",
			isVod: true,
			info: &youtube.VideoInfo{
				StreamStatus:    youtube.StreamNotAStream,
				DashManifestURL: "https://manifest.example/dash.mpd",
				Formats:         otfFormats,
			},
			want: "dash",
		},
		{
			name:  "not-a-stream whole-file with a manifest stays on VodStrategy",
			isVod: true,
			info: &youtube.VideoInfo{
				StreamStatus:    youtube.StreamNotAStream,
				DashManifestURL: "https://manifest.example/dash.mpd",
				Formats:         wholeFileFormats,
			},
			want: "vod",
		},
		{
			// No manifest to fall back to: the whole-file path takes what it
			// can fetch whole, and fails clearly when that loses a stream
			// (TestDownloadVodLeavesSegmentedFormatsOut).
			name:  "OTF VOD without a manifest goes to VodStrategy",
			isVod: true,
			info: &youtube.VideoInfo{
				StreamStatus: youtube.StreamVOD,
				Formats:      otfFormats,
			},
			want: "vod",
		},
		{
			name:  "not-a-stream uses VodStrategy",
			isVod: true,
			info: &youtube.VideoInfo{
				StreamStatus:    youtube.StreamNotAStream,
				DashManifestURL: "",
				Formats:         []youtube.Format{progressive()},
			},
			want: "vod",
		},
		{
			// Routing inversion (yt-dlp 8c1f07d81): post-live prefers the
			// manifest-free path even when a dash manifest is present —
			// post-live manifests only cover the trailing ~2h window.
			name:  "post-live with dash manifest prefers manifestless DASH",
			isVod: true,
			info: &youtube.VideoInfo{
				StreamStatus:    youtube.StreamPostLive,
				DashManifestURL: "https://manifest.example/dash.mpd",
				Formats:         manifestlessFormats,
			},
			want: "manifestless_dash",
		},
		{
			// Routing inversion (yt-dlp 8c1f07d81): live streams with split
			// adaptive URLs use manifest-free DASH even when a manifest is
			// present — yt-dlp declared live DASH manifests "no longer
			// properly supported" as of 2026-07.
			name:  "live with dash manifest prefers manifestless DASH",
			isVod: false,
			info: &youtube.VideoInfo{
				StreamStatus:    youtube.StreamLive,
				DashManifestURL: "https://manifest.example/dash.mpd",
				Formats:         manifestlessFormats,
			},
			want: "manifestless_dash",
		},
		{
			// The manifest-driven path survives as the fallback when the
			// format pool lacks usable split adaptive URLs.
			name:  "live with dash manifest but no split formats falls back to DashStrategy",
			isVod: false,
			info: &youtube.VideoInfo{
				StreamStatus:    youtube.StreamLive,
				DashManifestURL: "https://manifest.example/dash.mpd",
				Formats:         []youtube.Format{progressive()},
			},
			want: "dash",
		},
		{
			name:  "live manifestless uses manifestless DASH (unchanged)",
			isVod: false,
			info: &youtube.VideoInfo{
				StreamStatus:    youtube.StreamLive,
				DashManifestURL: "",
				Formats:         manifestlessFormats,
			},
			want: "manifestless_dash",
		},
		{
			name:  "live HLS-only uses HlsStrategy",
			isVod: false,
			info: &youtube.VideoInfo{
				StreamStatus:   youtube.StreamLive,
				HlsManifestURL: "https://manifest.example/hls.m3u8",
				Formats:        nil,
			},
			want: "hls",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			strategy, err := selectDownloadStrategy(tt.isVod, tt.info)
			if err != nil {
				t.Fatalf("selectDownloadStrategy returned error: %v", err)
			}
			if got := strategy.Kind(); got != tt.want {
				t.Errorf("selectDownloadStrategy() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDbResumeSeq(t *testing.T) {
	tests := []struct {
		name         string
		streamStatus youtube.StreamStatus
		lastSeq      *int
		want         int
	}{
		{"live resumes from db seq", youtube.StreamLive, new(4000), 4000},
		{"live with no db seq starts at 0", youtube.StreamLive, nil, 0},
		{
			// The trap: a re-processed cancelled live job carries a stale
			// LastVideoSeq. A finished post-live VOD must start at sq=0 (which
			// carries the ftyp+moov init for manifestless DASH); seeding the
			// stale seq would skip the init and reproduce the moov-less file.
			name:         "post-live ignores stale db seq and starts at 0",
			streamStatus: youtube.StreamPostLive,
			lastSeq:      new(4000),
			want:         0,
		},
		{"genuine VOD ignores stale db seq and starts at 0", youtube.StreamVOD, new(4000), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dbResumeSeq(tt.streamStatus, tt.lastSeq); got != tt.want {
				t.Errorf("dbResumeSeq(%q, %v) = %d, want %d", tt.streamStatus, tt.lastSeq, got, tt.want)
			}
		})
	}
}

func TestSelectDownloadStrategyNoStrategy(t *testing.T) {
	_, err := selectDownloadStrategy(false, &youtube.VideoInfo{
		StreamStatus: youtube.StreamLive,
	})
	if err == nil {
		t.Fatal("expected error when no strategy is available, got nil")
	}
}
