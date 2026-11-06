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

// TestSplitFinalizeRecordsPartOnesJoinOffset is owner decision CK-3 through
// the real split finalize: a live YouTube capture that split into parts
// stores where in the broadcast part 1 began (join_offset_seconds), read
// from part 1's own staged recording and recording-start entry by the
// single-file rules — so the chapter import, which lays the parts end to end
// from there, pre-fills its offset for a split archive too. Before CK-3 the
// split finalize never wrote the column.
//
//   - part 1 staged in the root as a fragmented MP4 that joined late: the
//     fragments' own timing, 208 s, whatever its sequence says;
//   - part 1 an MPEG-TS that joined late: its first sequence times the
//     cadence the finalize's result advertises (2500 × 2 s);
//   - part 1 recorded from sq=0: 0;
//   - part 1 in seg_0, the root holding a short span the split discarded
//     (a seg_0 dir makes the root no part: rootIsPartZero): seg_0's start,
//     never the root's;
//   - part 1's start never recorded: NULL, over an older value the row held;
//   - a Twitch split capture: not written.
//
// Mutants: the split finalize not writing join_offset_seconds (every row
// keeps its fixture value); splitJoinOffsetSeconds reading the root for a
// part 1 staged in seg_0 (the discarded span's 50 s); muxAndFinalize not
// handing its result to the split finalize (the TS join reads NULL); the platform gate dropped
// (the Twitch row reads 208); an unknown start leaving the older value.
func TestSplitFinalizeRecordsPartOnesJoinOffset(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	f64 := func(v float64) *float64 { return &v }
	fragment := func(offset int) func(t *testing.T, p string) {
		return func(t *testing.T, p string) { writeOffsetFragmentFixture(t, ffmpegPath, p, 4, offset) }
	}
	cases := []struct {
		name     string
		platform string
		partDir  string // part 1's staging dir, relative to the root
		file     string
		stage    func(t *testing.T, p string)
		starts   map[string]int
		hls      bool
		want     *float64
	}{
		{name: "late fMP4 join in the root", file: "video_stream", stage: fragment(208),
			starts: map[string]int{"video_stream": 100}, want: f64(208)},
		{name: "late TS join", file: "video.ts", hls: true,
			stage:  func(t *testing.T, p string) { writeOffsetTSFixture(t, ffmpegPath, p, 4, 77) },
			starts: map[string]int{"video.ts": 2500}, want: f64(5000)},
		{name: "from the start", file: "video_stream", stage: fragment(0),
			starts: map[string]int{"video_stream": 0}, want: f64(0)},
		{name: "part 1 in seg_0", partDir: "seg_0", file: "video_stream", stage: fragment(900),
			starts: map[string]int{"video_stream": 300}, want: f64(900)},
		{name: "start never recorded", file: "video_stream", stage: fragment(208)},
		{name: "Twitch", platform: "twitch", file: "video_stream", stage: fragment(208),
			starts: map[string]int{"video_stream": 100}, want: f64(4242)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, jobCtx := muxTestOrchestrator(t, notificationtest.New(), 0)
			o.muxer = engine.NewMuxer(ffmpegPath, discardLogger{})
			if tc.platform != "" {
				job := &database.Job{ID: "tw_mux", VideoID: "tw_mux", Title: "A Job", Platform: tc.platform, Status: database.StatusDownloading}
				if _, err := o.db.AddJob(job); err != nil {
					t.Fatal(err)
				}
				jobCtx.Job = job
			}
			// An older value the finalize must replace (or, for Twitch, leave).
			o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{"join_offset_seconds": 4242.0})
			jobCtx.Config = &JobConfig{}
			jobCtx.StagingDir = t.TempDir()
			jobCtx.OutputDir = t.TempDir()
			jobCtx.Filename = "a-job"

			// The root holds a short discarded span that sits 50 s in, when
			// part 1 is staged in a seg dir of its own.
			if tc.partDir != "" {
				fragment(50)(t, filepath.Join(jobCtx.StagingDir, "video_stream"))
				writeStarts(t, jobCtx.StagingDir, map[string]int{"video_stream": 25})
			}
			dir := filepath.Join(jobCtx.StagingDir, tc.partDir)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			tc.stage(t, filepath.Join(dir, tc.file))
			if tc.starts != nil {
				writeStarts(t, dir, tc.starts)
			}

			for i := range 2 {
				name := "a-job - part" + string(rune('1'+i)) + ".mp4"
				p := filepath.Join(jobCtx.OutputDir, name)
				if err := os.WriteFile(p, []byte("part"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := o.db.AddSegment(&database.Segment{JobID: jobCtx.Job.ID, SegmentIndex: i, Filename: name, FilePath: p, DurationSeconds: 100}); err != nil {
					t.Fatal(err)
				}
			}
			result := &DownloadResult{HasVideo: true, VideoFormat: &youtube.Format{TargetDurationSec: 2}}
			if tc.hls {
				result = &DownloadResult{HasVideo: true, IsHls: true, SegmentDurationSec: 2}
			}
			// The download pipeline's finalize, which takes the split
			// branch for a job with part rows.
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

// writeStarts writes dir's recording-start record.
func writeStarts(t *testing.T, dir string, starts map[string]int) {
	t.Helper()
	data, _ := json.Marshal(starts)
	if err := os.WriteFile(filepath.Join(dir, recordingStartFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
}
