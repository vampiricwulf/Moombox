package cipher

import (
	"context"
	"fmt"
	"regexp"
	"sync"
)

var stsRegex = regexp.MustCompile(`(?:signatureTimestamp|sts)\s*:\s*(\d+)`)

const stsCacheSize = 150

// stsInflight is an in-flight STS fetch; waiters read .sts / .err after .done
// closes.
type stsInflight struct {
	done chan struct{}
	sts  string
	err  error
}

// StsCache caches extracted signatureTimestamp values per player URL.
// Uses insertion-order tracking for FIFO eviction.
type StsCache struct {
	mu       sync.RWMutex
	cache    map[string]string
	order    []string // insertion order for FIFO eviction
	inflight map[string]*stsInflight
}

// NewStsCache creates a new STS cache.
func NewStsCache() *StsCache {
	return &StsCache{
		cache:    make(map[string]string),
		inflight: make(map[string]*stsInflight),
	}
}

// GetSts extracts the signatureTimestamp from a player JS file.
// Uses the provided player cache to fetch the JS. Concurrent callers for the
// same playerURL share a single underlying fetch+regex pass via the inflight
// map — otherwise a first-call-after-boot burst on the same channel could
// double-fetch the player and double-regex.
func (sc *StsCache) GetSts(ctx context.Context, playerCache *PlayerCache, playerURL string) (string, error) {
	key := CacheKey(playerURL)

	// Fast path: cache hit.
	sc.mu.RLock()
	sts, ok := sc.cache[key]
	sc.mu.RUnlock()
	if ok {
		return sts, nil
	}

	// Inflight dedup: if another goroutine is already fetching this player,
	// wait for its result instead of duplicating the work.
	sc.mu.Lock()
	if sts, ok := sc.cache[key]; ok { // TOCTOU guard after upgrade to write lock
		sc.mu.Unlock()
		return sts, nil
	}
	if entry, ok := sc.inflight[key]; ok {
		sc.mu.Unlock()
		select {
		case <-entry.done:
			return entry.sts, entry.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	entry := &stsInflight{done: make(chan struct{})}
	sc.inflight[key] = entry
	sc.mu.Unlock()

	// Fetch + extract outside the lock so other cache hits aren't blocked.
	sts, err := sc.fetchAndExtract(ctx, playerCache, playerURL)

	sc.mu.Lock()
	delete(sc.inflight, key)
	if err == nil {
		if _, exists := sc.cache[key]; !exists && len(sc.cache) >= stsCacheSize {
			// Evict the oldest entry (first in insertion order).
			evictKey := sc.order[0]
			sc.order = sc.order[1:]
			delete(sc.cache, evictKey)
		}
		if _, exists := sc.cache[key]; !exists {
			sc.order = append(sc.order, key)
		}
		sc.cache[key] = sts
	}
	sc.mu.Unlock()

	entry.sts = sts
	entry.err = err
	close(entry.done)
	return sts, err
}

// fetchAndExtract runs the cache miss path: download (via PlayerCache) +
// regex-scan for the signature timestamp.
func (sc *StsCache) fetchAndExtract(ctx context.Context, playerCache *PlayerCache, playerURL string) (string, error) {
	playerJS, err := playerCache.Fetch(ctx, playerURL)
	if err != nil {
		return "", fmt.Errorf("fetch player for STS: %w", err)
	}
	m := stsRegex.FindStringSubmatch(playerJS)
	if m == nil {
		return "", fmt.Errorf("signatureTimestamp not found in player script")
	}
	return m[1], nil
}

// InvalidateKey drops the cached STS for a single player. Called from
// Solver.InvalidateSolver so that re-fetching the player script also gets
// a fresh signatureTimestamp — a stale STS paired with a freshly compiled
// solver can produce a 403/loop cycle (see audit cipher.md C2).
func (sc *StsCache) InvalidateKey(key string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if _, ok := sc.cache[key]; !ok {
		return
	}
	delete(sc.cache, key)
	for i, k := range sc.order {
		if k == key {
			sc.order = append(sc.order[:i], sc.order[i+1:]...)
			break
		}
	}
}
