package twitch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
)

type testLogger struct{}

func (l *testLogger) Debug(msg string, args ...any) {}
func (l *testLogger) Info(msg string, args ...any)  {}
func (l *testLogger) Warn(msg string, args ...any)  {}
func (l *testLogger) Error(msg string, args ...any) {}

// seedEmoteCache puts a FRESH entry in the cache, the way a successful Resolve
// would. Tests must not poke er.cache directly any more: an entry with a zero
// fetchedAt is expired, so a direct poke would silently turn a cache-hit test
// into a network test.
func seedEmoteCache(er *EmoteResolver, key string, data *TwitchEmoteData) {
	er.mu.Lock()
	defer er.mu.Unlock()
	er.cache[key] = emoteCacheEntry{data: data, fetchedAt: er.now()}
	er.cacheOrder = append(er.cacheOrder, key)
}

// installEmoteFetchStub points the package HTTP client at one canned reply for
// every provider. status 0 means "the transport itself fails", i.e. all three
// providers are down. It returns the request counter.
//
// The swap is why no test in this file may call t.Parallel: twitchHTTPClient is
// shared with every other test in the package.
func installEmoteFetchStub(t *testing.T, status int, body string) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	prev := twitchHTTPClient
	t.Cleanup(func() { twitchHTTPClient = prev })
	twitchHTTPClient = &http.Client{Transport: probeRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if status == 0 {
			return nil, fmt.Errorf("emote provider unreachable")
		}
		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		return &http.Response{
			StatusCode: status,
			Header:     h,
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}
	return &calls
}

func TestEmoteResolverCacheHit(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})
	seedEmoteCache(er, "testchannel", &TwitchEmoteData{
		BTTV: []EmoteInfo{{ID: "1", Code: "test", URL: "https://example.com"}},
	})
	// No HTTP stub installed: a fetch here would hit the real network, which is
	// itself the assertion that the cache was used.
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
	for i := range 200 {
		seedEmoteCache(er, "channel_"+string(rune('a'+i%26))+string(rune('a'+i/26)), &TwitchEmoteData{})
	}
	if len(er.cache) != 200 {
		t.Fatalf("expected 200 cache entries, got %d", len(er.cache))
	}
	oldestKey := er.cacheOrder[0]

	er.mu.Lock()
	er.evictIfFullLocked()
	er.cache["new_channel"] = emoteCacheEntry{data: &TwitchEmoteData{}, fetchedAt: er.now()}
	er.cacheOrder = append(er.cacheOrder, "new_channel")
	er.mu.Unlock()

	if _, exists := er.cache[oldestKey]; exists {
		t.Error("expected oldest entry to be evicted")
	}
	if _, exists := er.cache["new_channel"]; !exists {
		t.Error("expected new entry to exist")
	}
	if len(er.cache) != 200 {
		t.Errorf("expected 200 cache entries after eviction, got %d", len(er.cache))
	}
}

func TestEmoteResolverLRUPromotion(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})
	seedEmoteCache(er, "a", &TwitchEmoteData{})
	seedEmoteCache(er, "b", &TwitchEmoteData{})
	seedEmoteCache(er, "c", &TwitchEmoteData{})

	er.Resolve(context.Background(), "ignored", "A")

	if er.cacheOrder[len(er.cacheOrder)-1] != "a" {
		t.Errorf("expected 'a' at end of order after access, got order: %v", er.cacheOrder)
	}
}

func TestEmoteResolverClear(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})
	seedEmoteCache(er, "test", &TwitchEmoteData{})

	er.Clear()

	if len(er.cache) != 0 {
		t.Errorf("expected empty cache after Clear(), got %d entries", len(er.cache))
	}
	if len(er.cacheOrder) != 0 {
		t.Errorf("expected empty cacheOrder after Clear(), got %d entries", len(er.cacheOrder))
	}
}

// TestEmoteResolverDoesNotCacheATotalFailure is the first half of T1-11.
//
// Resolve used to write the cache BEFORE looking at whether anything came back,
// so three simultaneously-failing providers — one BTTV outage, one flaky
// network minute — poisoned that channel for the whole process lifetime. A
// 24/7 daemon then served an empty emote set for days.
//
// Mutants: caching unconditionally (the second Resolve makes no requests and
// returns non-nil); returning a non-nil empty set on total failure (the
// per-downloader cache in resolveEmotesCached latches THAT instead, which is
// the same bug one layer up).
func TestEmoteResolverDoesNotCacheATotalFailure(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})
	calls := installEmoteFetchStub(t, 0, "")

	if got := er.Resolve(context.Background(), "chan-1", "chan-1"); got != nil {
		t.Errorf("Resolve with every provider down = %+v, want nil", got)
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("first Resolve made %d requests, want 3", n)
	}
	er.mu.Lock()
	cached := len(er.cache)
	er.mu.Unlock()
	if cached != 0 {
		t.Errorf("cache holds %d entries after a total failure, want 0", cached)
	}

	er.Resolve(context.Background(), "chan-1", "chan-1")
	if n := calls.Load(); n != 6 {
		t.Errorf("second Resolve brought the total to %d requests, want 6 — the failure was cached", n)
	}
}

