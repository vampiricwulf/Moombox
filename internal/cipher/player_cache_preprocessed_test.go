package cipher

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestCacheKey_LocaleInsensitive verifies that CacheKey produces the same
// result for URLs that differ only in their locale segment (en_US, en_GB,
// de, etc.). This is the key invariant that prevents the solver LRU from
// holding two ~44 MB compiled goja Runtimes for the same player when the
// routed path (synthetic en_US URL) and the legacy path (watch-page URL
// with an arbitrary locale) both call GetSolvers.
func TestCacheKey_LocaleInsensitive(t *testing.T) {
	const playerID = "cb017549"
	urls := []string{
		"https://www.youtube.com/s/player/" + playerID + "/player_ias.vflset/en_US/base.js",
		"https://www.youtube.com/s/player/" + playerID + "/player_ias.vflset/en_GB/base.js",
		"https://www.youtube.com/s/player/" + playerID + "/player_ias.vflset/de/base.js",
	}

	want := CacheKey(urls[0])
	for _, u := range urls[1:] {
		got := CacheKey(u)
		if got != want {
			t.Errorf("CacheKey(%q) = %q; want %q (same as en_US)", u, got, want)
		}
	}
}

// TestCacheKey_DistinctPlayers verifies that two different playerIDs produce
// different cache keys — i.e. the playerID-only hashing is still discriminating.
func TestCacheKey_DistinctPlayers(t *testing.T) {
	keyA := CacheKey("https://www.youtube.com/s/player/cb017549/player_ias.vflset/en_US/base.js")
	keyB := CacheKey("https://www.youtube.com/s/player/74edf1a3/player_ias.vflset/en_US/base.js")
	if keyA == keyB {
		t.Errorf("CacheKey collision: different playerIDs produced the same key %q", keyA)
	}
}

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
// the prepared copy. Without this, an explicit invalidation
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

// TestPlayerCachePreprocessedFilePath: the prepared copy sits beside the raw
// .js under the same key, and its name carries the ejs commit the port
// mirrors, so a port of another ejs never runs a copy this one prepared.
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
	rawNoExt := strings.TrimSuffix(filepath.Base(rawPath), ".js")
	if want := rawNoExt + ".prepared-ejs-" + ejsPortVersion[:7] + "-r" + strconv.Itoa(ejsPortRevision) + ".js"; filepath.Base(prePath) != want {
		t.Errorf("prepared copy is %q, want %q", filepath.Base(prePath), want)
	}
}

// TestRegexExtractorCopyIsNeverRun (owner ruling RV-12): the copy the regex
// extractor prepared, <key>.preprocessed.js, is in another format, so the
// port never reads it, and it never writes there, which keeps that file an
// older binary's own if one runs over this directory again. A changed body
// and Remove drop it with the player's other files, so that older binary
// cannot compile a copy of a body that has since changed.
//
// Mutants: GetPreprocessed or FilePathPreprocessed back on the old name (the
// regex copy is served); the rotation or Remove leaving the old file, or
// leaving a copy another port wrote under another pin or revision.
func TestRegexExtractorCopyIsNeverRun(t *testing.T) {
	dir := t.TempDir()
	pc, err := NewPlayerCache(dir, nopPlayerCacheLogger{})
	if err != nil {
		t.Fatalf("NewPlayerCache: %v", err)
	}
	url := "https://www.youtube.com/s/player/abcdef12/player_ias.vflset/en_US/base.js"
	legacy := pc.legacyPreprocessedPath(url)
	if err := os.WriteFile(legacy, []byte("regex extractor output"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := pc.GetPreprocessed(url); err != nil || got != "" {
		t.Errorf("GetPreprocessed with only the regex copy on disk = %q, %v; want a miss", got, err)
	}
	if err := pc.PutPreprocessed(url, "port output"); err != nil {
		t.Fatalf("PutPreprocessed: %v", err)
	}
	if b, _ := os.ReadFile(legacy); string(b) != "regex extractor output" {
		t.Errorf("the regex copy was overwritten: %q", b)
	}
	other := filepath.Join(pc.cacheDir, CacheKey(url)+".prepared-ejs-0123456-r3.js")
	if err := os.WriteFile(other, []byte("another port's output"), 0o644); err != nil {
		t.Fatal(err)
	}
	pc.Remove(url)
	for _, p := range []string{legacy, other, pc.FilePathPreprocessed(url)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("Remove left %s: stat err = %v", filepath.Base(p), err)
		}
	}
}
