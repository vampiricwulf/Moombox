package cookies

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMetaPathDerivation locks the .meta.json suffix contract.
func TestMetaPathDerivation(t *testing.T) {
	tests := []struct {
		cookiePath string
		want       string
	}{
		{"", ""},
		{"cookies.txt", "cookies.txt.meta.json"},
		{"/path/to/cookies.txt", "/path/to/cookies.txt.meta.json"},
	}
	for _, tc := range tests {
		t.Run(tc.cookiePath, func(t *testing.T) {
			if got := MetaPath(tc.cookiePath); got != tc.want {
				t.Errorf("MetaPath(%q) = %q, want %q", tc.cookiePath, got, tc.want)
			}
		})
	}
}

// TestLoadMetaMissingFileReturnsNilNil — fresh install / first run.
func TestLoadMetaMissingFileReturnsNilNil(t *testing.T) {
	dir := t.TempDir()
	cookiePath := filepath.Join(dir, "cookies.txt")
	meta, err := LoadMeta(cookiePath)
	if err != nil {
		t.Fatalf("LoadMeta on missing file: want nil err, got %v", err)
	}
	if meta != nil {
		t.Errorf("LoadMeta on missing file: want nil meta, got %+v", meta)
	}
}

// TestLoadMetaEmptyCookiePathReturnsNilNil — defensive against
// uninitialised callers.
func TestLoadMetaEmptyCookiePathReturnsNilNil(t *testing.T) {
	meta, err := LoadMeta("")
	if err != nil {
		t.Errorf("LoadMeta(\"\"): want nil err, got %v", err)
	}
	if meta != nil {
		t.Errorf("LoadMeta(\"\"): want nil meta, got %+v", meta)
	}
}

// TestSaveAndLoadMetaRoundTrip covers the happy path: write meta,
// read it back, verify equality.
func TestSaveAndLoadMetaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cookiePath := filepath.Join(dir, "cookies.txt")
	now := time.Now().UTC().Truncate(time.Second)

	saveMeta := CookieMeta{
		LastRefresh: now,
		Platforms:   []string{"youtube", "twitch"},
	}
	if err := SaveMeta(cookiePath, saveMeta); err != nil {
		t.Fatalf("SaveMeta: %v", err)
	}

	loaded, err := LoadMeta(cookiePath)
	if err != nil {
		t.Fatalf("LoadMeta: %v", err)
	}
	if loaded == nil {
		t.Fatal("LoadMeta after SaveMeta: got nil")
	}
	if !loaded.LastRefresh.Equal(now) {
		t.Errorf("LastRefresh: want %v, got %v", now, loaded.LastRefresh)
	}
	if loaded.Version != metaSchemaVersion {
		t.Errorf("Version: want %d, got %d", metaSchemaVersion, loaded.Version)
	}
	if len(loaded.Platforms) != 2 {
		t.Errorf("Platforms count: want 2, got %d", len(loaded.Platforms))
	}
}

// TestSaveMetaNormalisesPlatforms verifies sorted + deduped + lowered.
func TestSaveMetaNormalisesPlatforms(t *testing.T) {
	dir := t.TempDir()
	cookiePath := filepath.Join(dir, "cookies.txt")

	if err := SaveMeta(cookiePath, CookieMeta{
		Platforms: []string{"Twitch", "YouTube", "youtube", "  ", ""},
	}); err != nil {
		t.Fatalf("SaveMeta: %v", err)
	}

	loaded, err := LoadMeta(cookiePath)
	if err != nil {
		t.Fatalf("LoadMeta: %v", err)
	}
	got := loaded.Platforms
	want := []string{"twitch", "youtube"}
	if len(got) != len(want) {
		t.Fatalf("Platforms: want %v, got %v", want, got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("Platforms[%d]: want %q, got %q", i, want[i], got[i])
		}
	}
}

// TestLoadMetaWrongSchemaVersionTreatedAsMissing — older / future
// schema returns (nil, nil) so the caller writes a fresh sidecar
// rather than crashing on a stale layout.
func TestLoadMetaWrongSchemaVersionTreatedAsMissing(t *testing.T) {
	dir := t.TempDir()
	cookiePath := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(MetaPath(cookiePath), []byte(`{"version": 999, "lastRefresh": "2026-01-01T00:00:00Z"}`), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	meta, err := LoadMeta(cookiePath)
	if err != nil {
		t.Errorf("LoadMeta on future-version sidecar: want nil err, got %v", err)
	}
	if meta != nil {
		t.Errorf("LoadMeta on future-version sidecar: want nil meta, got %+v", meta)
	}
}

// TestLoadMetaCorruptedJSONReturnsError — distinguishes a real parse
// error from a missing file. Caller logs but doesn't crash.
func TestLoadMetaCorruptedJSONReturnsError(t *testing.T) {
	dir := t.TempDir()
	cookiePath := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(MetaPath(cookiePath), []byte(`{not json}`), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, err := LoadMeta(cookiePath)
	if err == nil {
		t.Fatal("LoadMeta on corrupted JSON: want error, got nil")
	}
}

// TestSaveMetaEmptyCookiePathErrors — defensive contract.
func TestSaveMetaEmptyCookiePathErrors(t *testing.T) {
	if err := SaveMeta("", CookieMeta{}); err == nil {
		t.Error("SaveMeta(\"\"): want error, got nil")
	}
}

// TestSaveMetaOmitsZeroLastRefresh locks F31: a CookieMeta whose
// LastRefresh is the zero time.Time should serialise WITHOUT a
// "lastRefresh" key in the JSON output (omitempty). Pre-fix, the
// 2026-04-26 go-fix pass stripped omitempty and zero-time wrote as
// "0001-01-01T00:00:00Z", bloating the sidecar and confusing
// downstream readers that distinguished "never set" from "explicit
// epoch".
func TestSaveMetaOmitsZeroLastRefresh(t *testing.T) {
	dir := t.TempDir()
	cookiePath := filepath.Join(dir, "cookies.txt")

	if err := SaveMeta(cookiePath, CookieMeta{
		Platforms: []string{"youtube"},
		// LastRefresh deliberately omitted — its zero value should not
		// produce a "lastRefresh" key in the serialised JSON.
	}); err != nil {
		t.Fatalf("SaveMeta: %v", err)
	}

	raw, err := os.ReadFile(MetaPath(cookiePath))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got := string(raw); got != "" && containsKey(got, "lastRefresh") {
		t.Errorf("expected lastRefresh key absent for zero-time; got JSON: %s", got)
	}
}

// containsKey is a tiny JSON key probe -- avoids the cost of a full
// json.Unmarshal-into-map for a one-shot membership check.
func containsKey(jsonStr, key string) bool {
	for i := 0; i < len(jsonStr)-len(key)-2; i++ {
		if jsonStr[i] == '"' && jsonStr[i+1+len(key)] == '"' && jsonStr[i+1:i+1+len(key)] == key {
			return true
		}
	}
	return false
}
