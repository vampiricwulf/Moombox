package bgutils

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vampiricwulf/Moombox/internal/bgutils/sidecar"
)

// graceSidecar is a sidecar handle whose child a test takes down and brings
// back: it mints only while up, like the real one.
type graceSidecar struct {
	up          atomic.Bool
	unrecovered atomic.Bool // true: no child has ever come up, or Stop was called
	mints       atomic.Int64
	// hold, when set, blocks the first IsHealthy call until it is closed:
	// a test holds a leader inside its mint while other callers join it.
	hold chan struct{}
	held atomic.Bool
}

func (g *graceSidecar) IsHealthy() bool {
	if g.hold != nil && g.held.CompareAndSwap(false, true) {
		<-g.hold
	}
	return g.up.Load()
}
func (g *graceSidecar) Recoverable() bool { return !g.unrecovered.Load() }

func (g *graceSidecar) GeneratePlayerPoToken(_ context.Context, binding, _ string) (string, error) {
	if !g.up.Load() {
		return "", errors.New("sidecar: unhealthy")
	}
	g.mints.Add(1)
	return "player-" + binding, nil
}

func (g *graceSidecar) GenerateGvsPoToken(_ context.Context, binding, _ string) (sidecar.GvsMintResult, error) {
	if !g.up.Load() {
		return sidecar.GvsMintResult{}, errors.New("sidecar: unhealthy")
	}
	g.mints.Add(1)
	return sidecar.GvsMintResult{PoToken: "gvs-" + binding, MinterFresh: true}, nil
}

func (g *graceSidecar) InvalidateCaches(context.Context) error { return nil }
func (g *graceSidecar) InvalidateIT(context.Context) error     { return nil }

// upAfter brings the child back after d, as the supervisor's restart does.
func (g *graceSidecar) upAfter(t *testing.T, d time.Duration) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("panic: %v", r)
			}
		}()
		time.Sleep(d)
		g.up.Store(true)
	}()
}

func newGraceProvider(side *graceSidecar) *PotProvider {
	pp := NewPotProvider(&BgConfig{}, bypassTestLogger{})
	pp.sidecar = side
	return pp
}

// TestSidecarMintGraceIsAMinute pins the production value (owner ruling
// R-F1: the same 60 s a signature solve waits).
func TestSidecarMintGraceIsAMinute(t *testing.T) {
	if sidecarMintGrace != 60*time.Second {
		t.Errorf("sidecarMintGrace = %v, want 60s", sidecarMintGrace)
	}
	if sidecarMintPoll != time.Second {
		t.Errorf("sidecarMintPoll = %v, want 1s", sidecarMintPoll)
	}
}

// A mint that meets the sidecar down — a crash, the supervisor's restart
// under way — waits for the child to come back within the outage's grace and
// is answered by it, on both entry points (owner ruling R-F1). It failed at
// once with errSidecarDown, and a GVS mint that fails sends its capture to
// the token-free pick or to HLS, over a restart of a few seconds.
//
// Mutants: generateAndMint not calling awaitSidecar (errSidecarDown at
// once); GenerateGvsPoToken not doing so (the same).
func TestAMintWaitsForASidecarBackWithinTheGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		side := &graceSidecar{}
		pp := newGraceProvider(side)
		ctx := context.Background()

		side.upAfter(t, 10*time.Second)
		start := time.Now()
		s, err := pp.GeneratePoToken(ctx, "VIDEOID", false)
		if err != nil || s.PoToken != "player-VIDEOID" {
			t.Fatalf("GeneratePoToken across a 10 s restart = %+v, %v; want the child's token once it is back", s, err)
		}
		if waited := time.Since(start); waited < 10*time.Second || waited > 11*time.Second {
			t.Errorf("GeneratePoToken waited %v, want about the restart's 10 s", waited)
		}

		side.up.Store(false)
		side.upAfter(t, 5*time.Second)
		start = time.Now()
		g, err := pp.GenerateGvsPoToken(ctx, "BINDING", "")
		if err != nil || g.PoToken != "gvs-BINDING" || !g.ViaSidecar {
			t.Fatalf("GenerateGvsPoToken across a 5 s restart = %+v, %v; want the child's token", g, err)
		}
		if waited := time.Since(start); waited < 5*time.Second || waited > 6*time.Second {
			t.Errorf("GenerateGvsPoToken waited %v, want about the restart's 5 s", waited)
		}

		// A bypassCache mint (the 403 credential refresh) waits the same way.
		side.up.Store(false)
		side.upAfter(t, 3*time.Second)
		if s, err := pp.GeneratePoToken(ctx, "VIDEOID", true); err != nil || s.PoToken != "gvs-VIDEOID" {
			t.Fatalf("a bypassCache mint across a 3 s restart = %+v, %v; want the child's token", s, err)
		}
		if got := pp.Stats().SidecarMintsHit; got != 3 {
			t.Errorf("SidecarMintsHit = %d, want 3", got)
		}
	})
}

