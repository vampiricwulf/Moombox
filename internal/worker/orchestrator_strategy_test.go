package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// adaptiveVideo / adaptiveAudio build the split video-only + audio-only
// adaptive entries that HasManifestlessDashFormats keys on.
func adaptiveVideo() youtube.Format {
	return youtube.Format{Itag: 137, URL: "https://v.example/videoplayback?x=1", MimeType: "video/mp4", Width: ptrInt(1920), Height: ptrInt(1080)}
}
func adaptiveAudio() youtube.Format {
	return youtube.Format{Itag: 140, URL: "https://a.example/videoplayback?x=2", MimeType: "audio/mp4"}
}
func progressive() youtube.Format {
	return youtube.Format{Itag: 18, URL: "https://p.example/videoplayback?x=3", MimeType: "video/mp4", Width: ptrInt(640), Height: ptrInt(360)}
}

func TestSelectDownloadStrategy(t *testing.T) {
	manifestlessFormats := []youtube.Format{adaptiveVideo(), adaptiveAudio()}

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
					{Itag: 248, URL: "https://v.example/videoplayback?x=1", MimeType: "video/webm", Width: ptrInt(1920), Height: ptrInt(1080), ContentLength: "58382400"},
					{Itag: 251, URL: "https://a.example/videoplayback?x=2", MimeType: "audio/webm", ContentLength: "3211436"},
				},
			},
			want: "vod",
		},
		{
			// Non-regression: a genuine finished VOD (StreamVOD) with the same
			// adaptive-format shape must KEEP using VodStrategy — its URLs serve
			// whole files, so the &sq path would be wrong.
			name:  "genuine VOD with adaptive formats stays on VodStrategy",
			isVod: true,
			info: &youtube.VideoInfo{
				StreamStatus:    youtube.StreamVOD,
				DashManifestURL: "",
				Formats:         manifestlessFormats,
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
			// A post-live stream that DOES expose a dash manifest should still
			// use the manifest-driven DASH path, not the manifestless fallback.
			name:  "post-live with dash manifest uses DashStrategy",
			isVod: true,
			info: &youtube.VideoInfo{
				StreamStatus:    youtube.StreamPostLive,
				DashManifestURL: "https://manifest.example/dash.mpd",
				Formats:         manifestlessFormats,
			},
			want: "dash",
		},
		{
			name:  "live with dash manifest uses DashStrategy",
			isVod: false,
			info: &youtube.VideoInfo{
				StreamStatus:    youtube.StreamLive,
				DashManifestURL: "https://manifest.example/dash.mpd",
				Formats:         manifestlessFormats,
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
		{"live resumes from db seq", youtube.StreamLive, ptrInt(4000), 4000},
		{"live with no db seq starts at 0", youtube.StreamLive, nil, 0},
		{
			// The trap: a re-processed cancelled live job carries a stale
			// LastVideoSeq. A finished post-live VOD must start at sq=0 (which
			// carries the ftyp+moov init for manifestless DASH); seeding the
			// stale seq would skip the init and reproduce the moov-less file.
			name:         "post-live ignores stale db seq and starts at 0",
			streamStatus: youtube.StreamPostLive,
			lastSeq:      ptrInt(4000),
			want:         0,
		},
		{"genuine VOD ignores stale db seq and starts at 0", youtube.StreamVOD, ptrInt(4000), 0},
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
