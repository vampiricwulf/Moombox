package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// archivePageSize is how many messages one synthetic replay page carries —
// YouTube's replay pages run 150-300, so 200 is representative.
const archivePageSize = 200

// buildArchiveMessage renders message i of a synthetic archive. Offsets are
// distinct and ascending (100ms apart), which is what makes the high-water
// mark exact: the replay stream is ordered by offset.
func buildArchiveMessage(i int) ChatMessage {
	return ChatMessage{
		ID:            fmt.Sprintf("m%06d", i),
		TimestampUsec: fmt.Sprintf("%d", int64(i)*100*1000),
		OffsetMs:      int64(i) * 100,
		HasOffset:     true,
		AuthorName:    "viewer",
		Message:       []MessagePart{{Type: "text", Text: "hello"}},
	}
}

// archivePage returns messages [from, from+archivePageSize) of an archive of
// total messages, plus the continuation for the page after it ("" at the end).
func archivePage(from, total int) *ChatApiResponse {
	end := min(from+archivePageSize, total)
	resp := &ChatApiResponse{TimeoutMs: -1}
	for i := from; i < end; i++ {
		resp.Messages = append(resp.Messages, buildArchiveMessage(i))
	}
	if end < total {
		resp.NextContinuation = fmt.Sprintf("replay-%d", end)
	} else {
		resp.IsComplete = true
	}
	return resp
}

// readChatFileMessages parses a written chat.json and reports its records and
// how many of them repeat an ID already seen earlier in the file.
func readChatFileMessages(t *testing.T, path string) (records, duplicates int) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read chat file: %v", err)
	}
	var data ChatData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("parse chat file: %v", err)
	}
	seen := make(map[string]struct{}, len(data.Messages))
	for _, m := range data.Messages {
		if _, ok := seen[m.ID]; ok {
			duplicates++
			continue
		}
		seen[m.ID] = struct{}{}
	}
	return len(data.Messages), duplicates
}

// TestReplayTraverseIsBoundedAndWritesNoDuplicates is the reviewer's
// replay-archive experiment, committed.
//
// The flip hands the run the watch page's RELOAD token — the START of the
// archive — so the pass re-covers ground the live half already captured, and
// the 5000-ID dedup window cannot span an archive bigger than itself. Before
// this round that produced 1,763 requests and 352,400 duplicate records in 2s
// on a 100k archive, because the archive's end was treated as a stale
// continuation and the same token was recovered again, forever.
//
// Here: a 20,000-message archive whose first 6,000 the live half already
// captured (so 1,000 of them are outside the culled ID window). The pass must
// traverse exactly once, write every message exactly once, and end the run as
// a completion — not a give-up.
//
// Mutants this kills:
//   - the archive's end recovers the reload token again → a second traverse (recoveries > 1)
//   - the high-water mark dropped                       → 6,000 duplicate records, not the
//     1,000 the culled window alone would explain: the window keeps evicting as the pass
//     re-serves the archive from the top, so it is ahead of the replay pass throughout
//     (measured, close-review Finding 13c)
//   - the end of a replay pass reported as a give-up    → Start returns non-nil
func TestReplayTraverseIsBoundedAndWritesNoDuplicates(t *testing.T) {
	const (
		archive  = 20000
		liveHalf = 6000
	)
	out := filepath.Join(t.TempDir(), "chat.json")
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "vidArchive",
		OutputFile:          out,
		IsLiveOrUpcoming:    true,
		InitialContinuation: "live-tok",
	})

	var (
		mu            sync.Mutex
		recoveries    int
		replayFetches int
		nextFrom      int
	)
	// The watch page has flipped to isReplay:true and hands back the reload
	// token, exactly as FetchFreshContinuation would.
	cd.testRecoveryOverride = func(context.Context) bool {
		mu.Lock()
		recoveries++
		mu.Unlock()
		cd.adoptFreshContinuation("replay-0", true)
		nextFrom = 0
		return true
	}
	livePolls := 0
	cd.testFetchOverride = func(context.Context) (*ChatApiResponse, error) {
		if !cd.isReplay() {
			livePolls++
			if livePolls == 1 {
				// The live half: everything the live endpoint served before
				// the broadcast ended.
				// TimeoutMs 1 only so the test does not spend the live
				// endpoint's real 5s fallback between its two live polls.
				resp := &ChatApiResponse{NextContinuation: "live-tok-2", TimeoutMs: 1}
				for i := range liveHalf {
					resp.Messages = append(resp.Messages, buildArchiveMessage(i))
				}
				return resp, nil
			}
			// ...and then the post-live 200-with-no-continuation.
			return &ChatApiResponse{IsComplete: true, TimeoutMs: -1}, nil
		}
		mu.Lock()
		replayFetches++
		over := replayFetches > 3*(archive/archivePageSize+1)
		mu.Unlock()
		if over {
			// A second traverse is running away: stop the loop so the
			// assertions below report it instead of the test hanging.
			cd.Stop()
			return &ChatApiResponse{IsComplete: true, TimeoutMs: -1}, nil
		}
		resp := archivePage(nextFrom, archive)
		nextFrom += archivePageSize
		return resp, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := cd.Start(ctx)

	if err != nil {
		t.Errorf("Start = %v, want nil — reaching the end of a replay archive is a completion, "+
			"not a give-up", err)
	}
	mu.Lock()
	gotRecoveries, gotFetches := recoveries, replayFetches
	mu.Unlock()
	if gotRecoveries != 1 {
		t.Errorf("recoveries = %d, want exactly 1 — the archive's end must never re-recover the "+
			"reload token, which re-pages the whole archive", gotRecoveries)
	}
	// One request per page of the archive, from offset 0 — the last page
	// carries the end marker, so there is no extra request for it.
	wantFetches := (archive + archivePageSize - 1) / archivePageSize
	if gotFetches != wantFetches {
		t.Errorf("replay requests = %d, want %d (one traverse from offset 0)", gotFetches, wantFetches)
	}
	records, duplicates := readChatFileMessages(t, out)
	t.Logf("ARCHIVE %d msgs (%d captured live): traverses=%d replay requests=%d records=%d duplicates=%d",
		archive, liveHalf, gotRecoveries, gotFetches, records, duplicates)
	if duplicates != 0 {
		t.Errorf("chat.json holds %d duplicate records — the high-water mark did not bound the "+
			"traverse (the 5000-ID window cannot span a %d-message archive)", duplicates, archive)
	}
	if records != archive {
		t.Errorf("chat.json holds %d records, want %d — one copy of every message", records, archive)
	}
}

