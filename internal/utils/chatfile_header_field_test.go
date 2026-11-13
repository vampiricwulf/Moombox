package utils

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type headerFieldMsg struct {
	ID   string   `json:"id"`
	Text string   `json:"text"`
	Tags []string `json:"tags,omitempty"`
}

type headerFieldDoc struct {
	Platform     string           `json:"platform"`
	DownloadedAt string           `json:"downloadedAt"`
	MessageCount int              `json:"messageCount"`
	Extra        map[string]any   `json:"extra,omitempty"`
	Emotes       map[string]any   `json:"emotes,omitempty"`
	Messages     []headerFieldMsg `json:"messages"`
}

// writeHeaderFieldFixture writes a chat file the way the downloaders do: the
// first messages through WriteChatFileAtomic (indented), the rest appended
// (compact, one per line).
func writeHeaderFieldFixture(t *testing.T, first, appended []headerFieldMsg) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chat.json")
	doc := headerFieldDoc{
		Platform: "twitch", DownloadedAt: "2026-06-11T12:00:00Z",
		MessageCount: len(first), Extra: map[string]any{"a": []int{1, 2}, "b": "<&>"},
		Messages: first,
	}
	if doc.Messages == nil {
		doc.Messages = []headerFieldMsg{}
	}
	if err := WriteChatFileAtomic(path, &doc); err != nil {
		t.Fatal(err)
	}
	if err := AppendChatMessages(path, appended, len(first)+len(appended), nil); err != nil {
		t.Fatal(err)
	}
	return path
}

func readHeaderFieldDoc(t *testing.T, path string) (headerFieldDoc, string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d headerFieldDoc
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, b)
	}
	return d, string(b)
}

// The rewrite keeps the count, the order and every header value, puts the new
// field directly before messages, copies message bytes as they were, and
// leaves a file that still takes appends.
func TestSetChatFileHeaderFieldRoundTrip(t *testing.T) {
	first := []headerFieldMsg{{ID: "1", Text: "one ] }", Tags: []string{"x", "y"}}, {ID: "2", Text: "two"}}
	appended := []headerFieldMsg{{ID: "3", Text: "three <b>"}, {ID: "4", Text: "four", Tags: []string{"z"}}}
	all := append(append([]headerFieldMsg{}, first...), appended...)
	path := writeHeaderFieldFixture(t, first, appended)
	before, beforeRaw := readHeaderFieldDoc(t, path)

	emotes := map[string]any{"bttv": []map[string]string{{"id": "e", "code": "kek", "url": "https://x.invalid/?a=1&b=2"}}}
	if err := SetChatFileHeaderField(path, "emotes", emotes); err != nil {
		t.Fatalf("SetChatFileHeaderField: %v", err)
	}
	after, afterRaw := readHeaderFieldDoc(t, path)

	if after.Platform != before.Platform || after.DownloadedAt != before.DownloadedAt || after.MessageCount != 4 {
		t.Errorf("header scalars changed: %+v", after)
	}
	if got, _ := json.Marshal(after.Extra["a"]); after.Extra["b"] != "<&>" || string(got) != "[1,2]" {
		t.Errorf("nested header value changed: %s", got)
	}
	if len(after.Messages) != 4 {
		t.Fatalf("messages = %d, want 4", len(after.Messages))
	}
	for i, m := range after.Messages {
		if want := all[i]; m.ID != want.ID || m.Text != want.Text || strings.Join(m.Tags, ",") != strings.Join(want.Tags, ",") {
			t.Errorf("message %d = %+v, want %+v", i, m, want)
		}
	}
	if after.Emotes == nil {
		t.Fatal("emotes not written")
	}
	// Field order: the original order, then emotes, then messages last.
	order := []string{`"platform"`, `"downloadedAt"`, `"messageCount"`, `"extra"`, `"emotes"`, `"messages"`}
	last := -1
	for _, k := range order {
		i := strings.Index(afterRaw, "\n  "+k+":")
		if i <= last {
			t.Fatalf("field %s out of order in:\n%s", k, afterRaw)
		}
		last = i
	}
	// Message bytes are carried over exactly: everything from the messages
	// array on is the same text as before, but for the separator an append
	// leaves after the indented block (a comma on its own line), which comes
	// out as a comma straight after the brace.
	tail := func(s string) string { return s[strings.Index(s, `"messages":`):] }
	if strings.Replace(tail(beforeRaw), "}\n  ,\n", "},\n", 1) != tail(afterRaw) {
		t.Errorf("messages array bytes changed:\n--- before\n%s\n--- after\n%s", tail(beforeRaw), tail(afterRaw))
	}
	// The padded count survives, so an in-place header update still fits.
	if !strings.Contains(afterRaw, `"messageCount": 4`+strings.Repeat(" ", messageCountPadWidth-1)+",") {
		t.Errorf("messageCount is not padded:\n%s", afterRaw[:200])
	}
	if ok, err := ChatFileEndIntact(path); err != nil || !ok {
		t.Fatalf("ChatFileEndIntact = %v, %v", ok, err)
	}
	if err := AppendChatMessages(path, []headerFieldMsg{{ID: "5", Text: "five"}}, 5, nil); err != nil {
		t.Fatalf("append after rewrite: %v", err)
	}
	final, _ := readHeaderFieldDoc(t, path)
	if len(final.Messages) != 5 || final.MessageCount != 5 || final.Messages[4].ID != "5" || final.Emotes == nil {
		t.Errorf("after append: count=%d len=%d emotes=%v", final.MessageCount, len(final.Messages), final.Emotes)
	}
	if n, ok := ReadChatFileMessageCount(path); !ok || n != 5 {
		t.Errorf("ReadChatFileMessageCount = %d, %v", n, ok)
	}
}

