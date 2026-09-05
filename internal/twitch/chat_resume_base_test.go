package twitch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// cancelledContext is an already-cancelled context: Start runs its resume path
// and returns from the top of the reconnect loop without opening a socket, so
// these tests exercise the real entry point with no network.
func cancelledContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// seedResumedPart writes the part file a session that died would have left
// behind: one message, flushed, with `base` as the part's recording start.
// Returns the chat path.
func seedResumedPart(t *testing.T, dir string, base time.Time, header bool) string {
	t.Helper()
	chatPath := filepath.Join(dir, "seg_2", "chat.json")
	seed := newTestChatDownloader(t, chatPath)
	if header {
		seed.SetRecordingStartTime(base.Format(time.RFC3339))
	}
	seed.addMessage(&TwitchChatMessage{
		ID:          "pre-restart",
		AuthorName:  "a",
		Message:     "thirty seconds in",
		TimestampMs: base.Add(30 * time.Second).UnixMilli(),
	})
	seed.flush()
	return chatPath
}

// TestStartKeepsResumedPartRecordingBase is T-F10. A daemon restart mid-part
// builds a fresh downloader over the SAME part file and the orchestrator hands
// it time.Now() as the recording start — right for a part that begins now,
// wrong for one that began hours ago. The resumed part's video is appended to,
// so the part's timeline still starts where it always did; rebasing the chat
// would drop every post-restart message back onto the head of the part, on top
// of the pre-restart chat and hours out of position.
//
// One file, one epoch: the message that arrives five seconds after the restart
// belongs at (restart + 5s − base), not at 5s.
func TestStartKeepsResumedPartRecordingBase(t *testing.T) {
	base := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	restart := base.Add(2 * time.Hour)
	chatPath := seedResumedPart(t, t.TempDir(), base, true)

	if d := readChatData(t, chatPath); d.RecordingStartTime != base.Format(time.RFC3339) {
		t.Fatalf("seeded part recordingStartTime = %q, want %q",
			d.RecordingStartTime, base.Format(time.RFC3339))
	}

	// The restarted daemon: a new downloader over the same part file, given
	// the restart time as its recording start (orchestrator_twitch.go's
	// SetRecordingStartTime / RollFile both pass time.Now()).
	cd := newTestChatDownloader(t, chatPath)
	cd.SetRecordingStartTime(restart.Format(time.RFC3339))

	// A cancelled context makes Start run its resume path and return before
	// the reconnect loop opens a socket — no network in this test.
	if err := cd.Start(cancelledContext(t)); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cd.addMessage(&TwitchChatMessage{
		ID:          "post-restart",
		AuthorName:  "b",
		Message:     "five seconds after the restart",
		TimestampMs: restart.Add(5 * time.Second).UnixMilli(),
	})
	cd.flush()

	d := readChatData(t, chatPath)
	if len(d.Messages) != 2 {
		t.Fatalf("part holds %d messages, want 2 (the resume must append, not rewrite)", len(d.Messages))
	}
	if got := d.Messages[0].OffsetMs; got != 30_000 {
		t.Errorf("pre-restart offset = %d, want 30000", got)
	}
	wantPost := restart.Add(5 * time.Second).Sub(base).Milliseconds()
	if got := d.Messages[1].OffsetMs; got != wantPost {
		t.Errorf("post-restart offset = %d, want %d (5000 means the part was rebased to the restart time)",
			got, wantPost)
	}
	if d.RecordingStartTime != base.Format(time.RFC3339) {
		t.Errorf("recordingStartTime after resume = %q, want the file's own base %q",
			d.RecordingStartTime, base.Format(time.RFC3339))
	}
}

// TestStartWithoutPartRecordingBase pins the other half of the rule: a part
// file that carries no recordingStartTime (written before the field existed,
// or by a run that never had a recording start) offers nothing to adopt, and
// the run's own start time stands. No base is invented.
func TestStartWithoutPartRecordingBase(t *testing.T) {
	base := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	restart := base.Add(2 * time.Hour)
	chatPath := seedResumedPart(t, t.TempDir(), base, false)

	if d := readChatData(t, chatPath); d.RecordingStartTime != "" {
		t.Fatalf("seeded part recordingStartTime = %q, want empty", d.RecordingStartTime)
	}

	cd := newTestChatDownloader(t, chatPath)
	cd.SetRecordingStartTime(restart.Format(time.RFC3339))

	if err := cd.Start(cancelledContext(t)); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cd.addMessage(&TwitchChatMessage{
		ID:          "post-restart",
		AuthorName:  "b",
		Message:     "five seconds after the restart",
		TimestampMs: restart.Add(5 * time.Second).UnixMilli(),
	})
	cd.flush()

	d := readChatData(t, chatPath)
	if len(d.Messages) != 2 {
		t.Fatalf("part holds %d messages, want 2", len(d.Messages))
	}
	if got := d.Messages[1].OffsetMs; got != 5_000 {
		t.Errorf("post-restart offset = %d, want 5000 (the run's own base stands)", got)
	}
}

// TestChatFileRecordingBaseMs covers the header reader directly, including the
// inputs that must return "nothing to adopt" rather than a guess.
func TestChatFileRecordingBaseMs(t *testing.T) {
	base := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		content string
		want    int64
		wantOK  bool
	}{
		{
			name:    "header epoch",
			content: `{"platform":"twitch","recordingStartTime":"2026-06-11T10:00:00Z","messages":[]}`,
			want:    base.UnixMilli(),
			wantOK:  true,
		},
		{"no such field", `{"platform":"twitch","messages":[]}`, 0, false},
		{"messages only", `{"messages":[]}`, 0, false},
		{"unparseable stamp", `{"recordingStartTime":"yesterday","messages":[]}`, 0, false},
		{"empty stamp", `{"recordingStartTime":"","messages":[]}`, 0, false},
		{"not an object", `[1,2,3]`, 0, false},
		{"not json", `chat`, 0, false},
		{"empty file", ``, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "chat.json")
			writeTestFile(t, path, tc.content)
			got, ok := chatFileRecordingBaseMs(path)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("chatFileRecordingBaseMs = (%d, %v), want (%d, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		if got, ok := chatFileRecordingBaseMs(filepath.Join(t.TempDir(), "absent.json")); ok || got != 0 {
			t.Errorf("chatFileRecordingBaseMs on a missing file = (%d, %v), want (0, false)", got, ok)
		}
	})

	// A header longer than the scan limit is not read past: the reader
	// reports nothing rather than pulling a multi-megabyte part into memory.
	t.Run("header past the scan limit", func(t *testing.T) {
		pad := make([]byte, chatHeaderScanLimit)
		for i := range pad {
			pad[i] = 'x'
		}
		path := filepath.Join(t.TempDir(), "chat.json")
		writeTestFile(t, path, `{"channelDisplayName":"`+string(pad)+
			`","recordingStartTime":"2026-06-11T10:00:00Z","messages":[]}`)
		if got, ok := chatFileRecordingBaseMs(path); ok || got != 0 {
			t.Errorf("chatFileRecordingBaseMs past the scan limit = (%d, %v), want (0, false)", got, ok)
		}
	})
}
