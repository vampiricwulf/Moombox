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
type StsCache struct {
	mu    sync.RWMutex
	cache map[string]string
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

	// Cache (bounded to stsCacheSize, evict random entry if full)
	sc.mu.Lock()
	if len(sc.cache) >= stsCacheSize {
		for k := range sc.cache {
			delete(sc.cache, k)
			break
		}
	}
	sc.cache[key] = sts
	sc.mu.Unlock()

	return sts, nil
}

// Invalidate clears the STS cache.
func (sc *StsCache) Invalidate() {
	sc.mu.Lock()
	sc.cache = make(map[string]string)
	sc.mu.Unlock()
}
