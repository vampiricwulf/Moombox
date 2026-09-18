package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/engine"
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
// Mutant: setting DiscardStaged unconditionally — a live manifest-free
// recording's staged file is O_TRUNC'd on every restart.
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

// ftypHeader is the first eight bytes of every MP4/M4A a manifest-free DASH
// capture that began at sq=0 writes: a box length followed by the 'ftyp' type.
var ftypHeader = []byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p'}

// stagedManifestlessFile writes a staged recording of n bytes at
// <dir>/video_stream, beginning with head, and returns its path.
func stagedManifestlessFile(t *testing.T, head []byte, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "video_stream")
	body := make([]byte, n)
	copy(body, head)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write staged file: %v", err)
	}
	return path
}

// asideOf returns the single <path>.restart-<ts> media file beside path, or
// "". The sidecar travels with it as <path>.restart-<ts>.resume.json, which is
// not the recording and is filtered out here.
func asideOf(t *testing.T, path string) string {
	t.Helper()
	all, err := filepath.Glob(path + ".restart-*")
	if err != nil {
		t.Fatalf("glob aside files: %v", err)
	}
	var matches []string
	for _, m := range all {
		if !strings.HasSuffix(m, ".resume.json") {
			matches = append(matches, m)
		}
	}
	switch len(matches) {
	case 0:
		return ""
	case 1:
		return matches[0]
	default:
		t.Fatalf("expected at most one aside file, got %v", matches)
		return ""
	}
}

// TestManifestlessStagingKeepsHeadedRecording is fix round 1, Important 1: the
// DiscardStaged opt-in must not destroy a muxable recording just because the
// stream probes post-live.
//
// Both rows are the re-entry the review named — a Downloading / incomplete_tail
// row re-entered over its old staging dir, whose sidecar the engine will
// REFUSE (corrupt JSON; or a timestamp past maxResumeStateAge = 7 days). The
// staged file is headed, so it can be muxed as it stands and must survive: it
// is moved aside, not truncated, and the fresh sq=0 file starts beside it.
//
// The engine downloader is driven for real afterwards, because the assertion
// that matters is not what the predicate returned but what is left on disk.
//
// Mutant: restoring the broad predicate (prepareManifestlessStaging = a bare
// manifestlessDiscardStaged, no inspection and no aside) — the engine opens
// the recording O_TRUNC and both rows lose 1 MiB.
func TestManifestlessStagingKeepsHeadedRecording(t *testing.T) {
	const streamURL = "http://127.0.0.1:1/videoplayback?id=abcdefghijk.1&itag=140"
	const staged = 1 << 20

	for _, tc := range []struct {
		name    string
		sidecar []byte
	}{
		{
			name:    "corrupt sidecar",
			sidecar: []byte("{not json"),
		},
		{
			name: "sidecar aged past maxResumeStateAge",
			sidecar: mustJSON(t, engine.ResumeState{
				LastSeq:      4100,
				BytesWritten: staged,
				Timestamp:    time.Now().Add(-8 * 24 * time.Hour).Unix(),
				BaseURL:      streamURL,
			}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := stagedManifestlessFile(t, ftypHeader, staged)
			if err := os.WriteFile(path+".resume.json", tc.sidecar, 0o644); err != nil {
				t.Fatalf("write sidecar: %v", err)
			}

			discard := prepareManifestlessStaging(path, youtube.StreamPostLive, false, nopWorkerLogger{})

			aside := asideOf(t, path)
			if aside == "" {
				t.Fatalf("no <file>.restart-* beside %s — the headed recording was not preserved", path)
			}
			if info, err := os.Stat(aside); err != nil || info.Size() != staged {
				t.Fatalf("aside file %s: err=%v size=%d, want %d bytes of recording", aside, err, sizeOrZero(info), staged)
			}

			// Now let the engine do exactly what the returned flag permits.
			d := engine.NewSegmentDownloader(engine.DownloaderOptions{
				BaseURL:       streamURL,
				OutputFile:    path,
				DiscardStaged: discard,
				MaxRetries:    1,
			})
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			_ = d.Start(ctx)

			if info, err := os.Stat(aside); err != nil || info.Size() != staged {
				t.Fatalf("after Start, aside file %s: err=%v size=%d, want the %d-byte recording intact",
					aside, err, sizeOrZero(info), staged)
			}
		})
	}
}