// The grace is the outage's, not each mint's: a child down past it fails the
// mint that waited at the minute, with errSidecarDown and not a context
// error, and every mint after it at once. A child that comes back ends the
// outage, and the next one is given its own grace.
//
// Mutants: awaitSidecar measuring from each mint (the second waits another
// minute); awaitSidecar returning the deadline as a context error;
// markSidecarUp not resetting downSince (the second outage's mint fails at
// once).
func TestTheMintGraceIsTheOutages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		side := &graceSidecar{}
		pp := newGraceProvider(side)
		ctx := context.Background()

		start := time.Now()
		if _, err := pp.GenerateGvsPoToken(ctx, "A", ""); !errors.Is(err, errSidecarDown) {
			t.Fatalf("a GVS mint with the child down past the grace = %v, want errSidecarDown", err)
		}
		if waited := time.Since(start); waited != sidecarMintGrace {
			t.Errorf("the first mint waited %v, want the grace (%v)", waited, sidecarMintGrace)
		}
		start = time.Now()
		if _, err := pp.GeneratePoToken(ctx, "B", false); !errors.Is(err, errSidecarDown) {
			t.Fatalf("a mint after the grace = %v, want errSidecarDown", err)
		}
		if _, err := pp.GenerateGvsPoToken(ctx, "C", ""); !errors.Is(err, errSidecarDown) {
			t.Fatalf("a GVS mint after the grace = %v, want errSidecarDown", err)
		}
		if waited := time.Since(start); waited != 0 {
			t.Errorf("mints after the grace waited %v, want none", waited)
		}
		if got := side.mints.Load(); got != 0 {
			t.Errorf("%d mints reached a child that was down", got)
		}

		side.up.Store(true)
		if _, err := pp.GenerateGvsPoToken(ctx, "A", ""); err != nil {
			t.Fatalf("a mint with the child back: %v", err)
		}
		side.up.Store(false)
		side.upAfter(t, 30*time.Second)
		if _, err := pp.GenerateGvsPoToken(ctx, "D", ""); err != nil {
			t.Errorf("a new outage's mint within its own grace = %v, want the child's token", err)
		}
	})
}

// The supervisor attaches the handle again each time it brings the child
// back (cmd/moombox), and that ends the outage too: one that no mint saw
// end must not leave its clock running into the next.
//
// Mutant: SetSidecar not calling markSidecarUp (the second outage's mint
// fails at once).
func TestAttachingTheSidecarAgainEndsTheOutage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		side := &graceSidecar{}
		pp := newGraceProvider(side)
		ctx := context.Background()

		if _, err := pp.GenerateGvsPoToken(ctx, "A", ""); !errors.Is(err, errSidecarDown) {
			t.Fatalf("a mint with the child down past the grace = %v, want errSidecarDown", err)
		}
		// The child comes back and dies again with no mint in between.
		pp.SetSidecar(nil)
		pp.sidecar = side
		side.upAfter(t, 20*time.Second)
		if _, err := pp.GenerateGvsPoToken(ctx, "B", ""); err != nil {
			t.Errorf("the next outage's mint = %v, want the child's token within its own grace", err)
		}
	})
}

