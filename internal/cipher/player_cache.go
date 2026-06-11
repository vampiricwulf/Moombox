package cipher

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/httpx"
	"golang.org/x/sync/singleflight"
)

const (
	// playerCacheTTL is the offline backstop. With conditional-GET revalidation
	// (see Fetch) the in-flight rotation case is handled regardless of TTL;
	// TTL only matters when revalidation can't reach YouTube. 24h is a
	// safe-enough offline window — rotations have been observed at ~36h.
	// 14d (the previous value) was offline-meaningless because the player
	// has rotated long before then.
	playerCacheTTL    = 24 * time.Hour
	playerCacheSubdir = "player_cache"
)

// playerHTTPClient fetches player.js. Backed by the shared httpx.Client
// transport for keep-alive amortisation across cipher / youtube / etc.
var playerHTTPClient = httpx.Client(30 * time.Second)

// fallbackLoggedURLs tracks URLs we've already warned about for the
// extract-fallback case. Bounded by the set of unique URLs Moombox
// sees in a single process (handful per session), so the map size
// is fine without explicit eviction.
var fallbackLoggedURLs sync.Map // map[string]struct{}

// PlayerCache manages disk-cached YouTube player JS files.
type PlayerCache struct {
	mu       sync.RWMutex
	cacheDir string
	// fetchSF coalesces concurrent Fetch calls for the same player so 5
	// parallel stream-setups don't all issue their own conditional GET.
	// Keyed on CacheKey(playerURL) so locale-variant URLs collapse — same
	// rationale as the in-memory solverData LRU.
	fetchSF singleflight.Group
	logger  interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
}

// playerMeta captures the conditional-revalidation headers from YouTube's
// last 200 OK response for a player JS. Persisted alongside the cached
// raw JS as <key>.meta.json so subsequent Fetches can replay
// If-Modified-Since / If-None-Match using YouTube's own timestamps
// rather than our local file mtime (which is later than YouTube's
// Last-Modified and would cause some CDN edges to return 304
// indefinitely under strict comparison).
type playerMeta struct {
	LastModified  string `json:"lastModified,omitempty"`
	ETag          string `json:"etag,omitempty"`
	ContentLength int64  `json:"contentLength,omitempty"`
}

// NewPlayerCache creates a player cache at the given base directory.
// If cacheDir is empty, uses ~/.cache/yt-cipher/.
func NewPlayerCache(cacheDir string, logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) (*PlayerCache, error) {
	if cacheDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("get home dir: %w", err)
		}
		cacheDir = filepath.Join(home, ".cache", "yt-cipher")
	}

	dir := filepath.Join(cacheDir, playerCacheSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}

	return &PlayerCache{cacheDir: dir, logger: logger}, nil
}

