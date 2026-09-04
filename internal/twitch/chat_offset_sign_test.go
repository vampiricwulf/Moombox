package twitch

import (
	"path/filepath"
	"testing"
	"time"
)

// A message that reached IRC before the recording base was sent BEFORE the
// video starts. Clamping it to 0 piled every such message onto 0:00 and made
// them indistinguishable from real 0:00 chat; the player renders negative
// offsets ("-1:30") and needs the sign to place them as pre-show.
func TestAddMessageKeepsNegativeOffsetBeforeRecordingStart(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	cd := newTestChatDownloader(t, out)
	cd.SetRecordingStartTime("2026-06-11T10:00:00Z")
	base := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC).UnixMilli()

	cd.addMessage(&TwitchChatMessage{ID: "early", AuthorName: "a", Message: "hi", TimestampMs: base - 90_000})
	cd.addMessage(&TwitchChatMessage{ID: "late", AuthorName: "b", Message: "yo", TimestampMs: base + 5_000})
	cd.flush()

	d := readChatData(t, out)
	want := map[string]int64{"early": -90_000, "late": 5_000}
	for _, m := range d.Messages {
		if m.OffsetMs != want[m.ID] {
			t.Errorf("%s offsetMs = %d, want %d", m.ID, m.OffsetMs, want[m.ID])
		}
	}
}
