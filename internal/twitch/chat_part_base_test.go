package twitch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// partBaseFixture: the local clock at the part's start (the provisional base
// the orchestrator hands over) is 30 s LATER than the part's first frame,
// which is what the video's first segment reports — the downloader joined
// the playlist window 30 s behind the live edge.
var (
	pdtFirstFrame   = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	pdtProvisional  = pdtFirstFrame.Add(30 * time.Second)
	pdtHeldMsgTime  = pdtFirstFrame.Add(40 * time.Second)
	pdtLaterMsgTime = pdtFirstFrame.Add(50 * time.Second)
)

func pdtMessage(id string, at time.Time) *TwitchChatMessage {
	return &TwitchChatMessage{ID: id, AuthorName: "a", Message: id, TimestampMs: at.UnixMilli()}
}

// TestPartBaseSettlesOnTheFirstSegmentsTime is D-T8's chat half. A part
// waiting for its video's first segment holds its messages — the flush writes
// nothing — and when the video reports that segment's program date-time the
// held messages are rebased onto it, the ones still to come are offset from
// it, and the header the first write puts on disk carries it: one file, one
// epoch, on the video's own clock instead of the local clock at the roll.
//
// Mutants: holdForPartBaseLocked never holding (the first flush writes the
// provisional base into the header, and the held message keeps a 10 s
// offset); SettlePartBase not rebasing the held messages (the held one reads
// 10 s, 30 s early); SettlePartBase not storing the base (both read 30 s
// early and the header names the local clock).
func TestPartBaseSettlesOnTheFirstSegmentsTime(t *testing.T) {
	chatPath := filepath.Join(t.TempDir(), "chat.json")
	cd := newTestChatDownloader(t, chatPath)
	cd.SetRecordingStartTime(pdtProvisional.Format(time.RFC3339))
	cd.AwaitPartBase()

	cd.addMessage(pdtMessage("held", pdtHeldMsgTime))
	if err := cd.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if _, err := os.Stat(chatPath); !os.IsNotExist(err) {
		t.Fatalf("the part was written while its base was still provisional (stat err %v)", err)
	}

	cd.SettlePartBase(chatPath, pdtFirstFrame)
	cd.addMessage(pdtMessage("later", pdtLaterMsgTime))
	if err := cd.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	d := readChatData(t, chatPath)
	if d.RecordingStartTime != pdtFirstFrame.Format(time.RFC3339) {
		t.Errorf("header recordingStartTime = %q, want the first segment's %q",
			d.RecordingStartTime, pdtFirstFrame.Format(time.RFC3339))
	}
	if len(d.Messages) != 2 {
		t.Fatalf("part holds %d messages, want 2", len(d.Messages))
	}
	for i, want := range []int64{40_000, 50_000} {
		if got := d.Messages[i].OffsetMs; got != want {
			t.Errorf("message %q offset = %d, want %d — offsets count from the part's first frame",
				d.Messages[i].ID, got, want)
		}
	}
}

