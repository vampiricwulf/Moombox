package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/logger"
	"github.com/vampiricwulf/Moombox/internal/updater"
)

// startUpdateLoop runs the loop until the test ends.
func startUpdateLoop(t *testing.T, check func() bool, timing updateCheckTiming) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runUpdateCheckLoop(ctx, func() bool { return true }, check, timing)
	}()
	t.Cleanup(func() { cancel(); <-done })
}

func waitChecks(t *testing.T, checks *atomic.Int32, n int32, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for checks.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("after 5 s: %d checks, want %d (%s)", checks.Load(), n, what)
		}
		time.Sleep(time.Millisecond)
	}
}

// A failed automatic check is tried again down the ladder, the same day, and
// the ladder ends: with the daily tick an hour away, two rungs make three
// checks and no more.
// Mutants: no retry arm (one check); a ladder that never ends (the count
// keeps climbing).
func TestUpdateCheckRetriesAFailedCheck(t *testing.T) {
	var checks atomic.Int32
	startUpdateLoop(t, func() bool { checks.Add(1); return false }, updateCheckTiming{
		initialDelay: time.Millisecond, period: time.Hour, poll: time.Hour,
		retry: []time.Duration{10 * time.Millisecond, 10 * time.Millisecond},
	})
	waitChecks(t, &checks, 3, "the boot check and two retries")
	time.Sleep(100 * time.Millisecond)
	if n := checks.Load(); n != 3 {
		t.Errorf("%d checks, want 3: the ladder must end at the daily tick", n)
	}
}

// A retry that gets an answer ends the ladder.
// Mutant: retry whatever the check answers — a third check.
func TestUpdateCheckStopsRetryingOnceAnswered(t *testing.T) {
	var checks atomic.Int32
	startUpdateLoop(t, func() bool { return checks.Add(1) >= 2 }, updateCheckTiming{
		initialDelay: time.Millisecond, period: time.Hour, poll: time.Hour,
		retry: []time.Duration{10 * time.Millisecond, 10 * time.Millisecond},
	})
	waitChecks(t, &checks, 2, "the failed boot check and the retry that answered")
	time.Sleep(100 * time.Millisecond)
	if n := checks.Load(); n != 2 {
		t.Errorf("%d checks, want 2", n)
	}
}

// A check due while the connectivity monitor says offline makes no request
// (and so logs no failure); the poll that finds the network up makes it.
// Mutant: ignore online — the check runs while offline.
func TestUpdateCheckWaitsForTheNetwork(t *testing.T) {
	var checks atomic.Int32
	var online atomic.Bool
	var polled atomic.Int32
	startUpdateLoop(t, func() bool { checks.Add(1); return true }, updateCheckTiming{
		initialDelay: time.Millisecond, period: time.Hour, poll: 5 * time.Millisecond,
		online:       func() bool { polled.Add(1); return online.Load() },
		offlineGrace: time.Hour,
	})
	deadline := time.Now().Add(5 * time.Second)
	for polled.Load() < 5 { // the boot attempt and a few polls, all offline
		if time.Now().After(deadline) {
			t.Fatal("the loop never asked whether the network was up")
		}
		time.Sleep(time.Millisecond)
	}
	if n := checks.Load(); n != 0 {
		t.Fatalf("%d checks while offline, want 0", n)
	}
	online.Store(true)
	waitChecks(t, &checks, 1, "the held check, once online")
	time.Sleep(50 * time.Millisecond)
	if n := checks.Load(); n != 1 {
		t.Errorf("%d checks after the restore, want 1", n)
	}
}

// A connectivity monitor that stays offline (it can be wrong) holds a check
// for the grace only; then the check is made anyway.
// Mutant: no grace — no check ever.
func TestUpdateCheckIsMadeAnywayAfterTheOfflineGrace(t *testing.T) {
	var checks atomic.Int32
	startUpdateLoop(t, func() bool { checks.Add(1); return true }, updateCheckTiming{
		initialDelay: time.Millisecond, period: time.Hour, poll: 5 * time.Millisecond,
		online:       func() bool { return false },
		offlineGrace: 60 * time.Millisecond,
	})
	waitChecks(t, &checks, 1, "the check made after the grace")
}

// A retry timer left armed by an earlier failed check does not restart the
// offline grace of a later check that is being held: the held check is made
// when its own grace runs out. Here the boot check fails online (retry in
// 1.5 s), the scheduled check at 1 s falls due offline (grace 0.8 s), and it
// is made at 1.8 s.
// Mutant: leave the retry armed while a check is held — it fires at 1.5 s,
// starts the grace again, the tick at 2 s starts it once more, and the check
// is not made before 2.8 s.
func TestUpdateCheckHeldOfflineIsNotPushedBackByAnOlderRetry(t *testing.T) {
	var checks atomic.Int32
	start := time.Now()
	startUpdateLoop(t, func() bool { checks.Add(1); return false }, updateCheckTiming{
		initialDelay: time.Millisecond, period: time.Second, poll: 5 * time.Millisecond,
		retry:        []time.Duration{1500 * time.Millisecond},
		online:       func() bool { return checks.Load() == 0 },
		offlineGrace: 800 * time.Millisecond,
	})
	waitChecks(t, &checks, 2, "the boot check and the held scheduled check")
	if took := time.Since(start); took > 2400*time.Millisecond {
		t.Errorf("the held check was made after %v, want about 1.8 s: an older retry restarted its grace", took)
	}
}

// The check tells the loop apart a failure, which is retried, from an answer.
func TestCheckAndBroadcastUpdateReportsAFailedCheck(t *testing.T) {
	log, err := logger.New(filepath.Join(t.TempDir(), "update.log"), "error", 4096, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	orig := checkForUpdate
	t.Cleanup(func() { checkForUpdate = orig })
	var lastTag string

	checkForUpdate = func(*updater.Updater, context.Context) (*updater.ReleaseInfo, error) {
		return nil, errors.New("GitHub API returned 403")
	}
	if checkAndBroadcastUpdate(context.Background(), nil, nil, nil, nil, log, nil, &lastTag) {
		t.Error("a failed check was reported as answered: it would not be retried")
	}

	// Cut short by shutdown: not a failure, nothing to retry.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !checkAndBroadcastUpdate(ctx, nil, nil, nil, nil, log, nil, &lastTag) {
		t.Error("a check cancelled by shutdown was reported as failed")
	}
}
