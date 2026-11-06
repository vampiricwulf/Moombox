package chat

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// orphanReplacement is a replaceChatItemAction as parseMarker reads it: the
// target it names, and its item parsed whole at sent (the epoch is
// 2025-01-01T00:00:00Z).
func orphanReplacement(target, id string, sent time.Duration) ChatMarker {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	text := []MessagePart{{Type: "text", Text: "held for review, then shown"}}
	return ChatMarker{Kind: MarkerReplaced, TargetID: target, Replacement: text, Message: &ChatMessage{
		ID: id, TimestampUsec: strconv.FormatInt(epoch.Add(sent).UnixMicro(), 10),
		AuthorName: "u", AuthorChannelID: "UCu", Message: text,
	}}
}

// TestOrphanReplacementBecomesAMessage is owner decision FE-14 at batch time:
// a replaceChatItemAction whose target was never stored — a placeholder's,
// the item YouTube shows while a message is held for review — put its marker
// on nothing, and the message it carried reached chat.json nowhere (the raw
// sidecar was its only copy). It is now committed as a message of its own,
// with the offset its own timestamp gives against the file's epoch, counted,
// and written with the next flush; a replacement of a message that IS stored
// stays a marker on it and adds nothing. In a replay pass it is committed
// past the high-water mark its own batch just advanced.
//
// Mutants: applyMarkers not returning orphans (the placeholder's message is
// missing); the dedup's completeness not consulted, so a flushed file holds
// every replacement for the end of the run (the count does not move
// mid-run); commitMessages applying the replay mark to orphans (the replay
// case drops it).
func TestOrphanReplacementBecomesAMessage(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID: "vid", OutputFile: out, IsLiveOrUpcoming: true, StreamStartTime: "2025-01-01T00:00:00Z",
	})
	if n, _ := cd.processBatch(&ChatApiResponse{Messages: []ChatMessage{damagedResumeMsg("a", 0), damagedResumeMsg("b", 1)}}); n != 2 {
		t.Fatalf("seed batch committed %d", n)
	}
	cd.writeChatFile() // on disk now: the next markers cannot see a buffer
	if !cd.flushedToDisk || len(cd.messages) != 0 {
		t.Fatalf("seed flush: flushed %v, buffered %d", cd.flushedToDisk, len(cd.messages))
	}

	n, _ := cd.processBatch(&ChatApiResponse{Markers: []ChatMarker{
		orphanReplacement("placeholder-1", "p1", 90*time.Second),
		orphanReplacement("a", "a", 60*time.Second), // a stored message's: a marker
	}})
	if n != 1 || cd.MessageCount() != 3 || len(cd.messages) != 1 {
		t.Fatalf("new %d, count %d, buffered %d; want the placeholder's message alone committed", n, cd.MessageCount(), len(cd.messages))
	}
	if m := cd.messages[0]; m.ID != "p1" || !m.HasOffset || m.OffsetMs != 90_000 || m.Replaced {
		t.Errorf("committed %+v, want p1 at its own offset 90000, a message not a marker", m)
	}
	cd.writeChatFile()
	cd.applyPendingMarkers()
	d := readDamagedResumeFile(t, out)
	got := map[string]ChatMessage{}
	for _, m := range d.Messages {
		got[m.ID] = m
	}
	if len(d.Messages) != 3 || got["p1"].ID == "" || !got["a"].Replaced {
		t.Errorf("file = %+v, want a (replaced), b and p1", d.Messages)
	}

	// A replay pass: the batch's own message advances the high-water mark
	// past the replacement's timestamp.
	rp := NewChatDownloader(ChatDownloaderOptions{VideoID: "vid", StreamStartTime: "2025-01-01T00:00:00Z"})
	rp.replay.Store(true)
	later := damagedResumeMsg("late", 600_000)
	if n, _ := rp.processBatch(&ChatApiResponse{Messages: []ChatMessage{later}, Markers: []ChatMarker{orphanReplacement("placeholder-2", "p2", 30*time.Second)}}); n != 2 {
		t.Errorf("replay batch committed %d, want its message and the orphaned replacement", n)
	}
}

// TestHeldOrphanReplacementIsStoredAtTheEnd is FE-14 where the batch cannot
// tell: the file is past what the dedup remembers (a resumed or long run), so
// the target may be on disk and the replacement is held. The end-of-run pass
// reads the whole file: a held replacement no message on disk is the target
// of is appended as a message of its own, once; one whose target is there is
// applied to it as before.
//
// Mutants: the first pass stopping at its first match (m2's deletion, so m5
// is never seen and its replacement is appended as an orphan); the orphans
// not committed (the file keeps 7); every held replacement treated as
// orphaned (m5's new item is appended too).
func TestHeldOrphanReplacementIsStoredAtTheEnd(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "vid", OutputFile: out, StreamStartTime: "2025-01-01T00:00:00Z"})
	for i := range 7 {
		cd.messages = append(cd.messages, damagedResumeMsg("m"+strconv.Itoa(i), i))
	}
	cd.messageCount = len(cd.messages)
	if !cd.writeFullChatFile() {
		t.Fatal("seed write failed")
	}
	cd.messages, cd.flushedToDisk = nil, true // the dedup knows none of them

	if orphans := cd.applyMarkers([]ChatMarker{
		{Kind: MarkerDeleted, TargetID: "m2"},
		orphanReplacement("m5", "m5", 5*time.Millisecond+time.Minute),
		orphanReplacement("placeholder", "p", 2*time.Minute),
	}); len(orphans) != 0 || len(cd.pendingMarkers) != 3 {
		t.Fatalf("orphans %d, held %d; want all held: the dedup cannot rule the file out", len(orphans), len(cd.pendingMarkers))
	}
	cd.applyPendingMarkers()

	d := readDamagedResumeFile(t, out)
	var ids []string
	for _, m := range d.Messages {
		ids = append(ids, m.ID)
	}
	if len(d.Messages) != 8 || ids[7] != "p" || d.Messages[7].OffsetMs != 120_000 || cd.messageCount != 8 {
		t.Fatalf("file holds %v (count %d), want m0..m6 and then p at 120000", ids, cd.messageCount)
	}
	if !d.Messages[5].Replaced || !d.Messages[2].Deleted {
		t.Errorf("m2 deleted %v, m5 replaced %v; want both markers applied", d.Messages[2].Deleted, d.Messages[5].Replaced)
	}
}
