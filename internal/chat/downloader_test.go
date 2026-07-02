package chat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// --- NewChatDownloader tests ---

func TestNewChatDownloaderDefaults(t *testing.T) {
	opts := ChatDownloaderOptions{
		VideoID:    "testVid123",
		VideoTitle: "Test Video",
		OutputFile: "/tmp/chat.json",
	}
	cd := NewChatDownloader(opts)
	if cd == nil {
		t.Fatal("expected non-nil ChatDownloader")
	}
	if cd.opts.VideoID != "testVid123" {
		t.Errorf("expected VideoID 'testVid123', got %q", cd.opts.VideoID)
	}
	if cd.opts.VideoTitle != "Test Video" {
		t.Errorf("expected VideoTitle 'Test Video', got %q", cd.opts.VideoTitle)
	}
}

func TestNewChatDownloaderResumeFileDefault(t *testing.T) {
	opts := ChatDownloaderOptions{
		VideoID:    "v1",
		OutputFile: "/tmp/chat.json",
	}
	cd := NewChatDownloader(opts)
	if cd.opts.ResumeFile != "/tmp/chat.json.resume.json" {
		t.Errorf("expected resume file '/tmp/chat.json.resume.json', got %q", cd.opts.ResumeFile)
	}
}

func TestNewChatDownloaderResumeFileCustom(t *testing.T) {
	opts := ChatDownloaderOptions{
		VideoID:    "v1",
		OutputFile: "/tmp/chat.json",
		ResumeFile: "/tmp/custom_resume.json",
	}
	cd := NewChatDownloader(opts)
	if cd.opts.ResumeFile != "/tmp/custom_resume.json" {
		t.Errorf("expected custom resume file, got %q", cd.opts.ResumeFile)
	}
}

func TestNewChatDownloaderDedupUsable(t *testing.T) {
	opts := ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json"}
	cd := NewChatDownloader(opts)

	// Verify dedup is usable immediately — can add without nil panic
	if !cd.dedup.Add("test_id") {
		t.Error("expected first Add to return true")
	}
	if !cd.dedup.Seen("test_id") {
		t.Error("expected dedup to be usable for dedup tracking")
	}
}

func TestNewChatDownloaderInitialContinuation(t *testing.T) {
	opts := ChatDownloaderOptions{
		VideoID:             "v1",
		OutputFile:          "/tmp/chat.json",
		InitialContinuation: "cont_token_abc",
	}
	cd := NewChatDownloader(opts)
	if cd.continuation != "cont_token_abc" {
		t.Errorf("expected continuation 'cont_token_abc', got %q", cd.continuation)
	}
}

func TestNewChatDownloaderStreamStartTimeParsing(t *testing.T) {
	tests := []struct {
		name          string
		startTime     string
		expectMs      int64
		expectNonZero bool
	}{
		{
			name:          "valid RFC3339",
			startTime:     "2024-01-15T10:30:00Z",
			expectNonZero: true,
		},
		{
			name:          "empty string",
			startTime:     "",
			expectMs:      0,
			expectNonZero: false,
		},
		{
			name:          "invalid format",
			startTime:     "not-a-date",
			expectMs:      0,
			expectNonZero: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := ChatDownloaderOptions{
				VideoID:         "v1",
				OutputFile:      "/tmp/chat.json",
				StreamStartTime: tt.startTime,
			}
			cd := NewChatDownloader(opts)
			if tt.expectNonZero && cd.streamStartMs == 0 {
				t.Error("expected non-zero streamStartMs")
			}
			if !tt.expectNonZero && cd.streamStartMs != 0 {
				t.Errorf("expected 0 streamStartMs, got %d", cd.streamStartMs)
			}
		})
	}
}

func TestNewChatDownloaderStreamStartTimeValue(t *testing.T) {
	startTime := "2024-01-15T10:30:00Z"
	parsedTime, _ := time.Parse(time.RFC3339, startTime)
	expectedMs := parsedTime.UnixMilli()

	opts := ChatDownloaderOptions{
		VideoID:         "v1",
		OutputFile:      "/tmp/chat.json",
		StreamStartTime: startTime,
	}
	cd := NewChatDownloader(opts)
	if cd.streamStartMs != expectedMs {
		t.Errorf("expected streamStartMs %d, got %d", expectedMs, cd.streamStartMs)
	}
}

// --- MessageCount tests ---

