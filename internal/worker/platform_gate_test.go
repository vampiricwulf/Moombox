package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
)

// TestFinalizeMultiSegmentJob_PlatformGate is the coordinator's item-1
// regression test: mergeSameFormatParts (Tier 4) must run for YouTube jobs
// ONLY. Twitch gap-split parts merging would (a) destroy deliberate
// gapless-part semantics when chat is off, (b) replace per-part
// twitch.TwitchChatData files with a YouTube-shaped chat.ChatData husk when
// every part's chat happens to be empty, and (c) pay a full throwaway video
// concat before the chat merge fails when chat is on. The gate lives at the
// finalizeMultiSegmentJob call site (jobCtx.Job.Platform == "youtube"), so
// this drives that real function end to end with a spy substituted for
// probeStreamParamsFn: for a Twitch job, the spy must never fire (merge
// never even attempted); for a YouTube job with the exact same shape, the
// spy DOES fire, proving the assertion isn't vacuous (e.g. from segments
// being too short to be mergeable at all).
func TestFinalizeMultiSegmentJob_PlatformGate(t *testing.T) {
	run := func(t *testing.T, platform string) (probeCalls int) {
		t.Helper()

		orig := probeStreamParamsFn
		defer func() { probeStreamParamsFn = orig }()
		probeStreamParamsFn = func(_ context.Context, _, _ string) (*streamParams, error) {
			probeCalls++
			return &streamParams{VCodec: "h264", Width: 1920, Height: 1080, ACodec: "aac"}, nil
		}

		dir := t.TempDir()
		db, err := database.Open(filepath.Join(dir, "test.db"))
		if err != nil {
			t.Fatalf("database.Open: %v", err)
		}
		t.Cleanup(func() { db.Close() })

		jobID := "gatejob_" + platform
		job := &database.Job{ID: jobID, VideoID: "", URL: "u", Platform: platform, Status: database.StatusMuxing}
		if _, err := db.AddJob(job); err != nil {
			t.Fatalf("AddJob: %v", err)
		}

		outDir := filepath.Join(dir, "out")
		stagingDir := filepath.Join(dir, "staging")
		if err := os.MkdirAll(stagingDir, 0o755); err != nil {
			t.Fatal(err)
		}
		// Pre-place a staging thumbnail so copyAssets short-circuits before
		// any network fetch attempt (VideoID is "" so the YouTube thumbnail
		// URL branch is skipped either way, but this keeps the test robust
		// against that branch's exact condition).
		if err := os.WriteFile(filepath.Join(stagingDir, "thumbnail.jpg"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}

		part1 := filepath.Join(outDir, "base - part1.mp4")
		part2 := filepath.Join(outDir, "base - part2.mp4")
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(part1, []byte("orig1"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(part2, []byte("orig2"), 0o644); err != nil {
			t.Fatal(err)
		}

		segments := []database.Segment{
			{JobID: jobID, SegmentIndex: 0, Filename: "base - part1.mp4", FilePath: part1, DurationSeconds: 100},
			{JobID: jobID, SegmentIndex: 1, Filename: "base - part2.mp4", FilePath: part2, DurationSeconds: 100},
		}

		o := &DownloadOrchestrator{
			muxer:  engine.NewMuxer("ffmpeg", &discardLogger{}),
			db:     db,
			logger: &discardLogger{},
		}
		jobCtx := &JobContext{
			Job:        job,
			Filename:   "base.mp4",
			OutputDir:  outDir,
			StagingDir: stagingDir,
		}

		if err := o.finalizeMultiSegmentJob(context.Background(), jobCtx, segments); err != nil {
			t.Fatalf("finalizeMultiSegmentJob: %v", err)
		}
		return probeCalls
	}

	t.Run("twitch: merge skipped, probe never called", func(t *testing.T) {
		if calls := run(t, "twitch"); calls != 0 {
			t.Errorf("probeStreamParamsFn called %d times for a Twitch job, want 0 (Tier 4 must be YouTube-only)", calls)
		}
	})

	t.Run("youtube: merge attempted, probe called (control -- proves the gate, not segment shape, decides)", func(t *testing.T) {
		if calls := run(t, "youtube"); calls == 0 {
			t.Errorf("probeStreamParamsFn called 0 times for a YouTube job, want >0 (Tier 4 should have attempted the merge)")
		}
	})
}
