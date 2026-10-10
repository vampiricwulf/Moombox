package cookies

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This file pins the SingletonLock liveness rule (D-X3): a symlink lock is
// judged by the "<hostname>-<pid>" its target names, never by an mtime its
// target does not have. Nothing here launches a browser — every launch site
// it reaches is handed a path that does not exist, and the assertions are
// about what happens BEFORE that path would be executed.

// foreignLockHost is a hostname no test machine has: ".invalid" is reserved
// (RFC 2606), so os.Hostname can never return it.
const foreignLockHost = "moombox-other-host.invalid"

// symlinkLock writes Chromium's POSIX SingletonLock shape — a symlink whose
// target is a NAME, never a file — or skips where this account cannot create
// symlinks (Windows without Developer Mode). The rule is POSIX-only in
// practice; Windows never writes one.
func symlinkLock(t *testing.T, target, path string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("cannot create a symlink here (%v) — the SingletonLock rule needs one", err)
	}
}

// stubLockHost pins what lockHostname answers for one test.
func stubLockHost(t *testing.T, host string, err error) {
	t.Helper()
	prev := lockHostname
	lockHostname = func() (string, error) { return host, err }
	t.Cleanup(func() { lockHostname = prev })
}

// stubPIDRunning pins what lockPIDRunning answers for one test, and records
// whether it was asked at all.
func stubPIDRunning(t *testing.T, running bool) *bool {
	t.Helper()
	prev := lockPIDRunning
	asked := false
	lockPIDRunning = func(int) bool { asked = true; return running }
	t.Cleanup(func() { lockPIDRunning = prev })
	return &asked
}