func TestMessageCountInitiallyZero(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json"})
	if cd.MessageCount() != 0 {
		t.Errorf("expected 0 messages, got %d", cd.MessageCount())
	}
}

func TestMessageCountConcurrentAccess(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json"})

	// Simulate concurrent increments from a "download goroutine"
	done := make(chan struct{})
	go func() {
		for range 100 {
			cd.mu.Lock()
			cd.messageCount++
			cd.mu.Unlock()
		}
		close(done)
	}()

	// Read count concurrently — should never panic or race
	for range 50 {
		_ = cd.MessageCount()
	}
	<-done

	if cd.MessageCount() != 100 {
		t.Errorf("expected 100 messages after concurrent increments, got %d", cd.MessageCount())
	}
}

// --- IsRunning tests ---

func TestIsRunningInitiallyFalse(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json"})
	if cd.IsRunning() {
		t.Error("expected IsRunning() == false initially")
	}
}

func TestIsRunningTransitionsViaStopMethod(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json"})

	// Simulate the running state that Start() would set
	cd.mu.Lock()
	cd.running = true
	cd.mu.Unlock()
	if !cd.IsRunning() {
		t.Error("expected IsRunning() == true when running")
	}

	// Stop should transition to not running
	cd.Stop()
	if cd.IsRunning() {
		t.Error("expected IsRunning() == false after Stop()")
	}
}

// --- SetOutputFile tests ---

func TestSetOutputFile(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:    "v1",
		OutputFile: "",
	})
	cd.SetOutputFile("/new/path/chat.json")
	if cd.opts.OutputFile != "/new/path/chat.json" {
		t.Errorf("expected output file '/new/path/chat.json', got %q", cd.opts.OutputFile)
	}
}

func TestSetOutputFileUpdatesResumeFile(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:    "v1",
		OutputFile: "",
	})
	// When OutputFile was empty, ResumeFile defaults to ".resume.json"
	cd.SetOutputFile("/new/path/chat.json")
	if cd.opts.ResumeFile != "/new/path/chat.json.resume.json" {
		t.Errorf("expected resume file '/new/path/chat.json.resume.json', got %q", cd.opts.ResumeFile)
	}
}

func TestSetOutputFilePreservesCustomResumeFile(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:    "v1",
		OutputFile: "/original/chat.json",
		ResumeFile: "/custom/resume.json",
	})
	cd.SetOutputFile("/new/path/chat.json")
	// Custom resume file should NOT be overwritten
	if cd.opts.ResumeFile != "/custom/resume.json" {
		t.Errorf("expected custom resume file preserved, got %q", cd.opts.ResumeFile)
	}
}

// --- Stop tests ---

func TestStopSetsFlags(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json"})
	cd.mu.Lock()
	cd.running = true
	cd.mu.Unlock()

	cd.Stop()

	cd.mu.Lock()
	running := cd.running
	cancelFlag := cd.cancelFlag
	cd.mu.Unlock()

	if running {
		t.Error("expected running == false after Stop()")
	}
	if !cancelFlag {
		t.Error("expected cancelFlag == true after Stop()")
	}
}

// --- MarkStreamEnded tests ---

func TestMarkStreamEndedSetsFlag(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json"})

	cd.MarkStreamEnded()

	cd.mu.Lock()
	ended := cd.streamEnded
	cd.mu.Unlock()

	if !ended {
		t.Error("expected streamEnded == true after MarkStreamEnded()")
	}
}

// --- dedup cull tests ---

func TestDedupTrimsToKeepSize(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json"})

	totalIDs := dedupKeepSize + 100
	for i := range totalIDs {
		cd.dedup.Add("msg_" + strconv.Itoa(i))
	}

	cd.dedup.Keep(dedupKeepSize)

	if cd.dedup.Len() != dedupKeepSize {
		t.Errorf("expected %d entries after cull, got %d", dedupKeepSize, cd.dedup.Len())
	}

	lastID := "msg_" + strconv.Itoa(totalIDs-1)
	if !cd.dedup.Seen(lastID) {
		t.Error("expected last inserted ID to be retained after cull")
	}

	firstID := "msg_0"
	if cd.dedup.Seen(firstID) {
		t.Error("expected first inserted ID to be removed after cull")
	}
}

