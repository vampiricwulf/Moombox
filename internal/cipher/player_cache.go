package cipher

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vampiricwulf/Moombox/internal/httpx"
)

const (
	playerCacheTTL    = 14 * 24 * time.Hour // 14 days
	playerCacheSubdir = "player_cache"
)

// playerHTTPClient fetches player.js. Backed by the shared httpx.Client
// transport for keep-alive amortisation across cipher / youtube / etc.
var playerHTTPClient = httpx.Client(30 * time.Second)

// PlayerCache manages disk-cached YouTube player JS files.
type PlayerCache struct {
	mu       sync.RWMutex
	cacheDir string
	logger   interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}
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

// CacheKey returns a short hex digest of the player URL for use as a cache
// key. Full 64-char SHA-256 was overkill for this domain (a few thousand
// entries at most) and hit Windows MAX_PATH limits faster when combined with
// a long cacheDir. 16 hex chars = 64 bits of hash space; collision probability
// across thousands of entries is ~1 in 2^50 — negligible for cache keys.
//
// Existing 64-char cached files from prior versions become unreachable and
// are harmlessly eviction-swept by the background 24h tick.
func CacheKey(playerURL string) string {
	h := sha256.Sum256([]byte(playerURL))
	return fmt.Sprintf("%x", h[:8]) // 8 bytes = 16 hex chars
}

// PlayerIDFromURL extracts the player ID from a player URL.
// e.g. "https://www.youtube.com/s/player/abcdef12/player_ias.vflset/en_US/base.js" -> "abcdef12"
func PlayerIDFromURL(playerURL string) string {
	parts := strings.Split(playerURL, "/")
	for i, p := range parts {
		if p == "player" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return CacheKey(playerURL)
}

// FilePath returns the cache file path for a player URL.
func (pc *PlayerCache) FilePath(playerURL string) string {
	key := CacheKey(playerURL)
	return filepath.Join(pc.cacheDir, key+".js")
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
	pc.mu.Lock()
	defer pc.mu.Unlock()

	path := pc.FilePath(playerURL)
	tmpPath := path + ".tmp"

	if err := os.WriteFile(tmpPath, []byte(js), 0o644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}

	return nil
}

// Fetch downloads player JS from the URL, caching on disk. Returns the JS source.
func (pc *PlayerCache) Fetch(ctx context.Context, playerURL string) (string, error) {
	// Check cache first
	cached, err := pc.Get(playerURL)
	if err == nil && cached != "" {
		return cached, nil
	}

	// Download
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, playerURL, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := playerHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrPlayerJSFetch, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: status %d", ErrPlayerJSFetch, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10MB max player JS
	if err != nil {
		return "", fmt.Errorf("read player JS: %w", err)
	}

	js := string(body)

	// Cache to disk (non-fatal if caching fails)
	if putErr := pc.Put(playerURL, js); putErr != nil && pc.logger != nil {
		pc.logger.Warn("cipher: failed to cache player JS to disk", "err", putErr)
	}

	return js, nil
}

// Evict removes expired entries from the cache.
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

// Remove deletes the cached player JS for a specific URL.
// Errors are logged but not returned — removal is best-effort
// (file may be held by another reader on Windows).
func (pc *PlayerCache) Remove(playerURL string) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	path := pc.FilePath(playerURL)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		if pc.logger != nil {
			pc.logger.Debug("cipher: failed to remove cached player", "path", path, "err", err)
		}
	}
}
