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
// after the part's file is on disk, one after the wait has been given up — and
// a zero time (no PDT in the playlist) releases the wait on the provisional
// base, which is today's behaviour.
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
// must still reach disk. Past ircPartBaseWait the provisional base stands, and
// the final flush on Start's exit never waits at all.
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
