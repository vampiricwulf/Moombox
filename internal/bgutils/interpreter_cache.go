package bgutils

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// interpreterHashSidecarFilename is the on-disk filename for the
// hash cache. Sits in BgConfig.CacheDir alongside any other bgutils
// state files. Audit reports/bgutils.md QI-4 / TD-5.
const interpreterHashSidecarFilename = "bgutils-interpreter-hash.json"

// interpreterHashSchemaVersion is the on-disk schema version.
const interpreterHashSchemaVersion = 1

// interpreterHashEntry is a single cache entry: the hash itself plus
// the wall-clock time it was last seen. Stale entries (>30 days) are
// pruned on load to bound the file size.
type interpreterHashEntry struct {
	Hash    string    `json:"hash"`
	SavedAt time.Time `json:"savedAt"`
}

// interpreterHashSidecar is the on-disk JSON shape.
type interpreterHashSidecar struct {
	Version int                             `json:"version"`
	Hashes  map[string]interpreterHashEntry `json:"hashes,omitempty"`
}

// interpreterHashCache is the in-memory cache backing the disk
// sidecar. Process-wide singleton populated lazily on first access.
type interpreterHashCache struct {
	mu       sync.Mutex
	cacheDir string
	hashes   map[string]interpreterHashEntry
	loaded   bool
}

// global cache; one per process. The CacheDir on first non-empty
// FetchChallenge call wins; subsequent calls with a different dir
// log a warning but use the original.
var globalInterpreterHashCache = &interpreterHashCache{}

// uaMajorRe extracts the major Chrome version from UserAgentFull (e.g.
// "Chrome/131.0.0.0" → "131"). Used to namespace the hash cache so a
// future UA change automatically invalidates the cached hash without
// needing an explicit migration.
var uaMajorRe = regexp.MustCompile(`Chrome/(\d+)\.`)

// hashCacheKey returns the lookup key for a given requestKey + UA.
// The UA dependency is intentional: a different Chrome major version
// would expect a different interpreter, so caching across versions is
// unsafe.
func hashCacheKey(requestKey, userAgent string) string {
	uaMajor := "unknown"
	if m := uaMajorRe.FindStringSubmatch(userAgent); m != nil {
		uaMajor = m[1]
	}
	return requestKey + "|" + uaMajor
}

// loadInterpreterHashCache reads the sidecar from disk and populates
// the in-memory map. Safe to call repeatedly; only loads on first
// call. Missing file = empty cache. Schema mismatch = empty cache (a
// fresh write replaces the stale file). Genuine read errors return
// false so the caller can log them.
func (c *interpreterHashCache) load(cacheDir string, logger interface {
	Warn(msg string, args ...any)
}) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded {
		return true
	}
	c.loaded = true
	c.cacheDir = cacheDir
	c.hashes = make(map[string]interpreterHashEntry)
	if cacheDir == "" {
		return true
	}
	data, err := os.ReadFile(filepath.Join(cacheDir, interpreterHashSidecarFilename))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) && logger != nil {
			logger.Warn("interpreter-hash cache: read failed", "err", err)
		}
		return true
	}
	var s interpreterHashSidecar
	if err := json.Unmarshal(data, &s); err != nil {
		if logger != nil {
			logger.Warn("interpreter-hash cache: parse failed", "err", err)
		}
		return true
	}
	if s.Version != interpreterHashSchemaVersion {
		// Different schema; treat as empty so a fresh write replaces.
		return true
	}
	// Prune entries older than 30 days (defense against unbounded growth).
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	for k, e := range s.Hashes {
		if e.SavedAt.After(cutoff) {
			c.hashes[k] = e
		}
	}
	return true
}

// get returns the cached hash for a key, or "" if not found.
func (c *interpreterHashCache) get(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		return "" // load() not called yet
	}
	if e, ok := c.hashes[key]; ok {
		return e.Hash
	}
	return ""
}

// set updates the cache and persists to disk. Errors during persist
// are logged but don't fail the call (the in-memory cache stays
// consistent across the process).
func (c *interpreterHashCache) set(key, hash string, logger interface {
	Warn(msg string, args ...any)
}) {
	c.mu.Lock()
	c.hashes[key] = interpreterHashEntry{
		Hash:    hash,
		SavedAt: time.Now(),
	}
	cacheDir := c.cacheDir
	snapshot := make(map[string]interpreterHashEntry, len(c.hashes))
	maps.Copy(snapshot, c.hashes)
	c.mu.Unlock()

	if cacheDir == "" {
		return
	}
	if err := writeInterpreterHashSidecar(cacheDir, snapshot); err != nil {
		if logger != nil {
			logger.Warn("interpreter-hash cache: write failed", "err", err)
		}
	}
}

// writeInterpreterHashSidecar atomically writes the sidecar.
func writeInterpreterHashSidecar(cacheDir string, hashes map[string]interpreterHashEntry) error {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return fmt.Errorf("ensure cache dir: %w", err)
	}
	s := interpreterHashSidecar{
		Version: interpreterHashSchemaVersion,
		Hashes:  hashes,
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	path := filepath.Join(cacheDir, interpreterHashSidecarFilename)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
