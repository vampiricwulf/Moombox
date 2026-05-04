package worker

import (
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// twitchHintTTL bounds how long a fresh-from-monitor TwitchStreamInfo stays
// usable. EnqueueJob fires the worker within milliseconds, so 60s is
// generous; the upper bound only matters as a leak guard if AddJob succeeded
// but EnqueueJob's signal got lost (which would be a separate bug anyway).
const twitchHintTTL = 60 * time.Second

type twitchHintEntry struct {
	info      *twitch.TwitchStreamInfo
	stashedAt time.Time
}

// twitchHintCache is a take-once map keyed by jobID, used by the monitor's
// OnStreamFound (and OnStreamRecover) callback to forward its already-fetched
// stream info to the worker's processTwitchLive. Eliminates a redundant
// GetStreamInfo call that exposed the worker to transient Twitch GQL flaps
// where StreamMetadata briefly returned Stream=nil between two consecutive
// requests for the same channel.
//
// Take-once semantics ensure the same hint can't accidentally be consumed by
// multiple processing attempts; user-driven Reinit always falls back to a
// fresh GetStreamInfo, which is the right behaviour at reinit time.
type twitchHintCache struct {
	mu      sync.Mutex
	entries map[string]twitchHintEntry
}

func newTwitchHintCache() *twitchHintCache {
	return &twitchHintCache{entries: map[string]twitchHintEntry{}}
}

// stash records a fresh TwitchStreamInfo for the given jobID. Safe to call
// on a nil receiver (test harnesses may not wire one up).
func (c *twitchHintCache) stash(jobID string, info *twitch.TwitchStreamInfo) {
	if c == nil || info == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]twitchHintEntry{}
	}
	c.entries[jobID] = twitchHintEntry{info: info, stashedAt: time.Now()}
}

// take consumes and returns the hint for jobID, or nil if absent or expired.
// Always removes the entry whether expired or fresh — take-once.
func (c *twitchHintCache) take(jobID string) *twitch.TwitchStreamInfo {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[jobID]
	if !ok {
		return nil
	}
	delete(c.entries, jobID)
	if time.Since(entry.stashedAt) > twitchHintTTL {
		return nil
	}
	return entry.info
}
