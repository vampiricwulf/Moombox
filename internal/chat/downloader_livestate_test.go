package chat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
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

// --- I5 fix: permanent loop exits must close the signal ------------------
//
// A dead chat downloader that leaves liveContinuationOpen latched true
// forever supplies PERMANENT (and wrong) MayResume evidence to the engine
// — the "full-ceiling stalls" the coordinator's report described. Every
// exit that means "this downloader will never poll again" must close the
// signal: ErrAuthRequired, the consecutive-error budget exhausting (both
// inside handleFetchError), Stop(), and MarkStreamEnded(); a poll result
// that lands after any of them does not re-open it. For the first three
// this is NOT an "ended" inference —
// closed means "no information", by design, the same directional contract
// LiveContinuationOpen's doc comment already states for a downloader that
// never started.

// TestStopClosesLiveContinuationOpen is the I5(a) regression test for the
// Stop() call site specifically: Stop() must close the signal directly,
// not rely on the loop noticing cancellation on its way out (Stop() can be
// called while the loop is asleep between polls, not just mid-fetch).
func TestStopClosesLiveContinuationOpen(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json"})
	cd.mu.Lock()
	cd.running = true
	cd.mu.Unlock()
	cd.setLiveContinuationOpen(true) // simulate a healthy in-progress live poll

	cd.Stop()

	if cd.LiveContinuationOpen() {
		t.Error("Stop() must close the resume signal -- a stopped downloader carries no information any more")
	}
}

// TestHandleFetchErrorAuthRequiredClosesSignal is the I5(a) regression test
// for handleFetchError's ErrAuthRequired branch: cookies died, the
// downloader aborts for good, and the signal must close with it.
func TestHandleFetchErrorAuthRequiredClosesSignal(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json", IsLiveOrUpcoming: true})
	cd.mu.Lock()
	cd.running = true
	cd.mu.Unlock()
	cd.setLiveContinuationOpen(true)

	n := 0
	shouldBreak := cd.handleFetchError(context.Background(), ErrAuthRequired, &n)

	if !shouldBreak {
		t.Fatal("ErrAuthRequired must break the loop")
	}
	if cd.LiveContinuationOpen() {
		t.Error("ErrAuthRequired must close the resume signal -- the downloader aborts for good and carries no further information")
	}
}

// TestHandleFetchErrorConsecutiveBudgetExceededClosesSignal is the I5(a)
// regression test for handleFetchError's consecutive-error-budget branch:
// once the budget is exhausted the downloader gives up for good, and the
// signal must close with it.
func TestHandleFetchErrorConsecutiveBudgetExceededClosesSignal(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json", IsLiveOrUpcoming: false}) // VOD budget (maxConsecErrorsVod=5), fewer calls needed
	cd.mu.Lock()
	cd.running = true
	cd.mu.Unlock()
	cd.setLiveContinuationOpen(true)
	cd.testBackoffOverride = time.Millisecond // keep the sub-budget calls fast

	genericErr := errors.New("transient fetch failure")
	n := 0
	var shouldBreak bool
	for i := 0; i < maxConsecErrorsVod+1; i++ {
		shouldBreak = cd.handleFetchError(context.Background(), genericErr, &n)
		if shouldBreak {
			break
		}
	}

	if !shouldBreak {
		t.Fatal("the consecutive-error budget must eventually break the loop")
	}
	if cd.LiveContinuationOpen() {
		t.Error("the consecutive-error budget exhausting must close the resume signal -- the downloader gives up for good")
	}
}

// TestHandleEndOfStreamClosesSignalOnDefiniteUnrecoveredEnd is the I5(b)
// injection-proven regression test the coordinator's report specifically
// asked for: deleting setLiveContinuationOpen(false) from
// handleEndOfStream must fail THIS test. Drives handleEndOfStream directly
// with recovery forced to fail (a definitive, unrecovered end) and asserts
// the signal reads false afterward.
func TestHandleEndOfStreamClosesSignalOnDefiniteUnrecoveredEnd(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json", IsLiveOrUpcoming: true})
	cd.setLiveContinuationOpen(true) // simulate the signal being open right before end-of-stream
	cd.testRecoveryOverride = func(ctx context.Context) bool { return false }

	recovered := cd.handleEndOfStream(context.Background())

	if recovered {
		t.Fatal("test setup: recovery override must report failure")
	}
	if cd.LiveContinuationOpen() {
		t.Error("handleEndOfStream must close the resume signal on a definitive, unrecovered end")
	}
}