// TestEmoteResolverCachesAPartialSuccess pins the other edge: ONE provider
// answering is enough to cache, including when it answers with no emotes.
//
// Mutant: gating the cache on `total > 0` instead of on "any provider
// answered" — a channel with no third-party emotes at all would then be
// refetched from three APIs on every part of every job, forever.
func TestEmoteResolverCachesAPartialSuccess(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})
	// A 200 with a body every provider parses to zero emotes.
	calls := installEmoteFetchStub(t, http.StatusOK, `{}`)

	if got := er.Resolve(context.Background(), "chan-1", "chan-1"); got == nil {
		t.Fatal("Resolve with three answering providers returned nil")
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("first Resolve made %d requests, want 3", n)
	}
	er.Resolve(context.Background(), "chan-1", "chan-1")
	if n := calls.Load(); n != 3 {
		t.Errorf("second Resolve brought the total to %d requests, want 3 — an emote-less "+
			"channel must still be cached", n)
	}
}

// TestEmoteResolverCachesWhenOnlyOneProviderAnswers is the MIXED case between
// the two tests above: BTTV answers, FFZ and 7TV are unreachable. Both of
// those are real field states — a single provider having a bad ten minutes is
// far commoner than all three going down together — and the rule Resolve
// applies is "ANY provider answered", not "all three did".
//
// Mutants this kills:
//   - gating the cache write on every provider having answered
//     (bttvOK && ffzOK && sevenTVOK): the second Resolve would refetch and the
//     total would be 6, so a channel would be re-fetched from three APIs on
//     every part of every job for as long as one provider stayed down.
//   - refetching on the second Resolve for any other reason — the count is
//     asserted, not merely the returned value.
//   - returning the DOWN providers' empty slices as though they were answers:
//     the BTTV assertion holds the half that did answer, and the FFZ/SevenTV
//     assertions hold that nothing was invented for the halves that did not.
func TestEmoteResolverCachesWhenOnlyOneProviderAnswers(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})
	var calls atomic.Int64
	prev := twitchHTTPClient
	t.Cleanup(func() { twitchHTTPClient = prev })
	twitchHTTPClient = &http.Client{Transport: probeRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if !strings.HasPrefix(req.URL.String(), constants.TwitchEmoteAPIs.BTTVChannel) {
			// FFZ and 7TV: the transport itself fails, which is the one thing
			// fetchFFZ/fetch7TV report as "did not answer" (a 200 with no
			// emotes is an answer).
			return nil, fmt.Errorf("emote provider unreachable")
		}
		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     h,
			Body:       io.NopCloser(strings.NewReader(`{"channelEmotes":[{"id":"e1","code":"catJAM"}],"sharedEmotes":[]}`)),
			Request:    req,
		}, nil
	})}

	got := er.Resolve(context.Background(), "chan-1", "chan-1")
	if got == nil {
		t.Fatal("Resolve returned nil although BTTV answered — one answering provider is a real, " +
			"cacheable answer")
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("first Resolve made %d requests, want 3", n)
	}
	if len(got.BTTV) != 1 || got.BTTV[0].Code != "catJAM" {
		t.Errorf("BTTV = %+v, want the single emote the answering provider sent", got.BTTV)
	}
	if len(got.FFZ) != 0 || len(got.SevenTV) != 0 {
		t.Errorf("FFZ = %+v, SevenTV = %+v, want both empty — neither provider answered, and an "+
			"unreachable provider must not contribute emotes", got.FFZ, got.SevenTV)
	}

	again := er.Resolve(context.Background(), "chan-1", "chan-1")
	if n := calls.Load(); n != 3 {
		t.Errorf("second Resolve brought the total to %d requests, want 3 — a partial answer must "+
			"be cached (mutant: caching only when all three providers answered)", n)
	}
	if again == nil || len(again.BTTV) != 1 || len(again.FFZ) != 0 || len(again.SevenTV) != 0 {
		t.Errorf("the second Resolve returned %+v, want the cached BTTV-only set from the first", again)
	}
}

// TestEmoteResolverRefetchesAfterTheTTL is the second half of T1-11: a daemon
// that runs for weeks must pick up a channel's new 7TV/BTTV/FFZ emotes.
//
// Mutants: no TTL at all (the post-expiry Resolve makes no requests); an
// expiry that DROPS the entry before refetching (the failing refetch below
// would then return nil and the job would lose the emote set it had).
func TestEmoteResolverRefetchesAfterTheTTL(t *testing.T) {
	er := NewEmoteResolver(&testLogger{})
	now := time.Now()
	er.now = func() time.Time { return now }

	stale := &TwitchEmoteData{BTTV: []EmoteInfo{{ID: "1", Code: "old"}}}
	seedEmoteCache(er, "chan-1", stale)

	now = now.Add(emoteCacheTTL - time.Minute)
	calls := installEmoteFetchStub(t, 0, "")
	er.Resolve(context.Background(), "chan-1", "chan-1")
	if n := calls.Load(); n != 0 {
		t.Fatalf("an entry one minute inside the TTL made %d requests, want 0", n)
	}

	now = now.Add(2 * time.Minute) // now past emoteCacheTTL
	got := er.Resolve(context.Background(), "chan-1", "chan-1")
	if n := calls.Load(); n != 3 {
		t.Errorf("an expired entry made %d requests, want 3", n)
	}
	if got == nil || len(got.BTTV) != 1 || got.BTTV[0].Code != "old" {
		t.Errorf("a failed refetch returned %+v, want the stale set served rather than dropped", got)
	}
}
