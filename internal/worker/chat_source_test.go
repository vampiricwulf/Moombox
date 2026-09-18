package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestChatSourceForReusesAFreshPage is row #56. FetchWatchPage already
// extracts the chat continuation on EVERY call, and GetVideoInfo had just
// fetched the page — so both chat-start sites were paying for a second 1-5 MB
// authenticated page they did not need.
//
// Mutants this kills:
//   - the carried source ignored     → fetched == 1
//   - the freshness gate dropped     → the stale subtest also sees fetched == 0
//   - an empty continuation accepted → the empty subtest sees fetched == 0
//   - the source not consumed on use → the second-setup subtest sees fetched == 0
func TestChatSourceForReusesAFreshPage(t *testing.T) {
	fetched := 0
	orig := fetchWatchPageForChat
	fetchWatchPageForChat = func(context.Context, string, string) (*youtube.WatchPageResult, error) {
		fetched++
		return &youtube.WatchPageResult{
			Ytcfg:            youtube.DefaultYtcfg(),
			ChatContinuation: "from-a-second-page",
		}, nil
	}
	t.Cleanup(func() { fetchWatchPageForChat = orig })

	t.Run("fresh source is reused", func(t *testing.T) {
		fetched = 0
		info := &youtube.VideoInfo{Chat: youtube.ChatSource{
			FetchedAt:    time.Now(),
			Continuation: "carried",
			IsReplay:     true,
			VisitorData:  "vd",
		}}
		got, err := chatSourceFor(context.Background(), info, "vid", "")
		if err != nil {
			t.Fatalf("chatSourceFor: %v", err)
		}
		if fetched != 0 {
			t.Errorf("the watch page was fetched %d times although the info carried a fresh source", fetched)
		}
		if got.Continuation != "carried" || !got.IsReplay || got.VisitorData != "vd" {
			t.Errorf("got %+v, want the carried source", got)
		}
	})

	t.Run("the carried source is used only once", func(t *testing.T) {
		fetched = 0
		info := &youtube.VideoInfo{Chat: youtube.ChatSource{
			FetchedAt:    time.Now(),
			Continuation: "carried",
		}}
		if _, err := chatSourceFor(context.Background(), info, "vid", ""); err != nil {
			t.Fatalf("chatSourceFor: %v", err)
		}
		got, err := chatSourceFor(context.Background(), info, "vid", "")
		if err != nil {
			t.Fatalf("chatSourceFor (second): %v", err)
		}
		if fetched != 1 || got.Continuation != "from-a-second-page" {
			t.Errorf("fetched=%d continuation=%q, want the SECOND setup to fetch its own page", fetched, got.Continuation)
		}
	})

	t.Run("a stale source is re-fetched", func(t *testing.T) {
		fetched = 0
		info := &youtube.VideoInfo{Chat: youtube.ChatSource{
			FetchedAt:    time.Now().Add(-10 * time.Minute),
			Continuation: "carried",
		}}
		got, err := chatSourceFor(context.Background(), info, "vid", "")
		if err != nil {
			t.Fatalf("chatSourceFor: %v", err)
		}
		if fetched != 1 || got.Continuation != "from-a-second-page" {
			t.Errorf("fetched=%d continuation=%q, want one fetch and the fresh token", fetched, got.Continuation)
		}
	})

	t.Run("no continuation means no source", func(t *testing.T) {
		fetched = 0
		info := &youtube.VideoInfo{Chat: youtube.ChatSource{FetchedAt: time.Now()}}
		if _, err := chatSourceFor(context.Background(), info, "vid", ""); err != nil {
			t.Fatalf("chatSourceFor: %v", err)
		}
		if fetched != 1 {
			t.Errorf("fetched=%d, want 1 — an empty continuation is not a usable source", fetched)
		}
	})

	t.Run("a nil info still works", func(t *testing.T) {
		fetched = 0
		if _, err := chatSourceFor(context.Background(), nil, "vid", ""); err != nil {
			t.Fatalf("chatSourceFor(nil): %v", err)
		}
		if fetched != 1 {
			t.Errorf("fetched=%d, want 1", fetched)
		}
	})

	t.Run("a fetch failure is surfaced", func(t *testing.T) {
		boom := errors.New("network down")
		fetchWatchPageForChat = func(context.Context, string, string) (*youtube.WatchPageResult, error) {
			return nil, boom
		}
		if _, err := chatSourceFor(context.Background(), nil, "vid", ""); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the fetch error", err)
		}
	})
}
