package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
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

// --- Part D: a restarted LIVE run polls the fresh continuation -----------

// withAllChatHeader attaches the viewSelector sub-menu extractAllChatContinuation
// reads, so a response can offer the unfiltered "Live Chat" upgrade token.
func withAllChatHeader(resp map[string]any, allChatToken string) map[string]any {
	cont := resp["continuationContents"].(map[string]any)
	liveChatCont := cont["liveChatContinuation"].(map[string]any)
	liveChatCont["header"] = map[string]any{
		"liveChatHeaderRenderer": map[string]any{
			"viewSelector": map[string]any{
				"sortFilterSubMenuRenderer": map[string]any{
					"subMenuItems": []any{
						map[string]any{"title": "Top chat"},
						map[string]any{
							"title": "Live chat",
							"continuation": map[string]any{
								"reloadContinuationData": map[string]any{"continuation": allChatToken},
							},
						},
					},
				},
			},
		},
	}
	return resp
}

// continuationRecorder answers every chat poll from a queue and records the
// continuation token each request actually carried — the only direct evidence
// of WHICH token the run polls with.
type continuationRecorder struct {
	mu        sync.Mutex
	tokens    []string
	responses []map[string]any
	t         *testing.T
}

func (h *continuationRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Continuation string `json:"continuation"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		h.t.Errorf("decode chat request body: %v", err)
	}
	h.mu.Lock()
	h.tokens = append(h.tokens, body.Continuation)
	i := len(h.tokens) - 1
	if i >= len(h.responses) {
		i = len(h.responses) - 1
	}
	out, _ := json.Marshal(h.responses[i])
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

// startAndRecordTokens runs cd.Start against a recorder and returns the
// continuation token of every poll the run made, in order.
func startAndRecordTokens(t *testing.T, cd *ChatDownloader, resps ...map[string]any) []string {
	t.Helper()
	rec := &continuationRecorder{responses: resps, t: t}
	server := httptest.NewServer(rec)
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
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]string(nil), rec.tokens...)
}

// seedResumeSidecar writes a resume sidecar for out with the given state.
func seedResumeSidecar(t *testing.T, out, videoID, continuation string, count int, ids []string) {
	t.Helper()
	store := utils.ResumeStore[ChatResumeState]{Path: out + ".resume.json"}
	if err := store.Save(ChatResumeState{
		MessageCount: count,
		Continuation: continuation,
		Timestamp:    time.Now().Unix(),
		VideoID:      videoID,
		RecentIDs:    ids,
	}); err != nil {
		t.Fatalf("seed resume sidecar: %v", err)
	}
}

// TestLiveResumePrefersFreshContinuation is the fix for the stale-token cost
// Part A's preservation introduced. The sidecar's continuation is BY
// DEFINITION the token the previous run left off at, and the exit Part A
// preserves it for is stale-continuation exhaustion — so the token in the
// sidecar is the expired one. A live/upcoming run whose caller has just
// fetched a fresh token from the watch page must poll THAT, and take only the
// count and dedup IDs from the sidecar.
//
// The fresh token is a Top Chat token (that is what the watch page serves), so
// the run must also still perform the All Chat upgrade — the second poll here
// proves it does.
func TestLiveResumePrefersFreshContinuation(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	seed := ChatData{
		VideoID:      "vidLiveResume",
		DownloadedAt: time.Now().UTC().Format(time.RFC3339),
		MessageCount: 2,
		Messages:     []ChatMessage{makeTestMessage("a"), makeTestMessage("b")},
	}
	if err := utils.WriteChatFileAtomic(out, &seed); err != nil {
		t.Fatalf("seed chat file: %v", err)
	}
	seedResumeSidecar(t, out, "vidLiveResume", "stale", 2, []string{"a", "b"})

	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "vidLiveResume",
		OutputFile:          out,
		InitialContinuation: "fresh",
		ApiKey:              "k",
		IsLiveOrUpcoming:    true,
	})
	cd.testRecoveryOverride = func(context.Context) bool { return false }

	tokens := startAndRecordTokens(t, cd,
		withAllChatHeader(chatResponseWithIDs(nil, "next"), "allchat"),
		chatResponseWithIDs(nil, ""),
	)

	if len(tokens) == 0 {
		t.Fatal("the run made no poll at all")
	}
	if tokens[0] != "fresh" {
		t.Errorf("first poll used continuation %q, want %q — a live run must keep the token the watch page just supplied, not the stale one in the sidecar",
			tokens[0], "fresh")
	}
	if len(tokens) < 2 || tokens[1] != "allchat" {
		t.Errorf("polls were %v, want the second to be the All Chat upgrade token — a fresh watch-page token is a Top Chat token and still needs the switch",
			tokens)
	}

	if cd.MessageCount() != 2 {
		t.Errorf("messageCount = %d, want 2 from the sidecar", cd.MessageCount())
	}
	for _, id := range []string{"a", "b"} {
		if !cd.dedup.Seen(id) {
			t.Errorf("dedup must still be restored from the sidecar's recentIds; %q is missing", id)
		}
	}
}

// TestReplayResumeKeepsSidecarContinuation is the sibling guard. For a replay
// the sidecar's continuation IS the position in the archive — a fresh
// watch-page token would restart the VOD from the top — so it always wins.
func TestReplayResumeKeepsSidecarContinuation(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	seed := ChatData{
		VideoID:      "vidReplayResume",
		DownloadedAt: time.Now().UTC().Format(time.RFC3339),
		MessageCount: 2,
		Messages:     []ChatMessage{makeTestMessage("a"), makeTestMessage("b")},
	}
	if err := utils.WriteChatFileAtomic(out, &seed); err != nil {
		t.Fatalf("seed chat file: %v", err)
	}
	seedResumeSidecar(t, out, "vidReplayResume", "stale", 2, []string{"a", "b"})

	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "vidReplayResume",
		OutputFile:          out,
		InitialContinuation: "fresh",
		ApiKey:              "k",
		IsReplay:            true,
		IsLiveOrUpcoming:    false,
	})

	tokens := startAndRecordTokens(t, cd, chatResponseWithIDs(nil, ""))

	if len(tokens) == 0 {
		t.Fatal("the run made no poll at all")
	}
	if tokens[0] != "stale" {
		t.Errorf("first poll used continuation %q, want %q — for a replay the sidecar's continuation is the position in the archive and must win",
			tokens[0], "stale")
	}
	if cd.MessageCount() != 2 {
		t.Errorf("messageCount = %d, want 2 from the sidecar", cd.MessageCount())
	}
}

// --- Fix round 1 ---------------------------------------------------------

// TestReplayStartDoesNotAdoptExistingFile gates adoption to the live/upcoming
// path. A replay/VOD run's continuation restarts the archive FROM THE TOP, and
// the loop culls the dedup to dedupKeepSize (5000) on its first successful
// fetch — so adoption on that path re-appends every message older than the
// retained window (the reviewer measured a 5002-message seed ending at 5004).
// The whole motivating bug is live/upcoming; the replay path must behave
// exactly as it did before this branch, i.e. start at zero and full-write.
func TestReplayStartDoesNotAdoptExistingFile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	seed := ChatData{
		VideoID:      "vidReplayNoAdopt",
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
		VideoID:             "vidReplayNoAdopt",
		OutputFile:          out,
		InitialContinuation: "tok0",
		ApiKey:              "k",
		IsReplay:            true,
		IsLiveOrUpcoming:    false,
	})
	var startCount int
	var startResuming bool
	cd.OnStart = func(messageCount int, resuming bool) { startCount, startResuming = messageCount, resuming }

	startWithScript(t, cd, chatResponseWithIDs([]string{"m4"}, ""))

	if startCount != 0 || startResuming {
		t.Errorf("OnStart(%d, %v), want (0, false) — a replay run must not adopt an existing chat file", startCount, startResuming)
	}
	got := readChatFileHeader(t, out)
	if len(got.Messages) != 1 || got.Messages[0].ID != "m4" || got.MessageCount != 1 {
		ids := make([]string, len(got.Messages))
		for i, m := range got.Messages {
			ids[i] = m.ID
		}
		t.Errorf("replay first flush must be a full write holding only the polled message, got %v (header count %d)", ids, got.MessageCount)
	}
}

// TestOnFinishFiresOncePerRun closes the reviewer's surviving mutant: nothing
// in either package pinned how many times OnFinish fires. Exactly once per run
// that reaches the end of Start. (It fires ZERO times when Start panics —
// Start's recovery defer returns before it — which is why the worker's restart
// flag is set from the run goroutine's own defer instead; see runEarlyChat.)
func TestOnFinishFiresOncePerRun(t *testing.T) {
	out := filepath.Join(t.TempDir(), "chat.json")
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "vidFinishOnce",
		OutputFile:          out,
		InitialContinuation: "tok0",
		ApiKey:              "k",
		IsReplay:            true,
		IsLiveOrUpcoming:    false,
	})
	finishes := 0
	cd.OnFinish = func() { finishes++ }

	startWithScript(t, cd, chatResponseWithIDs([]string{"m1"}, ""))

	if finishes != 1 {
		t.Errorf("OnFinish fired %d time(s), want exactly 1 per completed run", finishes)
	}
}