// lockSurvives reports whether path still exists AS a link or file, without
// following it — os.Stat on a SingletonLock always fails, which is the very
// confusion this rule replaces.
func lockSurvives(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// TestParseSingletonLockTarget pins the split. Chromium writes
// "<hostname>-<pid>"; hostnames carry hyphens; SingletonCookie's target is a
// bare number and SingletonSocket's a socket path, and neither may parse.
//
// Mutants: split at the FIRST hyphen (strings.IndexByte) — the hyphenated
// host row fails; ParseInt bitSize 64 — the over-int32 row parses; drop
// `n <= 0` — the pid-0 row parses (and kill(0, 0) would ask about Moombox's
// own process group); `i <= 0` → `i < 0` — the empty-host row parses.
func TestParseSingletonLockTarget(t *testing.T) {
	for _, tc := range []struct {
		target   string
		wantHost string
		wantPID  int
		wantOK   bool
	}{
		{"desktop-1234", "desktop", 1234, true},
		{"my-desk-top-42", "my-desk-top", 42, true},
		{"a1b2c3d4e5f6-7", "a1b2c3d4e5f6", 7, true}, // a container id as hostname
		{"1234567890123", "", 0, false},             // SingletonCookie
		{"/tmp/.org.chromium.Chromium.Ab12Cd/SingletonSocket", "", 0, false},
		{"127.0.1.1:+12345", "", 0, false}, // Firefox's `lock`
		{"desktop-", "", 0, false},
		{"-1234", "", 0, false},
		{"desktop-0", "", 0, false},
		{"desktop-4294967297", "", 0, false}, // 2^32+1: kill(2) would truncate it to pid 1
		{"desktop-12ab", "", 0, false},
	} {
		host, pid, ok := parseSingletonLockTarget(tc.target)
		if ok != tc.wantOK || host != tc.wantHost || pid != tc.wantPID {
			t.Errorf("parseSingletonLockTarget(%q) = (%q, %d, %v), want (%q, %d, %v)",
				tc.target, host, pid, ok, tc.wantHost, tc.wantPID, tc.wantOK)
		}
	}
}

// TestSingletonLockHolderDeletesOnlyAnOrphanOnThisMachine is the decision
// table. Exactly one row may answer nil: this machine's hostname with a pid
// that no longer answers. Another host is never deleted whatever its pid
// says — that pid is not signallable from here, which is why the row with a
// "dead" stub must still refuse — and a pid that answers is never deleted
// even when it may have been reused.
//
// Every refusal names the lock's full path and says when deleting it is safe:
// the operator is the only one who can find out that the browser is gone.
//
// Mutants: `host != local` → `host == local` (the foreign rows answer nil
// and the dead-local row refuses); delete the foreign case (the foreign/dead
// row deletes); delete the lockPIDRunning case (the live-local row deletes);
// delete the hostname-error case (that row falls into the foreign one and
// fails on the sentence, which must say why it could not tell); drop the
// delete clause from any one refusal (that row's path words fail).
func TestSingletonLockHolderDeletesOnlyAnOrphanOnThisMachine(t *testing.T) {
	const local = "desktop"
	const lockPath = "/srv/moombox/browser-profile/SingletonLock"
	deleteIt := "delete " + strconv.Quote(lockPath) + " if "
	for _, tc := range []struct {
		name      string
		lockHost  string
		hostErr   error
		running   bool
		wantInUse bool
		wantWords []string
	}{
		{"this machine, dead pid", local, nil, false, false, nil},
		{"this machine, live pid", local, nil, true, true, []string{"in use by desktop", "pid 4242 is still running",
			deleteIt + "that pid is no longer one"}},
		{"another machine, pid dead HERE", foreignLockHost, nil, false, true, []string{"in use by " + foreignLockHost, "pid 4242 there",
			deleteIt + "no browser on " + foreignLockHost + " is using that profile"}},
		{"another machine, pid live HERE", foreignLockHost, nil, true, true, []string{"in use by " + foreignLockHost,
			deleteIt + "no browser on " + foreignLockHost + " is using that profile"}},
		{"hostname unreadable", local, errors.New("uname failed"), false, true, []string{"in use by desktop", "hostname could not be read",
			deleteIt + "no browser on desktop is using that profile"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubLockHost(t, local, tc.hostErr)
			stubPIDRunning(t, tc.running)

			err := singletonLockHolder(lockPath, tc.lockHost, 4242)
			if gotInUse := errors.Is(err, ErrProfileInUse); gotInUse != tc.wantInUse || (err != nil) != tc.wantInUse {
				t.Fatalf("singletonLockHolder(%q, %q, 4242) = %v, want in-use %v", lockPath, tc.lockHost, err, tc.wantInUse)
			}
			for _, w := range tc.wantWords {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not say %q — the sentence is what both UIs show as the last cookie error", err, w)
				}
			}
		})
	}
}

// TestRemoveStaleLockKeepsASymlinkLockAForeignBrowserHolds is the defect
// itself, on a real symlink. Before this rule, os.Stat followed the link to a
// target that never exists, read the failure as "not present", and unlinked
// the lock — under a live browser, every time.
//
// Mutant: the pre-D-X3 removeStaleLock (no symlink branch) — the lock is
// unlinked and nil comes back, failing both assertions.
func TestRemoveStaleLockKeepsASymlinkLockAForeignBrowserHolds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "SingletonLock")
	symlinkLock(t, foreignLockHost+"-4242", path)

	err := removeStaleLock(path)

	if !errors.Is(err, ErrProfileInUse) {
		t.Errorf("removeStaleLock on another machine's lock = %v, want ErrProfileInUse", err)
	}
	if !lockSurvives(path) {
		t.Error("another machine's SingletonLock was unlinked — that browser may be running, and nothing here can tell")
	}
}

// TestRemoveStaleLockUnlinksAnOrphanedLocalSymlinkLock: the one deleting row,
// on a real symlink — this machine's hostname, a pid that no longer answers.
// Without it, the rule above could be "never delete a symlink" and pass.
//
// Mutant: return before the os.Remove in the symlink branch — the orphan
// survives.
func TestRemoveStaleLockUnlinksAnOrphanedLocalSymlinkLock(t *testing.T) {
	stubLockHost(t, "desktop", nil)
	asked := stubPIDRunning(t, false)
	path := filepath.Join(t.TempDir(), "SingletonLock")
	symlinkLock(t, "desktop-4242", path)

	if err := removeStaleLock(path); err != nil {
		t.Fatalf("removeStaleLock on an orphaned local lock = %v, want nil", err)
	}
	if !*asked {
		t.Error("the pid was never asked about — the lock went without its holder being checked")
	}
	if lockSurvives(path) {
		t.Error("an orphaned local SingletonLock survived; the next launch would refuse the profile for nothing")
	}
}

