package twitch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// warnHookLogger calls hook with every Warn message, so a test can act at the
// moment a code path announces itself.
type warnHookLogger struct {
	testLogger
	hook func(msg string)
}

func (l *warnHookLogger) Warn(msg string, args ...any) {
	if l.hook != nil {
		l.hook(msg)
	}
}

// TestARepairOfAResumedPartAndARollDoNotCross: repairDamagedPart read the
// part's count, salvaged it without flushMu and then added kept-before to
// whichever part was current — so a gap split's RollFile landing inside the
// repair charged the closed part's -2 to the NEW part, whose header then read
// -1 over an array of 1. The repair now holds flushMu, so the roll waits for
// it and closes the repaired part.
//
// The roll is fired from the repair's own Warn and given 200 ms to finish
// before the repair goes on: unguarded it finishes at once, guarded it cannot
// start until the repair is done.
//
// Mutant: drop the flushMu.Lock in repairDamagedPart — the new part's
// fileCount is -2.
func TestARepairOfAResumedPartAndARollDoNotCross(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.json")
	seed := newTestChatDownloader(t, path)
	for i := range 5 {
		seed.addMessage(damageTestMessage("m", i))
	}
	if err := seed.flush(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cut := strings.Index(string(raw), `"m3"`)
	if cut < 0 {
		t.Fatalf("no m3 in %s", raw)
	}
	if err := os.WriteFile(path, raw[:cut+2], 0o644); err != nil { // torn inside m3
		t.Fatal(err)
	}
	next := rollTestNextPart(t, path)

	lg := &warnHookLogger{}
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "testchan", ChannelDisplay: "TestChan", StreamID: "stream-1",
		OutputPath: path, StreamStartTime: "2026-06-11T10:00:00Z",
	}, lg)
	rolled := make(chan string, 1)
	var fired atomic.Bool
	lg.hook = func(msg string) {
		if !strings.Contains(msg, "resumed part file is damaged") || fired.Swap(true) {
			return
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("RollFile panicked: %v", r)
				}
			}()
			rolled <- cd.RollFile(next, "2026-06-11T11:00:00Z")
		}()
		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
		}
	}
	_ = cd.Start(cancelledContext(t))
	if !fired.Load() {
		t.Fatal("precondition: the resume did not repair the part")
	}
	if closed := <-rolled; closed != path {
		t.Fatalf("RollFile closed %q, want %q", closed, path)
	}

	cd.mu.Lock()
	fileCount, total := cd.fileCount, cd.totalCount
	cd.mu.Unlock()
	if fileCount != 0 || total != 3 {
		t.Errorf("after the roll fileCount %d, totalCount %d; want 0 for the new part and the 3 the repair kept", fileCount, total)
	}
	if d := readDamageTestFile(t, path); len(d.Messages) != 3 || d.MessageCount != 3 {
		t.Errorf("closed part holds %d messages (header %d), want the 3 intact", len(d.Messages), d.MessageCount)
	}
	cd.addMessage(damageTestMessage("n", 0))
	if err := cd.flush(); err != nil {
		t.Fatal(err)
	}
	if d := readDamageTestFile(t, next); len(d.Messages) != 1 || d.MessageCount != 1 {
		t.Errorf("new part holds %d messages under a header of %d, want 1 over 1", len(d.Messages), d.MessageCount)
	}
}

// startOnUnreadPart leaves a 40-message part with no sidecar, and Starts a
// fresh downloader over it while it cannot be read (the summary reader fails
// as a sharing violation does). unlock lets it be read again.
func startOnUnreadPart(t *testing.T) (cd *ChatDownloader, path string, unlock func()) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "chat.json")
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
	readChatPartFileSummary = func(string) (chatPartFileSummary, error) {
		return chatPartFileSummary{}, errors.New("read: the file is being used by another process")
	}
	cd = newTestChatDownloader(t, path)
	_ = cd.Start(cancelledContext(t))
	return cd, path, func() { readChatPartFileSummary = orig }
}

// rollTestNextPart is the next part's chat path in path's staging dir.
func rollTestNextPart(t *testing.T, path string) string {
	t.Helper()
	next := filepath.Join(filepath.Dir(path), "seg_1", "chat.json")
	if err := os.MkdirAll(filepath.Dir(next), 0o755); err != nil {
		t.Fatal(err)
	}
	return next
}

