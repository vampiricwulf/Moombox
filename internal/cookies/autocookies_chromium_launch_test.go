package cookies

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// This file covers the four Chromium launch helpers in
// autocookies_chromium.go that every headless-Chromium refresh depends on
// and that had no test before Arc 9 Task 1: waitForCDP, getFreePort,
// removeStaleLock, and cleanChromiumLockFiles. Never launches a real
// browser and never touches a real profile directory — everything here
// runs against t.TempDir() and stub HTTP servers.

// ageFile sets both atime and mtime of path to d in the past, so tests can
// exercise lockFileFreshThreshold's "stale" vs. "fresh" branches without
// ever sleeping.
func ageFile(t *testing.T, path string, d time.Duration) {
	t.Helper()
	when := time.Now().Add(-d)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("Chtimes(%q): %v", path, err)
	}
}

// --- removeStaleLock ---

// TestRemoveStaleLockRemovesAGenuinelyStaleFile is the (a) case: a lock file
// whose mtime is well past lockFileFreshThreshold is unlinked.
func TestRemoveStaleLockRemovesAGenuinelyStaleFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SingletonLock")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	ageFile(t, path, 10*time.Second)

	removeStaleLock(path)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale lock survived removeStaleLock: stat err = %v, want IsNotExist", err)
	}
}

// TestRemoveStaleLockKeepsAFreshFile is the case that matters: the whole
// reason lockFileFreshThreshold exists is so a lock file a LIVE browser is
// actively holding is never deleted out from under it (audit
// reports/cookies.md #9). "the stale lock is gone" (the test above) proves
// nothing about this property — this is the assertion that does.
//
// Standing mutation check: flip the freshness comparison in removeStaleLock
// (e.g. `>` instead of `<`, or drop the guard) and this test fails while the
// stale-removal test above keeps passing.
func TestRemoveStaleLockKeepsAFreshFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SingletonLock")
	if err := os.WriteFile(path, []byte("live"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	ageFile(t, path, 1*time.Second) // well under the 5s threshold

	removeStaleLock(path)

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a lock file touched 1s ago was removed — this is the live-browser guard "+
			"lockFileFreshThreshold exists for: stat err = %v, want nil", err)
	}
}

// TestRemoveStaleLockToleratesAMissingPath is the (c) case: a stat error is
// documented as "not present" — proceed with the unlink attempt, itself a
// no-op for a missing file. Must not panic and must leave no trace.
func TestRemoveStaleLockToleratesAMissingPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist")

	removeStaleLock(path) // must not panic

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("removeStaleLock on a missing path left something behind: stat err = %v", err)
	}
}