// TestManifestlessStagingDiscardsWhatCannotBeMuxed pins the other half of the
// narrowed rule: an empty staging dir keeps the ordinary byte-identical fresh
// start, and a non-empty file with no container header is disposable — a bare
// moof+mdat run no muxer can open. Neither produces an aside file, because
// there is nothing worth keeping.
//
// Mutant: treating every non-empty file as headed (stagedRecordingHeaded =
// true) — the unrecognisable row grows an aside file for unmuxable bytes and
// the staging dir fills up on every restart.
func TestManifestlessStagingDiscardsWhatCannotBeMuxed(t *testing.T) {
	t.Run("absent staging", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "video_stream")
		if !prepareManifestlessStaging(path, youtube.StreamPostLive, false, nopWorkerLogger{}) {
			t.Fatal("prepareManifestlessStaging = false for absent staging, want the ordinary fresh start")
		}
		if aside := asideOf(t, path); aside != "" {
			t.Fatalf("aside file %s created for absent staging", aside)
		}
	})

	t.Run("empty staging", func(t *testing.T) {
		path := stagedManifestlessFile(t, nil, 0)
		if !prepareManifestlessStaging(path, youtube.StreamPostLive, false, nopWorkerLogger{}) {
			t.Fatal("prepareManifestlessStaging = false for empty staging, want the ordinary fresh start")
		}
		if aside := asideOf(t, path); aside != "" {
			t.Fatalf("aside file %s created for empty staging", aside)
		}
	})

	t.Run("unrecognisable staging", func(t *testing.T) {
		// A bare fragment: 'moof' where a complete file carries 'ftyp'.
		path := stagedManifestlessFile(t, []byte{0x00, 0x00, 0x01, 0x00, 'm', 'o', 'o', 'f'}, 4096)
		if !prepareManifestlessStaging(path, youtube.StreamPostLive, false, nopWorkerLogger{}) {
			t.Fatal("prepareManifestlessStaging = false for headerless staging, want it discarded")
		}
		if aside := asideOf(t, path); aside != "" {
			t.Fatalf("aside file %s created for bytes no muxer can open", aside)
		}
	})
}

// TestManifestlessStagingNeverTouchesLiveOrForced pins that the narrowing did
// not widen the opt-in: a LIVE capture and a part force-starting at an
// orchestrator-provided seq are still refused outright, and — crucially — the
// staged file is left exactly where it is, not moved aside.
//
// Mutant: dropping the manifestlessDiscardStaged short-circuit — a live
// recording's staging file is renamed out from under the running resume.
func TestManifestlessStagingNeverTouchesLiveOrForced(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status youtube.StreamStatus
		forced bool
	}{
		{"live", youtube.StreamLive, false},
		{"forced start seq", youtube.StreamPostLive, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := stagedManifestlessFile(t, ftypHeader, 4096)
			if prepareManifestlessStaging(path, tc.status, tc.forced, nopWorkerLogger{}) {
				t.Fatal("prepareManifestlessStaging = true, want no discard for a resumable capture")
			}
			if aside := asideOf(t, path); aside != "" {
				t.Fatalf("aside file %s created for a capture that must keep appending", aside)
			}
			if info, err := os.Stat(path); err != nil || info.Size() != 4096 {
				t.Fatalf("staging at %s: err=%v size=%d, want it untouched", path, err, sizeOrZero(info))
			}
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func sizeOrZero(info os.FileInfo) int64 {
	if info == nil {
		return 0
	}
	return info.Size()
}
