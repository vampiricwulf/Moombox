package bgutils

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"
)

// inflightEntry holds the result of an inflight PO token generation.
type inflightEntry struct {
	done    chan struct{}
	session *SessionData
	err     error
}

// defaultMinterKey is the single key under which the PotProvider stores
// its (one) cached minter. The cache map shape is preserved (to keep the
// invalidation paths and the /api/pot debug route stable), but the key
// is no longer the contentBinding — one WebPoMinter can produce tokens
// for any binding, so caching per-binding wasted a BotGuard run on every
// new content binding (audit reports/bgutils.md CRIT-2). A future
// commit may compound the key with proxy/IP for multi-account setups,
// at which point this constant becomes the "no proxy" default.
const defaultMinterKey = "default"

// PotProvider manages PO token generation with triple-tier caching:
// 1. Session cache: quick returns for same content binding
// 2. Minter cache: ONE minter per process, reused across bindings
// 3. Inflight dedup: prevent concurrent generation for same key
type PotProvider struct {
	mu           sync.Mutex
	sessionCache map[string]*SessionData
	// minterCache holds at most one entry under defaultMinterKey.
	// Map shape preserved so InvalidateIntegrityTokens / Cleanup /
	// GetMinterCacheKeys stay structurally identical and a future
	// proxy/IP-keyed expansion is a one-line change. CRIT-2.
	minterCache map[string]*TokenMinter
	inflight    map[string]*inflightEntry
	// minterCreatingMu serialises minter creation across goroutines
	// that all see "no minter". Without it, two goroutines requesting
	// different bindings on a fresh process would each start a
	// BotGuard VM and the second one's would be replaced + leaked.
	// Held only during the minter-creation critical section, NEVER
	// while holding pp.mu — that would deadlock against the
	// minter-eviction AfterFunc which acquires pp.mu under the same
	// lock-ordering. CRIT-2.
	minterCreatingMu sync.Mutex
	config           *BgConfig
	logger           interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// NewPotProvider creates a new PO token provider.
func NewPotProvider(config *BgConfig, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *PotProvider {
	if config.RequestKey == "" {
		config.RequestKey = DefaultRequestKey
	}
	return &PotProvider{
		sessionCache: make(map[string]*SessionData),
		minterCache:  make(map[string]*TokenMinter),
		inflight:     make(map[string]*inflightEntry),
		config:       config,
		logger:       logger,
	}
}

// GeneratePoToken generates or retrieves a cached PO token for the given content binding.
// If contentBinding is empty, a default visitor data value is used.
func (pp *PotProvider) GeneratePoToken(ctx context.Context, contentBinding string, bypassCache bool) (*SessionData, error) {
	if contentBinding == "" {
		// Generate visitor-data-like value: base64("{timestamp}-{random}")
		raw := fmt.Sprintf("%d-%s", time.Now().UnixMilli(), randomAlphaNum(13))
		contentBinding = base64.StdEncoding.EncodeToString([]byte(raw))
	}

	pp.mu.Lock()

	// Cleanup expired entries
	pp.cleanupExpired()

	bindingPrefix := contentBinding[:min(len(contentBinding), 20)]

	// Check session cache (unless bypassing)
	if !bypassCache {
		if session, ok := pp.sessionCache[contentBinding]; ok {
			if time.Now().Before(session.ExpiresAt) {
				pp.mu.Unlock()
				pp.logger.Debug("[PotProvider] session cache hit", "binding", bindingPrefix)
				return session, nil
			}
			delete(pp.sessionCache, contentBinding)
		}
	}

	// Check for inflight request (dedup) — all waiters read from the same entry
	if entry, ok := pp.inflight[contentBinding]; ok {
		pp.mu.Unlock()
		pp.logger.Debug("[PotProvider] waiting for inflight request", "binding", bindingPrefix)
		select {
		case <-entry.done:
			return entry.session, entry.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// Mark as inflight
	entry := &inflightEntry{done: make(chan struct{})}
	pp.inflight[contentBinding] = entry

	// Check minter cache (unless bypassing — TS skips both caches when bypass_cache=true).
	// Single-minter design: the cached minter (if any) lives under
	// defaultMinterKey and serves every contentBinding. CRIT-2.
	var minter *TokenMinter
	var hasMinter bool
	if !bypassCache {
		minter, hasMinter = pp.minterCache[defaultMinterKey]
		if hasMinter && time.Now().After(minter.ExpiresAt) {
			delete(pp.minterCache, defaultMinterKey)
			hasMinter = false
		}
	}

	pp.mu.Unlock()

	// Recover panics from the Goja VM paths and convert them into errors.
	// Without this a panic escapes before the inflight map is cleared or
	// entry.done is closed, wedging every concurrent waiter on this binding
	// forever.
	session, err := func() (s *SessionData, e error) {
		defer func() {
			if r := recover(); r != nil {
				s = nil
				e = fmt.Errorf("bgutils mint panic: %v", r)
				pp.logger.Error("[PotProvider] mint panic", "binding", bindingPrefix, "panic", r)
			}
		}()
		if hasMinter {
			pp.logger.Debug("[PotProvider] using cached minter", "binding", bindingPrefix)
			return pp.mintPoToken(minter, contentBinding)
		}
		pp.logger.Debug("[PotProvider] generating new minter", "binding", bindingPrefix)
		return pp.generateAndMint(ctx, contentBinding, bypassCache)
	}()

	// Store result on the entry so all waiters can read it, then signal
	entry.session = session
	entry.err = err

	pp.mu.Lock()
	if err == nil {
		pp.sessionCache[contentBinding] = session
	}
	delete(pp.inflight, contentBinding)
	pp.mu.Unlock()

	close(entry.done)
	return session, err
}

// InvalidateCaches clears all cached data. Minter Cleanup runs outside pp.mu
// so a slow VM teardown can't block concurrent GeneratePoToken callers.
func (pp *PotProvider) InvalidateCaches() {
	pp.mu.Lock()
	toCleanup := make([]*TokenMinter, 0, len(pp.minterCache))
	for _, m := range pp.minterCache {
		toCleanup = append(toCleanup, m)
	}
	pp.sessionCache = make(map[string]*SessionData)
	pp.minterCache = make(map[string]*TokenMinter)
	pp.mu.Unlock()

	for _, m := range toCleanup {
		pp.safeCleanup(m, "InvalidateCaches")
	}
}

// InvalidateIntegrityTokens clears minter cache (forces BotGuard re-run).
// Minter Cleanup runs outside pp.mu (see InvalidateCaches rationale).
func (pp *PotProvider) InvalidateIntegrityTokens() {
	pp.mu.Lock()
	toCleanup := make([]*TokenMinter, 0, len(pp.minterCache))
	for _, m := range pp.minterCache {
		toCleanup = append(toCleanup, m)
	}
	pp.minterCache = make(map[string]*TokenMinter)
	pp.mu.Unlock()

	for _, m := range toCleanup {
		pp.safeCleanup(m, "InvalidateIntegrityTokens")
	}
}

// GetMinterCacheKeys returns the content binding keys in the minter cache.
func (pp *PotProvider) GetMinterCacheKeys() []string {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	keys := make([]string, 0, len(pp.minterCache))
	for k := range pp.minterCache {
		keys = append(keys, k)
	}
	return keys
}

// GeneratePoTokenString generates a PO token and returns just the token string.
func (pp *PotProvider) GeneratePoTokenString(ctx context.Context, contentBinding string, bypassCache bool) (string, error) {
	session, err := pp.GeneratePoToken(ctx, contentBinding, bypassCache)
	if err != nil {
		return "", err
	}
	return session.PoToken, nil
}

// GeneratePoTokenSession generates a PO token and returns both token and content binding.
// This is used by the /get_pot route to return the full session data matching yt-dlp expectations.
func (pp *PotProvider) GeneratePoTokenSession(ctx context.Context, contentBinding string, bypassCache bool) (poToken string, actualBinding string, err error) {
	session, err := pp.GeneratePoToken(ctx, contentBinding, bypassCache)
	if err != nil {
		return "", "", err
	}
	return session.PoToken, session.ContentBinding, nil
}

// Cleanup releases all resources including shutting down cached BotGuard VMs.
func (pp *PotProvider) Cleanup() {
	pp.InvalidateCaches()
}

// safeCleanup runs m.Cleanup() with panic recovery. Nil-safe for both the
// minter and the Cleanup func pointer. Used whenever a cached minter is
// replaced or evicted to keep Goja teardown failures from propagating.
func (pp *PotProvider) safeCleanup(m *TokenMinter, reason string) {
	if m == nil || m.Cleanup == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			pp.logger.Error("bgutils: minter Cleanup panic", "reason", reason, "panic", r)
		}
	}()
	m.Cleanup()
}

func (pp *PotProvider) mintPoToken(minter *TokenMinter, contentBinding string) (*SessionData, error) {
	token, err := minter.MintFunc(contentBinding)
	if err != nil {
		return nil, fmt.Errorf("mint token: %w", err)
	}

	return &SessionData{
		PoToken:        token,
		ContentBinding: contentBinding,
		ExpiresAt:      time.Now().Add(pp.config.sessionTTL()),
	}, nil
}

func (pp *PotProvider) generateAndMint(ctx context.Context, contentBinding string, bypassCache bool) (*SessionData, error) {
	// Serialize minter creation across goroutines: every "first request"
	// goroutine sees an empty minterCache and would otherwise race to
	// spin up a BotGuard VM, with the losers being replaced + leaked.
	// Holding minterCreatingMu through the (potentially seconds-long)
	// VM init is acceptable because the alternative — duplicate BotGuard
	// runs — is exactly what CRIT-2 is here to prevent.
	pp.minterCreatingMu.Lock()
	defer pp.minterCreatingMu.Unlock()

	// Re-check the cache under the creation lock. Another goroutine may
	// have just stored a minter while we waited. Skip the re-check on
	// bypassCache=true — the caller explicitly asked us to generate
	// fresh, and matching upstream's TS bypass_cache semantics means
	// honouring that even when a cached minter is available.
	if !bypassCache {
		pp.mu.Lock()
		if cached, ok := pp.minterCache[defaultMinterKey]; ok && time.Now().Before(cached.ExpiresAt) {
			pp.mu.Unlock()
			return pp.mintPoToken(cached, contentBinding)
		}
		pp.mu.Unlock()
	}

	client := NewWebPoClient(pp.config, pp.logger)

	minter, err := client.GenerateTokenMinter(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate minter: %w", err)
	}

	// Cache the minter (VM stays alive via minter.Cleanup reference).
	// Cleanup of a replaced minter runs OUTSIDE the lock: Goja shutdown can
	// take real wall time and may transitively invoke user-supplied callbacks
	// that re-enter the provider, which would deadlock under pp.mu. Panic
	// recovery shields the current goroutine from a misbehaving VM teardown.
	pp.mu.Lock()
	old := pp.minterCache[defaultMinterKey]
	pp.minterCache[defaultMinterKey] = minter
	pp.mu.Unlock()
	pp.safeCleanup(old, "replaced minter")

	// Schedule automatic eviction so the Goja VM doesn't linger after
	// expiry. ONE minter serves the whole process, so its TTL is the
	// integrity-token TTL (~6h per upstream).
	ttl := time.Until(minter.ExpiresAt)
	if ttl > 0 {
		time.AfterFunc(ttl, func() {
			defer func() {
				if r := recover(); r != nil {
					pp.logger.Error("minter eviction panic", "panic", r)
				}
			}()
			var cleanup func()
			pp.mu.Lock()
			evicted := false
			if cached, ok := pp.minterCache[defaultMinterKey]; ok && cached == minter {
				cleanup = cached.Cleanup
				delete(pp.minterCache, defaultMinterKey)
				evicted = true
			}
			pp.mu.Unlock()
			if cleanup != nil {
				cleanup()
			}
			if evicted {
				pp.logger.Debug("[PotProvider] evicted expired minter")
			}
		})
	}

	return pp.mintPoToken(minter, contentBinding)
}

func (pp *PotProvider) cleanupExpired() {
	now := time.Now()
	for k, s := range pp.sessionCache {
		if now.After(s.ExpiresAt) {
			delete(pp.sessionCache, k)
		}
	}
	for k, m := range pp.minterCache {
		if now.After(m.ExpiresAt) {
			if m.Cleanup != nil {
				m.Cleanup()
			}
			delete(pp.minterCache, k)
		}
	}
}

const alphaNum = "abcdefghijklmnopqrstuvwxyz0123456789"

// randomAlphaNum generates n cryptographically-random alphanumeric characters.
// Uses a single rand.Read call and modular reduction rather than per-byte
// big.Int allocations. The modular bias against len(alphaNum)=36 out of 256 is
// negligible for the probe-identifier use case (~0.4% skew per byte).
func randomAlphaNum(n int) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// Fallback: fill with index 0 (extremely unlikely to fail on Windows)
		for i := range buf {
			buf[i] = alphaNum[0]
		}
		return string(buf)
	}
	for i, b := range buf {
		buf[i] = alphaNum[int(b)%len(alphaNum)]
	}
	return string(buf)
}
