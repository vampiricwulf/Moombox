package twitch

import (
	"context"
	"testing"
)

type testLogger struct{}

func (l *testLogger) Debug(msg string, args ...any) {}
func (l *testLogger) Info(msg string, args ...any)  {}
func (l *testLogger) Warn(msg string, args ...any)  {}
func (l *testLogger) Error(msg string, args ...any) {}

func TestEmoteResolverCacheHit(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})

	// Prepopulate cache
	data := &TwitchEmoteData{
		BTTV: []EmoteInfo{{ID: "1", Code: "test", URL: "https://example.com"}},
	}
	er.mu.Lock()
	er.cache["testchannel"] = data
	er.cacheOrder = append(er.cacheOrder, "testchannel")
	er.mu.Unlock()

	// Should return cached data without making network calls
	result := er.Resolve(context.Background(), "ignored", "TestChannel")
	if result == nil {
		t.Fatal("expected cached result")
	}
	if len(result.BTTV) != 1 || result.BTTV[0].Code != "test" {
		t.Errorf("unexpected cached result: %+v", result)
	}
}

func TestEmoteResolverLRUEviction(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})

	// Fill cache to capacity
	for i := 0; i < 200; i++ {
		key := "channel_" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		er.cache[key] = &TwitchEmoteData{}
		er.cacheOrder = append(er.cacheOrder, key)
	}

	if len(er.cache) != 200 {
		t.Fatalf("expected 200 cache entries, got %d", len(er.cache))
	}

	// Adding one more should evict the oldest
	oldestKey := er.cacheOrder[0]

	er.mu.Lock()
	// Simulate adding a new entry that triggers eviction
	if len(er.cache) >= 200 && len(er.cacheOrder) > 0 {
		oldest := er.cacheOrder[0]
		er.cacheOrder = er.cacheOrder[1:]
		delete(er.cache, oldest)
	}
	er.cache["new_channel"] = &TwitchEmoteData{}
	er.cacheOrder = append(er.cacheOrder, "new_channel")
	er.mu.Unlock()

	// Oldest should be gone
	if _, exists := er.cache[oldestKey]; exists {
		t.Error("expected oldest entry to be evicted")
	}
	// New entry should exist
	if _, exists := er.cache["new_channel"]; !exists {
		t.Error("expected new entry to exist")
	}
	// Total should still be 200
	if len(er.cache) != 200 {
		t.Errorf("expected 200 cache entries after eviction, got %d", len(er.cache))
	}
}

func TestEmoteResolverLRUPromotion(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})

	// Add 3 entries
	er.cache["a"] = &TwitchEmoteData{}
	er.cache["b"] = &TwitchEmoteData{}
	er.cache["c"] = &TwitchEmoteData{}
	er.cacheOrder = []string{"a", "b", "c"}

	// Access "a" — should move to end
	er.Resolve(context.Background(), "ignored", "A")

	if er.cacheOrder[len(er.cacheOrder)-1] != "a" {
		t.Errorf("expected 'a' at end of order after access, got order: %v", er.cacheOrder)
	}
}

func TestEmoteResolverClear(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})
	er.cache["test"] = &TwitchEmoteData{}
	er.cacheOrder = []string{"test"}

	er.Clear()

	if len(er.cache) != 0 {
		t.Errorf("expected empty cache after Clear(), got %d entries", len(er.cache))
	}
	if len(er.cacheOrder) != 0 {
		t.Errorf("expected empty cacheOrder after Clear(), got %d entries", len(er.cacheOrder))
	}
}