// TestRemoveStaleLockAgesASymlinkThatNamesNoHolder: SingletonCookie's target
// is a bare number. It names no holder, so it keeps the age rule — which, on
// a target that does not exist, unlinks it as before. Only reached once
// SingletonLock has been judged (see TestCleanChromiumLockFilesStopsAtAHeldLock).
//
// Mutant: treat every symlink as a lock to keep (return nil when Lstat says
// symlink and the target does not parse) — the cookie survives.
func TestRemoveStaleLockAgesASymlinkThatNamesNoHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "SingletonCookie")
	symlinkLock(t, "1234567890123", path)

	if err := removeStaleLock(path); err != nil {
		t.Fatalf("removeStaleLock on SingletonCookie = %v, want nil", err)
	}
	if lockSurvives(path) {
		t.Error("a SingletonCookie link with no holder in it survived the age rule it has always had")
	}
}

// TestPidRunning pins the platform half against real processes: this test's
// own pid answers, and a child that has exited and been reaped does not.
//
// Mutant: invert the ESRCH test in lockholder_unix.go — both rows fail.
// The dead row is POSIX-only: it is the only platform the rule is reachable
// on, and Windows recycles pids fast enough to make it a coin toss there.
func TestPidRunning(t *testing.T) {
	if !pidRunning(os.Getpid()) {
		t.Errorf("pidRunning(own pid %d) = false — a live browser's lock would be deleted", os.Getpid())
	}
	if runtime.GOOS == "windows" {
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run a child to obtain a dead pid: %v", err)
	}
	if pid := cmd.Process.Pid; pidRunning(pid) {
		t.Errorf("pidRunning(%d) = true for a child that has exited and been reaped", pid)
	}
}

// TestCleanChromiumLockFilesStopsAtAHeldLock: a held SingletonLock stops the
// sweep before ANY sibling is touched. On Linux SingletonSocket and
// SingletonCookie are symlinks too, naming no holder; the age rule would
// unlink them under the very browser the lock has just said is running.
//
// Mutants: move "SingletonLock" after "SingletonSocket" in chromiumLockFiles
// — the socket link is gone before the lock is judged; `continue` instead of
// `return err` in the canonical loop — every sibling is unlinked and nil
// comes back.
func TestCleanChromiumLockFilesStopsAtAHeldLock(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "SingletonLock")
	symlinkLock(t, foreignLockHost+"-4242", lock)

	// A live browser's socket: a real, OLD file the link points at, so the
	// age rule — were it reached — would unlink the link.
	socketTarget := filepath.Join(t.TempDir(), "SingletonSocket")
	if err := os.WriteFile(socketTarget, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ageFile(t, socketTarget, time.Hour)
	socket := filepath.Join(dir, "SingletonSocket")
	symlinkLock(t, socketTarget, socket)
	cookie := filepath.Join(dir, "SingletonCookie")
	symlinkLock(t, "1234567890123", cookie)

	err := cleanChromiumLockFiles(dir)

	if !errors.Is(err, ErrProfileInUse) {
		t.Fatalf("cleanChromiumLockFiles = %v, want ErrProfileInUse", err)
	}
	for _, p := range []string{lock, socket, cookie} {
		if !lockSurvives(p) {
			t.Errorf("%s was unlinked while the profile's SingletonLock says another machine's browser holds it", filepath.Base(p))
		}
	}
}

