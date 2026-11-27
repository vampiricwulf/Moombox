package cipher

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	gojavm "github.com/dop251/goja"
	mbgoja "github.com/vampiricwulf/Moombox/internal/goja"
)

// solverCacheSize caps the in-memory LRU. VM heap is ~30-50MB each × 10 =
// ~500MB worst case. Multi-channel monitoring routinely has 4+ active player
// URLs; the previous cap of 3 caused constant re-compiles when the 4th video
// probed. 10 leaves comfortable headroom while still bounding memory.
const solverCacheSize = 10

// GojaResolver manages 2-tier caching (disk -> solver) for cipher decryption.
// It implements the Solver interface using in-process Goja VMs.
type GojaResolver struct {
	playerCache *PlayerCache
	stsCache    *StsCache
	solverMu    sync.RWMutex
	solverData  map[string]*Solvers // key -> compiled solvers (Goja VMs); read/write under solverMu
	// solverOrder MUST only be read or written under solverMu.Lock() — every
	// helper that touches it (cacheSolvers, touchLRU, InvalidateSolver) uses
	// in-place slice ops that alias the backing array. Reading it from
	// elsewhere without the lock would race with mutation. (audit cipher.md H4)
	solverOrder []string
	compileSlot compileSlot // serializes compilation to prevent thundering herd
	// timedOut maps the CacheKey of a player whose compile ran out of
	// compileTimeout, or whose body the port could not prepare
	// (ErrExtractorMismatch), to when it may be tried again and why it was
	// refused. Read and written under timedOutMu; held to
	// maxTimedOutPlayers entries.
	timedOutMu sync.Mutex
	timedOut   map[string]refusal
	logger     interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// compileSlot is a lock a waiter can give up on: one holder at a time, and a
// caller waiting for it leaves when its context ends. The zero value is free.
type compileSlot struct {
	once sync.Once
	ch   chan struct{}
}

// acquire takes the slot, or returns ctx's error once ctx ends first. A
// caller that gets nil must call release.
func (c *compileSlot) acquire(ctx context.Context) error {
	c.once.Do(func() { c.ch = make(chan struct{}, 1) })
	select {
	case c.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *compileSlot) release() { <-c.ch }

// compileTimeout bounds the goja run of a whole preprocessed player (the
// compile): a variable so a test can shorten it. Generous, because a slow
// arm64 box must still finish a working player well inside it.
var compileTimeout = 120 * time.Second

// compileRefusal is how long a player whose compile ran out of
// compileTimeout, or whose body the port could not prepare, is refused
// instead of tried again. Without it every later request for that player
// re-ran the script for the full bound, or fetched and parsed the body again
// (about 0.1 s and 110 MB at peak), while holding the compile slot. A body
// that changes under the player's URL ends the refusal (unrefuse).
const compileRefusal = 10 * time.Minute

// refusal is one entry of the refusal list: until when, and the failure
// that put it there (wrapping mbgoja.ErrExecutionTimeout or
// ErrExtractorMismatch).
type refusal struct {
	until time.Time
	cause error
}

// maxTimedOutPlayers caps the refusal list. Players rotate a few times a
// week, so more than a handful at once means the list is being churned;
// the entry closest to expiry goes first.
const maxTimedOutPlayers = 16

// resolverNow is the clock of the refusal list, a variable so a test can
// move past compileRefusal without waiting it out.
var resolverNow = time.Now

// ErrCompileRefused is what GetSolvers returns for a player whose compile
// ran out of compileTimeout less than compileRefusal ago.
var ErrCompileRefused = errors.New("cipher: player compile timed out recently; not compiling it again yet")

// refusedUntil returns when the player under key may be compiled again, or
// the zero time when it is not refused. An expired entry is dropped.
func (s *GojaResolver) refusedUntil(key string) time.Time {
	return s.refusalOf(key).until
}

// refusalOf returns the player's refusal, the zero one when it is not
// refused. An expired entry is dropped.
func (s *GojaResolver) refusalOf(key string) refusal {
	s.timedOutMu.Lock()
	defer s.timedOutMu.Unlock()
	r, ok := s.timedOut[key]
	if !ok {
		return refusal{}
	}
	if !resolverNow().Before(r.until) {
		delete(s.timedOut, key)
		return refusal{}
	}
	return r
}

// unrefuse ends the player's refusal: its body changed, so the failure on
// the old one says nothing about it.
func (s *GojaResolver) unrefuse(key string) {
	s.timedOutMu.Lock()
	defer s.timedOutMu.Unlock()
	delete(s.timedOut, key)
}

// refuse records that the player under key failed with cause: its compile
// timed out, or the port could not prepare its body.
func (s *GojaResolver) refuse(key string, cause error) {
	s.timedOutMu.Lock()
	defer s.timedOutMu.Unlock()
	now := resolverNow()
	if s.timedOut == nil {
		s.timedOut = make(map[string]refusal)
	}
	for k, r := range s.timedOut {
		if !now.Before(r.until) {
			delete(s.timedOut, k)
		}
	}
	for len(s.timedOut) >= maxTimedOutPlayers {
		var oldest string
		for k, r := range s.timedOut {
			if oldest == "" || r.until.Before(s.timedOut[oldest].until) {
				oldest = k
			}
		}
		delete(s.timedOut, oldest)
	}
	s.timedOut[key] = refusal{until: now.Add(compileRefusal), cause: cause}
}

// refusedErr is the error for a refused player, or nil when it is not. A
// player the port could not prepare is refused with that failure, so the
// error still wraps ErrExtractorMismatch.
func (s *GojaResolver) refusedErr(key, playerID string) error {
	r := s.refusalOf(key)
	if r.until.IsZero() {
		return nil
	}
	until := r.until.Format(time.RFC3339)
	if errors.Is(r.cause, ErrExtractorMismatch) {
		s.logger.Debug("cipher: the goja port could not prepare this player lately; not preparing it again yet",
			"playerID", playerID, "until", until)
		return fmt.Errorf("cipher: player %s not prepared again until %s: %w", playerID, until, r.cause)
	}
	return fmt.Errorf("%w (player %s, until %s)", ErrCompileRefused, playerID, until)
}

// NewGojaResolver creates a new GojaResolver with the given cache directory.
func NewGojaResolver(cacheDir string, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) (*GojaResolver, error) {
	pc, err := NewPlayerCache(cacheDir, logger)
	if err != nil {
		return nil, fmt.Errorf("create player cache: %w", err)
	}

	s := &GojaResolver{
		playerCache: pc,
		stsCache:    NewStsCache(),
		solverData:  make(map[string]*Solvers),
		logger:      logger,
	}

	// Auto-evict expired player cache entries on startup
	if err := pc.Evict(); err != nil {
		logger.Debug("[Cipher] player cache eviction skipped", "err", err)
	}

	// Background sweep: GojaResolver lives for the process lifetime. The 14-day
	// TTL on disk entries means a long-running 24/7 process would never
	// evict without this — startup-only eviction leaves entries for the
	// full window. Goroutine runs forever; panic recovery per CLAUDE.md
	// keeps a filesystem hiccup from killing it.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("cipher: disk eviction goroutine panic", "panic", r)
			}
		}()
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			if err := pc.Evict(); err != nil {
				logger.Warn("cipher: periodic disk eviction failed", "err", err)
			}
		}
	}()

	return s, nil
}

