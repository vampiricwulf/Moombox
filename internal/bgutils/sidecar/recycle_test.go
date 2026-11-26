package sidecar

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startPing runs s.ping on its own goroutine and hands back its result.
func startPing(ctx context.Context, s *Sidecar) <-chan error {
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- errors.New("ping panicked")
			}
		}()
		done <- s.ping(ctx)
	}()
	return done
}

// waitPending waits until n requests are registered with the child.
func waitPending(t *testing.T, s *Sidecar, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.PendingCount() == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("PendingCount never reached %d (now %d)", n, s.PendingCount())
}

// TestBeginRecycleRefusesWhileARequestIsPending: a recycle tears the child
// down, so one may begin only while nothing is waiting on the child's answer,
// and only one may run at a time.
func TestBeginRecycleRefusesWhileARequestIsPending(t *testing.T) {
	s, _, _ := newProbeSidecar(time.Minute, time.Minute, false)

	ctx, cancel := context.WithCancel(context.Background())
	hung := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				hung <- errors.New("call panicked")
			}
		}()
		hung <- s.call(ctx, "mint", nil, nil) // never answered
	}()
	waitPending(t, s, 1)

	if end, ok := s.BeginRecycle(); ok || end != nil {
		t.Error("BeginRecycle began a recycle with a request pending")
	}
	cancel()
	<-hung
	waitPending(t, s, 0)

	end, ok := s.BeginRecycle()
	if !ok {
		t.Fatal("BeginRecycle refused with nothing pending")
	}
	if _, again := s.BeginRecycle(); again {
		t.Error("a second recycle began while the first was running")
	}
	end()
	end() // idempotent
	if end2, ok := s.BeginRecycle(); !ok {
		t.Error("BeginRecycle refused after the previous recycle ended")
	} else {
		end2()
	}

	// A child that is already down is the supervisor's to restart: a recycle
	// begun on top of its death would start a second child.
	s.healthy.Store(false)
	if end, ok := s.BeginRecycle(); ok || end != nil {
		t.Error("BeginRecycle began a recycle on a child that is down")
	}
	if s.IsHealthy() {
		t.Error("a refused BeginRecycle left a gate installed")
	}
}

// TestACallArrivingDuringARecycleWaitsThenRunsOnTheNewChild: a caller that
// arrives while the child is being replaced is held before it registers with
// the old child, and goes on to the new one.
func TestACallArrivingDuringARecycleWaitsThenRunsOnTheNewChild(t *testing.T) {
	s, w, _ := newProbeSidecar(5*time.Second, time.Minute, true)

	end, ok := s.BeginRecycle()
	if !ok {
		t.Fatal("BeginRecycle refused")
	}
	s.healthy.Store(false) // the old child's teardown

	done := startPing(context.Background(), s)
	select {
	case err := <-done:
		t.Fatalf("the call returned during the recycle: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if n := s.PendingCount(); n != 0 {
		t.Errorf("PendingCount during the recycle = %d, want 0", n)
	}
	if n := w.count("ping"); n != 0 {
		t.Errorf("the waiting call reached the child %d times", n)
	}

	s.healthy.Store(true) // the new child is ready
	end()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the call after the recycle: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call never left the wait after the recycle ended")
	}
}

// TestACallWhoseContextEndsWhileWaitingOutARecycleGetsTheUnhealthyError: the wait is
// bounded by the caller's context and ends with today's error.
func TestACallWhoseContextEndsWhileWaitingOutARecycleGetsTheUnhealthyError(t *testing.T) {
	s, _, _ := newProbeSidecar(5*time.Second, time.Minute, true)
	end, ok := s.BeginRecycle()
	if !ok {
		t.Fatal("BeginRecycle refused")
	}
	defer end()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := s.ping(ctx)
	if err == nil || err.Error() != "sidecar: unhealthy" {
		t.Errorf("ping during a recycle past its context = %v, want sidecar: unhealthy", err)
	}
}

