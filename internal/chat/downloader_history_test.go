package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// --- shared harness ------------------------------------------------------
//
// Every test in this file drives the REAL runChatLoop through Start against
// an httptest server (the same rewriteTransport + scriptedHandler pair
// downloader_livestate_test.go uses). Nothing here touches the network.

// chatResponseWithIDs builds a minimal live-chat API response carrying one
// addChatItemAction per id, so a test can control exactly which message IDs a
// poll returns (minimalChatResponse hardcodes a single "msg_1"). An empty
// nextContinuation omits the continuations array, which parseResponse reads as
// IsComplete — the end-of-stream / stale-continuation branch.
func chatResponseWithIDs(ids []string, nextContinuation string) map[string]any {
	actions := make([]any, 0, len(ids))
	for _, id := range ids {
		actions = append(actions, map[string]any{
			"addChatItemAction": map[string]any{
				"item": map[string]any{
					"liveChatTextMessageRenderer": map[string]any{
						"id":            id,
						"message":       map[string]any{"runs": []any{map[string]any{"text": "hi"}}},
						"authorName":    map[string]any{"simpleText": "User"},
						"timestampUsec": "1700000000000000",
					},
				},
			},
		})
	}
	liveChatCont := map[string]any{"actions": actions}
	if nextContinuation != "" {
		liveChatCont["continuations"] = []any{
			map[string]any{
				"timedContinuationData": map[string]any{
					"continuation": nextContinuation,
					"timeoutMs":    float64(10),
				},
			},
		}
	}
	return map[string]any{
		"continuationContents": map[string]any{"liveChatContinuation": liveChatCont},
	}
}

// startWithScript points cd's HTTP client at a test server serving resps in
// order and runs Start to completion.
func startWithScript(t *testing.T, cd *ChatDownloader, resps ...map[string]any) {
	t.Helper()
	handler := &scriptedHandler{t: t, responses: resps, errors: make([]bool, len(resps))}
	server := httptest.NewServer(handler)
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	cd.api.client = &http.Client{Transport: rewriteTransport{target: target}}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := cd.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("test context expired during Start — the run took a cancellation path, not the one under test")
	}
}

// readSidecar reads the resume sidecar at path. ok is false when it is absent.
func readSidecar(t *testing.T, path string) (state ChatResumeState, ok bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ChatResumeState{}, false
		}
		t.Fatalf("read resume sidecar: %v", err)
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("unmarshal resume sidecar: %v", err)
	}
	return state, true
}

// --- Part A: the sidecar is cleared only on a genuine completion ---------

// TestStaleExitKeepsResumeSidecar is the regression test for the reported
// wipe. A waiting-room chat that YouTube resets after inactivity comes back
// as IsComplete with no continuation; recoverStaleContinuation then fails for
// the whole ~50-minute budget and the run leaves. That is NOT the stream
// ending, and the sidecar is the ONLY thing that tells the next run chat.json
// already holds history — clearing it made the next run start at count 0 and
// full-write over the archive on its first message.
func TestStaleExitKeepsResumeSidecar(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "vidStale",
		OutputFile:          out,
		InitialContinuation: "tok0",
		ApiKey:              "k",
		IsLiveOrUpcoming:    true,
	})
	cd.testRecoveryOverride = func(context.Context) bool { return false } // stale exhaustion

	startWithScript(t, cd, chatResponseWithIDs([]string{"m1"}, ""))

	state, ok := readSidecar(t, out+".resume.json")
	if !ok {
		t.Fatal("a live/upcoming run that exited on stale-continuation exhaustion must KEEP its resume sidecar — without it the next run starts at count 0 and overwrites chat.json")
	}
	if state.MessageCount != 1 {
		t.Errorf("sidecar messageCount = %d, want 1 (the count the run flushed)", state.MessageCount)
	}
	if state.Continuation == "" {
		t.Error("sidecar must carry the continuation the run left off at")
	}
	if state.VideoID != "vidStale" {
		t.Errorf("sidecar videoId = %q, want %q — a mismatched id is ignored on load", state.VideoID, "vidStale")
	}

	got := readChatFileHeader(t, out)
	if len(got.Messages) != 1 || got.Messages[0].ID != "m1" {
		t.Errorf("chat file must still hold the flushed message, got %d message(s)", len(got.Messages))
	}
}

