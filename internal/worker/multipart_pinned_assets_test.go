package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// muxSegment pins every part of a recording to the first part's directory and
// base, so a mid-job channel rename does not scatter one recording's parts.
// The finalize resolved the job's own assets and columns from the FRESH
// template instead: the description went to the new folder, and chat_filename
// joined the new folder to part 1's chat name — a file nobody wrote. They
// follow the parts now.
//
// Mutant: finalizeMultiSegmentJob ignoring pinnedPartLocation — filename,
// chat_filename and the description all name the new folder.
func TestMultiPartAssetsFollowThePinnedParts(t *testing.T) {
	w, db := testWorkerSetup(t)
	o := w.orchestrator
	job := &database.Job{ID: "tw_pinned", VideoID: "tw_pinned", URL: "https://twitch.tv/streamer",
		Platform: "twitch", Status: database.StatusDownloading, Description: "a stream"}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	oldDir := filepath.Join(root, "OldName")
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var segments []database.Segment
	for i, name := range []string{"Show [x] - part1", "Show [x] - part2"} {
		p := filepath.Join(oldDir, name+".mp4")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		seg := database.Segment{SegmentIndex: i, Filename: name + ".mp4", FilePath: p, Quality: "720p"}
		if i == 0 {
			seg.ChatFile = filepath.Join(oldDir, name+".chat.json")
			if err := os.WriteFile(seg.ChatFile, []byte(`{"messages":[]}`), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		segments = append(segments, seg)
	}
	jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, StagingDir: t.TempDir(),
		OutputDir: root, Filename: filepath.Join("NewName", "Show [x]"), Logger: &discardLogger{}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // no network for the thumbnail fallback inside copyAssets
	if err := o.finalizeMultiSegmentJob(ctx, jobCtx, segments); err != nil {
		t.Fatalf("finalizeMultiSegmentJob: %v", err)
	}
	fresh, _ := db.GetJob(job.ID)
	if want := filepath.Join("OldName", "Show [x]"); fresh.Filename != want {
		t.Errorf("filename = %q, want %q", fresh.Filename, want)
	}
	if want := filepath.Join("OldName", "Show [x] - part1.chat.json"); fresh.ChatFilename != want {
		t.Errorf("chat_filename = %q, want %q (the file part 1's mux wrote)", fresh.ChatFilename, want)
	}
	if want := filepath.Join(oldDir, "Show [x].description"); fresh.DescriptionFile != want || !fileExists(want) {
		t.Errorf("description_file = %q, want %q beside the parts", fresh.DescriptionFile, want)
	}
}
