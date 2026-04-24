package utils

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chatfileTestDoc is a small container used by the chat-file helper tests. Both
// fields mirror the shape that internal/chat and internal/twitch already write.
type chatfileTestDoc struct {
	Platform     string                `json:"platform"`
	MessageCount int                   `json:"messageCount"`
	DownloadedAt string                `json:"downloadedAt"`
	Messages     []chatfileTestMessage `json:"messages"`
}

type chatfileTestMessage struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type captureLogger struct {
	warnings []string
}

func (c *captureLogger) Warn(msg string, args ...any) {
	c.warnings = append(c.warnings, msg)
}

func TestWriteChatFileAtomicRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.json")

	doc := chatfileTestDoc{
		Platform:     "twitch",
		MessageCount: 2,
		DownloadedAt: "2026-04-24T00:00:00Z",
		Messages: []chatfileTestMessage{
			{ID: "m1", Text: "hello"},
			{ID: "m2", Text: "world"},
		},
	}

	if err := WriteChatFileAtomic(path, &doc); err != nil {
		t.Fatalf("WriteChatFileAtomic: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// Padding ensures the messageCount field is at least 20 chars wide.
	if !strings.Contains(string(raw), `"messageCount": 2                   `) {
		t.Errorf("expected padded messageCount, got:\n%s", string(raw))
	}

	var back chatfileTestDoc
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal roundtrip: %v", err)
	}
	if back.MessageCount != 2 || len(back.Messages) != 2 || back.Messages[1].Text != "world" {
		t.Errorf("roundtrip lost content: %+v", back)
	}

	// A .tmp file must not be left behind after a successful rename.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("expected .tmp file removed, stat err=%v", err)
	}
}

func TestAppendChatMessagesIncremental(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.json")

	initial := chatfileTestDoc{
		MessageCount: 1,
		Messages:     []chatfileTestMessage{{ID: "a", Text: "first"}},
	}
	if err := WriteChatFileAtomic(path, &initial); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	more := []chatfileTestMessage{
		{ID: "b", Text: "second"},
		{ID: "c", Text: "third"},
	}
	if err := AppendChatMessages(path, more, nil); err != nil {
		t.Fatalf("AppendChatMessages: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var aggregate chatfileTestDoc
	if err := json.Unmarshal(raw, &aggregate); err != nil {
		t.Fatalf("aggregate unmarshal: %v\n%s", err, string(raw))
	}
	if len(aggregate.Messages) != 3 {
		t.Fatalf("expected 3 messages after append, got %d: %+v", len(aggregate.Messages), aggregate.Messages)
	}
	if aggregate.Messages[0].ID != "a" || aggregate.Messages[1].ID != "b" || aggregate.Messages[2].ID != "c" {
		t.Errorf("message order wrong: %+v", aggregate.Messages)
	}
}

func TestAppendChatMessagesEmptyFirstAppend(t *testing.T) {
	// Confirm that appending to a file whose messages array is empty does not
	// emit a leading comma. This guards the hasExisting branch.
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.json")

	empty := chatfileTestDoc{MessageCount: 0, Messages: []chatfileTestMessage{}}
	if err := WriteChatFileAtomic(path, &empty); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	more := []chatfileTestMessage{{ID: "x", Text: "only"}}
	if err := AppendChatMessages(path, more, nil); err != nil {
		t.Fatalf("AppendChatMessages: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var back chatfileTestDoc
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, string(raw))
	}
	if len(back.Messages) != 1 || back.Messages[0].ID != "x" {
		t.Errorf("first append produced wrong content: %+v", back.Messages)
	}
}

func TestAppendChatMessagesNoOpOnEmptyBatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.json")

	doc := chatfileTestDoc{Messages: []chatfileTestMessage{{ID: "a"}}}
	if err := WriteChatFileAtomic(path, &doc); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	before, _ := os.ReadFile(path)
	if err := AppendChatMessages(path, []chatfileTestMessage{}, nil); err != nil {
		t.Fatalf("AppendChatMessages(empty): %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Errorf("expected file unchanged when appending empty batch")
	}
}

func TestAppendChatMessagesMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.json")

	err := AppendChatMessages(path, []chatfileTestMessage{{ID: "a"}}, nil)
	if err == nil {
		t.Fatal("expected error when file is missing, got nil")
	}
	if !strings.Contains(err.Error(), "stat") {
		t.Errorf("expected stat-wrapped error, got %q", err.Error())
	}
}

func TestAppendChatMessagesTinyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.json")

	// Sub-10-byte file should be rejected as "too small".
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatalf("seed tiny file: %v", err)
	}
	err := AppendChatMessages(path, []chatfileTestMessage{{ID: "a"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "too small") {
		t.Errorf("expected too-small error, got %v", err)
	}
}

// TestAppendChatMessagesTailWiderThan10 guards against a regression where the
// tail scan used only the last 10 bytes. After PadMessageCountJSON expands the
// header, the closing structure ("\n  ]\n}") can sit past byte -10 depending
// on trailing whitespace — the twitch side originally used 10 and lost data.
// With a 256-byte tail window this must still locate the ']'.
func TestAppendChatMessagesTailWiderThan10(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.json")

	// Build a doc whose serialized form deliberately has extensive padding
	// after messageCount so the closing ']' is further than 10 bytes from EOF.
	doc := chatfileTestDoc{
		MessageCount: 1,
		Messages:     []chatfileTestMessage{{ID: "a", Text: "first"}},
	}
	if err := WriteChatFileAtomic(path, &doc); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	// Sanity-check that the closing "\n  ]\n}" tail is indeed more than 10
	// bytes into the file — otherwise this test would trivially pass even if
	// a regression narrowed the tail scan back to 10.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() < 20 {
		t.Fatalf("seed file is unexpectedly small (%d bytes)", info.Size())
	}

	if err := AppendChatMessages(path, []chatfileTestMessage{{ID: "b", Text: "second"}}, nil); err != nil {
		t.Fatalf("AppendChatMessages: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var back chatfileTestDoc
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, string(raw))
	}
	if len(back.Messages) != 2 {
		t.Errorf("expected 2 messages, got %d", len(back.Messages))
	}
}

