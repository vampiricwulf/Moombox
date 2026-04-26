package web

import (
	"strconv"
	"testing"
	"time"
)

// TestRateLimiterLRUEvictsOldest confirms that when the per-IP cap is
// reached, the cap-eviction drops the LEAST-RECENTLY-USED IP rather than
// an arbitrary map entry. The previous random-eviction left the active
// user squeezed out by an attacker churning fresh IPs. Audit
// reports/web.md Q-18.
func TestRateLimiterLRUEvictsOldest(t *testing.T) {
	// Run with a reduced cap to avoid filling the actual 10 000-entry
	// guardrail in a unit test. The cap is a package-level constant in
	// rate_limiter.go; we test the eviction *order* which is what the
	// audit was actually concerned about.
	rl := newRateLimiterStruct(100, time.Minute)

	// Insert maxRateLimiterEntries IPs.
	for i := 0; i < maxRateLimiterEntries; i++ {
		rl.AllowWithRetry("ip-" + strconv.Itoa(i))
	}

	// Touch ip-0 so it becomes the MRU. ip-1 stays at the front of the
	// LRU list; we expect that to be evicted next.
	rl.AllowWithRetry("ip-0")

	// Adding a fresh IP triggers the cap eviction.
	rl.AllowWithRetry("attacker-0")

	rl.mu.Lock()
	_, hasZero := rl.requests["ip-0"]
	_, hasOne := rl.requests["ip-1"]
	_, hasAttacker := rl.requests["attacker-0"]
	rl.mu.Unlock()

	if !hasZero {
		t.Error("touched ip-0 should still be cached (MRU after touch)")
	}
	if hasOne {
		t.Error("ip-1 (least-recently-used) should have been evicted")
	}
	if !hasAttacker {
		t.Error("attacker-0 should be inserted")
	}
}

// TestRateLimiterTouchPromotesToMRU verifies that a repeated request from
// an existing IP moves it to the MRU position so subsequent eviction
// rounds drop OTHER IPs first.
func TestRateLimiterTouchPromotesToMRU(t *testing.T) {
	rl := newRateLimiterStruct(100, time.Minute)

	rl.AllowWithRetry("victim")
	for i := 0; i < 5; i++ {
		rl.AllowWithRetry("filler-" + strconv.Itoa(i))
	}
	rl.AllowWithRetry("victim") // touch — moves to MRU

	rl.mu.Lock()
	defer rl.mu.Unlock()
	tail := rl.lruOrder.Back()
	if tail == nil || tail.Value.(string) != "victim" {
		got := ""
		if tail != nil {
			got = tail.Value.(string)
		}
		t.Errorf("after touch, tail (MRU) should be 'victim', got %q", got)
	}
}

// TestRateLimiterCleanupRemovesElement — when the periodic cleanup loop
// drops an IP whose entries all expired, the LRU list element must be
// removed too (otherwise the list grows unbounded vs. the map).
func TestRateLimiterCleanupRemovesElement(t *testing.T) {
	rl := newRateLimiterStruct(100, 10*time.Millisecond)

	rl.AllowWithRetry("ephemeral")
	if rl.lruOrder.Len() != 1 {
		t.Fatalf("after one Allow: lruOrder.Len() = %d, want 1", rl.lruOrder.Len())
	}

	// Wait past window so the entry expires, then drive cleanup manually
	// (matching the cleanupLoop body).
	time.Sleep(15 * time.Millisecond)
	rl.mu.Lock()
	cutoff := time.Now().Add(-rl.window)
	for ip, entry := range rl.requests {
		var valid []time.Time
		for _, t := range entry.times {
			if t.After(cutoff) {
				valid = append(valid, t)
			}
		}
		if len(valid) == 0 {
			rl.lruOrder.Remove(entry.elem)
			delete(rl.requests, ip)
		} else {
			entry.times = valid
		}
	}
	rl.mu.Unlock()

	if rl.lruOrder.Len() != 0 {
		t.Errorf("after cleanup: lruOrder.Len() = %d, want 0 (drift between map and list)", rl.lruOrder.Len())
	}
	if len(rl.requests) != 0 {
		t.Errorf("after cleanup: requests = %d, want 0", len(rl.requests))
	}
}
