package bgutils

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
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

	// Observability counters (atomic, monotonically increasing across the
	// process lifetime). Operators sample via /stats and compute deltas
	// externally. Audit reports/bgutils.md TD-3 (CRIT-2 follow-on).
	sessionHits        atomic.Uint64
	minterHits         atomic.Uint64
	mintersCreated     atomic.Uint64
	mintersInvalidated atomic.Uint64
	mintersEvicted     atomic.Uint64
	generateErrors     atomic.Uint64
	inflightWaits      atomic.Uint64

	// sidecar, when non-nil and healthy, is the preferred PO-token mint
	// path: a real Node + V8 + JSDOM subprocess runs BotGuard end-to-end
	// and returns real integrity tokens (not the websafe fallback the
	// goja path is limited to). On any sidecar error we fall through to
	// the goja minterCache flow so PO-token generation never goes
	// completely dark.
	//
	// Set via SetSidecar (cmd/moombox wires it up after Start). Nil in
	// unit tests so existing PotProvider* tests keep exercising the
	// goja path without dragging in subprocess/embed dependencies.
	sidecar         *sidecar.Sidecar
	sidecarMintsHit atomic.Uint64
	sidecarMintsErr atomic.Uint64

	// gvsMints / gvsMintsChallenge count GenerateGvsPoToken calls: every
	// attempt (regardless of outcome or path) and the subset that carried a
	// non-empty watch-page challenge, respectively. Spec §4.3.
	gvsMints          atomic.Uint64
	gvsMintsChallenge atomic.Uint64
}

// PotStats is a snapshot of PotProvider counters. The numeric fields are
// monotonically increasing — operators compute deltas externally. The
// CachedMinters field is the live cache size, not a counter.
//
// Observability surfaces overlap deliberately:
//   - SidecarMintsHit / SidecarMintsErr count *attempts* through the
//     sidecar path. A sidecar failure increments SidecarMintsErr AND
//     causes a fall-through to the goja path; if THAT path also fails
//     GenerateErrors increments too. So a single end-user request that
//     fails via both paths produces +1 SidecarMintsErr and +1
//     GenerateErrors. Read SidecarMintsErr as "how often did the
//     preferred path fail and we fell back" and GenerateErrors as
//     "how often did the user-facing call ultimately error".
type PotStats struct {
	SessionHits        uint64 `json:"session_hits"`
	MinterHits         uint64 `json:"minter_hits"`
	MintersCreated     uint64 `json:"minters_created"`
	MintersInvalidated uint64 `json:"minters_invalidated"`
	MintersEvicted     uint64 `json:"minters_evicted"`
	GenerateErrors     uint64 `json:"generate_errors"`
	InflightWaits      uint64 `json:"inflight_waits"`
	CachedMinters      int    `json:"cached_minters"`
	SidecarMintsHit    uint64 `json:"sidecar_mints_hit"`
	SidecarMintsErr    uint64 `json:"sidecar_mints_err"`
	GvsMints           uint64 `json:"gvs_mints"`
	GvsMintsChallenge  uint64 `json:"gvs_mints_challenge"`
}