// TestPartBaseKeepsOneEpochPerFile: a report that is not for the part waiting
// changes nothing — one for another path (a downloader since replaced), one
// after a zero time ended the wait and the part's file went to disk, one for a
// resumed part whose file was there before it waited — and a zero time (no
// PDT in the playlist) releases the wait on the provisional base, which is
// today's behaviour.
//
// Mutants: SettlePartBase ignoring the path check (the stale report moves the
// base); ignoring the flushedToDisk check (the adopted part's new message is
// offset from the report, under a header that names the file's base); the
// zero time stored as a base (offsets count from the Unix epoch).
func TestPartBaseKeepsOneEpochPerFile(t *testing.T) {
	dir := t.TempDir()

	// A stale report for another part, then no PDT at all.
	chatPath := filepath.Join(dir, "a", "chat.json")
	cd := newTestChatDownloader(t, chatPath)
	cd.SetRecordingStartTime(pdtProvisional.Format(time.RFC3339))
	cd.AwaitPartBase()
	cd.addMessage(pdtMessage("m1", pdtHeldMsgTime))
	cd.SettlePartBase(filepath.Join(dir, "other", "chat.json"), pdtFirstFrame)
	cd.SettlePartBase(chatPath, time.Time{})
	if err := cd.flush(); err != nil {
		t.Fatal(err)
	}
	d := readChatData(t, chatPath)
	if d.RecordingStartTime != pdtProvisional.Format(time.RFC3339) || d.Messages[0].OffsetMs != 10_000 {
		t.Errorf("after a stale report and a PDT-less one: header %q, offset %d; want the provisional base %q and 10000",
			d.RecordingStartTime, d.Messages[0].OffsetMs, pdtProvisional.Format(time.RFC3339))
	}

	// A report after the file is on disk.
	cd.addMessage(pdtMessage("m2", pdtLaterMsgTime))
	cd.SettlePartBase(chatPath, pdtFirstFrame)
	if err := cd.flush(); err != nil {
		t.Fatal(err)
	}
	d = readChatData(t, chatPath)
	if d.RecordingStartTime != pdtProvisional.Format(time.RFC3339) || d.Messages[1].OffsetMs != 20_000 {
		t.Errorf("a report after the first write moved the part: header %q, offset %d; want %q and 20000",
			d.RecordingStartTime, d.Messages[1].OffsetMs, pdtProvisional.Format(time.RFC3339))
	}

	// A part told to wait whose file Start then finds on disk — a chat file
	// beside a video that starts fresh — keeps the file's base whatever the
	// video reports: the wait was armed, but the file already has an epoch.
	fileBase := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	seeded := seedResumedPart(t, filepath.Join(dir, "resumed"), fileBase, true)
	cd3 := newTestChatDownloader(t, seeded)
	cd3.SetRecordingStartTime(pdtProvisional.Format(time.RFC3339))
	cd3.AwaitPartBase()
	if err := cd3.Start(cancelledContext(t)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cd3.SettlePartBase(seeded, pdtFirstFrame)
	cd3.addMessage(pdtMessage("after", pdtLaterMsgTime))
	if err := cd3.flush(); err != nil {
		t.Fatal(err)
	}
	d = readChatData(t, seeded)
	want := pdtLaterMsgTime.Sub(fileBase).Milliseconds()
	if d.RecordingStartTime != fileBase.Format(time.RFC3339) || len(d.Messages) != 2 || d.Messages[1].OffsetMs != want {
		t.Errorf("an adopted part was rebased: header %q, messages %+v; want the file's base %q and the new message at %d",
			d.RecordingStartTime, d.Messages, fileBase.Format(time.RFC3339), want)
	}
}

// TestPartBaseWaitIsBounded: a part whose video never reports a first segment
// must still reach disk. Past ircPartBaseWait its messages go out on the
// provisional base, and the final flush on Start's exit never waits at all. A
// report that does come later still rebases the file; see
// TestLatePartBaseRebasesTheWrittenPart.
//
// Mutants: holdForPartBaseLocked ignoring the wait's bound (the part never
// reaches disk); ignoring final (Start's exit leaves the held messages in
// memory and the part file never appears).
func TestPartBaseWaitIsBounded(t *testing.T) {
	dir := t.TempDir()

	bounded := filepath.Join(dir, "bounded", "chat.json")
	cd := newTestChatDownloader(t, bounded)
	cd.delays.partBaseWait = 20 * time.Millisecond
	cd.SetRecordingStartTime(pdtProvisional.Format(time.RFC3339))
	cd.AwaitPartBase()
	cd.addMessage(pdtMessage("m", pdtHeldMsgTime))
	time.Sleep(40 * time.Millisecond)
	if err := cd.flush(); err != nil {
		t.Fatal(err)
	}
	if d := readChatData(t, bounded); d.RecordingStartTime != pdtProvisional.Format(time.RFC3339) || len(d.Messages) != 1 {
		t.Errorf("after the wait ran out: header %q with %d messages, want the provisional base and 1",
			d.RecordingStartTime, len(d.Messages))
	}

	final := filepath.Join(dir, "final", "chat.json")
	cd2 := newTestChatDownloader(t, final)
	cd2.SetRecordingStartTime(pdtProvisional.Format(time.RFC3339))
	cd2.AwaitPartBase()
	cd2.addMessage(pdtMessage("m", pdtHeldMsgTime))
	if err := cd2.flushFinal(); err != nil {
		t.Fatal(err)
	}
	if d := readChatData(t, final); len(d.Messages) != 1 {
		t.Errorf("the final flush wrote %d messages, want 1", len(d.Messages))
	}
}

// TestRollFileAwaitingBase: the part a roll opens waits for its own video,
// atomically with the swap; the part it closes is drained on whatever base it
// had, and a report addressed to the closed part cannot reach the new one.
//
// Mutants: rollFile not arming the wait for the new part (its first flush
// writes the provisional base before the report arrives); RollFileAwaitingBase
// passing false to rollFile (the same); SettlePartBase checking neither the
// waiting path nor the current one (the closed part's late report rebases the
// new part).
func TestRollFileAwaitingBase(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "chat.json")
	second := filepath.Join(dir, "seg_1", "chat.json")
	cd := newTestChatDownloader(t, first)
	cd.SetRecordingStartTime(pdtFirstFrame.Format(time.RFC3339))
	cd.addMessage(pdtMessage("p1", pdtHeldMsgTime))
	if err := cd.flush(); err != nil {
		t.Fatal(err)
	}

	rollAt := pdtFirstFrame.Add(time.Hour)
	if closed := cd.RollFileAwaitingBase(second, rollAt.Format(time.RFC3339)); closed != first {
		t.Fatalf("RollFileAwaitingBase closed %q, want %q", closed, first)
	}
	cd.addMessage(pdtMessage("p2", rollAt.Add(10*time.Second)))
	if err := cd.flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(second); !os.IsNotExist(err) {
		t.Fatalf("the new part was written before its video reported (stat err %v)", err)
	}

	cd.SettlePartBase(first, pdtFirstFrame.Add(5*time.Minute)) // the closed part's late report
	secondFrame := rollAt.Add(-20 * time.Second)
	cd.SettlePartBase(second, secondFrame)
	if err := cd.flush(); err != nil {
		t.Fatal(err)
	}
	d := readChatData(t, second)
	if d.RecordingStartTime != secondFrame.Format(time.RFC3339) || len(d.Messages) != 1 || d.Messages[0].OffsetMs != 30_000 {
		t.Errorf("new part: header %q, messages %+v; want base %q and one message at 30000",
			d.RecordingStartTime, d.Messages, secondFrame.Format(time.RFC3339))
	}
	if d := readChatData(t, first); d.RecordingStartTime != pdtFirstFrame.Format(time.RFC3339) {
		t.Errorf("closed part's header = %q, want it untouched", d.RecordingStartTime)
	}
}

