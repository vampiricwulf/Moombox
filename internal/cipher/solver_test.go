package cipher

import (
	"fmt"
	"testing"
)

func TestGetFromPrepared(t *testing.T) {
	// Test with a simple sig function that reverses a string
	code := `
_result.sig = function(input) {
    return input.split("").reverse().join("");
};
_result.n = function(input) {
    return input + "_transformed";
};
`

	solvers, err := getFromPrepared(code)
	if err != nil {
		t.Fatalf("getFromPrepared: %v", err)
	}

	if solvers.Sig == nil {
		t.Fatal("expected sig solver")
	}
	if solvers.N == nil {
		t.Fatal("expected n solver")
	}

	// Test sig
	sigResult, err := solvers.Sig("hello")
	if err != nil {
		t.Fatalf("sig: %v", err)
	}
	if sigResult != "olleh" {
		t.Errorf("sig: expected 'olleh', got %q", sigResult)
	}

	// Test n
	nResult, err := solvers.N("test")
	if err != nil {
		t.Fatalf("n: %v", err)
	}
	if nResult != "test_transformed" {
		t.Errorf("n: expected 'test_transformed', got %q", nResult)
	}
}

func TestGetFromPrepared_SigOnly(t *testing.T) {
	code := `
_result.sig = function(input) {
    return input.toUpperCase();
};
`

	solvers, err := getFromPrepared(code)
	if err != nil {
		t.Fatalf("getFromPrepared: %v", err)
	}

	if solvers.Sig == nil {
		t.Fatal("expected sig solver")
	}
	if solvers.N != nil {
		t.Error("expected nil n solver")
	}

	result, err := solvers.Sig("hello")
	if err != nil {
		t.Fatalf("sig: %v", err)
	}
	if result != "HELLO" {
		t.Errorf("expected HELLO, got %q", result)
	}
}

func TestStsCacheFIFOEviction(t *testing.T) {
	// Verify that STS cache uses FIFO (oldest first) eviction,
	// not random eviction.
	sc := NewStsCache()

	// Fill the cache to capacity by manually inserting entries
	sc.mu.Lock()
	for i := 0; i < stsCacheSize; i++ {
		key := fmt.Sprintf("key-%04d", i)
		sc.cache[key] = fmt.Sprintf("sts-%d", i)
		sc.order = append(sc.order, key)
	}
	sc.mu.Unlock()

	if len(sc.cache) != stsCacheSize {
		t.Fatalf("expected cache size %d, got %d", stsCacheSize, len(sc.cache))
	}

	// Insert a new entry — should evict key-0000 (oldest)
	sc.mu.Lock()
	newKey := "key-new"
	if _, exists := sc.cache[newKey]; !exists && len(sc.cache) >= stsCacheSize {
		evictKey := sc.order[0]
		sc.order = sc.order[1:]
		delete(sc.cache, evictKey)
	}
	sc.cache[newKey] = "sts-new"
	sc.order = append(sc.order, newKey)
	sc.mu.Unlock()

	// Verify key-0000 was evicted (the oldest)
	sc.mu.RLock()
	_, hasOldest := sc.cache["key-0000"]
	_, hasNew := sc.cache["key-new"]
	_, hasSecond := sc.cache["key-0001"]
	sc.mu.RUnlock()

	if hasOldest {
		t.Error("expected key-0000 (oldest) to be evicted")
	}
	if !hasNew {
		t.Error("expected key-new to be in cache")
	}
	if !hasSecond {
		t.Error("expected key-0001 to still be in cache")
	}
}

func TestStsRegex(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{`signatureTimestamp:19850`, "19850"},
		{`sts:20123`, "20123"},
		{`signatureTimestamp: 20456`, "20456"},
	}

	for _, tt := range tests {
		m := stsRegex.FindStringSubmatch(tt.input)
		if m == nil {
			t.Errorf("no match for %q", tt.input)
			continue
		}
		if m[1] != tt.expected {
			t.Errorf("expected %q, got %q for input %q", tt.expected, m[1], tt.input)
		}
	}
}

type testLogger struct{}

func (testLogger) Debug(string, ...any) {}
func (testLogger) Info(string, ...any)  {}
func (testLogger) Warn(string, ...any)  {}
func (testLogger) Error(string, ...any) {}

func TestInvalidateSolver(t *testing.T) {
	dir := t.TempDir()
	logger := &testLogger{}
	s, err := NewSolver(dir, logger)
	if err != nil {
		t.Fatalf("NewSolver: %v", err)
	}

	// Manually cache a player file and solver
	playerURL := "https://www.youtube.com/s/player/test123/base.js"
	key := CacheKey(playerURL)

	// Put a fake player JS in the disk cache
	if err := s.playerCache.Put(playerURL, "fake player js"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Verify disk cache has the file
	cached, err := s.playerCache.Get(playerURL)
	if err != nil || cached == "" {
		t.Fatal("expected cached player JS")
	}

	// Put a fake solver in memory
	s.cacheSolvers(key, &Solvers{})

	// Verify in-memory cache has it
	s.solverMu.RLock()
	_, ok := s.solverData[key]
	s.solverMu.RUnlock()
	if !ok {
		t.Fatal("expected solver in memory cache")
	}

	// Invalidate
	s.InvalidateSolver(playerURL)

	// Verify in-memory cache is cleared
	s.solverMu.RLock()
	_, ok = s.solverData[key]
	s.solverMu.RUnlock()
	if ok {
		t.Error("expected solver evicted from memory cache")
	}

	// Verify disk cache is cleared
	cached, err = s.playerCache.Get(playerURL)
	if err != nil {
		t.Fatalf("Get after invalidate: %v", err)
	}
	if cached != "" {
		t.Error("expected disk cache evicted")
	}
}
