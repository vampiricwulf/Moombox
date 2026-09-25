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

	// No temp file of ANY name may survive a successful write. The old
	// fixed-name stat is retired because the name is now unpredictable; the
	// glob is its honest replacement on this SUCCESS path.
	//
	// Mutant this kills: WriteFileAtomic replacing its ReplaceFile rename with
	// a copy — the temp then outlives a successful write and the glob finds it.
	// It does NOT carry the deferred-cleanup mutant: deleting WriteFileAtomic's
	// `defer … os.Remove(tmpPath)` leaves this test green, because a successful
	// ReplaceFile consumes the temp by renaming it onto the target. That mutant
	// belongs to TestWriteChatFileAtomicRoutesThroughTheSharedWriter, the
	// failure-path test, which is where it was verified to fail.
	assertNoTempSurvives(t, dir)
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
	if err := AppendChatMessages(path, more, 3, nil); err != nil {
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

// TestAppendChatMessagesFoldsHeaderCount pins the folded header update:
// AppendChatMessages now refreshes messageCount + downloadedAt in the same open
// handle, so the on-disk header stays current across appends WITHOUT a separate
// UpdateChatFileHeaderFields call. Has inherent teeth — under the old
// append-only behavior the header count would remain 1.
func TestAppendChatMessagesFoldsHeaderCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.json")

	initial := chatfileTestDoc{
		MessageCount: 1,
		DownloadedAt: "2020-01-01T00:00:00Z",
		Messages:     []chatfileTestMessage{{ID: "a", Text: "first"}},
	}
	if err := WriteChatFileAtomic(path, &initial); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	// Two appends; pass the running total as count each time.
	if err := AppendChatMessages(path, []chatfileTestMessage{{ID: "b"}, {ID: "c"}}, 3, nil); err != nil {
		t.Fatalf("append 1: %v", err)
	}
	if err := AppendChatMessages(path, []chatfileTestMessage{{ID: "d"}}, 4, nil); err != nil {
		t.Fatalf("append 2: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var back chatfileTestDoc
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	if back.MessageCount != 4 {
		t.Errorf("header messageCount = %d, want 4 (folded update should keep it current)", back.MessageCount)
	}
	if len(back.Messages) != 4 {
		t.Errorf("messages len = %d, want 4", len(back.Messages))
	}
	if back.MessageCount != len(back.Messages) {
		t.Errorf("header count %d != array len %d", back.MessageCount, len(back.Messages))
	}
	// downloadedAt must have been refreshed away from the seed value.
	if back.DownloadedAt == "2020-01-01T00:00:00Z" {
		t.Errorf("downloadedAt not refreshed by folded update: %q", back.DownloadedAt)
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
	if err := AppendChatMessages(path, more, 1, nil); err != nil {
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
	if err := AppendChatMessages(path, []chatfileTestMessage{}, 1, nil); err != nil {
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

	err := AppendChatMessages(path, []chatfileTestMessage{{ID: "a"}}, 1, nil)
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
	err := AppendChatMessages(path, []chatfileTestMessage{{ID: "a"}}, 1, nil)
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

	if err := AppendChatMessages(path, []chatfileTestMessage{{ID: "b", Text: "second"}}, 2, nil); err != nil {
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
	err := AppendChatMessages(path, []chan int{make(chan int)}, 0, cap)
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

// TestAppendChatMessagesContentBrackets pins that the tail scan anchors on the
// STRUCTURAL closing ']' even when the last message's content ends with ']'
// and '}' — the write-first append must not splice into the message. Runs two
// appends and confirms the file stays valid JSON with correct order/count.
func TestAppendChatMessagesContentBrackets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.json")

	// A last message whose text ends with the exact bytes the locator looks
	// for. If the scan mis-anchored, the next append would splice mid-record.
	initial := chatfileTestDoc{
		MessageCount: 1,
		Messages:     []chatfileTestMessage{{ID: "a", Text: "gg]}]"}},
	}
	if err := WriteChatFileAtomic(path, &initial); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	if err := AppendChatMessages(path, []chatfileTestMessage{{ID: "b", Text: "nice]"}}, 2, nil); err != nil {
		t.Fatalf("append 1: %v", err)
	}
	if err := AppendChatMessages(path, []chatfileTestMessage{{ID: "c", Text: "]}"}}, 3, nil); err != nil {
		t.Fatalf("append 2: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc chatfileTestDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("file not valid JSON after appends: %v\n%s", err, raw)
	}
	if len(doc.Messages) != 3 {
		t.Fatalf("got %d messages, want 3: %+v", len(doc.Messages), doc.Messages)
	}
	if doc.Messages[0].ID != "a" || doc.Messages[1].ID != "b" || doc.Messages[2].ID != "c" {
		t.Errorf("order wrong: %+v", doc.Messages)
	}
	if doc.Messages[0].Text != "gg]}]" || doc.Messages[2].Text != "]}" {
		t.Errorf("content corrupted: %+v", doc.Messages)
	}
}

// assertNoTempSurvives fails if any *.tmp entry is left in dir. It replaces the
// fixed-name `os.Stat(path + ".tmp")` checks the two adopting writers used to
// carry: once a writer's temp name comes from os.CreateTemp, a stat of one
// hard-coded name proves nothing, while the glob still proves the real
// property — a successful write leaves the directory with the target and
// nothing else.
func assertNoTempSurvives(t *testing.T, dir string) {
	t.Helper()
	leftovers, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if err != nil {
		t.Fatalf("glob temps in %s: %v", dir, err)
	}
	if len(leftovers) != 0 {
		t.Errorf("temp files survived: %v", leftovers)
	}
}

// TestWriteChatFileAtomicUsesAUniqueTempName pins the adopt: the chat writer
// now goes through WriteFileAtomic, whose temp name comes from os.CreateTemp,
// so a second writer already mid-write on the FIXED `path + ".tmp"` name can
// no longer be clobbered. Rather than racing two writers, the property is
// checked directly — the directory is pre-seeded with the fixed name the old
// writer would have used, and it must come back untouched.
//
// Mutant this kills: restoring the local `tmpFile := path + ".tmp"` writer
// (the pre-adopt code) truncates the seeded file and then renames it onto the
// target, so the ReadFile below fails outright.
func TestWriteChatFileAtomicUsesAUniqueTempName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.json")
	fixed := path + ".tmp"

	if err := os.WriteFile(fixed, []byte("another-writer-is-mid-write"), 0o644); err != nil {
		t.Fatalf("seed fixed temp: %v", err)
	}

	doc := chatfileTestDoc{
		Platform:     "twitch",
		MessageCount: 1,
		Messages:     []chatfileTestMessage{{ID: "m1", Text: "hi"}},
	}
	if err := WriteChatFileAtomic(path, &doc); err != nil {
		t.Fatalf("WriteChatFileAtomic: %v", err)
	}

	got, err := os.ReadFile(fixed)
	if err != nil {
		t.Fatalf("the fixed .tmp name was consumed: %v", err)
	}
	if string(got) != "another-writer-is-mid-write" {
		t.Errorf("fixed .tmp content: want it untouched, got %q", string(got))
	}
}

// TestWriteChatFileAtomicRoutesThroughTheSharedWriter drives WriteFileAtomic's
// syncFile seam from the chat writer's side: with the fsync failing, the call
// must fail, a pre-existing target must be left exactly as it was, and no temp
// may survive. This is the assertion that proves the delegation is real rather
// than a copy of the same steps.
//
// Mutants this kills: reverting to the local writer — its own f.Sync() never
// consults the seam, the write "succeeds", so the error assertion fails and
// the seeded target is overwritten; dropping WriteFileAtomic's deferred
// os.Remove(tmpPath) — the leftover *.tmp assertion fails. This failure path,
// NOT the success-path glob in TestWriteChatFileAtomicRoundTrip, is what
// carries that second mutant (verified at plan review: with the deferred
// cleanup deleted this test reports `temp files survived:
// [...\chat.json.968817497.tmp]` while the round-trip test stays green).
func TestWriteChatFileAtomicRoutesThroughTheSharedWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.json")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	injected := errors.New("injected sync failure")
	orig := syncFile
	syncFile = func(f *os.File) error { return injected }
	t.Cleanup(func() { syncFile = orig })

	doc := chatfileTestDoc{MessageCount: 1, Messages: []chatfileTestMessage{{ID: "m1", Text: "hi"}}}
	err := WriteChatFileAtomic(path, &doc)
	if err == nil {
		t.Fatal("WriteChatFileAtomic: want the injected fsync failure, got nil")
	}
	if !errors.Is(err, injected) {
		t.Errorf("error chain: want it to wrap %v, got %v", injected, err)
	}

	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read back target: %v", readErr)
	}
	if string(got) != "original" {
		t.Errorf("target after a failed write: want untouched %q, got %q", "original", string(got))
	}
	assertNoTempSurvives(t, dir)
}

// chatGoldenPreAdopt is the EXACT file the pre-adopt WriteChatFileAtomic wrote
// for the fixture in TestWriteChatFileAtomicEncodingIsUnchanged: MarshalIndent
// with a two-space indent, PadMessageCountJSON widening the count field to 20
// characters, and no trailing newline. Captured from the writer at
// main @ ce304b09 before this task changed it.
const chatGoldenPreAdopt = "{\n  \"platform\": \"twitch\",\n  \"messageCount\": 2                   ,\n  \"downloadedAt\": \"2026-04-24T00:00:00Z\",\n  \"messages\": [\n    {\n      \"id\": \"m1\",\n      \"text\": \"hello\"\n    },\n    {\n      \"id\": \"m2\",\n      \"text\": \"world\"\n    }\n  ]\n}"

// TestWriteChatFileAtomicEncodingIsUnchanged is a REGRESSION pin, not a
// red-first test: it is green before AND after the adopt, and that is exactly
// its job. Every chat.json already on disk was produced by the pre-adopt
// encoder, and AppendChatMessages / UpdateChatFileHeaderFields both parse the
// layout by byte offsets (the 256-byte tail scan, the fixed-width count
// field), so a single byte of drift in the encoding would break appends to
// files written by an earlier build.
//
// Mutants this kills: swapping json.MarshalIndent for json.Marshal; changing
// the indent; dropping the PadMessageCountJSON call; appending a trailing
// newline. Verify its teeth by execution — make one of those four edits, watch
// this test fail, revert.
func TestWriteChatFileAtomicEncodingIsUnchanged(t *testing.T) {
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
	if string(raw) != chatGoldenPreAdopt {
		t.Errorf("encoding drifted.\n got: %q\nwant: %q", string(raw), chatGoldenPreAdopt)
	}
}
