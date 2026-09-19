package cookies

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
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
// Fix round 2 (I2): the original version of this test planted its marker
// through jar.loadFrom, which was a sound seam in fix round 1 (loadFrom left
// the memo untouched) but became a false positive once fix round 1's own
// minor (b) restored loadFrom's memo-clearing — after that, loadFrom clearing
// the memo made the second Load re-read regardless of whether the settle
// window existed, so this test passed whether or not the rule it claimed to
// pin was even present. Rewritten to (d)'s direct-observation technique
// instead: a same-size rewrite with the stat forced back to what Load itself
// recorded, so the only way the assertion can hold is if the settle window
// genuinely refused to memoise a freshly written file's pair.
//
// Mutant named: dropping the `time.Since(postSt.ModTime()) >=
// cookieJarStatSettle` clause from Load's post-read memo computation while
// KEEPING the pre/post agreement check. cookies.txt has writers that write it
// twice inside one timestamp tick — the verify-and-roll-back pass writes the
// new set and then restores the previous one, and a restore that differs
// only in expiry digits has the SAME byte length — so a size+mtime pair
// trusted the instant it is recorded, with no settle margin, would leave the
// jar holding credentials the file no longer has. Verified by execution: with
// that clause removed, this test fails (the short-circuit fires on the
// freshly-written pair and the rewrite is hidden) while it passes with the
// clause present.
func TestLoadNeverTrustsAFreshlyWrittenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	const oldValue = "AAAAAAAA"
	const newValue = "BBBBBBBB" // same length as oldValue: the stat below must be restorable exactly
	if len(oldValue) != len(newValue) {
		t.Fatalf("fixture values must be equal length")
	}
	agedCookieFile(t, path, []string{cookieRow(".youtube.com", futureExpiry(), "SID", oldValue)}, 0) // fresh: mtime = now

	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != oldValue {
		t.Fatalf("setup: SID = %q, want %q", got, oldValue)
	}
	st := statOf(t, path)
	size, mod := st.Size(), st.ModTime()

	// Same-length rewrite, mtime forced back to the pair Load just recorded:
	// from the jar's point of view (stat only) this looks exactly like the
	// file it already parsed. The settle window is the ONLY thing that can
	// still tell them apart, since the first Load happened well inside it.
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
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != newValue {
		t.Errorf("SID = %q, want %q — a freshly written file's pair was memoised and its rewrite hidden",
			got, newValue)
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

// TestLoadClearsTheMemoWhenThePostReadStatFails is fix round 2's Minor 1:
// Load's post-read stat block had an `if postSt, postErr := os.Stat(...);
// postErr == nil && ... { ... }` with no else. If the file vanishes between
// the read returning and that stat — an unlucky delete, not necessarily a
// concurrent Moombox writer — an EARLIER loadedMemo=true and its (size,
// mtime) pair from a PRIOR successful Load simply survive, unrelated to the
// content this Load's read actually saw and installed.
//
// cookieJarReadFile is the seam: this test wraps it to delete the file the
// instant after the real read returns, landing the vanish exactly in the gap
// between the read and Load's post-read stat — the same technique
// TestLoadOnlyMemoisesWhenTheStatsAgree uses for a rewrite instead of a
// delete.
//
// The scenario needs a REAL, ordinary rewrite between the first Load and the
// vanish-during-read Load: the first Load's memo legitimately describes the
// file as it existed then, and the SECOND Load can only reach the read (where
// the vanish is injected) if its own pre-read check sees a genuinely
// different stat and declines to short-circuit — exactly what a plain
// rewrite (different mtime) provides, with nothing artificial about it.
//
// Mutant named: dropping (or gating away, e.g. behind `j.loadGen == gen`
// alone without also requiring `j.filePath == filePath`, or vice versa) the
// `j.loadedMemo = false` assignment in Load's post-read-stat-error branch.
// With that mutant the FIRST Load's trusted pair outlives the vanish-during-
// read Load, and a THIRD Load against a file recreated with that exact
// (now long-stale, and by this point completely unrelated to the jar's
// actual in-memory content) pair short-circuits instead of re-reading.
func TestLoadClearsTheMemoWhenThePostReadStatFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	const first = "AAAAAAAA"
	const second = "BBBBBBBB"
	const third = "CCCCCCCC"
	if len(first) != len(second) {
		t.Fatalf("fixture values must be equal length")
	}
	if len(first) != len(third) {
		t.Fatalf("fixture values must be equal length")
	}
	agedCookieFile(t, path, []string{cookieRow(".youtube.com", futureExpiry(), "SID", first)}, time.Hour)

	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != first {
		t.Fatalf("setup: SID = %q, want %q", got, first)
	}
	st1 := statOf(t, path)
	size1, mod1 := st1.Size(), st1.ModTime()

	// A genuine rewrite: legitimately invalidates the first Load's memo at
	// the PRE-read check (different mtime), so the vanish-during-read Load
	// below actually reaches cookieJarReadFile. This is what puts the FIRST
	// Load's loadedMemo=true and its (size1, mod1) pair at risk of
	// surviving the SECOND Load's own post-stat failure.
	agedCookieFile(t, path, []string{cookieRow(".youtube.com", futureExpiry(), "SID", second)}, 30*time.Minute)

	orig := cookieJarReadFile
	t.Cleanup(func() { cookieJarReadFile = orig })
	cookieJarReadFile = func(name string) ([]byte, error) {
		data, err := orig(name)
		if err == nil && name == path {
			// Land the vanish in the gap between the read returning and
			// Load's post-read stat.
			if rmErr := os.Remove(path); rmErr != nil {
				t.Fatal(rmErr)
			}
		}
		return data, err
	}
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	cookieJarReadFile = orig
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != second {
		t.Fatalf("setup: SID = %q after the vanish-during-read Load, want %q", got, second)
	}

	// Recreate the file with a THIRD value but the exact (size, mtime) the
	// FIRST Load recorded — the only pair a surviving, uncleared memo could
	// still be pinned to; it describes neither the second Load's content nor
	// this one.
	content := "# Netscape HTTP Cookie File\n" + cookieRow(".youtube.com", futureExpiry(), "SID", third) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod1, mod1); err != nil {
		t.Fatal(err)
	}
	if st3 := statOf(t, path); st3.Size() != size1 || !st3.ModTime().Equal(mod1) {
		t.Fatalf("fixture recreate did not restore (size, mtime): got (%d, %v), want (%d, %v)",
			st3.Size(), st3.ModTime(), size1, mod1)
	}

	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != third {
		t.Errorf("SID = %q, want %q — a stat error after a vanish-during-read Load left an earlier Load's "+
			"memo (and its by-now-unrelated pair) able to short-circuit a later Load", got, third)
	}
}

