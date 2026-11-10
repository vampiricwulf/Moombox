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
}

func (g *graceSidecar) IsHealthy() bool   { return g.up.Load() }
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

// A caller with less time than the grace keeps its own budget: it leaves on
// its deadline with its context's error, and the outage's clock goes on for
// the callers after it.
//
// Mutants: awaitSidecar not selecting on ctx (the caller is held the whole
// minute); the caller's leaving restarting the clock (the last mint below
// waits a further minute instead of the 40 s left).
func TestAMintWithAShorterBudgetThanTheGraceKeepsIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		side := &graceSidecar{}
		pp := newGraceProvider(side)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		start := time.Now()
		if _, err := pp.GenerateGvsPoToken(ctx, "A", ""); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a 20 s caller = %v, want its own deadline", err)
		}
		if _, err := pp.GeneratePoToken(ctx, "A", false); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a caller whose context has ended = %v, want its context's error", err)
		}
		if waited := time.Since(start); waited != 20*time.Second {
			t.Errorf("the 20 s caller waited %v", waited)
		}

		if _, err := pp.GenerateGvsPoToken(context.Background(), "B", ""); !errors.Is(err, errSidecarDown) {
			t.Fatalf("the next mint = %v, want errSidecarDown at the outage's minute", err)
		}
		if waited := time.Since(start); waited != sidecarMintGrace {
			t.Errorf("the outage's grace ended %v after it began, want %v", waited, sidecarMintGrace)
		}
	})
}

// A waiter that joined a mint whose leader left on its own deadline inside
// the grace asks again, and is answered by the child once it is back: the
// leader's context is not the waiter's answer.
func TestAWaiterOutlivesALeaderThatLeftDuringTheGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		side := &graceSidecar{}
		pp := newGraceProvider(side)
		side.upAfter(t, 30*time.Second)

		leaderCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
		synctest.Wait() // the leader is waiting out the grace
		s, err := pp.GeneratePoToken(context.Background(), "VIDEOID", false)
		if err != nil || s.PoToken != "player-VIDEOID" {
			t.Fatalf("the waiter = %+v, %v; want the child's token", s, err)
		}
		if err := <-leaderErr; !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("the leader = %v, want its own deadline", err)
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
