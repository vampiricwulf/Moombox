package cipher

import (
	"context"
	"fmt"
	"regexp"
	"sync"
)

// stsRegex anchors the match against an object-literal boundary (`,` or `{`
// before, `,` or `}` after) so a bare identifier inside a comment or string —
// or some unrelated `sts:` key in a CSS-shaped config — doesn't false-match.
// See audit cipher.md D3.
var stsRegex = regexp.MustCompile(`[,{]\s*(?:signatureTimestamp|sts)\s*:\s*(\d+)\s*[,}]`)

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
// double-fetch the player and double-regex. The shared pass belongs to none
// of them: a caller whose context ends leaves with that context's error, and
// the others still get the pass's answer.
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
	entry, ok := sc.inflight[key]
	if !ok {
		entry = &stsInflight{done: make(chan struct{})}
		sc.inflight[key] = entry
		// The fetch runs on its own goroutine, detached from the context of
		// the caller that started it: the callers sharing it come from
		// independent extractions, and a first caller cancelled mid-fetch
		// used to hand its context.Canceled to every one of them, whose
		// player requests then went out with no signatureTimestamp.
		// PlayerCache.Fetch bounds the detached fetch (about 94s).
		go sc.runInflight(context.WithoutCancel(ctx), entry, key, playerCache, playerURL)
	}
	sc.mu.Unlock()

	// Every caller, the one that started the fetch included, leaves on its
	// own context.
	select {
	case <-entry.done:
		return entry.sts, entry.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// runInflight fetches and extracts one player's STS for every caller waiting
// on entry, stores a success in the cache, and closes entry.done. ctx is
// never cancelled (GetSts detaches it).
func (sc *StsCache) runInflight(ctx context.Context, entry *stsInflight, key string, playerCache *PlayerCache, playerURL string) {
	var sts string
	var err error
	defer func() {
		if r := recover(); r != nil {
			sts, err = "", fmt.Errorf("fetch player for STS: panic: %v", r)
		}
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
	}()
	// Outside the lock, so other cache hits aren't blocked.
	sts, err = sc.fetchAndExtract(ctx, playerCache, playerURL)
}

// fetchAndExtract runs the cache miss path: download (via PlayerCache) +
// regex-scan for the signature timestamp.
func (sc *StsCache) fetchAndExtract(ctx context.Context, playerCache *PlayerCache, playerURL string) (string, error) {
	// STS extraction doesn't care about the changed flag: the StsCache is
	// keyed on player and gets explicitly invalidated by the cipher solver
	// (InvalidateKey, see InvalidateSolver). When Fetch returns changed=true
	// here, the upstream caller (compileSolver / PlayerJS) will have already
	// invalidated derivative state via dropSolverData.
	playerJS, _, err := playerCache.Fetch(ctx, playerURL)
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