// TestReplayHighWaterMarkIgnoresAFutureTimestamp is close-review Finding 2.
// The mark advances to the batch MAXIMUM, so ONE record carrying a
// far-future timestampUsec — a corrupt field, a clock-skewed server, an
// int64 sentinel — put the mark beyond every real message, and the replay
// pass the mark exists to bound then read the ENTIRE post-live tail as
// "below the mark" and dropped it. Silently: the row still reads `finished`.
//
// Chat timestamps are server-issued, so this is a robustness bound, not an
// attack surface: no chat message can be an hour in the future, so a batch
// whose maximum is past now+replayMarkFutureSlack never moves the mark. The
// record itself is still committed — the ID dedup covers it.
//
// Mutant this kills: the wallclock bound dropped from the advance in
// processBatch → tail captured 0 of 1400 on both rows.
func TestReplayHighWaterMarkIgnoresAFutureTimestamp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usec  int64
		total int
	}{
		{"an int64 sentinel", 9223372036854775807, 2000},
		{"two hours ahead of the wallclock", time.Now().Add(2 * time.Hour).UnixMicro(), 2000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const liveHalf = 600
			archive := tc.total
			out := filepath.Join(t.TempDir(), "chat.json")
			cd := NewChatDownloader(ChatDownloaderOptions{
				VideoID:             "vidPoison",
				OutputFile:          out,
				IsLiveOrUpcoming:    true,
				InitialContinuation: "live-tok",
			})

			var (
				mu       sync.Mutex
				fetches  int
				nextFrom int
			)
			cd.testRecoveryOverride = func(context.Context) bool {
				cd.adoptFreshContinuation("replay-0", true)
				nextFrom = 0
				return true
			}
			livePolls := 0
			cd.testFetchOverride = func(context.Context) (*ChatApiResponse, error) {
				if !cd.isReplay() {
					livePolls++
					if livePolls == 1 {
						resp := &ChatApiResponse{NextContinuation: "live-tok-2", TimeoutMs: 1}
						for i := range liveHalf {
							resp.Messages = append(resp.Messages, buildArchiveMessage(i))
						}
						// The poisoned record, served by the live endpoint
						// among perfectly ordinary ones.
						poisoned := buildArchiveMessage(liveHalf)
						poisoned.ID = "poisoned"
						poisoned.TimestampUsec = strconv.FormatInt(tc.usec, 10)
						resp.Messages = append(resp.Messages, poisoned)
						return resp, nil
					}
					return &ChatApiResponse{IsComplete: true, TimeoutMs: -1}, nil
				}
				mu.Lock()
				fetches++
				over := fetches > 3*(archive/archivePageSize+1)
				mu.Unlock()
				if over {
					cd.Stop()
					return &ChatApiResponse{IsComplete: true, TimeoutMs: -1}, nil
				}
				resp := archivePage(nextFrom, archive)
				nextFrom += archivePageSize
				return resp, nil
			}

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if err := cd.Start(ctx); err != nil {
				t.Fatalf("Start = %v, want nil", err)
			}

			raw, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("read chat file: %v", err)
			}
			var data ChatData
			if err := json.Unmarshal(raw, &data); err != nil {
				t.Fatalf("parse chat file: %v", err)
			}
			seen := make(map[string]struct{}, len(data.Messages))
			duplicates, tail := 0, 0
			for _, m := range data.Messages {
				if _, ok := seen[m.ID]; ok {
					duplicates++
					continue
				}
				seen[m.ID] = struct{}{}
				if n, err := strconv.Atoi(m.ID[1:]); err == nil && m.ID[0] == 'm' && n >= liveHalf {
					tail++
				}
			}
			t.Logf("records=%d duplicates=%d tail=%d/%d", len(data.Messages), duplicates, tail, archive-liveHalf)
			if tail != archive-liveHalf {
				t.Errorf("post-live tail captured %d of %d — one future timestamp moved the mark past every real message",
					tail, archive-liveHalf)
			}
			if duplicates != 0 {
				t.Errorf("chat.json holds %d duplicate records", duplicates)
			}
			if len(data.Messages) != archive+1 {
				t.Errorf("chat.json holds %d records, want %d (the archive plus the poisoned record)",
					len(data.Messages), archive+1)
			}
		})
	}
}

