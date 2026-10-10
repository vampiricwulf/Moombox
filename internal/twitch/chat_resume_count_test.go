package twitch

import (
	"os"
	"testing"
	"time"
)

// A crash inside the sidecar's save floor left the restored counts short by
// the messages flushed since its last save: restoreResumeState took them
// straight from the stale sidecar, and the deficit stayed in the part's
// header count and in the job's cumulative chat total for the life of the
// job. The part file's header, refreshed on every flush, makes it up.
//
// Mutant: restoreResumeState ignoring the header — the counts come back 3/3.
func TestARestoreAfterACrashCountsWhatTheSidecarMissed(t *testing.T) {
	cd := newSidecarTestChatDownloader(t)
	cd.delays.resumeSaveFloor = time.Hour
	for i, id := range []string{"m1", "m2", "m3"} {
		cd.addMessage(&TwitchChatMessage{ID: id, TimestampMs: int64(i + 1)})
	}
	cd.flush() // writes the file and the first sidecar (3)
	for i, id := range []string{"m4", "m5"} {
		cd.addMessage(&TwitchChatMessage{ID: id, TimestampMs: int64(i + 4)})
	}
	cd.flush() // appends; the sidecar is inside its floor and still says 3

	// The crash: a fresh downloader on the same part.
	next := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin:   "testchan",
		ChannelDisplay: "TestChan",
		StreamID:       "stream-1",
		OutputPath:     cd.outputPath,
	}, &testLogger{})
	state := next.loadResumeState()
	if state == nil || state.MessageCount != 3 {
		t.Fatalf("the sidecar holds %+v — the test needs one three messages behind", state)
	}
	next.restoreResumeState(state)
	next.mu.Lock()
	file, total := next.fileCount, next.totalCount
	next.mu.Unlock()
	if file != 5 || total != 5 {
		t.Errorf("restored file/total = %d/%d, want 5/5 — the part holds five messages", file, total)
	}

	// And the next flush writes the true count into the header.
	next.addMessage(&TwitchChatMessage{ID: "m6", TimestampMs: 6})
	next.flush()
	if d := readChatData(t, cd.outputPath); d.MessageCount != 6 || len(d.Messages) != 6 {
		t.Errorf("header says %d over %d messages, want 6 over 6", d.MessageCount, len(d.Messages))
	}
}

// The header only ever adds to the sidecar: a header behind it (an old
// unpadded file, a write the header refresh missed) never lowers a count.
func TestAHeaderBehindTheSidecarChangesNothing(t *testing.T) {
	cd := newSidecarTestChatDownloader(t)
	if err := os.WriteFile(cd.outputPath, []byte(`{"messageCount": 2, "messages":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cd.restoreResumeState(&ChatResumeState{MessageCount: 7, TotalCount: 40, RecentIDs: []string{"x"}})
	cd.mu.Lock()
	file, total := cd.fileCount, cd.totalCount
	cd.mu.Unlock()
	if file != 7 || total != 40 {
		t.Errorf("file/total = %d/%d, want the sidecar's 7/40", file, total)
	}
}