// TestPartBaseKeepsItsMilliseconds: a program date-time is not a whole second,
// and the header is what a resumed part adopts its base from
// (chatFileRecordingBaseMs). A header written to the second hands the resumed
// part a base up to 999 ms off the one the file's offsets were computed
// against — two clocks in one file again, just closer together.
//
// Mutant: writeFullChatFileTo formatting the header with time.RFC3339 (the
// .250 is dropped).
func TestPartBaseKeepsItsMilliseconds(t *testing.T) {
	chatPath := filepath.Join(t.TempDir(), "chat.json")
	cd := newTestChatDownloader(t, chatPath)
	cd.SetRecordingStartTime(pdtProvisional.Format(time.RFC3339))
	cd.AwaitPartBase()
	cd.addMessage(pdtMessage("m", pdtHeldMsgTime))
	frame := pdtFirstFrame.Add(250 * time.Millisecond)
	cd.SettlePartBase(chatPath, frame)
	if err := cd.flush(); err != nil {
		t.Fatal(err)
	}
	got, ok, err := chatFileRecordingBaseMs(chatPath)
	if err != nil || !ok || got != frame.UnixMilli() {
		t.Errorf("the header reads back as base %d (ok %v, err %v), want %d", got, ok, err, frame.UnixMilli())
	}
	if d := readChatData(t, chatPath); d.Messages[0].OffsetMs != 39_750 {
		t.Errorf("offset = %d, want 39750", d.Messages[0].OffsetMs)
	}
}

