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
	"time"
)

const (
	playerCacheTTL    = 14 * 24 * time.Hour // 14 days
	playerCacheSubdir = "player_cache"
)

// PlayerCache manages disk-cached YouTube player JS files.
type PlayerCache struct {
	cacheDir string
}

// NewPlayerCache creates a player cache at the given base directory.
// If cacheDir is empty, uses ~/.cache/yt-cipher/.
func NewPlayerCache(cacheDir string) (*PlayerCache, error) {
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

	return &PlayerCache{cacheDir: dir}, nil
}

// CacheKey returns the SHA256 hash of the player URL for use as a cache key.
func CacheKey(playerURL string) string {
	h := sha256.Sum256([]byte(playerURL))
	return fmt.Sprintf("%x", h)
}

// OverridePlayerURL allows rewriting a player URL before use.
// Currently a pass-through; can be extended to force a specific player version/variant.
func OverridePlayerURL(playerURL string) string {
	return playerURL
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
func (pc *PlayerCache) Get(playerURL string) (string, error) {
	path := pc.FilePath(playerURL)

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}

	// Check TTL
	if time.Since(info.ModTime()) > playerCacheTTL {
		os.Remove(path)
		return "", nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	// Touch access time (update mod time to extend TTL on access)
	now := time.Now()
	os.Chtimes(path, now, now)

	return string(data), nil
}

// Put caches player JS for the given URL. Uses atomic write (tmp + rename).
func (pc *PlayerCache) Put(playerURL string, js string) error {
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

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch player JS: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch player JS: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read player JS: %w", err)
	}

	js := string(body)

	// Cache to disk
	if putErr := pc.Put(playerURL, js); putErr != nil {
		// Non-fatal: continue even if caching fails
		_ = putErr
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
