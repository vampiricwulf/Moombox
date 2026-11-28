package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// Capture A's and capture B's first-segment wall clocks (chat-alignment
// measurement, 2026-11-27): the Lofi Girl live-edge HLS capture's sq 1738862
// and a from-start DASH capture's sq=0.
const (
	captureAStart = "2026-11-27T22:56:50.408302Z"
	captureBStart = "2026-11-27T23:29:04.76416Z"
)

// datedPlaylistServer serves a finished live playlist whose window opens at
// media sequence 7, dated by one leading #EXT-X-PROGRAM-DATE-TIME (capture
// A's first segment, to the millisecond, as an HLS date-time carries it).
func datedPlaylistServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			var b strings.Builder
			b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:7\n")
			b.WriteString("#EXT-X-PROGRAM-DATE-TIME:2026-11-27T22:56:50.408Z\n")
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
	return srv
}

// TestRunDownloadersRecordsTheFirstSegmentsWallclock: every capture
// runDownloaders runs leaves, beside its staged file, the wall clock of the
// first segment written into it (mediaStartFile, owner ruling RV-19) — here
// a live HLS playlist's date-time — beside the sequence recordingStartFile
// keeps, and a caller's own OnFirstMedia listener does not stop it. A file
// started over drops the entry its name left before its first write, and a
// resume keeps it: the resumed file is the same recording, whose time zero
// has not moved.
//
// Mutants: recordStartsOf not installing the wall-clock listener (no
// entry); installing it only when OnFirstMedia is free (the audio, whose
// OnFirstMedia is the caller's, has no entry); the drop on a fresh start
// removed (the OnStart listener finds the old entry); the drop made on a
// resume (the resumed file loses its entry).
func TestRunDownloadersRecordsTheFirstSegmentsWallclock(t *testing.T) {
	srv := datedPlaylistServer(t)
	dir := t.TempDir()
	newDL := func(out string) *engine.SegmentDownloader {
		return engine.NewSegmentDownloader(engine.DownloaderOptions{BaseURL: srv.URL + "/playlist.m3u8", OutputFile: out, StartSeq: -1, IsHls: true, StopOnGap: true})
	}
	video, audio := filepath.Join(dir, "video_stream"), filepath.Join(dir, "audio_stream")
	noteMediaStartFixture(t, video, captureBStart) // an earlier recording of the same name
	result := &DownloadResult{VideoPath: video, AudioPath: audio, VideoDownloader: newDL(video), AudioDownloader: newDL(audio)}
	result.AudioDownloader.OnFirstMedia = func(int) {}
	var atOpen map[string]string
	result.VideoDownloader.OnStart = func(int, bool) { atOpen = readStagingRecord[string](dir, mediaStartFile) }

	o := &DownloadOrchestrator{logger: discardLogger{}}
	if err := o.runDownloaders(t.Context(), result); err != nil {
		t.Fatalf("runDownloaders: %v", err)
	}
	if _, ok := atOpen["video_stream"]; ok {
		t.Errorf("when the file opened empty its old entry was still there: %v", atOpen)
	}
	const pdt = "2026-11-27T22:56:50.408Z"
	if got := readStagingRecord[string](dir, mediaStartFile); !reflect.DeepEqual(got, map[string]string{"video_stream": pdt, "audio_stream": pdt}) {
		t.Errorf("recorded wall clocks = %v, want both files at %s", got, pdt)
	}

	resumed := engine.NewSegmentDownloader(engine.DownloaderOptions{BaseURL: srv.URL + "/playlist.m3u8", OutputFile: video, StartSeq: 9, IsHls: true})
	recordStartsOf(&DownloadResult{VideoPath: video, VideoDownloader: resumed})
	resumed.OnStart(9, true)
	if got, ok := mediaStartTime(video, ""); !ok || got != pdt {
		t.Errorf("after a resume mediaStartTime = %q, %v; want the first recording's %s", got, ok, pdt)
	}
}

func noteMediaStartFixture(t *testing.T, path, value string) {
	t.Helper()
	entries := readStagingRecord[string](filepath.Dir(path), mediaStartFile)
	if entries == nil {
		entries = map[string]string{}
	}
	entries[filepath.Base(path)] = value
	data, _ := json.Marshal(entries)
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), mediaStartFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestMediaStartTimePrefersTheVideo: the archive's time zero is the video
// input's first-segment wall clock. The audio's stands in only when the video
// recorded none and both are known to have begun at the same sequence, or
// when there is no video input; an entry that is not a time is unknown.
//
// Mutants: the audio preferred (row 1 reads the audio's); the same-start
// check dropped (the "different starts" and "start unknown" rows read the
// audio's); a malformed entry accepted.
func TestMediaStartTimePrefersTheVideo(t *testing.T) {
	for _, tc := range []struct {
		name         string
		video, audio string // entries, "" for none
		starts       map[string]int
		noVideo      bool
		want         string
	}{
		{name: "video's", video: captureBStart, audio: captureAStart, want: captureBStart},
		{name: "WebM video, same start: audio's", audio: captureBStart, starts: map[string]int{"video_stream": 0, "audio_stream": 0}, want: captureBStart},
		{name: "WebM video, different starts", audio: captureBStart, starts: map[string]int{"video_stream": 3, "audio_stream": 0}},
		{name: "WebM video, start unknown", audio: captureBStart},
		{name: "audio only", noVideo: true, audio: captureBStart, want: captureBStart},
		{name: "malformed", video: "yesterday"},
		{name: "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			video, audio := filepath.Join(dir, "video_stream"), filepath.Join(dir, "audio_stream")
			if tc.video != "" {
				noteMediaStartFixture(t, video, tc.video)
			}
			if tc.audio != "" {
				noteMediaStartFixture(t, audio, tc.audio)
			}
			if tc.starts != nil {
				writeStarts(t, dir, tc.starts)
			}
			if tc.noVideo {
				video = ""
			}
			got, ok := mediaStartTime(video, audio)
			if ok != (tc.want != "") || got != tc.want {
				t.Errorf("mediaStartTime = %q, %v; want %q", got, ok, tc.want)
			}
		})
	}
}

