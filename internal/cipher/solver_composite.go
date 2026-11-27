package cipher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// ErrSidecarUnavailable is returned by the composite solver when sig is
// requested but no sidecar solver can answer it: none is configured
// (`[bgutils] use_sidecar = false`, or a first start that never came up), or
// the one installed reports its child down — crashed, or between a crash and
// the supervisor's restart (SidecarReady) — and neither a sig spec nor goja,
// asked last, answered it either; goja's failure is then in the error's text. Distinguishes from
// ErrSigUnavailable (player-specific extraction failure) so callers /
// tests can tell "no sig solver running" from "this player can't be sig'd."
//
// The down case used to surface as the sidecar's own untyped "sidecar:
// unhealthy" or "sidecar: not running", which read like any one request's
// failure. It is the worker's systemic signal (audit G10): every format whose
// URL needs a signature fails alike while it holds, so the VOD and live
// strategies drop to formats that need no player JS rather than re-select
// one itag at a time. A signature goja answers is no failure at all.
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
	// is up — whether player JS challenges can be solved by the sidecar
	// right now, rather than by the goja fallback alone. See compositeSolver's.
	SidecarReady() bool
	// ChallengesSolvable reports whether the JS challenges of the player at
	// playerURL can be solved right now, by any solver (owner ruling RV-16).
	// See compositeSolver's.
	ChallengesSolvable(playerURL string) bool
}

// playerRefuser is the optional half of a goja Solver that keeps a refusal
// list: the players it will not prepare again for a while (GojaResolver's,
// compileRefusal). A goja Solver without it — the test fakes — refuses none.
type playerRefuser interface {
	refuses(playerURL string) bool
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

// compositeSolver routes cipher requests across two underlying solvers, and
// is the one place that decides the order they are asked in (owner rulings
// RV-11 and RV-12):
//
//	sig: the sidecar, which asks ejs on the player's main body and then on
//	     its tv body (sidecarSolver.solve); then the player's learned sig
//	     spec; then, within the readiness grace of a sidecar outage, the
//	     sidecar once it is back (PR-R3); then goja, last.
//	n:   the sidecar (main body, then tv body), then goja.
//
// Goja runs the Go port of the same ejs (preprocessPlayer), so its answers
// are ejs's; it is still asked last, because the sidecar is the solver ejs
// itself runs in and a healthy one is never passed over. RoutedResolveURL and
// RoutedDecryptNInURL ask this solver; they do not ask goja for a signature
// themselves.
//
// The routing was first written down in
// docs/superpowers/specs/2026-05-05-cipher-via-ejs-sidecar-design.md section 7
// (removed after implementation; read it from git history, e.g. `git show
// 67755b9:docs/superpowers/specs/2026-05-05-cipher-via-ejs-sidecar-design.md`),
// when goja's regex extractor could not solve current players' signatures
// and sig was sidecar-only.
//
// One of sidecar or goja may be nil. A nil sidecar means a signature no
// spec answers goes to goja; a nil goja means neither has a fallback, and
// sig then fails with ErrSidecarUnavailable.
//
// The sig spec (audit G49) is yt-dlp's: a signature function only moves
// characters around, so solving the synthetic challenge chr(0)..chr(L-1)
// once per player and length gives the permutation, which then answers any
// signature of that length with no JS at all (solve_sig and the
// 'sigfuncs' cache, yt_dlp/extractor/youtube/_video.py:3257-3258 and
// 3311-3334 at b77c9304e, stored on disk). yt-dlp answers every signature
// from the spec; Moombox keeps the sidecar's direct answer as the primary
// and the spec as what answers while the sidecar is down, restarting, or
// switched off after having run, ahead of goja's compile of the player.
// Beyond upstream, a spec is kept only once it reproduces the answer of the
// solver that answered a real signature of that length: the sidecar, or
// goja when it was goja that answered.
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

	// players is what is remembered about the players ejs could not solve
	// with on the main variant. It is handed to every sidecar half installed
	// (memoryAdopter), so it outlives the sidecar restarts that rebuild them.
	players *playerMemory

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

	// downMu guards downSince: when an installed sidecar half was first
	// found down in the outage now running, zero while it is up. What the
	// readiness grace (sidecarReadyGrace) is measured from.
	downMu    sync.Mutex
	downSince time.Time
}

