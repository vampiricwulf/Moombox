package chat

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// TestAFailedFlushKeepsItsBatch: writeChatFile cleared the buffer and marked
// the file flushed even when the full write failed, so a transient failure
// lost that batch for good while the header still counted it. The batch now
// stays buffered and the next flush writes it.
//
// Mutant: clear the buffer after a failed writeFullChatFile again — the file
// ends up with m2 alone.
func TestAFailedFlushKeepsItsBatch(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: out})
	var reported []error
	cd.OnError = func(err error) { reported = append(reported, err) }

	// A directory where the file should be: the atomic write's rename fails.
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	cd.processBatch(&ChatApiResponse{Messages: []ChatMessage{{ID: "m1"}}})
	cd.writeChatFile()
	if len(reported) == 0 {
		t.Fatal("precondition: the first flush did not fail")
	}

	// The obstruction clears; the next batch flushes with the first.
	if err := os.Remove(out); err != nil {
		t.Fatal(err)
	}
	cd.processBatch(&ChatApiResponse{Messages: []ChatMessage{{ID: "m2"}}})
	cd.writeChatFile()

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var doc ChatData
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("chat file is not JSON: %v", err)
	}
	var ids []string
	for _, m := range doc.Messages {
		ids = append(ids, m.ID)
	}
	if len(ids) != 2 || ids[0] != "m1" || ids[1] != "m2" {
		t.Errorf("messages on disk = %v, want [m1 m2]", ids)
	}
	if doc.MessageCount != 2 {
		t.Errorf("header messageCount = %d, want 2", doc.MessageCount)
	}
}

// TestAPartialAppendKeepsItsBatch: an append whose write failed (a full disk)
// puts the file's end back, so the file holds what it held before — but the
// batch was dropped while cd.messageCount kept counting it, and the header and
// the job row over-counted the array for good. The batch now waits for the
// next flush.
//
// Mutant: clear cd.messages on ErrChatFilePartialWrite again — m2 never
// reaches the file and the header says 3 over an array of 2.
func TestAPartialAppendKeepsItsBatch(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: out})
	cd.processBatch(&ChatApiResponse{Messages: []ChatMessage{{ID: "m1"}}})
	cd.writeChatFile()

	real := appendChatMessages
	t.Cleanup(func() { appendChatMessages = real })
	appendChatMessages = func(string, []ChatMessage, int, utils.ChatFileLogger) error {
		return fmt.Errorf("%w: no space left on device", utils.ErrChatFilePartialWrite)
	}
	cd.processBatch(&ChatApiResponse{Messages: []ChatMessage{{ID: "m2"}}})
	cd.writeChatFile()

	appendChatMessages = real
	cd.processBatch(&ChatApiResponse{Messages: []ChatMessage{{ID: "m3"}}})
	cd.writeChatFile()

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var doc ChatData
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("chat file is not JSON: %v", err)
	}
	var ids []string
	for _, m := range doc.Messages {
		ids = append(ids, m.ID)
	}
	if len(ids) != 3 || ids[0] != "m1" || ids[1] != "m2" || ids[2] != "m3" {
		t.Errorf("messages on disk = %v, want [m1 m2 m3]", ids)
	}
	if doc.MessageCount != len(ids) {
		t.Errorf("header messageCount = %d, array %d", doc.MessageCount, len(ids))
	}
}
