package worker

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
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

// A directory in the part chat's place is no chat at all — the finalize's
// muxUnrecordedSegments reads it that way (fileExists) and records the part
// without it. The part mux used to try to copy it, fail the part, and leave
// the same outcome to that retry one wasted mux later.
//
// Mutant: drop the !fileExists arm of muxSegment's chat pre-check.
func TestPartChatDirectoryIsNoChat(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)

	staging, _ := muxFixtureJob(t, w, db, "j-part-chatdir")
	media := filepath.Join(staging, "video.mp4")
	writeMuxFixture(t, ffmpegPath, media, 5)
	chatDir := filepath.Join(staging, "chat.json")
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatal(err)
	}

	job, _ := db.GetJob("j-part-chatdir")
	seg, err := w.orchestrator.muxSegment(context.Background(), w.buildJobContext(job), 0, 0, time.Now().Unix(),
		QualityInfo{Label: "720p"}, &DownloadResult{HasVideo: true, VideoPath: media, ChatPath: chatDir})
	if err != nil {
		t.Fatalf("muxSegment with a directory for chat: %v, want the part recorded without chat", err)
	}
	if seg == nil || seg.ChatFile != "" {
		t.Errorf("segment = %+v, want one with no chat file", seg)
	}
}

// A part chat that exists but cannot be opened fails the part BEFORE ffmpeg
// runs: discovering it at the copy, after the mux, threw the whole part's mux
// away, again on the finalize's retry while the file stayed locked. A unix
// socket stands in for the locked file — os.Open refuses it for root too —
// and the muxer has no ffmpeg, so a mux that ran first fails with its own
// error instead.
//
// Mutant: drop the os.Open pre-check, or move it after MuxCopy.
func TestUnreadablePartChatFailsBeforeTheMux(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket fixture")
	}
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	w.orchestrator.SetFfmpegPath(filepath.Join(t.TempDir(), "no-ffmpeg"))

	staging, _ := muxFixtureJob(t, w, db, "j-part-chatlock")
	media := filepath.Join(staging, "video.mp4")
	if err := os.WriteFile(media, []byte("not really a video"), 0o644); err != nil {
		t.Fatal(err)
	}
	sockDir, err := os.MkdirTemp("", "ck")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "chat.json")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix socket: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	job, _ := db.GetJob("j-part-chatlock")
	seg, err := w.orchestrator.muxSegment(context.Background(), w.buildJobContext(job), 0, 0, time.Now().Unix(),
		QualityInfo{Label: "720p"}, &DownloadResult{HasVideo: true, VideoPath: media, ChatPath: sock})
	if seg != nil {
		t.Fatalf("an unreadable part chat recorded segment %+v", seg)
	}
	var pe *fs.PathError
	if !errors.As(err, &pe) || pe.Path != sock {
		t.Errorf("muxSegment err = %v, want the chat's open failure before any mux", err)
	}
}
