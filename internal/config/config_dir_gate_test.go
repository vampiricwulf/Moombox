package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// configDirFixture returns a real, EXISTING directory holding exactly the named
// entries (a name ending in "/" becomes a subdirectory, anything else a file).
// Existing, not merely named: utils.DirTighteningAllowed reads the contents,
// and a directory it cannot list is treated as shared data.
//
// Each test uses its OWN directory because Save's dacledDirs memo is
// process-lifetime and per-directory — a second Save into a directory an
// earlier test already marked would skip the whole block being tested.
func configDirFixture(t *testing.T, entries ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "confdir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		p := filepath.Join(dir, filepath.FromSlash(e))
		if strings.HasSuffix(e, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// dirGateRecorder records what a substituted seam was handed.
type dirGateRecorder struct {
	mu   sync.Mutex
	dirs []string
}

func (g *dirGateRecorder) record(dir string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.dirs = append(g.dirs, dir)
}

func (g *dirGateRecorder) seen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.dirs...)
}

// fakeDirTighteningAllowed / fakeApplyUserOnlyDACL swap the two seams Save goes
// through, restoring the real functions via t.Cleanup. Mirrors the pair
// internal/cookies already uses. The gate seam is the only way to drive the
// refusal arm on Windows, where the real predicate answers true for every
// directory (O-K leaves icacls unchanged); the apply seam keeps the assertion
// on "was the tightening actually skipped" rather than on a real icacls
// shell-out or a chmod this host cannot observe.
func fakeDirTighteningAllowed(t *testing.T, fn func(dir string) bool) {
	t.Helper()
	real := dirTighteningAllowed
	t.Cleanup(func() { dirTighteningAllowed = real })
	dirTighteningAllowed = fn
}

func fakeApplyUserOnlyDACL(t *testing.T, fn func(dir string) error) {
	t.Helper()
	real := applyUserOnlyDACL
	t.Cleanup(func() { applyUserOnlyDACL = real })
	applyUserOnlyDACL = fn
}

// TestSaveRefusesToTightenADirectoryTheGateDenies is config.Save's half of
// owner decision O-K. A settings save is the gesture most likely to be the
// FIRST write on a new container, and config.toml sits at /data/config.toml in
// the image — the same /data the cookie writer used to chmod.
//
// Mutants:
//   - gate only the cookie writer and leave Save's twin applying unconditionally
//     (R3's mutant) -> the gate is never consulted here and the apply still
//     runs; both assertions fail.
//   - consult the gate but apply anyway -> the apply assertion fails.
func TestSaveRefusesToTightenADirectoryTheGateDenies(t *testing.T) {
	dir := configDirFixture(t, "output/", "moombox.db", "moombox.log")
	var applies, gate dirGateRecorder
	fakeApplyUserOnlyDACL(t, func(d string) error { applies.record(d); return nil })
	fakeDirTighteningAllowed(t, func(d string) bool { gate.record(d); return false })

	if err := Save(Defaults(), filepath.Join(dir, "config.toml")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if seen := gate.seen(); len(seen) != 1 || seen[0] != dir {
		t.Fatalf("gate consulted with %v, want exactly [%q] — the config file's own parent", seen, dir)
	}
	if seen := applies.seen(); len(seen) != 0 {
		t.Errorf("the DACL/chmod apply ran on %v, want no apply at all — the gate refused this directory", seen)
	}
}

// TestSaveTightensADedicatedConfigDir is the other half: a directory holding
// nothing but the config file (and, in the usual layout, cookies.txt beside it)
// still earns the hardening — the password hash lives in there.
//
// Mutant: invert the gate -> the apply never runs and every dedicated config
// directory loses its tightening.
func TestSaveTightensADedicatedConfigDir(t *testing.T) {
	dir := configDirFixture(t, "cookies.txt")
	var applies dirGateRecorder
	fakeApplyUserOnlyDACL(t, func(d string) error { applies.record(d); return nil })
	fakeDirTighteningAllowed(t, func(d string) bool { return true })

	if err := Save(Defaults(), filepath.Join(dir, "config.toml")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if seen := applies.seen(); len(seen) != 1 || seen[0] != dir {
		t.Errorf("apply ran on %v, want exactly [%q]", seen, dir)
	}
}

// TestConfigDirGateIsTheSharedPredicate pins R3 from this side: Save's seam and
// the cookie writer's seam are two references to one rule, not two opinions.
//
// The CONTENT verdict is asserted per fixture as well, and that half is what
// makes this test mean anything on Windows: utils.DirTighteningAllowed answers
// true for every directory there (O-K leaves icacls unchanged), so the
// equivalence above compares true against true on the Windows leg and a seam
// pointed at a local predicate would sail through it.
//
// Mutants:
//   - point the seam at a local predicate, or at DirHoldsSharedData without the
//     negation -> the two answers diverge on one fixture (POSIX only).
//   - break the content rule itself (drop the directory-name check, stop
//     stripping SQLite's -wal suffix) -> the wantShared column fails, on BOTH
//     legs.
func TestConfigDirGateIsTheSharedPredicate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dir        string
		wantShared bool
	}{
		{"the data directory", configDirFixture(t, "config.toml", "output/", "staging/", "moombox.db"), true},
		{"config.toml beside cookies.txt", configDirFixture(t, "config.toml", "cookies.txt"), false},
		{"empty", configDirFixture(t), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, want := dirTighteningAllowed(tc.dir), utils.DirTighteningAllowed(tc.dir); got != want {
				t.Errorf("dirTighteningAllowed(%q) = %v, utils.DirTighteningAllowed = %v — both call sites must consult one rule", tc.dir, got, want)
			}
			if got := utils.DirHoldsSharedData(tc.dir); got != tc.wantShared {
				t.Errorf("utils.DirHoldsSharedData(%q) = %v, want %v — the CONTENT rule is what O-K is about, "+
					"and it is the half the OS-conditional predicate above cannot pin on Windows", tc.dir, got, tc.wantShared)
			}
		})
	}
}

// TestSaveLeavesTheDataDirectoryAloneOnPOSIX drives the REAL predicate over the
// Docker image's /data shape. Differential by design: Windows icacls is
// unchanged by O-K, POSIX must leave the operator's data volume traversable.
//
// Mutant: drop the gate from Save (or make DirTighteningAllowed content-tested
// on Windows too) -> the POSIX leg sees one apply where it wants none, or the
// Windows leg sees none where it wants one. A settings save is the gesture most
// likely to be the FIRST one on a new container, so this is the path that used
// to undo `chmod 777 /data` before any cookie was ever written.
func TestSaveLeavesTheDataDirectoryAloneOnPOSIX(t *testing.T) {
	dir := configDirFixture(t, "cookies.txt", "output/", "staging/", "moombox.db", "moombox.log")
	var applies dirGateRecorder
	fakeApplyUserOnlyDACL(t, func(d string) error { applies.record(d); return nil })

	if err := Save(Defaults(), filepath.Join(dir, "config.toml")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	want := 0
	if runtime.GOOS == "windows" {
		want = 1
	}
	if got := len(applies.seen()); got != want {
		t.Errorf("apply calls = %d on %s, want %d — Windows icacls unchanged; POSIX must not 0700 the data directory",
			got, runtime.GOOS, want)
	}
}
