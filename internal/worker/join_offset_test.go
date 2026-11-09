package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestFinalizeRecordsTheJoinOffset is owner decision CJ-3 through the real
// single-file finalize: a live YouTube capture's row learns where in the
// broadcast its recording began (join_offset_seconds), which the chapter
// import's offset pre-fills from.
//
//   - a late join recorded as a fragmented MP4 (DASH) is read from the
//     staged file's own timing: its first fragment sits 208 s into the
//     broadcast, whatever its sequence times the segment duration says;
//   - a late join in an MPEG-TS (HLS), whose start_time is a PTS of no use,
//     is its first sequence times the advertised segment duration — carried
//     as the live HLS strategy carries it (SegmentDurationSec: its variants
//     map to no VideoFormat);
//   - a capture recorded from sq=0 is 0;
//   - a VOD, and a capture whose start was never recorded, stay NULL.
//
// Mutants: the finalize not writing join_offset_seconds (every row NULL);
// joinOffsetSeconds ignoring the probe (the DASH join reads 200); the probe
// trusted for any container (the TS join reads its PTS); the result's
// SegmentDurationSec ignored (the HLS join reads NULL); recordedFromStart
// not answering 0 (the from-start row reads NULL); the VOD guard dropped
// (the VOD row reads 0).
func TestFinalizeRecordsTheJoinOffset(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	f64 := func(v float64) *float64 { return &v }
	cases := []struct {
		name   string
		vod    bool
		stage  func(t *testing.T, path string)
		file   string
		starts map[string]int // nil: no recording-start.json
		target int
		hls    bool // the result as the live HLS strategy builds it
		want   *float64
	}{
		{name: "late DASH join, read from the fragments", file: "video_stream",
			stage:  func(t *testing.T, p string) { writeOffsetFragmentFixture(t, ffmpegPath, p, 4, 208) },
			starts: map[string]int{"video_stream": 100}, target: 2, want: f64(208)},
		{name: "late HLS join, sequence times segment duration", file: "video.ts",
			stage:  func(t *testing.T, p string) { writeOffsetTSFixture(t, ffmpegPath, p, 4, 77) },
			starts: map[string]int{"video.ts": 2500}, target: 2, hls: true, want: f64(5000)},
		{name: "from the start", file: "video.mp4",
			stage:  func(t *testing.T, p string) { writeMuxFixture(t, ffmpegPath, p, 4) },
			starts: map[string]int{"video.mp4": 0}, target: 2, want: f64(0)},
		{name: "start never recorded", file: "video.mp4",
			stage: func(t *testing.T, p string) { writeMuxFixture(t, ffmpegPath, p, 4) }, target: 2},
		{name: "VOD", vod: true, file: "video.mp4",
			stage:  func(t *testing.T, p string) { writeMuxFixture(t, ffmpegPath, p, 4) },
			starts: map[string]int{"video.mp4": 0}, target: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, jobCtx := muxTestOrchestrator(t, notificationtest.New(), 0)
			o.muxer = engine.NewMuxer(ffmpegPath, discardLogger{})
			if tc.vod {
				o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{"is_vod": true})
				jobCtx.Job.IsVod = true
			}
			jobCtx.Config = &JobConfig{}
			jobCtx.StagingDir = t.TempDir()
			jobCtx.OutputDir = t.TempDir()
			jobCtx.Filename = "a-job"
			video := filepath.Join(jobCtx.StagingDir, tc.file)
			tc.stage(t, video)
			if tc.starts != nil {
				data, _ := json.Marshal(tc.starts)
				if err := os.WriteFile(filepath.Join(jobCtx.StagingDir, recordingStartFile), data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			result := &DownloadResult{HasVideo: true, VideoPath: video, VideoFormat: &youtube.Format{TargetDurationSec: tc.target}}
			if tc.hls {
				result = &DownloadResult{HasVideo: true, IsHls: true, VideoPath: video, SegmentDurationSec: tc.target}
			}
			if err := o.muxAndFinalize(context.Background(), jobCtx, result); err != nil {
				t.Fatalf("muxAndFinalize: %v", err)
			}
			job, _ := o.db.GetJob(jobCtx.Job.ID)
			if job.Status != database.StatusFinished {
				t.Fatalf("status %s", job.Status)
			}
			got := job.JoinOffsetSeconds
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("join_offset_seconds = %v, want NULL", *got)
			case tc.want != nil && got == nil:
				t.Errorf("join_offset_seconds = NULL, want %v", *tc.want)
			case tc.want != nil && (*got < *tc.want-0.5 || *got > *tc.want+0.5):
				t.Errorf("join_offset_seconds = %v, want %v", *got, *tc.want)
			}
		})
	}
}

// TestJoinOffsetAsksEveryLateInput: a late input that cannot say where it
// joined (an MPEG-TS with no advertised segment duration) does not stop the
// join point being read from the next late input that can (a fragmented MP4
// whose fragments sit 208 s into the broadcast).
//
// Mutant: joinOffsetSeconds giving up at the first late input (NULL).
func TestJoinOffsetAsksEveryLateInput(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	o, _ := muxTestOrchestrator(t, notificationtest.New(), 0)
	o.muxer = engine.NewMuxer(ffmpegPath, discardLogger{})
	dir := t.TempDir()
	video := filepath.Join(dir, "video.ts")
	audio := filepath.Join(dir, "audio_stream")
	writeOffsetTSFixture(t, ffmpegPath, video, 4, 77)
	writeOffsetFragmentFixture(t, ffmpegPath, audio, 4, 208)
	data, _ := json.Marshal(map[string]int{"video.ts": 2500, "audio_stream": 100})
	if err := os.WriteFile(filepath.Join(dir, recordingStartFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := o.joinOffsetSeconds(context.Background(), &DownloadResult{HasVideo: true, HasAudio: true, VideoPath: video, AudioPath: audio}, video, audio)
	if !ok || got < 207.5 || got > 208.5 {
		t.Errorf("joinOffsetSeconds = %v, %v; want 208 from the audio's fragments", got, ok)
	}
}

// TestReinitializeClearsTheJoinOffset: a Reinitialize and an automatic
// re-initialisation are new recordings, so the row forgets where the old
// one joined — a re-download finalized as a split archive never writes the
// column, and would otherwise pre-fill the next import with the old
// recording's join point (owner decision CJ-3).
//
// Mutants: either reset leaving join_offset_seconds (it still reads 5025).
func TestReinitializeClearsTheJoinOffset(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status database.JobStatus
		reset  func(w *DownloadWorker, id string) error
	}{
		{name: "Reinitialize", status: database.StatusError, reset: (*DownloadWorker).ReinitializeJob},
		{name: "AutoReinitialize", status: database.StatusError, reset: (*DownloadWorker).AutoReinitializeJob},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			id := "yt_join_" + tc.name
			if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, URL: "u", Platform: "youtube", Status: tc.status}); err != nil {
				t.Fatal(err)
			}
			db.UpdateJobFields(id, map[string]any{"join_offset_seconds": 5025.0})
			if job, _ := db.GetJob(id); job == nil || job.JoinOffsetSeconds == nil {
				t.Fatal("the fixture's join point was not stored")
			}
			tc.reset(w, id)
			job, _ := db.GetJob(id)
			if job == nil {
				t.Fatal("row gone")
			}
			if job.JoinOffsetSeconds != nil {
				t.Errorf("join_offset_seconds = %v after %s, want NULL", *job.JoinOffsetSeconds, tc.name)
			}
		})
	}
}