// writeChatFileHook replaces writeChatFile for one test. hook runs in place of
// each full-file write; it returns the real write's result or its own error.
func writeChatFileHook(t *testing.T, hook func(path string, data *TwitchChatData, real func(string, *TwitchChatData) error) error) {
	t.Helper()
	real := writeChatFile
	t.Cleanup(func() { writeChatFile = real })
	writeChatFile = func(path string, data *TwitchChatData) error { return hook(path, data, real) }
}

// pastTheWait leaves a part whose wait ran out: one message ("m1", at
// pdtHeldMsgTime) is on disk against the provisional base, the way the
// periodic flush writes it once ircPartBaseWait has passed.
func pastTheWait(t *testing.T, path string) *ChatDownloader {
	t.Helper()
	cd := newTestChatDownloader(t, path)
	cd.delays.partBaseWait = 20 * time.Millisecond
	cd.SetRecordingStartTime(pdtProvisional.Format(time.RFC3339))
	cd.AwaitPartBase()
	cd.addMessage(pdtMessage("m1", pdtHeldMsgTime))
	time.Sleep(40 * time.Millisecond)
	if err := cd.flush(); err != nil {
		t.Fatal(err)
	}
	if d := readChatData(t, path); d.RecordingStartTime != pdtProvisional.Format(time.RFC3339) || len(d.Messages) != 1 {
		t.Fatalf("precondition: header %q with %d messages, want the provisional base and 1",
			d.RecordingStartTime, len(d.Messages))
	}
	return cd
}

// TestLatePartBaseRebasesTheWrittenPart is the review finding on D-T8: a part
// whose video reports its first segment after ircPartBaseWait — an ad break
// the engine skips at the part's start — has a program date-time all the
// same, and D-T8 keeps the local clock only for a playlist with none. The wait
// gave up and the file went to disk on the provisional base, so the report is
// applied by rewriting the file whole: the header and every offset move to
// the first segment's time in one write, so the file never holds two epochs.
// The report itself only records the base; the flush does the write.
//
// A message lands while the rewrite is in flight (m3): it was offset against
// the provisional base, and must move with the rest.
//
// Mutants: holdForPartBaseLocked not marking the part (baseLatePath) when it
// gives up, SettlePartBase's late arm not recording lateBaseMs, or flushLocked
// not calling rebaseLatePartLocked (each: the header keeps the provisional
// base, m1 reads 10000); the messages that arrived during the write not
// rebased (m3 reads 30000); recordingStartMs not stored after the write (m4
// reads 40000).
func TestLatePartBaseRebasesTheWrittenPart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.json")
	cd := pastTheWait(t, path)
	cd.addMessage(pdtMessage("m2", pdtLaterMsgTime)) // pending, on the provisional base

	cd.SettlePartBase(path, pdtFirstFrame) // the first CONTENT segment, past the bound
	if d := readChatData(t, path); d.RecordingStartTime != pdtProvisional.Format(time.RFC3339) {
		t.Fatalf("SettlePartBase wrote the file (header %q) — it runs on the download goroutine", d.RecordingStartTime)
	}

	landed := false
	writeChatFileHook(t, func(path string, data *TwitchChatData, real func(string, *TwitchChatData) error) error {
		if !landed {
			landed = true
			cd.addMessage(pdtMessage("m3", pdtFirstFrame.Add(60*time.Second)))
		}
		return real(path, data)
	})
	if err := cd.flush(); err != nil {
		t.Fatal(err)
	}
	if err := cd.flush(); err != nil { // m3, which arrived during the rewrite
		t.Fatal(err)
	}
	cd.addMessage(pdtMessage("m4", pdtFirstFrame.Add(70*time.Second)))
	if err := cd.flush(); err != nil {
		t.Fatal(err)
	}

	d := readChatData(t, path)
	if d.RecordingStartTime != pdtFirstFrame.Format(time.RFC3339) {
		t.Errorf("header recordingStartTime = %q, want the first segment's %q — a reported PDT is the part's base, however late",
			d.RecordingStartTime, pdtFirstFrame.Format(time.RFC3339))
	}
	want := []int64{40_000, 50_000, 60_000, 70_000}
	if len(d.Messages) != len(want) || d.MessageCount != len(want) {
		t.Fatalf("part holds %d messages (header count %d), want %d", len(d.Messages), d.MessageCount, len(want))
	}
	for i, w := range want {
		if got := d.Messages[i].OffsetMs; got != w {
			t.Errorf("message %q offset = %d, want %d — offsets count from the part's first frame",
				d.Messages[i].ID, got, w)
		}
	}
}

