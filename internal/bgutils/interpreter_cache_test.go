package bgutils

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// resetGlobalCacheForTest replaces the package-global cache with a
// fresh one so tests don't share state. Returns a cleanup func.
func resetGlobalCacheForTest(t *testing.T) func() {
	t.Helper()
	old := globalInterpreterHashCache
	globalInterpreterHashCache = &interpreterHashCache{}
	return func() {
		globalInterpreterHashCache = old
	}
}

// TestHashCacheKeyExtractsChromeMajor — UA-major namespacing means a
// future Chrome version invalidates cached hashes automatically.
func TestHashCacheKeyExtractsChromeMajor(t *testing.T) {
	got := hashCacheKey("rk1", "Mozilla/5.0 (...) Chrome/131.0.0.0 Safari/537.36")
	if !strings.HasSuffix(got, "|131") {
		t.Errorf("hashCacheKey: want UA-major 131 suffix, got %q", got)
	}
}

// TestHashCacheKeyFallsBackToUnknown when UA has no Chrome version.
func TestHashCacheKeyFallsBackToUnknown(t *testing.T) {
	got := hashCacheKey("rk1", "Custom-UA/1.0")
	if !strings.HasSuffix(got, "|unknown") {
		t.Errorf("hashCacheKey: want |unknown suffix, got %q", got)
	}
}

// TestHashCacheLoadMissingFileIsEmpty — fresh install.
func TestHashCacheLoadMissingFileIsEmpty(t *testing.T) {
	cleanup := resetGlobalCacheForTest(t)
	defer cleanup()

	dir := t.TempDir()
	c := globalInterpreterHashCache
	if !c.load(dir, nil) {
		t.Fatal("load on missing file: want success, got false")
	}
	if got := c.get("anything"); got != "" {
		t.Errorf("get on empty cache: want \"\", got %q", got)
	}
}

// TestHashCacheRoundTrip exercises set → write → load → get.
func TestHashCacheRoundTrip(t *testing.T) {
	cleanup := resetGlobalCacheForTest(t)
	defer cleanup()

	dir := t.TempDir()
	c := globalInterpreterHashCache
	c.load(dir, nil)
	c.set("key1", "hash-abc-123", nil)

	// Verify on-disk file exists
	path := filepath.Join(dir, interpreterHashSidecarFilename)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("sidecar should exist after set: %v", err)
	}

	// Reset in-memory state, reload from disk
	cleanup() // restore global to old
	cleanup2 := resetGlobalCacheForTest(t)
	defer cleanup2()

	c2 := globalInterpreterHashCache
	c2.load(dir, nil)
	if got := c2.get("key1"); got != "hash-abc-123" {
		t.Errorf("after round-trip: want hash-abc-123, got %q", got)
	}
}

// TestHashCachePrunesStaleEntries — entries older than 30 days are
// dropped on load to bound the file size.
func TestHashCachePrunesStaleEntries(t *testing.T) {
	cleanup := resetGlobalCacheForTest(t)
	defer cleanup()

	dir := t.TempDir()
	stale := time.Now().Add(-31 * 24 * time.Hour)
	fresh := time.Now()
	hashes := map[string]interpreterHashEntry{
		"stale": {Hash: "old", SavedAt: stale},
		"fresh": {Hash: "new", SavedAt: fresh},
	}
	if err := writeInterpreterHashSidecar(dir, hashes); err != nil {
		t.Fatalf("seed: %v", err)
	}

	c := globalInterpreterHashCache
	c.load(dir, nil)
	if got := c.get("stale"); got != "" {
		t.Errorf("stale entry should be pruned, got %q", got)
	}
	if got := c.get("fresh"); got != "new" {
		t.Errorf("fresh entry: want new, got %q", got)
	}
}

// TestHashCacheLoadIsIdempotent — multiple load calls don't reload
// from disk (in-memory cache is authoritative after first load).
func TestHashCacheLoadIsIdempotent(t *testing.T) {
	cleanup := resetGlobalCacheForTest(t)
	defer cleanup()

	dir := t.TempDir()
	c := globalInterpreterHashCache
	c.load(dir, nil)
	c.set("key", "first", nil)

	// Mutate disk underneath us; load again — in-memory cache should
	// stay with "first" because subsequent load() is a no-op.
	hashes := map[string]interpreterHashEntry{
		"key": {Hash: "second", SavedAt: time.Now()},
	}
	if err := writeInterpreterHashSidecar(dir, hashes); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	c.load(dir, nil)
	if got := c.get("key"); got != "first" {
		t.Errorf("load is not idempotent: want first, got %q", got)
	}
}

// TestHashCacheSetIsThreadSafe — concurrent set calls don't corrupt.
func TestHashCacheSetIsThreadSafe(t *testing.T) {
	cleanup := resetGlobalCacheForTest(t)
	defer cleanup()

	dir := t.TempDir()
	c := globalInterpreterHashCache
	c.load(dir, nil)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			c.set("k"+string(rune('a'+id)), "h", nil)
		}(i)
	}
	wg.Wait()

	for i := 0; i < 8; i++ {
		key := "k" + string(rune('a'+i))
		if got := c.get(key); got == "" {
			t.Errorf("missing key %q after concurrent set", key)
		}
	}
}
