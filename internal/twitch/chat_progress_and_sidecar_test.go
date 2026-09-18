package twitch

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// newSidecarTestChatDownloader is a downloader wired to a temp part file, with
// no network and no credentials — everything these tests drive is in-process.
func newSidecarTestChatDownloader(t *testing.T) *ChatDownloader {
	t.Helper()
	return NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin:   "testchan",
		ChannelDisplay: "TestChan",
		StreamID:       "stream-1",
		OutputPath:     filepath.Join(t.TempDir(), "chat.json"),
	}, &testLogger{})
}

// TestIRCProgressCallbackRunsWithTheChatLockReleased is TWITCH-5 (report row
// #29). addMessage's progress callback reaches ProgressTracker -> UpdateJobFields
// under the database's FULL sync, up to ~60x/s on a busy channel; holding cd.mu
// across it queues the flusher tick, RollFile and MessageCount() behind an
// fsync. The YouTube twin (internal/chat/downloader.go) releases first.
//
// TryLock rather than a nested Lock on purpose: sync.Mutex is not reentrant, so
// a nested Lock under the old code would DEADLOCK the test rather than fail it,
// and a hung test is not a red.
//
// Mutant: restoring `defer cd.mu.Unlock()` in addMessage — TryLock then fails
// and sawLockFree stays false.
func TestIRCProgressCallbackRunsWithTheChatLockReleased(t *testing.T) {
	cd := newSidecarTestChatDownloader(t)
	var sawLockFree atomic.Bool
	var gotCount atomic.Int64
	cd.SetOnProgress(func(count int) {
		gotCount.Store(int64(count))
		if cd.mu.TryLock() {
			sawLockFree.Store(true)
			cd.mu.Unlock()
		}
	})

	cd.addMessage(&TwitchChatMessage{ID: "m1", TimestampMs: 1})

	if got := gotCount.Load(); got != 1 {
		t.Fatalf("the progress callback saw count %d, want 1 — without the callback firing "+
			"this test is vacuous", got)
	}
	if !sawLockFree.Load() {
		t.Error("the progress callback ran while cd.mu was still held: every progress DB " +
			"write blocks the flusher, RollFile and MessageCount()")
	}
}

// TestIRCResumeSidecarObeysTheFloor pins the owner's "IRC sidecar" ruling: a
// 5 s floor on the resume-sidecar save, the VOD path's shape. Today a ~39 KB
// marshal + fsync + rename follows EVERY flush — up to once a second while
// chat is pending, beside the chat.json append fsync.
//
// Driven through the throttle's return value so nothing depends on wall-clock
// timing: a floor of an hour can never elapse inside a test, and a floor of 0
// disables the throttle entirely.
//
// Mutants: flushLocked calling saveResumeState directly (the second call
// returns true); a floor that also swallows the FIRST save (the first call
// returns false).
func TestIRCResumeSidecarObeysTheFloor(t *testing.T) {
	cd := newSidecarTestChatDownloader(t)
	cd.delays.resumeSaveFloor = time.Hour

	if !cd.saveResumeStateThrottled() {
		t.Fatal("the first sidecar save was skipped — nothing has been written yet, so there " +
			"is no floor to be inside of")
	}
	if cd.saveResumeStateThrottled() {
		t.Error("a second sidecar save inside the floor wrote anyway — the floor is the whole ruling")
	}

	cd.delays.resumeSaveFloor = 0
	if !cd.saveResumeStateThrottled() {
		t.Error("a zero floor must disable the throttle (the shape tests use to drive the old cadence)")
	}
}

// TestIRCFlushDoesNotRewriteTheSidecarInsideTheFloor is the wiring half: the
// PERIODIC flush must go through the throttle. The sidecar is deleted after the
// first flush; if the second flush writes one, the throttle is not wired in.
//
// Mutant: flushLocked keeping its direct cd.saveResumeState() call.
func TestIRCFlushDoesNotRewriteTheSidecarInsideTheFloor(t *testing.T) {
	cd := newSidecarTestChatDownloader(t)
	cd.delays.resumeSaveFloor = time.Hour
	sidecar := chatResumePath(cd.outputPath)

	cd.addMessage(&TwitchChatMessage{ID: "m1", TimestampMs: 1})
	cd.flush()
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("the first flush wrote no sidecar (%v) — this test cannot say anything without one", err)
	}
	if err := os.Remove(sidecar); err != nil {
		t.Fatalf("remove sidecar: %v", err)
	}

	cd.addMessage(&TwitchChatMessage{ID: "m2", TimestampMs: 2})
	cd.flush()
	if _, err := os.Stat(sidecar); err == nil {
		t.Error("the second flush rewrote the sidecar inside the floor — every flush still " +
			"pays a marshal, an fsync and a rename")
	}
}

// TestPartRollClearsTheSidecarFloor guards the one place a floor could cost
// data: RollFile redirects recording to a NEW part whose sidecar does not exist
// yet. If the floor carried over from the closed part, the new part's first
// flush would leave it with no sidecar at all, and a crash inside the window
// would resume it from nothing.
//
// Mutant: dropping the `cd.lastResumeSave = time.Time{}` line from RollFile.
func TestPartRollClearsTheSidecarFloor(t *testing.T) {
	cd := newSidecarTestChatDownloader(t)
	cd.delays.resumeSaveFloor = time.Hour

	cd.addMessage(&TwitchChatMessage{ID: "m1", TimestampMs: 1})
	cd.flush()

	next := filepath.Join(t.TempDir(), "chat-part2.json")
	cd.RollFile(next, time.Now().UTC().Format(time.RFC3339))

	cd.addMessage(&TwitchChatMessage{ID: "m2", TimestampMs: 2})
	cd.flush()

	if _, err := os.Stat(chatResumePath(next)); err != nil {
		t.Errorf("the new part has no resume sidecar after its first flush (%v) — the floor "+
			"carried across the part boundary", err)
	}
}
