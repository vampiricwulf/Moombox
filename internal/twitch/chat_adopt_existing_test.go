package twitch

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// seedPartWithoutSidecar writes the part file a session that died would have
// left behind — three flushed messages based at `base` — and then DELETES the
// resume sidecar beside it, which is the case this file is about: a crash
// between the file write and the sidecar write, a sidecar deleted by hand, or
// one that was never written at all.
func seedPartWithoutSidecar(t *testing.T, dir string, base time.Time) string {
	t.Helper()
	chatPath := filepath.Join(dir, "seg_2", "chat.json")
	seed := newTestChatDownloader(t, chatPath)
	seed.SetRecordingStartTime(base.Format(time.RFC3339))
	for i, at := range []time.Duration{10 * time.Second, 20 * time.Second, 30 * time.Second} {
		seed.addMessage(&TwitchChatMessage{
			ID:          []string{"m1", "m2", "m3"}[i],
			AuthorName:  "a",
			Message:     "before the restart",
			TimestampMs: base.Add(at).UnixMilli(),
		})
	}
	seed.flush()

	if err := os.Remove(chatResumePath(chatPath)); err != nil {
		t.Fatalf("remove seeded resume sidecar: %v", err)
	}
	if d := readChatData(t, chatPath); len(d.Messages) != 3 {
		t.Fatalf("seeded part holds %d messages, want 3", len(d.Messages))
	}
	return chatPath
}

// TestStartAdoptsAPartFileWithoutASidecar is the adoption rule: the part FILE
// is the part's history, with or without a resume sidecar beside it.
//
// Without the adoption, a resumed session that finds no usable sidecar leaves
// flushedToDisk false, and the first flush takes writeFullChatFileTo — which
// replaces the part's whole archive with just the new batch. Everything that
// was recorded before the restart is gone.
func TestStartAdoptsAPartFileWithoutASidecar(t *testing.T) {
	base := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	chatPath := seedPartWithoutSidecar(t, t.TempDir(), base)

	// The restarted daemon: a new downloader over the same part file, with no
	// sidecar to restore from.
	cd := newTestChatDownloader(t, chatPath)
	cd.SetRecordingStartTime(base.Format(time.RFC3339))

	// A cancelled context makes Start run its resume path and return before
	// the reconnect loop opens a socket — no network in this test.
	if err := cd.Start(cancelledContext(t)); err != nil {
		t.Fatalf("Start: %v", err)
	}

	for i, at := range []time.Duration{40 * time.Second, 50 * time.Second} {
		cd.addMessage(&TwitchChatMessage{
			ID:          []string{"m4", "m5"}[i],
			AuthorName:  "b",
			Message:     "after the restart",
			TimestampMs: base.Add(at).UnixMilli(),
		})
	}
	cd.flush()

	d := readChatData(t, chatPath)
	if len(d.Messages) != 5 {
		t.Fatalf("part holds %d messages, want 5 (a resume without a sidecar must append, not overwrite)",
			len(d.Messages))
	}
	wantIDs := []string{"m1", "m2", "m3", "m4", "m5"}
	wantOffsets := []int64{10_000, 20_000, 30_000, 40_000, 50_000}
	for i, want := range wantIDs {
		if got := d.Messages[i].ID; got != want {
			t.Errorf("message %d id = %q, want %q", i, got, want)
		}
		if got := d.Messages[i].OffsetMs; got != wantOffsets[i] {
			t.Errorf("message %d offset = %d, want %d", i, got, wantOffsets[i])
		}
	}
	if d.MessageCount != 5 {
		t.Errorf("header messageCount = %d, want 5", d.MessageCount)
	}
	if got := cd.MessageCount(); got != 5 {
		t.Errorf("cd.MessageCount() = %d, want 5 (the adopted history counts)", got)
	}
}

// TestStartAdoptsTheFilesIDsIntoTheDedup pins the second half of the
// adoption: the tail of the file's message IDs seeds the dedup, so an IRC
// reconnect that replays messages already on disk appends them a second time
// no more than a reconnect within one session would.
func TestStartAdoptsTheFilesIDsIntoTheDedup(t *testing.T) {
	base := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	chatPath := seedPartWithoutSidecar(t, t.TempDir(), base)

	cd := newTestChatDownloader(t, chatPath)
	cd.SetRecordingStartTime(base.Format(time.RFC3339))
	if err := cd.Start(cancelledContext(t)); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// m2 and m3 are already in the file; only m4 is new.
	for i, at := range []time.Duration{20 * time.Second, 30 * time.Second, 40 * time.Second} {
		cd.addMessage(&TwitchChatMessage{
			ID:          []string{"m2", "m3", "m4"}[i],
			AuthorName:  "a",
			Message:     "replayed by the reconnect",
			TimestampMs: base.Add(at).UnixMilli(),
		})
	}
	cd.flush()

	d := readChatData(t, chatPath)
	got := make([]string, len(d.Messages))
	for i, m := range d.Messages {
		got[i] = m.ID
	}
	if strings.Join(got, ",") != "m1,m2,m3,m4" {
		t.Errorf("part holds %v, want [m1 m2 m3 m4] (the replay must be deduped against the file)", got)
	}
}