// TestReplayPagesDoNotResetTheStaleLadder pins the accounting half of the
// ruling: the ladder and its cap measure the LIVE endpoint's stale-recovery
// streak, so a replay page answering in between must not clear it.
//
// Production cannot reach a replay→live flip today (a replay pass never
// recovers, which is the other half of this round), so the flip back is driven
// from the fetch override. The rule is pinned anyway: a future caller that
// does flip back must not be able to clear a live streak for free, because
// that is exactly how the original storm evaded the cap.
//
// Mutants this kills:
//   - staleRecoveries reset on any successful poll → the second recovery is the
//     first again, so the ladder never sleeps and the run costs no time
func TestReplayPagesDoNotResetTheStaleLadder(t *testing.T) {
	origFloor, origCeil := liveChatPollDefaultForTesting, maxStaleRecoveryDelayForTesting
	liveChatPollDefaultForTesting, maxStaleRecoveryDelayForTesting = 200*time.Millisecond, 400*time.Millisecond
	t.Cleanup(func() {
		liveChatPollDefaultForTesting, maxStaleRecoveryDelayForTesting = origFloor, origCeil
	})

	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "vidLadder",
		IsLiveOrUpcoming:    true,
		InitialContinuation: "tok",
	})
	cd.running = true

	recoveries := 0
	cd.testRecoveryOverride = func(context.Context) bool {
		recoveries++
		switch recoveries {
		case 1:
			cd.adoptFreshContinuation("replay-0", true) // the page flipped to replay
			return true
		case 2:
			cd.continuation = "tok"
			return true
		}
		return false // third recovery gives up, ending the loop
	}
	fetches := 0
	cd.testFetchOverride = func(context.Context) (*ChatApiResponse, error) {
		fetches++
		if cd.isReplay() {
			if fetches == 2 {
				// One replay page that answers normally. TimeoutMs 1, not the
				// live default, so the only thing this run's elapsed time can
				// measure is the ladder.
				return &ChatApiResponse{NextContinuation: "replay-200", TimeoutMs: 1}, nil
			}
			// The page has gone live again (see the doc comment) — flipped
			// here, i.e. between polls, which is where a real flip lands.
			cd.adoptFreshContinuation("tok", false)
		}
		return &ChatApiResponse{IsComplete: true, TimeoutMs: -1}, nil
	}

	start := time.Now()
	done := make(chan struct{})
	go func() { defer close(done); cd.runChatLoop(context.Background(), false) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		cd.Stop()
		t.Fatal("runChatLoop never exited")
	}
	elapsed := time.Since(start)

	if recoveries != 3 {
		t.Fatalf("recoveries = %d, want 3 (live stale → replay page → live stale → give up)", recoveries)
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("the run took %v — the replay page cleared the live stale-recovery streak, so the "+
			"second recovery was floored as if it were the first", elapsed)
	}
}

