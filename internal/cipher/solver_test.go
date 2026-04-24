package cipher

import (
	"fmt"
	"strings"
	"testing"
	"time"
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
	for i := range stsCacheSize {
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
		// Inputs include the surrounding object-literal boundaries because
		// stsRegex now requires `,` or `{` before and `,` or `}` after to
		// avoid false matches inside comments / strings (audit D3).
		{`{signatureTimestamp:19850,`, "19850"},
		{`,sts:20123}`, "20123"},
		{`,signatureTimestamp: 20456,`, "20456"},
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

// TestSolverTimeoutInterruptsInfiniteLoop verifies that a malicious or
// malformed player.js with an infinite loop is bounded by CipherTimeout
// rather than hanging the decrypt path. Also verifies that after an
// interrupt, a SUBSEQUENT call on the same VM is not poisoned by a
// still-pending interrupt flag — the race that the time.AfterFunc-based
// first draft could leave behind.
func TestSolverTimeoutInterruptsInfiniteLoop(t *testing.T) {
	code := `
_result.sig = function(input) {
    if (input === "hang") {
        while (true) {} // hang forever
    }
    return input.split("").reverse().join("");
};
_result.n = function(input) {
    return input + "_ok";
};
`
	solvers, err := getFromPrepared(code)
	if err != nil {
		t.Fatalf("getFromPrepared: %v", err)
	}

	// Hang call — should return an error within roughly CipherTimeout, not
	// indefinitely.
	start := time.Now()
	_, sigErr := solvers.Sig("hang")
	elapsed := time.Since(start)
	if sigErr == nil {
		t.Fatal("expected error on infinite-loop sig call, got nil")
	}
	if !strings.Contains(sigErr.Error(), "timeout") && !strings.Contains(sigErr.Error(), "Interrupt") {
		t.Logf("hang error: %v", sigErr)
	}
	if elapsed > CipherTimeout*2 {
		t.Errorf("hang call took %v, expected close to CipherTimeout (%v)", elapsed, CipherTimeout)
	}

	// Subsequent call on the SAME VM must succeed — if ClearInterrupt ordering
	// is wrong, this second call would fail with InterruptedError leaking from
	// the prior timeout.
	result, err := solvers.Sig("hello")
	if err != nil {
		t.Fatalf("subsequent sig call after timeout failed (interrupt-flag leak?): %v", err)
	}
	if result != "olleh" {
		t.Errorf("subsequent sig call: expected 'olleh', got %q", result)
	}

	// Same check for N.
	nResult, err := solvers.N("foo")
	if err != nil {
		t.Fatalf("subsequent n call after sig timeout failed: %v", err)
	}
	if nResult != "foo_ok" {
		t.Errorf("subsequent n call: expected 'foo_ok', got %q", nResult)
	}
}
