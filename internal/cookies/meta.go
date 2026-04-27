package cookies

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CookieMeta is the on-disk sidecar that tracks LastRefresh + verified
// platforms across Moombox restarts. Without it, periodic refresh fires
// immediately on every startup — wasting a ~5s headless-Chrome launch
// (~150 MB of memory) when the cookies were already fresh from before
// the restart. Audit reports/cookies.md #48.
type CookieMeta struct {
	// Version is the on-disk schema version. Bumped if the meta shape
	// changes incompatibly so a stale sidecar from an older Moombox
	// can be safely ignored.
	Version int `json:"version"`

	// LastRefresh is the wall-clock time of the most recent successful
	// refresh (or initial setup). Empty when the meta file has never
	// been written or loaded (treated as zero-value time).
	// `omitzero` (Go 1.24+) actually drops the zero time.Time from
	// the JSON output -- `omitempty` does NOT, because time.Time is a
	// struct and encoding/json's omitempty only triggers on
	// false/0/nil/empty-collection/empty-string values, never on a
	// struct's zero value. We deliberately use omitzero here so a
	// fresh sidecar that has never been refreshed produces a clean
	// JSON file without the misleading "0001-01-01T00:00:00Z".
	LastRefresh time.Time `json:"lastRefresh,omitzero"`

	// Platforms records which platforms verified at LastRefresh time.
	// Keyed by lowercase platform name ("youtube", "twitch").
	Platforms []string `json:"platforms,omitempty"`
}

// metaSchemaVersion is the current sidecar schema version. Bump when
// the JSON shape changes in a non-additive way.
const metaSchemaVersion = 1

// metaSidecarSuffix is appended to the cookies.txt path to derive the
// sidecar path. Using `.meta.json` rather than `.json` makes the file's
// purpose obvious in a directory listing.
const metaSidecarSuffix = ".meta.json"

// MetaPath returns the on-disk location of the meta sidecar for a
// given cookies.txt path. Empty input returns empty (no path).
func MetaPath(cookiePath string) string {
	if cookiePath == "" {
		return ""
	}
	return cookiePath + metaSidecarSuffix
}

// LoadMeta reads the sidecar from disk. Returns (nil, nil) when the
// file does not exist (fresh install, never-refreshed state) so
// callers don't need to differentiate "missing" from "load failure"
// in the common case. Schema-mismatched sidecars also return (nil, nil)
// — the caller treats them as missing and writes a fresh one.
//
// Genuine read errors (permission denied, corrupted JSON) return (nil,
// error) so the caller can log them.
func LoadMeta(cookiePath string) (*CookieMeta, error) {
	if cookiePath == "" {
		return nil, nil
	}
	data, err := os.ReadFile(MetaPath(cookiePath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read cookie meta: %w", err)
	}
	var meta CookieMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse cookie meta: %w", err)
	}
	if meta.Version != metaSchemaVersion {
		// Older or future schema — treat as missing rather than crash.
		return nil, nil
	}
	return &meta, nil
}

// SaveMeta writes the sidecar atomically (tempfile + rename). Caller
// passes the path to cookies.txt (NOT the sidecar) so the helper can
// derive the right name + directory consistently with LoadMeta.
//
// The platforms slice is normalised: lowercased, deduped, sorted —
// makes the on-disk shape stable across restarts and easier to diff
// when debugging.
func SaveMeta(cookiePath string, meta CookieMeta) error {
	if cookiePath == "" {
		return errors.New("cookies.SaveMeta: cookiePath is empty")
	}
	meta.Version = metaSchemaVersion
	meta.Platforms = normalisePlatforms(meta.Platforms)

	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cookie meta: %w", err)
	}

	path := MetaPath(cookiePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("ensure meta dir: %w", err)
	}
	return writeFileAtomic(path, data, 0o600)
}

// normalisePlatforms lowercases, dedupes, and sorts the platforms list
// so SaveMeta's output is stable.
func normalisePlatforms(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	// Sort for stable output; insertion order isn't significant.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
