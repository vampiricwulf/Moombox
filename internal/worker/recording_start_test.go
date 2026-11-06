package worker

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
	if err := o.runDownloaders(t.Context(), result); err != nil {
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

// TestAFreshRecordingDropsTheEntryItsNameLeft: a recording from sq=0 that
// was set aside (setAsideStagedMedia) leaves its entry in the record, and the
// fresh file of the same name a later capture writes must not inherit it.
// Here that capture meets a finished playlist whose window opens at 7
// outside StopOnGap, so the engine's VOD-parallel writer records it: the
// file's entry is 7 and the finalize does not read it as from the start.
// When the downloader opens the file empty, the entry is already gone — a
// listener the caller installed on OnStart, which still runs, finds none —
// so a fresh file whose first write never comes is of unknown start, not the
// earlier recording's sq=0. A resume keeps the entry: its file is the same
// recording.
//
// Mutants: the drop on a fresh start removed (the OnStart listener finds the
// earlier 0); the drop made on a resume too (the resumed row loses its
// entry); the caller's OnStart not chained (it never runs); the engine's
// VOD-parallel writer not reporting (the record ends up empty, not 7).
func TestAFreshRecordingDropsTheEntryItsNameLeft(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:7\n"+
				"#EXTINF:2.000,\nseg7.ts\n#EXTINF:2.000,\nseg8.ts\n#EXT-X-ENDLIST\n")
			return
		}
		fmt.Fprintf(w, "[%s]", r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	video := filepath.Join(dir, "video.ts")
	if err := os.WriteFile(video, []byte("from the start"), 0o644); err != nil {
		t.Fatal(err)
	}
	noteRecordingStart(video, 0)
	if _, err := setAsideStagedMedia(dir, liveShapeStagingNames); err != nil {
		t.Fatalf("setAsideStagedMedia: %v", err)
	}

	d := engine.NewSegmentDownloader(engine.DownloaderOptions{BaseURL: srv.URL + "/v.m3u8", OutputFile: video, StartSeq: -1, IsHls: true})
	heardStart := false
	var startsAtOpen map[string]int
	d.OnStart = func(int, bool) {
		heardStart = true
		startsAtOpen = readRecordingStarts(dir)
	}
	o := &DownloadOrchestrator{logger: discardLogger{}}
	if err := o.runDownloaders(t.Context(), &DownloadResult{IsHls: true, VideoPath: video, VideoDownloader: d}); err != nil {
		t.Fatalf("runDownloaders: %v", err)
	}
	if data, _ := os.ReadFile(video); string(data) != "[/seg7.ts][/seg8.ts]" {
		t.Fatalf("video.ts = %q, want the fresh capture", data)
	}
	if !heardStart {
		t.Fatal("the caller's OnStart listener never ran")
	}
	if seq, ok := startsAtOpen["video.ts"]; ok {
		t.Errorf("when the file opened empty its entry was still %d, want none", seq)
	}
	if got := readRecordingStarts(dir); !reflect.DeepEqual(got, map[string]int{"video.ts": 7}) {
		t.Errorf("recorded starts = %v, want the fresh capture's 7", got)
	}
	if recordedFromStart(video) {
		t.Error("a fresh recording from seq 7 read as from the start")
	}

	// A resume continues the recording the entry describes.
	noteRecordingStart(video, 0)
	resumed := engine.NewSegmentDownloader(engine.DownloaderOptions{BaseURL: srv.URL + "/v.m3u8", OutputFile: video, StartSeq: 9, IsHls: true})
	recordStartsOf(&DownloadResult{VideoPath: video, VideoDownloader: resumed})
	resumed.OnStart(9, true)
	if !recordedFromStart(video) {
		t.Errorf("a resume dropped the entry: %v", readRecordingStarts(dir))
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
