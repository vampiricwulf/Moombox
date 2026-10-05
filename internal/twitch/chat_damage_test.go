package twitch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// damageTestMessage is IRC message i of a test session.
func damageTestMessage(prefix string, i int) *TwitchChatMessage {
	return &TwitchChatMessage{
		ID: fmt.Sprintf("%s%d", prefix, i), TimestampMs: 1767225600000 + int64(i)*1000,
		Message: "hi", MessageType: "chat",
	}
}

// readDamageTestFile parses a part file, failing the test when it does not.
func readDamageTestFile(t *testing.T, path string) TwitchChatData {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d TwitchChatData
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("the part file does not parse: %v", err)
	}
	return d
}

// TestAMidSessionDamagedPartIsSalvagedByTheFlush: when a part goes bad under
// a running session, the append refuses it (ErrChatFileDamaged) and the merge
// fallback used to read it with a full Unmarshal, which fails on exactly that
// file — so the batch was retried every second and never written. The
// fallback now salvages the intact messages, keeps the original as .corrupt,
// and the counters follow the file.
//
// Mutant: go back to readChatFileMessages in the fallback — the flush fails
// and the batch stays pending.
func TestAMidSessionDamagedPartIsSalvagedByTheFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.json")
	cd := newTestChatDownloader(t, path)
	for i := range 5 {
		cd.addMessage(damageTestMessage("old", i))
	}
	if err := cd.flush(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	damaged := append(raw, make([]byte, 600)...) // a crash-torn, zero-filled tail
	if err := os.WriteFile(path, damaged, 0o644); err != nil {
		t.Fatal(err)
	}

	for i := range 3 {
		cd.addMessage(damageTestMessage("new", i))
	}
	if err := cd.flush(); err != nil {
		t.Fatalf("flush over a damaged part: %v", err)
	}
	d := readDamageTestFile(t, path)
	if len(d.Messages) != 8 || d.MessageCount != 8 {
		t.Errorf("part holds %d messages (header %d), want the 5 old and 3 new", len(d.Messages), d.MessageCount)
	}
	if kept, err := os.ReadFile(path + chatCorruptSuffix); err != nil || string(kept) != string(damaged) {
		t.Errorf("the damaged original was not kept as .corrupt (err %v)", err)
	}
	cd.mu.Lock()
	pending, fileCount := len(cd.messages), cd.fileCount
	cd.mu.Unlock()
	if pending != 0 || fileCount != 8 {
		t.Errorf("pending %d, fileCount %d; want 0 and 8", pending, fileCount)
	}
}

// TestAPartialAppendKeepsTheTwitchBatch: an append whose write failed (a full
// disk) puts the file's end back, so the file holds none of the batch — which
// writeBatch dropped while the counters kept it, so the header over-counted
// the array for good. The batch now waits for the next flush.
//
// Mutant: return (count, nil) on ErrChatFilePartialWrite again — m3..m5 never
// reach the file and the header says 9 over an array of 6.
func TestAPartialAppendKeepsTheTwitchBatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.json")
	cd := newTestChatDownloader(t, path)
	for i := range 3 {
		cd.addMessage(damageTestMessage("m", i))
	}
	if err := cd.flush(); err != nil {
		t.Fatal(err)
	}

	real := appendChatMessages
	t.Cleanup(func() { appendChatMessages = real })
	appendChatMessages = func(string, []TwitchChatMessage, int, utils.ChatFileLogger) error {
		return fmt.Errorf("%w: file too large", utils.ErrChatFilePartialWrite)
	}
	for i := 3; i < 6; i++ {
		cd.addMessage(damageTestMessage("m", i))
	}
	if err := cd.flush(); !errors.Is(err, utils.ErrChatFilePartialWrite) {
		t.Fatalf("flush during the failure = %v, want the partial-write error", err)
	}
	appendChatMessages = real
	for i := 6; i < 9; i++ {
		cd.addMessage(damageTestMessage("m", i))
	}
	if err := cd.flush(); err != nil {
		t.Fatal(err)
	}
	d := readDamageTestFile(t, path)
	if len(d.Messages) != 9 || d.MessageCount != 9 {
		t.Errorf("part holds %d messages (header %d), want all 9", len(d.Messages), d.MessageCount)
	}
}

// TestAPartUnreadableAtStartIsNotOverwritten: a part file the adoption could
// not READ at Start (an AV lock, a sharing violation) is left in place, but
// flushedToDisk stayed false, so once the lock cleared the first flush wrote
// the file whole from the new batch: 40 messages replaced by 1. The flush now
// adopts it first, and holds the batch while it still cannot be read.
//
// Mutant: drop the partUnread retry in flushLocked — the part holds 1 message.
func TestAPartUnreadableAtStartIsNotOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.json")
	prev := newTestChatDownloader(t, path)
	for i := range 40 {
		prev.addMessage(damageTestMessage("old", i))
	}
	if err := prev.flush(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(chatResumePath(path)); err != nil { // no usable sidecar
		t.Fatal(err)
	}

	orig := readChatPartFileSummary
	t.Cleanup(func() { readChatPartFileSummary = orig })
	locked := func(string) (chatPartFileSummary, error) {
		return chatPartFileSummary{}, errors.New("read: the file is being used by another process")
	}
	readChatPartFileSummary = locked
	cd := newTestChatDownloader(t, path)
	_ = cd.Start(cancelledContext(t))

	cd.addMessage(damageTestMessage("new", 0))
	if err := cd.flush(); err == nil {
		t.Fatal("a flush while the part is still unreadable must hold the batch")
	}
	if d := readDamageTestFile(t, path); len(d.Messages) != 40 {
		t.Fatalf("the unreadable part was written over: %d messages", len(d.Messages))
	}

	readChatPartFileSummary = orig // the lock clears
	if err := cd.flush(); err != nil {
		t.Fatal(err)
	}
	d := readDamageTestFile(t, path)
	if len(d.Messages) != 41 || d.MessageCount != 41 {
		t.Errorf("part holds %d messages (header %d), want the 40 old and the new one", len(d.Messages), d.MessageCount)
	}
	if got := cd.MessageCount(); got != 41 {
		t.Errorf("MessageCount %d, want 41", got)
	}
}