func TestDedupNoopWhenBelowThreshold(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json"})

	for i := range 10 {
		cd.dedup.Add("msg_" + strconv.Itoa(i))
	}

	cd.dedup.Keep(dedupKeepSize)

	if cd.dedup.Len() != 10 {
		t.Errorf("expected 10 entries (no cull needed), got %d", cd.dedup.Len())
	}
}

// --- shouldStop tests ---

func TestShouldStopWhenNotRunning(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json"})
	// running defaults to false
	if !cd.shouldStop() {
		t.Error("expected shouldStop() == true when not running")
	}
}

func TestShouldStopWhenCancelFlag(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json"})
	cd.running = true
	cd.cancelFlag = true
	if !cd.shouldStop() {
		t.Error("expected shouldStop() == true when cancelFlag is set")
	}
}

func TestShouldStopWhenStreamEnded(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json"})
	cd.running = true
	cd.streamEnded = true
	if !cd.shouldStop() {
		t.Error("expected shouldStop() == true when streamEnded is set")
	}
}

func TestShouldStopFalseWhenRunning(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json"})
	cd.running = true
	if cd.shouldStop() {
		t.Error("expected shouldStop() == false when running and no flags set")
	}
}

// --- isStreamActive tests ---

func TestIsStreamActiveWhenLiveAndNotEnded(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:          "v1",
		OutputFile:       "/tmp/chat.json",
		IsLiveOrUpcoming: true,
	})
	if !cd.isStreamActive() {
		t.Error("expected isStreamActive() == true when IsLiveOrUpcoming and not ended")
	}
}

func TestIsStreamActiveFalseWhenEnded(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:          "v1",
		OutputFile:       "/tmp/chat.json",
		IsLiveOrUpcoming: true,
	})
	cd.streamEnded = true
	if cd.isStreamActive() {
		t.Error("expected isStreamActive() == false when streamEnded")
	}
}

func TestIsStreamActiveFalseWhenReplay(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:          "v1",
		OutputFile:       "/tmp/chat.json",
		IsLiveOrUpcoming: false,
	})
	if cd.isStreamActive() {
		t.Error("expected isStreamActive() == false for replay/VOD")
	}
}

// --- Callback fields tests ---

func TestCallbackFieldsAssignment(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json"})

	// Assign callbacks and verify they're callable
	startCalled := false
	progressCalled := false
	finishCalled := false
	errorCalled := false

	cd.OnStart = func(messageCount int, resuming bool) { startCalled = true }
	cd.SetOnProgress(func(p ChatProgress) { progressCalled = true })
	cd.OnFinish = func() { finishCalled = true }
	cd.OnError = func(err error) { errorCalled = true }

	cd.OnStart(0, false)
	cd.callOnProgress(ChatProgress{MessageCount: 10})
	cd.OnFinish()
	cd.OnError(nil)

	if !startCalled {
		t.Error("OnStart callback was not invoked")
	}
	if !progressCalled {
		t.Error("OnProgress callback was not invoked")
	}
	if !finishCalled {
		t.Error("OnFinish callback was not invoked")
	}
	if !errorCalled {
		t.Error("OnError callback was not invoked")
	}
}

// --- messageCount header accuracy tests ---

// makeTestMessage returns a minimal ChatMessage with the given ID.
func makeTestMessage(id string) ChatMessage {
	return ChatMessage{
		ID:            id,
		TimestampUsec: "1700000000000000",
		AuthorName:    "Test User",
		Message:       []MessagePart{{Type: "text", Text: "hello"}},
	}
}

// readChatFileHeader reads the chat.json at path and returns the parsed header fields.
func readChatFileHeader(t *testing.T, path string) ChatData {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readChatFileHeader: read file: %v", err)
	}
	var cd ChatData
	if err := json.Unmarshal(data, &cd); err != nil {
		t.Fatalf("readChatFileHeader: unmarshal: %v", err)
	}
	return cd
}

