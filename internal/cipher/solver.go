package cipher

import (
	"context"
	"fmt"
	"sync"
	"time"

	gojavm "github.com/dop251/goja"
)

const (
	solverCacheSize = 3
)

// Solver manages 2-tier caching (disk -> solver) for cipher decryption.
type Solver struct {
	playerCache *PlayerCache
	stsCache    *StsCache
	solverMu    sync.RWMutex
	solverData  map[string]*Solvers // key -> compiled solvers (Goja VMs)
	solverOrder []string            // insertion order for LRU eviction
	compileMu   sync.Mutex         // serializes compilation to prevent thundering herd
	logger      interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// NewSolver creates a new cipher solver with the given cache directory.
func NewSolver(cacheDir string, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) (*Solver, error) {
	pc, err := NewPlayerCache(cacheDir, logger)
	if err != nil {
		return nil, fmt.Errorf("create player cache: %w", err)
	}

	s := &Solver{
		playerCache: pc,
		stsCache:    NewStsCache(),
		solverData:  make(map[string]*Solvers),
		logger:      logger,
	}

	// Auto-evict expired player cache entries on startup
	if err := pc.Evict(); err != nil {
		logger.Debug("[Cipher] player cache eviction skipped", "err", err)
	}

	return s, nil
}

// GetSolvers returns the sig/n solver functions for a player URL.
// Uses 2-tier caching: solver cache (compiled Goja VMs) -> disk cache (raw JS).
// Serializes compilation so only one goroutine compiles per player URL.
func (s *Solver) GetSolvers(ctx context.Context, playerURL string) (*Solvers, error) {
	playerURL = OverridePlayerURL(playerURL)
	key := CacheKey(playerURL)
	playerID := PlayerIDFromURL(playerURL)

	// Check solver cache (fast path)
	s.solverMu.RLock()
	solvers, ok := s.solverData[key]
	s.solverMu.RUnlock()
	if ok {
		return solvers, nil
	}

	// Serialize compilation — only one goroutine compiles at a time.
	// Others block here then find the result in cache.
	s.compileMu.Lock()
	defer s.compileMu.Unlock()

	// Re-check cache after acquiring compile lock (another goroutine may have compiled)
	s.solverMu.RLock()
	solvers, ok = s.solverData[key]
	s.solverMu.RUnlock()
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

func (s *Solver) compileSolver(ctx context.Context, playerURL, playerID string) (*Solvers, error) {
	s.logger.Debug("cipher: fetching player JS", "playerID", playerID)
	playerJS, err := s.playerCache.Fetch(ctx, playerURL)
	if err != nil {
		return nil, fmt.Errorf("fetch player JS for %s: %w", playerID, err)
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

	preprocessed, err := preprocessPlayer(playerJS)
	if err != nil {
		return nil, fmt.Errorf("preprocess player %s: %w", playerID, err)
	}

	s.logger.Debug("cipher: compiling solver", "playerID", playerID)
	solvers, err := getFromPrepared(preprocessed)
	if err != nil {
		return nil, fmt.Errorf("compile solver for %s: %w", playerID, err)
	}

	return solvers, nil
}

// InvalidateCache clears all cached solvers (e.g., when player JS is updated).
func (s *Solver) InvalidateCache() {
	s.solverMu.Lock()
	s.solverData = make(map[string]*Solvers)
	s.solverOrder = nil
	s.solverMu.Unlock()
}

// InvalidateSolver evicts a specific player's solver from both the in-memory
// cache and the disk player cache. Called when a solver is known to be broken
// (e.g., producing 403 errors at runtime).
func (s *Solver) InvalidateSolver(playerURL string) {
	key := CacheKey(playerURL)

	s.solverMu.Lock()
	if _, ok := s.solverData[key]; ok {
		delete(s.solverData, key)
		for i, k := range s.solverOrder {
			if k == key {
				s.solverOrder = append(s.solverOrder[:i], s.solverOrder[i+1:]...)
				break
			}
		}
	}
	s.solverMu.Unlock()

	s.playerCache.Remove(playerURL)
	s.logger.Info("cipher: invalidated solver", "playerID", PlayerIDFromURL(playerURL))
}

func (s *Solver) cacheSolvers(key string, solvers *Solvers) {
	s.solverMu.Lock()
	defer s.solverMu.Unlock()

	// If key already exists, move it to the end (most recently used)
	if _, exists := s.solverData[key]; exists {
		for i, k := range s.solverOrder {
			if k == key {
				s.solverOrder = append(s.solverOrder[:i], s.solverOrder[i+1:]...)
				break
			}
		}
	} else if len(s.solverData) >= solverCacheSize {
		// Evict the oldest entry (LRU)
		evictKey := s.solverOrder[0]
		s.solverOrder = s.solverOrder[1:]
		delete(s.solverData, evictKey)
	}

	s.solverData[key] = solvers
	s.solverOrder = append(s.solverOrder, key)
}

// getFromPrepared executes preprocessed JS code in a Goja VM and extracts sig/n functions.
// The code is expected to set _result.sig and _result.n when invoked with a _result object.
func getFromPrepared(code string) (*Solvers, error) {
	vm := gojavm.New()

	// Create result container
	resultObj := vm.NewObject()
	if err := vm.Set("_result", resultObj); err != nil {
		return nil, err
	}

	// Execute the preprocessed code
	_, err := vm.RunString(code)
	if err != nil {
		return nil, fmt.Errorf("run preprocessed code: %w", err)
	}

	solvers := &Solvers{}

	// Extract sig function
	sigVal := resultObj.Get("sig")
	if sigVal != nil && !gojavm.IsUndefined(sigVal) && !gojavm.IsNull(sigVal) {
		sigFn, ok := gojavm.AssertFunction(sigVal)
		if ok {
			solvers.Sig = func(input string) (result string, err error) {
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("sig decrypt panic: %v", r)
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
						err = fmt.Errorf("n decrypt panic: %v", r)
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
