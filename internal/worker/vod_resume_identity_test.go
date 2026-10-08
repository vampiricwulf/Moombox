package worker

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// vodRenditionFill is the byte each test rendition is made of, by itag.
var vodRenditionFill = map[string]byte{"399": 'A', "136": 'B', "251": 'C', "140": 'D'}

// vodRenditionServer serves whole-file renditions of one VOD by itag, each
// size bytes of its vodRenditionFill behind an ftyp header, generated per
// request rather than held. While cut is set every rendition answers 404 from
// the 50 MB checkpoint on, so a run dies just past its first sidecar save.
// starts records each itag's chunk Range starts (the 1-byte probe excluded).
type vodRenditionServer struct {
	*httptest.Server
	mu     sync.Mutex
	cut    bool
	starts map[string][]int64
}

func newVodRenditionServer(t *testing.T, size int64) *vodRenditionServer {
	t.Helper()
	s := &vodRenditionServer{cut: true, starts: map[string][]int64{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		itag := r.URL.Query().Get("itag")
		var start, end int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		if end <= 0 || end >= size {
			end = size - 1
		}
		s.mu.Lock()
		cut := s.cut && start >= 10*engine.DownloadChunkSize
		if r.Header.Get("Range") != "bytes=0-0" {
			s.starts[itag] = append(s.starts[itag], start)
		}
		s.mu.Unlock()
		if cut {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body := bytes.Repeat([]byte{vodRenditionFill[itag]}, int(end-start+1))
		if start == 0 {
			copy(body, "\x00\x00\x00\x18ftypdash")
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s
}

// TestVodResumeChecksTheRendition pins the whole-file StreamID DownloadVod
// hands the engine for each stream. Every rendition stages to the same
// video.mp4 / audio.m4a, so the sidecar a run leaves is all that says whose
// prefix the bytes are; the URLs here carry no fingerprint and every
// rendition is the same length, so neither the URL identity nor the
// probed-total check can tell them apart and the StreamID (video, itag, clen)
// is the only thing that can. A run cut at its 50 MB checkpoints under
// max_video_resolution 1080 (itag 399) with audio itag 251 is resumed:
//
//   - with the cap lowered to 720 and the audio itag changed to 140, both
//     selections move and each file must be the new rendition's alone — each
//     used to be the old checkpoint with the new rendition's tail appended,
//     which muxed and finished clean;
//   - with both unchanged, the same renditions resume from the checkpoints
//     rather than starting over, so the identity is stable across two
//     extractions.
//
// Mutant: dropping `StreamID: vodStreamID(job.Job.VideoID,
// result.VideoFormat)` from the video downloader in DownloadVod — the video
// file splices. Mutant: dropping the audio downloader's StreamID — the audio
// file does.
func TestVodResumeChecksTheRendition(t *testing.T) {
	const size = 10*engine.DownloadChunkSize + 1<<20
	for _, tc := range []struct {
		name                 string
		resumeCap, audioItag int
		wantVideo, wantAudio string
		wantResume           bool
	}{
		{"a changed selection starts over", 720, 140, "136", "140", false},
		{"the same selection resumes", 1080, 251, "399", "251", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newVodRenditionServer(t, size)
			w1080, h1080, w720, h720, fps := 1920, 1080, 1280, 720, 30
			info := &youtube.VideoInfo{PlayerURL: "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js", StreamStatus: youtube.StreamVOD, Formats: []youtube.Format{
				{Itag: 399, URL: srv.URL + "/videoplayback?itag=399", MimeType: `video/mp4; codecs="av01.0.08M.08"`, Bitrate: 3_000_000, Width: &w1080, Height: &h1080, Fps: &fps, ContentLength: strconv.Itoa(size), Source: "android_vr"},
				{Itag: 136, URL: srv.URL + "/videoplayback?itag=136", MimeType: `video/mp4; codecs="avc1.4d401f"`, Bitrate: 1_500_000, Width: &w720, Height: &h720, Fps: &fps, ContentLength: strconv.Itoa(size), Source: "android_vr"},
				{Itag: 251, URL: srv.URL + "/videoplayback?itag=251", MimeType: `audio/webm; codecs="opus"`, Bitrate: 160_000, ContentLength: strconv.Itoa(size), Source: "android_vr"},
				{Itag: 140, URL: srv.URL + "/videoplayback?itag=140", MimeType: `audio/mp4; codecs="mp4a.40.2"`, Bitrate: 128_000, ContentLength: strconv.Itoa(size), Source: "android_vr"},
			}}
			job, _ := vodPotJob(t)
			job.Logger = discardLogger{} // two downloaders log at once
			o := &DownloadOrchestrator{logger: discardLogger{}}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			audio251 := 251
			job.Config.MaxVideoResolution = 1080
			job.Job.SelectedAudioItag = &audio251
			res1, err := DownloadVod(ctx, job, info, stubCipherSolver{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			// Each stream on its own, so neither one's cut cancels the other
			// before it reached its checkpoint.
			for _, d := range []*engine.SegmentDownloader{res1.VideoDownloader, res1.AudioDownloader} {
				if err := d.Start(ctx); err == nil {
					t.Fatal("run 1 finished; want it cut at the checkpoint")
				}
			}
			for _, p := range []string{res1.VideoPath, res1.AudioPath} {
				if !fileExists(p + ".resume.json") {
					t.Fatalf("run 1 left no sidecar beside %s", p)
				}
			}

			srv.mu.Lock()
			srv.cut, srv.starts = false, map[string][]int64{}
			srv.mu.Unlock()
			job.Config.MaxVideoResolution = tc.resumeCap
			job.Job.SelectedAudioItag = &tc.audioItag
			res2, err := DownloadVod(ctx, job, info, stubCipherSolver{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := o.runDownloaders(ctx, res2); err != nil {
				t.Fatalf("run 2: %v", err)
			}
			for _, s := range []struct{ path, itag string }{{res2.VideoPath, tc.wantVideo}, {res2.AudioPath, tc.wantAudio}} {
				got, _ := os.ReadFile(s.path)
				fill := vodRenditionFill[s.itag]
				if n := bytes.Count(got, []byte{fill}); int64(len(got)) != size || int64(n) != size-12 {
					t.Errorf("run 2 %s is %d bytes with %d of itag %s's — want itag %s's file alone (%d bytes)",
						s.path, len(got), n, s.itag, s.itag, size)
				}
				srv.mu.Lock()
				first := srv.starts[s.itag][0]
				srv.mu.Unlock()
				if resumed := first > 0; resumed != tc.wantResume {
					t.Errorf("run 2's first itag %s chunk started at byte %d; resumed = %v, want %v", s.itag, first, resumed, tc.wantResume)
				}
			}
		})
	}
}
