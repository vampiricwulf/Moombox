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

// TestStaleRecoveryDelayLadder pins the floor's shape. The FIRST recovery is
// not delayed — a genuinely expired mid-stream token must be replaced at once
// — and every consecutive one after it doubles from the live poll default to a
// 5-minute ceiling.
//
// Mutants this kills:
//   - a flat delay              → n=2 and n=4 both return 5s
//   - no ceiling                → n=10 returns 2560s
//   - the ladder starting at 0  → n=1 returns 0
func TestStaleRecoveryDelayLadder(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want time.Duration
	}{
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{4, 40 * time.Second},
		{10, 5 * time.Minute},
	} {
		if got := staleRecoveryDelay(tc.n); got != tc.want {
			t.Errorf("staleRecoveryDelay(%d) = %v, want %v", tc.n, got, tc.want)
		}
	}
}

// TestAdoptFreshContinuationHonoursTheReplayFlip is the other half of the row.
// A live broadcast that has just ended flips its watch page to isReplay:true
// and starts serving a REPLAY token; posting that to get_live_chat is exactly
// what makes the loop spin — the endpoint answers 200 with no continuation, so
// the loop recovers again, immediately, forever.
//
// Mutants this kills:
//   - the isReplay return still discarded          → isReplay() stays false
//   - a one-way latch (if isReplay { Store(true) }) → the second call never flips back
func TestAdoptFreshContinuationHonoursTheReplayFlip(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v", IsReplay: false, IsLiveOrUpcoming: true})
	if cd.isReplay() {
		t.Fatal("a live downloader started in replay mode")
	}

	cd.adoptFreshContinuation("tok-replay", true)
	if !cd.isReplay() {
		t.Error("the watch page's isReplay flip was discarded")
	}
	if cd.continuation != "tok-replay" {
		t.Errorf("continuation = %q, want the recovered token", cd.continuation)
	}

	// A page that is still live must not flip it back and forth for free.
	cd.adoptFreshContinuation("tok-live", false)
	if cd.isReplay() {
		t.Error("a live page did not flip the mode back")
	}
}

// TestRunChatLoopFloorsRepeatedStaleRecoveries is the reproduction, bounded.
// Against a watch page that keeps handing out a token the live endpoint
// reports complete, the loop used to re-fetch the ~5 MB page and re-poll with
// NO delay until Stop: 2,543 page fetches and 2,544 polls in 300 ms, measured.
//
// Mutants this kills:
//   - no floor at all                     → the whole run costs no measurable time
//   - the counter reset on every recovery → the loop never exits
//   - no consecutive cap                  → same
func TestRunChatLoopFloorsRepeatedStaleRecoveries(t *testing.T) {
	// Scale the ladder down so the test does not sleep for real minutes.
	origFloor, origCeil := liveChatPollDefaultForTesting, maxStaleRecoveryDelayForTesting
	liveChatPollDefaultForTesting, maxStaleRecoveryDelayForTesting = time.Millisecond, 4*time.Millisecond
	t.Cleanup(func() {
		liveChatPollDefaultForTesting, maxStaleRecoveryDelayForTesting = origFloor, origCeil
	})

	recoveries := 0
	cd := NewChatDownloader(ChatDownloaderOptions{VideoID: "v", IsLiveOrUpcoming: true, InitialContinuation: "tok"})
	cd.running = true
	cd.continuation = "tok"
	cd.testRecoveryOverride = func(context.Context) bool {
		recoveries++
		cd.continuation = "tok"
		return true
	}
	cd.testFetchOverride = func(context.Context) (*ChatApiResponse, error) {
		return &ChatApiResponse{IsComplete: true, TimeoutMs: -1}, nil
	}

	start := time.Now()
	done := make(chan struct{})
	go func() { defer close(done); cd.runChatLoop(context.Background(), false) }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cd.Stop()
		t.Fatal("runChatLoop never exited: the consecutive-recovery cap is missing")
	}
	elapsed := time.Since(start)

	// The ladder above sums to 35ms (1+2+4 then the 4ms ceiling); a run that
	// finishes in appreciably less than that slept nowhere, which is the
	// pre-change behaviour the storm measurement caught.
	if elapsed < 20*time.Millisecond {
		t.Errorf("%d recoveries took %v — the floor never sleeps", recoveries, elapsed)
	}
	if recoveries > maxStaleContinuationAttempts {
		t.Errorf("recoveries = %d, want at most maxStaleContinuationAttempts (%d)",
			recoveries, maxStaleContinuationAttempts)
	}
	if recoveries < 2 {
		t.Errorf("recoveries = %d — the first recovery must not be delayed away", recoveries)
	}
}

