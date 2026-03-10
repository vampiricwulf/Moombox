package chat

import (
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

func TestNewChatDownloaderSeenIDsUsable(t *testing.T) {
	opts := ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json"}
	cd := NewChatDownloader(opts)

	// Verify seenIDs is usable immediately — can add without nil panic
	cd.seenIDs["test_id"] = struct{}{}
	if _, exists := cd.seenIDs["test_id"]; !exists {
		t.Error("expected seenIDs to be usable for dedup tracking")
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
		for i := 0; i < 100; i++ {
			cd.mu.Lock()
			cd.messageCount++
			cd.mu.Unlock()
		}
		close(done)
	}()

	// Read count concurrently — should never panic or race
	for i := 0; i < 50; i++ {
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

// --- cullDedup tests ---

func TestCullDedupTrimsToKeepSize(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json"})

	// Fill seenIDs beyond dedupKeepSize
	totalIDs := dedupKeepSize + 100
	for i := 0; i < totalIDs; i++ {
		id := "msg_" + time.Now().Format("150405.000000") + "_" + string(rune('A'+i%26))
		// Use a simple scheme to avoid collisions
		id = "msg_" + strconv.Itoa(i)
		cd.seenIDs[id] = struct{}{}
		cd.seenOrder = append(cd.seenOrder, id)
	}

	cd.cullDedup()

	if len(cd.seenIDs) != dedupKeepSize {
		t.Errorf("expected %d seenIDs after cull, got %d", dedupKeepSize, len(cd.seenIDs))
	}
	if len(cd.seenOrder) != dedupKeepSize {
		t.Errorf("expected %d seenOrder after cull, got %d", dedupKeepSize, len(cd.seenOrder))
	}

	// The kept IDs should be the most recent ones
	lastID := "msg_" + strconv.Itoa(totalIDs-1)
	if _, exists := cd.seenIDs[lastID]; !exists {
		t.Error("expected last inserted ID to be retained after cull")
	}

	// The first IDs should be removed
	firstID := "msg_" + strconv.Itoa(0)
	if _, exists := cd.seenIDs[firstID]; exists {
		t.Error("expected first inserted ID to be removed after cull")
	}
}

func TestCullDedupNoopWhenBelowThreshold(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json"})

	for i := 0; i < 10; i++ {
		id := "msg_" + strconv.Itoa(i)
		cd.seenIDs[id] = struct{}{}
		cd.seenOrder = append(cd.seenOrder, id)
	}

	cd.cullDedup()

	if len(cd.seenIDs) != 10 {
		t.Errorf("expected 10 seenIDs (no cull needed), got %d", len(cd.seenIDs))
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
	cd.OnProgress = func(p ChatProgress) { progressCalled = true }
	cd.OnFinish = func() { finishCalled = true }
	cd.OnError = func(err error) { errorCalled = true }

	cd.OnStart(0, false)
	cd.OnProgress(ChatProgress{MessageCount: 10})
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

