package twitch

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// entryLogger records level, message AND args. The two lines this file asserts
// on carry a channel login, a path and epoch integers and nothing else, which
// is why it keeps args where chat_reauth_test.go's recordingLogger and
// internal/cookies' capturingLogger deliberately drop them — those wrap paths
// where a credential could reach the args, and this one does not.
type entryLogger struct {
	mu      sync.Mutex
	entries []logEntry
}

type logEntry struct {
	level string
	msg   string
	args  []any
}

func (l *entryLogger) record(level, msg string, args []any) {
	l.mu.Lock()
	l.entries = append(l.entries, logEntry{level: level, msg: msg, args: args})
	l.mu.Unlock()
}

func (l *entryLogger) Debug(msg string, args ...any) { l.record("DEBUG", msg, args) }
func (l *entryLogger) Info(msg string, args ...any)  { l.record("INFO", msg, args) }
func (l *entryLogger) Warn(msg string, args ...any)  { l.record("WARN", msg, args) }
func (l *entryLogger) Error(msg string, args ...any) { l.record("ERROR", msg, args) }

// find returns the first entry logged at exactly `level` with exactly `msg`.
func (l *entryLogger) find(level, msg string) (logEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		if e.level == level && e.msg == msg {
			return e, true
		}
	}
	return logEntry{}, false
}

func (l *entryLogger) messages() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.entries))
	for _, e := range l.entries {
		out = append(out, e.level+" "+e.msg)
	}
	return out
}

