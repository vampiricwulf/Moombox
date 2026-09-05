package twitch

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
				// Every case in this table is bad BYTES, so every one of them
				// must be reportable as such: the caller only preserves a
				// file as .corrupt on this sentinel.
				if !errors.Is(err, errChatPartMalformed) {
					t.Errorf("err = %v, want it to carry errChatPartMalformed", err)
				}
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

	// A directory where the part file should be: os.Open SUCCEEDS on both
	// Windows and Linux and the READ then fails (ERROR_INVALID_FUNCTION /
	// EISDIR), so this is the portable "the bytes never arrived" case. It must
	// NOT be reported as malformed — the caller preserves a malformed file by
	// RENAMING it, and renaming a directory aside is not a recovery.
	t.Run("a directory in the file's place is not malformed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "chat.json")
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		_, err := readChatPartFileSummary(path)
		if err == nil {
			t.Fatal("readChatPartFileSummary on a directory returned no error")
		}
		if errors.Is(err, errChatPartMalformed) {
			t.Errorf("err = %v, want it NOT to carry errChatPartMalformed", err)
		}
	})

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

// allLevelLogger captures log lines at every level so a test can assert that a
// code path did NOT run. Both branches that consume a part file's bytes log
// before they return, so their silence is proof the file was never read.
// Distinct from chat_irc_fallback_test.go's recordingLogger, which records
// Warn only and says in its own doc why it stays that way.
type allLevelLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *allLevelLogger) record(level, msg string) {
	l.mu.Lock()
	l.lines = append(l.lines, level+" "+msg)
	l.mu.Unlock()
}

func (l *allLevelLogger) Debug(msg string, args ...any) { l.record("DEBUG", msg) }
func (l *allLevelLogger) Info(msg string, args ...any)  { l.record("INFO", msg) }
func (l *allLevelLogger) Warn(msg string, args ...any)  { l.record("WARN", msg) }
func (l *allLevelLogger) Error(msg string, args ...any) { l.record("ERROR", msg) }