// TestLatePartBaseWriteFailureKeepsOneEpoch: a rewrite that fails must leave
// the file and the in-memory base agreeing on the provisional base — the
// pending batch is then appended on that base, as it would have been — and
// the next flush tries again.
//
// Mutants: rebaseLatePartLocked going on to the swap after a failed write
// (m2 is appended under the provisional header at 50000); lateBaseMs cleared
// before the write instead of after it (the retry never happens and the
// header keeps the provisional base).
func TestLatePartBaseWriteFailureKeepsOneEpoch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.json")
	cd := pastTheWait(t, path)
	cd.addMessage(pdtMessage("m2", pdtLaterMsgTime))
	cd.SettlePartBase(path, pdtFirstFrame)

	fail := true
	writeChatFileHook(t, func(path string, data *TwitchChatData, real func(string, *TwitchChatData) error) error {
		if fail {
			return os.ErrPermission
		}
		return real(path, data)
	})
	if err := cd.flush(); err != nil {
		t.Fatal(err)
	}
	d := readChatData(t, path)
	if d.RecordingStartTime != pdtProvisional.Format(time.RFC3339) || len(d.Messages) != 2 || d.Messages[1].OffsetMs != 20_000 {
		t.Fatalf("after the failed rewrite: header %q, messages %+v; want the provisional base and m2 at 20000",
			d.RecordingStartTime, d.Messages)
	}

	fail = false
	if err := cd.flush(); err != nil {
		t.Fatal(err)
	}
	d = readChatData(t, path)
	if d.RecordingStartTime != pdtFirstFrame.Format(time.RFC3339) || len(d.Messages) != 2 ||
		d.Messages[0].OffsetMs != 40_000 || d.Messages[1].OffsetMs != 50_000 {
		t.Errorf("after the retry: header %q, messages %+v; want %q with offsets 40000 and 50000",
			d.RecordingStartTime, d.Messages, pdtFirstFrame.Format(time.RFC3339))
	}
}

