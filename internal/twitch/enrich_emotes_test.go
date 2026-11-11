package twitch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// EnrichWithEmotes on a file written the way the downloaders write it (a
// first indented write, then compact appends): every header field and every
// message comes back as it was, the emotes block sits before the messages
// array, and the file still takes appends.
func TestEnrichWithEmotesKeepsHeaderAndMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.json")
	first := []TwitchChatMessage{
		{ID: "m1", AuthorName: "a", Message: "hello ] } <b>", MessageType: "chat", TimestampMs: 1000, OffsetMs: 1000,
			Raw: "@badges=;emotes=25:0-4 :a!a@a PRIVMSG #c :hello"},
		{ID: "m2", AuthorName: "b", Message: "two", MessageType: "chat", TimestampMs: 2000, OffsetMs: 2000},
	}
	appended := []TwitchChatMessage{
		{ID: "m3", AuthorName: "c", Message: "three", MessageType: "chat", TimestampMs: 3000, OffsetMs: 3000},
	}
	want := TwitchChatData{
		Platform: "twitch", ChannelLogin: "testchan", ChannelDisplayName: "TestChan",
		StreamID: "s1", StreamStartTime: "2026-06-11T09:55:00Z", RecordingStartTime: "2026-06-11T10:00:00Z",
		DownloadedAt: "2026-06-11T12:00:00Z", MessageCount: len(first), Messages: first,
	}
	if err := utils.WriteChatFileAtomic(path, &want); err != nil {
		t.Fatal(err)
	}
	if err := utils.AppendChatMessages(path, appended, 3, nil); err != nil {
		t.Fatal(err)
	}
	read := func() (TwitchChatData, string) {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var d TwitchChatData
		if err := json.Unmarshal(b, &d); err != nil {
			t.Fatalf("not valid JSON: %v", err)
		}
		return d, string(b)
	}
	before, _ := read()

	emotes := &TwitchEmoteData{
		BTTV:    []EmoteInfo{{ID: "1", Code: "kek", URL: "https://example.invalid/1?a=1&b=2"}},
		SevenTV: []EmoteInfo{{ID: "2", Code: "pog", URL: "https://example.invalid/2"}},
	}
	if err := EnrichWithEmotes(path, emotes); err != nil {
		t.Fatalf("EnrichWithEmotes: %v", err)
	}
	after, raw := read()

	if !reflect.DeepEqual(after.Emotes, emotes) {
		t.Errorf("emotes = %+v, want %+v", after.Emotes, emotes)
	}
	after.Emotes = nil
	if !reflect.DeepEqual(after, before) {
		t.Errorf("the file changed beyond its emotes block:\n got %+v\nwant %+v", after, before)
	}
	if e, m := strings.Index(raw, `"emotes":`), strings.Index(raw, `"messages":`); e < 0 || e > m {
		t.Errorf("emotes (%d) must come before messages (%d)", e, m)
	}
	if ok, err := utils.ChatFileEndIntact(path); err != nil || !ok {
		t.Fatalf("ChatFileEndIntact = %v, %v", ok, err)
	}
	if err := utils.AppendChatMessages(path, []TwitchChatMessage{{ID: "m4", Message: "four"}}, 4, nil); err != nil {
		t.Fatalf("append after enrichment: %v", err)
	}
	final, _ := read()
	if len(final.Messages) != 4 || final.MessageCount != 4 || final.Emotes == nil {
		t.Errorf("after append: count=%d len=%d emotes=%v", final.MessageCount, len(final.Messages), final.Emotes)
	}
}

// A file that does not parse is left as it was: no emotes, no change.
func TestEnrichWithEmotesLeavesADamagedFileAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.json")
	content := `{"platform":"twitch","messageCount":2,"messages":[{"id":"m1","message":"a"},{"id":"m2","mess`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnrichWithEmotes(path, &TwitchEmoteData{}); err == nil {
		t.Error("no error for a damaged file")
	}
	if b, _ := os.ReadFile(path); string(b) != content {
		t.Errorf("damaged file was changed: %q", b)
	}
}
