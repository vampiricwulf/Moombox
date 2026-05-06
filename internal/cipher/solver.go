package cipher

import (
	"context"
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
	compileMu   sync.Mutex // serializes compilation to prevent thundering herd
	logger      interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
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

	// Serialize compilation — only one goroutine compiles at a time.
	// Others block here then find the result in cache.
	s.compileMu.Lock()
	defer s.compileMu.Unlock()

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

	// Fetch, preprocess, compile
	solvers, err := s.compileSolver(ctx, playerURL, playerID)
	if err != nil {
		// Don't cache errors — allow retry on next request (may be transient).
		// The compileMu serialization prevents thundering herd during this request.
		s.logger.Error("cipher: solver compilation failed", "playerID", playerID, "error", err.Error())
		return nil, err
	}

	s.cacheSolvers(key, solvers)
	s.logger.Info("cipher: solver ready", "playerID", playerID,
		"hasSig", solvers.Sig != nil, "hasN", solvers.N != nil)
	return solvers, nil
}

func (s *GojaResolver) compileSolver(ctx context.Context, playerURL, playerID string) (*Solvers, error) {
	s.logger.Debug("cipher: fetching player JS", "playerID", playerID)
	playerJS, err := s.playerCache.Fetch(ctx, playerURL)
	if err != nil {
		// PlayerCache.Fetch already includes "fetch player JS" in its message;
		// wrap with a different verb to avoid "fetch player JS: fetch player JS: ..."
		return nil, fmt.Errorf("compile solver for %s: %w", playerID, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Log extraction strategy results for debugging
	urlClassName := findURLClassName(playerJS)
	nArrayCands := findNArrayCandidates(playerJS)
	sigOldCands := findSigCandidates(playerJS)
	alrSigChains := findAlrTransformChains(playerJS)

	s.logger.Debug("cipher: extraction results", "playerID", playerID, "size", len(playerJS),
		"urlClassName", urlClassName,
		"nArrayCandidates", len(nArrayCands),
		"sigOldCandidates", len(sigOldCands),
		"alrSigChains", len(alrSigChains))

	if urlClassName == "" && len(nArrayCands) == 0 {
		s.logger.Warn("cipher: no n-param strategy succeeded (URL class + array candidates)",
			"playerID", playerID)
	}
	if len(sigOldCands) == 0 && len(alrSigChains) == 0 {
		s.logger.Warn("cipher: no sig strategy succeeded (ALR chains + old candidates)",
			"playerID", playerID)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Second-tier cache: skip the 200-500ms extraction + preprocessing
	// pass when the .preprocessed.js sidecar is on disk. Only the slow
	// path runs preprocessPlayerWithBranch + writes the sidecar back.
	// Audit reports/cipher.md Q4.
	preprocessed, ppErr := s.playerCache.GetPreprocessed(playerURL)
	if ppErr != nil {
		s.logger.Debug("cipher: preprocessed cache read failed", "playerID", playerID, "err", ppErr)
	}
	if preprocessed != "" {
		s.logger.Debug("cipher: preprocessed cache hit", "playerID", playerID)
	} else {
		var branch string
		preprocessed, branch, err = preprocessPlayerWithBranch(playerJS)
		if err != nil {
			return nil, fmt.Errorf("preprocess player %s: %w", playerID, err)
		}
		s.logger.Info("cipher: extraction branch", "playerID", playerID, "branch", branch)
		if putErr := s.playerCache.PutPreprocessed(playerURL, preprocessed); putErr != nil {
			s.logger.Debug("cipher: preprocessed cache write failed", "playerID", playerID, "err", putErr)
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.logger.Debug("cipher: compiling solver", "playerID", playerID)
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
// it with a freshly-compiled solver, we can hit an invalidation loop.
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

// getFromPrepared executes preprocessed JS code in a Goja VM and extracts sig/n functions.
// The code is expected to set _result.sig and _result.n when invoked with a _result object.
//
// goja.RunString and vm.NewObject can panic on certain malformed inputs (stack
// overflows in deep recursion, some parser states). Without recovery the
// compileSolver call site would propagate the panic up the call tree and kill
// the containing goroutine, which in the worker would crash the download path
// it ran on. Wrap the whole thing in a deferred recover so a broken player
// script produces a clean error return the caller can handle.
func getFromPrepared(code string) (solvers *Solvers, err error) {
	defer func() {
		if r := recover(); r != nil {
			solvers = nil
			err = fmt.Errorf("compile cipher solver panicked: %v", r)
		}
	}()

	// internal/goja's runtime registers real TextEncoder/TextDecoder and the
	// shared DOM shim. Empty UA → internal/goja defaults to Chrome 131. Audit
	// reports/goja.md C4 + cipher.md D1/D4.
	vm, vmErr := mbgoja.NewRuntimeForCipher("")
	if vmErr != nil {
		return nil, fmt.Errorf("init cipher VM: %w", vmErr)
	}

	// Create result container
	resultObj := vm.NewObject()
	if err := vm.Set("_result", resultObj); err != nil {
		return nil, err
	}

	// Execute the preprocessed code
	_, err = vm.RunString(code)
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

// callWithTimeout invokes a goja callable under CipherTimeout and safely resets
// the VM's interrupt flag after the call returns.
//
// The invocation runs in a dedicated goroutine so that vm.Interrupt (which must
// be called from a non-VM goroutine) can race the completion. The select waits
// for either the goroutine's result or the timeout. On timeout, vm.Interrupt is
// fired and we BLOCK on the result channel until the goroutine actually exits —
// only then do we call ClearInterrupt. This ordering is what makes the next
// call to this VM start with a clean slate; the earlier time.AfterFunc
// implementation could leave a pending interrupt that aborted the next call.
//
// The caller still owns the VM mutex (Solvers.mu) for the duration of this
// function, so there is no concurrent VM access from elsewhere.
func callWithTimeout(vm *gojavm.Runtime, label string, fn func() (gojavm.Value, error)) (string, error) {
	type callResult struct {
		val gojavm.Value
		err error
	}

	done := make(chan callResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- callResult{nil, fmt.Errorf("cipher %s panic inside goroutine: %v", label, r)}
			}
		}()
		v, e := fn()
		done <- callResult{v, e}
	}()

	timer := time.NewTimer(CipherTimeout)
	defer timer.Stop()

	var r callResult
	select {
	case r = <-done:
		// Normal path — no interrupt fired. ClearInterrupt below is a no-op
		// but keeps the cleanup symmetric.
	case <-timer.C:
		vm.Interrupt(fmt.Sprintf("cipher %s decrypt timeout (%v)", label, CipherTimeout))
		r = <-done // wait for the goroutine to observe the interrupt and return
	}
	vm.ClearInterrupt()

	if r.err != nil {
		return "", fmt.Errorf("%s decrypt: %w", label, r.err)
	}
	return r.val.String(), nil
}
