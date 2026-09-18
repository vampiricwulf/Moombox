package worker

import (
	"strings"
	"sync"
	"sync/atomic"
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

// twitchHintKey is the cache key a stash and a take must agree on: the Twitch
// channel login, case-folded and namespaced.
//
// The CHANNEL, not the job (sweep-2 Task 11 fix round 1, Important 3). Keyed
// by job ID the cache could only ever answer the monitor's own brand-new row —
// the one shape that has just fetched the info anyway — while the job that
// actually waits on a poll loop, a manually added `tw_manual_<login>_<ns>`
// row, could never match a producer's `tw_<streamID>` key. A broadcast is
// identified to every one of these paths by its channel, so that is the key.
// The namespace prefix keeps it from ever colliding with a job ID, which the
// cache was keyed by before.
func twitchHintKey(login string) string { return "login:" + strings.ToLower(login) }

// twitchHintCache is a take-once map keyed by twitchHintKey, used by the monitor's
// OnStreamFound (and OnStreamRecover) callback to forward its already-fetched
// stream info to whichever job is about to ask the same question about that
// channel — processTwitchLive, or a manually added job parked in
// waitForTwitchLive. Eliminates a redundant GetStreamInfo call that exposed
// the worker to transient Twitch GQL flaps where StreamMetadata briefly
// returned Stream=nil between two consecutive requests for the same channel.
//
// Take-once semantics ensure the same hint can't accidentally be consumed by
// multiple processing attempts; user-driven Reinit always falls back to a
// fresh GetStreamInfo, which is the right behaviour at reinit time.
type twitchHintCache struct {
	mu      sync.Mutex
	entries map[string]twitchHintEntry

	// Observability counters — atomic so reads from the stats endpoint don't
	// contend with stash/take. hits = take returned non-nil; misses = take
	// returned nil (no entry, or entry was expired).
	hits   atomic.Uint64
	misses atomic.Uint64
}

func newTwitchHintCache() *twitchHintCache {
	return &twitchHintCache{entries: map[string]twitchHintEntry{}}
}

// TwitchHintStats is the snapshot returned by Stats. Exported so it can
// round-trip cleanly through the JSON stats endpoint.
type TwitchHintStats struct {
	Hits   uint64 `json:"hits"`
	Misses uint64 `json:"misses"`
}

// Stats returns a snapshot of the cache's hit/miss counters. Safe to call
// on a nil receiver (returns zero values).
func (c *twitchHintCache) Stats() TwitchHintStats {
	if c == nil {
		return TwitchHintStats{}
	}
	return TwitchHintStats{Hits: c.hits.Load(), Misses: c.misses.Load()}
}

// stash records a fresh TwitchStreamInfo under key (a twitchHintKey). Safe to
// call on a nil receiver (test harnesses may not wire one up).
func (c *twitchHintCache) stash(key string, info *twitch.TwitchStreamInfo) {
	if c == nil || info == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]twitchHintEntry{}
	}
	c.entries[key] = twitchHintEntry{info: info, stashedAt: time.Now()}
}

// take consumes and returns the hint for key, or nil if absent or expired.
// Always removes the entry whether expired or fresh — take-once.
func (c *twitchHintCache) take(key string) *twitch.TwitchStreamInfo {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	entry, ok := c.entries[key]
	if !ok {
		c.mu.Unlock()
		c.misses.Add(1)
		return nil
	}
	delete(c.entries, key)
	c.mu.Unlock()
	if time.Since(entry.stashedAt) > twitchHintTTL {
		c.misses.Add(1)
		return nil
	}
	c.hits.Add(1)
	return entry.info
}