// TestARollOverAnUnreadPartDoesNotWriteOverIt: RollFile ignored partUnread.
// Its drain took the first-write path flushLocked is forbidden for a part
// Start could not read, so a gap split with 5 messages held wrote them over
// the 40 the part already had, and cleared the flag. The roll now adopts the
// part first, and spills the batch beside it while it still cannot be read.
//
// Mutants: drop the `len(batch) > 0 && oldUnread` spill arm in RollFile — the
// part holds 5; drop the adoption before the boundary section — the part
// whose lock cleared holds 40 and the batch is spilled; drop `|| oldUnread`
// from closedPath — a roll with nothing pending reports no closed part.
func TestARollOverAnUnreadPartDoesNotWriteOverIt(t *testing.T) {
	t.Run("still unreadable", func(t *testing.T) {
		cd, path, _ := startOnUnreadPart(t)
		for i := range 5 {
			cd.addMessage(damageTestMessage("new", i))
		}
		if err := cd.flush(); err == nil {
			t.Fatal("precondition: the flush must hold the batch")
		}
		closed := cd.RollFile(rollTestNextPart(t, path), "2026-06-11T11:00:00Z")
		if closed != path {
			t.Errorf("RollFile returned %q, want the closed part %q", closed, path)
		}
		if d := readDamageTestFile(t, path); len(d.Messages) != 40 {
			t.Errorf("the unread part holds %d messages after the roll, want its 40", len(d.Messages))
		}
		raw, err := os.ReadFile(path + ".lostbatch.json")
		if err != nil {
			t.Fatalf("the boundary batch was not spilled: %v", err)
		}
		var spilled []TwitchChatMessage
		if err := json.Unmarshal(raw, &spilled); err != nil || len(spilled) != 5 {
			t.Errorf("spilled %d messages (err %v), want 5", len(spilled), err)
		}
	})
	t.Run("lock cleared", func(t *testing.T) {
		cd, path, unlock := startOnUnreadPart(t)
		for i := range 5 {
			cd.addMessage(damageTestMessage("new", i))
		}
		if err := cd.flush(); err == nil {
			t.Fatal("precondition: the flush must hold the batch")
		}
		unlock()
		if closed := cd.RollFile(rollTestNextPart(t, path), "2026-06-11T11:00:00Z"); closed != path {
			t.Errorf("RollFile returned %q, want the closed part %q", closed, path)
		}
		if d := readDamageTestFile(t, path); len(d.Messages) != 45 || d.MessageCount != 45 {
			t.Errorf("closed part holds %d messages (header %d), want the 40 old and the 5 new", len(d.Messages), d.MessageCount)
		}
		if _, err := os.Stat(path + ".lostbatch.json"); !os.IsNotExist(err) {
			t.Errorf("a batch that reached the part was spilled too (stat err %v)", err)
		}
		if got := cd.MessageCount(); got != 45 {
			t.Errorf("MessageCount %d, want 45", got)
		}
	})
	t.Run("nothing pending", func(t *testing.T) {
		cd, path, _ := startOnUnreadPart(t)
		if closed := cd.RollFile(rollTestNextPart(t, path), "2026-06-11T11:00:00Z"); closed != path {
			t.Errorf("RollFile returned %q, want the closed part %q: it holds the part's history", closed, path)
		}
	})
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

// TestAnExitAfterAFailedFlushSavesTheFilesCount: Start's exit paths save the
// sidecar right after a flush that may have failed, and the sidecar counted
// the batch that flush could not write. The next run restored it as if the
// part held them, and since the part's header may raise a restored count but
// never lower it, the header read 6 over an array of 4 for good. The sidecar
// now counts what the file holds.
//
// The spill is blocked (a directory in its place), so the batch is still
// pending when the exit saves: an interrupted exit that can spill it takes it
// out of the downloader first (spillUnwrittenBatch), and the save would
// then have nothing pending to leave out.
//
// Mutant: save fileCount/totalCount without subtracting the pending messages
// in saveResumeState — the sidecar says 5 and the header 6 over 4.
func TestAnExitAfterAFailedFlushSavesTheFilesCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.json")
	cd := newTestChatDownloader(t, path)
	for i := range 3 {
		cd.addMessage(damageTestMessage("m", i))
	}
	if err := cd.flush(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".lostbatch.json", 0o755); err != nil {
		t.Fatal(err)
	}
	real := appendChatMessages
	t.Cleanup(func() { appendChatMessages = real })
	appendChatMessages = func(string, []TwitchChatMessage, int, utils.ChatFileLogger) error {
		return fmt.Errorf("%w: disk full", utils.ErrChatFilePartialWrite)
	}
	for i := 3; i < 5; i++ {
		cd.addMessage(damageTestMessage("m", i))
	}
	_ = cd.Start(cancelledContext(t)) // an interrupted exit: its flush fails, its sidecar is saved
	appendChatMessages = real

	state := cd.loadResumeState()
	if state == nil {
		t.Fatal("the interrupted exit saved no sidecar")
	}
	if state.MessageCount != 3 || state.TotalCount != 3 {
		t.Errorf("the sidecar counts %d (total %d), want the 3 messages the part holds", state.MessageCount, state.TotalCount)
	}
	next := newTestChatDownloader(t, path)
	_ = next.Start(cancelledContext(t))
	next.addMessage(damageTestMessage("n", 0))
	if err := next.flush(); err != nil {
		t.Fatal(err)
	}
	if d := readDamageTestFile(t, path); len(d.Messages) != 4 || d.MessageCount != 4 {
		t.Errorf("part holds %d messages under a header of %d, want 4 over 4", len(d.Messages), d.MessageCount)
	}
	if got := next.MessageCount(); got != 4 {
		t.Errorf("MessageCount %d, want 4", got)
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

// TestAFailedFinalFlushOnAnInterruptedExitIsSpilledAndReported: Start's
// interrupted exit — Stop(), which is how ExecuteTwitch's outage finalize ends
// chat before it records that exit's verdict — ignored a final flush that
// could not write the pending batch. It saved the sidecar and returned nil
// with the batch in memory alone: the row read chat "finished", nothing wrote
// the batch afterwards, and the staging cleanup deleted the sidecar. The exit
// now spills the batch beside the part, as the stream-end drain does, and
// reports the capture incomplete. The spilled batch leaves the downloader for
// the rollUnwritten count, so a relaunched Start cannot write it to the part a
// second time, and every later end — of that relaunch, or of the run a
// restart resumes from the sidecar — still reports it. A spill that fails
// leaves the batch pending, and the relaunch writes it.
//
// Mutants: drop the spillUnwrittenBatch call in Start's interrupted arm, or
// return nil from it — Start returns nil and nothing is spilled; leave the
// spilled batch in cd.messages — the relaunch writes it to the part too; drop
// the rollUnwritten increment — the relaunch's end and the resumed run's end
// read clean; drop the fileCount/totalCount decrement — MessageCount keeps the
// spilled batch; save the sidecar ahead of the spill — the resumed run's end
// reads clean; take the batch out when the spill itself failed — the
// relaunch has nothing to write.
func TestAFailedFinalFlushOnAnInterruptedExitIsSpilledAndReported(t *testing.T) {
	endStream := func(t *testing.T, cd *ChatDownloader) error {
		t.Helper()
		cd.mu.Lock()
		cd.streamEnded = true
		cd.mu.Unlock()
		return cd.Start(cancelledContext(t))
	}
	wantFlushFailed := func(t *testing.T, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "final flush failed") {
			t.Errorf("the interrupted exit = %v, want the failed final flush reported", err)
		}
	}
	wantSpilled := func(t *testing.T, path, prefix string, n int) {
		t.Helper()
		raw, err := os.ReadFile(path + ".lostbatch.json")
		if err != nil {
			t.Errorf("the unwritten messages were not spilled: %v", err)
			return
		}
		var spilled []TwitchChatMessage
		if err := json.Unmarshal(raw, &spilled); err != nil {
			t.Fatalf("the spill does not parse: %v", err)
		}
		got := 0
		for _, m := range spilled {
			if strings.HasPrefix(m.ID, prefix) {
				got++
			}
		}
		if got != n {
			t.Errorf("the spill holds %d of the %d unwritten messages", got, n)
		}
	}
	// partHolds counts the messages with prefix in the part file.
	partHolds := func(t *testing.T, path, prefix string) int {
		t.Helper()
		n := 0
		for _, m := range readDamageTestFile(t, path).Messages {
			if strings.HasPrefix(m.ID, prefix) {
				n++
			}
		}
		return n
	}
	failAppends := func(t *testing.T) (restore func()) {
		t.Helper()
		real := appendChatMessages
		t.Cleanup(func() { appendChatMessages = real })
		appendChatMessages = func(string, []TwitchChatMessage, int, utils.ChatFileLogger) error {
			return fmt.Errorf("%w: disk full", utils.ErrChatFilePartialWrite)
		}
		return func() { appendChatMessages = real }
	}
	// partWithHistory is a part file holding 3 messages, with its sidecar,
	// and the downloader that wrote them.
	partWithHistory := func(t *testing.T) (*ChatDownloader, string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "chat.json")
		cd := newTestChatDownloader(t, path)
		for i := range 3 {
			cd.addMessage(damageTestMessage("old", i))
		}
		if err := cd.flush(); err != nil {
			t.Fatal(err)
		}
		cd.saveResumeState()
		return cd, path
	}

	// The part file still cannot be read at the exit, so the final flush
	// holds the batch rather than write over the history it cannot see.
	t.Run("unread part", func(t *testing.T) {
		cd, path, unlock := startOnUnreadPart(t)
		before := cd.MessageCount()
		for i := range 5 {
			cd.addMessage(damageTestMessage("tail", i))
		}
		cd.Stop()
		wantFlushFailed(t, cd.Start(cancelledContext(t)))
		wantSpilled(t, path, "tail", 5)
		if got := cd.MessageCount(); got != before {
			t.Errorf("MessageCount %d after the exit spilled 5, want %d", got, before)
		}

		// A relaunch in this process keeps the in-memory state. The part is
		// readable again, so it is adopted; the spilled batch must not follow
		// it in, and the end still reports the batch.
		unlock()
		err := endStream(t, cd)
		if err == nil || !strings.Contains(err.Error(), "5 messages could not be written") {
			t.Errorf("the relaunch's end = %v, want the 5 spilled messages reported", err)
		}
		if n := partHolds(t, path, "tail"); n != 0 {
			t.Errorf("the part holds %d of the spilled messages as well, want 0", n)
		}
	})

	// A full disk: the append puts the file's end back and the batch stays
	// pending. A restart resumes from the sidecar the exit saved.
	t.Run("full disk, then a restart", func(t *testing.T) {
		cd, path := partWithHistory(t)
		restore := failAppends(t)
		for i := range 3 {
			cd.addMessage(damageTestMessage("tail", i))
		}
		cd.Stop()
		wantFlushFailed(t, cd.Start(cancelledContext(t)))
		wantSpilled(t, path, "tail", 3)
		restore()

		resumed := newTestChatDownloader(t, path)
		_ = resumed.Start(cancelledContext(t))
		err := endStream(t, resumed)
		if err == nil || !strings.Contains(err.Error(), "3 messages could not be written") {
			t.Errorf("the resumed run's end = %v, want the 3 spilled messages reported", err)
		}
		if n := partHolds(t, path, "old"); n != 3 {
			t.Errorf("the part holds %d of its 3 messages", n)
		}
	})

	// The spill cannot be written either: the batch stays pending, and the
	// relaunch, with the disk back, writes it to the part.
	t.Run("the spill fails too", func(t *testing.T) {
		cd, path := partWithHistory(t)
		restore := failAppends(t)
		for i := range 3 {
			cd.addMessage(damageTestMessage("tail", i))
		}
		spill := path + ".lostbatch.json"
		if err := os.Mkdir(spill, 0o755); err != nil {
			t.Fatal(err)
		}
		before := cd.MessageCount()
		cd.Stop()
		wantFlushFailed(t, cd.Start(cancelledContext(t)))
		if got := cd.MessageCount(); got != before {
			t.Errorf("MessageCount %d after a spill that failed, want %d: the batch is still pending", got, before)
		}
		restore()
		if err := os.Remove(spill); err != nil {
			t.Fatal(err)
		}

		if err := endStream(t, cd); err != nil {
			t.Errorf("the relaunch's end = %v, want nil: it wrote the batch", err)
		}
		if n := partHolds(t, path, "tail"); n != 3 {
			t.Errorf("the part holds %d of the 3 pending messages after the relaunch, want 3", n)
		}
	})
}

