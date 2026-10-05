package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A chat copy that fails at finalize used to be one Warn: the row went
// Finished with no chat_file and its chat_status unchanged, so the staging
// cleanup saw nothing to keep and deleted the only copy of the chat. The
// archive now reads chat "incomplete", which is what makes the cleanup prune
// staging down to the capture instead of deleting it.
//
// Mutant: copyAssets not writing chat_status on a failed copy.
func TestChatCopyFailureKeepsTheCapture(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)

	staging, outputDir := muxFixtureJob(t, w, db, "j-chat-kept")
	writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), 5)
	chat := filepath.Join(staging, "chat.json")
	if err := os.WriteFile(chat, []byte(`{"videoId":"j-chat-kept","messageCount":1,"messages":[{"id":"m1"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	db.UpdateJobFields("j-chat-kept", map[string]any{"chat_status": "finished", "total_chat_messages": 1})
	job, _ := db.GetJob("j-chat-kept")
	dst := filepath.Join(outputDir, filepath.Base(w.buildJobContext(job).Filename)+".chat.json")
	if err := os.MkdirAll(dst, 0o755); err != nil { // a directory squatting on the chat's name
		t.Fatal(err)
	}

	if err := w.MuxJob("j-chat-kept"); err != nil {
		t.Fatalf("MuxJob: %v", err)
	}
	w.Stop()

	fresh, _ := db.GetJob("j-chat-kept")
	if fresh.ChatFile != "" {
		t.Fatalf("the fixture's copy did not fail: chat_file=%q", fresh.ChatFile)
	}
	if fresh.ChatStatus != chatStatusIncomplete {
		t.Errorf("chat_status = %q after a failed chat copy, want %q", fresh.ChatStatus, chatStatusIncomplete)
	}
	if _, err := os.Stat(chat); err != nil {
		t.Errorf("the only chat capture was deleted with staging: %v", err)
	}
}

// A part whose chat cannot be copied is not recorded: a recorded part's
// seg_N dir is swept with staging, and it held the only copy of that span's
// chat. Unrecorded, it stays unmuxed (shielded, retried by finalize), and its
// part file goes too so the retry writes it fresh.
//
// Mutant: muxSegment recording the part without its chat, as it did.
func TestPartChatCopyFailureLeavesThePartUnmuxed(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)

	staging, outputDir := muxFixtureJob(t, w, db, "j-part-chat")
	media := filepath.Join(staging, "video.mp4")
	writeMuxFixture(t, ffmpegPath, media, 5)
	chat := filepath.Join(staging, "chat.json")
	if err := os.WriteFile(chat, []byte(`{"messages":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	job, _ := db.GetJob("j-part-chat")
	jobCtx := w.buildJobContext(job)
	base, _ := w.orchestrator.resolveFreshFilename(jobCtx)
	partChat := filepath.Join(outputDir, filepath.Dir(base), filepath.Base(base)+" - part1.chat.json")
	if err := os.MkdirAll(partChat, 0o755); err != nil {
		t.Fatal(err)
	}

	seg, err := w.orchestrator.muxSegment(context.Background(), jobCtx, 0, 0, time.Now().Unix(),
		QualityInfo{Label: "720p"}, &DownloadResult{HasVideo: true, VideoPath: media, ChatPath: chat})
	if err == nil || seg != nil {
		t.Fatalf("muxSegment with an uncopyable chat = (%v, %v), want an error and no segment", seg, err)
	}
	if segs, _ := db.GetSegments("j-part-chat"); len(segs) != 0 {
		t.Errorf("the part was recorded without its chat: %+v", segs)
	}
	if left := mp4sIn(t, filepath.Dir(partChat)); len(left) != 0 {
		t.Errorf("the unrecorded part's file was left behind: %v", left)
	}
}
