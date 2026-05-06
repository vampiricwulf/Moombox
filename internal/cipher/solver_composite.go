package cipher

import (
	"context"
	"errors"
	"fmt"
)

// ErrSidecarUnavailable is returned by the composite solver when sig is
// requested but no sidecar solver is configured. Distinguishes from
// ErrSigUnavailable (player-specific extraction failure) so callers /
// tests can tell "sig not configured" from "this player can't be sig'd."
var ErrSidecarUnavailable = errors.New("cipher: sidecar solver not configured")

// compositeSolver routes cipher requests across two underlying solvers
// per the policy in docs/superpowers/specs/2026-05-05-cipher-via-ejs-sidecar-design.md
// section 7:
//
//	sig: sidecar only. Goja sig is dead on current players. If the
//	     sidecar fails, we surface the error.
//	n:   sidecar primary, goja fallback. n stays available even if
//	     the sidecar is down.
//
// One of sidecar or goja may be nil. A nil sidecar means sig fails
// with ErrSidecarUnavailable; a nil goja means n has no fallback.
type compositeSolver struct {
	sidecar Solver
	goja    Solver
}

// NewCompositeSolver wraps two underlying solvers with the routing
// policy. Pass nil for sidecar when the BotGuard sidecar is disabled
// or failed to start.
func NewCompositeSolver(sidecar, goja Solver) Solver {
	return newCompositeSolverWith(sidecar, goja)
}

func newCompositeSolverWith(sidecar, goja Solver) *compositeSolver {
	return &compositeSolver{sidecar: sidecar, goja: goja}
}

func (c *compositeSolver) Sig(ctx context.Context, playerID, encryptedSig string) (string, error) {
	if c.sidecar == nil {
		return "", ErrSidecarUnavailable
	}
	return c.sidecar.Sig(ctx, playerID, encryptedSig)
}

func (c *compositeSolver) N(ctx context.Context, playerID, encryptedN string) (string, error) {
	if c.sidecar != nil {
		out, err := c.sidecar.N(ctx, playerID, encryptedN)
		if err == nil {
			return out, nil
		}
		// fall through to goja
	}
	if c.goja == nil {
		return "", errors.New("cipher: no solver available for n")
	}
	return c.goja.N(ctx, playerID, encryptedN)
}

// Batch combines sig and n decryption into a single solver call where
// possible. The composite issues ONE sidecar call for both arrays;
// only on sidecar failure do we fall through to per-element goja for
// the n entries. Sig has no goja fallback (sidecar-only routing
// policy from the spec).
//
// TRADE-OFF: when both sigs and ns are non-empty and the sidecar call
// fails, the n entries that goja could have served are lost too —
// the entire batch errors. The pre-5cc4e5b two-call design avoided
// this at the cost of a second round-trip on every healthy mixed
// batch. The single-call design is correct for the steady state
// (sidecar healthy → one round-trip) but trades resilience: a
// transient sidecar hiccup with mixed input fails the whole batch.
// Callers that want max resilience should call Sig and N separately;
// Batch is for callers that prioritise round-trip count.
//
// Implementations MUST return a result for every input in sigs and
// ns on success, or return an error. Partial maps are a contract
// violation — both this composite and the underlying sidecarSolver
// validate completeness post-call.
func (c *compositeSolver) Batch(ctx context.Context, playerID string, sigs, ns []string) (map[string]string, map[string]string, error) {
	sigResults := map[string]string{}
	nResults := map[string]string{}

	if c.sidecar != nil {
		sr, nr, err := c.sidecar.Batch(ctx, playerID, sigs, ns)
		if err == nil {
			// Validate completeness — partial map = silent data loss.
			for _, sig := range sigs {
				if _, ok := sr[sig]; !ok {
					return nil, nil, fmt.Errorf("cipher: composite Batch missing sig result for input")
				}
			}
			for _, n := range ns {
				if _, ok := nr[n]; !ok {
					return nil, nil, fmt.Errorf("cipher: composite Batch missing n result for input")
				}
			}
			return sr, nr, nil
		}
		// Sidecar failed. Sig is sidecar-only; if any sigs were
		// requested, surface the error. n falls back to goja per-element.
		if len(sigs) > 0 {
			return nil, nil, err
		}
	} else if len(sigs) > 0 {
		// No sidecar configured but sig requested.
		return nil, nil, ErrSidecarUnavailable
	}

	// We're here because: sidecar nil with no sigs, OR sidecar n-batch failed.
	// Either way, n has to come from goja.
	if len(ns) > 0 {
		if c.goja == nil {
			return nil, nil, errors.New("cipher: no solver available for n batch")
		}
		for _, n := range ns {
			out, err := c.goja.N(ctx, playerID, n)
			if err != nil {
				return nil, nil, err
			}
			nResults[n] = out
		}
	}

	return sigResults, nResults, nil
}

// Compile-time check: compositeSolver satisfies cipher.Solver.
var _ Solver = (*compositeSolver)(nil)