// TestACallThatPassedTheWaitBeforeARecycleBeganWaitsAgain: a caller past
// the wait that finds the child already torn down by a recycle goes back to
// waiting instead of reporting the sidecar down.
func TestACallThatPassedTheWaitBeforeARecycleBeganWaitsAgain(t *testing.T) {
	s, _, _ := newProbeSidecar(5*time.Second, time.Minute, true)
	end, ok := s.BeginRecycle()
	if !ok {
		t.Fatal("BeginRecycle refused")
	}
	defer end()
	s.healthy.Store(false) // the old child's teardown

	if err := s.callOnce(context.Background(), "ping", nil, nil, true); !errors.Is(err, errRecycleGate) {
		t.Errorf("a gated attempt on a recycling, down sidecar = %v, want errRecycleGate", err)
	}
	if err := s.callOnce(context.Background(), "shutdown", nil, nil, false); err == nil || err.Error() != "sidecar: unhealthy" {
		t.Errorf("an ungated attempt on a down sidecar = %v, want sidecar: unhealthy", err)
	}
}

// TestIsHealthyIsTrueThroughoutARecycle: consumers that gate on IsHealthy go
// on into call and wait there, instead of failing fast or falling to goja.
func TestIsHealthyIsTrueThroughoutARecycle(t *testing.T) {
	s, _, _ := newProbeSidecar(5*time.Second, time.Minute, true)
	end, ok := s.BeginRecycle()
	if !ok {
		t.Fatal("BeginRecycle refused")
	}
	s.healthy.Store(false)
	if !s.IsHealthy() {
		t.Error("IsHealthy false while a recycle was running")
	}
	end()
	if s.IsHealthy() {
		t.Error("IsHealthy true after the recycle ended with the child down")
	}
}

// TestRecycleRunsOnUpPublishesNoHealthAndCountsNoRestart: a deliberate
// recycle is not an outage.
func TestRecycleRunsOnUpPublishesNoHealthAndCountsNoRestart(t *testing.T) {
	resetHealth(t)
	s, _, _ := newProbeSidecar(5*time.Second, time.Minute, true)

	restarted := 0
	sup := NewSupervisor(SupervisorConfig{
		Restart: func(ctx context.Context) error {
			restarted++
			s.healthy.Store(false)
			s.healthy.Store(true)
			return nil
		},
		BeginRecycle: s.BeginRecycle,
		Logger:       silentLogger{},
	})
	onUp := 0
	sup.SetOnUp(func() { onUp++ })
	lastUp := time.Unix(1700000000, 0)
	sup.lastAttempt = 2
	sup.lastUp = lastUp

	published := 0
	unsub := SubscribeHealth(func(Health) { published++ })
	defer unsub()

	if err := sup.Recycle(context.Background()); err != nil {
		t.Fatalf("Recycle: %v", err)
	}
	if restarted != 1 {
		t.Errorf("Restart ran %d times, want 1", restarted)
	}
	if onUp != 1 {
		t.Errorf("onUp ran %d times, want 1", onUp)
	}
	if published != 0 {
		t.Errorf("Recycle published health %d times, want 0", published)
	}
	if n := sup.restarts.Load(); n != 0 {
		t.Errorf("restarts = %d, want 0", n)
	}
	if sup.lastAttempt != 2 || !sup.lastUp.Equal(lastUp) {
		t.Errorf("ladder state moved: lastAttempt %d lastUp %v", sup.lastAttempt, sup.lastUp)
	}
	if len(sup.notices) != 0 {
		t.Error("Recycle queued a notice")
	}
	if end, ok := s.BeginRecycle(); !ok {
		t.Error("the gate was left installed after Recycle")
	} else {
		end()
	}
}

// TestRecycleRefusesWhenTheSidecarIsBusy: a pending request means no recycle.
func TestRecycleRefusesWhenTheSidecarIsBusy(t *testing.T) {
	sup := NewSupervisor(SupervisorConfig{
		Restart:      func(context.Context) error { t.Error("Restart ran"); return nil },
		BeginRecycle: func() (func(), bool) { return nil, false },
		Logger:       silentLogger{},
	})
	if err := sup.Recycle(context.Background()); !errors.Is(err, ErrRecycleBusy) {
		t.Errorf("Recycle = %v, want ErrRecycleBusy", err)
	}
}

