package worker

import (
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/twitch"
)

func TestTwitchHintCacheStashAndTake(t *testing.T) {
	c := newTwitchHintCache()
	info := &twitch.TwitchStreamInfo{StreamID: "111", IsLive: true}

	c.stash("tw_111", info)

	got := c.take("tw_111")
	if got == nil {
		t.Fatal("take after stash: want non-nil")
	}
	if got.StreamID != "111" {
		t.Errorf("StreamID: want 111, got %q", got.StreamID)
	}

	// take again — should be empty (take-once)
	if got := c.take("tw_111"); got != nil {
		t.Errorf("second take: want nil, got %+v", got)
	}
}

func TestTwitchHintCacheTakeMissing(t *testing.T) {
	c := newTwitchHintCache()
	if got := c.take("tw_does_not_exist"); got != nil {
		t.Errorf("take of unknown jobID: want nil, got %+v", got)
	}
}

func TestTwitchHintCacheTTLEviction(t *testing.T) {
	c := newTwitchHintCache()
	info := &twitch.TwitchStreamInfo{StreamID: "222", IsLive: true}

	// Stash with a manual past timestamp older than the TTL
	c.mu.Lock()
	c.entries["tw_222"] = twitchHintEntry{
		info:      info,
		stashedAt: time.Now().Add(-2 * twitchHintTTL),
	}
	c.mu.Unlock()

	if got := c.take("tw_222"); got != nil {
		t.Errorf("take of expired entry: want nil, got %+v", got)
	}

	// Verify the expired entry was removed (no zombie)
	c.mu.Lock()
	_, exists := c.entries["tw_222"]
	c.mu.Unlock()
	if exists {
		t.Error("expired entry should be removed from cache after take")
	}
}

func TestTwitchHintCacheNilSafe(t *testing.T) {
	var c *twitchHintCache
	// nil receiver should be safe (no-op)
	c.stash("tw_nil", &twitch.TwitchStreamInfo{StreamID: "nil"})
	if got := c.take("tw_nil"); got != nil {
		t.Errorf("take on nil cache: want nil, got %+v", got)
	}
	if s := c.Stats(); s.Hits != 0 || s.Misses != 0 {
		t.Errorf("Stats on nil cache: want zero, got %+v", s)
	}
}

func TestTwitchHintCacheStatsCounter(t *testing.T) {
	c := newTwitchHintCache()
	info := &twitch.TwitchStreamInfo{StreamID: "stat", IsLive: true}

	// 1 hit: stash + take
	c.stash("tw_stat", info)
	if got := c.take("tw_stat"); got == nil {
		t.Fatal("first take after stash: want non-nil")
	}

	// 2 misses: take of unknown ID twice
	c.take("tw_unknown_a")
	c.take("tw_unknown_b")

	// 1 miss via expiry: backdate then take
	c.mu.Lock()
	c.entries["tw_expired"] = twitchHintEntry{
		info:      info,
		stashedAt: time.Now().Add(-2 * twitchHintTTL),
	}
	c.mu.Unlock()
	if got := c.take("tw_expired"); got != nil {
		t.Fatal("expired take: want nil")
	}

	// 1 more hit: fresh stash + take
	c.stash("tw_stat2", info)
	if got := c.take("tw_stat2"); got == nil {
		t.Fatal("second take after stash: want non-nil")
	}

	s := c.Stats()
	if s.Hits != 2 {
		t.Errorf("Hits: want 2, got %d", s.Hits)
	}
	if s.Misses != 3 {
		t.Errorf("Misses: want 3, got %d", s.Misses)
	}
}

func TestTwitchHintCacheOverwriteOnDoubleStash(t *testing.T) {
	c := newTwitchHintCache()
	first := &twitch.TwitchStreamInfo{StreamID: "v1", IsLive: true}
	second := &twitch.TwitchStreamInfo{StreamID: "v2", IsLive: true}

	c.stash("tw_overwrite", first)
	c.stash("tw_overwrite", second)

	got := c.take("tw_overwrite")
	if got == nil {
		t.Fatal("take after double stash: want non-nil")
	}
	if got.StreamID != "v2" {
		t.Errorf("StreamID: want v2 (last write wins), got %q", got.StreamID)
	}
}

// TestProcessorStashAndTake verifies a stashed hint short-circuits the
// GetStreamInfo call. We can't easily inject a fake Twitch service without
// substantially refactoring StreamProcessor, so this test focuses on the
// observable side-effect: after stashing and a single take, the cache is
// empty (take-once). The end-to-end path is covered by the integration
// scenario in Phase 10.
func TestProcessorStashAndTake(t *testing.T) {
	sp := &StreamProcessor{twitchHints: newTwitchHintCache()}
	info := &twitch.TwitchStreamInfo{StreamID: "abc", IsLive: true}

	sp.StashTwitchStreamInfo("tw_abc", info)

	got := sp.twitchHints.take("tw_abc")
	if got == nil || got.StreamID != "abc" {
		t.Fatalf("expected stashed info to be retrievable, got %+v", got)
	}

	// take-once: second take is empty
	if got := sp.twitchHints.take("tw_abc"); got != nil {
		t.Errorf("second take after stash: want nil, got %+v", got)
	}
}