// TestStaleRecoveryCapRecordsAnIncompleteOutcome is the honesty half of the
// cap. Giving up after maxStaleContinuationAttempts consecutive recoveries
// leaves a LIVE broadcast whose chat this run stopped capturing: messages can
// still be missing, so Start must report it. The worker derives chat_status
// straight from that return (chatStatusForOutcome in
// internal/worker/orchestrator_chat.go: any non-nil outcome is "incomplete"),
// so a nil here is the difference between an honest badge and a truncated
// archive displayed as finished.
//
// Mutants this kills:
//   - Start returning nil at the cap  → the job row reads "finished"
//   - the cap breaking without arming the flag → same
func TestStaleRecoveryCapRecordsAnIncompleteOutcome(t *testing.T) {
	origFloor, origCeil := liveChatPollDefaultForTesting, maxStaleRecoveryDelayForTesting
	liveChatPollDefaultForTesting, maxStaleRecoveryDelayForTesting = time.Millisecond, 4*time.Millisecond
	t.Cleanup(func() {
		liveChatPollDefaultForTesting, maxStaleRecoveryDelayForTesting = origFloor, origCeil
	})

	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "vidCap",
		OutputFile:          filepath.Join(t.TempDir(), "chat.json"),
		IsLiveOrUpcoming:    true,
		InitialContinuation: "tok",
	})
	cd.testRecoveryOverride = func(context.Context) bool {
		cd.continuation = "tok"
		return true
	}
	cd.testFetchOverride = func(context.Context) (*ChatApiResponse, error) {
		return &ChatApiResponse{IsComplete: true, TimeoutMs: -1}, nil
	}

	err := cd.Start(context.Background())
	if err == nil {
		t.Fatal("Start returned nil after the stale-recovery cap gave up on a still-live " +
			"broadcast — the worker writes chat_status \"finished\" for a nil outcome")
	}
	if !errors.Is(err, errStaleRecoveryExhausted) {
		t.Errorf("Start = %v, want errStaleRecoveryExhausted", err)
	}
}

// TestInnerStaleRecoveryCapRecordsAnIncompleteOutcome is the INNER cap's half
// of the same honesty rule, and the Task 11 review's Important 1.
//
// There are two stale-continuation caps. The outer one — runChatLoop's
// staleRecoveries counter, pinned above — armed the verdict; the inner one,
// `recoverStaleContinuation` exhausting maxStaleContinuationAttempts tries at
// fetching a fresh token (~35 min of failed watch-page fetches), broke the
// loop with NO verdict at all. Start then returned nil and
// chatStatusForOutcome wrote chat_status "finished" over a live capture that
// had given up — the exact twin of the hole handleFetchError's budget closed
// with errChatFetchExhausted.
//
// The discrimination matters as much as the verdict: recoverStaleContinuation
// also returns false when the loop is asked to STOP (Stop / MarkStreamEnded /
// a cancelled context), and a bare setTerminalErr at the break would mislabel
// every normal end as incomplete. The second subtest is that control.
//
// Mutants this kill:
//   - the break left bare (no setTerminalErr) → the give-up subtest sees nil
//   - the guard dropped, i.e. every false return armed → the stop subtest
//     reports errStaleRecoveryExhausted for an ordinary shutdown
func TestInnerStaleRecoveryCapRecordsAnIncompleteOutcome(t *testing.T) {
	newCD := func(t *testing.T, name string) *ChatDownloader {
		t.Helper()
		return NewChatDownloader(ChatDownloaderOptions{
			VideoID:             name,
			OutputFile:          filepath.Join(t.TempDir(), "chat.json"),
			IsLiveOrUpcoming:    true,
			InitialContinuation: "tok",
		})
	}

	t.Run("exhausting the fresh-token budget is a give-up", func(t *testing.T) {
		cd := newCD(t, "vidInnerCap")
		// Exactly the inner cap's shape: recovery ran and could not produce a
		// token, while the broadcast is still live.
		cd.testRecoveryOverride = func(context.Context) bool { return false }
		cd.testFetchOverride = func(context.Context) (*ChatApiResponse, error) {
			return &ChatApiResponse{IsComplete: true, TimeoutMs: -1}, nil
		}

		err := cd.Start(context.Background())
		if err == nil {
			t.Fatal("Start returned nil after the fresh-continuation budget gave up on a still-live " +
				"broadcast — the worker writes chat_status \"finished\" for a nil outcome")
		}
		if !errors.Is(err, errStaleRecoveryExhausted) {
			t.Errorf("Start = %v, want errStaleRecoveryExhausted", err)
		}
	})

	t.Run("a stop during recovery is not", func(t *testing.T) {
		cd := newCD(t, "vidInnerStop")
		// recoverStaleContinuation's OTHER false: the loop was asked to stop
		// while it was retrying. That is an ordinary end, not a give-up.
		cd.testRecoveryOverride = func(context.Context) bool {
			cd.Stop()
			return false
		}
		cd.testFetchOverride = func(context.Context) (*ChatApiResponse, error) {
			return &ChatApiResponse{IsComplete: true, TimeoutMs: -1}, nil
		}

		if err := cd.Start(context.Background()); err != nil {
			t.Errorf("Start = %v, want nil — a stop mid-recovery is not a give-up", err)
		}
	})
}