// GetSolvers returns the sig/n solver functions for a player URL.
// Uses 2-tier caching: solver cache (compiled Goja VMs) -> disk cache (raw JS).
// Serializes compilation so only one goroutine compiles per player URL.
func (s *GojaResolver) GetSolvers(ctx context.Context, playerURL string) (*Solvers, error) {
	key := CacheKey(playerURL)
	playerID := PlayerIDFromURL(playerURL)

	// Check solver cache (fast path). Touch the LRU order on hit so the
	// cap=10 window reflects actual access patterns rather than first-insertion.
	s.solverMu.Lock()
	solvers, ok := s.solverData[key]
	if ok {
		s.touchLRU(key)
	}
	s.solverMu.Unlock()
	if ok {
		return solvers, nil
	}
	if err := s.refusedErr(key, playerID); err != nil {
		return nil, err
	}

	// Serialize compilation — only one goroutine compiles at a time.
	// Others wait here then find the result in cache. A caller whose context
	// ends while it waits leaves then: errors are not cached, so with the
	// player CDN down every caller ahead of it repeats the cold fetch (about
	// 94s each), and behind a plain mutex a cancelled job, or a shutdown,
	// waited out all of them.
	if err := s.compileSlot.acquire(ctx); err != nil {
		return nil, err
	}
	defer s.compileSlot.release()

	// Re-check cache after acquiring compile lock (another goroutine may have compiled).
	// Also touch the LRU order to reflect the access.
	s.solverMu.Lock()
	solvers, ok = s.solverData[key]
	if ok {
		s.touchLRU(key)
	}
	s.solverMu.Unlock()
	if ok {
		return solvers, nil
	}
	// A caller that queued behind a compile that timed out is refused too,
	// rather than running the same script for the full bound again.
	if err := s.refusedErr(key, playerID); err != nil {
		return nil, err
	}

	// Fetch, preprocess, compile
	solvers, err := s.compileSolver(ctx, playerURL, playerID)
	if err != nil {
		// Don't cache errors — allow retry on next request (may be transient).
		// The compileSlot serialization prevents thundering herd during this request.
		// The exceptions are failures the player's body decides: a compile
		// that ran out of compileTimeout, and a body the port could not
		// prepare (ErrExtractorMismatch; a fetch failure never wraps it). The
		// body is the same next time, so that player is refused for
		// compileRefusal. A cancelled context is the caller's, not the player's.
		if errors.Is(err, mbgoja.ErrExecutionTimeout) {
			s.refuse(key, err)
			s.logger.Error("cipher: solver compilation timed out; refusing this player for a while",
				"playerID", playerID, "timeout", compileTimeout, "refusedFor", compileRefusal)
			return nil, err
		}
		if errors.Is(err, ErrExtractorMismatch) {
			s.refuse(key, err)
			s.logger.Warn("cipher: the goja port could not prepare this player; not trying it again for a while",
				"playerID", playerID, "refusedFor", compileRefusal, "error", err.Error())
			return nil, err
		}
		s.logger.Error("cipher: solver compilation failed", "playerID", playerID, "error", err.Error())
		return nil, err
	}

	s.cacheSolvers(key, solvers)
	s.logger.Debug("cipher: solver ready", "playerID", playerID,
		"hasSig", solvers.Sig != nil, "hasN", solvers.N != nil)
	return solvers, nil
}

