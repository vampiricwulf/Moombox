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

func (c *compositeSolver) Batch(ctx context.Context, playerID string, sigs, ns []string) (map[string]string, map[string]string, error) {
	// Sig batch goes to sidecar (or fails); n batch falls back to goja
	// per-element on failure. Two passes keep the routing policy clear,
	// at the cost of a second round-trip when sidecar n succeeds.
	sigResults := map[string]string{}
	if len(sigs) > 0 {
		if c.sidecar == nil {
			return nil, nil, ErrSidecarUnavailable
		}
		sr, _, err := c.sidecar.Batch(ctx, playerID, sigs, nil)
		if err != nil {
			return nil, nil, err
		}
		// Validate sig completeness: composite sig is sidecar-only with no
		// fallback, so a partial map here is a hard failure not a quiet
		// success.
		for _, sig := range sigs {
			if _, ok := sr[sig]; !ok {
				return nil, nil, fmt.Errorf("cipher: composite Batch missing sig result for input")
			}
		}
		sigResults = sr
	}

	nResults := map[string]string{}
	if len(ns) > 0 {
		if c.sidecar != nil {
			_, nr, err := c.sidecar.Batch(ctx, playerID, nil, ns)
			if err == nil {
				for _, n := range ns {
					if _, ok := nr[n]; !ok {
						return nil, nil, fmt.Errorf("cipher: composite Batch missing n result for input")
					}
				}
				nResults = nr
			} else if c.goja == nil {
				return nil, nil, err
			} else {
				// fall through to per-element goja N
				for _, n := range ns {
					out, err := c.goja.N(ctx, playerID, n)
					if err != nil {
						return nil, nil, err
					}
					nResults[n] = out
				}
			}
		} else if c.goja != nil {
			for _, n := range ns {
				out, err := c.goja.N(ctx, playerID, n)
				if err != nil {
					return nil, nil, err
				}
				nResults[n] = out
			}
		} else {
			return nil, nil, errors.New("cipher: no solver available for n batch")
		}
	}

	return sigResults, nResults, nil
}

// Compile-time check: compositeSolver satisfies cipher.Solver.
var _ Solver = (*compositeSolver)(nil)