// TestAFailedFinalFlushAtTheStreamsEndOrAPanicIsSpilledLikeAnInterruptedOne:
// the stream-end drain spilled a batch its final flush could not write and
// kept it — pending, and in the part's count and the job total — where the
// interrupted exit's spill takes it out for the rollUnwritten count
// (spillUnwrittenBatch). So MessageCount, which the job's chat count follows,
// counted messages no part file holds; a relaunch in this process wrote the
// spilled batch to the part as well; and the sidecar the drain saved carried
// no count, so the end of a run resumed from it read clean and the staging
// cleanup deleted the spill. A panicking Start whose final flush failed did
// not spill at all: the batch lived in memory alone, and a restart lost it.
// Both arms now spill the way the interrupted exit does, and both still
// report the capture incomplete.
//
// Mutants: put back the stream-end arm's own dump, which keeps the batch —
// MessageCount keeps it, the relaunch writes it to the part and its end reads
// clean, and so does the resumed run's; save the sidecar ahead of the spill
// in either arm — the resumed run's end reads clean; drop the panic arm's
// spill — nothing is spilled, MessageCount keeps the batch, and the resumed
// run's end reads clean.
func TestAFailedFinalFlushAtTheStreamsEndOrAPanicIsSpilledLikeAnInterruptedOne(t *testing.T) {
	endStream := func(t *testing.T, cd *ChatDownloader) error {
		t.Helper()
		cd.mu.Lock()
		cd.streamEnded = true
		cd.mu.Unlock()
		return cd.Start(cancelledContext(t))
	}
	wantUnwritten := func(t *testing.T, what string, err error, n int) {
		t.Helper()
		if want := fmt.Sprintf("%d messages could not be written", n); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s = %v, want the %d spilled messages reported", what, err, n)
		}
	}
	spilled := func(t *testing.T, path string) int {
		t.Helper()
		raw, err := os.ReadFile(path + ".lostbatch.json")
		if err != nil {
			t.Errorf("the unwritten messages were not spilled: %v", err)
			return 0
		}
		var msgs []TwitchChatMessage
		if err := json.Unmarshal(raw, &msgs); err != nil {
			t.Fatalf("the spill does not parse: %v", err)
		}
		return len(msgs)
	}
	partHolds := func(t *testing.T, path, prefix string) int {
		t.Helper()
		n := 0
		for _, m := range readDamageTestFile(t, path).Messages {
			if strings.HasPrefix(m.ID, prefix) {
				n++
			}
		}
		return n
	}
	// partWithFullDisk is a part file holding 3 messages, with its sidecar,
	// and the downloader that wrote them holding 3 more no append can write.
	// restore gives the disk back.
	partWithFullDisk := func(t *testing.T) (cd *ChatDownloader, path string, restore func()) {
		t.Helper()
		path = filepath.Join(t.TempDir(), "chat.json")
		cd = newTestChatDownloader(t, path)
		for i := range 3 {
			cd.addMessage(damageTestMessage("old", i))
		}
		if err := cd.flush(); err != nil {
			t.Fatal(err)
		}
		cd.saveResumeState()
		real := appendChatMessages
		t.Cleanup(func() { appendChatMessages = real })
		appendChatMessages = func(string, []TwitchChatMessage, int, utils.ChatFileLogger) error {
			return fmt.Errorf("%w: disk full", utils.ErrChatFilePartialWrite)
		}
		for i := range 3 {
			cd.addMessage(damageTestMessage("tail", i))
		}
		return cd, path, func() { appendChatMessages = real }
	}
	// resumedEnd is the end of a run a restart resumes from the sidecar.
	resumedEnd := func(t *testing.T, path string) error {
		t.Helper()
		resumed := newTestChatDownloader(t, path)
		_ = resumed.Start(cancelledContext(t))
		return endStream(t, resumed)
	}

	t.Run("stream end, then a relaunch", func(t *testing.T) {
		cd, path, restore := partWithFullDisk(t)
		err := endStream(t, cd)
		if err == nil || !strings.Contains(err.Error(), "final flush failed") {
			t.Errorf("the stream's end = %v, want the failed final flush reported", err)
		}
		if n := spilled(t, path); n != 3 {
			t.Errorf("the spill holds %d messages, want the 3 unwritten", n)
		}
		if got := cd.MessageCount(); got != 3 {
			t.Errorf("MessageCount %d after the end spilled 3, want the 3 the part holds", got)
		}

		restore()
		wantUnwritten(t, "the relaunch's end", endStream(t, cd), 3)
		if n := partHolds(t, path, "tail"); n != 0 {
			t.Errorf("the part holds %d of the spilled messages as well, want 0", n)
		}
	})

	t.Run("stream end, then a restart", func(t *testing.T) {
		cd, path, restore := partWithFullDisk(t)
		_ = endStream(t, cd)
		restore()
		wantUnwritten(t, "the resumed run's end", resumedEnd(t, path), 3)
	})

	t.Run("a panic, then a restart", func(t *testing.T) {
		cd, path, restore := partWithFullDisk(t)
		cd.logger = &panicOnceLogger{} // panics on runIRCSession's first line
		if err := cd.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "panic") {
			t.Fatalf("Start = %v after a recovered panic, want the panic reported", err)
		}
		if n := spilled(t, path); n != 3 {
			t.Errorf("the spill holds %d messages, want the 3 unwritten", n)
		}
		if got := cd.MessageCount(); got != 3 {
			t.Errorf("MessageCount %d after the panic's exit spilled 3, want the 3 the part holds", got)
		}
		restore()
		wantUnwritten(t, "the resumed run's end", resumedEnd(t, path), 3)
	})
}