// "Don't wait when time is short" (owner ruling): a caller whose deadline is
// nearer than the mint grace does not wait for a restarting sidecar.
// It is told the sidecar is down at once, as every mint was before the grace
// existed, with its context live and the whole of its budget left for the
// work after the mint under the same deadline (the 403 recovery's player
// re-fetch and URL resolve under 10-45 s, an extraction's player request).
// Such a caller does not start the outage's clock: the first mint that does
// wait, long after, still has a full grace.
//
// Mutants: awaitSidecar waiting for a share of a short deadline, or for all
// of it (waited != 0, a spent or dead context); the short caller starting
// the outage's clock (the last mint below has 10 s of grace left and fails
// 45 s before the child is back).
func TestAMintWithAShortDeadlineDoesNotWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		side := &graceSidecar{}
		pp := newGraceProvider(side)
		start := time.Now()

		for _, budget := range []time.Duration{20 * time.Second, 45 * time.Second, sidecarMintGrace - time.Millisecond} {
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			if _, err := pp.GenerateGvsPoToken(ctx, "A", ""); !errors.Is(err, errSidecarDown) {
				t.Errorf("a %v caller's GVS mint = %v, want errSidecarDown", budget, err)
			}
			if _, err := pp.GeneratePoToken(ctx, "A", false); !errors.Is(err, errSidecarDown) {
				t.Errorf("a %v caller's mint = %v, want errSidecarDown", budget, err)
			}
			if _, err := pp.GeneratePoToken(ctx, "A", true); !errors.Is(err, errSidecarDown) {
				t.Errorf("a %v caller's bypassCache mint = %v, want errSidecarDown", budget, err)
			}
			if err := ctx.Err(); err != nil {
				t.Errorf("a %v caller's context ended in the mint: %v", budget, err)
			}
			if dl, _ := ctx.Deadline(); time.Until(dl) != budget {
				t.Errorf("a %v caller has %v of its budget left after three mints, want all of it", budget, time.Until(dl))
			}
			cancel()
		}
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("the short-deadline callers waited %v, want none", waited)
		}

		time.Sleep(50 * time.Second)
		side.upAfter(t, 55*time.Second)
		if _, err := pp.GenerateGvsPoToken(context.Background(), "B", ""); err != nil {
			t.Errorf("the first mint that waits, 50 s after callers that did not = %v; want the child's token within a full grace", err)
		}
	})
}

// A caller with the time waits: a deadline at least a whole grace away is
// treated as none. A 5 min caller is answered by the child
// when it comes back, a caller whose deadline is exactly the grace waits,
// and a 10 min caller waits out the whole grace and gets errSidecarDown, not
// a context error.
//
// Mutants: awaitSidecar refusing every caller that has a deadline (no wait,
// errSidecarDown at once); comparing with > instead of >= (the caller at
// exactly the grace fails at once).
func TestAMintWithALongDeadlineWaitsLikeOneWithNone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		side := &graceSidecar{}
		pp := newGraceProvider(side)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		side.upAfter(t, 10*time.Second)
		start := time.Now()
		if g, err := pp.GenerateGvsPoToken(ctx, "A", ""); err != nil || g.PoToken != "gvs-A" {
			t.Fatalf("a 5 min caller across a 10 s restart = %+v, %v; want the child's token", g, err)
		}
		if waited := time.Since(start); waited < 10*time.Second || waited > 11*time.Second {
			t.Errorf("the 5 min caller waited %v, want about the restart's 10 s", waited)
		}

		side.up.Store(false)
		side.upAfter(t, 10*time.Second)
		exact, cancelExact := context.WithTimeout(context.Background(), sidecarMintGrace)
		defer cancelExact()
		if s, err := pp.GeneratePoToken(exact, "B", false); err != nil || s.PoToken != "player-B" {
			t.Fatalf("a caller whose deadline is the grace = %+v, %v; want the child's token", s, err)
		}

		side.up.Store(false)
		long, cancelLong := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancelLong()
		start = time.Now()
		if _, err := pp.GenerateGvsPoToken(long, "C", ""); !errors.Is(err, errSidecarDown) {
			t.Fatalf("a 10 min caller with the child down past the grace = %v, want errSidecarDown", err)
		}
		if waited := time.Since(start); waited != sidecarMintGrace {
			t.Errorf("the 10 min caller waited %v, want the grace (%v)", waited, sidecarMintGrace)
		}
	})
}

