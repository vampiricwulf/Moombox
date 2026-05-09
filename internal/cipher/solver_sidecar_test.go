package cipher

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
)

// fakeSidecar implements sidecarClient with scripted responses.
type fakeSidecar struct {
	calls   []sidecar.SolveCipherRequest
	respond func(req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error)
}

func (f *fakeSidecar) SolveCipher(_ context.Context, req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
	f.calls = append(f.calls, req)
	return f.respond(req)
}

// fakePlayerSource serves a fixed JS string for a single playerID. Tracks
// RemovePlayerJS calls so tests can assert reactive-invalidation behaviour
// (Stage 2). When Removed is true, subsequent PlayerJS calls return the
// freshJS value — mirroring how a real PlayerSource would refetch from
// YouTube after eviction.
type fakePlayerSource struct {
	playerID string
	js       string
	freshJS  string
	Removed  bool
}

func (f *fakePlayerSource) PlayerJS(playerID string) (string, error) {
	if playerID != f.playerID {
		return "", errors.New("unknown playerID in fake source")
	}
	if f.Removed && f.freshJS != "" {
		return f.freshJS, nil
	}
	return f.js, nil
}

func (f *fakePlayerSource) RemovePlayerJS(playerID string) error {
	if playerID != f.playerID {
		return errors.New("unknown playerID in fake source")
	}
	f.Removed = true
	return nil
}

func TestSidecarSolverSendsPlayerJSOnFirstCallOnly(t *testing.T) {
	fake := &fakeSidecar{
		respond: func(req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
			return sidecar.SolveCipherResult{
				SigResults: map[string]string{"in": "out"},
			}, nil
		},
	}
	src := &fakePlayerSource{playerID: "P1", js: "JS"}
	s := newSidecarSolverWith(fake, src)
	ctx := context.Background()

	if _, err := s.Sig(ctx, "P1", "in"); err != nil {
		t.Fatalf("first Sig: %v", err)
	}
	if _, err := s.Sig(ctx, "P1", "in"); err != nil {
		t.Fatalf("second Sig: %v", err)
	}
	if got, want := len(fake.calls), 2; got != want {
		t.Fatalf("call count: got %d, want %d", got, want)
	}
	if fake.calls[0].PlayerJS == "" {
		t.Errorf("first call should include PlayerJS, got empty")
	}
	if fake.calls[1].PlayerJS != "" {
		t.Errorf("second call should omit PlayerJS, got %q", fake.calls[1].PlayerJS)
	}
}

func TestSidecarSolverRetriesOnPlayerNotLoaded(t *testing.T) {
	calls := 0
	fake := &fakeSidecar{
		respond: func(req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
			calls++
			if calls == 1 {
				return sidecar.SolveCipherResult{}, sidecar.ErrPlayerNotLoaded
			}
			return sidecar.SolveCipherResult{
				SigResults: map[string]string{"in": "out"},
			}, nil
		},
	}
	src := &fakePlayerSource{playerID: "P1", js: "JS"}
	s := newSidecarSolverWith(fake, src)
	// Pre-mark playerID as sent so the first call would normally omit JS.
	s.markPlayerSent("P1")

	out, err := s.Sig(context.Background(), "P1", "in")
	if err != nil {
		t.Fatalf("Sig: %v", err)
	}
	if out != "out" {
		t.Errorf("Sig result: got %q, want %q", out, "out")
	}
	if got, want := len(fake.calls), 2; got != want {
		t.Fatalf("expected one retry; got %d calls", got)
	}
	if fake.calls[0].PlayerJS != "" {
		t.Errorf("first call should have omitted PlayerJS")
	}
	if fake.calls[1].PlayerJS == "" {
		t.Errorf("retry should include PlayerJS")
	}
}

func TestSidecarSolverBatchPartialResponse(t *testing.T) {
	fake := &fakeSidecar{
		respond: func(req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
			// Return a partial map: sig "A" answered, sig "B" omitted
			return sidecar.SolveCipherResult{
				SigResults: map[string]string{"A": "decoded-A"},
				NResults:   map[string]string{},
			}, nil
		},
	}
	src := &fakePlayerSource{playerID: "P1", js: "JS"}
	s := newSidecarSolverWith(fake, src)

	_, _, err := s.Batch(context.Background(), "P1", []string{"A", "B"}, nil)
	if err == nil {
		t.Fatal("expected error on partial sidecar Batch response")
	}
	if !strings.Contains(err.Error(), "missing sig result") {
		t.Errorf("error should name the missing-result condition; got: %v", err)
	}
}

// TestSidecarSolverStaleJS_RecoversWithForceReload — sidecar reports the
// canonical stale-JS error on the first call; sidecarSolver evicts caches,
// retries with ForceReload=true + fresh JS, and recovers.
func TestSidecarSolverStaleJS_RecoversWithForceReload(t *testing.T) {
	calls := 0
	fake := &fakeSidecar{
		respond: func(req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
			calls++
			if calls == 1 {
				// Mimic the production error message we observed in field
				// logs against player 8fb635c2 — comes through here as a
				// generic error since the call() helper translates only
				// the PLAYER_NOT_LOADED sentinel.
				return sidecar.SolveCipherResult{}, errors.New("ejs solve sig: no solutions")
			}
			return sidecar.SolveCipherResult{
				SigResults: map[string]string{"in": "out"},
			}, nil
		},
	}
	src := &fakePlayerSource{playerID: "P1", js: "stale-js", freshJS: "fresh-js"}
	s := newSidecarSolverWith(fake, src)
	// Pre-mark sent so the first call would normally omit JS — that's the
	// scenario where the sidecar's cached stale JS would be exercised.
	s.markPlayerSent("P1")

	out, err := s.Sig(context.Background(), "P1", "in")
	if err != nil {
		t.Fatalf("Sig with stale-then-fresh: %v", err)
	}
	if out != "out" {
		t.Errorf("recovered Sig: want %q got %q", "out", out)
	}
	if !src.Removed {
		t.Errorf("PlayerSource.RemovePlayerJS should have been called on stale-JS detection")
	}
	if got, want := len(fake.calls), 2; got != want {
		t.Fatalf("expected exactly 2 sidecar calls; got %d", got)
	}
	// Retry call must have JS attached AND ForceReload=true.
	retry := fake.calls[1]
	if retry.PlayerJS == "" {
		t.Errorf("retry call should attach PlayerJS")
	}
	if retry.PlayerJS != "fresh-js" {
		t.Errorf("retry should attach freshly-fetched JS; got %q", retry.PlayerJS)
	}
	if !retry.ForceReload {
		t.Errorf("retry call should set ForceReload=true so sidecar drops its cached preprocessed entry")
	}
}

