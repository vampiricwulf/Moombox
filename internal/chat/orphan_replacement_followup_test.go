package chat

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestReplayReplacementKeepsItsWrapperOffset is FE-14's offset in a replay or
// VOD run: a replaceChatItemAction served inside a replayChatItemAction is
// parsed at the wrapper's videoOffsetTimeMsec, and an orphaned one is
// committed there — the offset a replay file keeps for every message (the
// mode rule), not one derived from its timestamp against the scheduled
// start, which would read it early or late by the late-start delta.
//
// Mutant: parseMarker parsing the replacement item at (0, false) — the
// marker's message carries no offset, and the replay run commits it at the
// timestamp's 90000 rather than YouTube's 42000.
func TestReplayReplacementKeepsItsWrapperOffset(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	sent := strconv.FormatInt(epoch.Add(90*time.Second).UnixMicro(), 10)
	item := map[string]any{"liveChatTextMessageRenderer": map[string]any{
		"id": "p", "timestampUsec": sent,
		"authorName":              map[string]any{"simpleText": "u"},
		"authorExternalChannelId": "UCu",
		"message":                 map[string]any{"runs": []any{map[string]any{"text": "shown"}}},
	}}
	data := map[string]any{"continuationContents": map[string]any{"liveChatContinuation": map[string]any{
		"actions": []any{map[string]any{"replayChatItemAction": map[string]any{
			"videoOffsetTimeMsec": "42000",
			"actions": []any{map[string]any{"replaceChatItemAction": map[string]any{
				"targetItemId": "placeholder", "replacementItem": item,
			}}},
		}}},
	}}}
	res, err := (&ChatAPI{}).parseResponse(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Markers) != 1 || res.Markers[0].Message == nil {
		t.Fatalf("markers = %+v, want the replacement with its item", res.Markers)
	}
	if m := res.Markers[0].Message; !m.HasOffset || m.OffsetMs != 42_000 {
		t.Fatalf("the replacement's message offset = %d (has %v), want the wrapper's 42000", m.OffsetMs, m.HasOffset)
	}

	rp := NewChatDownloader(ChatDownloaderOptions{VideoID: "vid", StreamStartTime: "2025-01-01T00:00:00Z"})
	rp.replay.Store(true)
	if n, _ := rp.processBatch(res); n != 1 || len(rp.messages) != 1 {
		t.Fatalf("replay batch committed %d, want the orphaned replacement", n)
	}
	if m := rp.messages[0]; m.ID != "p" || !m.HasOffset || m.OffsetMs != 42_000 {
		t.Errorf("committed %+v, want p at YouTube's 42000", m)
	}
}

// TestHeldOrphanReplacementIsNotStoredTwice is FE-14's end-of-run pass with a
// replacement whose new item has an ID of its own. When that item is already
// in the file — stored as an orphan earlier in the run, or by the run this
// one resumed, and past what the dedup remembers — it is not appended again;
// and two held replacements of one never-stored placeholder, carrying
// different new IDs, store one message, not two.
//
// Mutants: the pass looking only at the target ID (p is appended a second
// time); the one-per-target delete removed (q2 is appended beside q1).
func TestHeldOrphanReplacementIsNotStoredTwice(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "vid", OutputFile: out, StreamStartTime: "2025-01-01T00:00:00Z"})
	for i := range 7 {
		cd.messages = append(cd.messages, damagedResumeMsg("m"+strconv.Itoa(i), i))
	}
	cd.messages = append(cd.messages, *orphanReplacement("placeholder", "p", 2*time.Minute).Message)
	cd.messageCount = len(cd.messages)
	if !cd.writeFullChatFile() {
		t.Fatal("seed write failed")
	}
	cd.messages, cd.flushedToDisk = nil, true // the dedup knows none of them

	if orphans := cd.applyMarkers([]ChatMarker{
		orphanReplacement("placeholder", "p", 2*time.Minute),
		orphanReplacement("placeholder-2", "q1", 3*time.Minute),
		orphanReplacement("placeholder-2", "q2", 3*time.Minute),
	}); len(orphans) != 0 || len(cd.pendingMarkers) != 3 {
		t.Fatalf("orphans %d, held %d; want all held", len(orphans), len(cd.pendingMarkers))
	}
	cd.applyPendingMarkers()

	d := readDamagedResumeFile(t, out)
	count := map[string]int{}
	for _, m := range d.Messages {
		count[m.ID]++
	}
	if len(d.Messages) != 9 || count["p"] != 1 || count["q1"] != 1 || count["q2"] != 0 || cd.messageCount != 9 {
		t.Errorf("file holds %v (%d, count %d), want m0..m6, p once and q1 alone", count, len(d.Messages), cd.messageCount)
	}
}