// extractPlayerIDFromURL extracts the player ID from a player URL without
// any fallback — returns "" when the URL doesn't match the expected shape.
// Shared by CacheKey and PlayerIDFromURL to avoid a circular dependency
// (PlayerIDFromURL formerly fell back to CacheKey; CacheKey now needs
// extraction to derive a playerID-stable key).
//
// e.g. "https://www.youtube.com/s/player/abcdef12/player_ias.vflset/en_US/base.js" -> "abcdef12"
func extractPlayerIDFromURL(playerURL string) string {
	parts := strings.Split(playerURL, "/")
	for i, p := range parts {
		if p == "player" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// CacheKey returns a short hex digest for use as a solver/disk cache key.
// The key is derived from the playerID segment of the URL (the 8-ish char
// segment after /s/player/ in the canonical URL shape) so two URLs that
// differ only in their locale segment (e.g. en_US vs en_GB vs de) hash to
// the same key.
//
// This prevents the in-memory solver cache from holding two ~44 MB compiled
// goja Runtimes for the same player when the routed and legacy code paths
// reach GetSolvers via URLs with different locale segments (the routed path
// constructs synthetic en_US URLs via playerURLForID; the legacy path uses
// the watch-page playerURL which can carry any locale).
//
// Full 64-char SHA-256 was overkill for this domain (a few thousand entries
// at most) and hit Windows MAX_PATH limits faster when combined with a long
// cacheDir. 16 hex chars = 64 bits of hash space; collision probability
// across thousands of entries is ~1 in 2^50 — negligible for cache keys.
//
// Falls back to hashing the full URL for malformed inputs where the playerID
// can't be extracted (preserves a stable key even on unusual URL shapes).
//
// Existing disk-cached files from prior versions become unreachable and are
// harmlessly eviction-swept by the background 24h tick.
func CacheKey(playerURL string) string {
	id := extractPlayerIDFromURL(playerURL)
	if id == "" {
		// Malformed URL — fall back to hashing the full URL so callers
		// still get a stable, deterministic key.
		h := sha256.Sum256([]byte(playerURL))
		return fmt.Sprintf("%x", h[:8])
	}
	h := sha256.Sum256([]byte(id))
	return fmt.Sprintf("%x", h[:8]) // 8 bytes = 16 hex chars
}

// PlayerIDFromURL extracts the player ID from a player URL.
// e.g. "https://www.youtube.com/s/player/abcdef12/player_ias.vflset/en_US/base.js" -> "abcdef12"
//
// Falls back to the URL hash (via CacheKey) on malformed inputs — safe here
// because CacheKey's fallback path uses URL-hash directly (no extraction),
// so there is no recursion. Logs once per URL when the fallback fires so
// operators see the regression early; without this signal a bogus
// playerID-as-hash routes to the sidecar's unfetchable URL builder
// (playerURLForID) and fails the retry-with-JS path silently.
func PlayerIDFromURL(playerURL string) string {
	if id := extractPlayerIDFromURL(playerURL); id != "" {
		return id
	}

	// Fallback: URL doesn't match the canonical /s/player/<id>/... shape.
	// Warn once per URL so operators see the regression early.
	if _, loaded := fallbackLoggedURLs.LoadOrStore(playerURL, struct{}{}); !loaded {
		slog.Warn("PlayerIDFromURL: URL doesn't match canonical /s/player/<id>/ shape, falling back to URL hash; cipher routing through sidecar will fail for this player",
			"playerURL", playerURL)
	}
	return CacheKey(playerURL)
}

// FilePath returns the cache file path for a player URL.
func (pc *PlayerCache) FilePath(playerURL string) string {
	key := CacheKey(playerURL)
	return filepath.Join(pc.cacheDir, key+".js")
}

// FilePathPreprocessed returns the disk path for the preprocessed
// (sig/n binding-injected) form of a player. Audit reports/cipher.md
// Q4 — second tier disk cache that skips the 200-500 ms extraction +
// preprocessing pass on solver-LRU eviction. Same key + TTL as the
// raw cache so both age out together.
func (pc *PlayerCache) FilePathPreprocessed(playerURL string) string {
	key := CacheKey(playerURL)
	return filepath.Join(pc.cacheDir, key+".preprocessed.js")
}

// FilePathMeta returns the path of the meta.json sidecar that records
// YouTube's Last-Modified / ETag / Content-Length for the cached raw
// player JS. See playerMeta and Fetch for the conditional-revalidation
// flow.
func (pc *PlayerCache) FilePathMeta(playerURL string) string {
	key := CacheKey(playerURL)
	return filepath.Join(pc.cacheDir, key+".meta.json")
}

// readMeta returns the persisted meta for a player URL. Returns the
// zero value (empty fields) when no meta is present or it's unreadable;
// callers should treat that as "no header from YouTube last time" and
// fall back to file mtime as the conditional-GET hint (migration path).
func (pc *PlayerCache) readMeta(playerURL string) playerMeta {
	var m playerMeta
	data, err := os.ReadFile(pc.FilePathMeta(playerURL))
	if err != nil {
		return m
	}
	_ = json.Unmarshal(data, &m)
	return m
}

// writeMeta persists meta for a player URL via atomic write. Errors are
// non-fatal — meta loss only degrades the next conditional GET to the
// mtime fallback.
func (pc *PlayerCache) writeMeta(playerURL string, m playerMeta) {
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	if err := pc.atomicWrite(pc.FilePathMeta(playerURL), string(data)); err != nil && pc.logger != nil {
		pc.logger.Debug("cipher: meta write failed", "err", err)
	}
}

// Get returns the cached player JS for the given URL, or empty string if not cached/expired.
//
// TTL semantics use file ModTime which can drift in a couple of known cases:
//   - Windows AV or backup tools that touch the file reset ModTime, extending
//     the effective TTL. This is harmless — fresh-enough is still fresh.
//   - Manual file replacement with an older ModTime may expire a valid cache
//     prematurely. Also harmless — we just re-download.
//
// If a future copy-in-known-good-baseline feature is added, pair it with a
// sidecar metadata file instead of relying on ModTime.
func (pc *PlayerCache) Get(playerURL string) (string, error) {
	pc.mu.RLock()
	defer pc.mu.RUnlock()

	path := pc.FilePath(playerURL)

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}

	// Check TTL (see ModTime caveats above).
	if time.Since(info.ModTime()) > playerCacheTTL {
		os.Remove(path)
		return "", nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	return string(data), nil
}

// Put caches player JS for the given URL. Uses atomic write (tmp + rename).
func (pc *PlayerCache) Put(playerURL string, js string) error {
	return pc.atomicWrite(pc.FilePath(playerURL), js)
}

// GetPreprocessed returns the cached preprocessed (sig/n-binding-injected)
// player code, or "" if not cached / expired. TTL semantics match Get.
// Audit reports/cipher.md Q4.
func (pc *PlayerCache) GetPreprocessed(playerURL string) (string, error) {
	pc.mu.RLock()
	defer pc.mu.RUnlock()

	path := pc.FilePathPreprocessed(playerURL)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	if time.Since(info.ModTime()) > playerCacheTTL {
		os.Remove(path)
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// PutPreprocessed atomically writes the preprocessed player code.
// Audit reports/cipher.md Q4.
func (pc *PlayerCache) PutPreprocessed(playerURL string, code string) error {
	return pc.atomicWrite(pc.FilePathPreprocessed(playerURL), code)
}

// atomicWrite writes data to path via a temp file + rename. Holds pc.mu.Lock
// so concurrent writes don't race on the same target.
func (pc *PlayerCache) atomicWrite(path string, data string) error {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(data), 0o644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// FetchResult reports the outcome of a Fetch. JS is the (raw) player JS to
// use; Changed indicates whether the content differs from the prior cached
// copy, so callers can decide whether to also drop derivative state
// (compiled solvers, sidecar V8 entries). Changed is false on 304, on
// network-failure-with-cache, on cache-hit-with-no-revalidation, and on
// 200 responses whose Content-Length matches the cached size (treated as
// a Last-Modified flap).
type FetchResult struct {
	JS      string
	Changed bool
}

// Fetch returns the player JS for the given URL, performing conditional-GET
// revalidation against YouTube whenever a disk cache exists. The disk cache
// is always validated before serving; the offline TTL (playerCacheTTL) acts
// only as a backstop when revalidation can't reach YouTube.
//
// Concurrent Fetch calls for the same player coalesce via a singleflight
// keyed on CacheKey(playerURL) — five concurrent stream-setups for the
// same player issue one round-trip total.
//
// On a 200 response whose Content-Length matches the cached size, the
// content is treated as unchanged regardless of Last-Modified — YouTube CDN
// edges are occasionally split-cached and lie about Last-Modified; size
// is reliable. Player JS rotations have been observed to grow ~285 KB at
// a time (~12% growth), so identical-size rotations are improbable in
// practice and the reactive-invalidation path (cipher.ErrPlayerJSStale,
// see solver_sidecar.go) catches whatever slips through.
//
// The returned Changed=true also implies that <key>.preprocessed.js has
// been removed — the cache layer owns its own invalidation so callers
// don't have to know.
func (pc *PlayerCache) Fetch(ctx context.Context, playerURL string) (string, bool, error) {
	key := CacheKey(playerURL)
	res, err, _ := pc.fetchSF.Do(key, func() (any, error) {
		// Detach from the leader's context: coalesced waiters arrive from
		// independent jobs, and a leader cancelled mid-fetch would poison
		// every waiter with context.Canceled even though their own contexts
		// are live. playerHTTPClient's own 30s timeout still bounds the
		// round-trip.
		return pc.fetchInternal(context.WithoutCancel(ctx), playerURL)
	})
	if err != nil {
		return "", false, err
	}
	fr := res.(FetchResult)
	return fr.JS, fr.Changed, nil
}

// fetchInternal performs one (uncoalesced) round-trip. Always called via
// fetchSF.Do.
func (pc *PlayerCache) fetchInternal(ctx context.Context, playerURL string) (FetchResult, error) {
	// Inspect the disk cache. We treat "exists and within TTL" as the
	// candidate for conditional GET; "expired" forces a full GET (any
	// revalidation would replay headers from a doomed entry anyway).
	cachedJS, cachedExists, cachedMtime := pc.readCacheForRevalidation(playerURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, playerURL, nil)
	if err != nil {
		return FetchResult{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	if cachedExists {
		meta := pc.readMeta(playerURL)
		switch {
		case meta.LastModified != "":
			req.Header.Set("If-Modified-Since", meta.LastModified)
		case !cachedMtime.IsZero():
			// Migration: meta.json absent (post-upgrade or first fetch
			// pre-Stage-1). Replay file mtime — strictly later than the
			// real Last-Modified, so a 304 here means YouTube hasn't
			// rotated since we wrote the file, which is what we want.
			req.Header.Set("If-Modified-Since", cachedMtime.UTC().Format(http.TimeFormat))
		}
		if meta.ETag != "" {
			req.Header.Set("If-None-Match", meta.ETag)
		}
	}

	resp, err := playerHTTPClient.Do(req)
	if err != nil {
		// Network error during revalidation: prefer cached over a hard
		// failure so a momentary CDN blip doesn't take stream-setup down.
		// No-cache case is fatal as before.
		if cachedExists {
			if pc.logger != nil {
				pc.logger.Debug("cipher: revalidation network failure, serving cached", "err", err)
			}
			return FetchResult{JS: cachedJS, Changed: false}, nil
		}
		return FetchResult{}, fmt.Errorf("%w: %v", ErrPlayerJSFetch, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		if !cachedExists {
			// Shouldn't happen — we only sent conditional headers when
			// cachedExists. Treat as a hard fetch failure so the caller
			// re-attempts without the conditional.
			return FetchResult{}, fmt.Errorf("%w: server returned 304 with no cache", ErrPlayerJSFetch)
		}
		// Refresh mtimes on raw + preprocessed + meta so the 24h TTL
		// backstop doesn't age out the trio independently. Without this,
		// a long-idle player gets its raw .js refreshed by every 304 but
		// its preprocessed/meta sidecars expire on their own, forcing the
		// next compile to re-run preprocessing AND fall back to the
		// migration mtime path. Errors here are non-fatal.
		pc.touchCacheGroup(playerURL)
		return FetchResult{JS: cachedJS, Changed: false}, nil

	case http.StatusOK:
		body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10MB max player JS
		if err != nil {
			return FetchResult{}, fmt.Errorf("read player JS: %w", err)
		}
		js := string(body)

		newMeta := playerMeta{
			LastModified:  resp.Header.Get("Last-Modified"),
			ETag:          resp.Header.Get("ETag"),
			ContentLength: int64(len(body)),
		}

		// Last-Modified flap defense: identical body length = unchanged.
		// Player JS rotations grow materially (~12% per the 8fb635c2
		// rotation that drove this work); identical-length rotations are
		// improbable, and Stage 2's reactive invalidation catches anything
		// that slips through.
		if cachedExists && int64(len(cachedJS)) == int64(len(body)) {
			pc.writeMeta(playerURL, newMeta)
			pc.touchCacheGroup(playerURL)
			return FetchResult{JS: cachedJS, Changed: false}, nil
		}

		// Real change (or cold cache). Write new content + meta, drop the
		// preprocessed sidecar — it was derived from the old JS and would
		// otherwise short-circuit the next compile to stale bindings.
		if err := pc.Put(playerURL, js); err != nil && pc.logger != nil {
			pc.logger.Warn("cipher: failed to cache player JS to disk", "err", err)
		}
		pc.writeMeta(playerURL, newMeta)
		_ = os.Remove(pc.FilePathPreprocessed(playerURL))

		return FetchResult{JS: js, Changed: true}, nil

	default:
		if cachedExists {
			// Unusual status with cached fallback — same logic as network
			// error: don't crater stream-setup over a transient CDN issue.
			if pc.logger != nil {
				pc.logger.Debug("cipher: revalidation got unexpected status, serving cached",
					"status", resp.StatusCode)
			}
			return FetchResult{JS: cachedJS, Changed: false}, nil
		}
		return FetchResult{}, fmt.Errorf("%w: status %d", ErrPlayerJSFetch, resp.StatusCode)
	}
}

// touchCacheGroup refreshes the mtime on the raw, preprocessed, and meta
// sidecar files for a player URL. Called when conditional revalidation
// returns 304 (or 200 with identical Content-Length) so the trio ages
// out together rather than the preprocessed/meta sidecars expiring on
// their own and forcing the next compile to re-run preprocessing or
// fall back to the migration mtime path.
func (pc *PlayerCache) touchCacheGroup(playerURL string) {
	now := time.Now()
	for _, path := range []string{
		pc.FilePath(playerURL),
		pc.FilePathPreprocessed(playerURL),
		pc.FilePathMeta(playerURL),
	} {
		// Errors are non-fatal — a missing preprocessed/meta is normal on
		// migration or first-fetch flows.
		_ = os.Chtimes(path, now, now)
	}
}

// readCacheForRevalidation returns the cached JS, an exists flag, and the
// file mtime (zero when absent). Unlike Get, it does NOT delete TTL-expired
// entries — the caller still issues a conditional GET that may revalidate
// them; eviction happens only when YouTube responds with new content (or
// the periodic Evict tick fires).
func (pc *PlayerCache) readCacheForRevalidation(playerURL string) (js string, exists bool, mtime time.Time) {
	pc.mu.RLock()
	defer pc.mu.RUnlock()

	path := pc.FilePath(playerURL)
	info, err := os.Stat(path)
	if err != nil {
		return "", false, time.Time{}
	}
	if time.Since(info.ModTime()) > playerCacheTTL {
		// Beyond the offline backstop. Treat as missing — caller falls
		// through to a full GET.
		return "", false, time.Time{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, time.Time{}
	}
	return string(data), true, info.ModTime()
}

// Evict removes expired entries from the cache. Sweeps raw, preprocessed,
// and meta files together — they share TTL semantics, and an orphaned meta
// would mislead the next conditional GET (replay headers for content that
// was deleted).
func (pc *PlayerCache) Evict() error {
	entries, err := os.ReadDir(pc.cacheDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if time.Since(info.ModTime()) > playerCacheTTL {
			os.Remove(filepath.Join(pc.cacheDir, entry.Name()))
		}
	}
	return nil
}

// Remove deletes the cached player JS, preprocessed sidecar, and meta for
// a specific URL. Errors are logged but not returned — removal is
// best-effort (file may be held by another reader on Windows).
func (pc *PlayerCache) Remove(playerURL string) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	for _, path := range []string{
		pc.FilePath(playerURL),
		pc.FilePathPreprocessed(playerURL),
		pc.FilePathMeta(playerURL),
	} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			if pc.logger != nil {
				pc.logger.Debug("cipher: failed to remove cached player", "path", path, "err", err)
			}
		}
	}
}