// Stats returns a snapshot of the provider's observability counters.
// Spike of mintersCreated relative to mintersEvicted suggests YouTube
// is rotating ciphers more aggressively than usual; spike of
// generateErrors suggests BotGuard challenges are degrading. Audit
// reports/bgutils.md TD-3.
func (pp *PotProvider) Stats() PotStats {
	pp.mu.Lock()
	cached := len(pp.minterCache)
	pp.mu.Unlock()
	return PotStats{
		SessionHits:        pp.sessionHits.Load(),
		MinterHits:         pp.minterHits.Load(),
		MintersCreated:     pp.mintersCreated.Load(),
		MintersInvalidated: pp.mintersInvalidated.Load(),
		MintersEvicted:     pp.mintersEvicted.Load(),
		GenerateErrors:     pp.generateErrors.Load(),
		InflightWaits:      pp.inflightWaits.Load(),
		CachedMinters:      cached,
		SidecarMintsHit:    pp.sidecarMintsHit.Load(),
		SidecarMintsErr:    pp.sidecarMintsErr.Load(),
		GvsMints:           pp.gvsMints.Load(),
		GvsMintsChallenge:  pp.gvsMintsChallenge.Load(),
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

// SetSidecar attaches a started BotGuard sidecar to the provider. Call once
// after the subprocess handshake succeeds in cmd/moombox/services.go.
// Passing nil reverts to the goja-only path (useful for tests that want to
// exercise the fallback).
func (pp *PotProvider) SetSidecar(s *sidecar.Sidecar) {
	pp.mu.Lock()
	pp.sidecar = s
	pp.mu.Unlock()
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

	// Cleanup expired entries. cleanupExpired drops them from the maps
	// under pp.mu, but returns evicted minters whose Cleanup we must run
	// outside the lock (Cleanup acquires WebPoMinter.mu and would
	// deadlock against concurrent mintPoToken).
	expiredMinters := pp.cleanupExpired()
	if len(expiredMinters) > 0 {
		pp.mu.Unlock()
		for _, m := range expiredMinters {
			pp.safeCleanup(m, "expired")
		}
		pp.mu.Lock()
	}

	bindingPrefix := contentBinding[:min(len(contentBinding), 20)]

	// Check session cache (unless bypassing)
	if !bypassCache {
		if session, ok := pp.sessionCache[contentBinding]; ok {
			if time.Now().Before(session.ExpiresAt) {
				pp.mu.Unlock()
				pp.sessionHits.Add(1)
				pp.logger.Debug("[PotProvider] session cache hit", "binding", bindingPrefix)
				return session, nil
			}
			delete(pp.sessionCache, contentBinding)
		}
	}

	// Check for inflight request (dedup) — all waiters read from the same entry
	if entry, ok := pp.inflight[contentBinding]; ok {
		pp.mu.Unlock()
		pp.inflightWaits.Add(1)
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
			pp.minterHits.Add(1)
			pp.logger.Debug("[PotProvider] using cached minter", "binding", bindingPrefix)
			return pp.mintPoToken(minter, contentBinding)
		}
		pp.logger.Debug("[PotProvider] generating new minter", "binding", bindingPrefix)
		return pp.generateAndMint(ctx, contentBinding, bypassCache)
	}()

	if err != nil {
		pp.generateErrors.Add(1)
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

// InvalidateCaches clears all cached data. Minter Cleanup runs outside pp.mu
// so a slow VM teardown can't block concurrent GeneratePoToken callers.
// Also fans out to the sidecar (when attached) so its internal minter +
// session caches drop in lockstep.
func (pp *PotProvider) InvalidateCaches() {
	pp.mu.Lock()
	toCleanup := make([]*TokenMinter, 0, len(pp.minterCache))
	for _, m := range pp.minterCache {
		toCleanup = append(toCleanup, m)
	}
	pp.sessionCache = make(map[string]*SessionData)
	pp.minterCache = make(map[string]*TokenMinter)
	sc := pp.sidecar
	pp.mu.Unlock()

	for _, m := range toCleanup {
		pp.safeCleanup(m, "InvalidateCaches")
	}
	pp.mintersInvalidated.Add(uint64(len(toCleanup)))

	if sc != nil {
		// Bound the sidecar IPC by 5s so a hung child can't wedge an
		// operator-driven invalidate.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := sc.InvalidateCaches(ctx); err != nil {
			pp.logger.Warn("[PotProvider] sidecar InvalidateCaches failed", "err", err)
		}
	}
}

// InvalidateIntegrityTokens clears minter cache (forces BotGuard re-run).
// Minter Cleanup runs outside pp.mu (see InvalidateCaches rationale).
// Also tells the sidecar to drop its internal minter so the next mint
// re-runs BotGuard end-to-end.
func (pp *PotProvider) InvalidateIntegrityTokens() {
	pp.mu.Lock()
	toCleanup := make([]*TokenMinter, 0, len(pp.minterCache))
	for _, m := range pp.minterCache {
		toCleanup = append(toCleanup, m)
	}
	pp.minterCache = make(map[string]*TokenMinter)
	sc := pp.sidecar
	pp.mu.Unlock()

	for _, m := range toCleanup {
		pp.safeCleanup(m, "InvalidateIntegrityTokens")
	}
	pp.mintersInvalidated.Add(uint64(len(toCleanup)))

	if sc != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := sc.InvalidateIT(ctx); err != nil {
			pp.logger.Warn("[PotProvider] sidecar InvalidateIT failed", "err", err)
		}
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
	// Sidecar path: a real Node + V8 + JSDOM subprocess that passes
	// BotGuard's timing fingerprint and returns real integrity tokens.
	// Bypasses the goja minterCache entirely (the sidecar maintains its
	// own internal minter cache, ~6h TTL). On any sidecar error we fall
	// through to the goja path so token generation never goes dark.
	pp.mu.Lock()
	sc := pp.sidecar
	pp.mu.Unlock()
	if sc != nil && sc.IsHealthy() {
		bindingPrefix := contentBinding[:min(len(contentBinding), 20)]
		pp.logger.Debug("[PotProvider] minting via sidecar", "binding", bindingPrefix)
		token, err := sc.GeneratePoToken(ctx, contentBinding)
		if err == nil {
			pp.sidecarMintsHit.Add(1)
			return &SessionData{
				PoToken:        token,
				ContentBinding: contentBinding,
				ExpiresAt:      time.Now().Add(pp.config.sessionTTL()),
			}, nil
		}
		pp.sidecarMintsErr.Add(1)
		pp.logger.Warn("[PotProvider] sidecar mint failed; falling through to goja", "err", err)
	}

	return pp.gojaGenerateAndMint(ctx, contentBinding, bypassCache)
}

// gojaGenerateAndMint is the goja fallback: serialize minter creation, then
// generate + cache + schedule eviction/refresh, then mint. Split out of
// generateAndMint so the GVS path can reuse it without re-attempting the
// sidecar. bypassCache=true skips the under-lock cache re-check AND forces a
// fresh minter (matching upstream TS bypass_cache semantics).
func (pp *PotProvider) gojaGenerateAndMint(ctx context.Context, contentBinding string, bypassCache bool) (*SessionData, error) {
	// Goja fallback path. Serialize minter creation across goroutines:
	// every "first request" goroutine sees an empty minterCache and
	// would otherwise race to spin up a BotGuard VM, with the losers
	// being replaced + leaked. Holding minterCreatingMu through the
	// (potentially seconds-long) VM init is acceptable because the
	// alternative — duplicate BotGuard runs — is exactly what CRIT-2 is
	// here to prevent.
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
	pp.mintersCreated.Add(1)
	if old != nil {
		pp.mintersInvalidated.Add(1)
	}

	// Schedule automatic eviction so the Goja VM doesn't linger after
	// expiry. ONE minter serves the whole process, so its TTL is the
	// integrity-token TTL (~6h per upstream).
	ttl := time.Until(minter.ExpiresAt)
	if ttl > 0 {
		// Proactive refresh: re-mint shortly before expiry so the
		// user-facing first-call-after-expiry doesn't pay the 2-10s
		// BotGuard generation cost (which can miss live-stream segments
		// on a 5s segment cadence). The refresh runs on a background
		// ctx; its result atomically replaces the cached minter.
		// Audit reports/bgutils.md FRESH-2.
		if refreshAt := ttl - minterRefreshLead; refreshAt > 0 {
			time.AfterFunc(refreshAt, func() {
				defer func() {
					if r := recover(); r != nil {
						pp.logger.Error("minter proactive refresh panic", "panic", r)
					}
				}()
				pp.proactiveRefreshMinter(minter)
			})
		}
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
				pp.mintersEvicted.Add(1)
				pp.logger.Debug("[PotProvider] evicted expired minter")
			}
		})
	}

	return pp.mintPoToken(minter, contentBinding)
}

