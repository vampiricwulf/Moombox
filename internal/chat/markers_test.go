package chat

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
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
	// The replacement's item parsed whole (FE-14) is checked apart.
	var repl *ChatMessage
	if len(res.Markers) == 3 {
		repl, res.Markers[2].Message = res.Markers[2].Message, nil
	}
	if !reflect.DeepEqual(res.Markers, want) {
		t.Errorf("markers = %+v, want %+v", res.Markers, want)
	}
	if repl == nil || repl.ID != "m2" || repl.TimestampUsec != "1700000000000000" || len(repl.Message) != 1 || repl.Message[0].Text != "edited" {
		t.Errorf("the replacement's message = %+v, want m2's new item whole", repl)
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
// (the later message is flagged too); flushRawActions not writing; a sweep
// not held once it hit a buffered message of the same author (b, on disk,
// stays unflagged).
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
				// g: the banned author has a buffered message too, and the
				// sweep must still be held for their one on disk.
				Messages: []ChatMessage{msg("e", "UCx", 3), msg("g", "UCban", 5)},
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
	if len(d.Messages) != 6 || d.MessageCount != 6 {
		t.Fatalf("file holds %d messages (header %d), want 6", len(d.Messages), d.MessageCount)
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
	if !got["b"].Deleted || !got["g"].Deleted || got["f"].Deleted {
		t.Errorf("author sweep: b deleted %v, g deleted %v, f deleted %v; want the on-disk and buffered messages before the sweep, not the later one",
			got["b"].Deleted, got["g"].Deleted, got["f"].Deleted)
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
// re-run's temporary file has its own beside it.
func TestRawSidecarPath(t *testing.T) {
	for in, want := range map[string]string{
		"/s/chat.json":                     "/s/chat.raw.jsonl",
		"/s/chat.json" + replayRerunSuffix: "/s/chat.raw.jsonl" + replayRerunSuffix,
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

// TestPendingMarkersRewriteIsStreamed: the end-of-run rewrite holds one batch
// of messages at a time, never the file — every message reaches the new file
// through appends of at most markerRewriteBatch — and the flags, the sweep's
// bound and the order of the messages come through it. A held marker that
// names nothing on disk costs no rewrite at all.
//
// Mutants: the rewrite reading the file whole (SalvageChatMessages and
// writeFullChatFile, as it was: no batched appends); the first pass's
// no-match return removed (the no-op marker rewrites the file).
func TestPendingMarkersRewriteIsStreamed(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "vid", OutputFile: out, StreamStartTime: "2025-01-01T00:00:00Z"})
	var ids []string
	for i := range 7 {
		m := damagedResumeMsg(fmt.Sprintf("m%d", i), i)
		m.AuthorChannelID = "UCx"
		if i%2 == 1 {
			m.AuthorChannelID = "UCban"
		}
		cd.messages = append(cd.messages, m)
		ids = append(ids, m.ID)
	}
	cd.messageCount = len(cd.messages)
	if !cd.writeFullChatFile() {
		t.Fatal("seed write failed")
	}
	cd.messages, cd.flushedToDisk = nil, true

	oldBatch, real := markerRewriteBatch, appendChatMessages
	t.Cleanup(func() { markerRewriteBatch, appendChatMessages = oldBatch, real })
	markerRewriteBatch = 2
	var batches []int
	appendChatMessages = func(path string, msgs []ChatMessage, count int, l utils.ChatFileLogger) error {
		if path != out {
			batches = append(batches, len(msgs))
		}
		return real(path, msgs, count, l)
	}

	// Nothing on disk is named: no rewrite.
	cd.pendingMarkers = []pendingChatMarker{
		{marker: ChatMarker{Kind: MarkerReplaced, TargetID: "placeholder"}, onDisk: 7},
		{marker: ChatMarker{Kind: MarkerAuthorDeleted, AuthorChannelID: "UCnobody"}, onDisk: 7},
	}
	cd.applyPendingMarkers()
	if len(batches) != 0 {
		t.Fatalf("a marker naming nothing on disk rewrote the file (%v)", batches)
	}

	// The sweep arrived when 5 messages were on disk: m5 is after it.
	cd.pendingMarkers = []pendingChatMarker{
		{marker: ChatMarker{Kind: MarkerDeleted, TargetID: "m2", State: "[message retracted]"}, onDisk: 7},
		{marker: ChatMarker{Kind: MarkerAuthorDeleted, AuthorChannelID: "UCban"}, onDisk: 5},
	}
	cd.applyPendingMarkers()
	if !reflect.DeepEqual(batches, []int{2, 2, 2, 1}) {
		t.Errorf("rewrite appended %v, want batches of at most 2 covering all 7", batches)
	}
	d := readDamagedResumeFile(t, out)
	var gotIDs []string
	for _, m := range d.Messages {
		gotIDs = append(gotIDs, m.ID)
		wantDeleted := m.ID == "m1" || m.ID == "m2" || m.ID == "m3"
		if m.Deleted != wantDeleted {
			t.Errorf("%s deleted = %v, want %v", m.ID, m.Deleted, wantDeleted)
		}
	}
	if !reflect.DeepEqual(gotIDs, ids) || d.MessageCount != 7 || cd.messageCount != 7 {
		t.Errorf("rewritten file holds %v (header %d, count %d), want %v", gotIDs, d.MessageCount, cd.messageCount, ids)
	}
	if d.Messages[2].DeletedState != "[message retracted]" {
		t.Errorf("m2's state = %q", d.Messages[2].DeletedState)
	}
	if left, _ := filepath.Glob(out + ".markers-*"); len(left) != 0 {
		t.Errorf("rewrite left %v behind", left)
	}
}

// TestLiveRawLinesCarryTheirOffset: a live action that did not become a
// message is written in yt-dlp's live shape, {"replayChatItemAction":
// {"actions":[action]}, "videoOffsetTimeMsec": ..., "isLive": true}, at the
// offset of the newest message before it — in its own batch, or carried from
// an earlier one — against the chat file's epoch, so a deletion or a banner
// (no time of its own) can be placed on the archive's timeline. A replay
// action keeps the wrapper it came in.
//
// Mutants: live actions written bare (no wrapper); the offset taken from the
// batch's last message rather than the newest one before the action; the
// offset not carried to the next batch.
func TestLiveRawLinesCarryTheirOffset(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	text := func(id string, at time.Duration) map[string]any {
		return map[string]any{"addChatItemAction": map[string]any{"item": map[string]any{
			"liveChatTextMessageRenderer": map[string]any{
				"id": id, "timestampUsec": strconv.FormatInt(epoch.Add(at).UnixMicro(), 10),
				"authorName":              map[string]any{"simpleText": "a"},
				"authorExternalChannelId": "UCa",
				"message":                 map[string]any{"runs": []any{map[string]any{"text": "hi"}}},
			},
		}}}
	}
	batch := func(actions ...any) *ChatApiResponse {
		res, err := (&ChatAPI{}).parseResponse(map[string]any{"continuationContents": map[string]any{
			"liveChatContinuation": map[string]any{"actions": actions}}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	del := func(id string) map[string]any {
		return map[string]any{"markChatItemAsDeletedAction": map[string]any{"targetItemId": id}}
	}
	first := batch(text("m1", 5*time.Second), del("m1"), text("m2", 9*time.Second),
		map[string]any{"addBannerToLiveChatCommand": map[string]any{}})
	first.NextContinuation, first.TimeoutMs, first.IsComplete = "live1", 1, false
	second := batch(del("m2"))
	second.NextContinuation, second.TimeoutMs, second.IsComplete = "live2", 1, false

	out := filepath.Join(t.TempDir(), "chat.json")
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID: "vid", OutputFile: out, InitialContinuation: "live0",
		IsLiveOrUpcoming: true, StreamStartTime: epoch.Format(time.RFC3339),
	})
	call := 0
	cd.testFetchOverride = func(ctx context.Context) (*ChatApiResponse, error) {
		call++
		switch call {
		case 1:
			return first, nil
		case 2:
			return second, nil
		}
		return &ChatApiResponse{IsComplete: true}, nil
	}
	cd.testRecoveryOverride = func(ctx context.Context) bool { return false }
	_ = cd.Start(context.Background())

	data, err := os.ReadFile(rawSidecarPath(out))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []struct{ offset, key string }{{"5000", "markChatItemAsDeletedAction"}, {"9000", "addBannerToLiveChatCommand"}, {"9000", "markChatItemAsDeletedAction"}}
	if len(lines) != len(want) {
		t.Fatalf("raw sidecar = %q, want %d lines", lines, len(want))
	}
	for i, w := range want {
		var line struct {
			Replay struct {
				Actions []map[string]json.RawMessage `json:"actions"`
			} `json:"replayChatItemAction"`
			Offset string `json:"videoOffsetTimeMsec"`
			IsLive bool   `json:"isLive"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &line); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if !line.IsLive || line.Offset != w.offset || len(line.Replay.Actions) != 1 || line.Replay.Actions[0][w.key] == nil {
			t.Errorf("line %d = %s, want %s wrapped live at offset %s", i, lines[i], w.key, w.offset)
		}
	}
}
