package bgutils

import (
	"testing"
	"time"
)

// TestStatsZeroOnFreshProvider verifies a fresh provider returns
// all-zero counters and no cached minters.
func TestStatsZeroOnFreshProvider(t *testing.T) {
	pp := NewPotProvider(&BgConfig{RequestKey: DefaultRequestKey}, &testLogger{})

	got := pp.Stats()
	want := PotStats{}
	if got != want {
		t.Errorf("Stats on fresh provider: want zero, got %+v", got)
	}
}

// TestStatsCachedMintersReflectsLiveCount verifies the CachedMinters
// field is the live cache size (not a counter), and that putting a
// minter in the cache increments it without touching the counters.
func TestStatsCachedMintersReflectsLiveCount(t *testing.T) {
	pp := NewPotProvider(&BgConfig{RequestKey: DefaultRequestKey}, &testLogger{})

	pp.minterCache[defaultMinterKey] = &TokenMinter{ExpiresAt: time.Now().Add(1 * time.Hour)}

	got := pp.Stats()
	if got.CachedMinters != 1 {
		t.Errorf("CachedMinters with one entry: want 1, got %d", got.CachedMinters)
	}
	if got.MintersCreated != 0 {
		t.Errorf("MintersCreated should NOT increment when test seeds the cache directly: got %d", got.MintersCreated)
	}
}

// TestStatsInvalidateCachesIncrementsInvalidated verifies the
// InvalidateCaches counter increments by the number of evicted minters.
func TestStatsInvalidateCachesIncrementsInvalidated(t *testing.T) {
	pp := NewPotProvider(&BgConfig{RequestKey: DefaultRequestKey}, &testLogger{})
	pp.minterCache[defaultMinterKey] = &TokenMinter{ExpiresAt: time.Now().Add(1 * time.Hour)}

	pp.InvalidateCaches()

	got := pp.Stats()
	if got.MintersInvalidated != 1 {
		t.Errorf("MintersInvalidated after InvalidateCaches with 1 cached: want 1, got %d", got.MintersInvalidated)
	}
	if got.CachedMinters != 0 {
		t.Errorf("CachedMinters after InvalidateCaches: want 0, got %d", got.CachedMinters)
	}
}

// TestStatsInvalidateIntegrityTokensIncrementsInvalidated covers the
// other invalidation path.
func TestStatsInvalidateIntegrityTokensIncrementsInvalidated(t *testing.T) {
	pp := NewPotProvider(&BgConfig{RequestKey: DefaultRequestKey}, &testLogger{})
	pp.minterCache[defaultMinterKey] = &TokenMinter{ExpiresAt: time.Now().Add(1 * time.Hour)}

	pp.InvalidateIntegrityTokens()

	got := pp.Stats()
	if got.MintersInvalidated != 1 {
		t.Errorf("MintersInvalidated after InvalidateIntegrityTokens: want 1, got %d", got.MintersInvalidated)
	}
}

// TestStatsInvalidateOnEmptyCacheNoOp verifies that invalidating an
// empty cache is a no-op for the counters.
func TestStatsInvalidateOnEmptyCacheNoOp(t *testing.T) {
	pp := NewPotProvider(&BgConfig{RequestKey: DefaultRequestKey}, &testLogger{})

	pp.InvalidateCaches()
	pp.InvalidateIntegrityTokens()

	got := pp.Stats()
	if got.MintersInvalidated != 0 {
		t.Errorf("MintersInvalidated after invalidating empty cache: want 0, got %d", got.MintersInvalidated)
	}
}

// TestStatsSessionHitIncrementsCounter exercises GeneratePoToken's
// session-cache-hit branch by pre-seeding the session cache and
// verifying the counter increments. The valid session is returned
// straight from the cache without touching the minter machinery.
func TestStatsSessionHitIncrementsCounter(t *testing.T) {
	pp := NewPotProvider(&BgConfig{RequestKey: DefaultRequestKey}, &testLogger{})

	binding := "test-binding"
	pp.sessionCache[binding] = &SessionData{
		PoToken:        "cached-token",
		ContentBinding: binding,
		ExpiresAt:      time.Now().Add(1 * time.Hour),
	}

	session, err := pp.GeneratePoToken(t.Context(), binding, false)
	if err != nil {
		t.Fatalf("GeneratePoToken: %v", err)
	}
	if session.PoToken != "cached-token" {
		t.Errorf("returned session: want cached, got %v", session.PoToken)
	}

	got := pp.Stats()
	if got.SessionHits != 1 {
		t.Errorf("SessionHits after cache hit: want 1, got %d", got.SessionHits)
	}
	if got.MinterHits != 0 {
		t.Errorf("MinterHits should NOT increment on session hit: got %d", got.MinterHits)
	}
	if got.GenerateErrors != 0 {
		t.Errorf("GenerateErrors should be 0 on success: got %d", got.GenerateErrors)
	}
}

// TestStatsCountersAreMonotonic verifies that counters never decrement
// across multiple operations of the same kind.
func TestStatsCountersAreMonotonic(t *testing.T) {
	pp := NewPotProvider(&BgConfig{RequestKey: DefaultRequestKey}, &testLogger{})

	for i := 0; i < 5; i++ {
		pp.minterCache[defaultMinterKey] = &TokenMinter{ExpiresAt: time.Now().Add(1 * time.Hour)}
		pp.InvalidateIntegrityTokens()
	}

	got := pp.Stats()
	if got.MintersInvalidated != 5 {
		t.Errorf("MintersInvalidated after 5 invalidations: want 5, got %d", got.MintersInvalidated)
	}
}