// GvsMint is a GVS (segment-URL) PO-token mint result with the provenance
// the worker's "[POT] GVS mint" log line reports.
type GvsMint struct {
	PoToken      string
	MinterSource string // "challenge" | "att_get" | "goja-fallback"
	MinterFresh  bool
	ViaSidecar   bool
}

// GenerateGvsPoToken mints a segment-URL PO token under the
// fresh-minter-per-GVS-mint policy (spec §4.3): the session cache is fully
// bypassed (no read, no write), and the sidecar builds a fresh minter from
// the supplied watch-page challenge ("" → its /att/get fallback). The
// expensive BotGuard regeneration is deduplicated inside the sidecar
// (minterPromise), so no provider-side inflight entry is needed. When the
// sidecar is unavailable the goja flow runs with the challenge ignored —
// today's (session-incoherent) behavior, flagged as "goja-fallback".
//
// If POT-enforced media (premieres) still 403s with challenge-sourced
// minters, the next suspect is datasync-ID binding: yt-dlp binds GVS
// tokens to the account's datasync ID when cookies are sent with media
// requests (Moombox sends account cookies on segment fetches), while this
// path binds to videoID. See docs history: 2026-08-14 attestation POT
// coherence spec §3.
func (pp *PotProvider) GenerateGvsPoToken(ctx context.Context, contentBinding, challenge string) (GvsMint, error) {
	pp.gvsMints.Add(1)
	if challenge != "" {
		pp.gvsMintsChallenge.Add(1)
	}
	pp.mu.Lock()
	sc := pp.sidecar
	pp.mu.Unlock()
	if sc != nil && sc.IsHealthy() {
		res, err := sc.GenerateGvsPoToken(ctx, contentBinding, challenge)
		if err == nil {
			pp.sidecarMintsHit.Add(1)
			return GvsMint{PoToken: res.PoToken, MinterSource: res.MinterSource, MinterFresh: res.MinterFresh, ViaSidecar: true}, nil
		}
		pp.sidecarMintsErr.Add(1)
		pp.logger.Warn("[PotProvider] sidecar GVS mint failed; falling through to goja", "err", err)
	}
	session, err := func() (s *SessionData, e error) {
		defer func() {
			if r := recover(); r != nil {
				s, e = nil, fmt.Errorf("bgutils GVS mint panic: %v", r)
				pp.logger.Error("[PotProvider] GVS mint panic", "panic", r)
			}
		}()
		return pp.gojaGenerateAndMint(ctx, contentBinding, true)
	}()
	if err != nil {
		pp.generateErrors.Add(1)
		return GvsMint{}, err
	}
	return GvsMint{PoToken: session.PoToken, MinterSource: "goja-fallback", MinterFresh: true}, nil
}

