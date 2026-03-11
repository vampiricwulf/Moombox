package cipher

import (
	"context"
	"fmt"
	"regexp"
	"sync"
)

var stsRegex = regexp.MustCompile(`(?:signatureTimestamp|sts)\s*:\s*(\d+)`)

const stsCacheSize = 150

// StsCache caches extracted signatureTimestamp values per player URL.
// Uses insertion-order tracking for FIFO eviction.
type StsCache struct {
	mu    sync.RWMutex
	cache map[string]string
	order []string // insertion order for LRU eviction
}

// NewStsCache creates a new STS cache.
func NewStsCache() *StsCache {
	return &StsCache{
		cache: make(map[string]string),
	}
}

// GetSts extracts the signatureTimestamp from a player JS file.
// Uses the provided player cache to fetch the JS.
func (sc *StsCache) GetSts(ctx context.Context, playerCache *PlayerCache, playerURL string) (string, error) {
	key := CacheKey(playerURL)

	// Check cache
	sc.mu.RLock()
	sts, ok := sc.cache[key]
	sc.mu.RUnlock()
	if ok {
		return sts, nil
	}

	// Fetch player JS
	playerJS, err := playerCache.Fetch(ctx, playerURL)
	if err != nil {
		return "", fmt.Errorf("fetch player for STS: %w", err)
	}

	// Extract STS
	m := stsRegex.FindStringSubmatch(playerJS)
	if m == nil {
		return "", fmt.Errorf("signatureTimestamp not found in player script")
	}

	sts = m[1]

	// Cache with FIFO eviction — evict oldest entry when full
	sc.mu.Lock()
	_, exists := sc.cache[key]
	if !exists && len(sc.cache) >= stsCacheSize {
		// Evict the oldest entry (first in insertion order)
		evictKey := sc.order[0]
		sc.order = sc.order[1:]
		delete(sc.cache, evictKey)
	}
	sc.cache[key] = sts
	if !exists {
		sc.order = append(sc.order, key)
	}
	sc.mu.Unlock()

	return sts, nil
}

// Invalidate clears the STS cache.
func (sc *StsCache) Invalidate() {
	sc.mu.Lock()
	sc.cache = make(map[string]string)
	sc.order = nil
	sc.mu.Unlock()
}
