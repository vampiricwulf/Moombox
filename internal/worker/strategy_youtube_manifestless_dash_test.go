package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
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

// TestManifestlessPotBinding pins the GVS PO-token binding choice: the
// videoID binding belongs to the manifest-withholding experiment only; a
// response that DOES carry a dashManifestUrl (routed here because
// manifest-free is the primary live path since yt-dlp 8c1f07d81) must use
// the standard visitorData-style binding the manifest strategies use.
func TestManifestlessPotBinding(t *testing.T) {
	job := &JobContext{Job: &database.Job{ID: "j1", VideoID: "vid123"}}

	binding, label := manifestlessPotBinding(job, &youtube.VideoInfo{DashManifestURL: ""})
	if binding != "vid123" || label != "videoID" {
		t.Errorf("manifest withheld: binding=%q label=%q, want vid123/videoID", binding, label)
	}

	// Manifest present: falls through to poTokenBinding — with no YT session
	// wired, that resolves to the ChannelID fallback, which is precisely the
	// point: NOT the videoID. The label must report the actual source
	// ("channelID"), not claim visitorData when the fallback fired.
	info := &youtube.VideoInfo{DashManifestURL: "https://manifest.example/dash.mpd", ChannelID: "chan9"}
	binding, label = manifestlessPotBinding(job, info)
	if binding != "chan9" || label != "channelID" {
		t.Errorf("manifest present: binding=%q label=%q, want chan9/channelID", binding, label)
	}
}