// Setting the same field again replaces it and changes nothing else.
func TestSetChatFileHeaderFieldReplacesAndIsStable(t *testing.T) {
	path := writeHeaderFieldFixture(t, []headerFieldMsg{{ID: "1", Text: "one"}}, []headerFieldMsg{{ID: "2", Text: "two"}})
	if err := SetChatFileHeaderField(path, "emotes", map[string]any{"ffz": []int{1}}); err != nil {
		t.Fatal(err)
	}
	if err := SetChatFileHeaderField(path, "emotes", map[string]any{"bttv": []int{2}}); err != nil {
		t.Fatal(err)
	}
	_, once := readHeaderFieldDoc(t, path)
	if strings.Count(once, `"emotes"`) != 1 || strings.Contains(once, "ffz") {
		t.Fatalf("emotes not replaced:\n%s", once)
	}
	if err := SetChatFileHeaderField(path, "emotes", map[string]any{"bttv": []int{2}}); err != nil {
		t.Fatal(err)
	}
	if _, twice := readHeaderFieldDoc(t, path); twice != once {
		t.Errorf("a second identical rewrite changed the bytes")
	}
}

// A field that sits after the messages array (an old layout) is replaced by
// one in front of it.
func TestSetChatFileHeaderFieldMovesATrailingField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.json")
	os.WriteFile(path, []byte(`{"platform":"twitch","messageCount":1,"messages":[{"id":"1","text":"a"}],"emotes":{"old":1}}`), 0o644)
	if err := SetChatFileHeaderField(path, "emotes", map[string]int{"new": 2}); err != nil {
		t.Fatal(err)
	}
	d, raw := readHeaderFieldDoc(t, path)
	if len(d.Messages) != 1 || strings.Contains(raw, "old") || !strings.HasSuffix(raw, "\n  ]\n}") {
		t.Errorf("unexpected result:\n%s", raw)
	}
}

func TestSetChatFileHeaderFieldEmptyAndNullMessages(t *testing.T) {
	for _, content := range []string{
		`{"platform":"twitch","messageCount":0,"messages":[]}`,
		`{"platform":"twitch","messageCount":0,"messages":null}`,
		`{"platform":"twitch","messageCount":0}`,
	} {
		path := filepath.Join(t.TempDir(), "chat.json")
		os.WriteFile(path, []byte(content), 0o644)
		if err := SetChatFileHeaderField(path, "emotes", map[string]int{"n": 1}); err != nil {
			t.Fatalf("%s: %v", content, err)
		}
		d, raw := readHeaderFieldDoc(t, path)
		if d.Messages == nil || len(d.Messages) != 0 || d.Emotes == nil {
			t.Errorf("%s -> %s", content, raw)
		}
		if err := AppendChatMessages(path, []headerFieldMsg{{ID: "1"}}, 1, nil); err != nil {
			t.Errorf("%s: append after rewrite: %v", content, err)
		}
		if d, _ := readHeaderFieldDoc(t, path); len(d.Messages) != 1 {
			t.Errorf("%s: append lost", content)
		}
	}
}

// A file that cannot be rewritten is left byte-identical, with no temp behind.
func TestSetChatFileHeaderFieldLeavesABadFileAlone(t *testing.T) {
	for name, content := range map[string]string{
		"cut mid-record":       `{"platform":"twitch","messages":[{"id":"1","text":"a"},{"id":"2","te`,
		"not an object":        `[1,2,3]`,
		"field after messages": `{"messages":[],"other":1}`,
		"trailing data":        `{"messages":[]} x`,
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "chat.json")
		os.WriteFile(path, []byte(content), 0o644)
		if err := SetChatFileHeaderField(path, "emotes", map[string]int{"n": 1}); err == nil {
			t.Errorf("%s: no error", name)
		}
		if b, _ := os.ReadFile(path); string(b) != content {
			t.Errorf("%s: file changed to %q", name, b)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 1 {
			t.Errorf("%s: %d files left in the directory, want 1", name, len(entries))
		}
	}
}
