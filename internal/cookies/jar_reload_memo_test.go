package cookies

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// agedCookieFile writes rows to path and back-dates its mtime by age, so the
// stat memo is outside the racily-clean settle window and may be trusted.
func agedCookieFile(t *testing.T, path string, rows []string, age time.Duration) {
	t.Helper()
	content := "# Netscape HTTP Cookie File\n"
	for _, r := range rows {
		content += r + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func futureExpiry() string {
	return strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10)
}

// TestLoadShortCircuitsOnAnUnchangedFile is ledger item T2-23: GetVideoInfo
// and GetVideoInfoAuthenticated both SyncCookies, so cookies.txt was read and
// parsed twice per extraction.
//
// The seam is the jar itself: loadFrom writes state the FILE does not carry
// and leaves the memo cleared-then-unrecorded, so if the second Load re-reads
// the file it will overwrite that state with the file's.
//
// Mutant named: deleting the short-circuit (or comparing only the path) makes
// the second Load re-parse and SID goes back to "from-file".
func TestLoadShortCircuitsOnAnUnchangedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	agedCookieFile(t, path, []string{cookieRow(".youtube.com", futureExpiry(), "SID", "from-file")}, time.Hour)

	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	jar.loadFrom([]byte("# Netscape HTTP Cookie File\n"+
		cookieRow(".youtube.com", futureExpiry(), "SID", "from-memory")+"\n"), path)

	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != "from-memory" {
		t.Errorf("SID = %q, want %q — the second Load re-parsed an unchanged file", got, "from-memory")
	}
}

// TestLoadReparsesAfterAWrite pins the invalidation half.
//
// Mutant named: a memo that keys on the path alone, or that is recorded
// before the read rather than after it, never notices the rewrite.
func TestLoadReparsesAfterAWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	agedCookieFile(t, path, []string{cookieRow(".youtube.com", futureExpiry(), "SID", "first")}, time.Hour)

	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	agedCookieFile(t, path, []string{cookieRow(".youtube.com", futureExpiry(), "SID", "second-value")}, 30*time.Minute)
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != "second-value" {
		t.Errorf("SID = %q, want %q — a rewritten file was served from the memo", got, "second-value")
	}
}

// TestLoadNeverTrustsAFreshlyWrittenFile pins the racily-clean rule: a file
// whose mtime is inside cookieJarStatSettle of the load is never memoised.
//
// Mutant named: dropping the settle window. cookies.txt has writers that
// write twice inside one timestamp tick — the verify-and-roll-back pass
// writes the new set and then restores the previous one, and a restore that
// differs only in expiry digits has the same byte length — so a size+mtime
// pair alone would leave the jar holding credentials the file no longer has.
func TestLoadNeverTrustsAFreshlyWrittenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	agedCookieFile(t, path, []string{cookieRow(".youtube.com", futureExpiry(), "SID", "from-file")}, 0)

	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	jar.loadFrom([]byte("# Netscape HTTP Cookie File\n"+
		cookieRow(".youtube.com", futureExpiry(), "SID", "from-memory")+"\n"), path)

	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != "from-file" {
		t.Errorf("SID = %q, want %q — a file written this instant was memoised", got, "from-file")
	}
}

// TestLoadOfADeletedFileStillClearsTheJar pins that the memo is consulted
// only after a successful stat.
//
// Mutant named: a short-circuit placed ahead of the stat (or one that ignores
// the stat error) reports success and leaves a deleted credential file's
// cookies live in memory forever.
func TestLoadOfADeletedFileStillClearsTheJar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	agedCookieFile(t, path, []string{cookieRow(".youtube.com", futureExpiry(), "SID", "from-file")}, time.Hour)

	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := jar.Load(path); err != nil {
		t.Fatalf("Load of a missing file must stay a nil-error no-op: %v", err)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != "" {
		t.Errorf("SID = %q, want empty — a deleted cookie file was served from the memo", got)
	}
}