// arg reads a structured field out of a "key", value, "key", value list.
func (e logEntry) arg(key string) (any, bool) {
	for i := 0; i+1 < len(e.args); i += 2 {
		if k, ok := e.args[i].(string); ok && k == key {
			return e.args[i+1], true
		}
	}
	return nil, false
}

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
//
// The fixture base is deliberately NOT newTestChatDownloader's StreamStartTime
// (10:00:00Z). addMessage falls back to cd.streamStartMs whenever
// recordingStartMs is 0, so a base equal to the stream start would let the
// FALLBACK produce exactly the offsets this test calls proof of adoption.
func TestStartKeepsResumedPartRecordingBase(t *testing.T) {
	base := time.Date(2026, 6, 11, 10, 7, 0, 0, time.UTC) // ≠ the run's StreamStartTime
	restart := base.Add(2 * time.Hour)
	chatPath := seedResumedPart(t, t.TempDir(), base, true)

	if d := readChatData(t, chatPath); d.RecordingStartTime != base.Format(time.RFC3339) {
		t.Fatalf("seeded part recordingStartTime = %q, want %q",
			d.RecordingStartTime, base.Format(time.RFC3339))
	}

	// The restarted daemon: a new downloader over the same part file, given
	// the restart time as its recording start (orchestrator_twitch.go's
	// SetRecordingStartTime / RollFile both pass time.Now()).
	logger := &entryLogger{}
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "testchan", ChannelDisplay: "TestChan", StreamID: "stream-1",
		OutputPath: chatPath, StreamStartTime: "2026-06-11T10:00:00Z",
	}, logger)
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

	// The Info line is the only trace an operator has that the adoption ran,
	// so its message AND its fields are part of the contract, not decoration:
	// without fileBaseMs and runBaseMs beside each other, a log cannot say
	// which clock the part is on.
	e, ok := logger.find("INFO", "twitch chat: resuming part with its recorded base")
	if !ok {
		t.Fatalf("no adoption log line; got %v", logger.messages())
	}
	for _, want := range []struct {
		key string
		val any
	}{
		{"channel", "testchan"},
		{"path", chatPath},
		{"fileBaseMs", base.UnixMilli()},
		{"runBaseMs", restart.UnixMilli()},
	} {
		got, present := e.arg(want.key)
		if !present {
			t.Errorf("adoption log line has no %q field; args = %v", want.key, e.args)
			continue
		}
		if got != want.val {
			t.Errorf("adoption log %q = %v, want %v", want.key, got, want.val)
		}
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

// productionChatHeader is a FULL header as writeFullChatFileTo emits it: every
// field TwitchChatData declares, in the order it declares them, with the emotes
// block an enriched file carries. Marshalled from the production struct rather
// than hand-written, so a header field added later cannot quietly stop being
// covered by the reader's table below.
func productionChatHeader(t *testing.T, base time.Time) string {
	t.Helper()
	raw, err := json.Marshal(&TwitchChatData{
		Platform:           "twitch",
		ChannelLogin:       "testchan",
		ChannelDisplayName: "TestChan",
		StreamID:           "stream-1",
		StreamStartTime:    "2026-06-11T09:55:00Z",
		RecordingStartTime: base.Format(time.RFC3339),
		DownloadedAt:       "2026-06-11T12:00:00Z",
		MessageCount:       1,
		Emotes: &TwitchEmoteData{
			BTTV:    []EmoteInfo{{ID: "1", Code: "kek", URL: "https://example.invalid/1"}},
			SevenTV: []EmoteInfo{{ID: "2", Code: "pog", URL: "https://example.invalid/2"}},
		},
		Messages: []TwitchChatMessage{{
			ID: "m1", AuthorName: "a", Message: "hello", MessageType: "chat",
			TimestampMs: base.Add(time.Second).UnixMilli(), OffsetMs: 1000,
		}},
	})
	if err != nil {
		t.Fatalf("marshal production header: %v", err)
	}
	return string(raw)
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
		{
			// The shape the reader actually meets on disk. The minimal case
			// above proves the walk; this one proves the walk survives every
			// field in front of the one it wants.
			name:    "full production header",
			content: productionChatHeader(t, base),
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
			got, ok, err := chatFileRecordingBaseMs(path)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("chatFileRecordingBaseMs = (%d, %v), want (%d, %v)", got, ok, tc.want, tc.wantOK)
			}
			if err != nil {
				t.Errorf("err = %v, want nil — the bytes arrived, so nothing here is a READ failure", err)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		got, ok, err := chatFileRecordingBaseMs(filepath.Join(t.TempDir(), "absent.json"))
		if ok || got != 0 {
			t.Errorf("chatFileRecordingBaseMs on a missing file = (%d, %v), want (0, false)", got, ok)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("err = %v, want fs.ErrNotExist — the caller stays SILENT on a fresh part and only logs a real read failure", err)
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
		if got, ok, err := chatFileRecordingBaseMs(path); ok || got != 0 || err != nil {
			t.Errorf("chatFileRecordingBaseMs past the scan limit = (%d, %v, %v), want (0, false, nil)", got, ok, err)
		}
	})
}

// TestPartBaseIsNotAdoptedOntoARolledFile pins the outputPath guard in
// adoptPartRecordingBase.
//
// Start runs on its own goroutine while the video loop is already going, so a
// gap split can call RollFile BETWEEN the header read and the store. Without
// the `cd.outputPath == path` condition the closed part's base would land on
// the fresh part, putting every message in the new file hours out of position
// — the exact defect the adoption exists to prevent, in the other direction.
//
// The seam is how the race becomes deterministic: the roll happens inside the
// read, which is precisely the window the guard defends. Mutant: drop
// `cd.outputPath == path &&`.
func TestPartBaseIsNotAdoptedOntoARolledFile(t *testing.T) {
	dir := t.TempDir()
	oldBase := time.Date(2026, 6, 11, 10, 7, 0, 0, time.UTC)
	newBase := oldBase.Add(3 * time.Hour)
	oldPath := seedResumedPart(t, dir, oldBase, true)
	newPath := filepath.Join(dir, "seg_3", "chat.json")

	logger := &entryLogger{}
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "testchan", ChannelDisplay: "TestChan", StreamID: "stream-1",
		OutputPath: oldPath, StreamStartTime: "2026-06-11T10:00:00Z",
	}, logger)
	cd.SetRecordingStartTime(newBase.Format(time.RFC3339))

	realRead := chatFileRecordingBaseMs
	t.Cleanup(func() { chatFileRecordingBaseMs = realRead })
	chatFileRecordingBaseMs = func(path string) (int64, bool, error) {
		got, ok, err := realRead(path)
		// The gap split lands here, exactly as it can in production.
		cd.mu.Lock()
		cd.outputPath = newPath
		cd.mu.Unlock()
		return got, ok, err
	}

	cd.adoptPartRecordingBase()

	if got := cd.recordingStartMs.Load(); got != newBase.UnixMilli() {
		t.Errorf("recordingStartMs = %d, want the ROLLED part's own base %d — the closed part's base was adopted onto the new file",
			got, newBase.UnixMilli())
	}
	if _, ok := logger.find("INFO", "twitch chat: resuming part with its recorded base"); ok {
		t.Errorf("the adoption logged although it was declined; got %v", logger.messages())
	}
}

