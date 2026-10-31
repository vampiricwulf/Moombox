package cipher

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrSidecarUnavailable is returned by the composite solver when sig is
// requested but no sidecar solver can answer it: none is configured
// (`[bgutils] use_sidecar = false`, or a first start that never came up), or
// the one installed reports its child down — crashed, or between a crash and
// the supervisor's restart (SidecarReady). Distinguishes from
// ErrSigUnavailable (player-specific extraction failure) so callers /
// tests can tell "no sig solver running" from "this player can't be sig'd."
//
// The down case used to surface as the sidecar's own untyped "sidecar:
// unhealthy" or "sidecar: not running", which read like any one request's
// failure. It is the worker's systemic signal (audit G10): every format whose
// URL needs a signature fails alike while it holds, so the VOD and live
// strategies drop to formats that need no player JS rather than re-select
// one itag at a time.
var ErrSidecarUnavailable = errors.New("cipher: sidecar solver unavailable")

// SidecarSwappable is the composite solver's own interface: a Solver whose
// sidecar half can be replaced while the process runs. The BotGuard sidecar
// supervisor installs a freshly BUILT sidecar solver after every restart —
// the old one carries per-player "already sent" state that describes the dead
// child's memory, so it must be discarded rather than reused.
type SidecarSwappable interface {
	Solver
	SetSidecar(sidecar Solver)
	// SidecarReady reports whether a sidecar half is installed and its child
	// is up — whether player JS challenges (sig above all, which is
	// sidecar-only) can be solved at all right now. See compositeSolver's.
	SidecarReady() bool
}

// readinessReporter is the optional half of a Solver that knows whether it
// can be asked at all: the sidecar solver, through its *sidecar.Sidecar
// (IsHealthy). A Solver without it — goja, the test fakes — is taken as
// ready, so the healthy routing is what an unknown solver gets.
type readinessReporter interface {
	ready() bool
}

// solverReady reports whether s, a non-nil sidecar half, can be asked now.
func solverReady(s Solver) bool {
	r, ok := s.(readinessReporter)
	return !ok || r.ready()
}

// compositeSolver routes cipher requests across two underlying solvers
// per the policy in docs/superpowers/specs/2026-05-05-cipher-via-ejs-sidecar-design.md
// (removed after implementation; read it from git history, e.g. `git show
// 67755b9:docs/superpowers/specs/2026-05-05-cipher-via-ejs-sidecar-design.md`)
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
	// mu guards sidecarSolver only. goja is fixed at construction.
	mu            sync.RWMutex
	sidecarSolver Solver
	goja          Solver
}

// NewCompositeSolver wraps two underlying solvers with the routing
// policy. Pass nil for sidecar when the BotGuard sidecar is disabled
// or failed to start — the supervisor can install one later.
func NewCompositeSolver(sidecar, goja Solver) SidecarSwappable {
	return newCompositeSolverWith(sidecar, goja)
}

func newCompositeSolverWith(sidecar, goja Solver) *compositeSolver {
	return &compositeSolver{sidecarSolver: sidecar, goja: goja}
}

// SetSidecar installs (or, with nil, removes) the sidecar half. Safe to call
// while other goroutines are solving.
func (c *compositeSolver) SetSidecar(sidecar Solver) {
	c.mu.Lock()
	c.sidecarSolver = sidecar
	c.mu.Unlock()
}

// sidecar returns the current sidecar half. Every method takes ONE snapshot
// per call so a swap mid-call cannot make one method use two solvers.
func (c *compositeSolver) sidecar() Solver {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sidecarSolver
}

// SidecarReady is the solver's health accessor: true while a sidecar half is
// installed and reports its child up. It is false for the whole of an outage
// — `[bgutils] use_sidecar = false`, a first start that failed, a crash until
// the supervisor's restart installs the rebuilt solver (SetSidecar) — which
// is when no signature can be solved and n has only the goja fallback. The
// extraction reads it to rank the JS-less client first, yt-dlp's
// _DEFAULT_JSLESS_CLIENTS (_video.py:144, 2972-2977, 2994-3000):
// youtube.PlayerAPI's SetJSChallengeSolvable, wired in cmd/moombox.
func (c *compositeSolver) SidecarReady() bool {
	sc := c.sidecar()
	return sc != nil && solverReady(sc)
}

// sidecarDown is the error a sig request gets from sidecar half sc that
// failed with err — or, err nil, that was never asked because it reported
// itself down: ErrSidecarUnavailable whenever sc is down now, wrapping the
// child's own error when there is one, and err as it came otherwise. A
// request in flight when the child dies is drained with an error of the
// sidecar's own; read after the failure, the readiness types that one too.
func sidecarDown(sc Solver, err error) error {
	if solverReady(sc) {
		return err
	}
	if err == nil {
		return fmt.Errorf("%w: the sidecar is not running", ErrSidecarUnavailable)
	}
	return fmt.Errorf("%w: the sidecar is not running: %w", ErrSidecarUnavailable, err)
}

func (c *compositeSolver) Sig(ctx context.Context, playerID, encryptedSig string) (string, error) {
	sc := c.sidecar()
	if sc == nil {
		return "", ErrSidecarUnavailable
	}
	if !solverReady(sc) {
		return "", sidecarDown(sc, nil)
	}
	out, err := sc.Sig(ctx, playerID, encryptedSig)
	if err != nil {
		return "", sidecarDown(sc, err)
	}
	return out, nil
}

func (c *compositeSolver) N(ctx context.Context, playerID, encryptedN string) (string, error) {
	if sc := c.sidecar(); sc != nil {
		out, err := sc.N(ctx, playerID, encryptedN)
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
// violation — the underlying sidecarSolver validates completeness
// post-call; this composite trusts that inner validation.
//
// "Single round-trip" applies per-Batch-invocation. Callers that
// loop over per-element decryption (e.g., the DASH worker's
// per-stream RoutedDecryptNInURL) still pay one round-trip per
// element and don't benefit from this optimisation. Future
// work: gather per-stream n-params and call Batch once.
func (c *compositeSolver) Batch(ctx context.Context, playerID string, sigs, ns []string) (map[string]string, map[string]string, error) {
	sigResults := map[string]string{}
	nResults := map[string]string{}

	sc := c.sidecar()
	if sc != nil && !solverReady(sc) {
		// Down: not asked, as Sig does not ask it. The n entries go to goja.
		if len(sigs) > 0 {
			return nil, nil, sidecarDown(sc, nil)
		}
		sc = nil
	}
	if sc != nil {
		sr, nr, err := sc.Batch(ctx, playerID, sigs, ns)
		if err == nil {
			// sidecarSolver.Batch already validated completeness; trust it.
			return sr, nr, nil
		}
		// Sidecar failed. Sig is sidecar-only; if any sigs were
		// requested, surface the error — typed as Sig types it, when the
		// child is down. n falls back to goja per-element.
		if len(sigs) > 0 {
			return nil, nil, sidecarDown(sc, err)
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

// Compile-time checks: compositeSolver satisfies cipher.Solver and the
// swappable variant the BotGuard sidecar supervisor installs restarts through.
var _ Solver = (*compositeSolver)(nil)
var _ SidecarSwappable = (*compositeSolver)(nil)