// A roll with nothing pending drains nothing, so writeBatch's salvage never
// reads the closed part. A roll that landed between Start's resume and its
// repair handed a torn part to the part's mux and enrichment that way. The
// closed part is now salvaged at the roll itself when its end is torn.
//
// Mutant: RollFile without the empty-drain salvage — the closed part ends
// torn and no .corrupt is kept.
func TestARollWithNothingPendingSalvagesATornClosedPart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.json")
	cd := newTestChatDownloader(t, path)
	for i := range 5 {
		cd.addMessage(damageTestMessage("m", i))
	}
	if err := cd.flush(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cut := strings.Index(string(raw), `"m3"`)
	if cut < 0 {
		t.Fatalf("no m3 in %s", raw)
	}
	if err := os.WriteFile(path, raw[:cut+2], 0o644); err != nil { // torn inside m3
		t.Fatal(err)
	}

	closed := cd.RollFile(rollTestNextPart(t, path), "2026-06-11T11:00:00Z")
	if closed != path {
		t.Fatalf("RollFile returned %q, want the closed part", closed)
	}
	if intact, err := utils.ChatFileEndIntact(path); err != nil || !intact {
		t.Errorf("the closed part is still torn (intact=%v, err=%v)", intact, err)
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Errorf("the torn original was not kept beside it: %v", err)
	}
	if got := len(readDamageTestFile(t, path).Messages); got != 3 {
		t.Errorf("the salvaged part holds %d messages, want the 3 intact ones", got)
	}
}

