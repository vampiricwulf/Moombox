package chat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// writeBigChatFile builds a chat.json with n messages, each carrying a fat text
// run so the FILE is much larger than the summary a reader needs out of it.
func writeBigChatFile(t *testing.T, path string, n int) int64 {
	t.Helper()
	data := ChatData{
		VideoID:         "vidBig",
		VideoTitle:      "Marathon",
		StreamStartTime: "2026-09-15T00:00:00Z",
		DownloadedAt:    "2026-09-15T04:00:00Z",
		MessageCount:    n,
	}
	filler := strings.Repeat("x", 2048)
	for i := range n {
		data.Messages = append(data.Messages, ChatMessage{
			ID:            "msg-" + itoa(i),
			TimestampUsec: "1757894400000000",
			AuthorName:    "someone",
			Message:       []MessagePart{{Type: "text", Text: filler}},
		})
	}
	if err := utils.WriteChatFileAtomic(path, &data); err != nil {
		t.Fatalf("seed: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

// TestAdoptionSummaryIsStreamed: adoption needs three things out of the file on
// disk — the header epoch its offsets were computed against, how many messages
// it holds, and their IDs for the dedup — and used to get them by reading the
// whole file into memory and unmarshalling every message body. A marathon
// waiting room's chat.json is tens of megabytes and this runs at Start.
//
// Mutant: restoring os.ReadFile + json.Unmarshal allocates at least the file
// size in raw bytes plus the decoded slice, and fails the budget below.
func TestAdoptionSummaryIsStreamed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.json")
	const n = 2000
	size := writeBigChatFile(t, path, n)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	summary, err := readChatFileAdoptionSummary(path)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("readChatFileAdoptionSummary: %v", err)
	}

	if summary.messages != n {
		t.Errorf("messages = %d, want %d", summary.messages, n)
	}
	if len(summary.ids) != n {
		t.Errorf("ids = %d, want %d — every ID seeds the dedup", len(summary.ids), n)
	}
	if summary.ids[0] != "msg-0" || summary.ids[n-1] != "msg-"+itoa(n-1) {
		t.Errorf("ids are out of order: first %q last %q", summary.ids[0], summary.ids[n-1])
	}
	if summary.streamStartTime != "2026-09-15T00:00:00Z" {
		t.Errorf("streamStartTime = %q, want the header's", summary.streamStartTime)
	}

	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("summary allocated %d bytes for a %d-byte file (budget %d)", allocated, size, uint64(size)/2)
	if budget := uint64(size) / 2; allocated > budget {
		t.Errorf("the summary allocated %d bytes for a %d-byte file (budget %d) — it is not streaming",
			allocated, size, budget)
	}
}

// TestAdoptionSummaryRejectsABrokenTail: a well-formed messages array followed
// by garbage cannot be appended to, so it must be reported as damage — the
// caller then preserves the bytes as <chat.json>.corrupt instead of adopting.
//
// Mutant: dropping the trailing FinishJSONValue/EOF check adopts a file the
// append path would splice at the wrong bracket.
func TestAdoptionSummaryRejectsABrokenTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.json")
	body := `{"videoId":"v","messages":[{"id":"m1"}],"messageCount":` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readChatFileAdoptionSummary(path); err == nil {
		t.Error("a truncated tail must not read as an adoptable file")
	}
}
