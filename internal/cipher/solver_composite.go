package cipher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
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

	// specMu guards learning, refused and failed. learning holds the
	// (player, length) pairs a learnSigSpecs is already asking the sidecar
	// about, so a burst of first signatures asks once. refused holds the
	// pairs whose spec failed verification, or whose synthetic challenge the
	// sidecar answered without an answer for it: the spec is the player's,
	// and asking again each signature would add a round-trip to every solve
	// for a player whose spec never verifies. failed holds the pairs whose
	// synthetic challenge the sidecar failed outright (an ejs that throws on
	// it, a timeout, a crash), for as long as that sidecar is installed:
	// SetSidecar clears it, so a restarted child is asked once more, while
	// the one that failed is not asked again on every signature.
	specMu   sync.Mutex
	learning map[string]struct{}
	refused  map[string]struct{}
	failed   map[string]struct{}
}

// sigSpecProber is the sidecar half's way to ask the synthetic challenge
// without its stale-JS recovery (sidecarSolver.probeSigs). A sidecar half
// that has none is asked through Batch.
type sigSpecProber interface {
	probeSigs(ctx context.Context, playerID string, sigs []string) (map[string]string, error)
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
		failed:        make(map[string]struct{}),
	}
}

// SetSidecar installs (or, with nil, removes) the sidecar half. Safe to call
// while other goroutines are solving.
// The synthetic challenges the previous sidecar failed are asked again of
// the new one.
func (c *compositeSolver) SetSidecar(sidecar Solver) {
	c.mu.Lock()
	c.sidecarSolver = sidecar
	c.mu.Unlock()
	c.specMu.Lock()
	clear(c.failed)
	c.specMu.Unlock()
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
	if !solverReady(sc) {
		if out, ok := c.sigFromSpec(playerID, encryptedSig); ok {
			return out, nil
		}
		return "", sidecarDown(sc, nil)
	}
	out, err := sc.Sig(ctx, playerID, encryptedSig)
	if err != nil {
		if ctx.Err() == nil {
			if out, ok := c.sigFromSpec(playerID, encryptedSig); ok {
				return out, nil
			}
		}
		return "", sidecarDown(sc, err)
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
	if sc != nil && !solverReady(sc) {
		// Down: not asked, as Sig does not ask it. The n entries go to goja.
		if len(sigs) > 0 {
			var ok bool
			if sigResults, ok = c.sigsFromSpecs(playerID, sigs); !ok {
				return nil, nil, sidecarDown(sc, nil)
			}
		}
		sc = nil
	}
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
		// one, surface the error — typed as Sig types it, when the child is
		// down. n falls back to goja per-element.
		if len(sigs) > 0 {
			if ctx.Err() != nil {
				return nil, nil, err
			}
			var ok bool
			if sigResults, ok = c.sigsFromSpecs(playerID, sigs); !ok {
				return nil, nil, sidecarDown(sc, err)
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
// (persisted) while it succeeds, and once per installed sidecar while the
// synthetic challenge fails; its failure costs only the spec, and the caller
// already has its answer. The challenge goes through the sidecar's probe
// path, which never runs the stale-JS recovery a real signature gets.
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
		_, failed := c.failed[key]
		if busy || refused || failed {
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
	var answers map[string]string
	var err error
	if p, ok := sc.(sigSpecProber); ok {
		answers, err = p.probeSigs(ctx, playerID, synth)
	} else {
		answers, _, err = sc.Batch(ctx, playerID, synth, nil)
	}
	if err != nil {
		// The caller's own ended context says nothing about the challenge:
		// the next signature asks it again.
		if ctx.Err() == nil {
			c.specMu.Lock()
			for n := range samples {
				c.failed[sigSpecKey(playerID, n)] = struct{}{}
			}
			c.specMu.Unlock()
		}
		slog.Debug("cipher: sig spec: synthetic challenge failed", "playerID", playerID, "err", err)
		return
	}
	for n, smp := range samples {
		ans, ok := answers[syntheticSigChallenge(n)]
		if !ok {
			c.specMu.Lock()
			c.refused[sigSpecKey(playerID, n)] = struct{}{}
			c.specMu.Unlock()
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
