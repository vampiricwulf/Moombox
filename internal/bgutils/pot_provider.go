package bgutils

import (
	"context"
	"encoding/base64"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// inflightEntry holds the result of an inflight PO token generation.
type inflightEntry struct {
	done    chan struct{}
	session *SessionData
	err     error
}

// PotProvider manages PO token generation with triple-tier caching:
// 1. Session cache: quick returns for same content binding
// 2. Minter cache: avoid re-running BotGuard VM
// 3. Inflight dedup: prevent concurrent generation for same key
type PotProvider struct {
	mu           sync.Mutex
	sessionCache map[string]*SessionData
	minterCache  map[string]*TokenMinter
	inflight     map[string]*inflightEntry
	config       *BgConfig
	logger       interface {
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

	// Check minter cache (unless bypassing — TS skips both caches when bypass_cache=true)
	var minter *TokenMinter
	var hasMinter bool
	if !bypassCache {
		minter, hasMinter = pp.minterCache[contentBinding]
		if hasMinter && time.Now().After(minter.ExpiresAt) {
			delete(pp.minterCache, contentBinding)
			hasMinter = false
		}
	}

	pp.mu.Unlock()

	var session *SessionData
	var err error

	if hasMinter {
		pp.logger.Debug("[PotProvider] using cached minter", "binding", bindingPrefix)
		session, err = pp.mintPoToken(minter, contentBinding)
	} else {
		pp.logger.Debug("[PotProvider] generating new minter", "binding", bindingPrefix)
		session, err = pp.generateAndMint(ctx, contentBinding)
	}

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

// InvalidateCaches clears all cached data.
func (pp *PotProvider) InvalidateCaches() {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	// Shut down VMs for all cached minters
	for _, m := range pp.minterCache {
		if m.Cleanup != nil {
			m.Cleanup()
		}
	}
	pp.sessionCache = make(map[string]*SessionData)
	pp.minterCache = make(map[string]*TokenMinter)
}

// InvalidateIntegrityTokens clears minter cache (forces BotGuard re-run).
func (pp *PotProvider) InvalidateIntegrityTokens() {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	for _, m := range pp.minterCache {
		if m.Cleanup != nil {
			m.Cleanup()
		}
	}
	pp.minterCache = make(map[string]*TokenMinter)
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

func (pp *PotProvider) mintPoToken(minter *TokenMinter, contentBinding string) (*SessionData, error) {
	token, err := minter.MintFunc(contentBinding)
	if err != nil {
		return nil, fmt.Errorf("mint token: %w", err)
	}

	return &SessionData{
		PoToken:        token,
		ContentBinding: contentBinding,
		ExpiresAt:      time.Now().Add(SessionCacheTTL),
	}, nil
}

func (pp *PotProvider) generateAndMint(ctx context.Context, contentBinding string) (*SessionData, error) {
	client := NewWebPoClient(pp.config, pp.logger)

	minter, err := client.GenerateTokenMinter(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate minter: %w", err)
	}

	// Cache the minter (VM stays alive via minter.Cleanup reference)
	pp.mu.Lock()
	// Clean up any previous minter for this binding before replacing
	if old, ok := pp.minterCache[contentBinding]; ok && old.Cleanup != nil {
		old.Cleanup()
	}
	pp.minterCache[contentBinding] = minter
	pp.mu.Unlock()

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

func randomAlphaNum(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphaNum[rand.Intn(len(alphaNum))]
	}
	return string(b)
}
