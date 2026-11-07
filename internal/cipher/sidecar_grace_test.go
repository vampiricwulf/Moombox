package cipher

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// TestSidecarReadyGraceIsAMinute pins the production value (owner decision
// PR-R3: a 60 s grace for sidecar readiness before a failure counts as
// systemic).
func TestSidecarReadyGraceIsAMinute(t *testing.T) {
	if sidecarReadyGrace != 60*time.Second {
		t.Errorf("sidecarReadyGrace = %v, want 60s", sidecarReadyGrace)
	}
	if sidecarReadyPoll != time.Second {
		t.Errorf("sidecarReadyPoll = %v, want 1s", sidecarReadyPoll)
	}
}

// A signature that meets the sidecar down — a crash, the supervisor's
// restart under way — waits for the child to come back within the outage's
// readiness grace, and is answered by it, in Sig and in Batch (owner decision
// PR-R3). It failed at once with ErrSidecarUnavailable, which the worker
// reads as systemic: every format needing the player JS dropped for the run,
// a live capture switched path for good, over a restart of a few seconds.
//
// Mutants: Sig's not-ready arm not calling awaitSidecar (ErrSidecarUnavailable
// at once); Batch's not doing so (the same).
func TestASidecarBackWithinTheGraceAnswers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		side := &reportingSidecar{permutingSidecar: newPermutingSidecar()}
		c := newCompositeSolverWith(side, &specGoja{specs: map[string][]int{}})
		ctx := context.Background()

		go func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic: %v", r)
				}
			}()
			time.Sleep(10 * time.Second) // the supervisor's restart
			side.up.Store(true)
		}()
		start := time.Now()
		if got, err := c.Sig(ctx, specPlayer, sigA); err != nil || got != scramble(sigA) {
			t.Fatalf("Sig across a 10 s restart = %q, %v; want %q from the child once it is back", got, err, scramble(sigA))
		}
		if waited := time.Since(start); waited < 10*time.Second || waited > 11*time.Second {
			t.Errorf("Sig waited %v, want about the restart's 10 s", waited)
		}

		side.up.Store(false)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic: %v", r)
				}
			}()
			time.Sleep(5 * time.Second)
			side.up.Store(true)
		}()
		// A length no spec has been learned for, so only the child answers.
		fresh := sigA + "q"
		start = time.Now()
		sr, _, err := c.Batch(ctx, specPlayer, []string{fresh}, nil)
		if err != nil || sr[fresh] != scramble(fresh) {
			t.Fatalf("Batch across a 5 s restart = %v, %v; want the child's answer", sr, err)
		}
		if waited := time.Since(start); waited < 5*time.Second {
			t.Errorf("Batch waited %v, want the restart's 5 s", waited)
		}
	})
}

// The grace is the outage's, not each request's: a child down past it fails
// the request that waited at the minute, and every request after it at once,
// typed as before (ErrSidecarUnavailable). A child that comes back ends the
// outage, and the next one is given its own grace.
//
// Mutants: awaitSidecar measuring from each request (the second Sig waits
// another minute); markSidecarUp not resetting downSince (the second
// outage's request fails at once).
func TestTheReadinessGraceIsTheOutages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		side := &reportingSidecar{permutingSidecar: newPermutingSidecar()}
		c := newCompositeSolverWith(side, &specGoja{specs: map[string][]int{}})
		ctx := context.Background()

		start := time.Now()
		if _, err := c.Sig(ctx, specPlayer, sigA); !errors.Is(err, ErrSidecarUnavailable) {
			t.Fatalf("Sig with the child down past the grace = %v, want ErrSidecarUnavailable", err)
		}
		if waited := time.Since(start); waited != sidecarReadyGrace {
			t.Errorf("the first request waited %v, want the grace (%v)", waited, sidecarReadyGrace)
		}
		start = time.Now()
		if _, err := c.Sig(ctx, specPlayer, sigB); !errors.Is(err, ErrSidecarUnavailable) {
			t.Fatalf("Sig after the grace = %v, want ErrSidecarUnavailable", err)
		}
		if waited := time.Since(start); waited != 0 {
			t.Errorf("a request after the grace waited %v, want none", waited)
		}

		side.up.Store(true)
		if _, err := c.Sig(ctx, specPlayer, sigA); err != nil {
			t.Fatalf("Sig with the child back: %v", err)
		}
		side.up.Store(false)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic: %v", r)
				}
			}()
			time.Sleep(30 * time.Second)
			side.up.Store(true)
		}()
		if _, err := c.Sig(ctx, specPlayer, sigB+"y"); err != nil {
			t.Errorf("a new outage's request within its own grace = %v, want the child's answer", err)
		}
	})
}

// No sidecar half installed — `use_sidecar = false` — is no child to wait
// for: the request fails at once, as before.
func TestNoSidecarIsNotWaitedFor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newCompositeSolverWith(nil, &specGoja{specs: map[string][]int{}})
		start := time.Now()
		if _, err := c.Sig(context.Background(), specPlayer, sigA); !errors.Is(err, ErrSidecarUnavailable) {
			t.Fatalf("Sig with no sidecar = %v, want ErrSidecarUnavailable", err)
		}
		if waited := time.Since(start); waited != 0 {
			t.Errorf("waited %v for a sidecar that is not installed", waited)
		}
	})
}

// Batch ends an outage when it finds the child up, as Sig does: a Batch-only
// caller that waited past one outage's grace and then met the child back
// gives the next outage its own grace.
//
// Mutant: Batch not calling markSidecarUp on finding the half ready (the
// second outage's Batch fails at once).
func TestBatchEndsTheOutageItFindsOver(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		side := &reportingSidecar{permutingSidecar: newPermutingSidecar()}
		c := newCompositeSolverWith(side, &specGoja{specs: map[string][]int{}})
		ctx := context.Background()

		if _, _, err := c.Batch(ctx, specPlayer, []string{sigA}, nil); !errors.Is(err, ErrSidecarUnavailable) {
			t.Fatalf("Batch with the child down past the grace = %v, want ErrSidecarUnavailable", err)
		}
		side.up.Store(true)
		if _, _, err := c.Batch(ctx, specPlayer, []string{sigA}, nil); err != nil {
			t.Fatalf("Batch with the child back: %v", err)
		}
		side.up.Store(false)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic: %v", r)
				}
			}()
			time.Sleep(30 * time.Second)
			side.up.Store(true)
		}()
		if _, _, err := c.Batch(ctx, specPlayer, []string{sigB + "y"}, nil); err != nil {
			t.Errorf("a new outage's Batch within its own grace = %v, want the child's answer", err)
		}
	})
}
