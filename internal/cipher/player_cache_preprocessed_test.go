package cipher

import (
	"os"
	"path/filepath"
	"testing"
)

// nopLogger is a minimal logger sink for player_cache tests that don't
// exercise log output.
type nopPlayerCacheLogger struct{}

func (nopPlayerCacheLogger) Debug(msg string, args ...any) {}
func (nopPlayerCacheLogger) Info(msg string, args ...any)  {}
func (nopPlayerCacheLogger) Warn(msg string, args ...any)  {}
func (nopPlayerCacheLogger) Error(msg string, args ...any) {}

// TestPlayerCachePreprocessedRoundTrip exercises the new disk-cache tier
// for preprocessed player code added in test.35 (audit cipher.md Q4).
// PutPreprocessed writes; GetPreprocessed reads; the bytes round-trip.
func TestPlayerCachePreprocessedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	pc, err := NewPlayerCache(dir, nopPlayerCacheLogger{})
	if err != nil {
		t.Fatalf("NewPlayerCache: %v", err)
	}

	url := "https://www.youtube.com/s/player/abcd1234/player_ias.vflset/en_US/base.js"
	want := "var _yt_player = { /* preprocessed body with sig + n bindings */ };"

	if err := pc.PutPreprocessed(url, want); err != nil {
		t.Fatalf("PutPreprocessed: %v", err)
	}

	got, err := pc.GetPreprocessed(url)
	if err != nil {
		t.Fatalf("GetPreprocessed: %v", err)
	}
	if got != want {
		t.Errorf("GetPreprocessed round-trip: want %q, got %q", want, got)
	}
}

// TestPlayerCachePreprocessedMissingReturnsEmpty — fresh cache returns
// "" without an error so the caller treats it as "not cached, run the
// slow path".
func TestPlayerCachePreprocessedMissingReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	pc, err := NewPlayerCache(dir, nopPlayerCacheLogger{})
	if err != nil {
		t.Fatalf("NewPlayerCache: %v", err)
	}

	got, err := pc.GetPreprocessed("https://www.youtube.com/s/player/never/seen.js")
	if err != nil {
		t.Errorf("missing preprocessed: want nil err, got %v", err)
	}
	if got != "" {
		t.Errorf("missing preprocessed: want empty, got %q", got)
	}
}

// TestPlayerCacheRemoveCleansBothFiles — Remove drops the raw .js AND
// the .preprocessed.js sidecar. Without this, an explicit invalidation
// (cipher solver flagged broken at runtime) would leave a stale
// preprocessed sidecar that the next compile picks up.
func TestPlayerCacheRemoveCleansBothFiles(t *testing.T) {
	dir := t.TempDir()
	pc, err := NewPlayerCache(dir, nopPlayerCacheLogger{})
	if err != nil {
		t.Fatalf("NewPlayerCache: %v", err)
	}

	url := "https://www.youtube.com/s/player/test/base.js"
	if err := pc.Put(url, "raw"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := pc.PutPreprocessed(url, "preprocessed"); err != nil {
		t.Fatalf("PutPreprocessed: %v", err)
	}

	rawPath := pc.FilePath(url)
	prePath := pc.FilePathPreprocessed(url)
	if _, err := os.Stat(rawPath); err != nil {
		t.Fatalf("raw file should exist: %v", err)
	}
	if _, err := os.Stat(prePath); err != nil {
		t.Fatalf("preprocessed file should exist: %v", err)
	}

	pc.Remove(url)

	if _, err := os.Stat(rawPath); !os.IsNotExist(err) {
		t.Errorf("raw file should be removed: stat err = %v", err)
	}
	if _, err := os.Stat(prePath); !os.IsNotExist(err) {
		t.Errorf("preprocessed file should be removed: stat err = %v", err)
	}
}

// TestPlayerCachePreprocessedFilePath exercises the path derivation —
// must end in .preprocessed.js sibling to the raw .js (same key).
func TestPlayerCachePreprocessedFilePath(t *testing.T) {
	dir := t.TempDir()
	pc, err := NewPlayerCache(dir, nopPlayerCacheLogger{})
	if err != nil {
		t.Fatalf("NewPlayerCache: %v", err)
	}
	url := "https://www.youtube.com/s/player/abcdef12/player_ias.vflset/en_US/base.js"

	rawPath := pc.FilePath(url)
	prePath := pc.FilePathPreprocessed(url)
	if filepath.Dir(rawPath) != filepath.Dir(prePath) {
		t.Errorf("raw + preprocessed should live in the same dir: raw=%q pre=%q", rawPath, prePath)
	}
	rawKey := filepath.Base(rawPath)
	preKey := filepath.Base(prePath)
	rawNoExt := rawKey[:len(rawKey)-len(".js")]
	preNoExt := preKey[:len(preKey)-len(".preprocessed.js")]
	if rawNoExt != preNoExt {
		t.Errorf("raw + preprocessed should share key: raw=%q pre=%q", rawNoExt, preNoExt)
	}
}