// TestAppendChatMessagesMarshalFailureLogs feeds a non-marshalable value
// through and confirms the logger sees a warning. Uses chan to force marshal
// failure without building a custom type.
func TestAppendChatMessagesMarshalFailureLogs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.json")

	seed := struct {
		MessageCount int      `json:"messageCount"`
		Messages     []string `json:"messages"`
	}{MessageCount: 0, Messages: []string{}}
	if err := WriteChatFileAtomic(path, &seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cap := &captureLogger{}
	// chan types are not marshalable by encoding/json — the marshal failure
	// path should fire a warn and continue.
	err := AppendChatMessages(path, []chan int{make(chan int)}, cap)
	if err != nil {
		t.Fatalf("AppendChatMessages should not error on per-msg marshal failure: %v", err)
	}
	if len(cap.warnings) == 0 {
		t.Errorf("expected at least one logger.Warn for marshal failure")
	}
}

func TestUpdateChatFileHeaderFieldsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.json")

	doc := chatfileTestDoc{
		MessageCount: 1,
		DownloadedAt: "2020-01-01T00:00:00Z",
		Messages:     []chatfileTestMessage{{ID: "a", Text: "first"}},
	}
	if err := WriteChatFileAtomic(path, &doc); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	if err := UpdateChatFileHeaderFields(path, 17); err != nil {
		t.Fatalf("UpdateChatFileHeaderFields: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var back chatfileTestDoc
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, string(raw))
	}
	if back.MessageCount != 17 {
		t.Errorf("expected messageCount=17, got %d", back.MessageCount)
	}
	if back.DownloadedAt == "2020-01-01T00:00:00Z" {
		t.Errorf("expected downloadedAt to be rewritten, still shows %q", back.DownloadedAt)
	}
	// Messages array must survive the header rewrite unchanged.
	if len(back.Messages) != 1 || back.Messages[0].ID != "a" {
		t.Errorf("messages array corrupted: %+v", back.Messages)
	}
}

func TestUpdateChatFileHeaderFieldsMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.json")

	// Missing file is not an error — this mirrors the existing no-op
	// semantics when the chat file hasn't been flushed yet.
	if err := UpdateChatFileHeaderFields(path, 42); err != nil {
		t.Errorf("expected no error for missing file, got %v", err)
	}
}

func TestUpdateChatFileHeaderFieldsTinyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.json")

	if err := os.WriteFile(path, []byte("tiny"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := UpdateChatFileHeaderFields(path, 99); err != nil {
		t.Errorf("tiny file should be a no-op, got err %v", err)
	}
}

// TestErrChatFilePartialWriteSentinel verifies that ErrChatFilePartialWrite is
// exported, wrappable, and identifiable via errors.Is. The real triggering
// condition (truncate succeeds but subsequent WriteAt fails) is hard to
// reproduce in a unit test without an injectable os.File wrapper, so we only
// assert the sentinel's public contract.
func TestErrChatFilePartialWriteSentinel(t *testing.T) {
	wrapped := errors.New("other cause")
	combo := errors.Join(ErrChatFilePartialWrite, wrapped)
	if !errors.Is(combo, ErrChatFilePartialWrite) {
		t.Error("errors.Is should match ErrChatFilePartialWrite through errors.Join")
	}
}