// sidecarReadyGrace is how long an installed sidecar half may report its
// child down before a signature it cannot answer goes to goja, last, and
// fails with ErrSidecarUnavailable when goja cannot answer it either (owner
// decision PR-R3, ruling RV-12): a request meeting a down child within it
// waits for the child to come back. The supervisor restarts
// a crashed child in seconds, and ErrSidecarUnavailable is systemic to the
// worker (systemicResolveFailure): it drops every format needing the player
// JS for the run's setup, and a live capture switches path, for good. A
// several-streams-at-once BotGuard burst that restarts the child no longer
// commits every capture it meets to the fallback path. Measured from the
// outage's start, not per request: once the child has been down this long,
// every request goes to goja at once. A variable so a test can shorten
// it; production never writes it.
var sidecarReadyGrace = 60 * time.Second

// sidecarReadyPoll is how often a request waiting out the grace asks the
// installed half whether its child is back.
var sidecarReadyPoll = time.Second

// memoryAdopter is the sidecar half's way to take the composite's player
// memory in place of its own (sidecarSolver.adoptMemory). It is called on a
// half nothing has asked yet.
type memoryAdopter interface {
	adoptMemory(m *playerMemory)
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
	c := &compositeSolver{
		sidecarSolver: sidecar,
		goja:          goja,
		specs:         specs,
		players:       newPlayerMemory(),
		learning:      make(map[string]struct{}),
		refused:       make(map[string]struct{}),
		failed:        make(map[string]struct{}),
	}
	c.sharePlayerMemory(sidecar)
	return c
}

// sharePlayerMemory hands the composite's player memory to a sidecar half
// about to be installed.
func (c *compositeSolver) sharePlayerMemory(sidecar Solver) {
	if a, ok := sidecar.(memoryAdopter); ok {
		a.adoptMemory(c.players)
	}
}

