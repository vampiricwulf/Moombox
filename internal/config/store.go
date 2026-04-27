package config

import (
	"errors"
	"sync"
	"sync/atomic"
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
	mu  *sync.RWMutex
	cfg *MoomboxConfig
	// savePath is held in an atomic.Pointer so SavePath() / Update can read
	// it lock-free. Route writers call SavePath() while holding s.mu.Lock
	// (copy-on-write rollback pattern); a separate RLock here would have
	// deadlocked against the held write lock.
	savePath atomic.Pointer[string]
}

// NewStore returns a Store wrapping cfg with its own embedded RWMutex.
// savePath may be empty to skip the auto-save step in Update; that's
// useful during startup before a final config path is negotiated.
func NewStore(cfg *MoomboxConfig, savePath string) *Store {
	s := &Store{mu: &sync.RWMutex{}, cfg: cfg}
	s.savePath.Store(&savePath)
	return s
}

// NewStoreWithMutex returns a Store sharing an external RWMutex. Use this
// during the gradual migration so the Store's Read/Update and legacy
// cfgMu.RLock()/Lock() sites share the same critical section — without
// that, new-API and legacy callers would race. Once all legacy callers
// have migrated, NewStore is preferred.
func NewStoreWithMutex(cfg *MoomboxConfig, savePath string, mu *sync.RWMutex) *Store {
	s := &Store{mu: mu, cfg: cfg}
	s.savePath.Store(&savePath)
	return s
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
// normalizes, and saves to disk when savePath is set. On validation OR
// save failure the in-memory config is rolled back to its pre-fn state,
// keeping the in-memory and on-disk views consistent. Returns the error
// from validation (joined) or save.
//
// Rollback caveat: the snapshot is a SHALLOW copy of *s.cfg. If fn
// mutates slice ELEMENTS in place (e.g. `c.Channels[i].Name = "x"`) or
// appends/sorts the underlying array, the rollback restores only the
// slice header — the element mutations survive because both the
// snapshot and the live cfg share the same backing array. Today's
// callers all assign whole-slice replacements
// (`c.Cookies.Platforms = platforms`, `c.Channels = newChannels`),
// which works correctly with shallow rollback. New callers that need
// to mutate elements in place must deep-copy the slice first.
func (s *Store) Update(fn func(*MoomboxConfig)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Snapshot the current cfg so we can roll back on validation/save
	// failure. Without this, a failed Update leaves the in-memory cfg
	// mutated by fn while disk stays unchanged -- subsequent Read()
	// calls leak the bad value into monitors / web responses. See the
	// godoc above for the shallow-copy caveat.
	snapshot := *s.cfg

	fn(s.cfg)
	if errs := Validate(s.cfg); len(errs) > 0 {
		*s.cfg = snapshot
		return errors.Join(errs...)
	}
	Normalize(s.cfg)
	path := s.SavePath()
	if path == "" {
		return nil
	}
	if err := Save(s.cfg, path); err != nil {
		*s.cfg = snapshot
		return err
	}
	return nil
}

// SetSavePath installs or overrides the path used by Update for auto-save.
// Useful when the path is negotiated after NewStore (first-run wizard,
// test harnesses). Stored via atomic.Pointer so SavePath() can read it
// without grabbing s.mu — route writers call SavePath() while already
// holding s.mu.Lock for their copy-on-write pattern, and an internal
// RLock would have deadlocked against the held write lock.
func (s *Store) SetSavePath(path string) {
	s.savePath.Store(&path)
}

// SavePath returns the path Update / SaveLocked write to. Empty string
// means "no save configured" (e.g., during first-run setup before a path
// has been negotiated). Lock-free via atomic load so it's safe to call
// while holding s.mu in either direction; in practice route writers
// invoke this from inside their PUT /api/config critical section.
//
// DEPRECATED: kept for the gradual migration from external save callbacks
// to Store. Future write APIs (UpdateE) will absorb these direct callers.
func (s *Store) SavePath() string {
	if p := s.savePath.Load(); p != nil {
		return *p
	}
	return ""
}

// SaveLocked persists the current config to savePath. The caller MUST
// already hold the write lock (s.RWMutex().Lock()) — this method does NOT
// acquire it. Returns nil immediately if savePath is empty (e.g., during
// first-run setup before a path has been negotiated).
//
// Used by routes/TUI handlers that mutate the config and need the
// snapshot-mutate-save-rollback pattern: after mutating cfg under the lock
// they call SaveLocked, and if it returns an error they restore their
// snapshot before releasing the lock so the in-memory state stays in sync
// with disk.
//
// DEPRECATED: kept only for the gradual migration from external cfgMu +
// saveConfig callbacks to Store. New callers should use Update for simple
// mutations or build out an UpdateE-style transactional API for handlers
// that need both validation aborts and save-failure rollback.
func (s *Store) SaveLocked() error {
	path := s.SavePath()
	if path == "" {
		return nil
	}
	return Save(s.cfg, path)
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
