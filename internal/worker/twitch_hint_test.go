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
}
