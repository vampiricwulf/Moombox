package cipher

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
)

// ErrSidecarUnavailable is returned by the composite solver when sig is
// requested but no sidecar solver is configured. Distinguishes from
// ErrSigUnavailable (player-specific extraction failure) so callers /
// tests can tell "sig not configured" from "this player can't be sig'd."
var ErrSidecarUnavailable = errors.New("cipher: sidecar solver not configured")

// SidecarSwappable is the composite solver's own interface: a Solver whose
// sidecar half can be replaced while the process runs. The BotGuard sidecar
// supervisor installs a freshly BUILT sidecar solver after every restart —
// the old one carries per-player "already sent" state that describes the dead
// child's memory, so it must be discarded rather than reused.
type SidecarSwappable interface {
	Solver
	SetSidecar(sidecar Solver)
}

// compositeSolver routes cipher requests across two underlying solvers
// per the policy in docs/superpowers/specs/2026-05-05-cipher-via-ejs-sidecar-design.md
// (removed after implementation; read it from git history, e.g. `git show
// 67755b9:docs/superpowers/specs/2026-05-05-cipher-via-ejs-sidecar-design.md`)
// section 7:
//
//	sig: sidecar, then the player's learned sig spec. Goja sig is dead on
//	     current players. If the sidecar fails and no spec is stored for
//	     the signature's length, we surface the error.
//	n:   sidecar primary, goja fallback. n stays available even if
//	     the sidecar is down.
//
// One of sidecar or goja may be nil. A nil sidecar means sig fails
// with ErrSidecarUnavailable, unless a spec answers it; a nil goja means n
// has no fallback.
//
// The sig spec (audit G49) is yt-dlp's: a signature function only moves
// characters around, so solving the synthetic challenge chr(0)..chr(L-1)
// once per player and length gives the permutation, which then answers any
// signature of that length with no JS at all (solve_sig and the
// 'sigfuncs' cache, yt_dlp/extractor/youtube/_video.py:3257-3258 and
// 3311-3334 at b77c9304e, stored on disk). yt-dlp answers every signature
// from the spec; Moombox keeps the sidecar's direct answer as the primary
// and the spec as what answers while the sidecar is down, restarting, or
// switched off after having run, which is the outage that otherwise lost
// every signatureCipher format for players already known. Beyond upstream,
// a spec is kept only once it reproduces the sidecar's own answer for a real
// signature of that length.
type compositeSolver struct {
	// mu guards sidecarSolver only. goja is fixed at construction.
	mu            sync.RWMutex
	sidecarSolver Solver
	goja          Solver
	// specs is where learned sig specs live: goja when it is the
	// *GojaResolver, whose player cache keeps them beside the player's JS
	// and drops them with it (InvalidateSolver, RemovePlayerJS). Nil when
	// goja is not a store, and then nothing is learned.
	specs sigSpecStore

	// specMu guards learning and refused. learning holds the (player,
	// length) pairs a learnSigSpecs is already asking the sidecar about, so
	// a burst of first signatures asks once. refused holds the pairs whose
	// spec failed verification: asking again each signature would add a
	// round-trip to every solve for a player whose spec never verifies.
	specMu   sync.Mutex
	learning map[string]struct{}
	refused  map[string]struct{}
}

// sigSpecStore keeps the sig specs compositeSolver learns, by player id and
// signature length. *GojaResolver is the production one.
type sigSpecStore interface {
	sigSpec(playerID string, length int) ([]int, bool)
	putSigSpec(playerID string, length int, spec []int)
}

// maxSigSpecLength bounds the synthetic challenge: real signatures are about
// a hundred characters, and a spec for a very long input would mean an input
// that is not a signature.
const maxSigSpecLength = 1024

// NewCompositeSolver wraps two underlying solvers with the routing
// policy. Pass nil for sidecar when the BotGuard sidecar is disabled
// or failed to start — the supervisor can install one later.
func NewCompositeSolver(sidecar, goja Solver) SidecarSwappable {
	return newCompositeSolverWith(sidecar, goja)
}

