package chat

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestParseResponseKeepsEveryAction is G36's parser half (owner decision
// D16): every inner action of a replay wrapper is read, not only the first;
// markChatItemAsDeletedAction, markChatItemsByAuthorAsDeletedAction and
// replaceChatItemAction become markers; and every action that is not a
// message — the markers and an unrecognised one alike — is kept verbatim for
// the raw sidecar, an inner one re-wrapped with its wrapper's offset.
//
// Mutants: collectAction reading only the first inner action (the second
// message is lost); parseMarker not reading deletions; the raw copy dropped
// for unrecognised actions; the raw copy not re-wrapped.
func TestParseResponseKeepsEveryAction(t *testing.T) {
	text := func(id, author, msg string) map[string]any {
		return map[string]any{"addChatItemAction": map[string]any{"item": map[string]any{
			"liveChatTextMessageRenderer": map[string]any{
				"id": id, "timestampUsec": "1700000000000000",
				"authorName":              map[string]any{"simpleText": author},
				"authorExternalChannelId": "UC" + author,
				"message":                 map[string]any{"runs": []any{map[string]any{"text": msg}}},
			},
		}}}
	}
	data := map[string]any{"continuationContents": map[string]any{"liveChatContinuation": map[string]any{
		"actions": []any{
			map[string]any{"replayChatItemAction": map[string]any{
				"videoOffsetTimeMsec": "5000",
				"actions": []any{
					text("m1", "a", "first"),
					text("m2", "b", "second"),
					map[string]any{"addLiveChatTickerItemAction": map[string]any{"item": map[string]any{}}},
				},
			}},
			map[string]any{"markChatItemAsDeletedAction": map[string]any{
				"targetItemId":        "m1",
				"deletedStateMessage": map[string]any{"runs": []any{map[string]any{"text": "[message retracted]"}}},
			}},
			map[string]any{"markChatItemsByAuthorAsDeletedAction": map[string]any{"externalChannelId": "UCb"}},
			map[string]any{"replaceChatItemAction": map[string]any{
				"targetItemId":    "m2",
				"replacementItem": text("m2", "b", "edited")["addChatItemAction"].(map[string]any)["item"],
			}},
			map[string]any{"addBannerToLiveChatCommand": map[string]any{"bannerRenderer": map[string]any{}}},
		},
	}}}
	res, err := (&ChatAPI{}).parseResponse(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 2 || res.Messages[1].ID != "m2" || res.Messages[1].OffsetMs != 5000 {
		t.Fatalf("messages = %+v, want both inner messages at the wrapper's offset", res.Messages)
	}
	want := []ChatMarker{
		{Kind: MarkerDeleted, TargetID: "m1", State: "[message retracted]"},
		{Kind: MarkerAuthorDeleted, AuthorChannelID: "UCb"},
		{Kind: MarkerReplaced, TargetID: "m2", Replacement: []MessagePart{{Type: "text", Text: "edited"}}},
	}
	if !reflect.DeepEqual(res.Markers, want) {
		t.Errorf("markers = %+v, want %+v", res.Markers, want)
	}
	if len(res.RawActions) != 5 {
		t.Fatalf("raw actions = %d, want the ticker, three markers and the banner", len(res.RawActions))
	}
	var ticker map[string]any
	if err := json.Unmarshal(res.RawActions[0], &ticker); err != nil {
		t.Fatal(err)
	}
	wrap, _ := ticker["replayChatItemAction"].(map[string]any)
	inner, _ := wrap["actions"].([]any)
	if wrap["videoOffsetTimeMsec"] != "5000" || len(inner) != 1 || inner[0].(map[string]any)["addLiveChatTickerItemAction"] == nil {
		t.Errorf("the replay ticker's raw line = %s, want it alone in a wrapper with offset 5000", res.RawActions[0])
	}
	if !strings.Contains(string(res.RawActions[4]), "addBannerToLiveChatCommand") {
		t.Errorf("the banner's raw line = %s", res.RawActions[4])
	}
}

// TestMarkersFlagStoredMessages is the downloader half: a deletion of a
// message still buffered is applied at once; one of a message already on
// disk, a replacement, and an author's sweep are applied by the rewrite at
// the end of the run — the sweep only to the messages before it, not one the
// author sends later. The original text is kept, and the raw lines land in
// chat.raw.jsonl beside the chat file.
//
// Mutants: applyPendingMarkers not run (the on-disk messages carry no flags);
// the sweep bound by the file's length at the end rather than at the marker
// (the later message is flagged too); flushRawActions not writing.
func TestMarkersFlagStoredMessages(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID: "vid", OutputFile: out, InitialContinuation: "live0",
		IsLiveOrUpcoming: true, StreamStartTime: "2025-01-01T00:00:00Z",
	})
	msg := func(id, author string, i int) ChatMessage {
		m := damagedResumeMsg(id, i)
		m.AuthorChannelID = author
		return m
	}
	call := 0
	cd.testFetchOverride = func(ctx context.Context) (*ChatApiResponse, error) {
		call++
		switch call {
		case 1: // flushed at once: the first write
			return &ChatApiResponse{Messages: []ChatMessage{msg("a", "UCx", 0), msg("b", "UCban", 1), msg("c", "UCx", 2)}, NextContinuation: "live1", TimeoutMs: 1}, nil
		case 2: // buffered (inside writeInterval), deleted in the same batch
			return &ChatApiResponse{
				Messages: []ChatMessage{msg("e", "UCx", 3)},
				Markers: []ChatMarker{
					{Kind: MarkerDeleted, TargetID: "e"},
					{Kind: MarkerDeleted, TargetID: "a", State: "[message deleted]"},
					{Kind: MarkerReplaced, TargetID: "c", Replacement: []MessagePart{{Type: "text", Text: "new"}}},
					{Kind: MarkerAuthorDeleted, AuthorChannelID: "UCban"},
				},
				RawActions:       []json.RawMessage{json.RawMessage(`{"addBannerToLiveChatCommand":{}}`)},
				NextContinuation: "live2", TimeoutMs: 1,
			}, nil
		case 3: // the banned author back after a timeout
			return &ChatApiResponse{Messages: []ChatMessage{msg("f", "UCban", 4)}, NextContinuation: "live3", TimeoutMs: 1}, nil
		}
		return &ChatApiResponse{IsComplete: true}, nil
	}
	cd.testRecoveryOverride = func(ctx context.Context) bool { return false }
	_ = cd.Start(context.Background())

	d := readDamagedResumeFile(t, out)
	got := map[string]ChatMessage{}
	for _, m := range d.Messages {
		got[m.ID] = m
	}
	if len(d.Messages) != 5 || d.MessageCount != 5 {
		t.Fatalf("file holds %d messages (header %d), want 5", len(d.Messages), d.MessageCount)
	}
	if !got["e"].Deleted {
		t.Error("the buffered message's deletion was not applied")
	}
	if a := got["a"]; !a.Deleted || a.DeletedState != "[message deleted]" || a.Message[0].Text != "x" {
		t.Errorf("on-disk deletion: %+v, want flagged with the original text kept", a)
	}
	if c := got["c"]; !c.Replaced || len(c.Replacement) != 1 || c.Replacement[0].Text != "new" || c.Message[0].Text != "x" {
		t.Errorf("replacement: %+v, want flagged, the new text beside the original", c)
	}
	if !got["b"].Deleted || got["f"].Deleted {
		t.Errorf("author sweep: b deleted %v, f deleted %v; want only the message before the sweep", got["b"].Deleted, got["f"].Deleted)
	}

	f, err := os.Open(filepath.Join(filepath.Dir(out), "chat.raw.jsonl"))
	if err != nil {
		t.Fatalf("raw sidecar: %v", err)
	}
	defer f.Close()
	var lines []string
	for sc := bufio.NewScanner(f); sc.Scan(); {
		lines = append(lines, sc.Text())
	}
	if !reflect.DeepEqual(lines, []string{`{"addBannerToLiveChatCommand":{}}`}) {
		t.Errorf("raw sidecar = %q", lines)
	}
}