// TestHealthyLiveCadenceUnchangedByTheFloor is the differential the ruling
// asks for: the floor must never make a HEALTHY live chat slower. The normal
// cadence is YouTube's own timeoutMs, and the fallback when it sends none is
// the live poll default — neither goes anywhere near the recovery ladder.
//
// The ladder's test seams are scaled UP here, not down, precisely so any leak
// is loud: a floor that reached the ordinary poll path would stretch the
// fixture's 100 ms gaps to 3 s.
//
// Mutants this kills:
//   - the floor's sleep hoisted out of the end-of-stream branch → the gaps blow past the bound
//   - computePollDelay's fallback wired to the ladder seam       → the 5 s assertion fails
//   - the fallback changed away from liveChatPollDefault          → same
func TestHealthyLiveCadenceUnchangedByTheFloor(t *testing.T) {
	origFloor, origCeil := liveChatPollDefaultForTesting, maxStaleRecoveryDelayForTesting
	liveChatPollDefaultForTesting, maxStaleRecoveryDelayForTesting = 3*time.Second, time.Minute
	t.Cleanup(func() {
		liveChatPollDefaultForTesting, maxStaleRecoveryDelayForTesting = origFloor, origCeil
	})

	// The pure half, on the package's own live-chat fixture: the delay is the
	// hint YouTube sent, and an absent hint falls back to the live default.
	api := &ChatAPI{}
	fixture, err := api.parseResponse(minimalChatResponse("tok", false))
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	live := NewChatDownloader(ChatDownloaderOptions{VideoID: "v", IsLiveOrUpcoming: true})
	if got := live.computePollDelay(fixture); got != 100*time.Millisecond {
		t.Errorf("computePollDelay(fixture) = %v, want the fixture's 100ms timeoutMs", got)
	}
	if got := live.computePollDelay(&ChatApiResponse{TimeoutMs: -1}); got != liveChatPollDefault {
		t.Errorf("computePollDelay(no hint) = %v, want liveChatPollDefault (%v)", got, liveChatPollDefault)
	}

	// The loop half: three healthy polls in a row, then one end-of-stream that
	// fails recovery and exits. Every gap is the fixture's own timeoutMs.
	handler := &timedHandler{responses: []map[string]any{
		minimalChatResponse("token1", false),
		minimalChatResponse("token2", false),
		minimalChatResponse("token3", false),
		minimalChatResponse("", true),
	}}
	server := httptest.NewServer(handler)
	defer server.Close()

	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "vidCadence",
		OutputFile:          filepath.Join(t.TempDir(), "chat.json"),
		IsLiveOrUpcoming:    true,
		InitialContinuation: "initial_token",
		ApiKey:              "test_key",
	})
	targetURL, _ := url.Parse(server.URL)
	cd.api.client = &http.Client{Transport: rewriteTransport{target: targetURL}}
	cd.testRecoveryOverride = func(context.Context) bool { return false }

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = cd.Start(ctx)

	at := handler.timestamps()
	if len(at) != 4 {
		t.Fatalf("the loop made %d polls, want 4 — the fixture no longer drives the healthy path", len(at))
	}
	for i := 1; i < len(at); i++ {
		gap := at[i].Sub(at[i-1])
		if gap < 95*time.Millisecond {
			t.Errorf("poll %d landed %v after poll %d — faster than YouTube's own timeoutMs", i+1, gap, i)
		}
		if gap > time.Second {
			t.Errorf("poll %d landed %v after poll %d — the recovery floor leaked into the healthy path",
				i+1, gap, i)
		}
	}
}

// timedHandler is scriptedHandler with a clock: it records when each poll
// arrived so a test can assert the loop's cadence rather than only its
// sequence.
type timedHandler struct {
	mu        sync.Mutex
	responses []map[string]any
	at        []time.Time
}

func (h *timedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.at = append(h.at, time.Now())
	i := len(h.at) - 1
	if i >= len(h.responses) {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	body, _ := json.Marshal(h.responses[i])
	w.Write(body)
}

func (h *timedHandler) timestamps() []time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Time(nil), h.at...)
}