// TestReplayCompletionClearsResumeSidecar pins the existing behaviour the
// Part A rule must not break: a replay/VOD run whose loop reaches the end of
// the archive IS a genuine completion, so its sidecar goes.
func TestReplayCompletionClearsResumeSidecar(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "vidReplay",
		OutputFile:          out,
		InitialContinuation: "tok0",
		ApiKey:              "k",
		IsReplay:            true,
		IsLiveOrUpcoming:    false,
	})

	startWithScript(t, cd, chatResponseWithIDs([]string{"m1"}, ""))

	if _, ok := readSidecar(t, out+".resume.json"); ok {
		t.Error("a replay run that finished its loop is a genuine completion — the resume sidecar must be cleared")
	}
	if got := readChatFileHeader(t, out); len(got.Messages) != 1 {
		t.Errorf("chat file should hold the one downloaded message, got %d", len(got.Messages))
	}
}

// TestMarkStreamEndedClearsResumeSidecar pins the other genuine completion:
// the orchestrator's own end verdict. MarkStreamEnded means the broadcast is
// over, so there is no next run to hand history to.
func TestMarkStreamEndedClearsResumeSidecar(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "vidEnded",
		OutputFile:          out,
		InitialContinuation: "tok0",
		ApiKey:              "k",
		IsLiveOrUpcoming:    true,
	})
	// The orchestrator marks the stream ended while the run is mid-poll; the
	// flush + saveResume for this batch still happens, then the loop leaves on
	// shouldStop().
	cd.SetOnProgress(func(ChatProgress) { cd.MarkStreamEnded() })

	startWithScript(t, cd, chatResponseWithIDs([]string{"m1"}, "tok1"))

	if _, ok := readSidecar(t, out+".resume.json"); ok {
		t.Error("MarkStreamEnded is a genuine completion — the resume sidecar must be cleared")
	}
}

// --- Part B: a chat file with no sidecar is adopted as history -----------

// TestStartAdoptsExistingChatFileWithoutSidecar is the other half of the
// reported wipe. Part A stops the sidecar from being deleted, but every
// install already in the field has a chat.json whose sidecar is long gone —
// and a resume-less Start used to initialise messageCount = 0 and
// flushedToDisk = false, so its very first flush took writeFullChatFile and
// replaced the archive. Start must instead adopt what is already on disk:
// count it, seed the dedup with its IDs, and append from there.
func TestStartAdoptsExistingChatFileWithoutSidecar(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	seed := ChatData{
		VideoID:      "vidAdopt",
		VideoTitle:   "Waiting room",
		DownloadedAt: time.Now().UTC().Format(time.RFC3339),
		MessageCount: 3,
		Messages:     []ChatMessage{makeTestMessage("m1"), makeTestMessage("m2"), makeTestMessage("m3")},
	}
	if err := utils.WriteChatFileAtomic(out, &seed); err != nil {
		t.Fatalf("seed chat file: %v", err)
	}
	if _, ok := readSidecar(t, out+".resume.json"); ok {
		t.Fatal("test setup: there must be NO resume sidecar")
	}

	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "vidAdopt",
		VideoTitle:          "Waiting room",
		OutputFile:          out,
		InitialContinuation: "tok0",
		ApiKey:              "k",
		IsLiveOrUpcoming:    true,
	})
	cd.testRecoveryOverride = func(context.Context) bool { return false }
	var startCount int
	var startResuming bool
	var startCalls int
	cd.OnStart = func(messageCount int, resuming bool) {
		startCalls++
		startCount, startResuming = messageCount, resuming
	}

	// m2 is already in the file (an overlapping poll); m4 is genuinely new.
	startWithScript(t, cd, chatResponseWithIDs([]string{"m2", "m4"}, ""))

	if startCalls != 1 {
		t.Fatalf("OnStart called %d times, want 1", startCalls)
	}
	if startCount != 3 || !startResuming {
		t.Errorf("OnStart(%d, %v), want (3, true) — the adopted count must be reported as a resume so the job row never drops",
			startCount, startResuming)
	}

	got := readChatFileHeader(t, out)
	if len(got.Messages) != 4 {
		ids := make([]string, len(got.Messages))
		for i, m := range got.Messages {
			ids[i] = m.ID
		}
		t.Fatalf("chat file holds %d message(s) %v, want the 3 adopted plus m4", len(got.Messages), ids)
	}
	for i, want := range []string{"m1", "m2", "m3", "m4"} {
		if got.Messages[i].ID != want {
			t.Errorf("message %d id = %q, want %q", i, got.Messages[i].ID, want)
		}
	}
	if got.MessageCount != 4 {
		t.Errorf("header messageCount = %d, want 4", got.MessageCount)
	}
}