// The comparison is with the whole grace, not with what is left of it on
// the outage's clock. In a real outage a job's mint with no deadline starts
// that clock almost at once, and a 403 recovery that arrives later must
// still not wait: 16 s in, 44 s are left, and a 45 s recovery that waited
// them would leave 1 s for its URL re-fetch; 51 s in, a 10 s recovery would
// wait 9 of its 10. Both are told at once, with the whole budget left, as on
// main, whether they would lead a mint or join the one in flight. A caller
// with the time (5 min) that arrives late waits only what the outage has
// left.
//
// Mutants: mintWait comparing the deadline with the grace still left (the
// 45 s caller waits 44 s, the 10 s caller 9 s); refusing every caller that
// has a deadline once the clock is running (the 5 min caller does not wait).
func TestAMintComparesItsDeadlineWithTheWholeGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		side := &graceSidecar{}
		pp := newGraceProvider(side)

		outage := time.Now()
		first := make(chan error, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic: %v", r)
				}
			}()
			_, err := pp.GeneratePoToken(context.Background(), "A", false)
			first <- err
		}()

		for _, c := range []struct{ at, budget time.Duration }{
			{16 * time.Second, 45 * time.Second},
			{51 * time.Second, 10 * time.Second},
		} {
			time.Sleep(time.Until(outage.Add(c.at)))
			ctx, cancel := context.WithTimeout(context.Background(), c.budget)
			start := time.Now()
			if _, err := pp.GenerateGvsPoToken(ctx, "B", ""); !errors.Is(err, errSidecarDown) {
				t.Errorf("a %v caller's GVS mint %v into the outage = %v, want errSidecarDown", c.budget, c.at, err)
			}
			if _, err := pp.GeneratePoToken(ctx, "A", false); !errors.Is(err, errSidecarDown) {
				t.Errorf("a %v caller joining the mint in flight %v into the outage = %v, want errSidecarDown", c.budget, c.at, err)
			}
			if waited := time.Since(start); waited != 0 {
				t.Errorf("a %v caller %v into the outage waited %v, want none", c.budget, c.at, waited)
			}
			if dl, _ := ctx.Deadline(); time.Until(dl) != c.budget {
				t.Errorf("a %v caller has %v of its budget left after its mints, want all of it", c.budget, time.Until(dl))
			}
			cancel()
		}

		long, cancelLong := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancelLong()
		start := time.Now()
		if _, err := pp.GenerateGvsPoToken(long, "C", ""); !errors.Is(err, errSidecarDown) {
			t.Errorf("a 5 min caller 51 s into the outage = %v, want errSidecarDown once the grace is spent", err)
		}
		if waited := time.Since(start); waited != 9*time.Second {
			t.Errorf("the 5 min caller waited %v, want the 9 s the outage had left", waited)
		}
		if err := <-first; !errors.Is(err, errSidecarDown) {
			t.Errorf("the mint that started the outage's clock = %v, want errSidecarDown", err)
		}
	})
}

// A caller that is cancelled while it waits leaves at once with its
// context's error, and one whose context has already ended does not wait.
//
// Mutant: awaitSidecar not selecting on ctx (the caller is held the whole
// minute).
func TestACancelledMintWaitLeavesWithItsContextsError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pp := newGraceProvider(&graceSidecar{})
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(7*time.Second, cancel)
		start := time.Now()
		if _, err := pp.GenerateGvsPoToken(ctx, "A", ""); !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled caller = %v, want context.Canceled", err)
		}
		if waited := time.Since(start); waited != 7*time.Second {
			t.Errorf("the cancelled caller waited %v, want 7 s", waited)
		}
		if _, err := pp.GeneratePoToken(ctx, "A", false); !errors.Is(err, context.Canceled) {
			t.Fatalf("a caller whose context has ended = %v, want its context's error", err)
		}
		if waited := time.Since(start); waited != 7*time.Second {
			t.Errorf("a caller whose context had ended waited: %v since the start", waited)
		}
	})
}

