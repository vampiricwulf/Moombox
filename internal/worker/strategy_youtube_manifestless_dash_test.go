package worker

import (
	"testing"

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