// TestCleanChromiumLockFilesStopsAtAHeldGlobMatch: the glob loop answers the
// same way the canonical loop does, for a holder-naming link only it reaches.
//
// Its sentence names the variant — the file actually in the way — not the
// canonical SingletonLock the operator would otherwise go looking for.
//
// Mutant: discard removeStaleLock's error in the glob loop — nil comes back
// and the launch site would start a browser on a held profile.
func TestCleanChromiumLockFilesStopsAtAHeldGlobMatch(t *testing.T) {
	dir := t.TempDir()
	variant := filepath.Join(dir, "SingletonLock.lock")
	symlinkLock(t, foreignLockHost+"-4242", variant)

	err := cleanChromiumLockFiles(dir)
	if !errors.Is(err, ErrProfileInUse) {
		t.Fatalf("cleanChromiumLockFiles = %v, want ErrProfileInUse from the glob-matched lock", err)
	}
	if !strings.Contains(err.Error(), "delete "+strconv.Quote(variant)+" if ") {
		t.Errorf("the refusal %q does not name the glob-matched lock it stopped at", err)
	}
	if !lockSurvives(variant) {
		t.Error("the glob-matched lock was unlinked")
	}
}

// TestCleanChromiumLockFilesClearsAnOrphanedProfile is the other half: this
// machine's lock, its pid gone. Everything a crashed browser left goes, and
// the sweep says so with nil — so the rule above is not simply "refuse".
func TestCleanChromiumLockFilesClearsAnOrphanedProfile(t *testing.T) {
	stubLockHost(t, "desktop", nil)
	stubPIDRunning(t, false)
	dir := t.TempDir()
	lock := filepath.Join(dir, "SingletonLock")
	symlinkLock(t, "desktop-4242", lock)
	cookie := filepath.Join(dir, "SingletonCookie")
	symlinkLock(t, "1234567890123", cookie)

	if err := cleanChromiumLockFiles(dir); err != nil {
		t.Fatalf("cleanChromiumLockFiles on an orphaned profile = %v, want nil", err)
	}
	for _, p := range []string{lock, cookie} {
		if lockSurvives(p) {
			t.Errorf("%s survived on a profile whose browser is gone", filepath.Base(p))
		}
	}
}

// TestRefreshSkipsAProfileInUseAndSaysWhy is the item end to end, through the
// real RefreshCookiesDetailed and the real refreshChromium: a profile another
// machine's browser holds is a SKIP the status names, not a failure.
//
//   - err is ErrProfileInUse, and the lock is still there;
//   - Ran is false (declined: nothing was launched, nothing was written, so
//     no auth re-check is owed), while Mechanism says "browser" — that branch
//     WAS chosen;
//   - lastError — what GetStatus publishes, and what both UIs render as the
//     last cookie error — names the host.
//
// The browser path does not exist, so a regression that got past the lock
// would fail at exec rather than open a window.
//
// Mutants: delete the ErrProfileInUse arm in refreshCookiesDetailed — the
// pass falls to the abort below it and Ran comes back true; drop its
// setError — lastError stays empty; discard cleanChromiumLockFiles's error in
// refreshChromium — the pass reaches exec, fails "start headless browser",
// and err is not ErrProfileInUse.
func TestRefreshSkipsAProfileInUseAndSaysWhy(t *testing.T) {
	captureKills(t)
	profileDir := t.TempDir()
	lock := filepath.Join(profileDir, "SingletonLock")
	symlinkLock(t, foreignLockHost+"-4242", lock)

	cookiePath := ytAuthCookieFile(t)
	jar := NewCookieJar()
	if err := jar.Load(cookiePath); err != nil {
		t.Fatalf("load the fixture cookie file: %v", err)
	}
	s := NewAutoCookieService(profileDir, cookiePath, jar, nopAutoCookieLogger{})
	unlaunchable := filepath.Join(t.TempDir(), "not-a-browser")
	s.detectBrowser = func() *DetectedBrowser {
		return &DetectedBrowser{Type: "chrome", Path: unlaunchable, Name: "unlaunchable test browser"}
	}
	s.VerifyYouTubeAuth = func(context.Context) (bool, error) {
		t.Error("auth was verified on a pass that should have launched and read nothing")
		return true, nil
	}

	result, err := s.RefreshCookiesDetailed(context.Background())

	if !errors.Is(err, ErrProfileInUse) {
		t.Fatalf("RefreshCookiesDetailed = %v, want ErrProfileInUse", err)
	}
	if result.Ran {
		t.Error("Ran = true — a skipped pass wrote nothing, and Ran is what owes an auth re-check")
	}
	if result.Mechanism != RefreshMechanismBrowser {
		t.Errorf("Mechanism = %q, want %q — the browser branch was chosen, then declined", result.Mechanism, RefreshMechanismBrowser)
	}
	if got := lastErrorSnapshot(s); !strings.Contains(got, "in use by "+foreignLockHost) {
		t.Errorf("lastError = %q, want it to name %q — a skip that recurs every 30 minutes behind a blank status line is the silent failure lastError exists for", got, foreignLockHost)
	}
	if !lockSurvives(lock) {
		t.Error("the refresh unlinked another machine's SingletonLock")
	}
}

