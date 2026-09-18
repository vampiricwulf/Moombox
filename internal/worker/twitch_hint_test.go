package worker

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/twitch"
)

func TestTwitchHintCacheStashAndTake(t *testing.T) {
	c := newTwitchHintCache()
	info := &twitch.TwitchStreamInfo{StreamID: "111", IsLive: true}

	c.stash("tw_111", info)

	got := c.take("tw_111")
	if got == nil {
		t.Fatal("take after stash: want non-nil")
	}
	if got.StreamID != "111" {
		t.Errorf("StreamID: want 111, got %q", got.StreamID)
	}

	// take again — should be empty (take-once)
	if got := c.take("tw_111"); got != nil {
		t.Errorf("second take: want nil, got %+v", got)
	}
}

func TestTwitchHintCacheTakeMissing(t *testing.T) {
	c := newTwitchHintCache()
	if got := c.take("tw_does_not_exist"); got != nil {
		t.Errorf("take of unknown jobID: want nil, got %+v", got)
	}
}

func TestTwitchHintCacheTTLEviction(t *testing.T) {
	c := newTwitchHintCache()
	info := &twitch.TwitchStreamInfo{StreamID: "222", IsLive: true}

	// Stash with a manual past timestamp older than the TTL
	c.mu.Lock()
	c.entries["tw_222"] = twitchHintEntry{
		info:      info,
		stashedAt: time.Now().Add(-2 * twitchHintTTL),
	}
	c.mu.Unlock()

	if got := c.take("tw_222"); got != nil {
		t.Errorf("take of expired entry: want nil, got %+v", got)
	}

	// Verify the expired entry was removed (no zombie)
	c.mu.Lock()
	_, exists := c.entries["tw_222"]
	c.mu.Unlock()
	if exists {
		t.Error("expired entry should be removed from cache after take")
	}
}

func TestTwitchHintCacheNilSafe(t *testing.T) {
	var c *twitchHintCache
	// nil receiver should be safe (no-op)
	c.stash("tw_nil", &twitch.TwitchStreamInfo{StreamID: "nil"})
	if got := c.take("tw_nil"); got != nil {
		t.Errorf("take on nil cache: want nil, got %+v", got)
	}
	if s := c.Stats(); s.Hits != 0 || s.Misses != 0 {
		t.Errorf("Stats on nil cache: want zero, got %+v", s)
	}
}

func TestTwitchHintCacheStatsCounter(t *testing.T) {
	c := newTwitchHintCache()
	info := &twitch.TwitchStreamInfo{StreamID: "stat", IsLive: true}

	// 1 hit: stash + take
	c.stash("tw_stat", info)
	if got := c.take("tw_stat"); got == nil {
		t.Fatal("first take after stash: want non-nil")
	}

	// 2 misses: take of unknown ID twice
	c.take("tw_unknown_a")
	c.take("tw_unknown_b")

	// 1 miss via expiry: backdate then take
	c.mu.Lock()
	c.entries["tw_expired"] = twitchHintEntry{
		info:      info,
		stashedAt: time.Now().Add(-2 * twitchHintTTL),
	}
	c.mu.Unlock()
	if got := c.take("tw_expired"); got != nil {
		t.Fatal("expired take: want nil")
	}

	// 1 more hit: fresh stash + take
	c.stash("tw_stat2", info)
	if got := c.take("tw_stat2"); got == nil {
		t.Fatal("second take after stash: want non-nil")
	}

	s := c.Stats()
	if s.Hits != 2 {
		t.Errorf("Hits: want 2, got %d", s.Hits)
	}
	if s.Misses != 3 {
		t.Errorf("Misses: want 3, got %d", s.Misses)
	}
}

func TestTwitchHintCacheOverwriteOnDoubleStash(t *testing.T) {
	c := newTwitchHintCache()
	first := &twitch.TwitchStreamInfo{StreamID: "v1", IsLive: true}
	second := &twitch.TwitchStreamInfo{StreamID: "v2", IsLive: true}

	c.stash("tw_overwrite", first)
	c.stash("tw_overwrite", second)

	got := c.take("tw_overwrite")
	if got == nil {
		t.Fatal("take after double stash: want non-nil")
	}
	if got.StreamID != "v2" {
		t.Errorf("StreamID: want v2 (last write wins), got %q", got.StreamID)
	}
}

