package cookies

import (
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// fakeApplyUserOnlyDACL swaps the applyUserOnlyDACL seam for the duration of
// a test. Restored via t.Cleanup so a later test in the same binary hits the
// real utils.ApplyUserOnlyDACL again (no real icacls/chmod is ever exercised
// here — the codebase's own note in the brief: never spawn a real icacls in
// a test if a seam exists).
func fakeApplyUserOnlyDACL(t *testing.T, fn func(dir string) error) {
	t.Helper()
	real := applyUserOnlyDACL
	t.Cleanup(func() { applyUserOnlyDACL = real })
	applyUserOnlyDACL = fn
}

// TestTightenCookieDirOnceRetriesAfterFailure is the memo-before-apply
// regression test: with the old code (memoised BEFORE the apply ran), the
// very first failure would permanently disable hardening for the dir, and
// this test's second assertion would fail because no second attempt would
// ever be made.
func TestTightenCookieDirOnceRetriesAfterFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cookiedir")
	var calls int32
	done := make(chan struct{}, 8)
	fakeApplyUserOnlyDACL(t, func(d string) error {
		n := atomic.AddInt32(&calls, 1)
		defer func() { done <- struct{}{} }()
		if n == 1 {
			return errors.New("fake transient failure")
		}
		return nil
	})

	// First write: attempted, fails, must NOT be memoised as done.
	tightenCookieDirOnce(dir)
	<-done
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("after first write: calls = %d, want 1", got)
	}

	// Second write: the prior failure must have reset the dir to "not
	// started", so this attempts again (and this time succeeds).
	tightenCookieDirOnce(dir)
	<-done
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("after second write: calls = %d, want 2 (failure was not retried)", got)
	}

	// Third write: the second attempt succeeded, so this one must be
	// memoised — no third attempt at all.
	tightenCookieDirOnce(dir)
	select {
	case <-done:
		t.Fatal("third write re-attempted the DACL apply after a prior success")
	case <-time.After(100 * time.Millisecond):
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("after third write: calls = %d, want 2 (memoised on success)", got)
	}
}

// TestTightenCookieDirOnceConcurrentWritesSpawnOneApply asserts the
// "in flight" state: two writes landing while one apply is already running
// must spawn exactly one apply, not two. Without the in-flight state (the
// old two-state map, or a version that only memoises on success without an
// intermediate marker), the second call would see the dir as "not started"
// during the first call's shell-out and spawn a second apply — this test
// would then read the counter as 2.
func TestTightenCookieDirOnceConcurrentWritesSpawnOneApply(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cookiedir")
	var calls int32
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	done := make(chan struct{}, 4)
	fakeApplyUserOnlyDACL(t, func(d string) error {
		atomic.AddInt32(&calls, 1)
		started <- struct{}{}
		<-release
		done <- struct{}{}
		return nil
	})

	tightenCookieDirOnce(dir) // spawns the one apply, marks in flight synchronously
	tightenCookieDirOnce(dir) // lands while the first is in flight — must be a no-op

	<-started // the one apply has started

	select {
	case <-started:
		t.Fatal("a second apply started while the first was still in flight")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	<-done

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls = %d, want 1 (two concurrent writes must spawn one apply)", got)
	}
}

// TestTightenCookieDirOncePermanentFailureCostIsOnePerWrite is the Linux/
// "no not-supported sentinel" gate from the brief: utils.ApplyUserOnlyDACL
// never returns a distinguishable "unsupported" error on any platform (on
// Linux it is a real chmod that can fail for an ordinary permission reason,
// not a no-op), so a HOST where it fails permanently must cost exactly one
// shell-out per cookie write — never more (no runaway retry storm within a
// single write) and never fewer (no cap or backoff silently swallowing
// later writes' attempts).
func TestTightenCookieDirOncePermanentFailureCostIsOnePerWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cookiedir")
	var calls int32
	done := make(chan struct{}, 8)
	fakeApplyUserOnlyDACL(t, func(d string) error {
		atomic.AddInt32(&calls, 1)
		done <- struct{}{}
		return errors.New("fake permanent failure")
	})

	const writes = 3
	for i := 0; i < writes; i++ {
		tightenCookieDirOnce(dir)
		<-done
	}

	if got := atomic.LoadInt32(&calls); got != writes {
		t.Errorf("calls = %d, want %d (exactly one attempt per write on a permanently-failing host)", got, writes)
	}
}