// TestChromiumSetupRefusesAProfileInUse: the interactive launch takes the same
// rule. A sign-in window on a held profile would be a second browser on it.
//
// Mutant: discard cleanChromiumLockFiles's error in startChromiumSetup — the
// setup reaches exec and fails "start browser" instead.
func TestChromiumSetupRefusesAProfileInUse(t *testing.T) {
	captureKills(t)
	profileDir := t.TempDir()
	lock := filepath.Join(profileDir, "SingletonLock")
	symlinkLock(t, foreignLockHost+"-4242", lock)

	s := NewAutoCookieService(profileDir, "", NewCookieJar(), nopAutoCookieLogger{})
	unlaunchable := filepath.Join(t.TempDir(), "not-a-browser")
	s.detectBrowser = func() *DetectedBrowser {
		return &DetectedBrowser{Type: "chrome", Path: unlaunchable, Name: "unlaunchable test browser"}
	}

	err := s.StartSetup("youtube")

	if !errors.Is(err, ErrProfileInUse) {
		t.Fatalf("StartSetup = %v, want ErrProfileInUse", err)
	}
	if !strings.Contains(err.Error(), foreignLockHost) {
		t.Errorf("StartSetup error %q does not name the host holding the profile", err)
	}
	if !lockSurvives(lock) {
		t.Error("setup unlinked another machine's SingletonLock")
	}
	if s.GetStatus().SetupInProgress {
		t.Error("a refused setup left the setup slot held")
	}
}

// TestARefusedSetupKeepsTheProfileInUseLine: StartSetup clears lastError at its
// slot claim, before the Chromium launcher has judged the lock, so a refusal
// that only RETURNED its sentence left the status both UIs read blank — after
// a refresh had just recorded "in use by <host>", and while every later pass
// would still skip for that reason. The refusal records what it found.
//
// The holder changes between the two steps, so the line the test reads must
// be the setup's own finding, not the refresh's left standing.
//
// Mutant: drop the setError in startChromiumSetup's refusal — lastError is
// empty after the refused setup.
func TestARefusedSetupKeepsTheProfileInUseLine(t *testing.T) {
	captureKills(t)
	profileDir := t.TempDir()
	lock := filepath.Join(profileDir, "SingletonLock")
	symlinkLock(t, foreignLockHost+"-4242", lock)

	cookiePath := ytAuthCookieFile(t)
	jar := NewCookieJar()
	if err := jar.Load(cookiePath); err != nil {
		t.Fatalf("load the fixture cookie file: %v", err)
	}
	s := NewAutoCookieService(profileDir, cookiePath, jar, nopAutoCookieLogger{})
	unlaunchable := filepath.Join(t.TempDir(), "not-a-browser")
	s.detectBrowser = func() *DetectedBrowser {
		return &DetectedBrowser{Type: "chrome", Path: unlaunchable, Name: "unlaunchable test browser"}
	}

	if _, err := s.RefreshCookiesDetailed(context.Background()); !errors.Is(err, ErrProfileInUse) {
		t.Fatalf("RefreshCookiesDetailed = %v, want ErrProfileInUse", err)
	}
	if got := lastErrorSnapshot(s); !strings.Contains(got, "in use by "+foreignLockHost) {
		t.Fatalf("precondition: the refresh recorded lastError = %q, want it to name %q", got, foreignLockHost)
	}

	// Another machine's browser holds it by the time the operator asks to
	// sign in.
	const nextHost = "moombox-third-host.invalid"
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	symlinkLock(t, nextHost+"-5151", lock)

	if err := s.StartSetup("youtube"); !errors.Is(err, ErrProfileInUse) {
		t.Fatalf("StartSetup = %v, want ErrProfileInUse", err)
	}
	if got := lastErrorSnapshot(s); !strings.Contains(got, "in use by "+nextHost) {
		t.Errorf("lastError after the refused setup = %q, want it to name %q — the profile is still held, and the status both UIs read must still say by whom", got, nextHost)
	}
}

