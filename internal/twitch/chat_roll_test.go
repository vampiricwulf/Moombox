package twitch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newTestChatDownloader(t *testing.T, outputPath string) *ChatDownloader {
	t.Helper()
	return NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin:    "testchan",
		ChannelDisplay:  "TestChan",
		StreamID:        "stream-1",
		OutputPath:      outputPath,
		StreamStartTime: "2026-06-11T10:00:00Z",
	}, &testLogger{})
}

func readChatData(t *testing.T, path string) *TwitchChatData {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read chat file %s: %v", path, err)
	}
	var data TwitchChatData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("parse chat file %s: %v", path, err)
	}
	return &data
}

// TestRollFile pins the part-boundary contract: the closing file keeps its
// messages and per-file header count, the new file starts fresh with a
// rebased recording start, and dedup + the cumulative total survive the roll.
func TestRollFile(t *testing.T) {
	tmp := t.TempDir()
	part0 := filepath.Join(tmp, "chat.json")
	part1 := filepath.Join(tmp, "seg_1", "chat.json")

	cd := newTestChatDownloader(t, part0)
	cd.SetRecordingStartTime("2026-06-11T10:00:00Z")

	cd.addMessage(&TwitchChatMessage{ID: "m1", AuthorName: "a", Message: "one", TimestampMs: 1})
	cd.addMessage(&TwitchChatMessage{ID: "m2", AuthorName: "b", Message: "two", TimestampMs: 2})
	cd.flush()

	if _, err := os.Stat(part0 + ".resume.json"); err != nil {
		t.Fatalf("resume state missing after flush: %v", err)
	}

	closed := cd.RollFile(part1, "2026-06-11T11:00:00Z")
	if closed != part0 {
		t.Fatalf("RollFile returned %q, want %q", closed, part0)
	}
	if _, err := os.Stat(part0 + ".resume.json"); !os.IsNotExist(err) {
		t.Errorf("closed part's resume state should be removed, stat err = %v", err)
	}

	// Duplicate of part-0 message must still dedup across the boundary.
	cd.addMessage(&TwitchChatMessage{ID: "m1", AuthorName: "a", Message: "one", TimestampMs: 1})
	cd.addMessage(&TwitchChatMessage{ID: "m3", AuthorName: "c", Message: "three", TimestampMs: 3})
	cd.flush()

	d0 := readChatData(t, part0)
	if d0.MessageCount != 2 || len(d0.Messages) != 2 {
		t.Errorf("part0: header=%d len=%d, want 2/2", d0.MessageCount, len(d0.Messages))
	}
	if d0.RecordingStartTime != "2026-06-11T10:00:00Z" {
		t.Errorf("part0 recordingStartTime = %q", d0.RecordingStartTime)
	}

	d1 := readChatData(t, part1)
	if d1.MessageCount != 1 || len(d1.Messages) != 1 {
		t.Errorf("part1: header=%d len=%d, want 1/1 (dedup must drop m1)", d1.MessageCount, len(d1.Messages))
	}
	if len(d1.Messages) == 1 && d1.Messages[0].ID != "m3" {
		t.Errorf("part1 message = %q, want m3", d1.Messages[0].ID)
	}
	if d1.RecordingStartTime != "2026-06-11T11:00:00Z" {
		t.Errorf("part1 recordingStartTime = %q, want rebased start", d1.RecordingStartTime)
	}

	if got := cd.MessageCount(); got != 3 {
		t.Errorf("cumulative MessageCount = %d, want 3", got)
	}

	// Resume state for the new part: per-file count + cumulative total.
	cd.saveResumeState()
	store, err := os.ReadFile(part1 + ".resume.json")
	if err != nil {
		t.Fatalf("read part1 resume state: %v", err)
	}
	var state ChatResumeState
	if err := json.Unmarshal(store, &state); err != nil {
		t.Fatalf("parse resume state: %v", err)
	}
	if state.MessageCount != 1 || state.TotalCount != 3 {
		t.Errorf("resume state file/total = %d/%d, want 1/3", state.MessageCount, state.TotalCount)
	}
}

// TestRollFileNoMessages: rolling before anything was written returns ""
// (no part file to finalize) and still redirects future output.
func TestRollFileNoMessages(t *testing.T) {
	tmp := t.TempDir()
	part0 := filepath.Join(tmp, "chat.json")
	part1 := filepath.Join(tmp, "chat1.json")

	cd := newTestChatDownloader(t, part0)
	if closed := cd.RollFile(part1, "2026-06-11T11:00:00Z"); closed != "" {
		t.Fatalf("RollFile with no messages returned %q, want empty", closed)
	}
	if _, err := os.Stat(part0); !os.IsNotExist(err) {
		t.Errorf("no part0 file should exist, stat err = %v", err)
	}

	cd.addMessage(&TwitchChatMessage{ID: "m1", AuthorName: "a", Message: "one", TimestampMs: 1})
	cd.flush()
	if d := readChatData(t, part1); d.MessageCount != 1 {
		t.Errorf("post-roll message went to wrong file, part1 header = %d", d.MessageCount)
	}
}

// TestRestoreResumeState covers both state formats — pre-part-split states
// (no TotalCount) treat MessageCount as the cumulative total; new states
// carry the two counters separately — and both file conditions: with the
// chat file present the per-file counters continue, without it they restart
// so the first flush creates the file fresh (marking a missing file as
// flushed used to wedge every flush on the append path forever).
func TestRestoreResumeState(t *testing.T) {
	cases := []struct {
		name        string
		state       ChatResumeState
		fileOnDisk  bool
		wantFile    int
		wantTotal   int
		wantFlushed bool
	}{
		{"legacy single-file state", ChatResumeState{MessageCount: 42, RecentIDs: []string{"x"}}, true, 42, 42, true},
		{"part-aware state", ChatResumeState{MessageCount: 5, TotalCount: 120, RecentIDs: []string{"x"}}, true, 5, 120, true},
		{"state without its chat file", ChatResumeState{MessageCount: 5, TotalCount: 120, RecentIDs: []string{"x"}}, false, 0, 120, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chatPath := filepath.Join(t.TempDir(), "chat.json")
			if tc.fileOnDisk {
				if err := os.WriteFile(chatPath, []byte(`{"messages":[]}`), 0o644); err != nil {
					t.Fatalf("seed chat file: %v", err)
				}
			}
			cd := newTestChatDownloader(t, chatPath)
			cd.restoreResumeState(&tc.state)
			cd.mu.Lock()
			gotFile, gotTotal, flushed := cd.fileCount, cd.totalCount, cd.flushedToDisk
			cd.mu.Unlock()
			if gotFile != tc.wantFile || gotTotal != tc.wantTotal {
				t.Errorf("file/total = %d/%d, want %d/%d", gotFile, gotTotal, tc.wantFile, tc.wantTotal)
			}
			if flushed != tc.wantFlushed {
				t.Errorf("flushedToDisk = %v, want %v", flushed, tc.wantFlushed)
			}
			if cd.dedup.Len() != 1 {
				t.Errorf("dedup len = %d, want 1", cd.dedup.Len())
			}
		})
	}
}
