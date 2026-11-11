package twitch

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The GQL limiter is a lazily refilled token bucket: 10 tokens of burst,
// refilled at 10 per second from elapsed time, no goroutine and no ticker.
// These tests run in a synctest bubble, so time.Now and timers are the
// bubble's fake clock and nothing here sleeps for real.

const rateStep = time.Second / twitchGQLRatePerSec

// A fresh API hands out the whole burst without waiting.
func TestAcquireToken_BurstIsImmediate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := NewAPI(nil)
		start := time.Now()
		for i := range twitchGQLRatePerSec {
			if err := a.acquireToken(t.Context()); err != nil {
				t.Fatalf("token %d: %v", i+1, err)
			}
		}
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("the burst of %d waited %v, want no wait", twitchGQLRatePerSec, waited)
		}
	})
}

// The token after the burst waits one refill interval, and each one after
// it one more.
func TestAcquireToken_EleventhWaitsOneInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := NewAPI(nil)
		start := time.Now()
		for range twitchGQLRatePerSec {
			if err := a.acquireToken(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		for i := 1; i <= 3; i++ {
			if err := a.acquireToken(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got, want := time.Since(start), time.Duration(i)*rateStep; got != want {
				t.Fatalf("token %d arrived after %v, want %v", twitchGQLRatePerSec+i, got, want)
			}
		}
	})
}

// A long idle period refills the bucket to the burst and no further.
func TestAcquireToken_IdleNeverYieldsMoreThanBurst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := NewAPI(nil)
		if err := a.acquireToken(t.Context()); err != nil { // starts the clock
			t.Fatal(err)
		}
		time.Sleep(24 * time.Hour)

		start := time.Now()
		for range twitchGQLRatePerSec {
			if err := a.acquireToken(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("the burst after idling waited %v, want no wait", waited)
		}
		if err := a.acquireToken(t.Context()); err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(start); waited != rateStep {
			t.Fatalf("token %d after idling arrived after %v, want %v",
				twitchGQLRatePerSec+1, waited, rateStep)
		}
	})
}

// A partly drained bucket refills from elapsed time: half a second buys
// five tokens.
func TestAcquireToken_RefillsFromElapsedTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := NewAPI(nil)
		for range twitchGQLRatePerSec {
			if err := a.acquireToken(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(5 * rateStep)

		start := time.Now()
		for range 5 {
			if err := a.acquireToken(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("5 refilled tokens waited %v, want no wait", waited)
		}
		if err := a.acquireToken(t.Context()); err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(start); waited != rateStep {
			t.Fatalf("the 6th waited %v, want %v", waited, rateStep)
		}
	})
}

// Concurrent waiters each get exactly one token: 30 callers over an empty
// bucket are served one per interval, never two at once.
func TestAcquireToken_ConcurrentWaitersGetOneTokenEach(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := NewAPI(nil)
		for range twitchGQLRatePerSec {
			if err := a.acquireToken(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		const waiters = 30
		start := time.Now()
		var (
			mu      sync.Mutex
			arrived = map[time.Duration]int{}
			wg      sync.WaitGroup
		)
		for range waiters {
			wg.Go(func() {
				if err := a.acquireToken(t.Context()); err != nil {
					t.Errorf("waiter: %v", err)
					return
				}
				mu.Lock()
				arrived[time.Since(start)]++
				mu.Unlock()
			})
		}
		wg.Wait()

		if got, want := time.Since(start), waiters*rateStep; got != want {
			t.Fatalf("%d waiters drained in %v, want %v", waiters, got, want)
		}
		for i := 1; i <= waiters; i++ {
			at := time.Duration(i) * rateStep
			if arrived[at] != 1 {
				t.Fatalf("%d waiters served at %v, want exactly 1 (all: %v)", arrived[at], at, arrived)
			}
		}
	})
}

// A waiter whose context is cancelled returns at once with the context's
// error, and gives its place back: the next caller is served as if the
// cancelled one had never asked.
func TestAcquireToken_CancelledWaiterReturnsPromptly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := NewAPI(nil)
		for range twitchGQLRatePerSec {
			if err := a.acquireToken(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		// Queue 50 waiters (5 s of backlog), then cancel them all.
		ctx, cancel := context.WithCancel(t.Context())
		var failed atomic.Int32
		var wg sync.WaitGroup
		for range 50 {
			wg.Go(func() {
				if err := a.acquireToken(ctx); errors.Is(err, context.Canceled) {
					failed.Add(1)
				}
			})
		}
		synctest.Wait()
		start := time.Now()
		cancel()
		wg.Wait()
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("cancelled waiters took %v to return, want no wait", waited)
		}
		if failed.Load() != 50 {
			t.Fatalf("%d of 50 cancelled waiters returned context.Canceled", failed.Load())
		}
		// Their 50 reservations are returned: the next caller waits one
		// interval, not five seconds.
		if err := a.acquireToken(t.Context()); err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(start); waited != rateStep {
			t.Fatalf("the caller after the cancelled waiters waited %v, want %v", waited, rateStep)
		}
	})
}

// A context that is already done gets its error and takes no token.
func TestAcquireToken_DoneContextTakesNoToken(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := NewAPI(nil)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		for range 100 {
			if err := a.acquireToken(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
		}
		start := time.Now()
		for range twitchGQLRatePerSec {
			if err := a.acquireToken(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("the burst waited %v after 100 refused calls, want no wait", waited)
		}
	})
}

// The zero API (no constructor) works too: tests and callers build it both
// ways.
func TestAcquireToken_ZeroValueAPI(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := &API{}
		start := time.Now()
		for range twitchGQLRatePerSec + 1 {
			if err := a.acquireToken(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if waited := time.Since(start); waited != rateStep {
			t.Fatalf("burst+1 on a zero API took %v, want %v", waited, rateStep)
		}
	})
}
