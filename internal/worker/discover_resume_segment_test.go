package worker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestDiscoverResumeSegment pins the restart mapping between staging dirs,
// recorded segment rows, and the part a resumed Twitch job continues in.
// The "already muxed" cases are the load-bearing ones: before this mapping
// existed, a restart after a quality split appended new data into the root
// video_stream that was already muxed as part 1 — and finalize silently
// dropped everything captured after the restart.
func TestDiscoverResumeSegment(t *testing.T) {
	newOrch := func(t *testing.T) (*DownloadOrchestrator, *database.Database) {
		t.Helper()
		db, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
		if err != nil {
			t.Fatalf("database.Open: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		o := &DownloadOrchestrator{db: db, logger: discardLogger{}}
		return o, db
	}

	addJob := func(t *testing.T, db *database.Database, id string) {
		t.Helper()
		if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, URL: "https://twitch.tv/x", Platform: "twitch"}); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
	}
	addSegRow := func(t *testing.T, db *database.Database, jobID string, idx int) {
		t.Helper()
		if err := db.AddSegment(&database.Segment{JobID: jobID, SegmentIndex: idx, Quality: "1080p60", Filename: "x.mp4"}); err != nil {
			t.Fatalf("AddSegment: %v", err)
		}
	}
	stageMedia := func(t *testing.T, dir string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "video_stream"), []byte("ts"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	t.Run("fresh job uses root", func(t *testing.T) {
		o, db := newOrch(t)
		addJob(t, db, "j1")
		jobCtx := &JobContext{Job: &database.Job{ID: "j1"}, StagingDir: t.TempDir()}
		idx, dir := o.discoverResumeSegment(jobCtx)
		if idx != 0 || dir != jobCtx.StagingDir {
			t.Errorf("got (%d, %q), want (0, root)", idx, dir)
		}
	})

	t.Run("unmuxed root data resumes part 1", func(t *testing.T) {
		o, db := newOrch(t)
		addJob(t, db, "j1")
		staging := t.TempDir()
		stageMedia(t, staging)
		jobCtx := &JobContext{Job: &database.Job{ID: "j1"}, StagingDir: staging}
		idx, dir := o.discoverResumeSegment(jobCtx)
		if idx != 0 || dir != staging {
			t.Errorf("got (%d, %q), want (0, root)", idx, dir)
		}
	})

	t.Run("unmuxed seg dir resumes that part", func(t *testing.T) {
		o, db := newOrch(t)
		addJob(t, db, "j1")
		staging := t.TempDir()
		stageMedia(t, staging)
		stageMedia(t, filepath.Join(staging, "seg_1"))
		addSegRow(t, db, "j1", 0) // root was muxed as part 1
		jobCtx := &JobContext{Job: &database.Job{ID: "j1"}, StagingDir: staging}
		idx, dir := o.discoverResumeSegment(jobCtx)
		if idx != 1 || dir != filepath.Join(staging, "seg_1") {
			t.Errorf("got (%d, %q), want (1, seg_1)", idx, dir)
		}
	})

	t.Run("muxed root must not be appended into", func(t *testing.T) {
		o, db := newOrch(t)
		addJob(t, db, "j1")
		staging := t.TempDir()
		stageMedia(t, staging)
		addSegRow(t, db, "j1", 0) // root already muxed (e.g. outage muxed part 1)
		jobCtx := &JobContext{Job: &database.Job{ID: "j1"}, StagingDir: staging}
		idx, dir := o.discoverResumeSegment(jobCtx)
		if idx != 1 || dir != filepath.Join(staging, "seg_1") {
			t.Errorf("got (%d, %q), want (1, fresh seg_1)", idx, dir)
		}
	})

	t.Run("muxed seg dir advances to next index", func(t *testing.T) {
		o, db := newOrch(t)
		addJob(t, db, "j1")
		staging := t.TempDir()
		stageMedia(t, staging)
		stageMedia(t, filepath.Join(staging, "seg_1"))
		stageMedia(t, filepath.Join(staging, "seg_2"))
		addSegRow(t, db, "j1", 0)
		addSegRow(t, db, "j1", 1)
		addSegRow(t, db, "j1", 2) // all staged parts recorded
		jobCtx := &JobContext{Job: &database.Job{ID: "j1"}, StagingDir: staging}
		idx, dir := o.discoverResumeSegment(jobCtx)
		if idx != 3 || dir != filepath.Join(staging, "seg_3") {
			t.Errorf("got (%d, %q), want (3, fresh seg_3)", idx, dir)
		}
	})

	t.Run("short-skipped root with seg_0", func(t *testing.T) {
		o, db := newOrch(t)
		addJob(t, db, "j1")
		staging := t.TempDir()
		stageMedia(t, staging)
		stageMedia(t, filepath.Join(staging, "seg_0")) // root was short-skipped; seg_0 is part index 0
		jobCtx := &JobContext{Job: &database.Job{ID: "j1"}, StagingDir: staging}
		idx, dir := o.discoverResumeSegment(jobCtx)
		if idx != 0 || dir != filepath.Join(staging, "seg_0") {
			t.Errorf("got (%d, %q), want (0, seg_0)", idx, dir)
		}
	})
}