// TestEarlyChatHandoffReportsTheStaleRecoveryExhaustion covers the path the
// honest outcome was written for. tryStartEarlyChat owns the FIRST Start and
// discards its return; the orchestrator then Starts the same instance again
// and records THAT return as the job's chat outcome. An already-running arm
// that answers nil drops the give-up on the floor and the row reads
// "finished" over a capture that stopped.
//
// Mutants this kills:
//   - the already-running arm returns nil → the early-chat handoff writes "finished"
func TestEarlyChatHandoffReportsTheStaleRecoveryExhaustion(t *testing.T) {
	origFloor, origCeil := liveChatPollDefaultForTesting, maxStaleRecoveryDelayForTesting
	liveChatPollDefaultForTesting, maxStaleRecoveryDelayForTesting = time.Millisecond, 4*time.Millisecond
	t.Cleanup(func() {
		liveChatPollDefaultForTesting, maxStaleRecoveryDelayForTesting = origFloor, origCeil
	})

	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "vidHandoff",
		OutputFile:          filepath.Join(t.TempDir(), "chat.json"),
		IsLiveOrUpcoming:    true,
		InitialContinuation: "tok",
	})
	gate := make(chan struct{})
	var once sync.Once
	cd.testRecoveryOverride = func(context.Context) bool {
		cd.continuation = "tok"
		return true
	}
	cd.testFetchOverride = func(context.Context) (*ChatApiResponse, error) {
		// Hold the first poll so the waiter below is guaranteed to arrive
		// while this run is still going.
		once.Do(func() { <-gate })
		return &ChatApiResponse{IsComplete: true, TimeoutMs: -1}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	earlyDone := make(chan struct{})
	go func() {
		defer close(earlyDone)
		defer func() { _ = recover() }()
		_ = cd.Start(ctx) // tryStartEarlyChat discards this return
	}()

	deadline := time.Now().Add(5 * time.Second)
	for !cd.IsRunning() {
		if time.Now().After(deadline) {
			close(gate)
			t.Fatal("the early-chat run never started")
		}
		time.Sleep(time.Millisecond)
	}

	waiterErr := make(chan error, 1)
	go func() {
		defer func() { _ = recover() }()
		waiterErr <- cd.Start(ctx) // the orchestrator's Start on the same instance
	}()
	// Give the waiter time to reach the already-running arm, then let the run
	// proceed to its cap.
	time.Sleep(20 * time.Millisecond)
	close(gate)

	select {
	case err := <-waiterErr:
		if !errors.Is(err, errStaleRecoveryExhausted) {
			t.Errorf("the waiting Start returned %v, want errStaleRecoveryExhausted — the early-chat "+
				"handoff is the path this verdict exists for, and the worker records THIS return", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the waiting Start never returned")
	}
	<-earlyDone
}

// TestFetchErrorExhaustionRecordsAnIncompleteOutcome applies the same honesty
// rule to the terminal exit two branches away: a live capture that burns its
// whole consecutive-error budget has stopped for good with the broadcast still
// running, so the row must not read "finished".
//
// Mutants this kills:
//   - the consecutive-error exhaustion leaves no verdict → Start returns nil
func TestFetchErrorExhaustionRecordsAnIncompleteOutcome(t *testing.T) {
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "vidErrors",
		OutputFile:          filepath.Join(t.TempDir(), "chat.json"),
		IsLiveOrUpcoming:    true,
		InitialContinuation: "tok",
	})
	cd.testBackoffOverride = time.Millisecond
	cd.testFetchOverride = func(context.Context) (*ChatApiResponse, error) {
		return nil, errors.New("chat API returned status 500")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := cd.Start(ctx)

	if !errors.Is(err, errChatFetchExhausted) {
		t.Errorf("Start = %v, want errChatFetchExhausted — a live capture that exhausted its "+
			"consecutive-error budget stopped short, and the worker writes \"finished\" for a nil "+
			"outcome", err)
	}
}

// liveTimestampUsec is the absolute wallclock (µs) of synthetic message i:
// one message per second from a fixed epoch. Both endpoints carry this same
// field verbatim, which is exactly why the high-water mark is keyed on it.
func liveTimestampUsec(startMs int64, i int) string {
	return strconv.FormatInt((startMs+int64(i)*1000)*1000, 10)
}

// skewMessage renders message i of the epoch-skew archive. A LIVE record
// arrives with no videoOffsetTimeMsec (withOffset false) — processBatch
// derives its offset from the run's epoch — while a REPLAY record carries
// YouTube's own offset, measured from the broadcast's ACTUAL start.
func skewMessage(startMs int64, i int, withOffset bool) ChatMessage {
	m := ChatMessage{
		ID:            fmt.Sprintf("s%05d", i),
		TimestampUsec: liveTimestampUsec(startMs, i),
		AuthorName:    "viewer",
		Message:       []MessagePart{{Type: "text", Text: "hi"}},
	}
	if withOffset {
		m.OffsetMs = int64(i) * 1000
		m.HasOffset = true
	}
	return m
}

// countIDsWithPrefix reports how many records in a written chat file carry an
// ID in [firstIdx, lastIdx] of the skew archive.
func countSkewRange(t *testing.T, path string, firstIdx, lastIdx int) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read chat file: %v", err)
	}
	var data ChatData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("parse chat file: %v", err)
	}
	found := 0
	for i := firstIdx; i <= lastIdx; i++ {
		want := fmt.Sprintf("s%05d", i)
		for _, m := range data.Messages {
			if m.ID == want {
				found++
				break
			}
		}
	}
	return found
}