// TestProcessorStashAndTake verifies a stashed hint short-circuits the
// GetStreamInfo call. We can't easily inject a fake Twitch service without
// substantially refactoring StreamProcessor, so this test focuses on the
// observable side-effect: after stashing and a single take, the cache is
// empty (take-once). The end-to-end path is covered by the integration
// scenario in Phase 10.
func TestProcessorStashAndTake(t *testing.T) {
	sp := &StreamProcessor{twitchHints: newTwitchHintCache()}
	info := &twitch.TwitchStreamInfo{StreamID: "abc", IsLive: true}

	sp.StashTwitchStreamInfo("tw_abc", info)

	got := sp.twitchHints.take("tw_abc")
	if got == nil || got.StreamID != "abc" {
		t.Fatalf("expected stashed info to be retrievable, got %+v", got)
	}

	// take-once: second take is empty
	if got := sp.twitchHints.take("tw_abc"); got != nil {
		t.Errorf("second take after stash: want nil, got %+v", got)
	}
}

// TestWaitForTwitchLiveConsumesHint pins ENGINE-18 (report #53): a manually
// added offline channel polled GQL every 15-20 s while the monitor
// batch-polled the same channel and stashed a hint for the very same job —
// two GQL streams for one answer.
//
// Mutant: dropping the take() from the wait loop — the stash is never
// consumed and the poll still pays a GetStreamInfo round trip.
func TestWaitForTwitchLiveConsumesHint(t *testing.T) {
	c := newTwitchHintCache()
	c.stash("job1", &twitch.TwitchStreamInfo{IsLive: true, StreamID: "s1"})

	got := takeLiveHint(c, "job1")
	if got == nil || got.StreamID != "s1" {
		t.Fatalf("takeLiveHint = %v, want the stashed live info", got)
	}
	if again := takeLiveHint(c, "job1"); again != nil {
		t.Error("takeLiveHint returned the same hint twice — take-once semantics are broken")
	}
	c.stash("job2", &twitch.TwitchStreamInfo{IsLive: false})
	if got := takeLiveHint(c, "job2"); got != nil {
		t.Error("takeLiveHint returned a non-live hint — the wait must keep polling")
	}

	// The wait loop itself needs a live Twitch API and a 15-20 s sleep to
	// drive, so the CALL SITE is read from the syntax tree — the package's
	// technique for undrivable sites (queue_lifecycle_test.go,
	// stream_processor_early_chat_test.go). This is the arm the brief's mutant
	// (dropping the take() from the wait loop) fires: the helper still passes
	// every assertion above while the poll goes on paying its own round trip.
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "stream_processor_twitch.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse stream_processor_twitch.go: %v", err)
	}
	var wait *ast.FuncDecl
	for _, decl := range parsed.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "waitForTwitchLive" {
			wait = fn
		}
	}
	if wait == nil {
		t.Fatal("no waitForTwitchLive declaration in stream_processor_twitch.go")
	}
	takes := funcCallPositions(wait, "takeLiveHint")
	if len(takes) != 1 {
		t.Fatalf("waitForTwitchLive calls takeLiveHint %d time(s), want exactly 1 — the monitor's "+
			"stashed answer must be consumed by the wait, not left for a second GQL stream", len(takes))
	}
	probes := methodCallPositions(wait, "GetStreamInfo")
	if len(probes) != 1 || takes[0] > probes[0] {
		t.Errorf("takeLiveHint is at line %d and GetStreamInfo at %v — the hint must be consumed "+
			"BEFORE the poll, or it saves nothing",
			fset.Position(takes[0]).Line, probes)
	}
}

// funcCallPositions returns the position of every call to the plain (non-method)
// function named name inside n. methodCallPositions matches a selector, and
// takeLiveHint is a package-level function.
func funcCallPositions(n ast.Node, name string) []token.Pos {
	var out []token.Pos
	ast.Inspect(n, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == name {
			out = append(out, call.Pos())
		}
		return true
	})
	return out
}
