package worker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

// TestFinalizeFlagsAnArchiveMissingAdvertisedAudio is the missing-audio
// guard through the real single-file finalize: an HLS capture whose master
// advertised sound (the run's result says so, or for a re-finalize the
// record DownloadHls left beside the staged file) but whose archive has no
// audio stream finishes Finished, flagged missing_audio, with a Warn naming
// the job. A genuinely video-only source (nothing advertised), and an
// archive that has its audio, are not flagged, and a flag an earlier
// finalize set is cleared.
//
// Mutants: the finalize not writing missing_audio (the flagged rows read
// false); the guard ignoring the staging record (the re-finalize row reads
// false); the guard ignoring what was advertised (the video-only source is
// flagged).
func TestFinalizeFlagsAnArchiveMissingAdvertisedAudio(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	for _, tc := range []struct {
		name        string
		advertised  bool // the run's result
		record      bool // the staging record, no result flag (a re-finalize)
		withAudio   bool
		wantMissing bool
	}{
		{name: "advertised, archive silent", advertised: true, wantMissing: true},
		{name: "re-finalize reads the record", record: true, wantMissing: true},
		{name: "video-only source"},
		{name: "advertised and present", advertised: true, withAudio: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, jobCtx := muxTestOrchestrator(t, notificationtest.New(), 0)
			o.muxer = engine.NewMuxer(ffmpegPath, discardLogger{})
			logs := &captureLogger{}
			o.logger = logs
			o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{"missing_audio": !tc.wantMissing})
			jobCtx.Config = &JobConfig{}
			jobCtx.StagingDir = t.TempDir()
			jobCtx.OutputDir = t.TempDir()
			jobCtx.Filename = "a-job"
			result := &DownloadResult{IsHls: true, HasVideo: true, VideoPath: filepath.Join(jobCtx.StagingDir, "video.ts"), AdvertisedAudio: tc.advertised}
			writeOffsetTSFixture(t, ffmpegPath, result.VideoPath, 4, 0)
			if tc.withAudio {
				result.HasAudio, result.AudioPath = true, filepath.Join(jobCtx.StagingDir, hlsAudioFile)
				writeOffsetAudioTSFixture(t, ffmpegPath, result.AudioPath, 4, 0)
			}
			if tc.record {
				noteAdvertisedAudio(&DownloadResult{VideoPath: result.VideoPath, AdvertisedAudio: true})
			}
			if err := o.muxAndFinalize(context.Background(), jobCtx, result); err != nil {
				t.Fatalf("muxAndFinalize: %v", err)
			}
			job, _ := o.db.GetJob(jobCtx.Job.ID)
			if job.Status != database.StatusFinished || job.Error != "" {
				t.Fatalf("status %s error %q, want an error-free Finished", job.Status, job.Error)
			}
			if job.MissingAudio != tc.wantMissing {
				t.Errorf("missing_audio = %v, want %v", job.MissingAudio, tc.wantMissing)
			}
			id, logged := logs.field("the archive has no audio stream although the stream advertised sound for what was recorded; it is kept as it is, flagged on the job", "jobID")
			warned := logged && id == jobCtx.Job.ID
			if warned != tc.wantMissing {
				t.Errorf("Warn logged %v, want %v", warned, tc.wantMissing)
			}
		})
	}
}

// TestSplitFinalizeFlagsAPartMissingAdvertisedAudio is the missing-audio
// guard through the real split finalize: a two-part archive one of whose
// parts has no audio stream although its master advertised sound (the
// record DownloadHls left in that part's staging dir) finishes Finished,
// flagged missing_audio, with a Warn naming the job and the part. A part
// that is genuinely video-only (nothing advertised in its dir) is not
// flagged, nor are parts that have their audio, and a flag an earlier
// finalize set is cleared.
//
// Mutants: the split finalize writing false (the flagged row reads false);
// the guard reading the whole job's staging root instead of each part's dir
// (part 2's record is never seen); the guard ignoring what was advertised
// (the video-only source is flagged).
func TestSplitFinalizeFlagsAPartMissingAdvertisedAudio(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	for _, tc := range []struct {
		name        string
		silentPart  int  // 0: none, else the part (1 or 2) recorded video-only
		advertised  bool // the silent part's master advertised sound
		wantMissing bool
		wantPart    int
	}{
		{name: "part 2 silent, advertised", silentPart: 2, advertised: true, wantMissing: true, wantPart: 2},
		{name: "part 1 silent, advertised", silentPart: 1, advertised: true, wantMissing: true, wantPart: 1},
		{name: "part 2 genuinely video-only", silentPart: 2},
		{name: "both parts have audio"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, jobCtx := muxTestOrchestrator(t, notificationtest.New(), 0)
			o.muxer = engine.NewMuxer(ffmpegPath, discardLogger{})
			logs := &captureLogger{}
			o.logger = logs
			o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{"missing_audio": !tc.wantMissing})
			jobCtx.Config = &JobConfig{}
			jobCtx.StagingDir = t.TempDir()
			jobCtx.OutputDir = t.TempDir()
			jobCtx.Filename = "a-job"
			var segments []database.Segment
			for i := range 2 {
				part := i + 1
				dir := jobCtx.StagingDir
				if i > 0 {
					dir = filepath.Join(jobCtx.StagingDir, fmt.Sprintf("seg_%d", i))
					if err := os.MkdirAll(dir, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				silent := part == tc.silentPart
				noteAdvertisedAudio(&DownloadResult{VideoPath: filepath.Join(dir, "video.ts"), AdvertisedAudio: !silent || tc.advertised})
				out := filepath.Join(jobCtx.OutputDir, fmt.Sprintf("a-job - part%d.ts", part))
				if silent {
					writeOffsetTSFixture(t, ffmpegPath, out, 2, 0)
				} else {
					writeAVTSFixture(t, ffmpegPath, out, 2)
				}
				segments = append(segments, database.Segment{SegmentIndex: i, Filename: filepath.Base(out), FilePath: out, DurationSeconds: 2})
			}
			if err := o.finalizeMultiSegmentJob(context.Background(), jobCtx, segments, nil); err != nil {
				t.Fatalf("finalizeMultiSegmentJob: %v", err)
			}
			job, _ := o.db.GetJob(jobCtx.Job.ID)
			if job.Status != database.StatusFinished || job.Error != "" {
				t.Fatalf("status %s error %q, want an error-free Finished", job.Status, job.Error)
			}
			if job.MissingAudio != tc.wantMissing {
				t.Errorf("missing_audio = %v, want %v", job.MissingAudio, tc.wantMissing)
			}
			part, logged := logs.field("a part of the archive has no audio stream although the stream advertised sound for what was recorded; it is kept as it is, flagged on the job", "part")
			if logged != tc.wantMissing || (logged && part != tc.wantPart) {
				t.Errorf("Warn logged %v naming part %v, want %v naming part %d", logged, part, tc.wantMissing, tc.wantPart)
			}
		})
	}
}

// writeAVTSFixture writes an MPEG-TS of seconds with an h264 and an aac stream.
func writeAVTSFixture(t *testing.T, ffmpegPath, path string, seconds int) {
	t.Helper()
	cmd := exec.Command(ffmpegPath, "-nostdin", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=64x64:rate=5:duration=%d", seconds),
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:duration=%d", seconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac",
		"-f", "mpegts", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate A+V mpegts fixture %s: %v\n%s", path, err, out)
	}
}