// TestStartPreservesAnUnreadablePartFile pins the other half of the rule: a
// part file this package cannot read is MOVED ASIDE, never written over. The
// bytes are the only copy of something a human could still salvage, and the
// run has to keep archiving either way — so the file is preserved beside
// itself and the part starts fresh.
func TestStartPreservesAnUnreadablePartFile(t *testing.T) {
	base := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	chatPath := filepath.Join(t.TempDir(), "seg_2", "chat.json")
	if err := os.MkdirAll(filepath.Dir(chatPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const garbage = `{"platform":"twitch","messages":[{"id":"m1","mess`
	writeTestFile(t, chatPath, garbage)

	cd := newTestChatDownloader(t, chatPath)
	cd.SetRecordingStartTime(base.Format(time.RFC3339))

	if err := cd.Start(cancelledContext(t)); err != nil {
		t.Fatalf("Start: %v", err)
	}

	preserved, err := os.ReadFile(chatPath + chatCorruptSuffix)
	if err != nil {
		t.Fatalf("unreadable part file was not preserved as %s: %v", chatPath+chatCorruptSuffix, err)
	}
	if string(preserved) != garbage {
		t.Errorf("preserved bytes = %q, want the original file verbatim", string(preserved))
	}

	for i, at := range []time.Duration{40 * time.Second, 50 * time.Second} {
		cd.addMessage(&TwitchChatMessage{
			ID:          []string{"m4", "m5"}[i],
			AuthorName:  "b",
			Message:     "after the restart",
			TimestampMs: base.Add(at).UnixMilli(),
		})
	}
	cd.flush()

	d := readChatData(t, chatPath)
	if len(d.Messages) != 2 {
		t.Fatalf("fresh part holds %d messages, want 2", len(d.Messages))
	}
	if d.Messages[0].ID != "m4" || d.Messages[1].ID != "m5" {
		t.Errorf("fresh part ids = %q/%q, want m4/m5", d.Messages[0].ID, d.Messages[1].ID)
	}
}

// TestReadChatPartFileSummary covers the streaming reader directly, including
// an emote-enriched file (an "emotes" OBJECT sits between the header and the
// messages array) and the malformed inputs that must be reported as errors so
// the caller preserves the file instead of adopting a partial read.
func TestReadChatPartFileSummary(t *testing.T) {
	cases := []struct {
		name      string
		content   string
		wantCount int
		wantIDs   []string
		wantErr   bool
	}{
		{
			name: "header then messages",
			content: `{"platform":"twitch","recordingStartTime":"2026-06-11T10:00:00Z",` +
				`"messageCount":2,"messages":[{"id":"m1","message":"a"},{"id":"m2","message":"b"}]}`,
			wantCount: 2,
			wantIDs:   []string{"m1", "m2"},
		},
		{
			name: "emote enriched",
			content: `{"platform":"twitch","emotes":{"bttv":[{"id":"e1","code":"x","url":"u"}],` +
				`"ffz":null},"messages":[{"id":"m1"}]}`,
			wantCount: 1,
			wantIDs:   []string{"m1"},
		},
		{
			name:      "empty messages array",
			content:   `{"platform":"twitch","messages":[]}`,
			wantCount: 0,
		},
		{
			name:      "null messages",
			content:   `{"platform":"twitch","messages":null}`,
			wantCount: 0,
		},
		{
			name:      "no messages key",
			content:   `{"platform":"twitch","messageCount":0}`,
			wantCount: 0,
		},
		{
			name:      "message without an id",
			content:   `{"messages":[{"message":"a"},{"id":"m2"}]}`,
			wantCount: 2,
			wantIDs:   []string{"m2"},
		},
		{"truncated mid message", `{"messages":[{"id":"m1","mess`, 0, nil, true},
		{"truncated after the array", `{"messages":[{"id":"m1"}]`, 0, nil, true},
		{"trailing garbage", `{"messages":[{"id":"m1"}]} nope`, 0, nil, true},
		{"messages not an array", `{"messages":{"id":"m1"}}`, 0, nil, true},
		{"message not an object", `{"messages":[1,2]}`, 0, nil, true},
		{"not an object", `[1,2,3]`, 0, nil, true},
		{"not json", `chat`, 0, nil, true},
		{"empty file", ``, 0, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "chat.json")
			writeTestFile(t, path, tc.content)
			got, err := readChatPartFileSummary(path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("readChatPartFileSummary err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if got.messages != tc.wantCount {
				t.Errorf("messages = %d, want %d", got.messages, tc.wantCount)
			}
			if strings.Join(got.recentIDs, ",") != strings.Join(tc.wantIDs, ",") {
				t.Errorf("recentIDs = %v, want %v", got.recentIDs, tc.wantIDs)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		_, err := readChatPartFileSummary(filepath.Join(t.TempDir(), "absent.json"))
		if !os.IsNotExist(err) {
			t.Errorf("readChatPartFileSummary on a missing file = %v, want a not-exist error", err)
		}
	})

	// The dedup seed is a TAIL: a marathon part carries far more IDs than the
	// reconnect-replay window the dedup defends, and holding all of them would
	// scale with the file rather than with the window.
	t.Run("only the tail of the ids is kept", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(`{"messages":[`)
		total := chatDedupMax + 500
		for i := range total {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"id":"m`)
			b.WriteString(strconv.Itoa(i))
			b.WriteString(`"}`)
		}
		b.WriteString(`]}`)
		path := filepath.Join(t.TempDir(), "chat.json")
		writeTestFile(t, path, b.String())

		got, err := readChatPartFileSummary(path)
		if err != nil {
			t.Fatalf("readChatPartFileSummary: %v", err)
		}
		if got.messages != total {
			t.Errorf("messages = %d, want %d", got.messages, total)
		}
		if len(got.recentIDs) != chatDedupMax {
			t.Fatalf("recentIDs = %d, want %d", len(got.recentIDs), chatDedupMax)
		}
		if want := "m" + strconv.Itoa(total-1); got.recentIDs[len(got.recentIDs)-1] != want {
			t.Errorf("last retained id = %q, want %q", got.recentIDs[len(got.recentIDs)-1], want)
		}
		if want := "m" + strconv.Itoa(total-chatDedupMax); got.recentIDs[0] != want {
			t.Errorf("first retained id = %q, want %q", got.recentIDs[0], want)
		}
	})
}
