package cipher

import (
	"sync"
	"time"
)

// CipherTimeout bounds a single goja signature- or n-param-decryption call.
// A malformed player.js (e.g. an accidental infinite loop after a YouTube-side
// change) would otherwise hang the solver goroutine while holding Solvers.mu,
// freezing every download. Matches bgutils' DefaultMintTimeout (3s).
const CipherTimeout = 3 * time.Second

// Solvers holds the decrypted n-parameter and signature functions.
// The underlying Goja VM is not thread-safe, so a mutex serializes calls.
type Solvers struct {
	mu  sync.Mutex
	N   func(string) (string, error) // n-parameter decryption, nil if not found
	Sig func(string) (string, error) // signature decryption, nil if not found
}

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
	SignatureKey        string // default "sig"
	NParam             string
}

// ResolveURLResponse contains the resolved URL.
type ResolveURLResponse struct {
	URL string
}

