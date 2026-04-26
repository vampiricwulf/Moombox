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