// TestAPartAdoptedAtItsFirstFlushKeepsItsClock: a part Start could not read
// could not give up its recording base either, so the run kept the restart as
// its base — and the flush that adopted the part once the lock cleared
// appended messages offset against the restart to a file whose header and
// history count from 10:00: a message from 12:10 replayed at 0:10:00 instead
// of 2:10:00. The flush now adopts the base with the file and rebases the
// pending batch onto it.
//
// Mutants: drop adoptPartRecordingBase from adoptUnreadPart, or the rebase of
// the pending batch, or flushLocked's re-snapshot after it — new0 is written
// at 600000.
func TestAPartAdoptedAtItsFirstFlushKeepsItsClock(t *testing.T) {
	at := func(clock string) int64 {
		t.Helper()
		ts, err := time.Parse(time.RFC3339, "2026-06-11T"+clock+"Z")
		if err != nil {
			t.Fatal(err)
		}
		return ts.UnixMilli()
	}
	path := filepath.Join(t.TempDir(), "chat.json")
	seed := newTestChatDownloader(t, path)
	seed.SetRecordingStartTime("2026-06-11T10:00:00Z")
	seed.addMessage(&TwitchChatMessage{ID: "old0", TimestampMs: at("10:30:00"), Message: "hi", MessageType: "chat"})
	if err := seed.flush(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(chatResumePath(path)); err != nil { // no usable sidecar
		t.Fatal(err)
	}

	origSummary, origBase := readChatPartFileSummary, chatFileRecordingBaseMs
	t.Cleanup(func() { readChatPartFileSummary, chatFileRecordingBaseMs = origSummary, origBase })
	lockErr := errors.New("read: the file is being used by another process")
	readChatPartFileSummary = func(string) (chatPartFileSummary, error) { return chatPartFileSummary{}, lockErr }
	chatFileRecordingBaseMs = func(string) (int64, bool, error) { return 0, false, lockErr }

	cd := newTestChatDownloader(t, path)
	cd.SetRecordingStartTime("2026-06-11T12:00:00Z") // the restart, as the orchestrator hands it
	_ = cd.Start(cancelledContext(t))
	cd.addMessage(&TwitchChatMessage{ID: "new0", TimestampMs: at("12:10:00"), Message: "hi", MessageType: "chat"})
	readChatPartFileSummary, chatFileRecordingBaseMs = origSummary, origBase // the lock clears
	if err := cd.flush(); err != nil {
		t.Fatal(err)
	}
	cd.addMessage(&TwitchChatMessage{ID: "new1", TimestampMs: at("12:20:00"), Message: "hi", MessageType: "chat"})
	if err := cd.flush(); err != nil {
		t.Fatal(err)
	}

	d := readDamageTestFile(t, path)
	if d.RecordingStartTime != "2026-06-11T10:00:00Z" {
		t.Fatalf("the part's base became %q, want its own 10:00", d.RecordingStartTime)
	}
	want := map[string]int64{"old0": 1_800_000, "new0": 7_800_000, "new1": 8_400_000}
	if len(d.Messages) != len(want) {
		t.Fatalf("part holds %d messages, want %d", len(d.Messages), len(want))
	}
	for _, m := range d.Messages {
		if m.OffsetMs != want[m.ID] {
			t.Errorf("%s at offset %d, want %d against the part's 10:00 base", m.ID, m.OffsetMs, want[m.ID])
		}
	}
}

// TestAFailedFinalFlushIsReportedNotDropped: at the stream's end the pending
// messages were flushed, and when that failed they were thrown away with the
// sidecar cleared and nil returned — the job read "finished" over a capture
// that had lost them. They are now spilled beside the part, the sidecar is
// kept, and Start reports the failure (chat_status "incomplete").
//
// Mutant: drop the flushErr branch in Start's exit path — Start returns nil
// and nothing is spilled.
func TestAFailedFinalFlushIsReportedNotDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.json")
	// A directory where the part file belongs: it can be neither read nor
	// written over, so every flush fails.
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	cd := newTestChatDownloader(t, path)
	_ = cd.Start(cancelledContext(t))
	for i := range 3 {
		cd.addMessage(damageTestMessage("m", i))
	}
	cd.mu.Lock()
	cd.streamEnded = true
	cd.mu.Unlock()

	err := cd.Start(cancelledContext(t))
	if err == nil || !strings.Contains(err.Error(), "final flush failed") {
		t.Fatalf("Start at the stream's end = %v, want the failed final flush", err)
	}
	raw, rerr := os.ReadFile(path + ".lostbatch.json")
	if rerr != nil {
		t.Fatalf("the unwritten messages were not spilled: %v", rerr)
	}
	var spilled []TwitchChatMessage
	if err := json.Unmarshal(raw, &spilled); err != nil || len(spilled) != 3 {
		t.Errorf("spilled %d messages (err %v), want 3", len(spilled), err)
	}
	if _, err := os.Stat(chatResumePath(path)); err != nil {
		t.Errorf("the sidecar was not kept: %v", err)
	}
}