// TestPartCountersAreNotAdoptedOntoARolledFile is the same guard on the other
// store: adoptExistingPartFile's counters and dedup must not be seeded from the
// closed part's file after a RollFile. Seeding flushedToDisk from a file the
// downloader is no longer writing would make the new part's first flush take
// the APPEND path against a file that does not exist yet.
//
// Mutant: drop `cd.outputPath == path &&` from adoptExistingPartFile.
func TestPartCountersAreNotAdoptedOntoARolledFile(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 6, 11, 10, 7, 0, 0, time.UTC)
	oldPath := seedPartWithoutSidecar(t, dir, base) // three messages, no sidecar
	newPath := filepath.Join(dir, "seg_3", "chat.json")

	logger := &entryLogger{}
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "testchan", ChannelDisplay: "TestChan", StreamID: "stream-1",
		OutputPath: oldPath, StreamStartTime: "2026-06-11T10:00:00Z",
	}, logger)

	realRead := readChatPartFileSummary
	t.Cleanup(func() { readChatPartFileSummary = realRead })
	readChatPartFileSummary = func(path string) (chatPartFileSummary, error) {
		got, err := realRead(path)
		cd.mu.Lock()
		cd.outputPath = newPath
		cd.mu.Unlock()
		return got, err
	}

	if adopted := cd.adoptExistingPartFile(); adopted != 0 {
		t.Errorf("adopted %d messages onto a rolled file, want 0", adopted)
	}
	cd.mu.Lock()
	fileCount, flushed := cd.fileCount, cd.flushedToDisk
	cd.mu.Unlock()
	if fileCount != 0 {
		t.Errorf("fileCount = %d, want 0 — the closed part's count was seeded onto the new file", fileCount)
	}
	if flushed {
		t.Error("flushedToDisk is set for a part with no file yet — its first flush would take the append path against nothing")
	}
	if _, ok := logger.find("INFO", "twitch chat: adopting the existing part file"); ok {
		t.Errorf("the adoption logged although it was declined; got %v", logger.messages())
	}
}

// TestUnreadablePartBaseIsLogged is review F6. When a part file exists but its
// header cannot be READ — a Windows sharing violation, an AV lock, a directory
// in the file's place — the run's restart base stands and every offset in the
// part is shifted by the outage. "No file" and "could not read the file" are
// the same silent return value, so without this line the drift is invisible.
func TestUnreadablePartBaseIsLogged(t *testing.T) {
	dir := t.TempDir()
	// A DIRECTORY where the part file should be: os.Open succeeds on Windows
	// and Linux alike, and the read fails — portable, and it is one of the
	// shapes the adoption's own doc comment names.
	chatPath := filepath.Join(dir, "seg_2", "chat.json")
	if err := os.MkdirAll(chatPath, 0o755); err != nil {
		t.Fatalf("mkdir the blocking directory: %v", err)
	}

	restart := time.Date(2026, 6, 11, 12, 7, 0, 0, time.UTC)
	logger := &entryLogger{}
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "testchan", ChannelDisplay: "TestChan", StreamID: "stream-1",
		OutputPath: chatPath, StreamStartTime: "2026-06-11T10:00:00Z",
	}, logger)
	cd.SetRecordingStartTime(restart.Format(time.RFC3339))

	cd.adoptPartRecordingBase()

	if got := cd.recordingStartMs.Load(); got != restart.UnixMilli() {
		t.Errorf("recordingStartMs = %d, want the run's own start %d", got, restart.UnixMilli())
	}
	e, ok := logger.find("DEBUG", "twitch chat: could not read the part file's recording base; using the run's start")
	if !ok {
		t.Fatalf("an unreadable part base was not diagnosable from the log; got %v", logger.messages())
	}
	if got, present := e.arg("path"); !present || got != chatPath {
		t.Errorf("log line path = %v (present %v), want %q", got, present, chatPath)
	}
	if got, present := e.arg("err"); !present || got == nil {
		t.Errorf("log line carries no err field; args = %v", e.args)
	}
}

// A part file that is simply ABSENT is the ordinary fresh-part case and must
// stay silent — an operator reading a log full of "could not read" lines on
// every new part learns nothing from the one that matters.
func TestAbsentPartBaseIsSilent(t *testing.T) {
	restart := time.Date(2026, 6, 11, 12, 7, 0, 0, time.UTC)
	logger := &entryLogger{}
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "testchan", ChannelDisplay: "TestChan", StreamID: "stream-1",
		OutputPath: filepath.Join(t.TempDir(), "seg_1", "chat.json"), StreamStartTime: "2026-06-11T10:00:00Z",
	}, logger)
	cd.SetRecordingStartTime(restart.Format(time.RFC3339))

	cd.adoptPartRecordingBase()

	if got := logger.messages(); len(got) != 0 {
		t.Errorf("a fresh part logged %v, want nothing", got)
	}
}