// minterRefreshLead is how far before a cached minter's expiry the proactive
// refresh fires. 5 min is comfortably longer than the worst-case BotGuard
// run (typically 2-10s), so the cached minter still answers user-facing
// calls during the refresh window. Audit reports/bgutils.md FRESH-2.
const minterRefreshLead = 5 * time.Minute

// proactiveRefreshMinter regenerates the cached minter shortly before its
// expiry so the eviction-then-recreate cycle isn't paid by a user-facing
// GeneratePoToken call. Skips work if the cache no longer holds `original`
// (already evicted / invalidated / replaced); otherwise generates a fresh
// minter, swaps it in atomically, and cleans up `original`. Failures only
// log — the eviction AfterFunc still fires at ttl so a failed refresh
// doesn't leave a stale minter alive.
//
// Runs on its own background ctx (with a hard timeout) since the firing
// time isn't tied to any user request.
func (pp *PotProvider) proactiveRefreshMinter(original *TokenMinter) {
	pp.mu.Lock()
	cached, ok := pp.minterCache[defaultMinterKey]
	pp.mu.Unlock()
	if !ok || cached != original {
		// Already replaced or evicted; nothing to do.
		return
	}

	// Bound the refresh attempt — if BotGuard is misbehaving, fall back
	// on the eviction AfterFunc rather than wedging a goroutine.
	refreshCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Take the same creation lock the user-facing path uses so a refresh
	// can't race with a fresh GeneratePoToken trying to bootstrap a
	// missing minter (which would happen if eviction fired between the
	// cache check above and the swap below).
	pp.minterCreatingMu.Lock()
	defer pp.minterCreatingMu.Unlock()

	// Re-check under the creation lock — another goroutine may have
	// already replaced the minter while we waited.
	pp.mu.Lock()
	cached, ok = pp.minterCache[defaultMinterKey]
	pp.mu.Unlock()
	if !ok || cached != original {
		return
	}

	pp.logger.Debug("[PotProvider] proactively refreshing minter pre-expiry")

	client := NewWebPoClient(pp.config, pp.logger)
	fresh, err := client.GenerateTokenMinter(refreshCtx)
	if err != nil {
		pp.logger.Warn("[PotProvider] proactive refresh failed; falling back to eviction", "err", err)
		return
	}

	pp.mu.Lock()
	stillOurs := pp.minterCache[defaultMinterKey] == original
	if stillOurs {
		pp.minterCache[defaultMinterKey] = fresh
	}
	pp.mu.Unlock()

	if !stillOurs {
		// Lost the race; discard the fresh minter so we don't leak its VM.
		pp.safeCleanup(fresh, "refresh-race-loser")
		return
	}

	pp.safeCleanup(original, "proactive-refresh")
	pp.mintersCreated.Add(1)
	pp.mintersInvalidated.Add(1)

	// Schedule the refresh + eviction chain for the new minter so the
	// cycle continues. Mirrors the bottom of generateAndMint without
	// duplicating the rest of the user-facing logic.
	freshTTL := time.Until(fresh.ExpiresAt)
	if freshTTL > 0 {
		if refreshAt := freshTTL - minterRefreshLead; refreshAt > 0 {
			time.AfterFunc(refreshAt, func() {
				defer func() {
					if r := recover(); r != nil {
						pp.logger.Error("minter proactive refresh panic", "panic", r)
					}
				}()
				pp.proactiveRefreshMinter(fresh)
			})
		}
		time.AfterFunc(freshTTL, func() {
			defer func() {
				if r := recover(); r != nil {
					pp.logger.Error("minter eviction panic", "panic", r)
				}
			}()
			var cleanup func()
			pp.mu.Lock()
			evicted := false
			if cached, ok := pp.minterCache[defaultMinterKey]; ok && cached == fresh {
				cleanup = cached.Cleanup
				delete(pp.minterCache, defaultMinterKey)
				evicted = true
			}
			pp.mu.Unlock()
			if cleanup != nil {
				cleanup()
			}
			if evicted {
				pp.mintersEvicted.Add(1)
				pp.logger.Debug("[PotProvider] evicted expired minter")
			}
		})
	}
}

// cleanupExpired drops expired sessionCache and minterCache entries from
// pp under the caller-held pp.mu, and returns the minters that were
// evicted. Caller MUST run safeCleanup on each returned minter AFTER
// releasing pp.mu -- m.Cleanup() routes to WebPoMinter.Shutdown which
// acquires WebPoMinter.mu and runs the goja shutdown function. Holding
// pp.mu across that path can deadlock against a concurrent
// pp.mintPoToken (which holds WebPoMinter.mu and may queue on pp.mu via
// the inflight cleanup at line ~244). Same anti-pattern that CRIT-6
// fixed in InvalidateCaches/InvalidateIntegrityTokens.
func (pp *PotProvider) cleanupExpired() []*TokenMinter {
	now := time.Now()
	for k, s := range pp.sessionCache {
		if now.After(s.ExpiresAt) {
			delete(pp.sessionCache, k)
		}
	}
	var toCleanup []*TokenMinter
	for k, m := range pp.minterCache {
		if now.After(m.ExpiresAt) {
			toCleanup = append(toCleanup, m)
			delete(pp.minterCache, k)
			pp.mintersEvicted.Add(1)
		}
	}
	return toCleanup
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
