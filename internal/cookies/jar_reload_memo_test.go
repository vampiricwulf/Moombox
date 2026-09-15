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

// statOf is a small helper for the tests below that need to capture and
// later restore an exact (size, mtime) pair.
func statOf(t *testing.T, path string) os.FileInfo {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// TestLoadServesTheMemoWithoutReadingTheFile is ledger item T2-23:
// GetVideoInfo and GetVideoInfoAuthenticated both SyncCookies, so
// cookies.txt was read and parsed twice per extraction.
//
// The positive half of the short-circuit is observed directly rather than
// through a planted marker: the file's bytes are changed to a same-length-or-
// not value the parse would clearly surface if it ran, its (size, mtime) is
// then restored EXACTLY to what Load already recorded, and the assertion is
// that the OLD value still comes back on the next Load. The only way that
// can happen is if the second Load never opened the file — a real re-read
// would see the rewritten content, since the rewrite is on disk and complete
// well before the second Load runs.
//
// Mutant named: any code path that performs a real read on the second Load
// call — even one that then discards or ignores the result — makes this
// test observe the NEW value instead of the old one. Outside the settle
// window, an unchanged (size, mtime) pair is exactly what the design trusts
// without re-opening the file.
func TestLoadServesTheMemoWithoutReadingTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	const oldValue = "AAAAAAAA"
	const newValue = "BBBBBBBB" // same length as oldValue: the stat below must be restorable exactly
	if len(oldValue) != len(newValue) {
		t.Fatalf("fixture values must be equal length")
	}
	agedCookieFile(t, path, []string{cookieRow(".youtube.com", futureExpiry(), "SID", oldValue)}, time.Hour)

	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != oldValue {
		t.Fatalf("setup: SID = %q, want %q", got, oldValue)
	}
	st := statOf(t, path)
	size, mod := st.Size(), st.ModTime()

	// Same-length rewrite, mtime forced back to the recorded value: from the
	// jar's point of view (stat only) this file is indistinguishable from
	// the one it already parsed, even though the bytes differ.
	content := "# Netscape HTTP Cookie File\n" + cookieRow(".youtube.com", futureExpiry(), "SID", newValue) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
	if st2 := statOf(t, path); st2.Size() != size || !st2.ModTime().Equal(mod) {
		t.Fatalf("fixture rewrite did not restore (size, mtime): got (%d, %v), want (%d, %v)",
			st2.Size(), st2.ModTime(), size, mod)
	}

	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != oldValue {
		t.Errorf("SID = %q, want %q — Load must not have re-read a file whose (size, mtime) it already trusts",
			got, oldValue)
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

// TestLoadOfARecreatedFileWithTheOldStatIsReparsed pins the not-exist
// branch's loadedMemo reset (jar.go's Load, the os.IsNotExist arm). Without
// it, a file that is deleted and then recreated with the SAME (size, mtime)
// the jar last trusted would short-circuit on that stale pair and keep
// serving the pre-deletion state forever, even though the jar was supposed
// to have cleared itself the moment the file went away.
//
// Mutant named: dropping `j.loadedMemo = false` from the not-exist branch.
func TestLoadOfARecreatedFileWithTheOldStatIsReparsed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	const original = "original" // same length as replacement below
	const replacement = "replaced"
	if len(original) != len(replacement) {
		t.Fatalf("fixture values must be equal length")
	}
	agedCookieFile(t, path, []string{cookieRow(".youtube.com", futureExpiry(), "SID", original)}, time.Hour)

	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	st := statOf(t, path)
	size, mod := st.Size(), st.ModTime()

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != "" {
		t.Fatalf("setup: SID = %q after delete, want empty", got)
	}

	// Recreate a file whose (size, mtime) exactly match what was recorded
	// BEFORE the deletion, but whose content differs — only a real re-read
	// tells the two apart.
	content := "# Netscape HTTP Cookie File\n" + cookieRow(".youtube.com", futureExpiry(), "SID", replacement) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
	if st2 := statOf(t, path); st2.Size() != size || !st2.ModTime().Equal(mod) {
		t.Fatalf("fixture recreate did not restore (size, mtime): got (%d, %v), want (%d, %v)",
			st2.Size(), st2.ModTime(), size, mod)
	}

	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != replacement {
		t.Errorf("SID = %q, want %q — a recreated file with the pre-deletion (size, mtime) was short-circuited",
			got, replacement)
	}
}

// TestLoadOnlyMemoisesWhenTheStatsAgree is the regression test for the
// finding that the pair could be memoised from the post-read stat ALONE.
//
// A goroutine that stalls between the read returning and the post-read stat
// — descheduled, paged out, unlucky timing on a saturated 24/7 box — can let
// a rewrite land in that exact gap. Trusting the post-read stat by itself
// would then memoise a (size, mtime) pair that describes bytes the maps
// never actually held (the read returned the OLD content; the stat sees the
// NEW file), and every later Load would short-circuit on a file it never
// truly read. Before the memo existed this race harmlessly self-healed on
// the very next Load; the pre/post agreement check exists to keep that
// property.
//
// cookieJarReadFile is the seam: this test wraps it to perform the rewrite
// the instant after the real read returns, simulating the unlucky gap
// without needing an actual scheduling stall.
//
// Mutant named: computing loadedMemo from the post-read stat's own settle
// check alone (dropping the preOK/preSt agreement clauses). With that
// mutant, the rewritten file's OWN (aged) mtime clears the settle check on
// its own, the pair is memoised, and the second Load below wrongly serves
// the pre-rewrite value from the short-circuit instead of reading the file
// that is actually on disk.
func TestLoadOnlyMemoisesWhenTheStatsAgree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	const beforeRead = "before-read" // deliberately a different length than afterRead
	const afterRead = "after-read"
	if len(beforeRead) == len(afterRead) {
		t.Fatalf("fixture values must differ in length so a stat-only mutant cannot coincidentally agree")
	}
	agedCookieFile(t, path, []string{cookieRow(".youtube.com", futureExpiry(), "SID", beforeRead)}, time.Hour)

	orig := cookieJarReadFile
	t.Cleanup(func() { cookieJarReadFile = orig })
	cookieJarReadFile = func(name string) ([]byte, error) {
		data, err := orig(name)
		if err == nil && name == path {
			// Land the rewrite in the gap between the read returning and
			// Load's post-read stat. Aged so its OWN mtime would also clear
			// the settle window in isolation — the mutant this pins relies
			// on exactly that to misbehave.
			agedCookieFile(t, path, []string{cookieRow(".youtube.com", futureExpiry(), "SID", afterRead)}, time.Hour)
		}
		return data, err
	}

	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	// The read captured beforeRead; the injected rewrite must not have been
	// memoised as if it were what got parsed.
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != beforeRead {
		t.Fatalf("setup: SID = %q, want %q", got, beforeRead)
	}

	cookieJarReadFile = orig // restore before the real second Load
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != afterRead {
		t.Errorf("SID = %q, want %q — a rewrite landing between the pre- and post-read stats must not have been memoised",
			got, afterRead)
	}
}