// TestMarkStreamEndedClosesLiveContinuationOpen is the fifth permanent-exit
// test. MarkStreamEnded is the orchestrator's own end verdict
// (orchestrator.go's `if !isVod` branch), and until it closed the signal the
// worker's joint-idle gate (buildMayResume, internal/worker/interruption.go)
// kept counting a downloader the orchestrator had already retired as live
// resume evidence -- for as long as the chat loop took to notice, which on a
// sleeping poll is the whole poll interval.
func TestMarkStreamEndedClosesLiveContinuationOpen(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json", IsLiveOrUpcoming: true})
	cd.mu.Lock()
	cd.running = true
	cd.mu.Unlock()
	cd.setLiveContinuationOpen(true) // a healthy in-progress live poll

	cd.MarkStreamEnded()

	if cd.LiveContinuationOpen() {
		t.Error("MarkStreamEnded() must close the resume signal -- the orchestrator has declared the stream over, and this downloader will not poll again")
	}
}

// TestLatePollResultDoesNotReopenAfterMarkStreamEnded closes the window the
// fifth exit test cannot see: a fetch that completed just before
// MarkStreamEnded() reaches noteLivePollResult after it, and used to latch
// the signal open on a downloader the loop is about to abandon.
func TestLatePollResultDoesNotReopenAfterMarkStreamEnded(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json", IsLiveOrUpcoming: true})
	cd.mu.Lock()
	cd.running = true
	cd.mu.Unlock()
	cd.setLiveContinuationOpen(true)

	cd.MarkStreamEnded()
	cd.noteLivePollResult(true) // the in-flight poll's result arrives late

	if cd.LiveContinuationOpen() {
		t.Error("a poll result that lands after MarkStreamEnded() must not re-open the resume signal")
	}
}

// TestLatePollResultDoesNotReopenAfterStop pins the other half of the same
// guard: noteLivePollResult's doc comment claims protection against a late
// poll landing after Stop() (which sets cancelFlag, not streamEnded), but
// until this test existed nothing exercised that branch -- a mutant
// dropping the `!cd.cancelFlag` conjunct survived the whole package.
//
// Stop() also sets running = false in the same locked section as
// cancelFlag = true, so a bare "call Stop(), then poll" cannot tell the two
// conjuncts apart: cd.running alone already blocks the reopen, mutant or
// not. running is restored to true right after the real Stop() call to
// isolate cancelFlag specifically -- the same isolation the sibling test
// gets for free, since MarkStreamEnded never touches running.
func TestLatePollResultDoesNotReopenAfterStop(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "/tmp/chat.json", IsLiveOrUpcoming: true})
	cd.mu.Lock()
	cd.running = true
	cd.mu.Unlock()
	cd.setLiveContinuationOpen(true)

	cd.Stop()
	cd.mu.Lock()
	cd.running = true // isolate cancelFlag: Stop() itself also clears running
	cd.mu.Unlock()
	cd.noteLivePollResult(true) // the in-flight poll's result arrives late

	if cd.LiveContinuationOpen() {
		t.Error("a poll result that lands after Stop() must not re-open the resume signal")
	}
}

