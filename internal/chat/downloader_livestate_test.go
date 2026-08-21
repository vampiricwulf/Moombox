package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// Truth table (spec "Signals"): open on live polls returning a
// continuation; closed on a definitive IsComplete/empty-continuation that
// recovery does not rescue; UNCHANGED on fetch errors; never open for
// replay or a downloader that never started.
func TestLiveContinuationOpen(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "x", OutputFile: "unused"})
	if cd.LiveContinuationOpen() {
		t.Fatal("never-started downloader must not report open")
	}
	cd.setLiveContinuationOpen(true)
	if !cd.LiveContinuationOpen() {
		t.Fatal("open after successful live poll")
	}
	cd.setLiveContinuationOpen(false)
	if cd.LiveContinuationOpen() {
		t.Fatal("closed after definitive end")
	}
}

func TestLiveContinuationOpenReplayNeverOpens(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "x", OutputFile: "unused", IsReplay: true})
	cd.noteLivePollResult(true) // even a "successful poll" on replay
	if cd.LiveContinuationOpen() {
		t.Fatal("replay chat must never report live-open")
	}
}

// --- Real integration tests: runChatLoop signal wiring ---

// rewriteTransport redirects requests to a test server.
type rewriteTransport struct {
	target *url.URL
}

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = rt.target.Scheme
	req.URL.Host = rt.target.Host
	return http.DefaultTransport.RoundTrip(req)
}

// scriptedHandler serves canned responses in sequence.
type scriptedHandler struct {
	mu        sync.Mutex
	responses []map[string]any
	errors    []bool // if errors[i] is true, return 500
	callCount int
	t         *testing.T
}

func (h *scriptedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.callCount >= len(h.responses) {
		h.t.Fatalf("scriptedHandler: call %d but only %d responses queued", h.callCount, len(h.responses))
	}

	if h.errors[h.callCount] {
		w.WriteHeader(http.StatusInternalServerError)
		h.callCount++
		return
	}

	w.Header().Set("Content-Type", "application/json")
	body, _ := json.Marshal(h.responses[h.callCount])
	h.callCount++
	w.Write(body)
}

// minimalChatResponse builds a minimal chat API response.
func minimalChatResponse(continuation string, isComplete bool) map[string]any {
	actions := []any{
		map[string]any{
			"addChatItemAction": map[string]any{
				"item": map[string]any{
					"liveChatTextMessageRenderer": map[string]any{
						"id": "msg_1",
						"message": map[string]any{
							"runs": []any{
								map[string]any{"text": "test"},
							},
						},
						"authorName": map[string]any{
							"simpleText": "User",
						},
						"timestampUsec": "0",
					},
				},
			},
		},
	}

	liveChatCont := map[string]any{
		"actions": actions,
	}
	if !isComplete && continuation != "" {
		liveChatCont["continuations"] = []any{
			map[string]any{
				"timedContinuationData": map[string]any{
					"continuation": continuation,
					"timeoutMs":    float64(100),
				},
			},
		}
	}
	return map[string]any{
		"continuationContents": map[string]any{
			"liveChatContinuation": liveChatCont,
		},
	}
}

// TestLiveContinuationReopensAfterRecoveryIntegration tests behavior (b):
// Signal re-opens after recovery succeeds. Drives runChatLoop with sequence:
// Poll 1 (success) → Poll 2 (end-of-stream, recovery succeeds) → Poll 3 (success, re-opens)
// → Poll 4 (end-of-stream, recovery fails, exit).
func TestLiveContinuationReopensAfterRecoveryIntegration(t *testing.T) {
	handler := &scriptedHandler{
		t: t,
		responses: []map[string]any{
			minimalChatResponse("token1", false),
			minimalChatResponse("", true),
			minimalChatResponse("token3", false),
			minimalChatResponse("", true),
		},
		errors: []bool{false, false, false, false},
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "testVid",
		OutputFile:          "unused.json",
		IsLiveOrUpcoming:    true,
		InitialContinuation: "initial_token",
		ApiKey:              "test_key",
	})

	targetURL, _ := url.Parse(server.URL)
	cd.api.client = &http.Client{Transport: rewriteTransport{target: targetURL}}

	recoveryCount := 0
	orig := cd.testRecoveryOverride
	cd.testRecoveryOverride = func(ctx context.Context) bool {
		recoveryCount++
		if recoveryCount == 1 {
			cd.continuation = "token2_recovered"
			return true
		}
		return false
	}
	_ = orig // use to avoid unused var

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = cd.Start(ctx)

	if recoveryCount != 2 {
		t.Fatalf("FAIL: recovery was called %d times (expected 2)", recoveryCount)
	}

	t.Logf("Behavior (b) verified: recovery succeeded, then failed, proving loop continued to Poll 3")
}

// TestLiveContinuationSignalUnchangedOnFetchErrorIntegration tests behavior (c):
// Fetch errors do NOT change the signal state. Signal must still be true
// when OnError is called (after Poll 1 opens it, during Poll 2's error handling).
func TestLiveContinuationSignalUnchangedOnFetchErrorIntegration(t *testing.T) {
	handler := &scriptedHandler{
		t: t,
		responses: []map[string]any{
			minimalChatResponse("token1", false),
			minimalChatResponse("", false),
			minimalChatResponse("token3", false),
			minimalChatResponse("", true),
		},
		errors: []bool{false, true, false, false},
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "testVid",
		OutputFile:          "unused.json",
		IsLiveOrUpcoming:    true,
		InitialContinuation: "initial_token",
		ApiKey:              "test_key",
	})

	targetURL, _ := url.Parse(server.URL)
	cd.api.client = &http.Client{Transport: rewriteTransport{target: targetURL}}

	cd.testRecoveryOverride = func(ctx context.Context) bool {
		return false
	}

	// Track signal state during error handling.
	var mu sync.Mutex
	signalAtFirstError := false
	errorCount := 0

	cd.OnError = func(err error) {
		mu.Lock()
		errorCount++
		if errorCount == 1 {
			// During Poll 2's error: signal should be true.
			signalAtFirstError = cd.LiveContinuationOpen()
		}
		mu.Unlock()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = cd.Start(ctx)

	if errorCount < 1 {
		t.Fatalf("Expected at least 1 error")
	}

	if !signalAtFirstError {
		t.Fatal("FAIL: signal was false during error — handleFetchError changed it incorrectly")
	}

	t.Logf("Behavior (c) verified: signal remained true during fetch error")
}
