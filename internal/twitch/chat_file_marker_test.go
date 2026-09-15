package twitch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// readChatHeader decodes just the scalars of a chat file.
func readChatHeader(t *testing.T, path string) TwitchChatData {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var data TwitchChatData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return data
}

// TestLiveChatFileCarriesTheOffsetMarker pins the marker on the IRC writer.
//
// Mutant: writing the field but leaving it empty (or omitting it from the
// literal). player.js reads its ABSENCE as "this file predates the code-point
// fix" and re-shifts every emote span in it, so an unmarked new file renders
// worse than an unfixed old one.
func TestLiveChatFileCarriesTheOffsetMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.json")
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "testchan",
		StreamID:     "stream-1",
		OutputPath:   path,
	}, &testLogger{})

	msgs := []TwitchChatMessage{{ID: "m1", TimestampMs: 1700000000000, Message: "Kappa"}}
	if err := cd.writeFullChatFileTo(path, msgs, len(msgs), 0); err != nil {
		t.Fatalf("writeFullChatFileTo: %v", err)
	}
	if got := readChatHeader(t, path).EmoteOffsets; got != "utf16" {
		t.Errorf("emoteOffsets = %q, want %q", got, "utf16")
	}
}

// TestVodChatFileCarriesTheOffsetMarker is the VOD twin. The VOD path always
// emitted UTF-16 (api.go utf16Len), so its files must be marked too — an
// unmarked one would be corrected a second time at replay.
func TestVodChatFileCarriesTheOffsetMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vod.json")
	vcd := NewVodChatDownloader(NewAPI(&testLogger{}), VodChatOptions{
		VodID:      "v1",
		OutputPath: path,
	}, &testLogger{})
	if err := vcd.writeFullFile([]TwitchChatMessage{{ID: "c1", Message: "Kappa"}}); err != nil {
		t.Fatalf("writeFullFile: %v", err)
	}
	if got := readChatHeader(t, path).EmoteOffsets; got != "utf16" {
		t.Errorf("emoteOffsets = %q, want %q", got, "utf16")
	}
}

// TestMarkedChatFileStaysAppendableAndReadable pins the three readers that walk
// a chat file's header against the NEW scalar. The header's field order is
// already load-bearing (AppendChatMessages splices at the last ']', so the
// messages array must stay last), and this is the regression that would prove
// the new field broke it.
//
// Mutants: placing EmoteOffsets AFTER Messages in the struct (the append then
// splices into the scalar and the file stops parsing); a header reader that
// treats an unrecognised key as end-of-header (recordingStartTime would go
// missing and every resumed part's offsets would rebase onto the restart).
func TestMarkedChatFileStaysAppendableAndReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.json")
	cd := NewChatDownloader(ChatDownloaderOptions{
		ChannelLogin: "testchan",
		StreamID:     "stream-1",
		OutputPath:   path,
	}, &testLogger{})

	const baseMs = int64(1700000000000)
	first := []TwitchChatMessage{{ID: "m1", TimestampMs: baseMs, Message: "one"}}
	if err := cd.writeFullChatFileTo(path, first, 1, baseMs); err != nil {
		t.Fatalf("writeFullChatFileTo: %v", err)
	}

	second := []TwitchChatMessage{{ID: "m2", TimestampMs: baseMs + 1000, Message: "two"}}
	if err := utils.AppendChatMessages(path, second, 2, cd.logger); err != nil {
		t.Fatalf("AppendChatMessages against a marked file: %v", err)
	}

	data := readChatHeader(t, path)
	if data.EmoteOffsets != "utf16" {
		t.Errorf("emoteOffsets after append = %q, want %q", data.EmoteOffsets, "utf16")
	}
	if len(data.Messages) != 2 || data.Messages[0].ID != "m1" || data.Messages[1].ID != "m2" {
		t.Fatalf("messages after append = %+v, want m1 then m2", data.Messages)
	}

	gotBase, ok, err := chatFileRecordingBaseMs(path)
	if err != nil {
		t.Fatalf("chatFileRecordingBaseMs: %v", err)
	}
	if !ok || gotBase != baseMs {
		t.Errorf("chatFileRecordingBaseMs = (%d, %v), want (%d, true) — the marker hid the "+
			"recording base from the header scan", gotBase, ok, baseMs)
	}

	summary, err := readChatPartFileSummary(path)
	if err != nil {
		t.Fatalf("readChatPartFileSummary against a marked file: %v", err)
	}
	if summary.messages != 2 {
		t.Errorf("summary.messages = %d, want 2", summary.messages)
	}
	if strings.Join(summary.recentIDs, ",") != "m1,m2" {
		t.Errorf("summary.recentIDs = %v, want [m1 m2]", summary.recentIDs)
	}
}