// TestRemoveStaleLockToleratesADirectoryItCannotRemove is the (d) case:
// os.Remove on a non-empty directory fails. removeStaleLock discards that
// error (it has no error return at all), so it must tolerate the failure
// silently — the directory, and what is inside it, must survive.
func TestRemoveStaleLockToleratesADirectoryItCannotRemove(t *testing.T) {
	dir := t.TempDir()
	lockDir := filepath.Join(dir, "SingletonLock")
	if err := os.Mkdir(lockDir, 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	// Non-empty, so os.Remove(lockDir) is guaranteed to fail rather than
	// quietly succeeding the way it would on an empty directory.
	inner := filepath.Join(lockDir, "child")
	if err := os.WriteFile(inner, []byte("x"), 0o644); err != nil {
		t.Fatalf("write inner fixture: %v", err)
	}
	ageFile(t, lockDir, 10*time.Second) // stale by mtime, so the unlink is attempted

	removeStaleLock(lockDir) // must not panic despite os.Remove failing

	if info, err := os.Stat(lockDir); err != nil || !info.IsDir() {
		t.Fatalf("directory did not survive a failed os.Remove: stat = (%v, %v)", info, err)
	}
	if _, err := os.Stat(inner); err != nil {
		t.Fatalf("directory contents did not survive: stat err = %v", err)
	}
}

// --- cleanChromiumLockFiles ---

// TestCleanChromiumLockFiles is the junction-defect guard the brief names
// directly: "the stale locks are gone" is satisfied even by a function that
// empties the whole profile directory. The real assertion is that every
// survivor — a lock file that is still fresh, and files that are not locks
// at all — is untouched.
//
// The fixture is built from chromiumLockFiles itself (never hardcoded)
// so a future addition to that slice is automatically covered here too,
// with one deliberate override: the SingletonSocket entry is aged as FRESH
// rather than stale, to prove the live-browser guard applies even to a
// canonical name, not only to the Singleton*/*.lockfile* glob matches.
func TestCleanChromiumLockFiles(t *testing.T) {
	dir := t.TempDir()

	const freshName = "SingletonSocket" // must be a member of chromiumLockFiles
	foundFreshName := false
	for _, name := range chromiumLockFiles {
		if name == freshName {
			foundFreshName = true
			break
		}
	}
	if !foundFreshName {
		t.Fatalf("test fixture assumes %q is in chromiumLockFiles = %v — update the fixture", freshName, chromiumLockFiles)
	}

	// Canonical names: every one stale (10s) except freshName, which is
	// aged 1s to stand in for a lock a live browser is holding right now.
	for _, name := range chromiumLockFiles {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
		if name == freshName {
			ageFile(t, path, 1*time.Second)
		} else {
			ageFile(t, path, 10*time.Second)
		}
	}

	// A Singleton* variant NOT in the canonical list — only the glob loop
	// reaches this one. Stale, so it must be removed.
	globOnly := filepath.Join(dir, "SingletonLock.lock")
	if err := os.WriteFile(globOnly, []byte("x"), 0o644); err != nil {
		t.Fatalf("write SingletonLock.lock: %v", err)
	}
	ageFile(t, globOnly, 10*time.Second)

	// Bystanders: real profile files that match neither the canonical
	// names nor either glob. Stale by age, but must never be touched by
	// name at all — this is what catches "the function just deletes
	// everything in the directory".
	cookiesDB := filepath.Join(dir, "Cookies")
	if err := os.WriteFile(cookiesDB, []byte("sqlite"), 0o644); err != nil {
		t.Fatalf("write Cookies: %v", err)
	}
	ageFile(t, cookiesDB, 10*time.Second)

	localState := filepath.Join(dir, "Local State")
	if err := os.WriteFile(localState, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write Local State: %v", err)
	}
	ageFile(t, localState, 10*time.Second)

	cleanChromiumLockFiles(dir)

	// Removed: every canonical name except freshName, plus the glob-only match.
	for _, name := range chromiumLockFiles {
		if name == freshName {
			continue
		}
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("stale canonical lock %q survived cleanChromiumLockFiles: stat err = %v", name, err)
		}
	}
	if _, err := os.Stat(globOnly); !os.IsNotExist(err) {
		t.Errorf("stale glob-matched lock %q survived cleanChromiumLockFiles: stat err = %v", globOnly, err)
	}

	// Survivors, the assertion that actually matters here.
	if _, err := os.Stat(filepath.Join(dir, freshName)); err != nil {
		t.Errorf("live lock %q (aged 1s) was removed — the freshness guard did not apply "+
			"to a canonical name: stat err = %v", freshName, err)
	}
	if _, err := os.Stat(cookiesDB); err != nil {
		t.Errorf("Cookies (a real cookie DB, not a lock file) was removed: stat err = %v", err)
	}
	if _, err := os.Stat(localState); err != nil {
		t.Errorf("Local State (not a lock file) was removed: stat err = %v", err)
	}
}

// --- getFreePort ---

// TestGetFreePort checks the two things production actually depends on: a
// plausible TCP port number comes back, and the port is immediately
// bindable — getFreePort closes its probe listener before returning
// specifically so the caller (startChromiumSetup / refreshChromium) can
// bind Chromium's --remote-debugging-port to it right after. Two
// consecutive calls need not return different ports (not asserted here).
func TestGetFreePort(t *testing.T) {
	port, err := getFreePort()
	if err != nil {
		t.Fatalf("getFreePort() error = %v", err)
	}
	if port <= 0 || port > 65535 {
		t.Fatalf("getFreePort() = %d, want a port in (0, 65535]", port)
	}

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("port %d returned by getFreePort was not immediately bindable: %v", port, err)
	}
	ln.Close()
}