// TestABoundarySpillMakesTheCaptureIncomplete: a roll that could not write its
// boundary batch spilled it beside the closed part and said nothing more, so
// the stream ended "finished" and the staging cleanup deleted the spill with
// the part's dir. The spilled messages now leave the job total (it follows
// what the part files hold) and Start reports the capture incomplete at the
// stream's end — also after a restart, through the sidecar — and on an
// interrupted exit, which is how ExecuteTwitch's outage finalize ends chat.
//
// Mutants: drop either noteRollUnwritten call in RollFile; drop the
// rollUnwritten check in Start's stream-end path, or in its interrupted-exit
// arm; return it from that arm whatever the count; drop RollUnwritten from
// saveResumeState or restoreResumeState; drop the totalCount subtraction.
func TestABoundarySpillMakesTheCaptureIncomplete(t *testing.T) {
	endStream := func(t *testing.T, cd *ChatDownloader) error {
		t.Helper()
		cd.mu.Lock()
		cd.streamEnded = true
		cd.mu.Unlock()
		return cd.Start(cancelledContext(t))
	}
	wantIncomplete := func(t *testing.T, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "part boundary") {
			t.Errorf("Start = %v, want the boundary spill reported", err)
		}
	}

	t.Run("unread part", func(t *testing.T) {
		cd, path, _ := startOnUnreadPart(t)
		for i := range 5 {
			cd.addMessage(damageTestMessage("new", i))
		}
		_ = cd.flush() // held: the part cannot be read
		before := cd.MessageCount()
		next := rollTestNextPart(t, path)
		cd.RollFile(next, "2026-06-11T11:00:00Z")
		if got := cd.MessageCount(); got != before-5 {
			t.Errorf("MessageCount %d after spilling 5, want %d", got, before-5)
		}
		cd.addMessage(damageTestMessage("n", 0))
		if err := cd.flush(); err != nil {
			t.Fatal(err)
		}
		wantIncomplete(t, endStream(t, cd))
	})

	t.Run("drain fails", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "seg_0", "chat.json")
		cd := newTestChatDownloader(t, path)
		_ = cd.Start(cancelledContext(t)) // nothing there yet: not an unread part
		for i := range 3 {
			cd.addMessage(damageTestMessage("m", i))
		}
		// A file where the part's dir belongs: the drain cannot create it.
		if err := os.WriteFile(filepath.Dir(path), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		next := filepath.Join(root, "seg_1", "chat.json")
		if err := os.MkdirAll(filepath.Dir(next), 0o755); err != nil {
			t.Fatal(err)
		}
		cd.RollFile(next, "2026-06-11T11:00:00Z")
		if got := cd.MessageCount(); got != 0 {
			t.Errorf("MessageCount %d after the only 3 were spilled, want 0", got)
		}
		wantIncomplete(t, endStream(t, cd))
	})

	t.Run("across a restart", func(t *testing.T) {
		cd, path, _ := startOnUnreadPart(t)
		for i := range 5 {
			cd.addMessage(damageTestMessage("new", i))
		}
		_ = cd.flush()
		next := rollTestNextPart(t, path)
		cd.RollFile(next, "2026-06-11T11:00:00Z")
		cd.addMessage(damageTestMessage("n", 0))
		if err := cd.flush(); err != nil {
			t.Fatal(err)
		}
		cd.saveResumeState()

		resumed := newTestChatDownloader(t, next)
		_ = resumed.Start(cancelledContext(t))
		wantIncomplete(t, endStream(t, resumed))
	})

	// ExecuteTwitch's outage finalize — the broadcast ended while
	// connectivity was down — ends chat with Stop(), never MarkStreamEnded,
	// and records the verdict of that interrupted exit. It used to return nil
	// before the count was read, so the job read "finished" and the cleanup
	// deleted the spill. The exit keeps the sidecar carrying the count, for
	// the resumed run a shutdown hands it to.
	t.Run("interrupted exit", func(t *testing.T) {
		cd, path, _ := startOnUnreadPart(t)
		for i := range 5 {
			cd.addMessage(damageTestMessage("new", i))
		}
		_ = cd.flush()
		next := rollTestNextPart(t, path)
		cd.RollFile(next, "2026-06-11T11:00:00Z")
		cd.Stop()
		wantIncomplete(t, cd.Start(cancelledContext(t)))
		if _, err := os.Stat(chatResumePath(next)); err != nil {
			t.Errorf("the interrupted exit did not keep the sidecar carrying the count: %v", err)
		}
	})

	t.Run("no spill", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "chat.json")
		cd := newTestChatDownloader(t, path)
		_ = cd.Start(cancelledContext(t))
		cd.addMessage(damageTestMessage("m", 0))
		cd.RollFile(rollTestNextPart(t, path), "2026-06-11T11:00:00Z")
		if err := cd.Start(cancelledContext(t)); err != nil {
			t.Errorf("a clean roll's interrupted exit = %v, want nil", err)
		}
		if err := endStream(t, cd); err != nil {
			t.Errorf("a clean roll's stream end = %v, want nil", err)
		}
	})
}