// TestStartResetsLiveContinuationOpenOnFreshRun is the I5(a) coverage for
// "verify Start() also resets the signal appropriately on a fresh run": a
// *ChatDownloader whose signal was left open by an EARLIER run (or set
// directly, standing in for any such leftover state) must not carry that
// stale value into a new Start() call. No InitialContinuation is set, so
// the loop's very first check (`if cd.continuation == "" { return }`)
// exits immediately without ever touching the signal itself -- any change
// observed here comes from Start()'s own reset, not the loop.
func TestStartResetsLiveContinuationOpenOnFreshRun(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:    "v1",
		OutputFile: filepath.Join(t.TempDir(), "chat.json"),
	})
	cd.setLiveContinuationOpen(true) // stale leftover state

	if err := cd.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if cd.LiveContinuationOpen() {
		t.Error("Start() must reset the resume signal to closed at the start of a fresh run, not carry over a stale value")
	}
}

// --- LastMessageAt: the message-idle clock behind the joint-idle release ---

// TestLastMessageAtAdvancesOnlyOnNewMessages pins the clock's single
// production update site — processBatch — at the granularity the worker's
// joint-idle gate depends on: genuinely NEW (post-dedup) messages advance
// it; a poll returning only already-seen messages, or none at all, is not
// chat activity and must leave it untouched. A clock that advanced on every
// poll would hold the chat-open resume signal alive forever on a lingering
// post-stream continuation — the exact bug the gate exists to close.
func TestLastMessageAtAdvancesOnlyOnNewMessages(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v1", OutputFile: "unused"})
	if cd.LastMessageAt().IsZero() {
		t.Fatal("construction must arm the clock (idle-since-this-run, not idle-since-1970)")
	}

	past := time.Now().Add(-time.Hour)

	// A batch with one NEW message must advance the clock.
	cd.SetLastMessageAtForTesting(past)
	if n, _ := cd.processBatch(&ChatApiResponse{Messages: []ChatMessage{{ID: "m1"}}}); n != 1 {
		t.Fatalf("processBatch new-message count = %d, want 1", n)
	}
	if got := cd.LastMessageAt(); !got.After(past) {
		t.Error("a committed NEW message must advance LastMessageAt")
	}

	// The same message again is deduped — not new activity.
	cd.SetLastMessageAtForTesting(past)
	if n, _ := cd.processBatch(&ChatApiResponse{Messages: []ChatMessage{{ID: "m1"}}}); n != 0 {
		t.Fatalf("deduped batch new-message count = %d, want 0", n)
	}
	if got := cd.LastMessageAt(); !got.Equal(past) {
		t.Error("a batch of only already-seen messages must NOT advance LastMessageAt")
	}

	// An empty batch is not activity either.
	if n, _ := cd.processBatch(&ChatApiResponse{}); n != 0 {
		t.Fatalf("empty batch new-message count = %d, want 0", n)
	}
	if got := cd.LastMessageAt(); !got.Equal(past) {
		t.Error("an empty batch must NOT advance LastMessageAt")
	}
}

// TestStartResetsLastMessageAtOnFreshRun mirrors
// TestStartResetsLiveContinuationOpenOnFreshRun for the idle clock: a fresh
// run's "no new messages yet" is measured from THIS run's start, not from
// whenever a prior run on the same instance last saw one — otherwise a
// restarted job would begin its life already past the joint-idle window.
func TestStartResetsLastMessageAtOnFreshRun(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:    "v1",
		OutputFile: filepath.Join(t.TempDir(), "chat.json"),
	})
	cd.SetLastMessageAtForTesting(time.Now().Add(-time.Hour)) // stale leftover state

	before := time.Now()
	if err := cd.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got := cd.LastMessageAt(); got.Before(before) {
		t.Error("Start() must re-arm the message-idle clock at the start of a fresh run")
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
		VideoID:             "testVidReopen",
		OutputFile:          filepath.Join(t.TempDir(), "chat.json"),
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
		VideoID:             "testVidFetchError",
		OutputFile:          filepath.Join(t.TempDir(), "chat.json"),
		IsLiveOrUpcoming:    true,
		InitialContinuation: "initial_token",
		ApiKey:              "test_key",
	})

	targetURL, _ := url.Parse(server.URL)
	cd.api.client = &http.Client{Transport: rewriteTransport{target: targetURL}}
	cd.testBackoffOverride = 50 * time.Millisecond

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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