func newCompositeSolverWith(sidecar, goja Solver) *compositeSolver {
	specs, _ := goja.(sigSpecStore)
	return &compositeSolver{
		sidecarSolver: sidecar,
		goja:          goja,
		specs:         specs,
		learning:      make(map[string]struct{}),
		refused:       make(map[string]struct{}),
	}
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

// Sig asks the sidecar, and answers from the player's sig spec when the
// sidecar is absent or fails. A context that has ended is the caller's own
// error, not an outage, and is surfaced as it is.
func (c *compositeSolver) Sig(ctx context.Context, playerID, encryptedSig string) (string, error) {
	sc := c.sidecar()
	if sc == nil {
		if out, ok := c.sigFromSpec(playerID, encryptedSig); ok {
			return out, nil
		}
		return "", ErrSidecarUnavailable
	}
	out, err := sc.Sig(ctx, playerID, encryptedSig)
	if err != nil {
		if ctx.Err() == nil {
			if out, ok := c.sigFromSpec(playerID, encryptedSig); ok {
				return out, nil
			}
		}
		return "", err
	}
	c.learnSigSpecs(ctx, sc, playerID, map[string]string{encryptedSig: out})
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
	if sc != nil {
		sr, nr, err := sc.Batch(ctx, playerID, sigs, ns)
		if err == nil {
			// sidecarSolver.Batch already validated completeness; trust it.
			if len(sigs) > 0 {
				solved := make(map[string]string, len(sigs))
				for _, sig := range sigs {
					solved[sig] = sr[sig]
				}
				c.learnSigSpecs(ctx, sc, playerID, solved)
			}
			return sr, nr, nil
		}
		// Sidecar failed. Sig has no goja fallback, only the player's sig
		// specs: if any sigs were requested and a spec does not answer every
		// one, surface the error. n falls back to goja per-element.
		if len(sigs) > 0 {
			if ctx.Err() != nil {
				return nil, nil, err
			}
			var ok bool
			if sigResults, ok = c.sigsFromSpecs(playerID, sigs); !ok {
				return nil, nil, err
			}
		}
	} else if len(sigs) > 0 {
		// No sidecar configured but sig requested: the specs or nothing.
		var ok bool
		if sigResults, ok = c.sigsFromSpecs(playerID, sigs); !ok {
			return nil, nil, ErrSidecarUnavailable
		}
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

// sigFromSpec answers encryptedSig from the player's stored spec for its
// length, if there is one.
func (c *compositeSolver) sigFromSpec(playerID, encryptedSig string) (string, bool) {
	if c.specs == nil {
		return "", false
	}
	in := []rune(encryptedSig)
	spec, ok := c.specs.sigSpec(playerID, len(in))
	if !ok {
		return "", false
	}
	return applySigSpec(in, spec)
}

// sigsFromSpecs answers every one of sigs from the specs, or reports false
// when any one has none: a batch is answered whole or not at all.
func (c *compositeSolver) sigsFromSpecs(playerID string, sigs []string) (map[string]string, bool) {
	out := make(map[string]string, len(sigs))
	for _, sig := range sigs {
		solved, ok := c.sigFromSpec(playerID, sig)
		if !ok {
			return nil, false
		}
		out[sig] = solved
	}
	return out, true
}

// applySigSpec is yt-dlp's solve_sig: "".join(s[i] for i in spec). An index
// outside in (a spec stored for another length, a damaged file) is no answer.
func applySigSpec(in []rune, spec []int) (string, bool) {
	out := make([]rune, len(spec))
	for i, idx := range spec {
		if idx < 0 || idx >= len(in) {
			return "", false
		}
		out[i] = in[idx]
	}
	return string(out), true
}

// syntheticSigChallenge is yt-dlp's "".join(map(chr, range(L))): position i
// holds the character whose code is i, so the answer names, character by
// character, the input position it came from.
func syntheticSigChallenge(length int) string {
	r := make([]rune, length)
	for i := range r {
		r[i] = rune(i)
	}
	return string(r)
}

func sigSpecKey(playerID string, length int) string {
	return playerID + "\x00" + strconv.Itoa(length)
}

// learnSigSpecs stores a spec for every signature length in solved (real
// signatures the sidecar has just answered, mapped to its answers) that the
// player has none for yet. One extra sidecar call asks the synthetic
// challenge of every such length; its answer becomes a spec only if it
// reproduces the sidecar's answer for the real signature of that length.
// yt-dlp keeps the synthetic answer as it comes (_video.py:3325-3331);
// the check here is Moombox's own, as the spec is used only when the sidecar
// is not there to be asked, and a wrong one would build URLs that 403.
//
// Best-effort and synchronous: it runs at most once per player and length
// (persisted), its failure costs only the spec, and the caller already has
// its answer.
func (c *compositeSolver) learnSigSpecs(ctx context.Context, sc Solver, playerID string, solved map[string]string) {
	if c.specs == nil {
		return
	}
	type sample struct{ in, out string }
	samples := map[int]sample{}
	for in, out := range solved {
		n := len([]rune(in))
		if n == 0 || n > maxSigSpecLength {
			continue
		}
		if _, seen := samples[n]; seen {
			continue
		}
		if _, ok := c.specs.sigSpec(playerID, n); ok {
			continue
		}
		samples[n] = sample{in, out}
	}
	if len(samples) == 0 {
		return
	}

	c.specMu.Lock()
	for n := range samples {
		key := sigSpecKey(playerID, n)
		_, busy := c.learning[key]
		_, refused := c.refused[key]
		if busy || refused {
			delete(samples, n)
			continue
		}
		c.learning[key] = struct{}{}
	}
	c.specMu.Unlock()
	if len(samples) == 0 {
		return
	}
	defer func() {
		c.specMu.Lock()
		for n := range samples {
			delete(c.learning, sigSpecKey(playerID, n))
		}
		c.specMu.Unlock()
	}()

	synth := make([]string, 0, len(samples))
	for n := range samples {
		synth = append(synth, syntheticSigChallenge(n))
	}
	answers, _, err := sc.Batch(ctx, playerID, synth, nil)
	if err != nil {
		slog.Debug("cipher: sig spec: synthetic challenge failed", "playerID", playerID, "err", err)
		return
	}
	for n, smp := range samples {
		ans, ok := answers[syntheticSigChallenge(n)]
		if !ok {
			continue
		}
		spec := make([]int, 0, len(ans))
		for _, r := range ans {
			spec = append(spec, int(r))
		}
		if got, ok := applySigSpec([]rune(smp.in), spec); !ok || got != smp.out || len(spec) == 0 {
			c.specMu.Lock()
			c.refused[sigSpecKey(playerID, n)] = struct{}{}
			c.specMu.Unlock()
			slog.Debug("cipher: sig spec: synthetic answer does not reproduce the sidecar's, not stored",
				"playerID", playerID, "length", n)
			continue
		}
		c.specs.putSigSpec(playerID, n, spec)
	}
}

// Compile-time checks: compositeSolver satisfies cipher.Solver and the
// swappable variant the BotGuard sidecar supervisor installs restarts through.
var _ Solver = (*compositeSolver)(nil)
var _ SidecarSwappable = (*compositeSolver)(nil)