// TestLoadDeclinesTheMemoAfterALaterInstall pins the `j.loadGen == gen`
// clause in Load's post-read block — the SECOND, independent race the memo
// has to survive, distinct from the pre/post-stat agreement one.
//
// Two concurrent Loads (or a Load racing a loadFrom) on the same jar can
// install in one order and reach the post-read block in the other:
// internal/youtube/auth.go calls SyncCookies, and so Load, once per
// extraction, while the refresh pass loads from its own goroutine. If G1
// installs first and G2 installs second, G1's OWN pre- and post-read stats
// still agree — G1's read really was internally consistent — so without the
// generation check G1 would memoise a (size, mtime) pair that now vouches for
// bytes G2 installed, not the bytes G1 read. Every later Load would then
// short-circuit on it.
//
// cookieJarAfterParse is the seam. It fires between THIS call's parseInto and
// its post-read stat, which is precisely where the losing install has to land
// and precisely where cookieJarReadFile (which fires BEFORE parseInto) cannot
// reach — the gap that made this clause "not deterministically testable" in
// fix round 2. The outer Load plays G1; the seam body plays G2 by installing
// straight through parseInto, exactly as a concurrent loadFrom would.
//
// Mutant named: dropping `j.loadGen == gen` from Load's post-read memo
// computation. Verified by execution — with that clause removed the outer
// Load memoises (preOK holds, the stats agree, the hour-old mtime clears the
// settle window, so every OTHER clause is true by construction), the second
// Load short-circuits, and the jar keeps serving G2's bytes instead of the
// file's. Passes with the clause present.
func TestLoadDeclinesTheMemoAfterALaterInstall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	const fromFile = "AAAAAAAA"    // what the outer Load's own read installs
	const fromLaterG2 = "BBBBBBBB" // what the install landing after it replaces that with
	agedCookieFile(t, path, []string{cookieRow(".youtube.com", futureExpiry(), "SID", fromFile)}, time.Hour)

	jar := NewCookieJar()

	orig := cookieJarAfterParse
	t.Cleanup(func() { cookieJarAfterParse = orig })
	cookieJarAfterParse = func() {
		// G2 installs AFTER G1's parseInto returned, so loadGen moves past
		// the value G1 is holding. Deliberately NOT from the file: the point
		// is that the jar's maps no longer hold what G1 read, while the file
		// on disk is untouched and its (size, mtime) still agree across G1's
		// two stats.
		stale := "# Netscape HTTP Cookie File\n" +
			cookieRow(".youtube.com", futureExpiry(), "SID", fromLaterG2) + "\n"
		jar.parseInto([]byte(stale), path)
	}

	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	cookieJarAfterParse = orig // restore before the real second Load

	// Direct observation of the memo, not an inference from behaviour: the
	// clause under test IS this boolean.
	jar.mu.RLock()
	memoised := jar.loadedMemo
	jar.mu.RUnlock()
	if memoised {
		t.Errorf("loadedMemo = true — Load memoised a (size, mtime) pair that now vouches for bytes a LATER install put in the maps, not the bytes this Load read")
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != fromLaterG2 {
		t.Fatalf("setup: SID = %q after the later install, want %q", got, fromLaterG2)
	}

	// The behavioural half: because nothing was memoised, the next Load must
	// go back to the file. Under the mutant it short-circuits and fromLaterG2
	// survives forever.
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != fromFile {
		t.Errorf("SID = %q, want %q — a memo recorded across a later install let the next Load short-circuit on a file it never re-read",
			got, fromFile)
	}
}