// A leader that did not wait because its own deadline was short is not the
// answer for a waiter that has the time: the waiter asks for itself, waits
// for the child and is answered by it. A caller with a short deadline that
// arrives while that waiter leads the mint does not join its wait: it is
// told at once, as it would be leading the mint itself.
//
// Mutants: the waiter taking the leader's errSidecarDown as its own answer
// (it fails at once, 30 s before the child is back); a short-deadline caller
// joining the mint in flight (it is held until its deadline and gets
// DeadlineExceeded).
func TestAWaiterWithTheTimeOutlivesALeaderWithout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		side := &graceSidecar{hold: make(chan struct{})}
		pp := newGraceProvider(side)
		start := time.Now()

		leaderCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		leaderErr := make(chan error, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic: %v", r)
				}
			}()
			_, err := pp.GeneratePoToken(leaderCtx, "VIDEOID", false)
			leaderErr <- err
		}()
		synctest.Wait() // the leader is inside its mint, held at the health read

		type answer struct {
			s   *SessionData
			err error
			at  time.Duration
		}
		waiter := make(chan answer, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic: %v", r)
				}
			}()
			s, err := pp.GeneratePoToken(context.Background(), "VIDEOID", false)
			waiter <- answer{s, err, time.Since(start)}
		}()
		synctest.Wait() // the waiter has joined the leader's mint
		if got := pp.Stats().InflightWaits; got != 1 {
			t.Fatalf("InflightWaits = %d, want 1: the waiter did not join the leader's mint", got)
		}

		side.upAfter(t, 30*time.Second)
		close(side.hold)
		if err := <-leaderErr; !errors.Is(err, errSidecarDown) {
			t.Errorf("the 20 s leader = %v, want errSidecarDown", err)
		}
		if waited := time.Since(start); waited != 0 {
			t.Errorf("the 20 s leader waited %v, want none", waited)
		}
		synctest.Wait() // the waiter leads the mint now and waits for the child

		lateCtx, cancelLate := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancelLate()
		if _, err := pp.GeneratePoToken(lateCtx, "VIDEOID", false); !errors.Is(err, errSidecarDown) {
			t.Errorf("a 20 s caller arriving during the waiter's mint = %v, want errSidecarDown", err)
		}
		if waited := time.Since(start); waited != 0 {
			t.Errorf("the 20 s caller waited %v behind the mint in flight, want none", waited)
		}

		got := <-waiter
		if got.err != nil || got.s.PoToken != "player-VIDEOID" {
			t.Fatalf("the waiter = %+v, %v; want the child's token", got.s, got.err)
		}
		if got.at < 30*time.Second || got.at > 31*time.Second {
			t.Errorf("the waiter was answered after %v, want about the restart's 30 s", got.at)
		}
	})
}

// A waiter that joined a mint whose leader was cancelled during the grace
// asks again too (inflightEntry.leaderGone).
func TestAWaiterOutlivesALeaderCancelledDuringTheGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		side := &graceSidecar{}
		pp := newGraceProvider(side)
		side.upAfter(t, 30*time.Second)

		leaderCtx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(5*time.Second, cancel)
		leaderErr := make(chan error, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic: %v", r)
				}
			}()
			_, err := pp.GeneratePoToken(leaderCtx, "VIDEOID", false)
			leaderErr <- err
		}()
		synctest.Wait()
		s, err := pp.GeneratePoToken(context.Background(), "VIDEOID", false)
		if err != nil || s.PoToken != "player-VIDEOID" {
			t.Fatalf("the waiter = %+v, %v; want the child's token", s, err)
		}
		if err := <-leaderErr; !errors.Is(err, context.Canceled) {
			t.Errorf("the leader = %v, want context.Canceled", err)
		}
	})
}

// A handle with no child to wait for is not waited for: one whose first
// start has not succeeded, and one that was stopped while a mint waited.
//
// Mutants: awaitSidecar not reading Recoverable before the wait (the first
// mint waits a minute); not reading it in the loop (the second waits out the
// grace instead of leaving at the stop).
func TestAHandleThatCannotComeBackIsNotWaitedFor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		side := &graceSidecar{}
		side.unrecovered.Store(true)
		pp := newGraceProvider(side)
		ctx := context.Background()

		start := time.Now()
		if _, err := pp.GeneratePoToken(ctx, "A", false); !errors.Is(err, errSidecarDown) {
			t.Fatalf("GeneratePoToken = %v, want errSidecarDown", err)
		}
		if _, err := pp.GenerateGvsPoToken(ctx, "A", ""); !errors.Is(err, errSidecarDown) {
			t.Fatalf("GenerateGvsPoToken = %v, want errSidecarDown", err)
		}
		if waited := time.Since(start); waited != 0 {
			t.Errorf("waited %v for a child that has never come up", waited)
		}

		side.unrecovered.Store(false)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic: %v", r)
				}
			}()
			time.Sleep(4500 * time.Millisecond)
			side.unrecovered.Store(true) // Stop
		}()
		start = time.Now()
		if _, err := pp.GenerateGvsPoToken(ctx, "B", ""); !errors.Is(err, errSidecarDown) {
			t.Fatalf("a mint across a Stop = %v, want errSidecarDown", err)
		}
		if waited := time.Since(start); waited != 5*time.Second {
			t.Errorf("a mint across a Stop waited %v, want to leave at the next poll (5 s)", waited)
		}
	})
}
