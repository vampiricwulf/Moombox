package cipher

import (
	"context"
	"fmt"
	"sync"

	gojavm "github.com/dop251/goja"
)

const (
	preprocessedCacheSize = 150
	solverCacheSize       = 50
)

// Solver manages 3-tier caching (disk -> preprocessed -> solver) for cipher decryption.
type Solver struct {
	playerCache      *PlayerCache
	stsCache         *StsCache
	preprocessedMu   sync.RWMutex
	preprocessedData map[string]string // key -> preprocessed JS code
	solverMu         sync.RWMutex
	solverData       map[string]*Solvers // key -> compiled solvers
	logger           interface {
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
		playerCache:      pc,
		stsCache:         NewStsCache(),
		preprocessedData: make(map[string]string),
		solverData:       make(map[string]*Solvers),
		logger:           logger,
	}

	// Auto-evict expired player cache entries on startup
	if err := pc.Evict(); err != nil {
		logger.Debug("[Cipher] player cache eviction skipped", "err", err)
	}

	return s, nil
}

// GetSolvers returns the sig/n solver functions for a player URL.
// Uses 3-tier caching: solver cache -> preprocessed cache -> disk cache.
func (s *Solver) GetSolvers(ctx context.Context, playerURL string) (*Solvers, error) {
	key := CacheKey(playerURL)
	playerID := PlayerIDFromURL(playerURL)

	// Tier 3: Check solver cache
	s.solverMu.RLock()
	solvers, ok := s.solverData[key]
	s.solverMu.RUnlock()
	if ok {
		s.logger.Debug("cipher: solver cache hit", "playerID", playerID)
		return solvers, nil
	}

	// Tier 2: Check preprocessed cache
	s.preprocessedMu.RLock()
	preprocessed, ok := s.preprocessedData[key]
	s.preprocessedMu.RUnlock()

	if ok {
		s.logger.Debug("cipher: preprocessed cache hit", "playerID", playerID)
		solvers, err := getFromPrepared(preprocessed)
		if err != nil {
			return nil, fmt.Errorf("execute preprocessed code: %w", err)
		}
		s.cacheSolvers(key, solvers)
		return solvers, nil
	}

	// Tier 1: Fetch player JS (from disk cache or network)
	s.logger.Debug("cipher: fetching player JS", "playerID", playerID)
	playerJS, err := s.playerCache.Fetch(ctx, playerURL)
	if err != nil {
		return nil, fmt.Errorf("fetch player JS for %s: %w", playerID, err)
	}

	// Preprocess
	s.logger.Debug("cipher: preprocessing player JS", "playerID", playerID)
	preprocessed, err = preprocessPlayer(playerJS)
	if err != nil {
		return nil, fmt.Errorf("preprocess player %s: %w", playerID, err)
	}
	s.cachePreprocessed(key, preprocessed)

	// Execute
	s.logger.Debug("cipher: executing preprocessed code", "playerID", playerID)
	solvers, err = getFromPrepared(preprocessed)
	if err != nil {
		return nil, fmt.Errorf("execute preprocessed code for %s: %w", playerID, err)
	}
	s.cacheSolvers(key, solvers)

	s.logger.Info("cipher: solver ready", "playerID", playerID,
		"hasSig", solvers.Sig != nil, "hasN", solvers.N != nil)
	return solvers, nil
}

// InvalidateCache clears all cached solvers (e.g., when player JS is updated).
func (s *Solver) InvalidateCache() {
	s.solverMu.Lock()
	s.solverData = make(map[string]*Solvers)
	s.solverMu.Unlock()

	s.preprocessedMu.Lock()
	s.preprocessedData = make(map[string]string)
	s.preprocessedMu.Unlock()
}

func (s *Solver) cachePreprocessed(key, code string) {
	s.preprocessedMu.Lock()
	defer s.preprocessedMu.Unlock()

	// Simple size bound: evict oldest if too many
	if len(s.preprocessedData) >= preprocessedCacheSize {
		// Remove one entry (not LRU, just any)
		for k := range s.preprocessedData {
			delete(s.preprocessedData, k)
			break
		}
	}
	s.preprocessedData[key] = code
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