// TestUnmarkedMessageSerializesAsBefore: the marker fields are omitempty, so
// a message nothing happened to — every message of every file written before
// them — has the JSON shape it always had, and the existing readers see no
// change.
func TestUnmarkedMessageSerializesAsBefore(t *testing.T) {
	b, err := json.Marshal(ChatMessage{ID: "x", Message: []MessagePart{{Type: "text", Text: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"deleted", "deletedState", "replaced", "replacement"} {
		if strings.Contains(string(b), `"`+key+`"`) {
			t.Errorf("an unmarked message carries %q: %s", key, b)
		}
	}
}

// TestRawSidecarPath: chat.json's sidecar is chat.raw.jsonl, and a replay
// re-run's temporary file shares the archive's.
func TestRawSidecarPath(t *testing.T) {
	for in, want := range map[string]string{
		"/s/chat.json":                     "/s/chat.raw.jsonl",
		"/s/chat.json" + replayRerunSuffix: "/s/chat.raw.jsonl",
	} {
		if got := rawSidecarPath(in); got != want {
			t.Errorf("rawSidecarPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestApplyMarkersToTheBuffer: a capture that has not flushed (early chat,
// before staging exists) holds every message in memory, so a marker is
// applied there and nothing is held for the end-of-run rewrite.
//
// Mutant: applyMarkers ignoring the buffer (the message stays unflagged).
func TestApplyMarkersToTheBuffer(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "vid"})
	cd.messages = []ChatMessage{{ID: "a", AuthorChannelID: "UCx"}, {ID: "b", AuthorChannelID: "UCy"}}
	cd.messageCount = 2
	cd.applyMarkers([]ChatMarker{{Kind: MarkerDeleted, TargetID: "b", State: "[message retracted]"}, {Kind: MarkerAuthorDeleted, AuthorChannelID: "UCx"}})
	if !cd.messages[0].Deleted || !cd.messages[1].Deleted || cd.messages[1].DeletedState != "[message retracted]" {
		t.Errorf("buffer = %+v, want both flagged", cd.messages)
	}
	if len(cd.pendingMarkers) != 0 {
		t.Errorf("held %d markers for a file that was never flushed", len(cd.pendingMarkers))
	}
}