// memoIsSet reports the jar's memo flag. In-package on purpose: the invariant
// this file's last two tests pin is about a FIELD, and asserting it through a
// behavioural proxy alone would leave the next reader guessing which of the
// two halves broke.
func memoIsSet(j *CookieJar) bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.loadedMemo
}

// TestLoadNeverMemoisesBesideALoadError is fix round 1's Important 1.
//
// The memo's invariant is "memo true implies the sentinel is empty", and it
// was not enforced. A CONCURRENT Load whose read fails does not bump loadGen —
// the error arm installs no maps, so it deliberately does not — so a failing
// read landing between a successful Load's parseInto and its post-read stat
// left the successful call free to memoise over a sentinel the failing call
// had just written. From then on EVERY Load short-circuits at the pre-read
// memo check and returns before anything can clear lastLoadErr: both
// dashboards stay red on a perfectly readable file until the file's
// (size, mtime) changes — never, for a hand-maintained cookies.txt.
//
// Reachable in production without any exotic timing: the refresh pass calls
// Load from its own goroutine while every extraction's SyncCookies calls it
// too, and one transient EMFILE/EIO/CIFS hiccup on either is enough.
//
// The interleaving is landed through cookieJarAfterParse, the seam that exists
// for exactly this window — cookieJarReadFile cannot reach it, because that
// seam fires BEFORE parseInto rather than between parseInto and the stat.
//
// Mutants:
//   - drop the `j.lastLoadErr == ""` conjunct from the memo assignment -> the
//     memo latches beside the sentinel and the verification Load below never
//     reads the file, so LastLoadError() stays set forever.
//   - clear lastLoadErr somewhere other than an install (say, in Load's
//     short-circuit) -> the first assertion still passes and the second stops
//     meaning anything; the invariant is what is asserted, not the symptom.
func TestLoadNeverMemoisesBesideALoadError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	agedCookieFile(t, path, []string{cookieRow(".youtube.com", futureExpiry(), "SID", "from-the-file")}, time.Hour)

	jar := NewCookieJar()

	origHook := cookieJarAfterParse
	t.Cleanup(func() { cookieJarAfterParse = origHook })
	var once sync.Once
	cookieJarAfterParse = func() {
		// ONCE: only the first Load stages the race. The verification Load
		// below must run against an untouched jar and a readable file.
		once.Do(func() {
			// The concurrent goroutine, landed synchronously in its window.
			// Its own Load takes the error arm, which never reaches this
			// seam, so there is no re-entry to guard against.
			realRead := cookieJarReadFile
			cookieJarReadFile = func(string) ([]byte, error) {
				return nil, &fs.PathError{Op: "open", Path: path, Err: syscall.EACCES}
			}
			if err := jar.Load(path); err == nil {
				t.Error("premise broken: the injected concurrent Load was supposed to fail")
			}
			cookieJarReadFile = realRead
		})
	}

	if err := jar.Load(path); err != nil {
		t.Fatalf("the outer Load reads a perfectly good file: %v", err)
	}

	// THE INVARIANT. The successful call's own read really was internally
	// consistent, so every other memo clause holds; this is the one that must
	// not.
	if memoIsSet(jar) && jar.LastLoadError() != "" {
		t.Errorf("loadedMemo is set while LastLoadError() = %q — every later Load now "+
			"short-circuits before anything can clear the sentinel", jar.LastLoadError())
	}

	// And the consequence, end to end: one ordinary Load of a readable file
	// takes the badge back down.
	if err := jar.Load(path); err != nil {
		t.Fatalf("the verification Load: %v", err)
	}
	if got := jar.LastLoadError(); got != "" {
		t.Errorf("LastLoadError() = %q after a Load of a READABLE file — the memo short-circuit "+
			"returned before anything could clear it, so both dashboards stay red until restart", got)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != "from-the-file" {
		t.Errorf("SID = %q, want %q — the jar must hold what the file says", got, "from-the-file")
	}
}