// SetSidecar installs (or, with nil, removes) the sidecar half. Safe to call
// while other goroutines are solving.
// The synthetic challenges the previous sidecar failed are asked again of
// the new one. What is remembered about players (which are solved with the
// tv body) carries over to it.
func (c *compositeSolver) SetSidecar(sidecar Solver) {
	c.sharePlayerMemory(sidecar)
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
// is when signatures have only the sig specs and the goja fallback, and n
// only the goja fallback. Whether those can still solve a player is
// ChallengesSolvable's question, not this one's.
func (c *compositeSolver) SidecarReady() bool {
	sc := c.sidecar()
	return sc != nil && solverReady(sc)
}

// ChallengesSolvable is the one predicate for whether the JS challenges
// (signatureCipher signatures, n) of the player at playerURL can be solved
// right now (owner ruling RV-16: "If it works, then we shouldn't treat them
// as unsolvable"): the sidecar is ready, or goja — the Go port of ejs, asked
// last (rulings RV-11, RV-12) — is there and is not refusing that player
// (GojaResolver's refusal list: a player whose body the port could not
// prepare, or whose compile ran out of time, within the last ten minutes).
// It says nothing about the order the solvers are asked in.
//
// An empty playerURL names no player, and goja solves nothing without one:
// with the sidecar down the answer is then false, as it was before RV-16. A
// player goja has not been asked about yet is taken as solvable; a failure
// on it surfaces through the solve itself (ErrSidecarUnavailable,
// ErrNUnsolved), which the worker's challenge-free fallback acts on, and
// refuses the player for later asks: a body the port could not prepare, a
// compile that ran out of time, or an n answer that is no answer
// (checkNResult).
//
// The extraction reads it to rank the JS-less client first when it is false,
// yt-dlp's _DEFAULT_JSLESS_CLIENTS (_video.py:144, 2972-2977, 2994-3000):
// youtube.PlayerAPI's SetJSChallengeSolvable, wired in cmd/moombox. A PO
// token is another matter: with the sidecar down none is minted whatever this
// answers, and the worker's missing_pot degrade handles that.
func (c *compositeSolver) ChallengesSolvable(playerURL string) bool {
	if c.SidecarReady() {
		return true
	}
	if c.goja == nil || playerURL == "" {
		return false
	}
	r, ok := c.goja.(playerRefuser)
	return !ok || !r.refuses(playerURL)
}

// awaitSidecar waits, within the readiness grace (sidecarReadyGrace) of the
// outage now running, for an installed sidecar half to report its child up,
// and returns it then; nil when the grace is spent, ctx ends, or no half is
// installed (`use_sidecar = false`, or a first start that never came up:
// there is no child to wait for). The caller has already found the half it
// snapshotted down, and the player's sig spec unable to answer.
func (c *compositeSolver) awaitSidecar(ctx context.Context) Solver {
	if c.sidecar() == nil {
		return nil
	}
	c.downMu.Lock()
	if c.downSince.IsZero() {
		c.downSince = time.Now()
	}
	left := sidecarReadyGrace - time.Since(c.downSince)
	c.downMu.Unlock()
	if left <= 0 {
		return nil
	}
	deadline := time.NewTimer(left)
	defer deadline.Stop()
	poll := time.NewTicker(sidecarReadyPoll)
	defer poll.Stop()
	for {
		if sc := c.sidecar(); sc != nil && solverReady(sc) {
			c.markSidecarUp()
			return sc
		}
		select {
		case <-ctx.Done():
			return nil
		case <-deadline.C:
			return nil
		case <-poll.C:
		}
	}
}

// markSidecarUp ends the outage the readiness grace is measured over: the
// next time the child is found down starts a new one.
func (c *compositeSolver) markSidecarUp() {
	c.downMu.Lock()
	c.downSince = time.Time{}
	c.downMu.Unlock()
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

// Sig asks the sidecar, answers from the player's sig spec when the sidecar
// is absent or fails, and asks goja last (owner ruling RV-12). A context that
// has ended is the caller's own error, not an outage, and is surfaced as it
// is, goja not asked.
func (c *compositeSolver) Sig(ctx context.Context, playerID, encryptedSig string) (string, error) {
	out, err := c.sidecarSig(ctx, playerID, encryptedSig)
	if err == nil {
		return out, nil
	}
	if c.goja == nil || ctx.Err() != nil {
		return "", err
	}
	c.noteGojaFall(ctx, playerID, "sig", err)
	out, gerr := c.goja.Sig(ctx, playerID, encryptedSig)
	if gerr != nil {
		// What the sidecar failed with stays the one errors.Is sees
		// (ErrSidecarUnavailable, ErrPlayerJSStale): the worker reads it as
		// a failure every format of the player shares.
		return "", fmt.Errorf("%w (goja fallback: %v)", err, gerr)
	}
	c.learnSigSpecs(ctx, c.goja, playerID, map[string]string{encryptedSig: out})
	return out, nil
}

// sidecarSig is everything Sig tries ahead of goja: the sidecar, the sig
// spec, and the sidecar again once it is back within the readiness grace.
func (c *compositeSolver) sidecarSig(ctx context.Context, playerID, encryptedSig string) (string, error) {
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
		// Within the outage's readiness grace, wait for the child (PR-R3).
		up := c.awaitSidecar(ctx)
		if up == nil {
			return "", sidecarDown(sc, nil)
		}
		sc = up
	} else {
		c.markSidecarUp()
	}
	out, err := sc.Sig(ctx, playerID, encryptedSig)
	if err != nil {
		if ctx.Err() == nil {
			if out, ok := c.sigFromSpec(playerID, encryptedSig); ok {
				return out, nil
			}
			// A request the child died under: once it is back within the
			// grace, asked once more of the half that is up.
			if !solverReady(sc) {
				if up := c.awaitSidecar(ctx); up != nil {
					if out, rerr := up.Sig(ctx, playerID, encryptedSig); rerr == nil {
						c.learnSigSpecs(ctx, up, playerID, map[string]string{encryptedSig: out})
						return out, nil
					}
				}
			}
		}
		return "", sidecarDown(sc, err)
	}
	c.learnSigSpecs(ctx, sc, playerID, map[string]string{encryptedSig: out})
	return out, nil
}

// N asks the sidecar, which asks ejs on the player's main body and then on
// its tv body, and goja only after that has failed (owner ruling RV-11).
// With no sidecar half installed goja is all there is. An answer that
// checkNResult says is no answer (empty, or ending with its own challenge:
// the exception sentinel) counts as that solver failing: the sidecar's goes
// on to goja, as yt-dlp's director moves to its next provider on an invalid
// response (jsc/_director.py:105-124, 6224a3898), and goja's fails the
// solve. solveN used to do this check and then ask goja a second time
// itself, a second place deciding the order.
//
// The error names every solver that failed, the sidecar's cause the one
// errors.Is sees (ErrPlayerJSStale, ErrSidecarUnavailable).
func (c *compositeSolver) N(ctx context.Context, playerID, encryptedN string) (string, error) {
	var sideErr error
	if sc := c.sidecar(); sc != nil {
		out, err := sc.N(ctx, playerID, encryptedN)
		if err == nil {
			err = checkNResult(encryptedN, out)
		}
		if err == nil {
			return out, nil
		}
		if c.goja == nil {
			return "", fmt.Errorf("%w (no goja fallback)", err)
		}
		sideErr = err
		c.noteGojaFall(ctx, playerID, "n", err)
	}
	if c.goja == nil {
		return "", errors.New("cipher: no solver available for n")
	}
	out, err := c.goja.N(ctx, playerID, encryptedN)
	if err == nil {
		err = checkNResult(encryptedN, out)
	}
	switch {
	case err == nil:
		return out, nil
	case sideErr != nil:
		return "", fmt.Errorf("%w (goja fallback: %v)", sideErr, err)
	default:
		return "", err
	}
}

// noteGojaFall logs, once per player, that a solve the sidecar failed (err)
// goes to goja, the last solver asked. Goja runs the Go port of the ejs the
// sidecar runs, but a player ejs cannot solve with on either body is one the
// port is not likely to solve either, so the operator is told the first time
// a player gets there; the line carries what the sidecar failed with, the
// main body's failure and the tv body's.
//
// The Warn is kept for a solve the sidecar was there for and that failed on
// the player itself (playerFailure): ejs cannot solve with it on either body,
// or its file is not to be had. Nothing else spends the player's one line, so
// a player that timed out once and breaks for real later is still announced:
//
//   - a timeout or a fetch that failed in transit says nothing about the
//     player (a Debug line), on the main body or, the main one having
//     failed for good, on the tv one (errTVNotAnswered);
//   - the caller's own context ended: the solve stopped where it was, the
//     main body's failure in hand and the tv body never asked;
//   - the sidecar is down or not installed, or its child died under the
//     request (not ready now, or ErrSidecarUnavailable in the error of a
//     child that is already back): the tv ask may be what the death cut
//     short, and the supervisor logs the outage for itself.
func (c *compositeSolver) noteGojaFall(ctx context.Context, playerID, op string, err error) {
	if !solverReady(c.sidecar()) || errors.Is(err, ErrSidecarUnavailable) || ctx.Err() != nil {
		return
	}
	if !playerFailure(err) || errors.Is(err, errTVNotAnswered) {
		slog.Debug("cipher: the sidecar failed a solve; goja, the last solver asked, is tried",
			"playerID", playerID, "challenge", op, "err", err)
		return
	}
	if !c.players.firstGojaFall(playerID) {
		return
	}
	slog.Warn("cipher: the sidecar could not solve for this player; goja, the last solver asked, is tried",
		"playerID", playerID, "challenge", op, "err", err)
}

// Batch combines sig and n decryption into a single solver call where
// possible. The composite issues ONE sidecar call for both arrays, and asks
// in Sig's and N's order when it fails: the signatures from the player's sig
// specs, and from goja for any no spec answers (owner ruling RV-12), and the
// n entries from goja. A mixed batch the sidecar cannot answer is no longer
// lost whole: it used to fail as soon as one signature had no spec. Every n
// answer, the sidecar's and goja's, is held to checkNResult as N holds it:
// a bad sidecar n sends the n entries to goja, a bad goja n fails the batch.
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
	// sigErr is why the signatures got no answer ahead of goja; nil while
	// they have one, or there are none.
	var sigErr error
	sigsAnswered := len(sigs) == 0

	sc := c.sidecar()
	if sc != nil && !solverReady(sc) {
		// Down: not asked, as Sig does not ask it. The n entries go to goja.
		// Signatures no spec answers wait for the child within the outage's
		// readiness grace, as Sig's do (PR-R3), and go to it once it is back.
		down := sc
		sc = nil
		if !sigsAnswered {
			if res, ok := c.sigsFromSpecs(playerID, sigs); ok {
				sigResults, sigsAnswered = res, true
			} else if sc = c.awaitSidecar(ctx); sc == nil {
				sigErr = sidecarDown(down, nil)
			}
		}
	} else if sc != nil {
		// Up: the outage, if one was open, is over, as Sig ends it. Left
		// open, a Batch-only caller that waited past the grace kept
		// downSince, and the next outage was given no grace at all.
		c.markSidecarUp()
	}
	if sc != nil {
		sr, nr, err := sc.Batch(ctx, playerID, sigs, ns)
		if err == nil {
			// sidecarSolver.Batch already validated completeness; trust it.
			// Its n answers are held to checkNResult as N holds them: a bad
			// one sends the n entries to goja, the signatures standing.
			if len(sigs) > 0 {
				solved := make(map[string]string, len(sigs))
				for _, sig := range sigs {
					solved[sig] = sr[sig]
				}
				c.learnSigSpecs(ctx, sc, playerID, solved)
			}
			if err = checkNResults(ns, nr); err == nil {
				return sr, nr, nil
			}
			if len(sigs) > 0 {
				sigResults = sr
			}
			sigsAnswered = true
		}
		// Sidecar failed: the signatures from the specs if they answer
		// every one; a child that died under the batch asked once more
		// within the readiness grace, as Sig asks it (PR-R3); goja below
		// otherwise. Typed as Sig types it, when the child is down. n falls
		// back to goja per-element.
		if !sigsAnswered {
			if ctx.Err() != nil {
				return nil, nil, err
			}
			if res, ok := c.sigsFromSpecs(playerID, sigs); ok {
				sigResults, sigsAnswered = res, true
			} else {
				if !solverReady(sc) {
					if up := c.awaitSidecar(ctx); up != nil {
						if sr, nr, rerr := up.Batch(ctx, playerID, sigs, ns); rerr == nil {
							solved := make(map[string]string, len(sigs))
							for _, sig := range sigs {
								solved[sig] = sr[sig]
							}
							c.learnSigSpecs(ctx, up, playerID, solved)
							if err = checkNResults(ns, nr); err == nil {
								return sr, nr, nil
							}
							sigResults, sigsAnswered = sr, true
						}
					}
				}
				if !sigsAnswered {
					sigErr = sidecarDown(sc, err)
				}
			}
		}
		if c.goja != nil && (len(ns) > 0 || sigErr != nil) {
			op := "n"
			if sigErr != nil {
				op = "sig"
			}
			c.noteGojaFall(ctx, playerID, op, err)
		}
	} else if !sigsAnswered && sigErr == nil {
		// No sidecar configured: the specs, then goja.
		if res, ok := c.sigsFromSpecs(playerID, sigs); ok {
			sigResults, sigsAnswered = res, true
		} else {
			sigErr = ErrSidecarUnavailable
		}
	}

	if sigErr != nil {
		// Goja, last, for every signature no spec answers.
		if c.goja == nil || ctx.Err() != nil {
			return nil, nil, sigErr
		}
		fromGoja := map[string]string{}
		for _, sig := range sigs {
			if out, ok := c.sigFromSpec(playerID, sig); ok {
				sigResults[sig] = out
				continue
			}
			out, err := c.goja.Sig(ctx, playerID, sig)
			if err != nil {
				return nil, nil, fmt.Errorf("%w (goja fallback: %v)", sigErr, err)
			}
			sigResults[sig] = out
			fromGoja[sig] = out
		}
		c.learnSigSpecs(ctx, c.goja, playerID, fromGoja)
	}

	// n comes from goja here: the sidecar is not configured, down, or
	// failed the batch.
	if len(ns) > 0 {
		if c.goja == nil {
			return nil, nil, errors.New("cipher: no solver available for n batch")
		}
		for _, n := range ns {
			out, err := c.goja.N(ctx, playerID, n)
			if err == nil {
				err = checkNResult(n, out)
			}
			if err != nil {
				return nil, nil, fmt.Errorf("goja n: %w", err)
			}
			nResults[n] = out
		}
	}

	return sigResults, nResults, nil
}