func (s *GojaResolver) compileSolver(ctx context.Context, playerURL, playerID string) (*Solvers, error) {
	s.logger.Debug("cipher: fetching player JS", "playerID", playerID)
	playerJS, changed, err := s.playerCache.Fetch(ctx, playerURL)
	if err != nil {
		// PlayerCache.Fetch already includes "fetch player JS" in its message;
		// wrap with a different verb to avoid "fetch player JS: fetch player JS: ..."
		return nil, fmt.Errorf("compile solver for %s: %w", playerID, err)
	}
	// Fetch already removed the prepared copy on changed=true; we
	// only need to drop in-memory solverData here. compileSolver is normally
	// called when solverData has no entry, so this is mostly defensive against
	// future call sites and against the path where Fetch coincidentally
	// rotates the JS while compileSolver is racing in.
	if changed {
		s.dropSolverData(CacheKey(playerURL))
		s.unrefuse(CacheKey(playerURL))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Second-tier cache: skip the parse and match (about 0.1 s) when the
	// prepared copy is on disk. Only the slow path runs preprocessPlayer
	// and writes the copy back. Audit reports/cipher.md Q4.
	preprocessed, ppErr := s.playerCache.GetPreprocessed(playerURL)
	if ppErr != nil {
		s.logger.Debug("cipher: preprocessed cache read failed", "playerID", playerID, "err", ppErr)
	}
	if preprocessed != "" {
		s.logger.Debug("cipher: preprocessed cache hit", "playerID", playerID)
	} else {
		var st prepareStats
		preprocessed, st, err = preprocessPlayer(playerJS)
		if err != nil {
			return nil, fmt.Errorf("preprocess player %s: %w", playerID, err)
		}
		s.logger.Debug("cipher: player prepared", "playerID", playerID, "size", len(playerJS),
			"solvers", st.solvers, "routes", st.routes, "stringPool", st.pool,
			"kept", st.kept, "dropped", st.dropped)
		if putErr := s.playerCache.PutPreprocessed(playerURL, preprocessed); putErr != nil {
			s.logger.Debug("cipher: preprocessed cache write failed", "playerID", playerID, "err", putErr)
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// The caller's context bounds the fetch and the wait for the slot, not
	// the compile: a compile cut short by a short-deadline caller would fill
	// no cache, and the next caller would start again from zero.
	s.logger.Debug("cipher: compiling solver", "playerID", playerID)
	compileStarting()
	solvers, err := getFromPrepared(preprocessed)
	if err != nil {
		return nil, fmt.Errorf("compile cipher solver for %s: %w", playerID, err)
	}

	return solvers, nil
}

// InvalidateSolver evicts a specific player's solver from both the in-memory
// cache and the disk player cache. Called when a solver is known to be broken
// (e.g., producing 403 errors at runtime). The STS cache keyed on the same
// player is also dropped — if we keep the stale signatureTimestamp and pair
// it with a freshly-compiled solver, we can hit an invalidation loop. The
// player's sig specs go with its disk entry (PlayerCache.Remove): a spec the
// sidecar's stale-JS recovery has just called into question must not answer
// the next signature in its place.
func (s *GojaResolver) InvalidateSolver(playerURL string) {
	key := CacheKey(playerURL)

	s.solverMu.Lock()
	if _, ok := s.solverData[key]; ok {
		delete(s.solverData, key)
		// Copy-based removal — avoids aliasing the backing array if any
		// future reader iterates solverOrder under RLock (audit H4).
		for i, k := range s.solverOrder {
			if k == key {
				newOrder := make([]string, 0, len(s.solverOrder)-1)
				newOrder = append(newOrder, s.solverOrder[:i]...)
				newOrder = append(newOrder, s.solverOrder[i+1:]...)
				s.solverOrder = newOrder
				break
			}
		}
	}
	s.solverMu.Unlock()

	if s.stsCache != nil {
		s.stsCache.InvalidateKey(key)
	}
	s.playerCache.Remove(playerURL)
	s.unrefuse(key)
	s.logger.Info("cipher: invalidated solver", "playerID", PlayerIDFromURL(playerURL))
}

func (s *GojaResolver) cacheSolvers(key string, solvers *Solvers) {
	s.solverMu.Lock()
	defer s.solverMu.Unlock()

	// If key already exists, refresh its position via touchLRU (which also
	// handles the "was missing" no-op path cleanly).
	if _, exists := s.solverData[key]; exists {
		s.touchLRU(key)
	} else if len(s.solverData) >= solverCacheSize {
		// Evict the oldest entry (head of order slice).
		evictKey := s.solverOrder[0]
		s.solverOrder = s.solverOrder[1:]
		delete(s.solverData, evictKey)
	}

	s.solverData[key] = solvers
	if !containsKey(s.solverOrder, key) {
		s.solverOrder = append(s.solverOrder, key)
	}
}

// touchLRU moves key to the end of solverOrder so the LRU window reflects
// the most-recently-USED entries, not the most-recently-INSERTED ones.
// Caller must hold s.solverMu.Lock() (not RLock — this mutates).
func (s *GojaResolver) touchLRU(key string) {
	for i, k := range s.solverOrder {
		if k == key {
			s.solverOrder = append(append(s.solverOrder[:i], s.solverOrder[i+1:]...), key)
			return
		}
	}
}

func containsKey(order []string, key string) bool {
	return slices.Contains(order, key)
}

// compileStarting runs after compileSolver's last look at the caller's
// context and just before the compile. Production leaves it empty; a test
// swaps it to act at exactly that point (cancel the caller).
var compileStarting = func() {}

// getFromPrepared is ejs getFromPrepared: it runs a prepared player
// (preprocessPlayer) as the body of function(_result){...}, as ejs runs it
// with Function("_result", code), and takes the sig and n functions it leaves
// on _result. The runtime is a bare one (mbgoja.NewPlainRuntime, with the
// call-stack bound): the prepared copy carries ejs setup.ts, the only globals
// the player needs to load, and no DOM shim is registered.
//
// goja.RunString and vm.NewObject can panic on certain malformed inputs (stack
// overflows in deep recursion, some parser states). Without recovery the
// compileSolver call site would propagate the panic up the call tree and kill
// the containing goroutine, which in the worker would crash the download path
// it ran on. Wrap the whole thing in a deferred recover so a broken player
// script produces a clean error return the caller can handle.
//
// The run is bounded by compileTimeout alone: its end interrupts the script
// (a JS-level loop; a hang inside a native Go call is not reached by an
// interrupt), and the error then wraps mbgoja.ErrExecutionTimeout.
func getFromPrepared(code string) (solvers *Solvers, err error) {
	defer func() {
		if r := recover(); r != nil {
			solvers = nil
			err = fmt.Errorf("compile cipher solver panicked: %v", r)
		}
	}()

	vm := mbgoja.NewPlainRuntime()

	// Create result container
	resultObj := vm.NewObject()
	if err := vm.Set("_result", resultObj); err != nil {
		return nil, err
	}

	// Run the prepared code. The newline before the close keeps a trailing
	// line comment in the player from swallowing it.
	_, err = mbgoja.RunStringWithTimeout(context.Background(), vm, "(function(_result){"+code+"\n})(_result)", compileTimeout)
	if err != nil {
		return nil, fmt.Errorf("run preprocessed code: %w", err)
	}

	solvers = &Solvers{}

	// Extract sig function
	sigVal := resultObj.Get("sig")
	if sigVal != nil && !gojavm.IsUndefined(sigVal) && !gojavm.IsNull(sigVal) {
		sigFn, ok := gojavm.AssertFunction(sigVal)
		if ok {
			solvers.Sig = func(input string) (result string, err error) {
				defer func() {
					if r := recover(); r != nil {
						solvers.MarkDead()
						err = fmt.Errorf("sig decrypt panic (solver poisoned): %v", r)
					}
				}()
				return callWithTimeout(vm, "sig", func() (gojavm.Value, error) {
					return sigFn(gojavm.Undefined(), vm.ToValue(input))
				})
			}
		}
	}

	// Extract n function
	nVal := resultObj.Get("n")
	if nVal != nil && !gojavm.IsUndefined(nVal) && !gojavm.IsNull(nVal) {
		nFn, ok := gojavm.AssertFunction(nVal)
		if ok {
			solvers.N = func(input string) (result string, err error) {
				defer func() {
					if r := recover(); r != nil {
						solvers.MarkDead()
						err = fmt.Errorf("n decrypt panic (solver poisoned): %v", r)
					}
				}()
				return callWithTimeout(vm, "n", func() (gojavm.Value, error) {
					return nFn(gojavm.Undefined(), vm.ToValue(input))
				})
			}
		}
	}

	return solvers, nil
}

// playerURLForID rebuilds the canonical YouTube player JS URL from an
// 11-character playerID. The URL shape is stable across the player
// versions Moombox supports; the cipher.Solver interface speaks in
// playerIDs (matching how the JS sidecar identifies players) while
// the in-process GojaResolver internals key on URLs (matching how
// they appear in YouTube responses). Reconstructing here keeps the
// internals untouched.
//
// TODO: this URL shape is asserted to be stable across Moombox-
// supported player versions. If YouTube ever serves players from a
// different path (e.g., a regionalised CDN host or a versioned
// path scheme), this helper and any cache identity that flows through
// it (CacheKey playerID-hash, sidecar's per-player cache keyed on
// playerID) will need to learn about the new shape.
func playerURLForID(playerID string) string {
	return playerVariantURL(playerID, mainVariantPath)
}

// Sig satisfies cipher.Solver. Returns ErrSigUnavailable only when the
// prepared copy left no sig function. A copy from preprocessPlayer always
// has one (it sets _result.n and _result.sig together), so for a real player
// this is effectively unreachable: a player the port cannot match fails
// earlier, in GetSolvers, with ErrExtractorMismatch.
func (s *GojaResolver) Sig(ctx context.Context, playerID, encryptedSig string) (string, error) {
	solvers, err := s.GetSolvers(ctx, playerURLForID(playerID))
	if err != nil {
		return "", err
	}
	if !solvers.HasSig() {
		return "", ErrSigUnavailable
	}
	return solvers.DecryptSig(encryptedSig)
}

// N satisfies cipher.Solver. Returns a non-sentinel error when the
// prepared copy left no n function, which, as for Sig, a copy from
// preprocessPlayer never does: a player the port cannot match fails earlier,
// in GetSolvers, with ErrExtractorMismatch. Callers should treat either as a
// hard failure rather than substituting a fallback.
func (s *GojaResolver) N(ctx context.Context, playerID, encryptedN string) (string, error) {
	solvers, err := s.GetSolvers(ctx, playerURLForID(playerID))
	if err != nil {
		return "", err
	}
	if !solvers.HasN() {
		return "", fmt.Errorf("cipher: n unavailable for player %s", playerID)
	}
	return solvers.DecryptN(encryptedN)
}

// Batch satisfies cipher.Solver. Goja has no round-trip overhead so
// the implementation is a straight loop. ErrSigUnavailable on a sig
// challenge skips the entry rather than failing the batch — the
// missing key in the result map signals "unavailable for this
// player" to callers that handle that case (today: the composite
// solver routes around an unavailable sig). Direct callers of
// GojaResolver.Batch (rare; composite is the typical entry point)
// must not assume every input has an output entry; check len(sigs)
// vs len(sigResults) if a fully-populated map is required.
func (s *GojaResolver) Batch(ctx context.Context, playerID string, sigs, ns []string) (map[string]string, map[string]string, error) {
	sigResults := make(map[string]string, len(sigs))
	for _, sig := range sigs {
		out, err := s.Sig(ctx, playerID, sig)
		if err != nil {
			if errors.Is(err, ErrSigUnavailable) {
				continue
			}
			return nil, nil, err
		}
		sigResults[sig] = out
	}
	nResults := make(map[string]string, len(ns))
	for _, n := range ns {
		out, err := s.N(ctx, playerID, n)
		if err != nil {
			return nil, nil, err
		}
		nResults[n] = out
	}
	return sigResults, nResults, nil
}

// PlayerJS satisfies cipher.PlayerSource. Returns the raw player JS
// source for the given playerID by reusing the goja path's existing
// player_cache.go fetch + cache. Translates playerID → canonical URL
// internally so the cache identity is the same as for in-process
// solving — one fetch serves both the goja extractor and the sidecar
// solver.
//
// When Fetch detects a content change (proactive revalidation, Stage 1)
// the in-memory compiled solver for this player is dropped too — without
// that, the goja fallback path would happily keep serving the old solver
// against fresh JS from YouTube and produce 403s at the CDN.
func (s *GojaResolver) PlayerJS(playerID string) (string, error) {
	playerURL := playerURLForID(playerID)
	js, changed, err := s.playerCache.Fetch(context.Background(), playerURL)
	if err != nil {
		return "", err
	}
	if changed {
		s.dropSolverData(CacheKey(playerURL))
		s.unrefuse(CacheKey(playerURL))
	}
	return js, nil
}

// RemovePlayerJS satisfies cipher.PlayerSource. Drops every cache layer
// for the given playerID — disk raw + preprocessed + meta, in-memory
// compiled solver, STS entry — so the next solver request re-fetches
// from YouTube. Equivalent to InvalidateSolver but expressed in
// playerID terms (matching the sidecar's identity model).
func (s *GojaResolver) RemovePlayerJS(playerID string) error {
	s.InvalidateSolver(playerURLForID(playerID))
	return nil
}

// dropSolverData evicts the compiled solver entry for a given cache key
// without touching disk caches (Fetch owns its own files). Called when
// the underlying player JS is known to have rotated — see compileSolver
// and PlayerJS. Mirrors the in-memory portion of InvalidateSolver but
// without the disk-side Remove (which Fetch has already handled
// surgically: only the preprocessed sidecar, not the freshly-written
// raw JS we still want).
func (s *GojaResolver) dropSolverData(key string) {
	s.solverMu.Lock()
	defer s.solverMu.Unlock()
	if _, ok := s.solverData[key]; !ok {
		return
	}
	delete(s.solverData, key)
	for i, k := range s.solverOrder {
		if k == key {
			newOrder := make([]string, 0, len(s.solverOrder)-1)
			newOrder = append(newOrder, s.solverOrder[:i]...)
			newOrder = append(newOrder, s.solverOrder[i+1:]...)
			s.solverOrder = newOrder
			break
		}
	}
}

// sigSpec satisfies sigSpecStore: the spec learned for signatures of this
// length on this player, from its player cache entry.
func (s *GojaResolver) sigSpec(playerID string, length int) ([]int, bool) {
	return s.playerCache.GetSigSpec(playerURLForID(playerID), length)
}

// putSigSpec satisfies sigSpecStore. A failed write only costs the spec: the
// next signature of this length learns it again.
func (s *GojaResolver) putSigSpec(playerID string, length int, spec []int) {
	if err := s.playerCache.PutSigSpec(playerURLForID(playerID), length, spec); err != nil {
		s.logger.Debug("cipher: sig spec write failed", "playerID", playerID, "err", err)
	}
}

// Compile-time check: GojaResolver satisfies the cipher.Solver interface.
var _ Solver = (*GojaResolver)(nil)

// Compile-time check: GojaResolver satisfies cipher.PlayerSource.
var _ PlayerSource = (*GojaResolver)(nil)

// Compile-time check: GojaResolver is the composite solver's sig spec store.
var _ sigSpecStore = (*GojaResolver)(nil)

// callTimeout is the bound callWithTimeout puts on a call: CipherTimeout, a
// variable so a test can interrupt a hung call without waiting it out.
var callTimeout = CipherTimeout

// callWithTimeout invokes a goja callable under CipherTimeout via the shared
// mbgoja.CallWithTimeout helper, which owns the Interrupt/ClearInterrupt
// ordering contract (fire, block until the goroutine exits, only then
// clear). The caller still owns the VM mutex (Solvers.mu) for the duration
// of this function, so there is no concurrent VM access from elsewhere.
func callWithTimeout(vm *gojavm.Runtime, label string, fn func() (gojavm.Value, error)) (string, error) {
	v, err := mbgoja.CallWithTimeout(vm, callTimeout, "cipher "+label, fn)
	if err != nil {
		return "", fmt.Errorf("%s decrypt: %w", label, err)
	}
	// ejs answers null when the URL it built holds no answer; String() would
	// make that the text "null" and put it in a URL.
	if v == nil || gojavm.IsUndefined(v) || gojavm.IsNull(v) {
		return "", fmt.Errorf("%s decrypt: the solver gave no answer", label)
	}
	return v.String(), nil
}