// TestFailedLoadClearsTheMemoItNoLongerDescribes is fix round 1's Minor 2: the
// `j.loadedMemo = false` on Load's error arm was an unpinned behaviour change.
//
// The arm records filePath for a read that FAILED, which is what COOKIES-2
// needs — but it therefore moves filePath out from under a (loadedSize,
// loadedMod) pair that describes a DIFFERENT file. Leave the memo standing and
// a later Load of the new path whose (size, mtime) coincide with the old pair
// short-circuits on bytes it never read, and the jar serves the previous
// file's credentials forever.
//
// The coincidence is staged exactly rather than hoped for: the second file is
// written to the same byte length and its mtime forced to the first file's.
//
// Mutant:
//   - drop `j.loadedMemo = false` from the error arm -> the final Load
//     short-circuits and the jar still holds the FIRST file's SID.
func TestFailedLoadClearsTheMemoItNoLongerDescribes(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.txt")
	second := filepath.Join(dir, "second.txt")
	const firstValue = "AAAAAAAA"
	const secondValue = "BBBBBBBB" // same length: the (size, mtime) pair must be forgeable
	if len(firstValue) != len(secondValue) {
		t.Fatalf("fixture values must be equal length")
	}

	agedCookieFile(t, first, []string{cookieRow(".youtube.com", futureExpiry(), "SID", firstValue)}, time.Hour)
	jar := NewCookieJar()
	if err := jar.Load(first); err != nil {
		t.Fatal(err)
	}
	if !memoIsSet(jar) {
		t.Fatal("setup: the first Load did not memoise, so there is nothing for the error arm to clear")
	}
	st1 := statOf(t, first)

	// The second file, made indistinguishable from the first by the only two
	// facts the memo compares.
	agedCookieFile(t, second, []string{cookieRow(".youtube.com", futureExpiry(), "SID", secondValue)}, time.Hour)
	if err := os.Chtimes(second, st1.ModTime(), st1.ModTime()); err != nil {
		t.Fatal(err)
	}
	if st2 := statOf(t, second); st2.Size() != st1.Size() || !st2.ModTime().Equal(st1.ModTime()) {
		t.Fatalf("fixture did not forge (size, mtime): got (%d, %v), want (%d, %v)",
			st2.Size(), st2.ModTime(), st1.Size(), st1.ModTime())
	}

	// A failed read of the SECOND path: filePath moves, the pair does not.
	realRead := cookieJarReadFile
	cookieJarReadFile = func(string) ([]byte, error) {
		return nil, &fs.PathError{Op: "open", Path: second, Err: syscall.EACCES}
	}
	if err := jar.Load(second); err == nil {
		t.Fatal("premise broken: the injected read was supposed to fail")
	}
	cookieJarReadFile = realRead

	if err := jar.Load(second); err != nil {
		t.Fatal(err)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != secondValue {
		t.Errorf("SID = %q, want %q — the Load short-circuited on a memo that describes a file "+
			"this jar is no longer pointed at", got, secondValue)
	}
}