// checkNResults is checkNResult over every n answer of a batch.
func checkNResults(ns []string, out map[string]string) error {
	for _, n := range ns {
		if err := checkNResult(n, out[n]); err != nil {
			return err
		}
	}
	return nil
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
// signatures sc has just answered, mapped to its answers) that the player
// has none for yet. One extra call of sc asks the synthetic challenge of
// every such length; its answer becomes a spec only if it reproduces sc's
// answer for the real signature of that length.
// yt-dlp keeps the synthetic answer as it comes (_video.py:3325-3331);
// the check here is Moombox's own, as the spec is used only when the sidecar
// is not there to be asked, and a wrong one would build URLs that 403.
//
// Best-effort and synchronous: it runs at most once per player and length
// (persisted) while it succeeds, and once per installed sidecar while the
// synthetic challenge fails; its failure costs only the spec, and the caller
// already has its answer. The challenge goes through the sidecar's probe
// path, which never runs the stale-JS recovery a real signature gets.
//
// sc is the solver that answered: the sidecar half, or goja when it was
// asked last (owner ruling RV-12). Goja answers the synthetic challenge
// through GojaResolver.Batch, the challenge percent-escaped on its way into
// the player as ejs does it. Goja's failures are kept in neither refused nor
// failed, which are the sidecar's: a goja-answered signature asks goja's
// synthetic challenge again until a spec is stored.
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
	// What goja does with the challenge says nothing about the sidecar:
	// refused and failed are the sidecar's, and a mark from goja would stop
	// the sidecar learning the same length later.
	mark := func(set map[string]struct{}, n int) {
		if sc == c.goja {
			return
		}
		c.specMu.Lock()
		set[sigSpecKey(playerID, n)] = struct{}{}
		c.specMu.Unlock()
	}
	if err != nil {
		// The caller's own ended context says nothing about the challenge:
		// the next signature asks it again.
		if ctx.Err() == nil {
			for n := range samples {
				mark(c.failed, n)
			}
		}
		slog.Debug("cipher: sig spec: synthetic challenge failed", "playerID", playerID, "err", err)
		return
	}
	for n, smp := range samples {
		ans, ok := answers[syntheticSigChallenge(n)]
		if !ok {
			mark(c.refused, n)
			continue
		}
		spec := make([]int, 0, len(ans))
		for _, r := range ans {
			spec = append(spec, int(r))
		}
		if got, ok := applySigSpec([]rune(smp.in), spec); !ok || got != smp.out || len(spec) == 0 {
			mark(c.refused, n)
			slog.Debug("cipher: sig spec: synthetic answer does not reproduce the real one's, not stored",
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
