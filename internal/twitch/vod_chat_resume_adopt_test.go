package twitch

import (
	"context"
	"path/filepath"
	"testing"
)

// Start's periodic flush is `flush(); saveResumeState(offset)`. A process
// killed between the two left chat.json holding a page the sidecar did not
// know about; the resumed run re-entered by the sidecar's older offset,
// re-fetched that page and appended it a second time, with the header count
// short as well. Resume now learns the file's message IDs and count.
//
// Mutant: Start without adoptExistingFile — c1-0 and c1-1 appear twice.
func TestVodChatResumeDoesNotDuplicateThePageTheSidecarMissed(t *testing.T) {
	pages := []vodCommentPageSpec{
		{count: 2, offset: 900, hasNext: true},
		{count: 2, offset: 1000, hasNext: false},
	}
	installVodCommentStub(t, pages)
	out := filepath.Join(t.TempDir(), "vod.chat.json")

	// The on-disk state that kill leaves: the file holds pages 0 and 1, the
	// sidecar describes page 0 only.
	prev := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{VodID: "v1", OutputPath: out}, &testLogger{})
	add := func(ids []string, offsetMs int64) {
		for _, id := range ids {
			prev.dedup.Add(id)
			prev.messages = append(prev.messages, TwitchChatMessage{ID: id, OffsetMs: offsetMs, Message: "hello", MessageType: "chat"})
			prev.totalCount.Add(1)
		}
		prev.flush()
	}
	add([]string{"c0-0", "c0-1"}, 900_000)
	prev.saveResumeState(900)
	add([]string{"c1-0", "c1-1"}, 1_000_000)
	// killed here: saveResumeState(1000) never ran

	vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{VodID: "v1", OutputPath: out}, &testLogger{})
	if err := vcd.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	msgs, err := readChatFileMessages(out)
	if err != nil {
		t.Fatalf("read chat file: %v", err)
	}
	seen := map[string]int{}
	for _, m := range msgs {
		seen[m.ID]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("%s is in the file %d times, want once", id, n)
		}
	}
	if len(msgs) != 4 || vcd.MessageCount() != 4 {
		t.Errorf("file holds %d messages, total reads %d — want 4 and 4", len(msgs), vcd.MessageCount())
	}
}