// TestRollFileAppliesALatePartBase: a late report still pending when the part
// closes is written into the closed file before its boundary drain, so the
// part leaves on its first segment's time; one whose rewrite fails dies with
// the part instead of reaching the next one.
//
// Mutants: rollFile not calling rebaseLatePartLocked (the closed header keeps
// the provisional base); rollFile not clearing lateBaseMs (the next part's
// first flush rewrites it onto the closed part's PDT).
func TestRollFileAppliesALatePartBase(t *testing.T) {
	dir := t.TempDir()

	t.Run("applied", func(t *testing.T) {
		first := filepath.Join(dir, "a", "chat.json")
		cd := pastTheWait(t, first)
		cd.addMessage(pdtMessage("m2", pdtLaterMsgTime))
		cd.SettlePartBase(first, pdtFirstFrame)
		if closed := cd.RollFileAwaitingBase(filepath.Join(dir, "a", "seg_1", "chat.json"),
			pdtFirstFrame.Add(time.Hour).Format(time.RFC3339)); closed != first {
			t.Fatalf("RollFileAwaitingBase closed %q, want %q", closed, first)
		}
		d := readChatData(t, first)
		if d.RecordingStartTime != pdtFirstFrame.Format(time.RFC3339) || len(d.Messages) != 2 ||
			d.Messages[0].OffsetMs != 40_000 || d.Messages[1].OffsetMs != 50_000 {
			t.Errorf("closed part: header %q, messages %+v; want %q with offsets 40000 and 50000",
				d.RecordingStartTime, d.Messages, pdtFirstFrame.Format(time.RFC3339))
		}
	})

	t.Run("unwritten", func(t *testing.T) {
		first := filepath.Join(dir, "b", "chat.json")
		second := filepath.Join(dir, "b", "seg_1", "chat.json")
		cd := pastTheWait(t, first)
		cd.SettlePartBase(first, pdtFirstFrame)
		fail := true
		writeChatFileHook(t, func(path string, data *TwitchChatData, real func(string, *TwitchChatData) error) error {
			if fail {
				return os.ErrPermission
			}
			return real(path, data)
		})
		nextBase := pdtFirstFrame.Add(time.Hour)
		cd.RollFile(second, nextBase.Format(time.RFC3339))
		fail = false
		// Two flushes: the first creates the next part's file, and only a
		// file on disk can be rewritten onto a stale base.
		for i, id := range []string{"n1", "n2"} {
			cd.addMessage(pdtMessage(id, nextBase.Add(time.Duration(5*(i+1))*time.Second)))
			if err := cd.flush(); err != nil {
				t.Fatal(err)
			}
		}
		if d := readChatData(t, first); d.RecordingStartTime != pdtProvisional.Format(time.RFC3339) {
			t.Errorf("closed part's header = %q, want the provisional base it was written on", d.RecordingStartTime)
		}
		d := readChatData(t, second)
		if d.RecordingStartTime != nextBase.Format(time.RFC3339) || len(d.Messages) != 2 ||
			d.Messages[0].OffsetMs != 5_000 || d.Messages[1].OffsetMs != 10_000 {
			t.Errorf("next part: header %q, messages %+v; want its own base %q with offsets 5000 and 10000 — "+
				"the closed part's late base reached it", d.RecordingStartTime, d.Messages, nextBase.Format(time.RFC3339))
		}
	})
}

// TestLatePartBaseAfterTheFinalFlush: the final flush at the chat's exit writes
// a waiting part on the provisional base, but the part stays open to its
// report — the orchestrator rolls parts whether or not the chat runs, and a
// roll writes a pending late base into the part it closes.
//
// Mutant: holdForPartBaseLocked marking the part (baseLatePath) on the
// periodic flush only (the header keeps the provisional base).
func TestLatePartBaseAfterTheFinalFlush(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "chat.json")
	cd := newTestChatDownloader(t, first)
	cd.SetRecordingStartTime(pdtProvisional.Format(time.RFC3339))
	cd.AwaitPartBase()
	cd.addMessage(pdtMessage("m1", pdtHeldMsgTime))
	if err := cd.flushFinal(); err != nil {
		t.Fatal(err)
	}
	cd.SettlePartBase(first, pdtFirstFrame)
	cd.RollFile(filepath.Join(dir, "seg_1", "chat.json"), pdtFirstFrame.Add(time.Hour).Format(time.RFC3339))
	if d := readChatData(t, first); d.RecordingStartTime != pdtFirstFrame.Format(time.RFC3339) || d.Messages[0].OffsetMs != 40_000 {
		t.Errorf("header %q, offset %d; want %q and 40000",
			d.RecordingStartTime, d.Messages[0].OffsetMs, pdtFirstFrame.Format(time.RFC3339))
	}
}