// TestEarlyChatHandoffTailSurvivesTheScheduledEpochSkew is the leg the
// high-water mark exists for, on the path it was written for.
//
// tryStartEarlyChat gives the run the SCHEDULED start as its epoch and nothing
// ever corrects it (there is no epoch setter), so a live record's derived
// offset is measured from the schedule while the replay archive's offsets come
// from the broadcast's ACTUAL start. A stream that goes live five minutes late
// therefore leaves a mark five minutes too high, and the adopted pass drops
// the entire post-live tail it exists to recover — silently, with the row
// still reading "finished".
//
// The absolute TimestampUsec both endpoints carry is the only key no epoch
// enters.
//
// Mutants this kills:
//   - the mark keyed on the epoch-relative OffsetMs → 0 of 60 tail messages captured
func TestEarlyChatHandoffTailSurvivesTheScheduledEpochSkew(t *testing.T) {
	const (
		actualStartMs = int64(1700000000000) // 2023-11-14T22:13:20Z
		skewMs        = int64(300000)        // the broadcast went live 5 minutes late
		liveMsgs      = 100
		tailMsgs      = 60
	)
	out := filepath.Join(t.TempDir(), "chat.json")
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "vidSkew",
		OutputFile:          out,
		IsLiveOrUpcoming:    true,
		InitialContinuation: "live-tok",
		// Exactly what tryStartEarlyChat passes: the SCHEDULED start.
		StreamStartTime: time.UnixMilli(actualStartMs - skewMs).UTC().Format(time.RFC3339),
	})

	recoveries := 0
	cd.testRecoveryOverride = func(context.Context) bool {
		recoveries++
		cd.adoptFreshContinuation("replay-0", true)
		return true
	}
	livePolls, replayPolls := 0, 0
	cd.testFetchOverride = func(context.Context) (*ChatApiResponse, error) {
		if !cd.isReplay() {
			livePolls++
			if livePolls == 1 {
				resp := &ChatApiResponse{NextContinuation: "live-2", TimeoutMs: 1}
				for i := range liveMsgs {
					resp.Messages = append(resp.Messages, skewMessage(actualStartMs, i, false))
				}
				return resp, nil
			}
			return &ChatApiResponse{IsComplete: true, TimeoutMs: -1}, nil
		}
		replayPolls++
		resp := &ChatApiResponse{TimeoutMs: -1}
		switch replayPolls {
		case 1: // the archive's head — everything the live half already has
			for i := range liveMsgs {
				resp.Messages = append(resp.Messages, skewMessage(actualStartMs, i, true))
			}
			resp.NextContinuation = "replay-100"
		default: // the post-live tail, which is why the flip is worth making
			for i := liveMsgs; i < liveMsgs+tailMsgs; i++ {
				resp.Messages = append(resp.Messages, skewMessage(actualStartMs, i, true))
			}
			resp.IsComplete = true
		}
		return resp, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := cd.Start(ctx); err != nil {
		t.Fatalf("Start = %v, want nil", err)
	}

	tail := countSkewRange(t, out, liveMsgs, liveMsgs+tailMsgs-1)
	records, duplicates := readChatFileMessages(t, out)
	t.Logf("SKEW skew=%v live=%d tail=%d: records=%d duplicates=%d tailCaptured=%d",
		time.Duration(skewMs)*time.Millisecond, liveMsgs, tailMsgs, records, duplicates, tail)
	if tail != tailMsgs {
		t.Errorf("the post-live tail is %d of %d — the mark was measured against a different clock "+
			"than the archive's offsets, so the pass dropped the messages it exists to recover",
			tail, tailMsgs)
	}
	if duplicates != 0 {
		t.Errorf("chat.json holds %d duplicate records", duplicates)
	}
	if records != liveMsgs+tailMsgs {
		t.Errorf("chat.json holds %d records, want %d", records, liveMsgs+tailMsgs)
	}
	if recoveries != 1 {
		t.Errorf("recoveries = %d, want 1", recoveries)
	}
}

