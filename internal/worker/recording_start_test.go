package worker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/engine"
)

// TestRunDownloadersRecordsWhereEachRecordingBegins: every capture
// runDownloaders runs leaves, beside its staged file, the sequence the file
// began at (recordingStartFile) — here a live HLS playlist whose window
// opens at media sequence 7, so the fresh file begins at 7. A second
// downloader in the same dir adds its own entry beside the first, and a
// listener a caller installed is kept, not replaced.
//
// Mutants: runDownloaders not calling recordStartsOf (no record); the record
// rewritten from scratch on each note (the audio entry drops the video's);
// recordStartsOf replacing an installed listener (the caller's is not
// called).
func TestRunDownloadersRecordsWhereEachRecordingBegins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			var b strings.Builder
			b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:7\n")
			for i := 7; i < 9; i++ {
				fmt.Fprintf(&b, "#EXTINF:2.000,live\nseg%d.ts\n", i)
			}
			b.WriteString("#EXT-X-ENDLIST\n")
			fmt.Fprint(w, b.String())
			return
		}
		fmt.Fprintf(w, "[%s]", r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	newDL := func(out string) *engine.SegmentDownloader {
		return engine.NewSegmentDownloader(engine.DownloaderOptions{
			BaseURL:    srv.URL + "/playlist.m3u8",
			OutputFile: out,
			StartSeq:   -1,
			IsHls:      true,
			StopOnGap:  true,
		})
	}
	video, audio := filepath.Join(dir, "video_stream"), filepath.Join(dir, "audio_stream")
	result := &DownloadResult{VideoPath: video, AudioPath: audio, VideoDownloader: newDL(video), AudioDownloader: newDL(audio)}
	var callerHeard []int
	result.AudioDownloader.OnFirstMedia = func(seq int) { callerHeard = append(callerHeard, seq) }

	o := &DownloadOrchestrator{logger: discardLogger{}}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := o.runDownloaders(ctx, result); err != nil {
		t.Fatalf("runDownloaders: %v", err)
	}
	if got := readRecordingStarts(dir); !reflect.DeepEqual(got, map[string]int{"video_stream": 7}) {
		t.Errorf("recorded starts = %v, want the video's 7 (the audio's listener was the caller's)", got)
	}
	if !reflect.DeepEqual(callerHeard, []int{7}) {
		t.Errorf("the caller's own listener heard %v, want [7]", callerHeard)
	}

	noteRecordingStart(audio, 0)
	if got := readRecordingStarts(dir); !reflect.DeepEqual(got, map[string]int{"video_stream": 7, "audio_stream": 0}) {
		t.Errorf("after a second entry: %v, want both", got)
	}
}

// TestRecordedFromStart: a single-file mux's inputs are from the start only
// when each has a recorded start of 0; one late input, one with no record,
// or no input at all is not.
//
// Mutants: an input with no entry read as 0 (the unknown rows answer true);
// only the first input checked (the late-audio row answers true); no input
// answering true.
func TestRecordedFromStart(t *testing.T) {
	dir := t.TempDir()
	video, audio, other := filepath.Join(dir, "video_stream"), filepath.Join(dir, "audio_stream"), filepath.Join(dir, "video.ts")
	noteRecordingStart(video, 0)
	noteRecordingStart(audio, 0)
	if !recordedFromStart(video, audio) {
		t.Error("both from sq=0: want from the start")
	}
	if !recordedFromStart(video, "") {
		t.Error("video-only from sq=0: want from the start")
	}
	if recordedFromStart(video, other) {
		t.Error("an input with no recorded start answered from the start")
	}
	if recordedFromStart(filepath.Join(t.TempDir(), "video_stream")) {
		t.Error("a dir with no record answered from the start")
	}
	if recordedFromStart("", "") {
		t.Error("no inputs answered from the start")
	}
	noteRecordingStart(audio, 3600)
	if recordedFromStart(video, audio) {
		t.Error("a late audio answered from the start")
	}
}