// --- waitForCDP ---

// newLoopback127Server starts an httptest server bound explicitly to a
// 127.0.0.1:0 listener rather than relying on httptest.NewServer's own
// listener selection, because waitForCDP hard-codes "http://127.0.0.1:%d" —
// the test must guarantee the server actually answers on that address.
func newLoopback127Server(t *testing.T, handler http.Handler) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on 127.0.0.1:0: %v", err)
	}
	srv := httptest.NewUnstartedServer(handler)
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return ln.Addr().(*net.TCPAddr).Port
}

// TestWaitForCDPReturnsPromptlyOn200 is case (a): the very first poll
// succeeds, so waitForCDP must not wait out any backoff at all.
func TestWaitForCDPReturnsPromptlyOn200(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	port := newLoopback127Server(t, mux)

	start := time.Now()
	err := waitForCDP(context.Background(), port, 2*time.Second)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("waitForCDP() error = %v, want nil", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("waitForCDP took %v to see an immediate 200, want a prompt return", elapsed)
	}
}

// TestWaitForCDPTimesOutOnPersistent500 is case (b): the endpoint answers
// but never with 200. waitForCDP must return the timeout error only after
// its deadline elapses, and the short ~600ms timeout here is chosen so the
// 200ms/400ms backoff schedule runs at least twice before that deadline —
// without asserting the exact intervals, which Arc 8 already found to be
// flaky under load.
func TestWaitForCDPTimesOutOnPersistent500(t *testing.T) {
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	port := newLoopback127Server(t, mux)

	const timeout = 600 * time.Millisecond
	start := time.Now()
	err := waitForCDP(context.Background(), port, timeout)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("waitForCDP() error = nil, want a timeout error — the endpoint never answered 200")
	}
	if elapsed < timeout {
		t.Errorf("waitForCDP returned after %v, want it to have waited out its own %v timeout", elapsed, timeout)
	}
	if got := hits.Load(); got < 2 {
		t.Errorf("CDP endpoint was polled %d time(s), want at least 2 so the backoff schedule actually ran", got)
	}
}

// TestWaitForCDPTimesOutWithNoServer is case (c): nothing is listening on
// the port at all, so every poll fails at the transport level rather than
// with a non-200 status. Must still resolve to the same timeout error, not
// hang or return some other error shape.
func TestWaitForCDPTimesOutWithNoServer(t *testing.T) {
	port, err := getFreePort()
	if err != nil {
		t.Fatalf("getFreePort() error = %v", err)
	}

	const timeout = 300 * time.Millisecond
	start := time.Now()
	err = waitForCDP(context.Background(), port, timeout)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("waitForCDP() error = nil, want a timeout error — nothing is listening on this port")
	}
	if elapsed < timeout {
		t.Errorf("waitForCDP returned after %v, want it to have waited out its own %v timeout", elapsed, timeout)
	}
}

// TestWaitForCDPReturnsCtxErrBeforeDeadline is case (d): a cancelled
// context must short-circuit waitForCDP well before its own (deliberately
// generous, 5s) timeout deadline. The result is read through a channel
// against a hard 3s test-level cap — per the brief's own trap — so that a
// regression which makes waitForCDP ignore ctx entirely fails this test
// promptly instead of the run silently absorbing the full 5s wait.
//
// Standing mutation check: remove the `if ctx.Err() != nil` branch in
// waitForCDP and this test's outer select fires its timeout arm instead of
// observing ctx.Err().
func TestWaitForCDPReturnsCtxErrBeforeDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before waitForCDP is ever called

	resultCh := make(chan error, 1)
	start := time.Now()
	go func() {
		resultCh <- waitForCDP(ctx, 1, 5*time.Second)
	}()

	select {
	case err := <-resultCh:
		elapsed := time.Since(start)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waitForCDP() error = %v, want errors.Is(err, context.Canceled)", err)
		}
		if elapsed > 1*time.Second {
			t.Errorf("waitForCDP took %v to notice a cancelled ctx, want well under its 5s timeout", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waitForCDP did not return within 3s of a cancelled ctx — " +
			"it appears to be ignoring ctx and waiting out its own timeout instead")
	}
}
