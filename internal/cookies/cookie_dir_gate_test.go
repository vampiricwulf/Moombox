package cookies

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// cookieDirFixture returns a real, EXISTING directory holding exactly the named
// entries (a name ending in "/" becomes a subdirectory, anything else a file) —
// the shape a cookie file's parent really has when writeFileAtomic calls
// tightenCookieDirOnce on it.
//
// Existing, not merely named, on purpose: utils.DirTighteningAllowed reads the
// directory's CONTENTS, and a directory it cannot list is treated as shared
// data (the fail-safe arm), so a fixture that is only a path would be refused
// the tightening on POSIX and never reach the apply seam at all.
func cookieDirFixture(t *testing.T, entries ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cookiedir")
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

// fakeDirTighteningAllowed swaps the dirTighteningAllowed seam for the duration
// of a test, restoring the real utils.DirTighteningAllowed via t.Cleanup. Same
// shape, and the same reason, as fakeApplyUserOnlyDACL: the real predicate
// answers true for EVERY directory on Windows (O-K leaves icacls alone), so on
// this host nothing but a substituted verdict can drive the refusal arm.
func fakeDirTighteningAllowed(t *testing.T, fn func(dir string) bool) {
	t.Helper()
	real := dirTighteningAllowed
	t.Cleanup(func() { dirTighteningAllowed = real })
	dirTighteningAllowed = fn
}

// gateRecorder records the directories a substituted gate was asked about.
type gateRecorder struct {
	mu   sync.Mutex
	dirs []string
}

func (g *gateRecorder) record(dir string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.dirs = append(g.dirs, dir)
}

func (g *gateRecorder) seen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.dirs...)
}

// TestTightenCookieDirOnceRefusesADirectoryTheGateDenies is the cookie half of
// owner decision O-K: the parent chmod is gated, and the gate's NO is final.
//
// Mutants:
//   - drop the gate from tightenCookieDirOnce (the shipped behaviour) -> the
//     gate is never consulted and the apply fires anyway; both assertions fail,
//     and in the image the first cookie write chmods /data 0700 again.
//   - move the check inside the goroutine, after the memo claim -> the memo
//     assertion fails: a refused directory is recorded in flight and, because
//     nothing ever resolves it, every later write for that directory is deduped
//     against an apply that will never run.
func TestTightenCookieDirOnceRefusesADirectoryTheGateDenies(t *testing.T) {
	dir := cookieDirFixture(t, "cookies.txt", "output/", "moombox.db")
	applied := make(chan struct{}, 4)
	fakeApplyUserOnlyDACL(t, func(d string) error {
		applied <- struct{}{}
		return nil
	})
	var rec gateRecorder
	fakeDirTighteningAllowed(t, func(d string) bool {
		rec.record(d)
		return false
	})

	tightenCookieDirOnce(dir)

	if seen := rec.seen(); len(seen) != 1 || seen[0] != dir {
		t.Fatalf("gate consulted with %v, want exactly [%q] — the cookie file's own parent", seen, dir)
	}
	select {
	case <-applied:
		t.Fatal("the DACL/chmod apply ran on a directory the gate refused")
	case <-time.After(100 * time.Millisecond):
	}
	tightenedCookieDirsMu.Lock()
	_, present := tightenedCookieDirs[dir]
	tightenedCookieDirsMu.Unlock()
	if present {
		t.Error("a refused directory was recorded in the memo — the gate is checked BEFORE the claim so nothing is left in flight")
	}
}

// TestTightenCookieDirOnceAsksTheGateBeforeClaimingTheMemo pins the ORDER,
// deterministically: it holds the gate open and looks at the memo while the
// verdict is still pending.
//
// The order is load-bearing. The memo is claimed synchronously, before the
// apply goroutine is spawned, so a gate consulted after the claim would record
// a refused directory as in-flight; the goroutine's deferred cleanup only
// removes the entry because it happens to return early, and any arrangement
// that leaves it behind deduplicates every later write for that directory
// against an apply that will never run.
//
// Mutant: move the check inside the goroutine, or below the claim -> the memo
// already holds an entry while the gate is still deciding, and this fails
// whichever goroutine wins.
func TestTightenCookieDirOnceAsksTheGateBeforeClaimingTheMemo(t *testing.T) {
	dir := cookieDirFixture(t, "cookies.txt", "output/")
	fakeApplyUserOnlyDACL(t, func(d string) error { return nil })
	entered := make(chan struct{})
	release := make(chan struct{})
	fakeDirTighteningAllowed(t, func(d string) bool {
		close(entered)
		<-release
		return false
	})

	returned := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("tightenCookieDirOnce panicked: %v", r)
			}
			close(returned)
		}()
		tightenCookieDirOnce(dir)
	}()

	awaitSignal(t, entered, "the gate being consulted")
	tightenedCookieDirsMu.Lock()
	_, present := tightenedCookieDirs[dir]
	tightenedCookieDirsMu.Unlock()
	close(release)
	awaitSignal(t, returned, "tightenCookieDirOnce returning")

	if present {
		t.Error("the memo was claimed before the gate answered — a refused directory must never be recorded as in flight")
	}
}