// TestSidecarSolverStaleJS_PersistentReturnsErrPlayerJSStale — when the
// stale-JS error survives the retry, the caller gets ErrPlayerJSStale
// (as opposed to the raw ejs message) so it can branch on it.
func TestSidecarSolverStaleJS_PersistentReturnsErrPlayerJSStale(t *testing.T) {
	fake := &fakeSidecar{
		respond: func(req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
			return sidecar.SolveCipherResult{}, errors.New("ejs solve sig: no solutions")
		},
	}
	src := &fakePlayerSource{playerID: "P1", js: "JS"}
	s := newSidecarSolverWith(fake, src)

	_, err := s.Sig(context.Background(), "P1", "in")
	if err == nil {
		t.Fatal("expected error on persistent stale-JS")
	}
	if !errors.Is(err, ErrPlayerJSStale) {
		t.Errorf("persistent stale-JS should match ErrPlayerJSStale; got: %v", err)
	}
	if got, want := len(fake.calls), 2; got != want {
		t.Errorf("expected exactly 2 calls (initial + retry); got %d", got)
	}
}

// TestSidecarSolverForceReloadProtocolContract — pins the sidecar
// JSON-RPC contract: when ForceReload is set, a fake sidecar that
// caches by playerID should drop its entry; without ForceReload, a
// repeat call to the same playerID with new bytes is silently ignored.
// This guards against the regression that motivated Stage 2 in the
// first place (cipher.js's `if (!entry)` semantics).
func TestSidecarSolverForceReloadProtocolContract(t *testing.T) {
	// fakeCachingSidecar models the sidecar's actual behaviour: caches
	// playerJS by playerID, ignores repeat playerJS unless forceReload is
	// set, returns "ejs solve sig: no solutions" when its cached JS is
	// the stale value.
	type cacheState struct {
		js string
	}
	cache := map[string]*cacheState{}
	fake := &fakeSidecar{
		respond: func(req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
			entry, ok := cache[req.PlayerID]
			if req.ForceReload {
				delete(cache, req.PlayerID)
				ok = false
			}
			if !ok {
				if req.PlayerJS == "" {
					return sidecar.SolveCipherResult{}, sidecar.ErrPlayerNotLoaded
				}
				cache[req.PlayerID] = &cacheState{js: req.PlayerJS}
				entry = cache[req.PlayerID]
			}
			// Return stale-JS error iff the cached JS is the stale value.
			if entry.js == "stale-js" {
				return sidecar.SolveCipherResult{}, errors.New("ejs solve sig: no solutions")
			}
			return sidecar.SolveCipherResult{
				SigResults: map[string]string{"in": "decoded"},
			}, nil
		},
	}
	src := &fakePlayerSource{playerID: "P1", js: "stale-js", freshJS: "fresh-js"}
	s := newSidecarSolverWith(fake, src)

	out, err := s.Sig(context.Background(), "P1", "in")
	if err != nil {
		t.Fatalf("Sig: %v", err)
	}
	if out != "decoded" {
		t.Errorf("recovered Sig: want %q got %q", "decoded", out)
	}
	// Sanity: the sidecar's cache should now hold the fresh JS, proving
	// ForceReload actually evicted the stale entry. (Without ForceReload,
	// the second SolveCipher would have ignored the new playerJS and the
	// cache would still hold "stale-js" — which our error logic would
	// then surface as a persistent stale-JS failure. The fact that we got
	// `out == "decoded"` here proves the protocol contract.)
	if cache["P1"].js != "fresh-js" {
		t.Errorf("sidecar cache should hold fresh JS after ForceReload retry; got %q", cache["P1"].js)
	}
}

func TestSidecarSolverDoubleNotLoadedReturnsWrappedError(t *testing.T) {
	fake := &fakeSidecar{
		respond: func(req sidecar.SolveCipherRequest) (sidecar.SolveCipherResult, error) {
			return sidecar.SolveCipherResult{}, sidecar.ErrPlayerNotLoaded
		},
	}
	src := &fakePlayerSource{playerID: "P1", js: "JS"}
	s := newSidecarSolverWith(fake, src)

	_, err := s.Sig(context.Background(), "P1", "in")
	if err == nil {
		t.Fatal("expected error on double ErrPlayerNotLoaded")
	}
	if errors.Is(err, sidecar.ErrPlayerNotLoaded) {
		t.Errorf("error should NOT match ErrPlayerNotLoaded sentinel anymore (it's now a permanent failure, not a recoverable cache miss): %v", err)
	}
	if !strings.Contains(err.Error(), "after retry-with-JS") {
		t.Errorf("error should mention 'after retry-with-JS' so logs distinguish from transient miss; got: %v", err)
	}
	if got, want := len(fake.calls), 2; got != want {
		t.Errorf("expected exactly 2 calls (initial + retry); got %d", got)
	}
}
