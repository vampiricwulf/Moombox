package cipher

import (
	"context"
	"fmt"
	"sync"

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
	pc, err := NewPlayerCache(cacheDir)
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

	nArrayCands := findNArrayCandidates(playerJS)
	sigOldCands := findSigCandidates(playerJS)
	alrSigChain := findAlrTransformChain(playerJS)
	s.logger.Debug("cipher: preprocessing player JS", "playerID", playerID, "size", len(playerJS),
		"nArrayCandidates", len(nArrayCands), "sigOldCandidates", len(sigOldCands),
		"hasAlrSigChain", alrSigChain != "")
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
	s.solverMu.Unlock()
}

func (s *Solver) cacheSolvers(key string, solvers *Solvers) {
	s.solverMu.Lock()
	defer s.solverMu.Unlock()

	if len(s.solverData) >= solverCacheSize {
		for k := range s.solverData {
			delete(s.solverData, k)
			break
		}
	}
	s.solverData[key] = solvers
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
			solvers.Sig = func(input string) (string, error) {
				result, err := sigFn(gojavm.Undefined(), vm.ToValue(input))
				if err != nil {
					return "", fmt.Errorf("sig decrypt: %w", err)
				}
				return result.String(), nil
			}
		}
	}

	// Extract n function
	nVal := resultObj.Get("n")
	if nVal != nil && !gojavm.IsUndefined(nVal) && !gojavm.IsNull(nVal) {
		nFn, ok := gojavm.AssertFunction(nVal)
		if ok {
			solvers.N = func(input string) (string, error) {
				result, err := nFn(gojavm.Undefined(), vm.ToValue(input))
				if err != nil {
					return "", fmt.Errorf("n decrypt: %w", err)
				}
				return result.String(), nil
			}
		}
	}

	return solvers, nil
}