func (l *allLevelLogger) sawContaining(needle string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

// TestStartLeavesAnUnreadablePartFileWhenTheSidecarRestoredIt is the gate: the
// adoption is for the case where NO sidecar restored the part, and it must not
// run — must not even READ the file — when one did.
//
// The sidecar sets flushedToDisk from a file that merely stats, so if the
// adoption still ran here its corrupt branch would rename that file aside
// while the flag stayed true: every later flush would then take the append
// path against a path with no file, fail, fail again through the merge
// fallback, and keep the batch pending forever — the whole broadcast buffered
// in memory with nothing on disk.
//
// What this test does NOT do is repair that input. A part file that has become
// unreadable while its sidecar still points at it loses the session's chat
// today and lost it before the adoption existed too (the append finds no
// closing bracket and the merge fallback cannot parse the file, so flushLocked
// retries forever). That is PRE-EXISTING behaviour, pinned here as it stands:
// the file is left exactly where it is, holding exactly its own bytes.
func TestStartLeavesAnUnreadablePartFileWhenTheSidecarRestoredIt(t *testing.T) {
	base := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	chatPath := filepath.Join(t.TempDir(), "seg_2", "chat.json")
	seed := newTestChatDownloader(t, chatPath)
	seed.SetRecordingStartTime(base.Format(time.RFC3339))
	seed.addMessage(&TwitchChatMessage{
		ID: "m1", AuthorName: "a", Message: "before", TimestampMs: base.Add(10 * time.Second).UnixMilli(),
	})
	seed.flush()
	// The sidecar survives; the file it points at goes bad under it.
	if _, err := os.Stat(chatResumePath(chatPath)); err != nil {
		t.Fatalf("seeded sidecar missing: %v", err)
	}
	const garbage = `{"platform":"twitch","messages":[{"id":"m1","mess`
	writeTestFile(t, chatPath, garbage)

	logger := &allLevelLogger{}
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "testchan", ChannelDisplay: "TestChan", StreamID: "stream-1",
		OutputPath: chatPath, StreamStartTime: "2026-06-11T10:00:00Z",
	}, logger)
	cd.SetRecordingStartTime(base.Format(time.RFC3339))
	if err := cd.Start(cancelledContext(t)); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !logger.sawContaining("Resuming from saved state") {
		t.Fatalf("the sidecar did not restore the part; the test input is wrong")
	}
	if _, err := os.Stat(chatPath + chatCorruptSuffix); err == nil {
		t.Errorf("the part file was renamed to %s even though the sidecar restored it", chatPath+chatCorruptSuffix)
	}
	onDisk, err := os.ReadFile(chatPath)
	if err != nil {
		t.Fatalf("the part file is gone from its path: %v", err)
	}
	if string(onDisk) != garbage {
		t.Errorf("part file = %q, want it left exactly as it was", string(onDisk))
	}

	// The O(file) pass must not have run at all: for THIS input the reader
	// cannot return without the caller logging one of its two failure lines.
	if logger.sawContaining("unreadable") || logger.sawContaining("cannot open") ||
		logger.sawContaining("adopting the existing part file") {
		t.Errorf("the part file was read even though the sidecar restored it: %v", logger.lines)
	}

	// And the pre-existing outcome, pinned as it stands rather than repaired:
	// flushedToDisk is true from the sidecar, so the flush takes the append
	// path, AppendChatMessages finds no closing bracket in the garbage, the
	// merge fallback cannot parse it either, and flushLocked keeps the batch
	// pending for the next attempt. The session's chat does not reach disk —
	// which is exactly what this input did before the adoption existed.
	for i, at := range []time.Duration{40 * time.Second, 50 * time.Second} {
		cd.addMessage(&TwitchChatMessage{
			ID:          []string{"m4", "m5"}[i],
			AuthorName:  "b",
			Message:     "after the restart",
			TimestampMs: base.Add(at).UnixMilli(),
		})
	}
	cd.flush()

	after, err := os.ReadFile(chatPath)
	if err != nil || string(after) != garbage {
		t.Errorf("the flush changed the unreadable file (err %v): %q", err, string(after))
	}
	cd.mu.Lock()
	pending := len(cd.messages)
	cd.mu.Unlock()
	if pending != 2 {
		t.Errorf("pending messages after the failed flush = %d, want 2 (the batch is retried, not dropped)", pending)
	}
}

// TestStartLeavesAPartFileItCannotReadAlone pins the other half of Minor 3: a
// file whose BYTES could not be read is not a verdict on its content, so it is
// left exactly where it is. Only a file that read fine and did not parse is
// preserved as .corrupt — and preservation means a RENAME, which against a
// healthy file (an antivirus lock, a Windows sharing violation, a directory in
// the file's place) would move it out from under the archive for no reason.
//
// A directory at the output path is the portable way to make the read fail:
// os.Open succeeds on both Windows and Linux, and the first Read does not —
// which is why the warning says "cannot read", not "cannot open".
func TestStartLeavesAPartFileItCannotReadAlone(t *testing.T) {
	base := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	chatPath := filepath.Join(t.TempDir(), "seg_2", "chat.json")
	if err := os.MkdirAll(chatPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	logger := &allLevelLogger{}
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "testchan", ChannelDisplay: "TestChan", StreamID: "stream-1",
		OutputPath: chatPath, StreamStartTime: "2026-06-11T10:00:00Z",
	}, logger)
	cd.SetRecordingStartTime(base.Format(time.RFC3339))
	if err := cd.Start(cancelledContext(t)); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := os.Stat(chatPath + chatCorruptSuffix); err == nil {
		t.Errorf("an unreadable-but-not-malformed path was moved to %s", chatPath+chatCorruptSuffix)
	}
	info, err := os.Stat(chatPath)
	if err != nil || !info.IsDir() {
		t.Errorf("the path was not left alone: stat err = %v", err)
	}
	if !logger.sawContaining("cannot read the existing part file") {
		t.Errorf("no warning for the part file that could not be read: %v", logger.lines)
	}
	if logger.sawContaining("unreadable") {
		t.Errorf("an open failure was reported as corruption: %v", logger.lines)
	}
}