// TestStartWithoutSidecarOrFileStartsFresh pins the unchanged path: nothing
// on disk means nothing to adopt, so the run starts at zero and its first
// flush is the ordinary full write.
func TestStartWithoutSidecarOrFileStartsFresh(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "vidFresh",
		OutputFile:          out,
		InitialContinuation: "tok0",
		ApiKey:              "k",
		IsLiveOrUpcoming:    true,
	})
	cd.testRecoveryOverride = func(context.Context) bool { return false }
	var startCount int
	var startResuming bool
	cd.OnStart = func(messageCount int, resuming bool) { startCount, startResuming = messageCount, resuming }
	var errs []error
	cd.OnError = func(err error) { errs = append(errs, err) }

	startWithScript(t, cd, chatResponseWithIDs([]string{"m1"}, ""))

	if startCount != 0 || startResuming {
		t.Errorf("OnStart(%d, %v), want (0, false)", startCount, startResuming)
	}
	if len(errs) != 0 {
		t.Errorf("a fresh start must not report an IO error, got %v", errs)
	}
	got := readChatFileHeader(t, out)
	if len(got.Messages) != 1 || got.MessageCount != 1 {
		t.Fatalf("want a full write holding exactly 1 message, got %d message(s) / header count %d",
			len(got.Messages), got.MessageCount)
	}
	// A full write stamps the whole header; the append path never would have
	// created the file at all (its stat would have failed into reportIOError).
	if got.VideoID != "vidFresh" {
		t.Errorf("header videoId = %q, want %q — the first write must be a complete full write", got.VideoID, "vidFresh")
	}
}

// TestUnparseableChatFileIsNotOverwritten covers the third case of the
// adoption rule. A chat.json that does not parse cannot be adopted, but it
// must not be destroyed either: the run reports the failure through OnError
// (which also latches ioErrorOccurred, so the sidecar survives) and moves the
// unreadable bytes aside to <chat.json>.corrupt before writing anything.
//
// Overwriting in place — the obvious alternative — throws away the only copy
// of something a human could still salvage. Refusing to write at all is worse
// again: a multi-hour waiting room would buffer its whole chat in memory and
// persist nothing.
func TestUnparseableChatFileIsNotOverwritten(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	garbage := []byte(`{"videoId":"vidGarbage","messages":[{"id":"m1"},{"id":"m2"` + "\n")
	if err := os.WriteFile(out, garbage, 0o644); err != nil {
		t.Fatalf("seed garbage: %v", err)
	}

	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "vidGarbage",
		OutputFile:          out,
		InitialContinuation: "tok0",
		ApiKey:              "k",
		IsLiveOrUpcoming:    true,
	})
	cd.testRecoveryOverride = func(context.Context) bool { return false }
	var errs []error
	cd.OnError = func(err error) { errs = append(errs, err) }

	startWithScript(t, cd, chatResponseWithIDs([]string{"new1"}, ""))

	if len(errs) == 0 {
		t.Fatal("an unparseable chat file must be reported through OnError, not swallowed")
	}

	preserved, err := os.ReadFile(out + ".corrupt")
	if err != nil {
		t.Fatalf("the unreadable bytes must be preserved next to the file: %v", err)
	}
	if string(preserved) != string(garbage) {
		t.Errorf("preserved copy = %q, want the original bytes %q", preserved, garbage)
	}

	got := readChatFileHeader(t, out)
	if len(got.Messages) != 1 || got.Messages[0].ID != "new1" {
		t.Errorf("the run must keep archiving after preserving the corrupt file, got %d message(s)", len(got.Messages))
	}
}
