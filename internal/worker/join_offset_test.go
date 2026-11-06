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
//     is its first sequence times the advertised segment duration;
//   - a capture recorded from sq=0 is 0;
//   - a VOD, and a capture whose start was never recorded, stay NULL.
//
// Mutants: the finalize not writing join_offset_seconds (every row NULL);
// joinOffsetSeconds ignoring the probe (the DASH join reads 200); the probe
// trusted for any container (the TS join reads its PTS); recordedFromStart
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
		want   *float64
	}{
		{name: "late DASH join, read from the fragments", file: "video_stream",
			stage:  func(t *testing.T, p string) { writeOffsetFragmentFixture(t, ffmpegPath, p, 4, 208) },
			starts: map[string]int{"video_stream": 100}, target: 2, want: f64(208)},
		{name: "late HLS join, sequence times segment duration", file: "video.ts",
			stage:  func(t *testing.T, p string) { writeOffsetTSFixture(t, ffmpegPath, p, 4, 77) },
			starts: map[string]int{"video.ts": 2500}, target: 2, want: f64(5000)},
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
