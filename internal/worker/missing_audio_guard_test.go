package worker

import (
	"context"
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
