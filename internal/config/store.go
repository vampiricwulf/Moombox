package config

import (
	"errors"
	"sync"
)

// Store wraps *MoomboxConfig with a RWMutex (embedded or externally shared)
// and an optional save path. It offers two primary APIs:
//
//   - Read(fn) — run a read-only function with the config under RLock.
//     Multiple Read calls proceed concurrently; Update is blocked until
//     all outstanding Reads release.
//   - Update(fn) — run a mutation function under the write lock; Validate
//     the result; Normalize; save to disk when savePath is set. Returns
//     joined validation errors without committing on failure.
//
// Store is the long-term replacement for the external-cfgMu pattern
// (DECISIONS #8). It coexists with the pre-refactor callers during the
// gradual migration — RWMutex() exposes the underlying lock for legacy
// sites that still do manual cfgMu.RLock()/Lock(), and Config() returns
// the raw *MoomboxConfig pointer for dep-injected APIs that take both.
//
// During gradual migration, construct the Store via NewStoreWithMutex so
// it shares the same critical section as legacy cfgMu callers. Once all
// legacy callers have moved to Read/Update, NewStore's embedded-mutex
// variant becomes preferred.
//
// Zero-value Store is not usable — construct via NewStore / NewStoreWithMutex.
type Store struct {
	mu       *sync.RWMutex
	cfg      *MoomboxConfig
	savePath string
}

// NewStore returns a Store wrapping cfg with its own embedded RWMutex.
// savePath may be empty to skip the auto-save step in Update; that's
// useful during startup before a final config path is negotiated.
func NewStore(cfg *MoomboxConfig, savePath string) *Store {
	return &Store{mu: &sync.RWMutex{}, cfg: cfg, savePath: savePath}
}

// NewStoreWithMutex returns a Store sharing an external RWMutex. Use this
// during the gradual migration so the Store's Read/Update and legacy
// cfgMu.RLock()/Lock() sites share the same critical section — without
// that, new-API and legacy callers would race. Once all legacy callers
// have migrated, NewStore is preferred.
func NewStoreWithMutex(cfg *MoomboxConfig, savePath string, mu *sync.RWMutex) *Store {
	return &Store{mu: mu, cfg: cfg, savePath: savePath}
}

// Read runs fn with the config held under a read lock. Multiple concurrent
// Read calls proceed in parallel; Update is blocked for the duration of
// every outstanding Read. fn must not mutate the config — any required
// mutation goes through Update. fn must not spawn async work that
// continues to read *MoomboxConfig after fn returns, since the lock is
// released when fn exits; callers that need post-return access should
// copy the specific fields they need inside fn.
//
// This is the safe replacement for the old
//
//	cfgMu.RLock(); x := cfg.Something; cfgMu.RUnlock()
//
// pattern — the field read happens while the lock is held.
func (s *Store) Read(fn func(*MoomboxConfig)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(s.cfg)
}

// Update runs fn with the write lock held, then validates the result,
// normalizes, and saves to disk when savePath is set. On validation failure
// the in-memory config is NOT rolled back automatically (fn may have
// mutated it) but the error is returned and the saved file is unchanged.
// Callers that need transactional semantics should deep-copy before
// calling fn and re-apply on error.
//
// Returns a joined error when validation fails, a save error otherwise.
func (s *Store) Update(fn func(*MoomboxConfig)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.cfg)
	if errs := Validate(s.cfg); len(errs) > 0 {
		return errors.Join(errs...)
	}
	Normalize(s.cfg)
	if s.savePath == "" {
		return nil
	}
	return Save(s.cfg, s.savePath)
}

// SetSavePath installs or overrides the path used by Update for auto-save.
// Useful when the path is negotiated after NewStore (first-run wizard,
// test harnesses).
func (s *Store) SetSavePath(path string) {
	s.mu.Lock()
	s.savePath = path
	s.mu.Unlock()
}

// RWMutex returns the underlying read/write mutex for legacy call sites
// that still do manual RLock()/Lock() around direct *MoomboxConfig access.
// New code should use Read/Update instead.
//
// DEPRECATED: kept only for the gradual migration from external cfgMu to
// Store. Planned removal once all cfgMu-holding call sites have migrated.
func (s *Store) RWMutex() *sync.RWMutex {
	return s.mu
}

// Config returns the raw *MoomboxConfig pointer for dependency-injected
// APIs that take both a *MoomboxConfig and a *sync.RWMutex (web.Server,
// routes.* constructors, etc.). Equivalent to Snapshot() but named to
// signal the raw-access intent.
//
// DEPRECATED: kept only for the gradual migration; new APIs should take
// *Store directly.
func (s *Store) Config() *MoomboxConfig {
	return s.cfg
}
