package cipher

import "errors"

// Exported sentinel errors for cipher-package failures that consumers may
// classify (e.g. cipher-rotation vs network-blip vs extractor-needs-update).
// Producers wrap these with `fmt.Errorf("...: %w", ErrXxx)` so consumers
// can match via `errors.Is(err, ErrXxx)` while still surfacing the
// contextual prefix.
//
// Audit cross-cutting C3 sentinel migration follow-on.
var (
	// ErrExtractorMismatch signals that a regex-based extractor failed to
	// locate the expected pattern in player JS. Typically means YouTube
	// rotated the player and the extractor needs an update. Includes:
	// "function %q not found", "object %q not found", "array %q not
	// found", "no n-param or sig candidates found", "could not find
	// signature function name", "could not find IIFE closing bracket",
	// etc.
	ErrExtractorMismatch = errors.New("cipher extractor pattern did not match player JS")

	// ErrPlayerJSFetch signals a network-layer failure fetching the
	// player.js file. Typically transient; consumer can retry with backoff.
	ErrPlayerJSFetch = errors.New("failed to fetch player JS")

	// ErrSigDecrypt signals a JS-execution failure inside DecryptSig. The
	// signature solver ran but produced an error or panicked. Often
	// indicates a cipher rotation that broke the extracted solver.
	ErrSigDecrypt = errors.New("signature decrypt failed")

	// ErrNDecrypt signals a JS-execution failure inside DecryptN. The
	// n-param solver ran but produced an error or panicked. Same family
	// as ErrSigDecrypt; often indicates cipher rotation.
	ErrNDecrypt = errors.New("n-parameter decrypt failed")

	// ErrInputRequired signals a programmer-error: the caller passed an
	// empty player URL or stream URL where one is required.
	ErrInputRequired = errors.New("required cipher input is missing")
)

// ErrSigUnavailable indicates the underlying solver could not extract or
// run a sig algorithm for the requested player. Returned by solvers that
// support the cipher.Solver interface but cannot fulfil sig specifically
// (e.g., the legacy goja extractor on a player whose sig is no longer
// pattern-matchable). Composite solvers may surface this verbatim or
// substitute a higher-quality alternative.
var ErrSigUnavailable = errors.New("cipher: sig unavailable for this player")

// ErrPlayerJSStale signals that the sidecar (or another solver) reported
// it could not solve a challenge against its cached preprocessed player
// JS — the canonical case being ejs's "no solutions" error after YouTube
// rotates the cipher algorithm. The sidecarSolver eats this once: it
// invalidates disk + in-memory + sidecar caches and retries with fresh
// JS. Persistent failure (retry also fails) surfaces this sentinel to
// the caller, indicating either an actual Moombox extractor bug or a
// player rotation we can't yet handle. Callers should treat this as
// "cipher pipeline is broken for this player; surfacing as a real error
// rather than masking with goja fallback."
var ErrPlayerJSStale = errors.New("cipher: player JS is stale or unsupported")
