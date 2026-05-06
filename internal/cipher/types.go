package cipher

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ErrSolverDead is returned by DecryptN/DecryptSig after the Solvers'
// underlying Goja VM panicked — subsequent calls fail fast instead of
// re-entering a potentially-corrupt runtime. Callers should catch this
// and call Solver.InvalidateSolver to force a re-compile.
var ErrSolverDead = errors.New("cipher solver is poisoned (prior panic); recompile required")

// CipherTimeout bounds a single goja signature- or n-param-decryption call.
// A malformed player.js (e.g. an accidental infinite loop after a YouTube-side
// change) would otherwise hang the solver goroutine while holding Solvers.mu,
// freezing every download. Matches bgutils' DefaultMintTimeout (3s).
const CipherTimeout = 3 * time.Second

// Solvers holds the decrypted n-parameter and signature functions.
// The underlying Goja VM is not thread-safe, so a mutex serializes calls.
//
// The dead flag is set by the Sig/N closures if their inline recover fires:
// a Goja panic can leave the VM in a corrupt state where subsequent calls
// panic again. Tripping dead forces the next DecryptN/DecryptSig to return
// ErrSolverDead so the caller can InvalidateSolver and recompile.
type Solvers struct {
	mu   sync.Mutex
	dead atomic.Bool
	N    func(string) (string, error) // n-parameter decryption, nil if not found
	Sig  func(string) (string, error) // signature decryption, nil if not found
}

// MarkDead flags the solver as poisoned. Callers inside the Sig/N closures
// should invoke this on panic recovery before returning the error.
func (s *Solvers) MarkDead() {
	if s != nil {
		s.dead.Store(true)
	}
}

// IsDead reports whether the solver has been marked poisoned. Exposed for
// external diagnostics; DecryptN/DecryptSig already return ErrSolverDead
// when this is true.
func (s *Solvers) IsDead() bool { return s != nil && s.dead.Load() }

// DecryptN calls the N solver with mutex protection.
//
// When s.N is nil the input passes through unchanged. Callers that need to
// distinguish "no n-decryption required" (e.g. ANDROID_VR which returns
// direct URLs) from "decryption silently skipped" should gate on HasN()
// first — this method's pass-through is intentional but ambiguous.
func (s *Solvers) DecryptN(input string) (string, error) {
	if s.N == nil {
		return input, nil
	}
	if s.dead.Load() {
		return "", ErrSolverDead
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.N(input)
}

// DecryptSig calls the Sig solver with mutex protection. See DecryptN's
// note on the nil-solver pass-through.
func (s *Solvers) DecryptSig(input string) (string, error) {
	if s.Sig == nil {
		return input, nil
	}
	if s.dead.Load() {
		return "", ErrSolverDead
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Sig(input)
}

// HasN reports whether an n-parameter solver was extracted from the player.
// Call this when the caller needs to distinguish "format doesn't need n
// decryption" from "decryption was silently skipped".
func (s *Solvers) HasN() bool { return s != nil && s.N != nil }

// HasSig reports whether a signature solver was extracted from the player.
func (s *Solvers) HasSig() bool { return s != nil && s.Sig != nil }

// SignatureRequest contains the inputs for signature decryption.
type SignatureRequest struct {
	EncryptedSignature string
	NParam             string
	PlayerURL          string
}

// SignatureResponse contains the decrypted signature and n-parameter.
type SignatureResponse struct {
	DecryptedSignature string
	DecryptedNSig      string
}

// ResolveURLRequest contains the inputs for URL resolution.
type ResolveURLRequest struct {
	StreamURL          string
	PlayerURL          string
	EncryptedSignature string
	SignatureKey       string // default "sig"
	NParam             string
}

// ResolveURLResponse contains the resolved URL.
type ResolveURLResponse struct {
	URL string
}

// Solver is the contract for cipher solving. Sig and N each take the
// encrypted parameter value plus the playerID that produced it; both
// return the decrypted value. A solver that cannot serve sig (e.g., a
// legacy in-process extractor on a current player) returns
// ErrSigUnavailable from Sig; the composite solver routes around such
// solvers for sig.
//
// Batch is a hint that callers can use to amortise per-implementation
// overhead when a manifest yields multiple sig and/or n challenges
// at once. Each implementation chooses how (or whether) to amortise:
// the goja-backed solver runs a straight loop because solver
// invocation is in-process and has no batch advantage; the
// sidecar-backed solver issues a single JSON-RPC for both sig and n.
// The composite solver routes through one or the other.
//
// Implementations MUST return a result for every input in sigs and
// ns on success, or return an error. Partial maps are a contract
// violation.
type Solver interface {
	Sig(ctx context.Context, playerID, encryptedSig string) (string, error)
	N(ctx context.Context, playerID, encryptedN string) (string, error)
	Batch(ctx context.Context, playerID string, sigs, ns []string) (sigResults, nResults map[string]string, err error)
}