// TestTightenCookieDirOnceAppliesWhenTheGateAllows is the other half: the gate
// narrows the tightening, it does not retire it.
//
// Mutant: invert the gate (return early when it answers true) -> no apply ever
// runs and the dedicated secrets directory silently loses its hardening.
func TestTightenCookieDirOnceAppliesWhenTheGateAllows(t *testing.T) {
	dir := cookieDirFixture(t, "cookies.txt")
	applied := make(chan string, 4)
	fakeApplyUserOnlyDACL(t, func(d string) error {
		applied <- d
		return nil
	})
	fakeDirTighteningAllowed(t, func(d string) bool { return true })

	tightenCookieDirOnce(dir)

	select {
	case got := <-applied:
		if got != dir {
			t.Errorf("apply ran on %q, want %q", got, dir)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the DACL/chmod apply never ran for a directory the gate allowed")
	}
	awaitSettled(t, dir)
	tightenedCookieDirsMu.Lock()
	state := tightenedCookieDirs[dir]
	tightenedCookieDirsMu.Unlock()
	if state != dirTighteningDone {
		t.Errorf("memo state = %v, want dirTighteningDone", state)
	}
}

// TestCookieDirGateIsTheSharedPredicate pins R3: the cookie writer and
// config.Save consult the SAME rule. The seam exists for the tests above, not
// as a second opinion — so its production value has to answer exactly what
// utils.DirTighteningAllowed answers.
//
// The CONTENT verdict is asserted per fixture as well, and that half is what
// makes this test mean anything on Windows: utils.DirTighteningAllowed answers
// true for every directory there (O-K leaves icacls unchanged), so the
// equivalence compares true against true on the Windows leg and a seam pointed
// at a local predicate would sail through it.
//
// Mutants:
//   - point the seam at a local predicate (or at DirHoldsSharedData without
//     the negation) -> the answers diverge on one fixture (POSIX only).
//   - break the content rule itself -> the wantShared column fails, on BOTH
//     legs.
func TestCookieDirGateIsTheSharedPredicate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dir        string
		wantShared bool
	}{
		{"the data directory", cookieDirFixture(t, "cookies.txt", "output/", "moombox.db", "moombox.log"), true},
		{"cookies.txt alone", cookieDirFixture(t, "cookies.txt"), false},
		{"empty", cookieDirFixture(t), false},
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

// TestTightenCookieDirOnceLeavesTheDataDirectoryAloneOnPOSIX drives the REAL
// predicate over the Docker image's /data shape — cookies.txt beside output/,
// staging/, the database and the log. It is deliberately differential: on
// Windows icacls is unchanged by O-K and the apply must still run; on POSIX the
// 0700 would take the operator's archives away from their own host user, so it
// must not.
//
// Mutant: make DirTighteningAllowed content-tested on Windows too -> the
// Windows leg fails here as well as in internal/utils.
func TestTightenCookieDirOnceLeavesTheDataDirectoryAloneOnPOSIX(t *testing.T) {
	dir := cookieDirFixture(t, "cookies.txt", "output/", "staging/", "moombox.db", "moombox.log")
	var calls int32
	applied := make(chan struct{}, 4)
	fakeApplyUserOnlyDACL(t, func(d string) error {
		atomic.AddInt32(&calls, 1)
		applied <- struct{}{}
		return nil
	})

	tightenCookieDirOnce(dir)

	if runtime.GOOS == "windows" {
		select {
		case <-applied:
		case <-time.After(5 * time.Second):
			t.Fatal("Windows lost its cookie-directory hardening — O-K leaves icacls unchanged")
		}
		awaitSettled(t, dir)
		return
	}
	select {
	case <-applied:
		t.Fatal("POSIX chmodded the data directory 0700 — that is the container bug O-K forbids")
	case <-time.After(100 * time.Millisecond):
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("apply calls = %d, want 0", got)
	}
}