// TestAFailedRecycleStartHandsOverToTheDeathPath: the child is gone and the
// new one did not come up, so the supervisor's restart ladder takes over.
func TestAFailedRecycleStartHandsOverToTheDeathPath(t *testing.T) {
	resetHealth(t)
	s, _, _ := newProbeSidecar(5*time.Second, time.Minute, true)
	var done <-chan error
	sup := NewSupervisor(SupervisorConfig{
		Restart: func(ctx context.Context) error {
			s.healthy.Store(false)
			done = startPing(context.Background(), s)
			return errors.New("exited before ready")
		},
		BeginRecycle: s.BeginRecycle,
		Logger:       silentLogger{},
	})

	err := sup.Recycle(context.Background())
	if err == nil || err.Error() != "exited before ready" {
		t.Errorf("Recycle = %v, want the start error", err)
	}
	select {
	case reason := <-sup.notices:
		if !strings.HasPrefix(reason, "recycle failed: ") {
			t.Errorf("notice = %q, want a recycle failed: prefix", reason)
		}
	default:
		t.Error("a failed recycle queued no notice")
	}
	// The child is down, so IsHealthy is true only while a gate is installed.
	if s.IsHealthy() {
		t.Error("the gate was left installed after a failed recycle")
	}
	select {
	case err := <-done:
		if err == nil || err.Error() != "sidecar: unhealthy" {
			t.Errorf("the waiting call after a failed recycle = %v, want sidecar: unhealthy", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a call was left waiting after a failed recycle")
	}
}

// TestARecycleCutShortByShutdownIsNotADeath: Moombox shutting down cancels the
// recycle's context before Stop latches; the start error that follows is not
// an outage for the supervisor to announce and repair.
func TestARecycleCutShortByShutdownIsNotADeath(t *testing.T) {
	resetHealth(t)
	s, _, _ := newProbeSidecar(5*time.Second, time.Minute, true)
	ctx, cancel := context.WithCancel(context.Background())
	sup := NewSupervisor(SupervisorConfig{
		Restart: func(startCtx context.Context) error {
			s.healthy.Store(false)
			cancel()
			<-startCtx.Done()
			return startCtx.Err()
		},
		BeginRecycle: s.BeginRecycle,
		Logger:       silentLogger{},
	})

	if err := sup.Recycle(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Recycle = %v, want context.Canceled", err)
	}
	if len(sup.notices) != 0 {
		t.Error("a recycle cut short by shutdown queued a death notice")
	}
	if s.IsHealthy() {
		t.Error("the gate was left installed after a cancelled recycle")
	}
}

// TestStopDuringARecycleReleasesWaiters: Stop is terminal; a caller held by
// the recycle gets its error instead of waiting forever.
func TestStopDuringARecycleReleasesWaiters(t *testing.T) {
	resetHealth(t)
	s, _, _ := newProbeSidecar(5*time.Second, time.Minute, true)
	var done <-chan error
	sup := NewSupervisor(SupervisorConfig{
		Restart: func(ctx context.Context) error {
			s.healthy.Store(false)
			done = startPing(context.Background(), s)
			select {
			case err := <-done:
				t.Errorf("the call returned during the recycle: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			return ErrStopped
		},
		BeginRecycle: s.BeginRecycle,
		Logger:       silentLogger{},
	})

	if err := sup.Recycle(context.Background()); !errors.Is(err, ErrStopped) {
		t.Errorf("Recycle = %v, want ErrStopped", err)
	}
	if len(sup.notices) != 0 {
		t.Error("a recycle ended by Stop queued a notice")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Error("the waiting call succeeded on a stopped sidecar")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop during a recycle left the waiting call stuck")
	}
}

// TestSidecarTellsTheChildWhereToKeepPreparedPlayers: a real child given a
// player writes its prepared copy under <cacheDir>/players.
func TestSidecarTellsTheChildWhereToKeepPreparedPlayers(t *testing.T) {
	playerJS := readCipherFixture(t)
	cacheDir := sharedCacheDir(t)
	playersDir := filepath.Join(cacheDir, "players")
	if err := os.RemoveAll(playersDir); err != nil {
		t.Fatal(err)
	}
	s := startSidecar(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.SolveCipher(ctx, SolveCipherRequest{
		PlayerID:    "cb017549",
		PlayerJS:    string(playerJS),
		NChallenges: []string{"abcdefghij_12345"},
	}); err != nil {
		t.Fatalf("SolveCipher: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(playersDir, "cb017549.*.js"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Errorf("prepared players under %s = %v, want one for cb017549", playersDir, matches)
	}
}