// TestMessageCountHeaderAccurateAfterIncrementalFlushes verifies that the
// messageCount field in the JSON header matches the actual number of messages
// after each incremental flush, simulating what runChatLoop does every ~1s.
//
// This is the regression test for the bug where messageCount was only updated
// on clean completion — leaving it stale after an interruption.
func TestMessageCountHeaderAccurateAfterIncrementalFlushes(t *testing.T) {
	dir := t.TempDir()
	outputFile := filepath.Join(dir, "chat.json")

	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:    "testVid",
		VideoTitle: "Test Stream",
		OutputFile: outputFile,
	})

	// --- Flush 1: first batch of 3 messages (not yet flushed to disk) ---
	batch1 := []ChatMessage{
		makeTestMessage("msg1"),
		makeTestMessage("msg2"),
		makeTestMessage("msg3"),
	}
	cd.messages = append(cd.messages, batch1...)
	cd.messageCount = len(batch1)

	cd.writeChatFile() // full write (flushedToDisk == false)
	cd.flushedToDisk = true
	cd.messages = nil

	// After the first full write updateChatFileHeader is called (mirrors runChatLoop)
	cd.updateChatFileHeader()

	got := readChatFileHeader(t, outputFile)
	if got.MessageCount != 3 {
		t.Errorf("after flush 1: want messageCount=3 in header, got %d", got.MessageCount)
	}
	if len(got.Messages) != 3 {
		t.Errorf("after flush 1: want 3 messages in array, got %d", len(got.Messages))
	}

	// --- Flush 2: incremental append of 2 more messages ---
	batch2 := []ChatMessage{
		makeTestMessage("msg4"),
		makeTestMessage("msg5"),
	}
	cd.messages = append(cd.messages, batch2...)
	cd.messageCount += len(batch2) // now 5

	cd.writeChatFile() // incremental append (flushedToDisk == true)
	cd.messages = nil

	// This is the call added by the fix — must update header after incremental flush
	cd.updateChatFileHeader()

	got = readChatFileHeader(t, outputFile)
	if got.MessageCount != 5 {
		t.Errorf("after flush 2: want messageCount=5 in header, got %d", got.MessageCount)
	}
	if len(got.Messages) != 5 {
		t.Errorf("after flush 2: want 5 messages in array, got %d", len(got.Messages))
	}

	// --- Flush 3: one more incremental append ---
	batch3 := []ChatMessage{
		makeTestMessage("msg6"),
	}
	cd.messages = append(cd.messages, batch3...)
	cd.messageCount += len(batch3) // now 6

	cd.writeChatFile()
	cd.messages = nil
	cd.updateChatFileHeader()

	got = readChatFileHeader(t, outputFile)
	if got.MessageCount != 6 {
		t.Errorf("after flush 3: want messageCount=6 in header, got %d", got.MessageCount)
	}
	if len(got.Messages) != 6 {
		t.Errorf("after flush 3: want 6 messages in array, got %d", len(got.Messages))
	}

	// Final invariant: header count always equals actual array length
	if got.MessageCount != len(got.Messages) {
		t.Errorf("header messageCount (%d) does not match actual message array length (%d)",
			got.MessageCount, len(got.Messages))
	}
}

// TestIncrementalFlushKeepsHeaderCountCurrent verifies that an incremental
// flush keeps the header messageCount in sync with the message array WITHOUT a
// separate updateChatFileHeader call: writeChatFile folds the header refresh
// into the append's open handle. (Previously the header went stale until a
// follow-up updateChatFileHeader; this pins the folded-update behavior.)
func TestIncrementalFlushKeepsHeaderCountCurrent(t *testing.T) {
	dir := t.TempDir()
	outputFile := filepath.Join(dir, "chat.json")

	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:    "testVid",
		VideoTitle: "Test Stream",
		OutputFile: outputFile,
	})

	// First full write with 2 messages (clean initial state).
	cd.messages = []ChatMessage{makeTestMessage("m1"), makeTestMessage("m2")}
	cd.messageCount = 2
	cd.writeChatFile()
	cd.flushedToDisk = true
	cd.messages = nil

	// Incremental flush of 3 more messages — NO separate updateChatFileHeader.
	cd.messages = []ChatMessage{
		makeTestMessage("m3"),
		makeTestMessage("m4"),
		makeTestMessage("m5"),
	}
	cd.messageCount += 3 // now 5
	cd.writeChatFile()   // appends to file AND folds the header count refresh
	cd.messages = nil

	got := readChatFileHeader(t, outputFile)
	// The folded append keeps the header count current with the array.
	if got.MessageCount != 5 {
		t.Errorf("expected header count kept current at 5 by the folded append, got %d", got.MessageCount)
	}
	if len(got.Messages) != 5 {
		t.Errorf("expected 5 messages in array, got %d", len(got.Messages))
	}
	if got.MessageCount != len(got.Messages) {
		t.Errorf("header count (%d) should match array length (%d)", got.MessageCount, len(got.Messages))
	}
}