// TestReplayTraverseBoundsItselfWithNoStreamEpoch is the other leg. With no
// StreamStartTime (both chat construction sites gate on
// videoInfo.ScheduledStartTime being non-empty) a live record never gets an
// offset at all, so an offset-keyed mark is never set and the guard is inert:
// the whole live half comes back as duplicate records, which is precisely what
// the mark was added to prevent.
//
// Mutants this kills:
//   - the mark keyed on OffsetMs/HasOffset → 6,000 duplicates on a 20k archive
func TestReplayTraverseBoundsItselfWithNoStreamEpoch(t *testing.T) {
	const (
		archive  = 20000
		liveHalf = 6000
	)
	out := filepath.Join(t.TempDir(), "chat.json")
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:             "vidNoEpoch",
		OutputFile:          out,
		IsLiveOrUpcoming:    true,
		InitialContinuation: "live-tok",
		// StreamStartTime deliberately absent.
	})

	nextFrom := 0
	cd.testRecoveryOverride = func(context.Context) bool {
		cd.adoptFreshContinuation("replay-0", true)
		nextFrom = 0
		return true
	}
	livePolls, replayFetches := 0, 0
	cd.testFetchOverride = func(context.Context) (*ChatApiResponse, error) {
		if !cd.isReplay() {
			livePolls++
			if livePolls == 1 {
				resp := &ChatApiResponse{NextContinuation: "live-2", TimeoutMs: 1}
				for i := range liveHalf {
					// The live endpoint ships no videoOffsetTimeMsec, and with
					// no epoch nothing can derive one.
					m := buildArchiveMessage(i)
					m.OffsetMs, m.HasOffset = 0, false
					resp.Messages = append(resp.Messages, m)
				}
				return resp, nil
			}
			return &ChatApiResponse{IsComplete: true, TimeoutMs: -1}, nil
		}
		replayFetches++
		if replayFetches > 3*(archive/archivePageSize+1) {
			cd.Stop()
			return &ChatApiResponse{IsComplete: true, TimeoutMs: -1}, nil
		}
		resp := archivePage(nextFrom, archive)
		nextFrom += archivePageSize
		return resp, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := cd.Start(ctx); err != nil {
		t.Fatalf("Start = %v, want nil", err)
	}

	records, duplicates := readChatFileMessages(t, out)
	t.Logf("NOEPOCH archive=%d live=%d: replay requests=%d records=%d duplicates=%d",
		archive, liveHalf, replayFetches, records, duplicates)
	if duplicates != 0 {
		t.Errorf("chat.json holds %d duplicate records with no stream epoch — the mark was never "+
			"set, so the live half was re-appended in full", duplicates)
	}
	if records != archive {
		t.Errorf("chat.json holds %d records, want %d", records, archive)
	}
}

