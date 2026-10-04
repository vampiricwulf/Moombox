package chat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
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