// TestAProfileInUseLineNamesTheLockToDelete: the line both UIs show for a
// profile another machine's browser holds — AutoCookieStatus.LastError, read
// through GetStatus as the dashboard and the TUI read it — names the lock by
// its full path and says to delete it if no browser on that machine is using
// the profile, on the refresh's skip and on the setup's refusal alike. A lock
// whose browser crashed on that machine is never cleared by any pass, and
// "close it there" alone gave the operator no file to remove. The lock itself
// is still left where it is.
//
// Mutants: hand singletonLockHolder filepath.Base(path) in removeStaleLock —
// neither line carries the full path; drop the delete clause from the
// foreign-host refusal — both lines fail; drop the setError in
// startChromiumSetup's refusal — the setup leg's status has no line at all.
func TestAProfileInUseLineNamesTheLockToDelete(t *testing.T) {
	heldProfile := func(t *testing.T) (*AutoCookieService, string) {
		t.Helper()
		captureKills(t)
		profileDir := t.TempDir()
		lock := filepath.Join(profileDir, "SingletonLock")
		symlinkLock(t, foreignLockHost+"-4242", lock)
		cookiePath := ytAuthCookieFile(t)
		jar := NewCookieJar()
		if err := jar.Load(cookiePath); err != nil {
			t.Fatalf("load the fixture cookie file: %v", err)
		}
		s := NewAutoCookieService(profileDir, cookiePath, jar, nopAutoCookieLogger{})
		unlaunchable := filepath.Join(t.TempDir(), "not-a-browser")
		s.detectBrowser = func() *DetectedBrowser {
			return &DetectedBrowser{Type: "chrome", Path: unlaunchable, Name: "unlaunchable test browser"}
		}
		return s, lock
	}
	checkLine := func(t *testing.T, s *AutoCookieService, lock string) {
		t.Helper()
		if !filepath.IsAbs(lock) {
			t.Fatalf("fixture lock path %q is not absolute — the line must carry the full path", lock)
		}
		line := s.GetStatus().LastError
		if line == nil {
			t.Fatal("GetStatus().LastError is nil — the held profile left no line for either UI to show")
		}
		for _, want := range []string{
			"in use by " + foreignLockHost,
			"delete " + strconv.Quote(lock) + " if no browser on " + foreignLockHost + " is using that profile",
		} {
			if !strings.Contains(*line, want) {
				t.Errorf("the status line %q does not say %q", *line, want)
			}
		}
		if !lockSurvives(lock) {
			t.Error("the lock was unlinked — the sentence tells the operator to delete it; Moombox must not")
		}
	}

	t.Run("refresh", func(t *testing.T) {
		s, lock := heldProfile(t)
		if _, err := s.RefreshCookiesDetailed(context.Background()); !errors.Is(err, ErrProfileInUse) {
			t.Fatalf("RefreshCookiesDetailed = %v, want ErrProfileInUse", err)
		}
		checkLine(t, s, lock)
	})

	t.Run("setup", func(t *testing.T) {
		s, lock := heldProfile(t)
		if err := s.StartSetup("youtube"); !errors.Is(err, ErrProfileInUse) {
			t.Fatalf("StartSetup = %v, want ErrProfileInUse", err)
		}
		checkLine(t, s, lock)
	})
}