// TestReplayTieAtTheMarkKeepsRealMessages pins the strictly-below rule on the
// ordinary VOD path, which nothing pinned before: messages sharing one
// millisecond are routine on a busy stream, and a page boundary can land
// inside such a cluster. Dropping "at or below" the mark would discard the
// rest of that cluster on every boundary — real messages, silently. Ties fall
// through to the ID dedup instead, which covers them exactly.
//
// Mutants this kills:
//   - the comparison widened to <= → the 100 cluster members after the page
//     boundary are lost (500 records instead of 600)
func TestReplayTieAtTheMarkKeepsRealMessages(t *testing.T) {
	const (
		archive      = 600
		clusterFrom  = 300
		clusterUntil = 500 // [300, 500): straddles the 400-message page boundary
	)
	out := filepath.Join(t.TempDir(), "chat.json")
	cd := NewChatDownloader(ChatDownloaderOptions{
		VideoID:          "vidTie",
		OutputFile:       out,
		IsReplay:         true,
		IsLiveOrUpcoming: false,
	})
	cd.continuation = "replay-0"

	nextFrom := 0
	cd.testFetchOverride = func(context.Context) (*ChatApiResponse, error) {
		resp := archivePage(nextFrom, archive)
		for i := range resp.Messages {
			idx := nextFrom + i
			if idx >= clusterFrom && idx < clusterUntil {
				// One instant, 200 messages — the mark lands inside it. Both
				// fields come from the archive's own message at clusterFrom,
				// so the stream stays ordered in time either side of it.
				at := buildArchiveMessage(clusterFrom)
				resp.Messages[i].OffsetMs = at.OffsetMs
				resp.Messages[i].TimestampUsec = at.TimestampUsec
			}
		}
		nextFrom += archivePageSize
		return resp, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := cd.Start(ctx); err != nil {
		t.Fatalf("Start = %v, want nil", err)
	}

	records, duplicates := readChatFileMessages(t, out)
	t.Logf("TIE archive=%d cluster=[%d,%d): records=%d duplicates=%d",
		archive, clusterFrom, clusterUntil, records, duplicates)
	if records != archive {
		t.Errorf("chat.json holds %d records, want %d — a same-millisecond cluster straddling a "+
			"page boundary lost its tail to the high-water comparison", records, archive)
	}
	if duplicates != 0 {
		t.Errorf("chat.json holds %d duplicate records", duplicates)
	}
}

// TestAuthRefusalIsAGiveUp: ErrAuthRequired is a permanent loop exit, but it
// set no verdict, so Start returned nil and the worker wrote chat_status
// "finished" for a capture that stopped when the chat API refused the
// credentials — members-only cookies expiring mid-stream is the realistic
// trigger. (A replay run still clears its sidecar on this exit, by the
// documented completion rule; the next run adopts chat.json as history.)
//
// Mutant: no setTerminalErr in the ErrAuthRequired branch → Start returns nil.
func TestAuthRefusalIsAGiveUp(t *testing.T) {
	for _, live := range []bool{true, false} {
		t.Run(fmt.Sprintf("live=%v", live), func(t *testing.T) {
			cd := NewChatDownloader(ChatDownloaderOptions{
				VideoID:             "vidAuth",
				OutputFile:          filepath.Join(t.TempDir(), "chat.json"),
				IsLiveOrUpcoming:    live,
				IsReplay:            !live,
				InitialContinuation: "tok",
			})
			cd.testBackoffOverride = time.Millisecond
			calls := 0
			cd.testFetchOverride = func(context.Context) (*ChatApiResponse, error) {
				calls++
				if calls == 1 {
					return &ChatApiResponse{
						Messages:         []ChatMessage{{ID: "m1", TimestampUsec: "1700000000000000", AuthorName: "a", Message: []MessagePart{{Type: "text", Text: "hi"}}, OffsetMs: 1000, HasOffset: true}},
						NextContinuation: "tok2",
						TimeoutMs:        1,
					}, nil
				}
				return nil, fmt.Errorf("%w: status 401", ErrAuthRequired)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := cd.Start(ctx); !errors.Is(err, ErrAuthRequired) {
				t.Fatalf("Start = %v, want a give-up wrapping ErrAuthRequired", err)
			}
		})
	}
}