// TestFinalizeRecordsTheMediaStartTime is owner ruling RV-19's anchor
// through the real single-file finalize: a live YouTube capture's row learns
// the wall clock of its archive's time zero from the staged record; one
// whose first segment recorded none, and a VOD, read NULL over an older
// value; a Twitch row is not touched.
//
// Mutants: the finalize not writing media_start_time (the live row stays
// at the older value); the VOD guard dropped (the VOD row reads the
// staged value); the platform gate dropped (the Twitch row is cleared).
func TestFinalizeRecordsTheMediaStartTime(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	const older = "2026-01-01T00:00:00Z"
	for _, tc := range []struct {
		name     string
		platform string
		vod      bool
		entry    string
		want     string
	}{
		{name: "live capture", entry: captureAStart, want: captureAStart},
		{name: "no wall clock recorded"},
		{name: "VOD", vod: true, entry: captureAStart},
		{name: "Twitch", platform: "twitch", entry: captureAStart, want: older},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, jobCtx := muxTestOrchestrator(t, notificationtest.New(), 0)
			o.muxer = engine.NewMuxer(ffmpegPath, discardLogger{})
			if tc.platform != "" {
				job := &database.Job{ID: "tw_anchor", VideoID: "tw_anchor", Title: "A Job", Platform: tc.platform, Status: database.StatusDownloading}
				if _, err := o.db.AddJob(job); err != nil {
					t.Fatal(err)
				}
				jobCtx.Job = job
			}
			if tc.vod {
				o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{"is_vod": true})
				jobCtx.Job.IsVod = true
			}
			o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{"media_start_time": older})
			jobCtx.Config = &JobConfig{}
			jobCtx.StagingDir = t.TempDir()
			jobCtx.OutputDir = t.TempDir()
			jobCtx.Filename = "a-job"
			video := filepath.Join(jobCtx.StagingDir, "video.mp4")
			writeMuxFixture(t, ffmpegPath, video, 4)
			if tc.entry != "" {
				noteMediaStartFixture(t, video, tc.entry)
			}
			result := &DownloadResult{HasVideo: true, VideoPath: video, VideoFormat: &youtube.Format{TargetDurationSec: 2}}
			if err := o.muxAndFinalize(context.Background(), jobCtx, result); err != nil {
				t.Fatalf("muxAndFinalize: %v", err)
			}
			job, _ := o.db.GetJob(jobCtx.Job.ID)
			if job.Status != database.StatusFinished {
				t.Fatalf("status %s", job.Status)
			}
			if job.MediaStartTime != tc.want {
				t.Errorf("media_start_time = %q, want %q", job.MediaStartTime, tc.want)
			}
		})
	}
}

// TestSplitFinalizeKeepsPartOnesMediaStartTime: a split live YouTube
// archive's time zero is part 1's — the split archive's chat runs on one
// timeline from there — read from part 1's staging dir (seg_0 here, beside a
// root holding a discarded span and a part 2 in seg_1, each with its own
// later wall clock), so neither a later part nor the discarded span moves it.
//
// Mutants: the split finalize not writing media_start_time (NULL);
// splitMediaStartTime reading the root (the discarded span's) or the last
// part (part 2's).
func TestSplitFinalizeKeepsPartOnesMediaStartTime(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	o, jobCtx := muxTestOrchestrator(t, notificationtest.New(), 0)
	o.muxer = engine.NewMuxer(ffmpegPath, discardLogger{})
	jobCtx.Config = &JobConfig{}
	jobCtx.StagingDir = t.TempDir()
	jobCtx.OutputDir = t.TempDir()
	jobCtx.Filename = "a-job"
	stage := func(dir, wall string) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "video_stream")
		writeOffsetFragmentFixture(t, ffmpegPath, p, 4, 0)
		noteMediaStartFixture(t, p, wall)
	}
	stage(jobCtx.StagingDir, "2026-11-27T22:00:00Z")
	stage(filepath.Join(jobCtx.StagingDir, "seg_0"), captureAStart)
	stage(filepath.Join(jobCtx.StagingDir, "seg_1"), "2026-11-27T23:40:00Z")
	for i := range 2 {
		name := fmt.Sprintf("a-job - part%d.mp4", i+1)
		p := filepath.Join(jobCtx.OutputDir, name)
		if err := os.WriteFile(p, []byte("part"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := o.db.AddSegment(&database.Segment{JobID: jobCtx.Job.ID, SegmentIndex: i, Filename: name, FilePath: p, DurationSeconds: 100}); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.muxAndFinalize(context.Background(), jobCtx, &DownloadResult{HasVideo: true, VideoFormat: &youtube.Format{TargetDurationSec: 2}}); err != nil {
		t.Fatalf("muxAndFinalize: %v", err)
	}
	job, _ := o.db.GetJob(jobCtx.Job.ID)
	if job.Status != database.StatusFinished {
		t.Fatalf("status %s", job.Status)
	}
	if job.MediaStartTime != captureAStart {
		t.Errorf("media_start_time = %q, want part 1's %s", job.MediaStartTime, captureAStart)
	}
}
